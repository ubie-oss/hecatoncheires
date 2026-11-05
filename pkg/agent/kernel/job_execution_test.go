package kernel_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/gollem-dev/agentkit"
	procmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
)

func executionPolicy(mode model.JobExecutionMode) *model.JobExecution {
	return &model.JobExecution{
		Mode: mode, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob},
		Models: model.JobExecutionModels{Main: "main", Sub: "cheap", Advisor: "main"},
	}
}

func TestJobExecutionScopePinsPolicyAndRole(t *testing.T) {
	sc := kernel.Scope{WorkspaceID: "ws", JobID: "job", ToolSets: []string{"*"}, JobExecution: executionPolicy(model.JobExecutionModeStandard), StageRole: kernel.StageRoleAdvisor}
	meta := sc.Metadata()
	sc.JobExecution.Models.Advisor = "later-edit"
	sc.JobExecution.Cascade = true
	got := kernel.ScopeFrom(meta)
	if got.JobExecution.Cascade || got.JobExecution.Models.Advisor != "main" || got.StageRole != kernel.StageRoleAdvisor {
		gt.Bool(t, false).Describef("policy snapshot changed: %+v", got).True().Required()
	}
	child := kernel.ScopeFrom(kernel.WithStageRole(meta, kernel.StageRoleSub))
	if child.StageRole != kernel.StageRoleSub || got.StageRole != kernel.StageRoleAdvisor || child.JobID != got.JobID || !reflect.DeepEqual(child.JobExecution, got.JobExecution) {
		gt.Bool(t, false).Describe("child role discarded or changed the parent scope").True().Required()
	}
	if err := child.Validate(); err != nil {
		gt.NoError(t, err).Required()
	}
	child.JobID = ""
	if child.Validate() == nil {
		gt.Bool(t, false).Describe("policy without job was accepted").True().Required()
	}
	malformed := kernel.ScopeFrom(map[string]string{"job_id": "job", "toolsets": "*", "job_execution": "{invalid"})
	if malformed.JobExecution == nil || malformed.Validate() == nil {
		gt.Bool(t, false).Describe("malformed policy silently became a legacy run").True().Required()
	}
}

func TestJobExecutionModelSelectionIsScopedAndPriced(t *testing.T) {
	p, err := kernel.NewModelPolicy(validPolicyInput())
	if err != nil {
		gt.NoError(t, err).Required()
	}
	strategyRole := agentkit.DefineModelRole("test.execution")
	for _, tc := range []struct {
		name        string
		agent       agentkit.AgentName
		role        kernel.StageRole
		job         string
		defaultRole bool
	}{
		{"root", kernel.AgentJob, kernel.StageRoleMain, "job", true},
		{"task", kernel.AgentTask, kernel.StageRoleSub, "job", false},
		{"implicit task", kernel.AgentTask, "", "job", false},
		{"advisor task", kernel.AgentTask, kernel.StageRoleAdvisor, "job", false},
		{"other agent", kernel.AgentAssist, kernel.StageRoleSub, "job", false},
		{"other task", kernel.AgentTask, kernel.StageRoleSub, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := kernel.Scope{JobID: tc.job, JobExecution: executionPolicy(model.JobExecutionModeOrchestrator), StageRole: tc.role}
			var got agentkit.ModelRole
			h := kernel.ModelRoleHandlerForTest(p, func(_ context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
				got = req.Role
				return &agentkit.GenerateResult{}, nil
			})
			_, err := h(context.Background(), &agentkit.GenerateRequest{Effect: agentkit.EffectContext{Agent: tc.agent, Metadata: sc.Metadata()}, Role: strategyRole})
			if err != nil {
				gt.NoError(t, err).Required()
			}
			if tc.defaultRole {
				if got != nil {
					gt.Bool(t, false).Describe("explicit default did not select the positional model").True().Required()
				}
			} else if (tc.agent != kernel.AgentTask || tc.job == "") && got != strategyRole {
				gt.Bool(t, false).Describe("job execution leaked to an unrelated agent").True().Required()
			} else if tc.agent == kernel.AgentTask && tc.job != "" && got == strategyRole {
				gt.Bool(t, false).Describe("task model was not selected").True().Required()
			}
			proc := &agentkit.Process{Agent: tc.agent, Metadata: sc.Metadata()}
			wantRate := validPolicyInput().Defs[0].Rate
			if tc.agent == kernel.AgentTask && tc.job != "" {
				wantRate = validPolicyInput().Defs[1].Rate
			}
			if p.Resolve(proc).Rate != wantRate {
				gt.Bool(t, false).Describe("selected model and rate disagree").True().Required()
			}
		})
	}
}

func TestJobExecutionOrchestratorPalettesAreJobLocal(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	ctx = seedCase(t, ctx, repo, &model.Case{Title: "policy", SlackChannelID: "C-JOB"})
	deps := kernel.ToolDeps{Repo: repo, Registry: testRegistry(channelWorkspace()), SlackBot: stubSlackBot{}, ActionUC: stubActionMutator{}}
	factory, err := kernel.NewToolFactory(deps)
	if err != nil {
		gt.NoError(t, err).Required()
	}
	for _, tc := range []struct {
		name      string
		kind      agentkit.AgentName
		job       string
		policy    *model.JobExecution
		wantReads bool
	}{
		{"configured root", kernel.AgentJob, "job", executionPolicy(model.JobExecutionModeOrchestrator), false},
		{"legacy root", kernel.AgentJob, "job", nil, true},
		{"other job", kernel.AgentJob, "other", executionPolicy(model.JobExecutionModeStandard), true},
		{"simple root", kernel.AgentJobSimple, "job", executionPolicy(model.JobExecutionModeOrchestrator), true},
		{"cascade main", kernel.AgentJobCascadeMain, "job", executionPolicy(model.JobExecutionModeStandard), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, JobID: tc.job, JobExecution: tc.policy, ToolSets: []string{"*"}}
			tools, err := factory(ctx, newProcess(tc.kind, sc))
			if err != nil {
				gt.NoError(t, err).Required()
			}
			names := toolNames(tools)
			if tc.name == "configured root" {
				sc.StageRole = kernel.StageRoleAdvisor
				advisorTools, err := factory(ctx, newProcess(tc.kind, sc))
				gt.NoError(t, err).Required()
				gt.Array(t, advisorTools).Length(0)
			}
			if slices.Contains(names, "slack__get_messages") != tc.wantReads {
				gt.Bool(t, false).Describef("unexpected reads: %v", names).True().Required()
			}
		})
	}
	probe, err := kernel.NewToolSetProbe(deps)
	if err != nil {
		gt.NoError(t, err).Required()
	}
	sc := kernel.Scope{WorkspaceID: "ws-1", CaseID: 1, JobID: "job", JobExecution: executionPolicy(model.JobExecutionModeOrchestrator)}
	ids, err := probe.Available(ctx, sc, agent.KnownToolSetIDsJob)
	if err != nil {
		gt.NoError(t, err).Required()
	}
	if !slices.Contains(ids, agent.ToolSetCoreRO) || !slices.Contains(ids, agent.ToolSetCoreJob) {
		gt.Bool(t, false).Describef("incomplete vocabulary: %v", ids).True().Required()
	}
	names, err := probe.ReadOnlyNames(ctx, sc, agent.KnownToolSetIDsJob)
	if err != nil {
		gt.NoError(t, err).Required()
	}
	if !slices.Contains(names, "slack__get_messages") {
		gt.Bool(t, false).Describe("actual readonly names missing").True().Required()
	}
	noWriteTool(t, names)
}

func TestPinnedExecutionRemovedAliasesNeverFallBackToDefault(t *testing.T) {
	p, err := kernel.NewModelPolicy(validPolicyInput())
	if err != nil {
		gt.NoError(t, err).Required()
	}
	for _, role := range []kernel.StageRole{kernel.StageRoleMain, kernel.StageRoleSub, kernel.StageRoleAdvisor} {
		t.Run(string(role), func(t *testing.T) {
			execution := executionPolicy(model.JobExecutionModeStandard)
			switch role {
			case kernel.StageRoleMain:
				execution.Models.Main = "removed-main"
			case kernel.StageRoleSub:
				execution.Models.Sub = "removed-sub"
			case kernel.StageRoleAdvisor:
				execution.Models.Advisor = "removed-advisor"
			}
			sc := kernel.Scope{JobID: "job", JobRunID: "run", LLMModel: "cheap", JobExecution: execution, StageRole: role}
			called := false
			h := kernel.ModelRoleHandlerForTest(p, func(context.Context, *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
				called = true
				return &agentkit.GenerateResult{}, nil
			})
			_, err := h(context.Background(), &agentkit.GenerateRequest{Effect: agentkit.EffectContext{Agent: kernel.AgentJob, Metadata: sc.Metadata()}})
			if err == nil || called {
				gt.Bool(t, false).Describe("removed pinned alias selected a default client").True().Required()
			}
			if p.ModelName(sc) != "" {
				gt.Bool(t, false).Describe("removed pinned alias selected a default model name").True().Required()
			}
			if p.Resolve(&agentkit.Process{Agent: kernel.AgentJob, Metadata: sc.Metadata()}).Rate.IsPriced() {
				gt.Bool(t, false).Describe("removed pinned alias selected the default rate").True().Required()
			}
			if _, known := p.UsageCost(sc); known {
				gt.Bool(t, false).Describe("removed pinned alias was reported as known/free").True().Required()
			}
			remaining, _ := p.Remaining(sc.Metadata(), agentkit.Metrics{})
			if remaining != 0 {
				gt.Bool(t, false).Describe("removed pinned alias retained a spend allowance").True().Required()
			}
		})
	}
}

func TestCascadeWriteBarrierCoversDescendantsAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	toolsRepo := memory.New()
	ctx = seedCase(t, ctx, toolsRepo, &model.Case{Title: "policy", SlackChannelID: "C-JOB"})
	probe, err := kernel.NewToolSetProbe(kernel.ToolDeps{Repo: toolsRepo, Registry: testRegistry(channelWorkspace()), SlackBot: stubSlackBot{}, ActionUC: stubActionMutator{}})
	gt.NoError(t, err).Required()
	for _, tc := range []struct {
		name, tool  string
		role, owner kernel.StageRole
		want        bool
	}{
		{"main read", "slack__get_messages", kernel.StageRoleMain, kernel.StageRoleMain, false},
		{"main write", "core__create_action", kernel.StageRoleMain, kernel.StageRoleMain, true},
		{"nested sub write", "core__create_action", kernel.StageRoleSub, kernel.StageRoleMain, true},
		{"unknown tool", "unknown__operation", kernel.StageRoleSub, kernel.StageRoleMain, true},
		{"advisor sub write", "core__create_action", kernel.StageRoleSub, kernel.StageRoleAdvisor, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := procmemory.New()
			p, sc := durableAccountingFixture(t, repo)
			sc.WorkspaceID, sc.CaseID = "ws-1", 1
			sc.StageRole, sc.StageOwner = tc.role, tc.owner
			calls := 0
			handler := kernel.CascadeWriteMiddlewareForTest(p, probe)(func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
				calls++
				barrier, err := p.CascadeWriteStarted(ctx, "root")
				gt.NoError(t, err).Required()
				gt.Value(t, barrier).Equal(tc.want)
				return nil, nil
			})
			_, err := handler(ctx, &agentkit.ToolCallRequest{Effect: agentkit.EffectContext{ProcessID: "nested-task", RootID: "root", Agent: kernel.AgentTask, Metadata: sc.Metadata()}, Call: gollem.FunctionCall{Name: tc.tool}})
			gt.NoError(t, err).Required()
			gt.Number(t, calls).Equal(1)
			restarted, _ := durableAccountingFixture(t, repo)
			barrier, err := restarted.CascadeWriteStarted(ctx, "root")
			gt.NoError(t, err).Required()
			gt.Value(t, barrier).Equal(tc.want)
		})
	}
	repo := failingUsageRepository{Repository: procmemory.New(), fail: kernel.CascadeWriteEventForTest}
	p, sc := durableAccountingFixture(t, repo)
	sc.WorkspaceID, sc.CaseID = "ws-1", 1
	called := false
	handler := kernel.CascadeWriteMiddlewareForTest(p, probe)(func(context.Context, *agentkit.ToolCallRequest) (map[string]any, error) {
		called = true
		return nil, nil
	})
	_, err = handler(ctx, &agentkit.ToolCallRequest{Effect: agentkit.EffectContext{RootID: "root", Agent: kernel.AgentTask, Metadata: sc.Metadata()}, Call: gollem.FunctionCall{Name: "core__create_action"}})
	gt.Error(t, err)
	gt.Bool(t, called).False()
}

func TestAdvisorTaskModelDependsOnExecutionMode(t *testing.T) {
	for _, tc := range []struct {
		mode        model.JobExecutionMode
		wantDefault bool
	}{{model.JobExecutionModeStandard, true}, {model.JobExecutionModeOrchestrator, false}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			p, err := kernel.NewModelPolicy(validPolicyInput())
			gt.NoError(t, err).Required()
			sc := kernel.Scope{JobID: "job", StageRole: kernel.StageRoleAdvisor, JobExecution: executionPolicy(tc.mode)}
			requested := agentkit.DefineModelRole("task-role")
			var got agentkit.ModelRole
			h := kernel.ModelRoleHandlerForTest(p, func(_ context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
				got = req.Role
				return &agentkit.GenerateResult{}, nil
			})
			_, err = h(context.Background(), &agentkit.GenerateRequest{Role: requested, Effect: agentkit.EffectContext{Agent: kernel.AgentTask, Metadata: sc.Metadata()}})
			gt.NoError(t, err).Required()
			gt.Value(t, got == nil).Describef("mode=%s", tc.mode).Equal(tc.wantDefault)
			gt.Bool(t, got == requested).False()
			meta := kernel.WithStageRole(sc.Metadata(), kernel.StageRoleSub)
			child := kernel.ScopeFrom(meta)
			gt.Value(t, child.StageOwner).Equal(kernel.StageRoleAdvisor)
			gt.Value(t, sc.StageRole).Equal(kernel.StageRoleAdvisor)
		})
	}
}
