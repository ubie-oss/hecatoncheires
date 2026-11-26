// Package scenario defines the TOML schema for eval scenarios and loads /
// validates them. A scenario file carries both the system-under-test workspace
// configuration (authored with the same top-level layout as a normal workspace
// config file) and the eval-specific tables (meta / input / cases / tools /
// persona / expect). The workspace part is extracted by handing the same file
// to config.LoadWorkspaceConfigs, which ignores the eval-only keys; the
// eval-only tables are decoded into Scenario. Keeping both in one file lets a
// scenario be fully self-contained.
package scenario

import (
	"os"
	"slices"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/pelletier/go-toml/v2"
)

// Scenario is one eval case. The TOML-decoded eval tables plus the workspace
// configuration extracted from the same file.
type Scenario struct {
	Meta  Meta       `toml:"meta"`
	Input Input      `toml:"input"`
	Cases []CaseSeed `toml:"cases"`
	// SlackUsers seeds the workspace's known Slack users.
	SlackUsers []SlackUserSeed `toml:"slack_users"`
	Sources    []Source        `toml:"sources"`
	Tools      map[string]Tool `toml:"tools"`
	Persona    Persona         `toml:"persona"`
	// Job (table key `run_job`) selects which workspace Job to run for the
	// job workflow. The key is `run_job`, not `job`, to avoid
	// colliding with the workspace config's `[[job]]` array in the same file.
	Job    *JobSpec `toml:"run_job"`
	Expect Expect   `toml:"expect"`

	// Workspace is loaded from the same file via config.LoadWorkspaceConfigs.
	// Not a TOML field of this struct.
	Workspace *config.WorkspaceConfig `toml:"-"`
	// Path is the source file path, retained for diagnostics.
	Path string `toml:"-"`
}

// Meta identifies the scenario and selects the workflow driver.
type Meta struct {
	ID          string `toml:"id"`
	Description string `toml:"description"`
	Workflow    string `toml:"workflow"`
	// Language is the i18n language the system-under-test agent responds in
	// (the simulated end-user's locale). Distinct from the eval output language.
	Language string `toml:"language"`
	// Now pins the clock used by the evaluated agent (RFC 3339).
	Now string `toml:"now"`
}

// Clock returns the pinned clock, or nil when the scenario does not pin one.
func (m Meta) Clock() (func() time.Time, error) {
	if strings.TrimSpace(m.Now) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(m.Now))
	if err != nil {
		return nil, goerr.Wrap(err, "meta.now must be RFC 3339", goerr.V("now", m.Now))
	}
	return func() time.Time { return t.UTC() }, nil
}

// Input is the triggering input. For thread_mode_initial it is the first
// top-level post in the monitored channel.
type Input struct {
	Text string `toml:"text"`
	// Reporter optionally pins the posting user id. Empty means the harness
	// synthesizes one (the reporter identity does not affect the evaluation).
	Reporter string `toml:"reporter"`
}

// CaseSeed is a pre-existing case injected into the memory repository before the run.
type CaseSeed struct {
	Title       string         `toml:"title"`
	Description string         `toml:"description"`
	BoardStatus string         `toml:"board_status"`
	Fields      map[string]any `toml:"fields"`
	// Reporter pins the case reporter (a Slack user id). Empty uses the seed id.
	Reporter string `toml:"reporter"`
	// Thread is the Slack thread the case is bound to, as slack__get_messages
	// returns it. The first entry is the root post. Include only messages known
	// when the Job would have started.
	Thread []ThreadMessage `toml:"thread"`
}

// ThreadMessage is one seeded message of a case's Slack thread.
type ThreadMessage struct {
	User string `toml:"user"`
	Text string `toml:"text"`
}

// SlackUserSeed is a Slack user known to the workspace, so that assigning it
// passes the existence check the production use case applies.
type SlackUserSeed struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
}

// Source is a workspace data source seeded into the memory repository before
// the run. Source-aware tools / workflows (e.g. the workspace-metadata tool,
// Job runs) read these from the same repo. Exactly one type-specific config
// block must be present, matching Type.
type Source struct {
	Name        string            `toml:"name"`
	Type        string            `toml:"type"` // notion_db | notion_page | slack | github
	Description string            `toml:"description"`
	Enabled     *bool             `toml:"enabled"` // default true
	NotionDB    *NotionDBSource   `toml:"notion_db"`
	NotionPage  *NotionPageSource `toml:"notion_page"`
	Slack       *SlackSource      `toml:"slack"`
	GitHub      *GitHubSource     `toml:"github"`
}

// IsEnabled reports the effective enabled flag (default true when unset).
func (s Source) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// NotionDBSource configures a notion_db source.
type NotionDBSource struct {
	DatabaseID    string `toml:"database_id"`
	DatabaseTitle string `toml:"database_title"`
	DatabaseURL   string `toml:"database_url"`
}

// NotionPageSource configures a notion_page source.
type NotionPageSource struct {
	PageID    string `toml:"page_id"`
	PageTitle string `toml:"page_title"`
	PageURL   string `toml:"page_url"`
	Recursive bool   `toml:"recursive"`
	MaxDepth  int    `toml:"max_depth"`
}

// SlackSource configures a slack source.
type SlackSource struct {
	Channels []SlackChannelRef `toml:"channels"`
}

// SlackChannelRef is one Slack channel in a slack source.
type SlackChannelRef struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
}

// GitHubSource configures a github source.
type GitHubSource struct {
	Repositories []GitHubRepoRef `toml:"repositories"`
}

// GitHubRepoRef is one repository in a github source.
type GitHubRepoRef struct {
	Owner string `toml:"owner"`
	Repo  string `toml:"repo"`
}

// Tool describes how one agent tool behaves during the run. By default a tool
// is simulated: when called, the ToolSimulator LLM produces a response from
// Background. Setting Live runs the real client instead (Background ignored).
type Tool struct {
	Background string `toml:"background"`
	Live       bool   `toml:"live"`
}

// Persona is the simulated end-user that answers the agent's questions.
type Persona struct {
	Description string `toml:"description"`
	Knowledge   string `toml:"knowledge"`
	// MaxAnswerTurns bounds the question/answer loop. Zero means no follow-up
	// answers are produced.
	MaxAnswerTurns int `toml:"max_answer_turns"`
}

// JobSpec selects which workspace Job to run and against which seeded case.
// Used by the job workflow. The Job must be declared in the
// workspace config ([[job]]); the target case must be one of the seeded
// [[cases]] (matched by title, or the first when TargetCase is empty).
type JobSpec struct {
	ID string `toml:"id"`
	// IDs runs several Jobs in order against the same case. Set either ID or IDs.
	IDs        []string `toml:"ids"`
	TargetCase string   `toml:"target_case"`
}

// SelectedIDs returns the Job IDs in replay order.
func (j JobSpec) SelectedIDs() []string {
	if len(j.IDs) > 0 {
		return j.IDs
	}
	return []string{j.ID}
}

// WorkflowJob is the workflow kind that runs a Job against a case.
const WorkflowJob = "job"

// Expect holds the checklist the judge evaluates the produced artifact against.
type Expect struct {
	Checks []Check `toml:"checks"`
}

// Check is one natural-language yes/no question the judge answers against the
// produced artifact (case state + transcript + tool calls).
type Check struct {
	ID       string `toml:"id"`
	Question string `toml:"question"`
}

// Load reads and parses a scenario file: it decodes the eval-specific tables
// and extracts the workspace configuration from the same file (reusing the
// existing config loader, which validates the workspace and ignores eval-only
// keys).
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path) // #nosec G304 - path is a CLI-provided scenario file
	if err != nil {
		return nil, goerr.Wrap(err, "failed to read scenario file", goerr.V("path", path))
	}

	var sc Scenario
	if err := toml.Unmarshal(data, &sc); err != nil {
		return nil, goerr.Wrap(err, "failed to parse scenario TOML", goerr.V("path", path))
	}
	sc.Path = path

	wsConfigs, err := config.LoadWorkspaceConfigs([]string{path})
	if err != nil {
		return nil, goerr.Wrap(err, "failed to load workspace config from scenario", goerr.V("path", path))
	}
	if len(wsConfigs) != 1 {
		return nil, goerr.New("scenario must define exactly one workspace",
			goerr.V("path", path), goerr.V("count", len(wsConfigs)))
	}
	sc.Workspace = wsConfigs[0]

	return &sc, nil
}

// validateSource checks one source: a name, a known type, and exactly the
// matching type-specific config block with the minimum required fields.
func validateSource(scenarioID string, idx int, s Source) error {
	v := func(msg string) error {
		return goerr.New(msg, goerr.V("scenario_id", scenarioID), goerr.V("source_index", idx), goerr.V("source_name", s.Name))
	}
	if strings.TrimSpace(s.Name) == "" {
		return v("source name is required")
	}
	switch s.Type {
	case "notion_db":
		if s.NotionDB == nil || strings.TrimSpace(s.NotionDB.DatabaseID) == "" {
			return v("notion_db source requires [sources.notion_db] with database_id")
		}
	case "notion_page":
		if s.NotionPage == nil || strings.TrimSpace(s.NotionPage.PageID) == "" {
			return v("notion_page source requires [sources.notion_page] with page_id")
		}
	case "slack":
		if s.Slack == nil || len(s.Slack.Channels) == 0 {
			return v("slack source requires [sources.slack] with at least one channel")
		}
		for _, ch := range s.Slack.Channels {
			if strings.TrimSpace(ch.ID) == "" {
				return v("slack source channel requires an id")
			}
		}
	case "github":
		if s.GitHub == nil || len(s.GitHub.Repositories) == 0 {
			return v("github source requires [sources.github] with at least one repository")
		}
		for _, r := range s.GitHub.Repositories {
			if strings.TrimSpace(r.Owner) == "" || strings.TrimSpace(r.Repo) == "" {
				return v("github source repository requires owner and repo")
			}
		}
	default:
		return v("unknown source type (want notion_db | notion_page | slack | github)")
	}
	return nil
}

// ValidateOptions injects the sets the validator checks scenario references
// against. They are passed in (rather than imported) to avoid an import cycle
// with the driver / tool packages.
type ValidateOptions struct {
	// KnownWorkflows is the set of registered workflow driver kinds.
	KnownWorkflows []string
	// KnownTools is the catalog of valid tool names. When empty, tool names are
	// not checked (e.g. during partial validation).
	KnownTools []string
}

// Validate checks the eval-specific parts of the scenario. The workspace part
// was already validated by Load via config.LoadWorkspaceConfigs.
func (sc *Scenario) Validate(opts ValidateOptions) error {
	if sc.Meta.ID == "" {
		return goerr.New("meta.id is required")
	}
	if sc.Meta.Workflow == "" {
		return goerr.New("meta.workflow is required", goerr.V("scenario_id", sc.Meta.ID))
	}
	if !slices.Contains(opts.KnownWorkflows, sc.Meta.Workflow) {
		return goerr.New("unknown workflow kind",
			goerr.V("scenario_id", sc.Meta.ID),
			goerr.V("workflow", sc.Meta.Workflow),
			goerr.V("known", opts.KnownWorkflows))
	}
	if sc.Meta.Language != "" {
		if _, err := i18n.ParseLang(sc.Meta.Language); err != nil {
			return goerr.Wrap(err, "invalid meta.language", goerr.V("scenario_id", sc.Meta.ID))
		}
	}

	if _, err := sc.Meta.Clock(); err != nil {
		return err
	}

	// input.text drives the message-triggered workflows (e.g. thread_mode_initial).
	// The job workflow has no triggering post — its input is the seeded
	// target case plus the selected job — so input.text is not required there.
	if sc.Meta.Workflow != WorkflowJob && strings.TrimSpace(sc.Input.Text) == "" {
		return goerr.New("input.text is required", goerr.V("scenario_id", sc.Meta.ID))
	}

	if len(sc.Expect.Checks) == 0 {
		return goerr.New("expect.checks must define at least one check", goerr.V("scenario_id", sc.Meta.ID))
	}
	seen := make(map[string]bool, len(sc.Expect.Checks))
	for i, c := range sc.Expect.Checks {
		if c.ID == "" {
			return goerr.New("check id is required", goerr.V("scenario_id", sc.Meta.ID), goerr.V("index", i))
		}
		if seen[c.ID] {
			return goerr.New("duplicate check id", goerr.V("scenario_id", sc.Meta.ID), goerr.V("id", c.ID))
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Question) == "" {
			return goerr.New("check question is required", goerr.V("scenario_id", sc.Meta.ID), goerr.V("id", c.ID))
		}
	}

	if len(opts.KnownTools) > 0 {
		for name := range sc.Tools {
			if !slices.Contains(opts.KnownTools, name) {
				return goerr.New("unknown tool name",
					goerr.V("scenario_id", sc.Meta.ID),
					goerr.V("tool", name),
					goerr.V("known", opts.KnownTools))
			}
		}
	}

	for i := range sc.Sources {
		if err := validateSource(sc.Meta.ID, i, sc.Sources[i]); err != nil {
			return err
		}
	}

	if sc.Meta.Workflow == WorkflowJob {
		if sc.Job == nil || (sc.Job.ID == "" && len(sc.Job.IDs) == 0) {
			return goerr.New("job workflow requires [run_job] with id or ids", goerr.V("scenario_id", sc.Meta.ID))
		}
		if sc.Job.ID != "" && len(sc.Job.IDs) > 0 {
			return goerr.New("run_job.id and run_job.ids are mutually exclusive", goerr.V("scenario_id", sc.Meta.ID))
		}
		seenJobs := make(map[string]bool)
		for _, id := range sc.Job.SelectedIDs() {
			if strings.TrimSpace(id) == "" || seenJobs[id] {
				return goerr.New("run_job must use nonempty, unique job IDs", goerr.V("scenario_id", sc.Meta.ID), goerr.V("job_id", id))
			}
			if sc.Workspace != nil && !slices.ContainsFunc(sc.Workspace.Jobs, func(j *model.Job) bool { return j.ID == id }) {
				return goerr.New("job workflow: job id not found in workspace", goerr.V("scenario_id", sc.Meta.ID), goerr.V("job_id", id))
			}
			seenJobs[id] = true
		}
		if len(sc.Cases) == 0 {
			return goerr.New("job workflow requires at least one [[cases]] (the target case)", goerr.V("scenario_id", sc.Meta.ID))
		}
	}

	if sc.Persona.MaxAnswerTurns < 0 {
		return goerr.New("persona.max_answer_turns must be >= 0",
			goerr.V("scenario_id", sc.Meta.ID),
			goerr.V("max_answer_turns", sc.Persona.MaxAnswerTurns))
	}

	return nil
}
