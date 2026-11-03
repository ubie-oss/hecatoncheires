package model_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/robfig/cron/v3"
)

func TestCaseLifecycle_IsValid(t *testing.T) {
	gt.Bool(t, model.CaseLifecycleCreated.IsValid()).True()
	gt.Bool(t, model.CaseLifecycleClosed.IsValid()).True()
	gt.Bool(t, model.CaseLifecycle("updated").IsValid()).False()
	gt.Bool(t, model.CaseLifecycle("").IsValid()).False()
}

func TestCaseEventConfig_Matches(t *testing.T) {
	cfg := &model.CaseEventConfig{
		On: []model.CaseLifecycle{
			model.CaseLifecycleCreated,
			model.CaseLifecycleClosed,
		},
	}
	gt.Bool(t, cfg.Matches(model.CaseLifecycleCreated)).True()
	gt.Bool(t, cfg.Matches(model.CaseLifecycleClosed)).True()
	gt.Bool(t, cfg.Matches(model.CaseLifecycle("other"))).False()

	// Nil receiver returns false (no match).
	var nilCfg *model.CaseEventConfig
	gt.Bool(t, nilCfg.Matches(model.CaseLifecycleCreated)).False()
}

func TestCaseEventConfig_Validate(t *testing.T) {
	t.Run("ok with one value", func(t *testing.T) {
		cfg := &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}}
		gt.NoError(t, cfg.Validate())
	})
	t.Run("ok with multiple", func(t *testing.T) {
		cfg := &model.CaseEventConfig{On: []model.CaseLifecycle{
			model.CaseLifecycleCreated, model.CaseLifecycleClosed,
		}}
		gt.NoError(t, cfg.Validate())
	})
	t.Run("empty on", func(t *testing.T) {
		cfg := &model.CaseEventConfig{}
		gt.Error(t, cfg.Validate())
	})
	t.Run("invalid lifecycle", func(t *testing.T) {
		cfg := &model.CaseEventConfig{On: []model.CaseLifecycle{"invalid"}}
		gt.Error(t, cfg.Validate())
	})
	t.Run("duplicate", func(t *testing.T) {
		cfg := &model.CaseEventConfig{On: []model.CaseLifecycle{
			model.CaseLifecycleCreated, model.CaseLifecycleCreated,
		}}
		gt.Error(t, cfg.Validate())
	})
	t.Run("nil receiver", func(t *testing.T) {
		var cfg *model.CaseEventConfig
		gt.Error(t, cfg.Validate())
	})
}

func TestScheduledEventConfig_Validate(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse("0 9 * * *")
	gt.NoError(t, err).Required()

	t.Run("every only", func(t *testing.T) {
		cfg := &model.ScheduledEventConfig{Every: time.Hour}
		gt.NoError(t, cfg.Validate())
	})
	t.Run("cron only", func(t *testing.T) {
		cfg := &model.ScheduledEventConfig{Cron: sched, CronExpr: "0 9 * * *"}
		gt.NoError(t, cfg.Validate())
	})
	t.Run("both", func(t *testing.T) {
		cfg := &model.ScheduledEventConfig{Every: time.Hour, Cron: sched, CronExpr: "0 9 * * *"}
		gt.Error(t, cfg.Validate())
	})
	t.Run("neither", func(t *testing.T) {
		cfg := &model.ScheduledEventConfig{}
		gt.Error(t, cfg.Validate())
	})
	t.Run("nil receiver", func(t *testing.T) {
		var cfg *model.ScheduledEventConfig
		gt.Error(t, cfg.Validate())
	})
}

func TestJobEvents_Validate(t *testing.T) {
	t.Run("case only", func(t *testing.T) {
		ev := &model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		}
		gt.NoError(t, ev.Validate())
	})
	t.Run("scheduled only", func(t *testing.T) {
		ev := &model.JobEvents{
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		}
		gt.NoError(t, ev.Validate())
	})
	t.Run("both", func(t *testing.T) {
		ev := &model.JobEvents{
			Case:      &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleClosed}},
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		}
		gt.NoError(t, ev.Validate())
	})
	t.Run("none", func(t *testing.T) {
		ev := &model.JobEvents{}
		gt.Error(t, ev.Validate())
	})
	t.Run("invalid sub-config propagates", func(t *testing.T) {
		ev := &model.JobEvents{
			Case: &model.CaseEventConfig{On: nil}, // empty -> error
		}
		gt.Error(t, ev.Validate())
	})
}

func TestJob_Validate(t *testing.T) {
	ok := &model.Job{
		ID:     "test-job",
		Prompt: "do a thing",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	gt.NoError(t, ok.Validate())

	t.Run("missing id", func(t *testing.T) {
		j := *ok
		j.ID = ""
		gt.Error(t, j.Validate())
	})
	t.Run("missing prompt", func(t *testing.T) {
		j := *ok
		j.Prompt = ""
		gt.Error(t, j.Validate())
	})
	t.Run("missing events", func(t *testing.T) {
		j := *ok
		j.Events = model.JobEvents{}
		gt.Error(t, j.Validate())
	})
	t.Run("nil receiver", func(t *testing.T) {
		var j *model.Job
		gt.Error(t, j.Validate())
	})
	t.Run("strategy zero value is accepted", func(t *testing.T) {
		j := *ok
		j.Strategy = ""
		gt.NoError(t, j.Validate())
	})
	t.Run("strategy simple is accepted", func(t *testing.T) {
		j := *ok
		j.Strategy = model.JobStrategySimple
		gt.NoError(t, j.Validate())
	})
	t.Run("strategy planexec is accepted", func(t *testing.T) {
		j := *ok
		j.Strategy = model.JobStrategyPlanexec
		gt.NoError(t, j.Validate())
	})
	t.Run("unknown strategy is rejected", func(t *testing.T) {
		j := *ok
		j.Strategy = model.JobStrategy("ultra")
		gt.Error(t, j.Validate())
	})
	t.Run("interactive with planexec is accepted", func(t *testing.T) {
		j := *ok
		j.Strategy = model.JobStrategyPlanexec
		j.Interactive = true
		gt.NoError(t, j.Validate())
	})
	t.Run("interactive with simple is rejected", func(t *testing.T) {
		j := *ok
		j.Strategy = model.JobStrategySimple
		j.Interactive = true
		gt.Error(t, j.Validate())
	})
	t.Run("interactive with empty (defaults to simple) strategy is rejected", func(t *testing.T) {
		j := *ok
		j.Strategy = ""
		j.Interactive = true
		gt.Error(t, j.Validate())
	})
	t.Run("non-interactive is accepted on any valid strategy", func(t *testing.T) {
		j := *ok
		j.Strategy = model.JobStrategySimple
		j.Interactive = false
		gt.NoError(t, j.Validate())
	})
}

func TestJobStrategy_IsValid(t *testing.T) {
	gt.Bool(t, model.JobStrategySimple.IsValid()).True()
	gt.Bool(t, model.JobStrategyPlanexec.IsValid()).True()
	gt.Bool(t, model.JobStrategy("").IsValid()).False()
	gt.Bool(t, model.JobStrategy("planning").IsValid()).False()
}

func TestNormaliseJobStrategy(t *testing.T) {
	gt.Value(t, model.NormaliseJobStrategy("")).Equal(model.JobStrategySimple)
	gt.Value(t, model.NormaliseJobStrategy(model.JobStrategySimple)).Equal(model.JobStrategySimple)
	gt.Value(t, model.NormaliseJobStrategy(model.JobStrategyPlanexec)).Equal(model.JobStrategyPlanexec)
	// Unknown non-empty values pass through unchanged so Validate can
	// reject them with a useful message.
	gt.Value(t, model.NormaliseJobStrategy(model.JobStrategy("ultra"))).Equal(model.JobStrategy("ultra"))
}

func TestJob_Listens(t *testing.T) {
	j := &model.Job{
		ID:     "test-job",
		Prompt: "do a thing",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{
				model.CaseLifecycleCreated,
			}},
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		},
	}

	gt.Bool(t, j.ListensCase(model.CaseLifecycleCreated)).True()
	gt.Bool(t, j.ListensCase(model.CaseLifecycleClosed)).False()
	gt.Bool(t, j.ListensScheduled()).True()

	t.Run("disabled job listens to nothing", func(t *testing.T) {
		d := *j
		d.Disabled = true
		gt.Bool(t, d.ListensCase(model.CaseLifecycleCreated)).False()
		gt.Bool(t, d.ListensScheduled()).False()
	})

	t.Run("nil receiver listens to nothing", func(t *testing.T) {
		var n *model.Job
		gt.Bool(t, n.ListensCase(model.CaseLifecycleCreated)).False()
		gt.Bool(t, n.ListensScheduled()).False()
	})

	t.Run("scheduled-only job does not match case", func(t *testing.T) {
		s := &model.Job{
			ID:     "sched",
			Prompt: "x",
			Events: model.JobEvents{
				Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
			},
		}
		gt.Bool(t, s.ListensCase(model.CaseLifecycleCreated)).False()
		gt.Bool(t, s.ListensScheduled()).True()
	})
}

func TestJob_Reflection_Field(t *testing.T) {
	// Reflection=true passes Validate and the field is retained on the struct.
	j := &model.Job{
		ID:         "reflect-job",
		Prompt:     "do a thing",
		Reflection: true,
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	gt.NoError(t, j.Validate())
	gt.Bool(t, j.Reflection).True()

	// Reflection=false also passes Validate.
	j2 := *j
	j2.Reflection = false
	gt.NoError(t, j2.Validate())
	gt.Bool(t, j2.Reflection).False()
}

func TestJobExecutionJSONPreservesLegacyShape(t *testing.T) {
	job := &model.Job{ID: "example", LLMModel: "example-model"}
	raw, err := json.Marshal(job)
	gt.NoError(t, err).Required()
	var fields map[string]json.RawMessage
	gt.NoError(t, json.Unmarshal(raw, &fields)).Required()
	_, present := fields["Execution"]
	gt.Bool(t, present).False()
	gt.Value(t, string(fields["LLMModel"])).Equal(`"example-model"`)
	job.Execution = &model.JobExecution{Mode: model.JobExecutionModeOrchestrator, Cascade: true, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}, Models: model.JobExecutionModels{Advisor: "example-advisor"}}
	raw, err = json.Marshal(job)
	gt.NoError(t, err).Required()
	gt.NoError(t, json.Unmarshal(raw, &fields)).Required()
	var policy model.JobExecution
	gt.NoError(t, json.Unmarshal(fields["Execution"], &policy)).Required()
	gt.Value(t, policy).Equal(*job.Execution)
}
