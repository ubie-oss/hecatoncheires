package scenario_test

import (
	"path/filepath"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/scenario"
)

func validPath() string {
	return filepath.Join("testdata", "valid_thread_initial.toml")
}

var knownWorkflows = []string{"thread_mode_initial"}

func TestLoad_Valid(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	gt.V(t, sc.Meta.ID).Equal("thread-initial-login-issue")
	gt.V(t, sc.Meta.Workflow).Equal("thread_mode_initial")
	gt.V(t, sc.Meta.Language).Equal("en")
	gt.V(t, sc.Input.Text).NotEqual("")

	// Workspace extracted from the same file via the config loader.
	gt.V(t, sc.Workspace).NotNil()
	gt.V(t, sc.Workspace.ID).Equal("support")
	gt.B(t, sc.Workspace.CaseMode.IsThread()).True()
	gt.V(t, sc.Workspace.SlackMonitorChannel).Equal("C0123456789")

	// Eval-specific tables.
	gt.A(t, sc.Expect.Checks).Length(2)
	gt.V(t, sc.Expect.Checks[0].ID).Equal("title-identifies-login-failure")
	gt.A(t, sc.Cases).Length(1)
	gt.V(t, sc.Cases[0].BoardStatus).Equal("done")
	gt.Number(t, sc.Persona.MaxAnswerTurns).Equal(3)

	tool, ok := sc.Tools["slack_search"]
	gt.B(t, ok).True()
	gt.V(t, tool.Background).NotEqual("")
	gt.B(t, tool.Live).False()
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := scenario.Load(filepath.Join("testdata", "does_not_exist.toml"))
	gt.Error(t, err)
}

func TestLoad_BadWorkspaceFailsValidation(t *testing.T) {
	// Thread mode without [case] status set: the reused workspace loader must
	// reject it at Load time.
	_, err := scenario.Load(filepath.Join("testdata", "bad_workspace_thread_no_case.toml"))
	gt.Error(t, err)
}

func TestValidate_OK(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	gt.NoError(t, sc.Validate(scenario.ValidateOptions{
		KnownWorkflows: knownWorkflows,
		KnownTools:     []string{"slack_search", "notion_search", "github_search"},
	}))
}

func TestValidate_UnknownWorkflow(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Meta.Workflow = "no_such_workflow"
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_EmptyChecks(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Expect.Checks = nil
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_DuplicateCheckID(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Expect.Checks = []scenario.Check{
		{ID: "dup", Question: "q1"},
		{ID: "dup", Question: "q2"},
	}
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_EmptyCheckQuestion(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Expect.Checks = []scenario.Check{{ID: "c1", Question: "   "}}
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_EmptyInputText(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Input.Text = ""
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_UnknownTool(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	gt.Error(t, sc.Validate(scenario.ValidateOptions{
		KnownWorkflows: knownWorkflows,
		KnownTools:     []string{"notion_search"}, // slack_search missing from catalog
	}))
}

func TestValidate_UnknownToolSkippedWhenCatalogEmpty(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	// Empty KnownTools means tool names are not checked.
	gt.NoError(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_NegativeMaxAnswerTurns(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Persona.MaxAnswerTurns = -1
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestLoad_Sources(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	gt.A(t, sc.Sources).Length(2)

	gt.V(t, sc.Sources[0].Name).Equal("Incident runbooks")
	gt.V(t, sc.Sources[0].Type).Equal("notion_db")
	gt.B(t, sc.Sources[0].IsEnabled()).True() // default when unset
	gt.V(t, sc.Sources[0].NotionDB).NotNil()
	gt.V(t, sc.Sources[0].NotionDB.DatabaseID).Equal("11112222333344445555666677778888")

	gt.V(t, sc.Sources[1].Type).Equal("slack")
	gt.V(t, sc.Sources[1].Slack).NotNil()
	gt.A(t, sc.Sources[1].Slack.Channels).Length(1)
	gt.V(t, sc.Sources[1].Slack.Channels[0].ID).Equal("C0123456789")
}

func TestValidate_SourceUnknownType(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Sources = []scenario.Source{{Name: "x", Type: "bogus"}}
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_SourceMissingConfig(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Sources = []scenario.Source{{Name: "x", Type: "notion_db"}} // no notion_db block
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_SourceMissingName(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Sources = []scenario.Source{{Type: "github", GitHub: &scenario.GitHubSource{Repositories: []scenario.GitHubRepoRef{{Owner: "o", Repo: "r"}}}}}
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

func TestValidate_SourceGitHubOK(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	sc.Sources = []scenario.Source{{Name: "repos", Type: "github", GitHub: &scenario.GitHubSource{Repositories: []scenario.GitHubRepoRef{{Owner: "o", Repo: "r"}}}}}
	gt.NoError(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}

var jobKnownWorkflows = []string{"thread_mode_initial", "job"}

func TestLoad_JobScenario(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("testdata", "job_simple.toml"))
	gt.NoError(t, err)
	gt.V(t, sc.Meta.Workflow).Equal("job")
	gt.V(t, sc.Job).NotNil()
	gt.V(t, sc.Job.ID).Equal("triage_summary")
	gt.V(t, sc.Job.TargetCase).Equal("Cannot log in to portal (503)")
	gt.A(t, sc.Cases).Length(1)
	// The workspace [[job]] is loaded into the workspace config, not the eval struct.
	gt.A(t, sc.Workspace.Jobs).Length(1)
}

func TestValidate_JobOK(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("testdata", "job_simple.toml"))
	gt.NoError(t, err)
	gt.NoError(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: jobKnownWorkflows}))
}

func TestValidate_JobMissingRunJob(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("testdata", "job_simple.toml"))
	gt.NoError(t, err)
	sc.Job = nil
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: jobKnownWorkflows}))
}

func TestValidate_JobMissingTargetCase(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("testdata", "job_simple.toml"))
	gt.NoError(t, err)
	sc.Cases = nil
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: jobKnownWorkflows}))
}

func TestWorkspaceCaseMode(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err)
	gt.V(t, sc.Workspace.CaseMode).Equal(model.CaseModeThread)
}

func TestValidateJobSelection(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("testdata", "job_simple.toml"))
	gt.NoError(t, err).Required()
	opts := scenario.ValidateOptions{KnownWorkflows: jobKnownWorkflows}

	followup := *sc.Workspace.Jobs[0]
	followup.ID = "followup"
	sc.Workspace.Jobs = append(sc.Workspace.Jobs, &followup)
	sc.Job.ID = ""
	sc.Job.IDs = []string{"followup", "triage_summary"}
	gt.NoError(t, sc.Validate(opts))
	gt.V(t, sc.Job.SelectedIDs()[0]).Equal("followup")

	sc.Job.IDs = []string{"followup", "followup"}
	gt.Error(t, sc.Validate(opts))
	sc.Job.IDs = []string{"followup", "missing"}
	gt.Error(t, sc.Validate(opts))
	sc.Job.IDs = []string{"followup"}
	sc.Job.ID = "triage_summary"
	gt.Error(t, sc.Validate(opts))
}

func TestValidateInvalidReplayClock(t *testing.T) {
	sc, err := scenario.Load(validPath())
	gt.NoError(t, err).Required()
	sc.Meta.Now = "2026-04-11"
	gt.Error(t, sc.Validate(scenario.ValidateOptions{KnownWorkflows: knownWorkflows}))
}
