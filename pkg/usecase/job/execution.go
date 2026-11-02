package job

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"slices"
	"text/template"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/goerr/v2"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

// loadExecutionUsage pins a fresh billing snapshot for a finish callback,
// including callbacks invoked by cancellation outside any worker claim.
func (d *DurableRuntime) loadExecutionUsage(ctx context.Context, proc *agentkit.Process) (func(), error) {
	if agentkernel.ScopeFrom(proc.Metadata).JobExecution == nil {
		return func() {}, nil
	}
	release, err := d.Models.LoadUsage(ctx, proc.RootID)
	if err != nil {
		return func() {}, goerr.Wrap(err, "accounting_unknown: restore completed job usage")
	}
	return release, nil
}

func (d *DurableRuntime) executionAccountingError(sc agentkernel.Scope, runErr error) error {
	if sc.JobExecution != nil {
		if _, known := d.Models.UsageCost(sc); !known {
			return errors.Join(runErr, goerr.New("accounting_unknown: job execution cost is incomplete"))
		}
	}
	return runErr
}

// resolvedExecution snapshots the job-local policy only for selected targets.
// Inactive policies do not alter the existing strategy, model or tools.
func resolvedExecution(j *model.Job, strategy model.JobStrategy) *model.JobExecution {
	if j == nil || j.Execution == nil || !j.Execution.AppliesTo(strategy) {
		return nil
	}
	policy := *j.Execution
	policy.ApplyTo = slices.Clone(policy.ApplyTo)
	if policy.Models.Main == "" {
		policy.Models.Main = j.LLMModel
	}
	if policy.Models.Sub == "" {
		policy.Models.Sub = policy.Models.Main
	}
	return &policy
}

//go:embed prompts/orchestrator.md
var orchestratorPromptSource string

//go:embed prompts/cascade_main.md
var cascadeMainPromptSource string

var orchestratorPrompt = template.Must(template.New("orchestrator").Parse(orchestratorPromptSource))
var cascadeMainPrompt = template.Must(template.New("cascade-main").Parse(cascadeMainPromptSource))

type executionPromptInput struct {
	SystemPrompt string
	AllowHandoff bool
}

func renderExecutionPrompt(tmpl *template.Template, input executionPromptInput) (string, error) {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, input); err != nil {
		return "", goerr.Wrap(err, "render job execution prompt")
	}
	return out.String(), nil
}
