package config_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/m-mizutani/gt"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
	"github.com/urfave/cli/v3"
)

func TestGlobalLoadersRejectObsoleteJobExecution(t *testing.T) {
	loaders := map[string]func([]string) error{
		"workspace groups": func(paths []string) error { _, err := config.LoadWorkspaceGroups(paths); return err },
		"export":           func(paths []string) error { _, err := config.LoadExportConfig(paths); return err },
		"models":           func(paths []string) error { _, err := config.LoadLLMModels(paths); return err },
		"agent":            func(paths []string) error { _, err := config.LoadAgentSection(paths); return err },
	}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			path := writeGlobalConfig(t, "obsolete.toml", "[job_execution]\nmode = 'cascade'\n")
			if err := load([]string{path}); err == nil || !strings.Contains(err.Error(), "invalid job execution config") {
				t.Fatalf("global loader silently accepted obsolete job_execution: %v", err)
			}
			path = writeGlobalConfig(t, "legacy.toml", "historically_unknown = 'accepted'\n[agent]\nbudget_usd = 1\n")
			if err := load([]string{path}); err != nil {
				t.Fatalf("global loader changed legacy unknown-key handling: %v", err)
			}
		})
	}
}

// writeGlobalConfig writes content to a temp .toml file and returns its path.
func writeGlobalConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	gt.NoError(t, os.WriteFile(path, []byte(content), 0600)).Required()
	return path
}

// wsRegistry builds a workspace registry populated with the given IDs.
func wsRegistry(ids ...string) *model.WorkspaceRegistry {
	reg := model.NewWorkspaceRegistry()
	for _, id := range ids {
		reg.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: id, Name: id}})
	}
	return reg
}

// runConfigureGroups exercises the real --global-config flag path.
func runConfigureGroups(t *testing.T, ws *model.WorkspaceRegistry, paths ...string) (*model.WorkspaceGroupRegistry, error) {
	t.Helper()
	var appCfg config.AppConfig
	var result *model.WorkspaceGroupRegistry
	var resErr error
	cmd := &cli.Command{
		Flags: appCfg.Flags(),
		Action: func(_ context.Context, c *cli.Command) error {
			result, resErr = appCfg.ConfigureGroups(c, ws)
			return nil
		},
	}
	args := []string{"test"}
	for _, p := range paths {
		args = append(args, "--global-config", p)
	}
	gt.NoError(t, cmd.Run(context.Background(), args)).Required()
	return result, resErr
}

func TestLoadWorkspaceGroups_SingleFileMultipleGroups(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "security"
name = "Security"
description = "Security workspaces"
members = ["risk", "incident"]

[[workspace_group]]
id = "corp"
members = ["legal"]
`)

	groups, err := config.LoadWorkspaceGroups([]string{path})
	gt.NoError(t, err).Required()
	gt.Array(t, groups).Length(2).Required()

	gt.Value(t, groups[0].ID).Equal("security")
	gt.Value(t, groups[0].Name).Equal("Security")
	gt.Value(t, groups[0].Description).Equal("Security workspaces")
	gt.Array(t, groups[0].MemberIDs).Length(2)
	gt.Value(t, groups[0].MemberIDs[0]).Equal("risk")
	gt.Value(t, groups[0].MemberIDs[1]).Equal("incident")

	// name defaults to id when omitted.
	gt.Value(t, groups[1].ID).Equal("corp")
	gt.Value(t, groups[1].Name).Equal("corp")
	gt.Array(t, groups[1].MemberIDs).Length(1)
	gt.Value(t, groups[1].MemberIDs[0]).Equal("legal")
}

func TestLoadWorkspaceGroups_EmptyMembersAllowed(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "wip"
name = "Work in progress"
`)

	groups, err := config.LoadWorkspaceGroups([]string{path})
	gt.NoError(t, err).Required()
	gt.Array(t, groups).Length(1).Required()
	gt.Value(t, groups[0].ID).Equal("wip")
	gt.Array(t, groups[0].MemberIDs).Length(0)
}

func TestLoadWorkspaceGroups_Directory(t *testing.T) {
	dir := t.TempDir()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "a.toml"), []byte(`
[[workspace_group]]
id = "security"
members = ["risk"]
`), 0600)).Required()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "b.toml"), []byte(`
[[workspace_group]]
id = "corp"
members = ["legal"]
`), 0600)).Required()

	groups, err := config.LoadWorkspaceGroups([]string{dir})
	gt.NoError(t, err).Required()
	gt.Array(t, groups).Length(2)

	ids := map[string]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}
	gt.Bool(t, ids["security"]).True()
	gt.Bool(t, ids["corp"]).True()
}

func TestLoadWorkspaceGroups_DuplicateGroupIDAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "a.toml"), []byte(`
[[workspace_group]]
id = "security"
members = ["risk"]
`), 0600)).Required()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "b.toml"), []byte(`
[[workspace_group]]
id = "security"
members = ["legal"]
`), 0600)).Required()

	_, err := config.LoadWorkspaceGroups([]string{dir})
	gt.Error(t, err).Is(config.ErrDuplicateWorkspaceGroupID)
}

func TestLoadWorkspaceGroups_MissingID(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
name = "No ID"
members = ["risk"]
`)
	_, err := config.LoadWorkspaceGroups([]string{path})
	gt.Error(t, err).Is(config.ErrMissingWorkspaceGroupID)
}

func TestLoadWorkspaceGroups_InvalidID(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "Bad_ID"
members = ["risk"]
`)
	_, err := config.LoadWorkspaceGroups([]string{path})
	gt.Error(t, err).Is(config.ErrInvalidWorkspaceGroupID)
}

func TestLoadWorkspaceGroups_DuplicateMemberWithinGroup(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "security"
members = ["risk", "risk"]
`)
	_, err := config.LoadWorkspaceGroups([]string{path})
	gt.Error(t, err).Is(config.ErrDuplicateGroupMember)
}

func TestLoadWorkspaceGroups_NoPaths(t *testing.T) {
	groups, err := config.LoadWorkspaceGroups(nil)
	gt.NoError(t, err)
	gt.Array(t, groups).Length(0)
}

func TestLoadWorkspaceGroups_RejectsWorkspaceSection(t *testing.T) {
	// A misplaced workspace definition in a global config file must be rejected,
	// not silently ignored.
	path := writeGlobalConfig(t, "global.toml", `
[workspace]
id = "risk"

[[workspace_group]]
id = "security"
members = ["risk"]
`)
	_, err := config.LoadWorkspaceGroups([]string{path})
	gt.Error(t, err).Is(config.ErrGlobalConfigContainsWorkspace)
}

func TestLoadWorkspaceGroups_DeduplicatesOverlappingPaths(t *testing.T) {
	// The same file reachable via both a direct path and its directory must be
	// collected once, not twice (which would look like a duplicate group ID).
	dir := t.TempDir()
	path := filepath.Join(dir, "global.toml")
	gt.NoError(t, os.WriteFile(path, []byte(`
[[workspace_group]]
id = "security"
members = ["risk"]
`), 0600)).Required()

	groups, err := config.LoadWorkspaceGroups([]string{path, dir, path})
	gt.NoError(t, err).Required()
	gt.Array(t, groups).Length(1).Required()
	gt.Value(t, groups[0].ID).Equal("security")
}

func TestConfigureGroups_Dormant(t *testing.T) {
	// No --global-config flag: registry is empty, not nil.
	reg, err := runConfigureGroups(t, wsRegistry("risk"))
	gt.NoError(t, err).Required()
	gt.Value(t, reg).NotNil()
	gt.Array(t, reg.List()).Length(0)
}

func TestConfigureGroups_Valid(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "security"
name = "Security"
members = ["risk", "incident"]
`)
	reg, err := runConfigureGroups(t, wsRegistry("risk", "incident", "legal"), path)
	gt.NoError(t, err).Required()

	groups := reg.List()
	gt.Array(t, groups).Length(1).Required()
	gt.Value(t, groups[0].ID).Equal("security")
	gt.Value(t, groups[0].Name).Equal("Security")
	gt.Array(t, groups[0].MemberIDs).Length(2)
	gt.Value(t, groups[0].MemberIDs[0]).Equal("risk")
	gt.Value(t, groups[0].MemberIDs[1]).Equal("incident")
}

func TestConfigureGroups_UnknownMember(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "security"
members = ["risk", "ghost"]
`)
	// "ghost" is not a registered workspace.
	_, err := runConfigureGroups(t, wsRegistry("risk"), path)
	gt.Error(t, err).Is(config.ErrUnknownGroupMember)
}

func TestConfigureGroups_MultiMembership(t *testing.T) {
	path := writeGlobalConfig(t, "global.toml", `
[[workspace_group]]
id = "security"
members = ["risk", "incident"]

[[workspace_group]]
id = "audit"
members = ["risk", "legal"]
`)
	reg, err := runConfigureGroups(t, wsRegistry("risk", "incident", "legal"), path)
	gt.NoError(t, err).Required()

	sec, err := reg.Get("security")
	gt.NoError(t, err).Required()
	audit, err := reg.Get("audit")
	gt.NoError(t, err).Required()

	// "risk" belongs to both groups.
	gt.Bool(t, slices.Contains(sec.MemberIDs, "risk")).True()
	gt.Bool(t, slices.Contains(audit.MemberIDs, "risk")).True()
	gt.Bool(t, slices.Contains(audit.MemberIDs, "legal")).True()
}

const exportConfigTOML = `
[export]
include_private = true

[export.bigquery]
project = "my-project"
location = "asia-northeast1"

[[export.bigquery.workspace]]
id = "risk"
dataset = "hecato_risk"

[[export.bigquery.workspace]]
id = "incident"
dataset = "hecato_incident"
include_private = false
`

func TestLoadExportConfig_Basic(t *testing.T) {
	path := writeGlobalConfig(t, "export.toml", exportConfigTOML)

	section, err := config.LoadExportConfig([]string{path})
	gt.NoError(t, err).Required()
	gt.Value(t, section).NotNil().Required()
	gt.Bool(t, section.IncludePrivate).True()
	gt.Value(t, section.BigQuery).NotNil().Required()
	gt.Value(t, section.BigQuery.Project).Equal("my-project")
	gt.Value(t, section.BigQuery.Location).Equal("asia-northeast1")
	gt.Array(t, section.BigQuery.Workspaces).Length(2)
	gt.Value(t, section.BigQuery.Workspaces[0].ID).Equal("risk")
	gt.Value(t, section.BigQuery.Workspaces[0].Dataset).Equal("hecato_risk")

	// Per-workspace resolution: "risk" inherits the section default (true);
	// "incident" overrides to false.
	gt.Bool(t, section.IncludePrivateFor(section.BigQuery.Workspaces[0])).True()
	gt.Bool(t, section.IncludePrivateFor(section.BigQuery.Workspaces[1])).False()
}

func TestExportSection_IncludePrivateFor_DefaultsToExcluded(t *testing.T) {
	// With no include_private set anywhere, the effective value is false — private
	// data is NOT exported by default.
	s := &config.ExportSection{BigQuery: &config.ExportBigQuerySection{
		Project:    "p",
		Workspaces: []config.ExportWorkspaceMapping{{ID: "risk", Dataset: "ds"}},
	}}
	gt.Bool(t, s.IncludePrivateFor(s.BigQuery.Workspaces[0])).False()
}

func TestLoadExportConfig_None(t *testing.T) {
	path := writeGlobalConfig(t, "noexport.toml", "[[workspace_group]]\nid = \"g\"\n")
	section, err := config.LoadExportConfig([]string{path})
	gt.NoError(t, err).Required()
	gt.Value(t, section).Nil()
}

func TestLoadExportConfig_DuplicateAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "a.toml"), []byte(exportConfigTOML), 0600)).Required()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "b.toml"), []byte(exportConfigTOML), 0600)).Required()

	_, err := config.LoadExportConfig([]string{dir})
	gt.Error(t, err).Is(config.ErrDuplicateExportConfig)
}

func TestExportSection_Validate(t *testing.T) {
	reg := wsRegistry("risk", "incident")

	t.Run("valid", func(t *testing.T) {
		section, err := config.LoadExportConfig([]string{writeGlobalConfig(t, "e.toml", exportConfigTOML)})
		gt.NoError(t, err).Required()
		gt.NoError(t, section.Validate(reg))
	})

	t.Run("missing project", func(t *testing.T) {
		s := &config.ExportSection{BigQuery: &config.ExportBigQuerySection{}}
		gt.Error(t, s.Validate(reg)).Is(config.ErrInvalidExportConfig)
	})

	t.Run("missing bigquery", func(t *testing.T) {
		s := &config.ExportSection{}
		gt.Error(t, s.Validate(reg)).Is(config.ErrInvalidExportConfig)
	})

	t.Run("unknown workspace", func(t *testing.T) {
		s := &config.ExportSection{BigQuery: &config.ExportBigQuerySection{
			Project:    "p",
			Workspaces: []config.ExportWorkspaceMapping{{ID: "nope", Dataset: "ds"}},
		}}
		gt.Error(t, s.Validate(reg)).Is(config.ErrUnknownExportWorkspace)
	})

	t.Run("invalid dataset name (hyphen)", func(t *testing.T) {
		s := &config.ExportSection{BigQuery: &config.ExportBigQuerySection{
			Project:    "p",
			Workspaces: []config.ExportWorkspaceMapping{{ID: "risk", Dataset: "bad-name"}},
		}}
		gt.Error(t, s.Validate(reg)).Is(config.ErrInvalidExportDataset)
	})

	t.Run("duplicate workspace id", func(t *testing.T) {
		s := &config.ExportSection{BigQuery: &config.ExportBigQuerySection{
			Project: "p",
			Workspaces: []config.ExportWorkspaceMapping{
				{ID: "risk", Dataset: "ds1"},
				{ID: "risk", Dataset: "ds2"},
			},
		}}
		gt.Error(t, s.Validate(reg)).Is(config.ErrDuplicateExportWorkspace)
	})

	t.Run("duplicate dataset", func(t *testing.T) {
		s := &config.ExportSection{BigQuery: &config.ExportBigQuerySection{
			Project: "p",
			Workspaces: []config.ExportWorkspaceMapping{
				{ID: "risk", Dataset: "same"},
				{ID: "incident", Dataset: "same"},
			},
		}}
		gt.Error(t, s.Validate(reg)).Is(config.ErrDuplicateExportWorkspace)
	})
}

// TestLoadLLMModels_Basic pins the resolved form of a definition: the reference
// name comes from the alias when there is one, prices are converted out of the
// dollars-per-million-tokens an operator writes, and the provider survives.
func TestLoadLLMModels_Basic(t *testing.T) {
	path := writeGlobalConfig(t, "models.toml", `
[[llm_model]]
provider = "claude"
model = "claude-opus-5"
input_usd_per_mtok = 5.0
output_usd_per_mtok = 25.0
cache_read_usd_per_mtok = 0.5
cache_write_usd_per_mtok = 6.25

[[llm_model]]
alias = "cheap"
provider = "gemini"
model = "gemini-3.7-flash"
input_usd_per_mtok = 0.75
output_usd_per_mtok = 3.75
cache_read_usd_per_mtok = 0.075
`)

	defs, err := config.LoadLLMModels([]string{path})
	gt.NoError(t, err).Required()
	gt.Array(t, defs).Length(2).Required()

	// No alias: the model name is the reference name.
	gt.String(t, defs[0].Ref).Equal("claude-opus-5")
	gt.String(t, defs[0].Provider).Equal(agentkernel.ProviderClaude)
	gt.String(t, defs[0].Model).Equal("claude-opus-5")
	gt.Value(t, defs[0].Rate).Equal(pricing.Rate{
		Input: 5000, Output: 25000, CacheRead: 500, CacheWrite: 6250,
	})

	// An alias replaces the reference name but not the model name.
	gt.String(t, defs[1].Ref).Equal("cheap")
	gt.String(t, defs[1].Provider).Equal(agentkernel.ProviderGemini)
	gt.String(t, defs[1].Model).Equal("gemini-3.7-flash")
	gt.Value(t, defs[1].Rate).Equal(pricing.Rate{Input: 750, Output: 3750, CacheRead: 75})
}

// Definitions are a SET, so several files may each declare some. Only a repeated
// reference name is a conflict.
func TestLoadLLMModels_AcrossFiles(t *testing.T) {
	dir := t.TempDir()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "a.toml"), []byte(`
[[llm_model]]
alias = "main"
provider = "claude"
model = "claude-opus-5"
input_usd_per_mtok = 5.0
output_usd_per_mtok = 25.0
`), 0600)).Required()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "b.toml"), []byte(`
[[llm_model]]
alias = "cheap"
provider = "gemini"
model = "gemini-3.7-flash"
input_usd_per_mtok = 0.75
output_usd_per_mtok = 3.75
`), 0600)).Required()

	defs, err := config.LoadLLMModels([]string{dir})
	gt.NoError(t, err).Required()

	refs := make([]string, 0, len(defs))
	for _, d := range defs {
		refs = append(refs, d.Ref)
	}
	slices.Sort(refs)
	gt.Array(t, refs).Equal([]string{"cheap", "main"})
}

func TestLoadLLMModels_DuplicateRefAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	entry := `
[[llm_model]]
alias = "main"
provider = "claude"
model = "claude-opus-5"
input_usd_per_mtok = 5.0
output_usd_per_mtok = 25.0
`
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "a.toml"), []byte(entry), 0600)).Required()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "b.toml"), []byte(entry), 0600)).Required()

	_, err := config.LoadLLMModels([]string{dir})
	gt.Error(t, err).Is(config.ErrDuplicateLLMModelRef)
}

// An alias colliding with another entry's model name is the same collision: the
// reference name is what a Job writes, whichever half it came from.
func TestLoadLLMModels_AliasCollidesWithAModelName(t *testing.T) {
	path := writeGlobalConfig(t, "models.toml", `
[[llm_model]]
provider = "claude"
model = "claude-opus-5"
input_usd_per_mtok = 5.0
output_usd_per_mtok = 25.0

[[llm_model]]
alias = "claude-opus-5"
provider = "gemini"
model = "gemini-3.7-flash"
input_usd_per_mtok = 0.75
output_usd_per_mtok = 3.75
`)

	_, err := config.LoadLLMModels([]string{path})
	gt.Error(t, err).Is(config.ErrDuplicateLLMModelRef)
}

func TestLoadLLMModels_NoPaths(t *testing.T) {
	defs, err := config.LoadLLMModels(nil)
	gt.NoError(t, err)
	gt.Array(t, defs).Length(0)
}

func TestLoadLLMModels_RejectsWorkspaceSection(t *testing.T) {
	path := writeGlobalConfig(t, "bad.toml", `
[workspace]
id = "risk"

[[llm_model]]
provider = "claude"
model = "claude-opus-5"
input_usd_per_mtok = 5.0
output_usd_per_mtok = 25.0
`)

	_, err := config.LoadLLMModels([]string{path})
	gt.Error(t, err).Is(config.ErrGlobalConfigContainsWorkspace)
}

func TestLLMModelSection_Validate(t *testing.T) {
	valid := func() *config.LLMModelSection {
		return &config.LLMModelSection{
			Provider:         "gemini",
			Model:            "gemini-3.7-flash",
			InputUSDPerMTok:  0.75,
			OutputUSDPerMTok: 3.75,
		}
	}

	t.Run("valid", func(t *testing.T) {
		def, err := valid().Validate()
		gt.NoError(t, err).Required()
		gt.String(t, def.Ref).Equal("gemini-3.7-flash")
	})

	testCases := map[string]struct {
		mutate func(*config.LLMModelSection) *config.LLMModelSection
		// wantIs is the sentinel the error must carry, or nil when the rejection
		// comes from agentkernel.ModelDef.Validate, which carries none.
		wantIs error
	}{
		"no model": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection { s.Model = ""; return s },
			wantIs: config.ErrInvalidLLMModel,
		},
		"unknown provider": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection { s.Provider = "bedrock"; return s },
		},
		"missing input price": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection { s.InputUSDPerMTok = 0; return s },
			wantIs: config.ErrInvalidLLMModelPrice,
		},
		"missing output price": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection { s.OutputUSDPerMTok = 0; return s },
			wantIs: config.ErrInvalidLLMModelPrice,
		},
		"negative input price": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection { s.InputUSDPerMTok = -1; return s },
			wantIs: config.ErrInvalidLLMModelPrice,
		},
		"negative cache price": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection {
				s.CacheReadUSDPerMTok = -0.1
				return s
			},
			wantIs: config.ErrInvalidLLMModelPrice,
		},
		// A per-token figure written where a per-million one belongs would price
		// the model at nothing, which is what a money budget cannot tolerate.
		"price too small to represent": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection {
				s.InputUSDPerMTok = 0.0000001
				return s
			},
			wantIs: config.ErrInvalidLLMModelPrice,
		},
		"invalid alias": {
			mutate: func(s *config.LLMModelSection) *config.LLMModelSection { s.Alias = "has space"; return s },
			wantIs: config.ErrInvalidLLMModelRef,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			_, err := tc.mutate(valid()).Validate()
			gt.Value(t, err).NotNil().Required()
			if tc.wantIs != nil {
				gt.Error(t, err).Is(tc.wantIs)
			}
		})
	}
}

func TestLLMModelSection_Validate_Reasoning(t *testing.T) {
	claudeModel := func(thinking, display, effort string) *config.LLMModelSection {
		return &config.LLMModelSection{
			Provider:         "claude",
			Model:            "claude-sonnet-5-5",
			InputUSDPerMTok:  3,
			OutputUSDPerMTok: 15,
			Thinking:         thinking,
			ThinkingDisplay:  display,
			Effort:           effort,
		}
	}

	accepted := map[string]struct {
		section *config.LLMModelSection
	}{
		"none set":                {claudeModel("", "", "")},
		"adaptive alone":          {claudeModel("adaptive", "", "")},
		"adaptive summarized":     {claudeModel("adaptive", "summarized", "")},
		"adaptive omitted":        {claudeModel("adaptive", "omitted", "")},
		"effort low":              {claudeModel("", "", "low")},
		"effort medium":           {claudeModel("", "", "medium")},
		"effort high":             {claudeModel("", "", "high")},
		"effort xhigh":            {claudeModel("", "", "xhigh")},
		"effort max":              {claudeModel("", "", "max")},
		"all three":               {claudeModel("adaptive", "summarized", "high")},
		"non-claude with nothing": {&config.LLMModelSection{Provider: "gemini", Model: "gemini-3.7-flash", InputUSDPerMTok: 1, OutputUSDPerMTok: 2}},
	}
	for name, tc := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			def, err := tc.section.Validate()
			gt.NoError(t, err).Required()
			// Carried to the definition unchanged, which is what NewClientFor reads.
			gt.String(t, def.Thinking).Equal(tc.section.Thinking)
			gt.String(t, def.ThinkingDisplay).Equal(tc.section.ThinkingDisplay)
			gt.String(t, def.Effort).Equal(tc.section.Effort)
		})
	}

	nonClaude := func(provider, model string, mutate func(*config.LLMModelSection)) *config.LLMModelSection {
		s := &config.LLMModelSection{Provider: provider, Model: model, InputUSDPerMTok: 1, OutputUSDPerMTok: 2}
		mutate(s)
		return s
	}
	rejected := map[string]*config.LLMModelSection{
		"unknown thinking":           claudeModel("enabled", "", ""),
		"thinking in another case":   claudeModel("Adaptive", "", ""),
		"unknown display":            claudeModel("adaptive", "full", ""),
		"display without thinking":   claudeModel("", "summarized", ""),
		"unknown effort":             claudeModel("", "", "minimal"),
		"thinking on a gemini model": nonClaude("gemini", "gemini-3.7-flash", func(s *config.LLMModelSection) { s.Thinking = "adaptive" }),
		"effort on an openai model":  nonClaude("openai", "gpt-5", func(s *config.LLMModelSection) { s.Effort = "high" }),
		"display on a gemini model":  nonClaude("gemini", "gemini-3.7-flash", func(s *config.LLMModelSection) { s.ThinkingDisplay = "omitted" }),
		"all three on an openai model": nonClaude("openai", "gpt-5", func(s *config.LLMModelSection) {
			s.Thinking, s.ThinkingDisplay, s.Effort = "adaptive", "summarized", "high"
		}),
	}
	for name, section := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := section.Validate()
			gt.Error(t, err).Is(agentkernel.ErrInvalidReasoningSetting)
		})
	}
}

// The three keys are read from the document under the names the API sends them
// as, and a rejected value fails the load — that is, the process start.
func TestLoadLLMModels_Reasoning(t *testing.T) {
	t.Run("read from the document", func(t *testing.T) {
		path := writeGlobalConfig(t, "models.toml", `
[[llm_model]]
provider = "claude"
model = "claude-sonnet-5-5"
input_usd_per_mtok = 3.0
output_usd_per_mtok = 15.0
thinking = "adaptive"
thinking_display = "summarized"
effort = "xhigh"
`)
		defs, err := config.LoadLLMModels([]string{path})
		gt.NoError(t, err).Required()
		gt.Array(t, defs).Length(1).Required()
		gt.String(t, defs[0].Thinking).Equal("adaptive")
		gt.String(t, defs[0].ThinkingDisplay).Equal("summarized")
		gt.String(t, defs[0].Effort).Equal("xhigh")
	})

	t.Run("rejected for another provider", func(t *testing.T) {
		path := writeGlobalConfig(t, "models.toml", `
[[llm_model]]
provider = "gemini"
model = "gemini-3.7-flash"
input_usd_per_mtok = 0.75
output_usd_per_mtok = 3.75
effort = "high"
`)
		_, err := config.LoadLLMModels([]string{path})
		gt.Error(t, err).Is(agentkernel.ErrInvalidReasoningSetting)
	})
}

func TestLoadAgentSection_Basic(t *testing.T) {
	path := writeGlobalConfig(t, "agent.toml", `
[agent]
default_budget_usd = 3.5
`)

	sec, err := config.LoadAgentSection([]string{path})
	gt.NoError(t, err).Required()
	gt.Value(t, sec).NotNil().Required()
	gt.Value(t, sec.DefaultBudget()).Equal(pricing.FromUSD(3.5))
}

func TestLoadAgentSection_None(t *testing.T) {
	path := writeGlobalConfig(t, "empty.toml", `
[[workspace_group]]
id = "g1"
`)

	sec, err := config.LoadAgentSection([]string{path})
	gt.NoError(t, err)
	gt.Value(t, sec).Nil()

	// A nil section answers "not set" rather than panicking, which is what a
	// deployment with no [agent] relies on.
	gt.Value(t, sec.DefaultBudget()).Equal(pricing.NanoUSD(0))
}

func TestLoadAgentSection_DuplicateAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	entry := "[agent]\ndefault_budget_usd = 1.0\n"
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "a.toml"), []byte(entry), 0600)).Required()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "b.toml"), []byte(entry), 0600)).Required()

	_, err := config.LoadAgentSection([]string{dir})
	gt.Error(t, err).Is(config.ErrDuplicateAgentConfig)
}

func TestLoadAgentSection_RejectsInvalidBudget(t *testing.T) {
	t.Run("negative", func(t *testing.T) {
		path := writeGlobalConfig(t, "agent.toml", "[agent]\ndefault_budget_usd = -1.0\n")
		_, err := config.LoadAgentSection([]string{path})
		gt.Error(t, err).Is(config.ErrInvalidBudget)
	})

	t.Run("too small to represent", func(t *testing.T) {
		path := writeGlobalConfig(t, "agent.toml", "[agent]\ndefault_budget_usd = 0.0000000001\n")
		_, err := config.LoadAgentSection([]string{path})
		gt.Error(t, err).Is(config.ErrInvalidBudget)
	})
}

// TestValidateJobModels pins the cross-document check: the Jobs come from
// --config and the definitions from --global-config, so nothing else can catch a
// Job pointing at a model that does not exist.
func TestValidateJobModels(t *testing.T) {
	defs := []agentkernel.ModelDef{{
		Ref: "cheap", Provider: agentkernel.ProviderGemini, Model: "gemini-3.7-flash",
		Rate: pricing.Rate{Input: 750, Output: 3750},
	}}

	registryWithJob := func(j *model.Job) *model.WorkspaceRegistry {
		reg := model.NewWorkspaceRegistry()
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "risk", Name: "risk"},
			Jobs:      []*model.Job{j},
		})
		return reg
	}

	t.Run("a defined model passes", func(t *testing.T) {
		reg := registryWithJob(&model.Job{ID: "daily", LLMModel: "cheap"})
		gt.NoError(t, config.ValidateJobModels(defs, reg))
	})

	t.Run("no model named passes", func(t *testing.T) {
		reg := registryWithJob(&model.Job{ID: "daily"})
		gt.NoError(t, config.ValidateJobModels(defs, reg))
	})

	t.Run("an undefined model is refused", func(t *testing.T) {
		reg := registryWithJob(&model.Job{ID: "daily", LLMModel: "expensive"})
		err := config.ValidateJobModels(defs, reg)
		gt.Error(t, err).Is(config.ErrUnknownLLMModelRef)
		gt.String(t, err.Error()).Contains("undefined model")
	})

	// A disabled Job does not run, so it needs no client and no definition.
	t.Run("a disabled job is ignored", func(t *testing.T) {
		reg := registryWithJob(&model.Job{ID: "daily", LLMModel: "expensive", Disabled: true})
		gt.NoError(t, config.ValidateJobModels(defs, reg))
	})

	t.Run("a nil registry passes", func(t *testing.T) {
		gt.NoError(t, config.ValidateJobModels(defs, nil))
	})
	for _, role := range []string{"main", "sub", "advisor"} {
		t.Run("undefined execution role "+role, func(t *testing.T) {
			models := model.JobExecutionModels{Main: "cheap", Sub: "cheap", Advisor: "cheap"}
			switch role {
			case "main":
				models.Main = "undefined"
			case "sub":
				models.Sub = "undefined"
			case "advisor":
				models.Advisor = "undefined"
			}
			reg := registryWithJob(&model.Job{ID: "daily", Execution: &model.JobExecution{Models: models}})
			gt.Error(t, config.ValidateJobModels(defs, reg)).Is(config.ErrUnknownLLMModelRef)
		})
	}
	t.Run("execution roles use concrete model names without aliases", func(t *testing.T) {
		var named []agentkernel.ModelDef
		for _, name := range []string{"example-main-1", "example-sub-1", "example-advisor-1"} {
			section := config.LLMModelSection{Provider: "openai", Model: name, InputUSDPerMTok: 1, OutputUSDPerMTok: 2}
			def, err := section.Validate()
			gt.NoError(t, err).Required()
			gt.Value(t, def.Ref).Equal(name)
			named = append(named, def)
		}
		reg := registryWithJob(&model.Job{ID: "example", Execution: &model.JobExecution{
			Mode: model.JobExecutionModeOrchestrator, Cascade: true,
			Models: model.JobExecutionModels{Main: named[0].Ref, Sub: named[1].Ref, Advisor: named[2].Ref},
		}})
		gt.NoError(t, config.ValidateJobModels(named, reg))
	})

	t.Run("all execution roles defined", func(t *testing.T) {
		reg := registryWithJob(&model.Job{ID: "daily", Execution: &model.JobExecution{
			Models: model.JobExecutionModels{Main: "cheap", Sub: "cheap", Advisor: "cheap"},
		}})
		gt.NoError(t, config.ValidateJobModels(defs, reg))
	})
}
