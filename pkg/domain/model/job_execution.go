package model

import (
	"slices"

	"github.com/m-mizutani/goerr/v2"
)

// JobExecutionMode selects how a job allocates work across model roles.
type JobExecutionMode string

const (
	JobExecutionModeStandard     JobExecutionMode = "standard"
	JobExecutionModeOrchestrator JobExecutionMode = "orchestrator"
)

// JobExecutionTarget names the existing runtime to which an execution policy applies.
type JobExecutionTarget string

const (
	JobExecutionTargetJob       JobExecutionTarget = "job"
	JobExecutionTargetJobSimple JobExecutionTarget = "job-simple"
)

// JobExecutionModels holds model references, not provider model identifiers.
// Empty Main uses the deployment default; empty Sub follows Main.
type JobExecutionModels struct {
	Main    string
	Sub     string
	Advisor string
}

// JobExecution is an optional job-local policy. A nil policy preserves legacy behavior.
type JobExecution struct {
	Mode    JobExecutionMode
	Cascade bool
	ApplyTo []JobExecutionTarget
	Models  JobExecutionModels
}

// AppliesTo reports whether this policy applies to a job's base strategy.
func (e *JobExecution) AppliesTo(strategy JobStrategy) bool {
	if e == nil {
		return false
	}
	target := JobExecutionTargetJobSimple
	if NormaliseJobStrategy(strategy) == JobStrategyPlanexec {
		target = JobExecutionTargetJob
	}
	return slices.Contains(e.ApplyTo, target)
}

// Validate checks the policy independently of model definitions in global config.
func (e *JobExecution) Validate() error {
	if e == nil {
		return nil
	}
	switch e.Mode {
	case JobExecutionModeStandard, JobExecutionModeOrchestrator:
	case "cascade":
		return goerr.New("job execution mode cascade is not supported; use mode standard or orchestrator with cascade = true")
	default:
		return goerr.New("invalid job execution mode", goerr.V("mode", string(e.Mode)))
	}
	if len(e.ApplyTo) == 0 {
		return goerr.New("job execution apply_to must not be empty")
	}
	seen := make(map[JobExecutionTarget]bool)
	for _, target := range e.ApplyTo {
		switch target {
		case JobExecutionTargetJob, JobExecutionTargetJobSimple:
		default:
			return goerr.New("invalid job execution target", goerr.V("target", string(target)))
		}
		if seen[target] {
			return goerr.New("duplicate job execution target", goerr.V("target", string(target)))
		}
		seen[target] = true
	}
	if e.Cascade && e.Models.Advisor == "" {
		return goerr.New("cascade job execution requires models.advisor")
	}
	return nil
}

// ModelRefs returns all explicitly named model references, including roles that
// are dormant for a selected mode. Definitions must remain valid when modes change.
func (j *Job) ModelRefs() []string {
	if j == nil {
		return nil
	}
	refs := []string{j.LLMModel}
	if j.Execution != nil {
		refs = append(refs, j.Execution.Models.Main, j.Execution.Models.Sub, j.Execution.Models.Advisor)
	}
	refs = slices.DeleteFunc(refs, func(s string) bool { return s == "" })
	slices.Sort(refs)
	return slices.Compact(refs)
}
