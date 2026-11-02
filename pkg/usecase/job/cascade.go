package job

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/goerr/v2"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/runtrace"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

type cascadeInput struct {
	Orchestrator bool              `json:"orchestrator,omitempty"`
	Main         cascadeMainInput  `json:"main"`
	Strategy     model.JobStrategy `json:"strategy"`
	MainPlanexec planexec.Input    `json:"main_planexec"`
	Planexec     planexec.Input    `json:"planexec"`
}

type cascadeState struct {
	Input     cascadeInput       `json:"input"`
	Phase     string             `json:"phase"`
	MainID    agentkit.ProcessID `json:"main_id,omitempty"`
	AdvisorID agentkit.ProcessID `json:"advisor_id,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	// MainCost is the main stage's committed spend when it finishes, before
	// the advisor exists. It remains available for audit after a handoff.
	MainCost pricing.NanoUSD `json:"main_cost_nano_usd,omitempty"`
}

type cascadeOutput struct {
	Accepted agentkit.ProcessID `json:"accepted,omitempty"`
	Advisor  bool               `json:"advisor,omitempty"`
	Reason   string             `json:"reason,omitempty"`
}

// The controller generates no text and performs no business operation. Child
// creation, stage choice and awaits are committed atomically by agentkit. One
// root owns the subject, budget, run log and completion callback throughout.
type cascadeStrategy struct {
	runtime *DurableRuntime
	limiter agentkit.Limiter
}

func (*cascadeStrategy) Version() int { return 1 }
func (s *cascadeStrategy) Limit(ctx context.Context, p *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
	return s.limiter(ctx, p, m)
}
func (*cascadeStrategy) Init(in cascadeInput) (cascadeState, error) {
	if in.Strategy != model.JobStrategySimple && in.Strategy != model.JobStrategyPlanexec {
		return cascadeState{}, goerr.New("cascade requires a known advisor strategy")
	}
	if _, err := (&cascadeMainStrategy{}).Init(in.Main); err != nil {
		return cascadeState{}, err
	}
	if in.Orchestrator {
		if in.Strategy != model.JobStrategyPlanexec {
			return cascadeState{}, goerr.New("orchestrated cascade requires planexec")
		}
		if err := in.MainPlanexec.Validate(); err != nil {
			return cascadeState{}, err
		}
	}
	if in.Strategy == model.JobStrategyPlanexec {
		if err := in.Planexec.Validate(); err != nil {
			return cascadeState{}, err
		}
	}
	return cascadeState{Input: in, Phase: "start"}, nil
}

func (s *cascadeStrategy) Step(ctx context.Context, sys agentkit.Syscalls, st cascadeState) (cascadeState, agentkit.Decision[cascadeOutput], error) {
	switch st.Phase {
	case "start":
		meta := agentkernel.WithStageRole(sys.Metadata(), agentkernel.StageRoleMain)
		// A stage receives a share of the original logical-run budget. The
		// controller retains the original total and never replenishes it.
		if !s.runtime.Models.IsZero() {
			remaining, _ := s.runtime.Models.Remaining(meta, sys.Metrics())
			if remaining <= 1 {
				return st, agentkit.Fail[cascadeOutput](agentkit.FailureLimitExceeded, "no cascade budget remains"), nil
			}
			meta = agentkernel.WithBudget(meta, remaining/2)
		}
		var pid agentkit.ProcessID
		var err error
		if st.Input.Orchestrator {
			input := st.Input.MainPlanexec
			spec := cascadeEscalationSpec()
			input.HandoffTool = &spec
			pid, err = s.runtime.planexec.SpawnChild(ctx, sys, input, agentkit.WithMetadata(meta))
		} else {
			pid, err = s.runtime.cascadeMain.SpawnChild(ctx, sys, st.Input.Main, agentkit.WithMetadata(meta))
		}
		if err != nil {
			return st, agentkit.Decision[cascadeOutput]{}, err
		}
		st.MainID, st.Phase = pid, "main"
		return st, agentkit.Suspend[cascadeOutput](agentkit.WaitChildren("main", pid)), nil
	case "main":
		child, err := cascadeChild(sys, "main", st.MainID)
		if err != nil {
			return st, agentkit.Decision[cascadeOutput]{}, err
		}
		var out cascadeMainOutput
		if st.Input.Orchestrator {
			if child.Status != agentkit.ProcessSucceeded {
				out.Escalate = "main_failed"
			} else {
				var result planexec.Output[planexec.TextResult]
				if err := json.Unmarshal(child.Output, &result); err != nil {
					return st, agentkit.Decision[cascadeOutput]{}, err
				}
				switch result.Kind {
				case planexec.OutputHandoff:
					out.Escalate = result.HandoffReason
				case planexec.OutputFallback:
					out.Escalate = "main_fallback"
				case planexec.OutputQuestion:
					out.Hold = "unattended main requested a question"
				}
			}
		} else {
			if child.Status != agentkit.ProcessSucceeded {
				return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, "cascade main failed; advisor restart withheld"), nil
			}
			if err := json.Unmarshal(child.Output, &out); err != nil {
				return st, agentkit.Decision[cascadeOutput]{}, err
			}
		}
		if out.Hold != "" {
			return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, out.Hold), nil
		}
		if out.Escalate == "" {
			return st, agentkit.Done(cascadeOutput{Accepted: st.MainID}), nil
		}
		if out.WriteStarted {
			return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, "advisor restart forbidden after a write"), nil
		}
		st.Reason = out.Escalate
		st.MainCost = s.runtime.Models.Cost(agentkernel.ScopeFrom(sys.Metadata()), sys.Metrics())
		// Persist the routing decision independently of child creation. The
		// reason comes solely from runtime observations, never an eval oracle.
		st.Phase = "advisor_start"
		return st, agentkit.Continue[cascadeOutput](), nil
	case "advisor_start":
		if !s.runtime.Models.IsZero() {
			if _, known := s.runtime.Models.UsageCost(agentkernel.ScopeFrom(sys.Metadata())); !known {
				return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, "accounting_unknown: advisor restart withheld"), nil
			}
			written, err := s.runtime.Models.CascadeWriteStarted(ctx, sys.RootID())
			if err != nil {
				return st, agentkit.Decision[cascadeOutput]{}, err
			}
			if written {
				return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, "advisor restart forbidden after a subtree write"), nil
			}
		}
		meta := agentkernel.WithStageRole(sys.Metadata(), agentkernel.StageRoleAdvisor)
		if !s.runtime.Models.IsZero() {
			remaining, _ := s.runtime.Models.Remaining(sys.Metadata(), sys.Metrics())
			if remaining <= 0 {
				return st, agentkit.Fail[cascadeOutput](agentkit.FailureLimitExceeded, "no budget remains for advisor"), nil
			}
			meta = agentkernel.WithBudget(meta, remaining)
		}
		opts := []agentkit.SpawnOption{agentkit.WithMetadata(meta)}
		var pid agentkit.ProcessID
		var err error
		if st.Input.Strategy == model.JobStrategyPlanexec {
			input := st.Input.Planexec
			input.HandoffTool = nil
			pid, err = s.runtime.planexec.SpawnChild(ctx, sys, input, opts...)
		} else {
			pid, err = s.runtime.simple.SpawnChild(ctx, sys, st.Input.Main.Input, opts...)
		}
		if err != nil {
			return st, agentkit.Decision[cascadeOutput]{}, err
		}
		st.AdvisorID, st.Phase = pid, "advisor"
		return st, agentkit.Suspend[cascadeOutput](agentkit.WaitChildren("advisor", pid)), nil
	case "advisor":
		child, err := cascadeChild(sys, "advisor", st.AdvisorID)
		if err != nil {
			return st, agentkit.Decision[cascadeOutput]{}, err
		}
		if child.Status != agentkit.ProcessSucceeded {
			return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, "cascade advisor did not complete"), nil
		}
		if st.Input.Strategy == model.JobStrategyPlanexec {
			var out planexec.Output[planexec.TextResult]
			if err := json.Unmarshal(child.Output, &out); err != nil {
				return st, agentkit.Decision[cascadeOutput]{}, err
			}
			if out.Kind == planexec.OutputFallback || out.Kind == planexec.OutputQuestion || out.Kind == planexec.OutputHandoff {
				return st, agentkit.Fail[cascadeOutput](agentkit.FailureStrategyError, fallbackReason(out.FallbackReason)), nil
			}
		}
		return st, agentkit.Done(cascadeOutput{Accepted: st.AdvisorID, Advisor: true, Reason: st.Reason}), nil
	default:
		return st, agentkit.Decision[cascadeOutput]{}, goerr.New("unknown cascade phase")
	}
}

func cascadeChild(sys agentkit.Syscalls, key agentkit.AwaitKey, pid agentkit.ProcessID) (agentkit.ChildResult, error) {
	aw, ok := sys.Await(key)
	if !ok || aw.Status != agentkit.AwaitResponded || len(aw.Results) != 1 || aw.Results[0].ProcessID != pid {
		return agentkit.ChildResult{}, goerr.New("cascade child await is missing or inconsistent")
	}
	return aw.Results[0], nil
}

func (*cascadeStrategy) EncodeState(st cascadeState) ([]byte, error) { return json.Marshal(st) }
func (*cascadeStrategy) DecodeState(_ int, raw []byte) (cascadeState, error) {
	var st cascadeState
	err := json.Unmarshal(raw, &st)
	return st, err
}
func (*cascadeStrategy) EncodeOutput(out cascadeOutput) ([]byte, error) { return json.Marshal(out) }

func (d *DurableRuntime) registerCascade(reg *agentkit.Registry, limiter agentkit.Limiter) error {
	main, err := agentkit.Register(reg, agentkernel.AgentJobCascadeMain, 1, &cascadeMainStrategy{limiter: limiter},
		agentkit.WithHistoryStore[cascadeMainOutput](d.History))
	if err != nil {
		return err
	}
	d.cascadeMain = main
	controller, err := agentkit.Register(reg, agentkernel.AgentJobCascade, 1, &cascadeStrategy{runtime: d, limiter: limiter},
		agentkit.WithOnFinish(d.onCascadeFinish))
	if err != nil {
		return err
	}
	d.cascade = controller
	return nil
}

// Child stages reuse ordinary strategies, but only the logical root is allowed
// to close the JobRun or send its completion and reflection effects.
func cascadeChildProcess(proc *agentkit.Process) bool {
	sc := agentkernel.ScopeFrom(proc.Metadata)
	return proc.ParentID != nil && sc.JobExecution != nil && sc.JobExecution.Cascade
}

func (d *DurableRuntime) onCascadeFinish(ctx context.Context, pid agentkit.ProcessID, res agentkit.FinishResult[cascadeOutput]) error {
	if d.runner == nil {
		return goerr.New("durable runtime has no job runner")
	}
	proc, err := d.Kernel.GetProcess(ctx, pid)
	if err != nil {
		return err
	}
	sc := agentkernel.ScopeFrom(proc.Metadata)
	releaseUsage, loadErr := d.loadExecutionUsage(ctx, proc)
	defer releaseUsage()
	key := model.JobRunKey{WorkspaceID: sc.WorkspaceID, CaseID: sc.CaseID, JobID: sc.JobID}
	var runErr error
	if res.Status != agentkit.ProcessSucceeded || res.Output == nil || res.Output.Accepted == "" {
		runErr = failureError(res.Failure)
	}
	runErr = d.executionAccountingError(sc, errors.Join(runErr, loadErr))
	runtrace.FinishRun(ctx, d.runner.deps.Repo, key, sc.JobRunID, d.processUsage(sc, proc), runErr, d.runner.clock())
	j, c := d.reloadRunContext(ctx, sc)
	d.postCompletionMarker(ctx, sc, j, runErr)
	if runErr == nil {
		accepted, err := d.Kernel.GetProcess(ctx, res.Output.Accepted)
		if err != nil {
			return goerr.Wrap(err, "read accepted cascade history")
		}
		d.reflect(ctx, accepted, sc, j, c)
	}
	return nil
}
