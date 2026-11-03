package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
	"github.com/pelletier/go-toml/v2"
)

const executionJobBase = `[[job]]
id = "worker"
prompt = "synthetic prompt"
events.case = { on = ["created"] }
`

func TestJobExecutionExampleLoads(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "job-execution.toml")
	data, err := os.ReadFile(path)
	gt.NoError(t, err).Required()
	workspaces, err := config.ParseWorkspaceConfigs([]config.WorkspaceConfigSource{{Name: path, Data: data}})
	gt.NoError(t, err).Required()
	gt.Array(t, workspaces).Length(1).Required()
	gt.Array(t, workspaces[0].Jobs).Length(1).Required()
	job := workspaces[0].Jobs[0]
	gt.String(t, job.ID).Equal("review_open_work")
	gt.Value(t, job.Strategy).Equal(model.JobStrategySimple)
	gt.Value(t, job.Execution).NotNil().Required()
	gt.Value(t, job.Execution.Mode).Equal(model.JobExecutionModeOrchestrator)
	gt.Bool(t, job.Execution.Cascade).True()
	gt.Value(t, job.Execution.ApplyTo).Equal([]model.JobExecutionTarget{model.JobExecutionTargetJob, model.JobExecutionTargetJobSimple})
	gt.Value(t, job.Execution.Models).Equal(model.JobExecutionModels{Main: "claude-haiku-5-5", Sub: "claude-haiku-5-5", Advisor: "claude-sonnet-5-5"})
	gt.Value(t, job.ModelRefs()).Equal([]string{"claude-haiku-5-5", "claude-sonnet-5-5"})
}

func parseExecutionJob(t *testing.T, src string) (*model.Job, error) {
	t.Helper()
	if err := config.ValidateJobExecutionTOML([]byte(src)); err != nil {
		return nil, err
	}
	var app config.AppConfig
	if err := toml.Unmarshal([]byte(src), &app); err != nil {
		return nil, err
	}
	return app.Jobs[0].Validate("")
}

func TestJobExecutionDefaultsAndRoles(t *testing.T) {
	t.Run("absent keeps exact legacy", func(t *testing.T) {
		job, err := parseExecutionJob(t, executionJobBase+`llm_model = "legacy"`)
		gt.Bool(t, err != nil || job.Execution != nil || job.LLMModel != "legacy").False().Required()
	})
	t.Run("defaults inherit legacy main and sub", func(t *testing.T) {
		job, err := parseExecutionJob(t, executionJobBase+"llm_model = \"legacy\"\n[job.execution]\n")
		gt.NoError(t, err).Required()
		gt.Bool(t, job.Execution.Mode != model.JobExecutionModeStandard || job.Execution.Models.Main != "legacy" || job.Execution.Models.Sub != "legacy" || job.Execution.Models.Advisor != "").False().Required()
		gt.Bool(t, !job.Execution.AppliesTo(model.JobStrategySimple) || !job.Execution.AppliesTo(model.JobStrategyPlanexec)).False().Required()
	})
	t.Run("main sub and advisor retain their meanings", func(t *testing.T) {
		job, err := parseExecutionJob(t, executionJobBase+`[job.execution]
mode = "orchestrator"
apply_to = ["job"]
[job.execution.models]
main = "parent"
sub = "reader"
advisor = "expert"
`)
		gt.NoError(t, err).Required()
		gt.Bool(t, job.Execution.Models != (model.JobExecutionModels{Main: "parent", Sub: "reader", Advisor: "expert"})).False().Required()
		gt.Bool(t, job.Execution.AppliesTo(model.JobStrategySimple) || !job.Execution.AppliesTo(model.JobStrategyPlanexec)).False().Required()
	})
	t.Run("sub inherits explicitly configured main", func(t *testing.T) {
		job, err := parseExecutionJob(t, executionJobBase+`llm_model = "parent"
[job.execution.models]
main = "parent"
`)
		gt.Bool(t, err != nil || job.Execution.Models.Sub != "parent").False().Required()
	})
}

func TestJobExecutionRejectsInvalidConfiguration(t *testing.T) {
	for name, suffix := range map[string]string{
		"mode":                  "[job.execution]\nmode = 'automatic'",
		"target":                "[job.execution]\napply_to = ['proposal']",
		"empty targets":         "[job.execution]\napply_to = []",
		"duplicate targets":     "[job.execution]\napply_to = ['job', 'job']",
		"main alias":            "[job.execution.models]\nmain = 'has space'",
		"sub alias":             "[job.execution.models]\nsub = 'has space'",
		"advisor alias":         "[job.execution.models]\nadvisor = 'has space'",
		"oversized alias":       "[job.execution.models]\nsub = '" + strings.Repeat("a", 101) + "'",
		"conflicting main":      "llm_model = 'legacy'\n[job.execution.models]\nmain = 'different'",
		"cascade advisor":       "[job.execution]\ncascade = true",
		"obsolete cascade mode": "[job.execution]\nmode = 'cascade'",
		"interactive cascade":   "strategy = 'planexec'\ninteractive = true\n[job.execution]\nmode = 'standard'\ncascade = true\n[job.execution.models]\nadvisor = 'expert'",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseExecutionJob(t, executionJobBase+suffix)
			gt.Bool(t, err == nil).False().Required()
		})
	}
}

func TestJobExecutionStrictKeysOnlyInNewSection(t *testing.T) {
	for name, suffix := range map[string]string{
		"execution typo":      "[job.execution]\nmod = 'cascade'",
		"role typo":           "[job.execution.models]\nadvisr = 'expert'",
		"nested table":        "[job.execution.unknown]\nvalue = 'x'",
		"cascade type":        "[job.execution]\ncascade = 'true'",
		"mode type":           "[job.execution]\nmode = true",
		"targets type":        "[job.execution]\napply_to = 'job'",
		"target element type": "[job.execution]\napply_to = [true]",
		"models type":         "[job.execution]\nmodels = true",
		"role type":           "[job.execution.models]\nmain = true",
		"inline typo":         "execution = { mode = 'standard', models = { child = 'reader' } }",
	} {
		t.Run(name, func(t *testing.T) {
			err := config.ValidateJobExecutionTOML([]byte(executionJobBase + suffix))
			gt.Bool(t, err == nil).False().Required()
		})
	}
	err := config.ValidateJobExecutionTOML([]byte("[job_execution]\nmode = 'cascade'\n"))
	gt.NoError(t, err)
	err = config.ValidateJobExecutionTOML([]byte(executionJobBase + "historically_unknown = 'accepted'"))
	gt.Bool(t, err != nil).False().Required()
}

func TestJobExecutionPublicLoadersValidateNewSection(t *testing.T) {
	const workspace = "[workspace]\nid = 'synthetic'\nname = 'Synthetic'\n"
	for name, src := range map[string]string{
		"unknown execution": workspace + executionJobBase + "[job.execution]\nmod = 'cascade'\n",
		"unknown role":      workspace + executionJobBase + "[job.execution.models]\nadvisr = 'expert'\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.ParseWorkspaceConfigs([]config.WorkspaceConfigSource{{Name: "synthetic-memory.toml", Data: []byte(src)}})
			gt.Bool(t, err == nil).False().Required()
			path := filepath.Join(t.TempDir(), "synthetic.toml")
			gt.NoError(t, os.WriteFile(path, []byte(src), 0600)).Required()
			_, err = config.LoadFieldSchema(path)
			gt.Bool(t, err == nil).False().Required()
		})
	}
	t.Run("valid execution reaches public loader", func(t *testing.T) {
		src := workspace + executionJobBase + "[job.execution]\nmode = 'orchestrator'\n[job.execution.models]\nmain = 'parent'\nsub = 'reader'\n"
		configs, err := config.ParseWorkspaceConfigs([]config.WorkspaceConfigSource{{Name: "synthetic-memory.toml", Data: []byte(src)}})
		gt.NoError(t, err).Required()
		gt.Bool(t, len(configs) != 1 || len(configs[0].Jobs) != 1 || configs[0].Jobs[0].Execution.Models.Sub != "reader").False().Required()
	})
}

func TestJobExecutionJobIsolation(t *testing.T) {
	src := executionJobBase + `[job.execution]
mode = "standard"
cascade = true
[job.execution.models]
main = "cheap"
advisor = "expert"
[[job]]
id = "legacy"
prompt = "synthetic legacy"
events.case = { on = ["created"] }
[[job]]
id = "another"
prompt = "synthetic second"
events.case = { on = ["created"] }
[job.execution.models]
main = "other"
`
	var app config.AppConfig
	err := toml.Unmarshal([]byte(src), &app)
	gt.Bool(t, err != nil).False().Required()
	var jobs []*model.Job
	for _, section := range app.Jobs {
		job, err := section.Validate("")
		gt.NoError(t, err).Required()
		jobs = append(jobs, job)
	}
	gt.Bool(t, jobs[1].Execution != nil || !reflect.DeepEqual(jobs[0].ModelRefs(), []string{"cheap", "expert"}) || !reflect.DeepEqual(jobs[2].ModelRefs(), []string{"other"})).False().Required()
}

func TestJobExecutionIndependentAxes(t *testing.T) {
	for _, mode := range []string{"standard", "orchestrator"} {
		for _, cascade := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cascade=%t", mode, cascade), func(t *testing.T) {
				src := executionJobBase + fmt.Sprintf("[job.execution]\nmode = '%s'\ncascade = %t\n[job.execution.models]\nmain = 'example-model-1'\nsub = 'example-model-2'\nadvisor = 'example-model-3'\n", mode, cascade)
				job, err := parseExecutionJob(t, src)
				gt.NoError(t, err).Required()
				gt.Value(t, job.Execution.Mode).Equal(model.JobExecutionMode(mode))
				gt.Value(t, job.Execution.Cascade).Equal(cascade)
				gt.Value(t, job.Execution.Models.Main).Equal("example-model-1")
			})
		}
	}
	for _, mode := range []string{"standard", "orchestrator"} {
		_, err := parseExecutionJob(t, executionJobBase+"strategy = 'planexec'\ninteractive = true\n[job.execution]\nmode = '"+mode+"'\ncascade = true\n[job.execution.models]\nadvisor = 'example-model-3'\n")
		gt.Error(t, err)
		job, err := parseExecutionJob(t, executionJobBase+"strategy = 'planexec'\ninteractive = true\n[job.execution]\nmode = '"+mode+"'\ncascade = true\napply_to = ['job-simple']\n[job.execution.models]\nadvisor = 'example-model-3'\n")
		gt.NoError(t, err).Required()
		gt.Bool(t, job.Execution.AppliesTo(job.Strategy)).False()
	}
}

func TestJobExecutionPublicLoadersIgnoreUnknownTopLevelPolicy(t *testing.T) {
	const src = "historically_unknown = 'accepted'\n[workspace]\nid = 'example'\nname = 'Example'\n" + executionJobBase + "llm_model = 'legacy'\nbudget_usd = 0.5\n[job_execution]\nmode = 'cascade'\ncascade = 'ignored'\n[job_execution.models]\nmain = 'undefined'\n"
	configs, err := config.ParseWorkspaceConfigs([]config.WorkspaceConfigSource{{Name: "example.toml", Data: []byte(src)}})
	gt.NoError(t, err).Required()
	gt.Array(t, configs).Length(1).Required()
	gt.Array(t, configs[0].Jobs).Length(1).Required()
	job := configs[0].Jobs[0]
	gt.Value(t, job.Execution).Nil()
	gt.Value(t, job.LLMModel).Equal("legacy")
	gt.Value(t, job.Budget).Equal(pricing.FromUSD(0.5))
	gt.Value(t, job.ModelRefs()).Equal([]string{"legacy"})
	path := filepath.Join(t.TempDir(), "example.toml")
	gt.NoError(t, os.WriteFile(path, []byte(src), 0600)).Required()
	_, err = config.LoadFieldSchema(path)
	gt.NoError(t, err)
	loaded, err := config.LoadWorkspaceConfigs([]string{path})
	gt.NoError(t, err).Required()
	gt.Array(t, loaded).Length(1).Required()
	gt.Array(t, loaded[0].Jobs).Length(1).Required()
	gt.Value(t, loaded[0].Jobs[0].Execution).Nil()
	gt.Value(t, loaded[0].Jobs[0].ModelRefs()).Equal([]string{"legacy"})
}
