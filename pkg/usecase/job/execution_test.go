package job_test

import (
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"testing"
)

func TestResolvedExecutionSnapshotsOnlySelectedTarget(t *testing.T) {
	j := &model.Job{LLMModel: "legacy", Execution: &model.JobExecution{Mode: model.JobExecutionModeStandard, Cascade: true, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}, Models: model.JobExecutionModels{Advisor: "advisor"}}}
	gt.Value(t, job.ResolvedExecutionForTest(j, model.JobStrategySimple)).Nil()
	got := job.ResolvedExecutionForTest(j, model.JobStrategyPlanexec)
	gt.String(t, got.Models.Main).Equal("legacy")
	gt.String(t, got.Models.Sub).Equal("legacy")
	j.Execution.Models.Main = "changed"
	j.Execution.ApplyTo[0] = model.JobExecutionTargetJobSimple
	gt.String(t, got.Models.Main).Equal("legacy")
	gt.Value(t, got.ApplyTo[0]).Equal(model.JobExecutionTargetJob)
}

func TestExecutionPrompts(t *testing.T) {
	for _, system := range []string{"original constraints", ""} {
		for _, orchestrator := range []bool{false, true} {
			for _, handoff := range []bool{false, true} {
				text, err := job.RenderExecutionPromptForTest(system, orchestrator, handoff)
				gt.NoError(t, err).Required()
				if system != "" {
					gt.String(t, text).Contains(system)
				}
				if orchestrator {
					gt.String(t, text).Contains("Orchestrated execution")
					gt.String(t, text).Contains("read-only tasks")
					if handoff {
						gt.String(t, text).Contains("Advisor handoff")
					} else {
						gt.String(t, text).NotContains("Advisor handoff")
					}
				} else {
					gt.String(t, text).Contains("[Cascade execution]")
				}
			}
		}
	}
}
