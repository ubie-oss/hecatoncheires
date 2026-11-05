package job_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"time"

	"github.com/gollem-dev/agentkit"
	procmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
)

type cascadeTestTool struct {
	name   string
	err    error
	result map[string]any
	calls  atomic.Int32
	before func()
}

func (t *cascadeTestTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{Name: t.name, Description: "test"}
}
func (t *cascadeTestTool) Run(context.Context, map[string]any) (map[string]any, error) {
	if t.before != nil {
		t.before()
	}
	t.calls.Add(1)
	return t.result, t.err
}

func cascadeCalls(names ...string) *gollem.Response {
	r := &gollem.Response{InputToken: 10, OutputToken: 5}
	for _, name := range names {
		args := map[string]any{}
		if name == job.CascadeEscalationToolNameForTest {
			args["reason"] = "conflict"
		}
		r.FunctionCalls = append(r.FunctionCalls, &gollem.FunctionCall{ID: name, Name: name, Arguments: args})
	}
	return r
}
func cascadeText(s string) *gollem.Response {
	return &gollem.Response{Texts: []string{s}, InputToken: 10, OutputToken: 5}
}

func TestCascadeRoutesBeforeWritesAndHoldsAfterWrites(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		responses                []*gollem.Response
		readErr, writeErr        error
		readResult               map[string]any
		readOnly                 []string
		wantWrites               int32
		wantAdvisor, wantSuccess bool
		strategy                 model.JobStrategy
	}{
		{name: "main completes", responses: []*gollem.Response{cascadeCalls("read", "write"), cascadeText("done")}, readOnly: []string{"read"}, wantWrites: 1, wantSuccess: true},
		{name: "fresh advisor drops pending main write", responses: []*gollem.Response{cascadeCalls("read", job.CascadeEscalationToolNameForTest, "write"), cascadeCalls("write"), cascadeText("advisor done")}, readOnly: []string{"read"}, wantWrites: 1, wantAdvisor: true, wantSuccess: true},
		{name: "cannot escalate after write", responses: []*gollem.Response{cascadeCalls("write", job.CascadeEscalationToolNameForTest)}, wantWrites: 1},
		{name: "read service failure holds", responses: []*gollem.Response{cascadeCalls("read", "write")}, readOnly: []string{"read"}, readErr: errors.New("source unavailable")},
		{name: "structured read failure holds", responses: []*gollem.Response{cascadeCalls("read", "write")}, readOnly: []string{"read"}, readResult: map[string]any{"success": false}},
		{name: "partial bulk read failure holds", responses: []*gollem.Response{cascadeCalls("read", "write")}, readOnly: []string{"read"}, readResult: map[string]any{"results": []map[string]any{{"messages": []any{}}, {"error": "permission denied"}}}},
		{name: "uncertain write holds without repeat", responses: []*gollem.Response{cascadeCalls("write")}, writeErr: errors.New("response lost"), wantWrites: 1},
		{name: "unclassified tool has write barrier", responses: []*gollem.Response{cascadeCalls("read", job.CascadeEscalationToolNameForTest)}},
		{name: "original planexec advisor", strategy: model.JobStrategyPlanexec, responses: []*gollem.Response{cascadeCalls(job.CascadeEscalationToolNameForTest), cascadeText(`{"message":"nothing to update","finalize":{"reason":"checked"}}`), cascadeText(`{"text":"advisor done"}`)}, wantAdvisor: true, wantSuccess: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			read := &cascadeTestTool{name: "read", err: tt.readErr, result: tt.readResult}
			write := &cascadeTestTool{name: "write", err: tt.writeErr}
			var generations atomic.Int32
			var advisorFirstHistory atomic.Int32
			llm := &mock.LLMClientMock{NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
				cfg := gollem.NewSessionConfig(opts...)
				if !strings.Contains(cfg.SystemPrompt(), "[Cascade execution]") && advisorFirstHistory.CompareAndSwap(0, 1) {
					if h := cfg.History(); h != nil && len(h.Messages) > 0 {
						advisorFirstHistory.Store(2)
					}
				}
				return &mock.SessionMock{
					GenerateFunc: func(context.Context, []gollem.Input, ...gollem.GenerateOption) (*gollem.Response, error) {
						i := int(generations.Add(1)) - 1
						if i >= len(tt.responses) {
							return nil, errors.New("unexpected generate")
						}
						return tt.responses[i], nil
					},
					HistoryFunc: func() (*gollem.History, error) {
						return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
					},
				}, nil
			}}
			reg := agentkit.NewRegistry()
			history := agentarchive.NewMemoryHistoryStore()
			d := &job.DurableRuntime{History: history}
			limiter := func(_ context.Context, _ *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
				if m.Steps >= 100 {
					return agentkit.LimitStop("test step cap")
				}
				return agentkit.LimitPass()
			}
			controller, err := job.RegisterCascadeForTest(reg, d, limiter)
			gt.NoError(t, err).Required()
			repo := procmemory.New()
			k, err := agentkit.New(repo, llm, reg, agentkit.WithToolFactory(func(context.Context, *agentkit.Process) ([]gollem.Tool, error) {
				return []gollem.Tool{read, write}, nil
			}))
			gt.NoError(t, err).Required()
			d.Kernel = k
			strategy := model.NormaliseJobStrategy(tt.strategy)
			sc := agentkernel.Scope{WorkspaceID: "ws", CaseID: 1, JobID: "job", JobRunID: "run", ToolSets: []string{"*"}, JobExecution: &model.JobExecution{Mode: model.JobExecutionModeStandard, Cascade: true, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob, model.JobExecutionTargetJobSimple}, Models: model.JobExecutionModels{Main: "main", Advisor: "advisor"}}}
			pid, err := controller.Spawn(ctx, k, job.CascadeInputForTest{Main: job.CascadeMainInputForTest{Input: react.Input{SystemPrompt: "original system", Prompt: "original request"}, ReadOnlyNames: tt.readOnly}, Strategy: strategy, Planexec: planexec.Input{SystemPrompt: "original system", UserInput: "original request", KnownToolIDs: []string{"core_ro"}}}, agentkit.WithMetadata(sc.Metadata()))
			gt.NoError(t, err).Required()
			// Inspect persisted state from INSIDE the effect. An in-memory boolean
			// set immediately before Run would not satisfy this assertion.
			write.before = func() {
				root, err := k.GetProcess(ctx, pid)
				if err != nil {
					gt.NoError(t, err)
					return
				}
				var st job.CascadeStateForTest
				if err := json.Unmarshal(root.State, &st); err != nil {
					gt.NoError(t, err)
					return
				}
				if st.AdvisorID != "" {
					return
				}
				main, err := k.GetProcess(ctx, st.MainID)
				if err != nil {
					gt.NoError(t, err)
					return
				}
				var mst job.CascadeMainStateForTest
				if err := json.Unmarshal(main.State, &mst); err != nil {
					gt.NoError(t, err)
					return
				}
				if !mst.WriteStarted {
					gt.Bool(t, mst.WriteStarted).True()
				}
			}
			served := make(chan error, 1)
			async.DispatchCancelable(ctx, func(ctx context.Context) error {
				served <- agentkernel.Serve(ctx, k, agentkit.WithPollInterval(time.Millisecond))
				return nil
			})
			var proc *agentkit.Process
			for {
				proc, err = k.GetProcess(ctx, pid)
				gt.NoError(t, err).Required()
				if proc.Status.Terminal() {
					break
				}
				select {
				case <-ctx.Done():
					gt.NoError(t, ctx.Err()).Required()
				case <-time.After(time.Millisecond):
				}
			}
			cancel()
			if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
				gt.NoError(t, err)
			}
			async.Wait()
			gt.Value(t, proc.Status == agentkit.ProcessSucceeded).Equal(tt.wantSuccess)
			gt.Value(t, write.calls.Load()).Equal(tt.wantWrites)
			gt.Value(t, int(generations.Load())).Equal(len(tt.responses))
			var st job.CascadeStateForTest
			gt.NoError(t, json.Unmarshal(proc.State, &st)).Required()
			gt.Value(t, st.AdvisorID != "").Equal(tt.wantAdvisor)
			if tt.wantAdvisor {
				if advisorFirstHistory.Load() != 1 {
					gt.Value(t, advisorFirstHistory.Load()).Equal(int32(1))
				}
				advisor, err := k.GetProcess(context.Background(), st.AdvisorID)
				gt.NoError(t, err).Required()
				if agentkernel.ScopeFrom(advisor.Metadata).StageRole != agentkernel.StageRoleAdvisor {
					gt.Value(t, agentkernel.ScopeFrom(advisor.Metadata).StageRole).Equal(agentkernel.StageRoleAdvisor)
				}
				if advisor.InheritedHistory != nil {
					gt.Value(t, advisor.InheritedHistory).Nil()
				}
			}
		})
	}
}
