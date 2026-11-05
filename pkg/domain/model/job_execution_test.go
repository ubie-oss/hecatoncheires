package model_test

import (
	"reflect"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

func TestJobExecutionAppliesToBaseStrategy(t *testing.T) {
	var absent *model.JobExecution
	gt.Bool(t, absent.AppliesTo(model.JobStrategySimple)).False()
	e := &model.JobExecution{ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJobSimple}}
	gt.Bool(t, !e.AppliesTo("") || !e.AppliesTo(model.JobStrategySimple) || e.AppliesTo(model.JobStrategyPlanexec)).False()
	e.ApplyTo = []model.JobExecutionTarget{model.JobExecutionTargetJob}
	gt.Bool(t, e.AppliesTo(model.JobStrategySimple) || !e.AppliesTo(model.JobStrategyPlanexec)).False()
}

func TestJobModelRefsIncludesAllRoles(t *testing.T) {
	j := &model.Job{LLMModel: "parent", Execution: &model.JobExecution{
		Models: model.JobExecutionModels{Main: "parent", Sub: "reader", Advisor: "expert"},
	}}
	got := j.ModelRefs()
	gt.Bool(t, !reflect.DeepEqual(got, []string{"expert", "parent", "reader"})).False()
	var absent *model.Job
	gt.Bool(t, len(absent.ModelRefs()) != 0).False()
}

func TestJobExecutionValidateAxes(t *testing.T) {
	var absent *model.JobExecution
	gt.NoError(t, absent.Validate())
	for _, mode := range []model.JobExecutionMode{model.JobExecutionModeStandard, model.JobExecutionModeOrchestrator} {
		for _, cascade := range []bool{false, true} {
			execution := &model.JobExecution{Mode: mode, Cascade: cascade, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}, Models: model.JobExecutionModels{Advisor: "example-advisor"}}
			gt.NoError(t, execution.Validate())
			execution.Models.Advisor = ""
			if cascade {
				gt.Error(t, execution.Validate())
			} else {
				gt.NoError(t, execution.Validate())
			}
		}
	}
	for _, execution := range []*model.JobExecution{
		{Mode: "cascade", ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}},
		{Mode: "unknown", ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}},
		{Mode: model.JobExecutionModeStandard},
		{Mode: model.JobExecutionModeStandard, ApplyTo: []model.JobExecutionTarget{"unknown"}},
		{Mode: model.JobExecutionModeStandard, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob, model.JobExecutionTargetJob}},
	} {
		gt.Error(t, execution.Validate())
	}
}
