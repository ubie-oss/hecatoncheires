package kernel

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
)

// StageRole is the job-local model role persisted for one execution stage.
type StageRole string

const (
	StageRoleMain    StageRole = "main"
	StageRoleSub     StageRole = "sub"
	StageRoleAdvisor StageRole = "advisor"
)

// WithStageRole preserves the complete parent scope while selecting a child stage.
func WithStageRole(parent map[string]string, role StageRole) map[string]string {
	next := maps.Clone(parent)
	if next == nil {
		next = make(map[string]string)
	}
	if next[metaStageOwner] == "" && next[metaStageRole] == string(StageRoleAdvisor) {
		next[metaStageOwner] = string(StageRoleAdvisor)
	}
	if role == "" {
		delete(next, metaStageRole)
	} else {
		next[metaStageRole] = string(role)
		if role == StageRoleMain || role == StageRoleAdvisor {
			next[metaStageOwner] = string(role)
		}
	}
	return next
}

func (s Scope) hasJobExecution() bool { return s.JobID != "" && s.JobExecution != nil }

func isJobExecutionAgent(name agentkit.AgentName) bool {
	switch name {
	case AgentJob, AgentJobSimple, AgentJobCascade, AgentJobCascadeMain, AgentTask:
		return true
	default:
		return false
	}
}

func executionScopeForAgent(sc Scope, name agentkit.AgentName) Scope {
	if !isJobExecutionAgent(name) {
		sc.JobExecution, sc.StageRole, sc.StageOwner = nil, "", ""
		return sc
	}
	if sc.hasJobExecution() && sc.StageOwner == "" {
		sc.StageOwner = stageOwner(sc)
	}
	if name == AgentTask && sc.hasJobExecution() && (sc.StageRole == "" || sc.StageRole == StageRoleMain || (sc.StageRole == StageRoleAdvisor && sc.JobExecution.Mode == model.JobExecutionModeOrchestrator)) {
		sc.StageRole = StageRoleSub
	}
	return sc
}

func (s Scope) executionModelRef() string {
	if !s.hasJobExecution() {
		return s.LLMModel
	}
	models := s.JobExecution.Models
	main := models.Main
	if main == "" {
		main = s.LLMModel
	}
	switch s.StageRole {
	case StageRoleSub:
		if models.Sub != "" {
			return models.Sub
		}
	case StageRoleAdvisor:
		return models.Advisor
	}
	return main
}

func (s Scope) delegatesJobReads() bool {
	return s.hasJobExecution() && s.JobExecution.Mode == model.JobExecutionModeOrchestrator
}

func (s Scope) offersReadOnlyJobSets() bool {
	return s.hasJobExecution() && (s.JobExecution.Mode == model.JobExecutionModeOrchestrator || s.JobExecution.Cascade)
}

func stageOwner(sc Scope) StageRole {
	if sc.StageOwner != "" {
		return sc.StageOwner
	}
	if sc.StageRole == StageRoleAdvisor {
		return StageRoleAdvisor
	}
	return StageRoleMain
}

const cascadeWriteEvent agentkit.EventType = "hecatoncheires.cascade.write_attempt"

// CascadeWriteStarted reports persisted write attempts anywhere in the main
// stage's subtree. It reads shared storage so a restarted controller sees the
// barrier before deciding whether it may start an advisor.
func (p ModelPolicy) CascadeWriteStarted(ctx context.Context, rootID agentkit.ProcessID) (bool, error) {
	if p.durableUsage == nil {
		return false, goerr.New("cascade accounting is unavailable")
	}
	p.durableUsage.mu.RLock()
	repo := p.durableUsage.repo
	p.durableUsage.mu.RUnlock()
	if repo == nil {
		return false, goerr.New("cascade repository is unavailable")
	}
	events, err := repo.ListEvents(ctx, rootID, agentkit.EventQuery{})
	if err != nil {
		return false, goerr.Wrap(err, "read cascade write barrier")
	}
	for _, event := range events {
		if event.Type == cascadeWriteEvent {
			return true, nil
		}
	}
	return false, nil
}

func cascadeWriteMiddleware(p ModelPolicy, probe *ToolSetProbe) agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			sc := executionScopeForAgent(ScopeFrom(req.Effect.Metadata), req.Effect.Agent)
			if !sc.hasJobExecution() || !sc.JobExecution.Cascade || stageOwner(sc) == StageRoleAdvisor {
				return next(ctx, req)
			}
			names, err := probe.ReadOnlyNames(ctx, sc, agent.KnownToolSetIDsJob)
			if err != nil {
				return nil, goerr.Wrap(err, "resolve cascade read tools")
			}
			if !slices.Contains(names, req.Call.Name) {
				p.durableUsage.mu.RLock()
				repo := p.durableUsage.repo
				p.durableUsage.mu.RUnlock()
				if repo == nil {
					return nil, goerr.New("cascade repository is unavailable")
				}
				event := &agentkit.Event{ID: agentkit.EventID(uuid.Must(uuid.NewV7()).String()), ProcessID: req.Effect.RootID, Type: cascadeWriteEvent, At: time.Now()}
				if err := repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{event}}); err != nil {
					return nil, goerr.Wrap(err, "persist cascade write attempt")
				}
			}
			return next(ctx, req)
		}
	}
}
