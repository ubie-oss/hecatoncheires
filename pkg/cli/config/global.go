package config

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
	"github.com/pelletier/go-toml/v2"
	"github.com/urfave/cli/v3"
)

// datasetIDPattern validates a BigQuery dataset ID's character set: letters,
// numbers, and underscores only. Hyphens are NOT allowed by BigQuery, so a
// workspace ID (which may contain hyphens) cannot be used directly as a dataset
// name — the [[export.bigquery.workspace]] mapping supplies an explicit name.
// The length bound (maxDatasetIDLength) is checked separately because RE2 caps
// bounded repetition below BigQuery's limit.
var datasetIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// maxDatasetIDLength is BigQuery's maximum dataset ID length.
const maxDatasetIDLength = 1024

// GlobalConfig represents a deployment-wide configuration file supplied via
// --global-config. It is distinct from the per-workspace files under --config
// (which stay "1 file = 1 workspace"): a global config file carries settings
// that span workspaces. Today it holds workspace group definitions only; new
// deployment-wide sections can be added here later without a new flag.
type GlobalConfig struct {
	// Workspace captures a stray [workspace] section so the loader can reject
	// it. Workspace definitions belong under --config, never here. It is a raw
	// map (not the real WorkspaceBaseConfig) because its only use is presence
	// detection; an empty [workspace] table still unmarshals to a non-nil map.
	Workspace       map[string]any          `toml:"workspace"`
	WorkspaceGroups []WorkspaceGroupSection `toml:"workspace_group"`
	Export          *ExportSection          `toml:"export"`
	Agent           *AgentSection           `toml:"agent"`
	LLMModels       []LLMModelSection       `toml:"llm_model"`
}

// WorkspaceGroupSection represents a single [[workspace_group]] table.
type WorkspaceGroupSection struct {
	ID          string   `toml:"id"`
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Members     []string `toml:"members"`
}

// Validate checks one group section in isolation: id presence and format, and
// member uniqueness within this group. Cross-file id uniqueness and member
// existence are enforced by the loader / ConfigureGroups once the full group
// and workspace sets are known.
func (s *WorkspaceGroupSection) Validate() error {
	if s.ID == "" {
		return goerr.Wrap(ErrMissingWorkspaceGroupID, "[[workspace_group]] id is required")
	}
	if !workspaceIDPattern.MatchString(s.ID) || len(s.ID) > 63 {
		return goerr.Wrap(ErrInvalidWorkspaceGroupID,
			"workspace group ID must match ^[a-z0-9]+(-[a-z0-9]+)*$ and be at most 63 characters",
			goerr.V(WorkspaceGroupIDKey, s.ID))
	}
	seen := make(map[string]bool, len(s.Members))
	for _, m := range s.Members {
		if seen[m] {
			return goerr.Wrap(ErrDuplicateGroupMember, "duplicate workspace group member",
				goerr.V(WorkspaceGroupIDKey, s.ID),
				goerr.V(GroupMemberKey, m))
		}
		seen[m] = true
	}
	return nil
}

// collectTOMLFiles expands the given file/dir paths into a flat list of .toml
// file paths, walking directories recursively. Shared by LoadWorkspaceConfigs
// and LoadWorkspaceGroups so the two loaders discover files identically.
func collectTOMLFiles(paths []string) ([]string, error) {
	var tomlFiles []string
	// Deduplicate by absolute path so overlapping inputs (a file listed twice,
	// or a file also reachable through a listed directory) collect it once.
	// Without this, the same file yields a spurious duplicate-ID error at load.
	seen := make(map[string]bool)
	add := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = filepath.Clean(path)
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		tomlFiles = append(tomlFiles, path)
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to stat config path", goerr.V(ConfigPathKey, p))
		}

		if info.IsDir() {
			err := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && strings.HasSuffix(d.Name(), ".toml") {
					add(path)
				}
				return nil
			})
			if err != nil {
				return nil, goerr.Wrap(err, "failed to walk config directory", goerr.V(ConfigPathKey, p))
			}
		} else {
			add(p)
		}
	}
	return tomlFiles, nil
}

// LoadWorkspaceGroups walks the given file/dir paths, parses each .toml as a
// GlobalConfig, validates every [[workspace_group]] section, and rejects
// duplicate group IDs across files. It does not know the workspace set; member
// existence is checked by ConfigureGroups. Zero files (empty paths) yields an
// empty slice with no error — an unset --global-config is a valid state.
func LoadWorkspaceGroups(paths []string) ([]*model.WorkspaceGroup, error) {
	tomlFiles, err := collectTOMLFiles(paths)
	if err != nil {
		return nil, err
	}

	var groups []*model.WorkspaceGroup
	seenIDs := make(map[string]string) // group ID -> file path
	for _, f := range tomlFiles {
		// #nosec G304 - path is expected to be provided by CLI argument
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to read global config file", goerr.V(ConfigPathKey, f))
		}

		if err := ValidateJobExecutionTOML(data); err != nil {
			return nil, goerr.Wrap(err, "invalid job execution config", goerr.V(ConfigPathKey, f))
		}
		var gc GlobalConfig
		if err := toml.Unmarshal(data, &gc); err != nil {
			return nil, goerr.Wrap(err, "failed to parse global config TOML", goerr.V(ConfigPathKey, f))
		}

		// A [workspace] section in a global config file is almost certainly a
		// misplaced workspace definition. Reject it loudly rather than ignore it
		// silently (the docs promise this file never carries [workspace]).
		if gc.Workspace != nil {
			return nil, goerr.Wrap(ErrGlobalConfigContainsWorkspace,
				"global config file must not contain a [workspace] section",
				goerr.V(ConfigPathKey, f))
		}

		for i := range gc.WorkspaceGroups {
			section := &gc.WorkspaceGroups[i]
			if err := section.Validate(); err != nil {
				return nil, goerr.Wrap(err, "invalid [[workspace_group]]", goerr.V(ConfigPathKey, f))
			}

			if existing, ok := seenIDs[section.ID]; ok {
				return nil, goerr.Wrap(ErrDuplicateWorkspaceGroupID, "duplicate workspace group ID",
					goerr.V(WorkspaceGroupIDKey, section.ID),
					goerr.V("first_file", existing),
					goerr.V("second_file", f))
			}
			seenIDs[section.ID] = f

			name := section.Name
			if name == "" {
				name = section.ID
			}
			g := &model.WorkspaceGroup{
				ID:          section.ID,
				Name:        name,
				Description: section.Description,
				MemberIDs:   section.Members,
			}
			if err := g.Validate(); err != nil {
				return nil, goerr.Wrap(err, "invalid workspace group", goerr.V(ConfigPathKey, f))
			}
			groups = append(groups, g)
		}
	}

	return groups, nil
}

// ConfigureGroups reads the --global-config flag, loads workspace groups, and
// cross-checks every member against the workspace registry. It returns a
// never-nil registry: an unset flag yields an empty registry (feature
// dormant). It is a separate method from Configure so the callers that do not
// need groups (assist / diagnosis / job runtime) are untouched.
func (a *AppConfig) ConfigureGroups(c *cli.Command, ws *model.WorkspaceRegistry) (*model.WorkspaceGroupRegistry, error) {
	registry := model.NewWorkspaceGroupRegistry()

	paths := c.StringSlice("global-config")
	if len(paths) == 0 {
		return registry, nil
	}

	groups, err := LoadWorkspaceGroups(paths)
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		for _, memberID := range g.MemberIDs {
			if _, err := ws.Get(memberID); err != nil {
				return nil, goerr.Wrap(ErrUnknownGroupMember,
					"workspace group member references an unknown workspace",
					goerr.V(WorkspaceGroupIDKey, g.ID),
					goerr.V(GroupMemberKey, memberID))
			}
		}
		registry.Register(g)
		logging.Default().Info("Registered workspace group",
			"id", g.ID, "name", g.Name, "member_count", len(g.MemberIDs))
	}

	return registry, nil
}

// ExportSection represents the [export] section of a global config file: the
// deployment-wide configuration for the `export` subcommand. Nil when no global
// config declares [export] (the feature is then unavailable).
type ExportSection struct {
	// IncludePrivate is the default for every workspace: when true, private Cases
	// (and their Actions / Memos) are exported too. Defaults to false — private
	// data is NOT exported unless explicitly opted in. A per-workspace mapping may
	// override it.
	IncludePrivate bool `toml:"include_private"`
	// BigQuery is the BigQuery sink configuration. Required (the only sink today).
	BigQuery *ExportBigQuerySection `toml:"bigquery"`
}

// IncludePrivateFor returns the effective include_private for a mapping: the
// mapping's own value when set, otherwise the section-level default.
func (s *ExportSection) IncludePrivateFor(m ExportWorkspaceMapping) bool {
	if m.IncludePrivate != nil {
		return *m.IncludePrivate
	}
	return s.IncludePrivate
}

// ExportBigQuerySection configures the BigQuery export sink.
type ExportBigQuerySection struct {
	// Project is the destination GCP project ID. Required.
	Project string `toml:"project"`
	// Location is the BigQuery location (e.g. "US", "asia-northeast1") used when a
	// dataset must be created. Optional.
	Location string `toml:"location"`
	// Workspaces maps each exported workspace to its destination dataset. A
	// workspace not listed here is not exported.
	Workspaces []ExportWorkspaceMapping `toml:"workspace"`
}

// ExportWorkspaceMapping maps one workspace to a BigQuery dataset.
type ExportWorkspaceMapping struct {
	// ID is the workspace ID (must exist in the workspace registry).
	ID string `toml:"id"`
	// Dataset is the destination BigQuery dataset name. BigQuery dataset names
	// forbid hyphens, so this is given explicitly rather than derived from ID.
	Dataset string `toml:"dataset"`
	// IncludePrivate overrides the section-level default for this workspace when
	// set (non-nil). Nil means "inherit [export].include_private".
	IncludePrivate *bool `toml:"include_private"`
}

// Validate checks the export section against the workspace registry: BigQuery
// project presence, dataset-name validity, uniqueness of workspace ids and
// dataset names, and existence of every referenced workspace.
func (s *ExportSection) Validate(ws *model.WorkspaceRegistry) error {
	if s.BigQuery == nil {
		return goerr.Wrap(ErrInvalidExportConfig, "[export.bigquery] section is required")
	}
	if s.BigQuery.Project == "" {
		return goerr.Wrap(ErrInvalidExportConfig, "[export.bigquery] project is required")
	}
	seenID := make(map[string]bool, len(s.BigQuery.Workspaces))
	seenDataset := make(map[string]bool, len(s.BigQuery.Workspaces))
	for _, m := range s.BigQuery.Workspaces {
		if m.ID == "" {
			return goerr.Wrap(ErrInvalidExportConfig, "[[export.bigquery.workspace]] id is required")
		}
		if !datasetIDPattern.MatchString(m.Dataset) || len(m.Dataset) > maxDatasetIDLength {
			return goerr.Wrap(ErrInvalidExportDataset,
				"dataset name must be letters/numbers/underscores only, at most 1024 chars (BigQuery forbids hyphens)",
				goerr.V(WorkspaceIDKey, m.ID), goerr.V(ExportDatasetKey, m.Dataset))
		}
		if seenID[m.ID] {
			return goerr.Wrap(ErrDuplicateExportWorkspace, "duplicate export workspace id",
				goerr.V(WorkspaceIDKey, m.ID))
		}
		if seenDataset[m.Dataset] {
			return goerr.Wrap(ErrDuplicateExportWorkspace, "duplicate export dataset name",
				goerr.V(ExportDatasetKey, m.Dataset))
		}
		seenID[m.ID] = true
		seenDataset[m.Dataset] = true
		if _, err := ws.Get(m.ID); err != nil {
			return goerr.Wrap(ErrUnknownExportWorkspace,
				"export workspace mapping references an unknown workspace",
				goerr.V(WorkspaceIDKey, m.ID))
		}
	}
	return nil
}

// LoadExportConfig walks the given file/dir paths, parses each .toml as a
// GlobalConfig, and returns the single [export] section found. It returns (nil,
// nil) when no file declares [export], and an error when more than one does (the
// export config must have a single home). A stray [workspace] section is
// rejected, mirroring LoadWorkspaceGroups. Structural validation against the
// workspace registry is done by ConfigureExport, not here.
func LoadExportConfig(paths []string) (*ExportSection, error) {
	tomlFiles, err := collectTOMLFiles(paths)
	if err != nil {
		return nil, err
	}

	var found *ExportSection
	var foundFile string
	for _, f := range tomlFiles {
		// #nosec G304 - path is expected to be provided by CLI argument
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to read global config file", goerr.V(ConfigPathKey, f))
		}

		if err := ValidateJobExecutionTOML(data); err != nil {
			return nil, goerr.Wrap(err, "invalid job execution config", goerr.V(ConfigPathKey, f))
		}
		var gc GlobalConfig
		if err := toml.Unmarshal(data, &gc); err != nil {
			return nil, goerr.Wrap(err, "failed to parse global config TOML", goerr.V(ConfigPathKey, f))
		}
		if gc.Workspace != nil {
			return nil, goerr.Wrap(ErrGlobalConfigContainsWorkspace,
				"global config file must not contain a [workspace] section",
				goerr.V(ConfigPathKey, f))
		}
		if gc.Export == nil {
			continue
		}
		if found != nil {
			return nil, goerr.Wrap(ErrDuplicateExportConfig,
				"more than one global config file defines [export]",
				goerr.V("first_file", foundFile), goerr.V("second_file", f))
		}
		found = gc.Export
		foundFile = f
	}

	return found, nil
}

// llmModelRefPattern is the character set of a model reference name. It allows
// dots, at-signs and hyphens so a provider's own model name — "gemini-3.7-flash",
// "claude-opus-4-5@20251101" — can be used as the reference name unchanged, which
// is what an entry declaring no alias does.
var llmModelRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// maxLLMModelRefLength bounds the reference name. It appears in log fields, error
// values and Process metadata, none of which benefit from an unbounded string.
const maxLLMModelRefLength = 100

// AgentSection is the [agent] section of a global config file: the
// deployment-wide agent settings that belong in a document rather than on the
// command line. Nil when no global config declares it.
type AgentSection struct {
	// DefaultBudgetUSD is what one agent run may spend when neither the Job nor
	// the command line says otherwise. Zero means "not set here".
	DefaultBudgetUSD float64 `toml:"default_budget_usd"`
}

// Validate checks the value range only. Zero is a legitimate "not set", so it is
// accepted; a negative figure is not, and neither is one so small it rounds away
// to nothing (which would read as "not set" and silently hand the run the
// built-in default).
func (s *AgentSection) Validate() error {
	if s == nil {
		return nil
	}
	if _, err := budgetFromUSD(s.DefaultBudgetUSD); err != nil {
		return goerr.Wrap(err, "invalid [agent] default_budget_usd")
	}
	return nil
}

// DefaultBudget returns the configured budget, or 0 when the section sets none.
func (s *AgentSection) DefaultBudget() pricing.NanoUSD {
	if s == nil {
		return 0
	}
	return pricing.FromUSD(s.DefaultBudgetUSD)
}

// LLMModelSection is one [[llm_model]] entry: a model this deployment may use,
// and what it costs.
//
// Prices are written in dollars per 1M tokens — the unit every provider
// publishes — and converted once, here. Writing them per token would mean
// ten-digit figures nobody can check against a price page.
type LLMModelSection struct {
	// Alias is the name Jobs and --llm-model refer to this entry by. Optional:
	// without it the reference name is Model.
	Alias string `toml:"alias"`
	// Provider is which client serves it: openai, claude or gemini.
	Provider string `toml:"provider"`
	// Model is the model name handed to that provider.
	Model string `toml:"model"`
	// InputUSDPerMTok and OutputUSDPerMTok are required and must be positive: a
	// model priced at nothing has an unbounded budget.
	InputUSDPerMTok  float64 `toml:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `toml:"output_usd_per_mtok"`
	// CacheReadUSDPerMTok and CacheWriteUSDPerMTok are optional (0 when the
	// provider bills no per-token cache read or write).
	CacheReadUSDPerMTok  float64 `toml:"cache_read_usd_per_mtok"`
	CacheWriteUSDPerMTok float64 `toml:"cache_write_usd_per_mtok"`
	// Thinking, ThinkingDisplay and Effort are optional and claude-only. They
	// name the request fields they are sent as (thinking.type, thinking.display,
	// output_config.effort), and each is left out of the request when unset.
	Thinking        string `toml:"thinking"`
	ThinkingDisplay string `toml:"thinking_display"`
	Effort          string `toml:"effort"`
}

// Validate checks one entry and returns its resolved form.
func (s *LLMModelSection) Validate() (agentkernel.ModelDef, error) {
	if s == nil {
		return agentkernel.ModelDef{}, goerr.New("llm model section is nil")
	}
	ref := s.Alias
	if ref == "" {
		ref = s.Model
	}
	if ref == "" {
		return agentkernel.ModelDef{}, goerr.Wrap(ErrInvalidLLMModel,
			"[[llm_model]] model is required")
	}
	if !llmModelRefPattern.MatchString(ref) || len(ref) > maxLLMModelRefLength {
		return agentkernel.ModelDef{}, goerr.Wrap(ErrInvalidLLMModelRef,
			"model reference name must match ^[A-Za-z0-9][A-Za-z0-9._@-]*$ and be at most 100 characters",
			goerr.V(LLMModelRefKey, ref))
	}
	rate, err := s.rate()
	if err != nil {
		return agentkernel.ModelDef{}, goerr.Wrap(err, "invalid [[llm_model]] price",
			goerr.V(LLMModelRefKey, ref))
	}
	def := agentkernel.ModelDef{
		Ref:             ref,
		Provider:        s.Provider,
		Model:           s.Model,
		Rate:            rate,
		Thinking:        s.Thinking,
		ThinkingDisplay: s.ThinkingDisplay,
		Effort:          s.Effort,
	}
	if err := def.Validate(); err != nil {
		return agentkernel.ModelDef{}, goerr.Wrap(err, "invalid [[llm_model]]",
			goerr.V(LLMModelRefKey, ref))
	}
	return def, nil
}

// rate converts the four published prices. A positive price that rounds away to
// zero is refused rather than accepted as free: it is a unit mistake (a per-token
// figure written where a per-million one belongs), and accepting it would price
// the model at nothing.
func (s *LLMModelSection) rate() (pricing.Rate, error) {
	var rate pricing.Rate
	type priceField struct {
		name     string
		usd      float64
		out      *pricing.NanoUSD
		required bool
	}
	for _, f := range []priceField{
		{"input_usd_per_mtok", s.InputUSDPerMTok, &rate.Input, true},
		{"output_usd_per_mtok", s.OutputUSDPerMTok, &rate.Output, true},
		{"cache_read_usd_per_mtok", s.CacheReadUSDPerMTok, &rate.CacheRead, false},
		{"cache_write_usd_per_mtok", s.CacheWriteUSDPerMTok, &rate.CacheWrite, false},
	} {
		if f.usd < 0 {
			return pricing.Rate{}, goerr.Wrap(ErrInvalidLLMModelPrice,
				"price must not be negative",
				goerr.V("field", f.name), goerr.V("value", f.usd))
		}
		if f.required && f.usd == 0 {
			return pricing.Rate{}, goerr.Wrap(ErrInvalidLLMModelPrice,
				"price is required and must be positive",
				goerr.V("field", f.name))
		}
		converted := pricing.FromUSDPerMTok(f.usd)
		if f.usd > 0 && converted == 0 {
			return pricing.Rate{}, goerr.Wrap(ErrInvalidLLMModelPrice,
				"price is too small to represent; it is written in USD per 1M tokens",
				goerr.V("field", f.name), goerr.V("value", f.usd))
		}
		*f.out = converted
	}
	return rate, nil
}

// budgetFromUSD converts a configured budget in dollars, refusing the two shapes
// that would be read as "not set" by mistake: a negative figure, and a positive
// one small enough to round away to zero.
func budgetFromUSD(usd float64) (pricing.NanoUSD, error) {
	if usd < 0 {
		return 0, goerr.Wrap(ErrInvalidBudget, "budget must not be negative",
			goerr.V("budget_usd", usd))
	}
	converted := pricing.FromUSD(usd)
	if usd > 0 && converted == 0 {
		return 0, goerr.Wrap(ErrInvalidBudget, "budget is too small to represent",
			goerr.V("budget_usd", usd))
	}
	return converted, nil
}

// LoadLLMModels walks the given file/dir paths, parses each .toml as a
// GlobalConfig, validates every [[llm_model]] section, and rejects duplicate
// reference names across files. Zero files (an unset --global-config) yields an
// empty slice with no error — a deployment with no LLM configured is a valid
// state.
//
// Unlike [export], definitions are a SET and may be spread over several files:
// two files declaring models do not conflict, and the only collision that
// matters is a repeated reference name.
func LoadLLMModels(paths []string) ([]agentkernel.ModelDef, error) {
	tomlFiles, err := collectTOMLFiles(paths)
	if err != nil {
		return nil, err
	}

	var defs []agentkernel.ModelDef
	seenRefs := make(map[string]string) // reference name -> file path
	for _, f := range tomlFiles {
		gc, err := loadGlobalConfigFile(f)
		if err != nil {
			return nil, err
		}
		for i := range gc.LLMModels {
			def, err := gc.LLMModels[i].Validate()
			if err != nil {
				return nil, goerr.Wrap(err, "invalid [[llm_model]]", goerr.V(ConfigPathKey, f))
			}
			if existing, ok := seenRefs[def.Ref]; ok {
				return nil, goerr.Wrap(ErrDuplicateLLMModelRef,
					"duplicate model reference name",
					goerr.V(LLMModelRefKey, def.Ref),
					goerr.V("first_file", existing),
					goerr.V("second_file", f))
			}
			seenRefs[def.Ref] = f
			defs = append(defs, def)
		}
	}
	return defs, nil
}

// LoadAgentSection walks the given file/dir paths and returns the single [agent]
// section found. It returns (nil, nil) when no file declares one, and an error
// when more than one does — the deployment-wide agent settings must have a single
// home, exactly as [export] does.
func LoadAgentSection(paths []string) (*AgentSection, error) {
	tomlFiles, err := collectTOMLFiles(paths)
	if err != nil {
		return nil, err
	}

	var found *AgentSection
	var foundFile string
	for _, f := range tomlFiles {
		gc, err := loadGlobalConfigFile(f)
		if err != nil {
			return nil, err
		}
		if gc.Agent == nil {
			continue
		}
		if found != nil {
			return nil, goerr.Wrap(ErrDuplicateAgentConfig,
				"more than one global config file defines [agent]",
				goerr.V("first_file", foundFile), goerr.V("second_file", f))
		}
		if err := gc.Agent.Validate(); err != nil {
			return nil, goerr.Wrap(err, "invalid [agent] section", goerr.V(ConfigPathKey, f))
		}
		found = gc.Agent
		foundFile = f
	}
	return found, nil
}

// loadGlobalConfigFile reads and parses one global config file, rejecting a
// stray [workspace] section. It is shared by every loader here so they all
// discover and reject the same way.
func loadGlobalConfigFile(path string) (*GlobalConfig, error) {
	// #nosec G304 - path is expected to be provided by CLI argument
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to read global config file",
			goerr.V(ConfigPathKey, path))
	}
	if err := ValidateJobExecutionTOML(data); err != nil {
		return nil, goerr.Wrap(err, "invalid job execution config", goerr.V(ConfigPathKey, path))
	}
	var gc GlobalConfig
	if err := toml.Unmarshal(data, &gc); err != nil {
		return nil, goerr.Wrap(err, "failed to parse global config TOML",
			goerr.V(ConfigPathKey, path))
	}
	// A [workspace] section in a global config file is almost certainly a
	// misplaced workspace definition. Reject it loudly rather than ignore it
	// silently (the docs promise this file never carries [workspace]).
	if gc.Workspace != nil {
		return nil, goerr.Wrap(ErrGlobalConfigContainsWorkspace,
			"global config file must not contain a [workspace] section",
			goerr.V(ConfigPathKey, path))
	}
	return &gc, nil
}

// ConfigureLLMModels reads the --global-config flag and loads every model
// definition. It mirrors ConfigureGroups / ConfigureExport so callers that do not
// need models are untouched.
func (a *AppConfig) ConfigureLLMModels(c *cli.Command) ([]agentkernel.ModelDef, error) {
	paths := c.StringSlice("global-config")
	if len(paths) == 0 {
		return nil, nil
	}
	return LoadLLMModels(paths)
}

// ConfigureAgentSection reads the --global-config flag and loads the [agent]
// section, or (nil, nil) when none is declared.
func (a *AppConfig) ConfigureAgentSection(c *cli.Command) (*AgentSection, error) {
	paths := c.StringSlice("global-config")
	if len(paths) == 0 {
		return nil, nil
	}
	return LoadAgentSection(paths)
}

// ValidateJobModels checks that every model a Job names is actually defined.
//
// It is a cross-document check — the Jobs come from --config, the definitions
// from --global-config — so it cannot live in either document's own Validate. It
// runs at startup, where a Job pointing at an undefined model must stop the
// process rather than fail at the hour that Job is due, and in `validate`, where
// an operator asks the same question without deploying.
func ValidateJobModels(defs []agentkernel.ModelDef, ws *model.WorkspaceRegistry) error {
	if ws == nil {
		return nil
	}
	known := make(map[string]struct{}, len(defs))
	for _, d := range defs {
		known[d.Ref] = struct{}{}
	}
	for _, entry := range ws.List() {
		if entry == nil {
			continue
		}
		for _, j := range entry.Jobs {
			if j == nil || j.Disabled {
				continue
			}
			for _, ref := range j.ModelRefs() {
				if _, ok := known[ref]; !ok {
					return goerr.Wrap(ErrUnknownLLMModelRef,
						"job references an undefined model",
						goerr.V(WorkspaceIDKey, entry.Workspace.ID),
						goerr.V("job_id", j.ID),
						goerr.V(LLMModelRefKey, ref),
						goerr.V("known", slices.Sorted(maps.Keys(known))))
				}
			}
		}
	}
	return nil
}

// ConfigureExport reads the --global-config flag, loads the [export] section,
// and validates it against the workspace registry. It returns (nil, nil) when no
// [export] is configured (the export subcommand then errors out with a clear
// message). It mirrors ConfigureGroups so callers that do not export are
// untouched.
func (a *AppConfig) ConfigureExport(c *cli.Command, ws *model.WorkspaceRegistry) (*ExportSection, error) {
	paths := c.StringSlice("global-config")
	if len(paths) == 0 {
		return nil, nil
	}

	section, err := LoadExportConfig(paths)
	if err != nil {
		return nil, err
	}
	if section == nil {
		return nil, nil
	}
	if err := section.Validate(ws); err != nil {
		return nil, err
	}
	return section, nil
}
