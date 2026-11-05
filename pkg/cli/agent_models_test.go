package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/cli"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// twoModels declares an expensive default and a cheap alternative, which is the
// shape a deployment that gives one Job its own model has.
//
// The provider is openai because its client is built from an API key alone: a
// test that resolves models must not depend on ambient cloud credentials, which
// exist on a developer's machine and not in CI. The credential rules themselves
// are covered by geminiModel below and by config.LLM's own tests.
const twoModels = `
[[llm_model]]
alias = "main"
provider = "openai"
model = "gpt-4o"
input_usd_per_mtok = 4.0
output_usd_per_mtok = 18.0

[[llm_model]]
alias = "cheap"
provider = "openai"
model = "gpt-4o-mini"
input_usd_per_mtok = 0.75
output_usd_per_mtok = 3.75
`

// geminiModel needs a GCP project, so it is what the missing-credential case
// uses.
const geminiModel = `
[[llm_model]]
alias = "main"
provider = "gemini"
model = "gemini-3.7-flash"
input_usd_per_mtok = 0.75
output_usd_per_mtok = 3.75
`

// writeModelsConfig writes a global config file and returns its path.
func writeModelsConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "global.toml")
	gt.NoError(t, os.WriteFile(path, []byte(content), 0600)).Required()
	return path
}

// jobRegistry builds a registry holding one workspace with the given Jobs.
func jobRegistry(jobs ...*model.Job) *model.WorkspaceRegistry {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "risk", Name: "risk"},
		Jobs:      jobs,
	})
	return reg
}

func TestJobModelRefs(t *testing.T) {
	t.Run("nil registry", func(t *testing.T) {
		gt.Array(t, cli.JobModelRefsForTest(nil)).Length(0)
	})

	t.Run("deduplicated and sorted", func(t *testing.T) {
		reg := jobRegistry(
			&model.Job{ID: "a", LLMModel: "cheap"},
			&model.Job{ID: "b", LLMModel: "cheap"},
			&model.Job{ID: "c", LLMModel: "aux"},
		)
		gt.Array(t, cli.JobModelRefsForTest(reg)).Equal([]string{"aux", "cheap"})
	})

	// A disabled Job never runs, and a Job naming no model uses the default one;
	// neither needs a client of its own.
	t.Run("disabled and unnamed jobs are excluded", func(t *testing.T) {
		reg := jobRegistry(
			&model.Job{ID: "a", LLMModel: "cheap", Disabled: true},
			&model.Job{ID: "b"},
		)
		gt.Array(t, cli.JobModelRefsForTest(reg)).Length(0)
	})
	t.Run("all execution roles across workspaces", func(t *testing.T) {
		reg := jobRegistry(&model.Job{ID: "roles", Execution: &model.JobExecution{
			Models: model.JobExecutionModels{Main: "parent", Sub: "reader", Advisor: "expert"},
		}})
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "other", Name: "other"},
			Jobs: []*model.Job{
				{ID: "another", Execution: &model.JobExecution{Models: model.JobExecutionModels{Main: "reader", Advisor: "specialist"}}},
				{ID: "disabled", Disabled: true, Execution: &model.JobExecution{Models: model.JobExecutionModels{Main: "unused"}}},
			},
		})
		gt.Array(t, cli.JobModelRefsForTest(reg)).Equal([]string{"expert", "parent", "reader", "specialist"})
	})
}

// TestBuildLLMSetupDisabled pins that a deployment with no model named still
// starts: the AI features stay dormant, exactly as they did when no provider was
// configured.
func TestBuildLLMSetupDisabled(t *testing.T) {
	setup, err := cli.BuildLLMSetupForTest(context.Background(), nil)
	gt.NoError(t, err).Required()
	gt.Value(t, setup.Default).Nil()
	gt.Bool(t, setup.Policy.IsZero()).True()
}

// TestBuildLLMSetupRefusesConfiguredModelsWithNoDefault pins the half-finished
// configuration: models were declared and nothing will use them. Starting
// quietly degraded there hides the mistake until someone wonders why no agent
// ever answers.
func TestBuildLLMSetupRefusesConfiguredModelsWithNoDefault(t *testing.T) {
	path := writeModelsConfig(t, twoModels)

	_, err := cli.BuildLLMSetupForTest(context.Background(), nil, "--global-config", path)

	gt.Error(t, err)
	gt.String(t, err.Error()).Contains("--llm-model is required")
}

func TestBuildLLMSetupRefusesAnUndefinedDefault(t *testing.T) {
	path := writeModelsConfig(t, twoModels)

	_, err := cli.BuildLLMSetupForTest(context.Background(), nil,
		"--global-config", path, "--llm-model", "nonexistent",
		"--llm-openai-api-key", "test-key")

	gt.Error(t, err).Is(config.ErrUnknownLLMModelRef)
}

func TestBuildLLMSetupRefusesAnUndefinedJobModel(t *testing.T) {
	path := writeModelsConfig(t, twoModels)
	reg := jobRegistry(&model.Job{ID: "daily", LLMModel: "was-removed"})

	_, err := cli.BuildLLMSetupForTest(context.Background(), reg,
		"--global-config", path, "--llm-model", "main",
		"--llm-openai-api-key", "test-key")

	gt.Error(t, err).Is(config.ErrUnknownLLMModelRef)
	// The Job is named on the error so an operator knows which one to fix; it
	// travels as a goerr value rather than in the message.
	gt.Value(t, goerr.Values(err)["job_id"]).Equal("daily")
}

// TestBuildLLMSetupResolvesEachRunsModelAndBudget is the end of the mechanism:
// after startup, a run naming a model is priced at that model's rate, and a Job
// naming a budget is bounded by it.
func TestBuildLLMSetupResolvesEachRunsModelAndBudget(t *testing.T) {
	path := writeModelsConfig(t, twoModels+`
[agent]
default_budget_usd = 3.0
`)
	reg := jobRegistry(&model.Job{ID: "daily", LLMModel: "cheap", Budget: pricing.FromUSD(0.5)})

	setup, err := cli.BuildLLMSetupForTest(context.Background(), reg,
		"--global-config", path, "--llm-model", "main",
		"--llm-openai-api-key", "test-key")
	gt.NoError(t, err).Required()
	gt.Value(t, setup.Default).NotNil()
	gt.Array(t, setup.Policy.Refs()).Equal([]string{"cheap", "main"})

	// A run that names nothing: the default model's rate and the [agent] budget.
	onDefault := setup.Policy.Resolve(nil)
	gt.Value(t, onDefault.Budget).Equal(pricing.FromUSD(3))
	gt.Value(t, onDefault.Rate.Input).Equal(pricing.FromUSDPerMTok(4))
}

// TestBuildLLMSetupUsesTheFlagBudgetOverTheDocument pins the precedence at the
// composition root, where the two settings actually meet.
func TestBuildLLMSetupUsesTheFlagBudgetOverTheDocument(t *testing.T) {
	path := writeModelsConfig(t, twoModels+`
[agent]
default_budget_usd = 3.0
`)

	setup, err := cli.BuildLLMSetupForTest(context.Background(), nil,
		"--global-config", path, "--llm-model", "main",
		"--llm-openai-api-key", "test-key",
		"--agent-default-budget-usd", "9.0")
	gt.NoError(t, err).Required()

	gt.Value(t, setup.Policy.Resolve(nil).Budget).Equal(pricing.FromUSD(9))
}

// TestBuildLLMSetupRefusesAModelWithoutCredentials pins that the credentials each
// provider needs are checked at startup rather than at the first generate.
func TestBuildLLMSetupRefusesAModelWithoutCredentials(t *testing.T) {
	t.Run("openai without an api key", func(t *testing.T) {
		path := writeModelsConfig(t, twoModels)

		_, err := cli.BuildLLMSetupForTest(context.Background(), nil,
			"--global-config", path, "--llm-model", "main")

		gt.Error(t, err)
		gt.String(t, err.Error()).Contains("openai-api-key")
	})

	t.Run("gemini without a project", func(t *testing.T) {
		path := writeModelsConfig(t, geminiModel)

		_, err := cli.BuildLLMSetupForTest(context.Background(), nil,
			"--global-config", path, "--llm-model", "main")

		gt.Error(t, err)
		gt.String(t, err.Error()).Contains("gemini-project-id")
	})
}
