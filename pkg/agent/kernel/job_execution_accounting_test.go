package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"strings"
	"testing"

	"github.com/gollem-dev/agentkit"
	procmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

func durableAccountingFixture(t *testing.T, repo agentkit.Repository) (kernel.ModelPolicy, kernel.Scope) {
	t.Helper()
	p, err := kernel.NewModelPolicy(kernel.ModelPolicyInput{Defs: []kernel.ModelDef{
		{Ref: "main", Provider: kernel.ProviderClaude, Model: "main-model", Rate: pricing.Rate{Input: 10, Output: 50}},
		{Ref: "sub", Provider: kernel.ProviderGemini, Model: "sub-model", Rate: pricing.Rate{Input: 1, Output: 5}},
	}, DefaultRef: "main", DefaultBudget: pricing.FromUSD(1), Clients: map[string]gollem.LLMClient{"sub": &mock.LLMClientMock{}}})
	if err != nil {
		gt.NoError(t, err).Required()
	}
	kernel.BindUsageForTest(p, repo)
	sc := kernel.Scope{WorkspaceID: "ws", JobID: "job", JobRunID: "run", ToolSets: []string{"*"}, StageRole: kernel.StageRoleMain, JobExecution: &model.JobExecution{Mode: model.JobExecutionModeStandard, Cascade: true, ApplyTo: []model.JobExecutionTarget{model.JobExecutionTargetJob}, Models: model.JobExecutionModels{Main: "main", Sub: "sub", Advisor: "main"}}}
	return p, sc
}

func seedUsageRoot(t *testing.T, repo agentkit.Repository, sc kernel.Scope) {
	t.Helper()
	if err := repo.Apply(context.Background(), agentkit.ChangeSet{Processes: []*agentkit.Process{{ID: "root", RootID: "root", Agent: kernel.AgentJobCascade, Metadata: sc.Metadata(), Status: agentkit.ProcessPending}}}); err != nil {
		gt.NoError(t, err).Required()
	}
}

func usageResponseClient(response *gollem.Response, providerErr error, calls *int) gollem.LLMClient {
	return &mock.LLMClientMock{NewSessionFunc: func(context.Context, ...gollem.SessionOption) (gollem.Session, error) {
		return &mock.SessionMock{GenerateFunc: func(context.Context, []gollem.Input, ...gollem.GenerateOption) (*gollem.Response, error) {
			*calls++
			return response, providerErr
		}}, nil
	}}
}

func generateUsage(t *testing.T, p kernel.ModelPolicy, sc kernel.Scope, ref, pid string, ancestors []string, client gollem.LLMClient) error {
	t.Helper()
	session, err := p.WrapClient(ref, client).NewSession(context.Background())
	if err != nil {
		gt.NoError(t, err).Required()
	}
	effect := agentkit.EffectContext{ProcessID: agentkit.ProcessID(pid), RootID: "root", Agent: kernel.AgentTask, Metadata: sc.Metadata(), StateSeq: 3}
	ctx := context.WithValue(context.Background(), kernel.DurableAncestorsKeyForTest{}, ancestors)
	c := kernel.DurableUsageForEffectForTest(ctx, effect, "child")
	_, err = session.Generate(context.WithValue(ctx, kernel.DurableUsageContextKeyForTest{}, c), nil)
	return err
}

func TestDurableJobUsageMixedPricesRestartAndInclusiveCosts(t *testing.T) {
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	if err := kernel.LoadUsageForTest(context.Background(), p, "root"); err != nil {
		gt.NoError(t, err).Required()
	}
	before, err := repo.GetProcess(context.Background(), "root")
	if err != nil {
		gt.NoError(t, err).Required()
	}
	calls := 0
	raw := usageResponseClient(&gollem.Response{InputToken: 100, OutputToken: 10}, nil, &calls)
	sub := sc
	sub.StageRole = kernel.StageRoleSub
	// Repeated physical SDK attempts at the same state sequence each cost money.
	for range 2 {
		if err := generateUsage(t, p, sub, "sub", "task", []string{"root"}, raw); err != nil {
			gt.NoError(t, err).Required()
		}
	}
	advisor := sc
	advisor.StageRole = kernel.StageRoleAdvisor
	if err := generateUsage(t, p, advisor, "main", "advisor-task", []string{"advisor", "root"}, raw); err != nil {
		gt.NoError(t, err).Required()
	}
	advisorSub := advisor
	advisorSub.StageRole, advisorSub.StageOwner = kernel.StageRoleSub, kernel.StageRoleAdvisor
	gt.NoError(t, generateUsage(t, p, advisorSub, "sub", "advisor-sub", []string{"advisor", "root"}, raw)).Required()
	want := pricing.NanoUSD(3*(100+10*5) + (100*10 + 10*50))
	if cost, known := p.UsageCost(sc); !known || cost != want {
		gt.Bool(t, false).Describef("mixed total=%d known=%t", cost, known).True().Required()
	}
	if cost, known := p.UsageCost(advisor); !known || cost != 1650 {
		gt.Bool(t, false).Describef("advisor stage=%d known=%t", cost, known).True().Required()
	}
	for _, tc := range []struct {
		pid  string
		want pricing.NanoUSD
	}{{"root", want}, {"task", 300}, {"advisor", 1650}, {"advisor-task", 1500}} {
		cost, known := kernel.ProcessCostForTest(p, "root", tc.pid)
		if !known || cost != tc.want {
			gt.Bool(t, false).Describef("inclusive %s=%d known=%t", tc.pid, cost, known).True().Required()
		}
	}
	after, _ := repo.GetProcess(context.Background(), "root")
	if after.Rev != before.Rev {
		gt.Bool(t, false).Describe("billing event changed Process.Rev").True().Required()
	}
	events, _ := repo.ListEvents(context.Background(), "root", agentkit.EventQuery{})
	if len(events) != 8 || calls != 4 {
		gt.Bool(t, false).Describef("attempts/events=%d/%d", calls, len(events)).True().Required()
	}
	// A completely new policy restores the per-call prices, including descendants.
	restarted, _ := durableAccountingFixture(t, repo)
	if err := kernel.LoadUsageForTest(context.Background(), restarted, "root"); err != nil {
		gt.NoError(t, err).Required()
	}
	if cost, known := restarted.UsageCost(sc); !known || cost != want {
		gt.Bool(t, false).Describef("restored total=%d known=%t", cost, known).True().Required()
	}
	advisorCost, advisorKnown := restarted.UsageCost(advisor)
	gt.Value(t, advisorCost).Equal(pricing.NanoUSD(1650))
	gt.Bool(t, advisorKnown).True()
	limit := restarted.Resolve(&agentkit.Process{ID: "root", RootID: "root", Agent: kernel.AgentJobCascade, Metadata: sc.Metadata()})
	if limit.Spent == nil || *limit.Spent != want {
		gt.Bool(t, false).Describef("root measured money=%+v", limit).True().Required()
	}
	// A colliding run ID in another workspace is not this run's billing scope.
	other := sc
	other.WorkspaceID = "other"
	if cost, _ := restarted.UsageCost(other); cost != 0 {
		gt.Bool(t, false).Describe("workspace usage leaked").True().Required()
	}
}

func TestDurableJobUsageUnknownStopsAndDoesNotBecomeFree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *gollem.Response
		pending  bool
	}{{"provider returned no usage", nil, false}, {"zero usage is ambiguous", &gollem.Response{}, false}, {"worker died after dispatch", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := procmemory.New()
			p, sc := durableAccountingFixture(t, repo)
			seedUsageRoot(t, repo, sc)
			if err := kernel.LoadUsageForTest(context.Background(), p, "root"); err != nil {
				gt.NoError(t, err).Required()
			}
			if tc.pending {
				c := kernel.DurableUsageRecordForTest{AttemptID: "interrupted", WorkspaceID: sc.WorkspaceID, JobID: sc.JobID}
				c.RunID, c.RootID, c.ProcessID = sc.JobRunID, "root", "root"
				if err := kernel.AppendUsageForTest(context.Background(), p, c); err != nil {
					gt.NoError(t, err).Required()
				}
			} else {
				calls := 0
				_ = generateUsage(t, p, sc, "main", "root", nil, usageResponseClient(tc.response, errors.New("provider failure"), &calls))
			}
			restarted, _ := durableAccountingFixture(t, repo)
			if err := kernel.LoadUsageForTest(context.Background(), restarted, "root"); err != nil {
				gt.NoError(t, err).Required()
			}
			if _, known := restarted.UsageCost(sc); known {
				gt.Bool(t, false).Describe("unknown usage became free").True().Required()
			}
			limit := restarted.Resolve(&agentkit.Process{ID: "root", RootID: "root", Agent: kernel.AgentJobCascade, Metadata: sc.Metadata()})
			if limit.Validate() == nil {
				gt.Bool(t, false).Describe("unknown usage did not stop the budget").True().Required()
			}
			remaining, _ := restarted.Remaining(sc.Metadata(), agentkit.Metrics{})
			if remaining != 0 {
				gt.Bool(t, false).Describe("unknown usage was given spending allowance").True().Required()
			}
		})
	}
}

type failingUsageRepository struct {
	agentkit.Repository
	fail agentkit.EventType
}

func (r failingUsageRepository) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	for _, event := range cs.Events {
		if event.Type == r.fail {
			return errors.New("billing persistence unavailable")
		}
	}
	return r.Repository.Apply(ctx, cs)
}

func TestDurableJobUsagePersistenceBracketsProviderCall(t *testing.T) {
	for _, typ := range []agentkit.EventType{kernel.UsagePendingEventForTest, kernel.UsageCompletedEventForTest} {
		t.Run(string(typ), func(t *testing.T) {
			base := procmemory.New()
			repo := failingUsageRepository{Repository: base, fail: typ}
			p, sc := durableAccountingFixture(t, repo)
			seedUsageRoot(t, repo, sc)
			if err := kernel.LoadUsageForTest(context.Background(), p, "root"); err != nil {
				gt.NoError(t, err).Required()
			}
			calls := 0
			if err := generateUsage(t, p, sc, "main", "root", nil, usageResponseClient(&gollem.Response{InputToken: 10, OutputToken: 5}, nil, &calls)); err == nil {
				gt.Bool(t, false).Describe("persistence failure was ignored").True().Required()
			}
			wantCalls := 0
			if typ == kernel.UsageCompletedEventForTest {
				wantCalls = 1
			}
			if calls != wantCalls {
				gt.Bool(t, false).Describef("SDK calls=%d want=%d", calls, wantCalls).True().Required()
			}
			if typ == kernel.UsageCompletedEventForTest {
				if _, known := p.UsageCost(sc); known {
					gt.Bool(t, false).Describe("lost provider charge became known/free").True().Required()
				}
			}
		})
	}
}

func TestDurableJobUsageLeavesLegacyCallsUnchanged(t *testing.T) {
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	calls := 0
	s, err := p.WrapClient("main", usageResponseClient(&gollem.Response{InputToken: 10}, nil, &calls)).NewSession(context.Background())
	if err != nil {
		gt.NoError(t, err).Required()
	}
	if _, err := s.Generate(context.Background(), nil); err != nil {
		gt.NoError(t, err).Required()
	}
	events, _ := repo.ListEvents(context.Background(), "root", agentkit.EventQuery{})
	if calls != 1 || len(events) != 0 {
		gt.Bool(t, false).Describe("legacy call entered durable job accounting").True().Required()
	}
}

func TestScopedJobUsageDropsOnlyTheCacheAndRequiresRestore(t *testing.T) {
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	if err := kernel.LoadUsageForTest(context.Background(), p, "root"); err != nil {
		gt.NoError(t, err).Required()
	}
	calls := 0
	if err := generateUsage(t, p, sc, "main", "root", nil, usageResponseClient(&gollem.Response{InputToken: 10, OutputToken: 5}, nil, &calls)); err != nil {
		gt.NoError(t, err).Required()
	}
	release, err := p.LoadUsage(context.Background(), "root")
	gt.NoError(t, err).Required()
	release()
	release()
	if cost, known := p.UsageCost(sc); cost != 0 || known {
		gt.Bool(t, false).Describe("evicted usage was reported as free/known").True().Required()
	}
	if !kernel.UsageCacheEmptyForTest(p) {
		gt.Bool(t, false).Describe("completed run leaked its usage cache").True().Required()
	}
	events, _ := repo.ListEvents(context.Background(), "root", agentkit.EventQuery{})
	if len(events) != 2 {
		gt.Bool(t, false).Describe("cache eviction deleted durable billing evidence").True().Required()
	}
	if err := kernel.LoadUsageForTest(context.Background(), p, "root"); err != nil {
		gt.NoError(t, err).Required()
	}
	if cost, known := p.UsageCost(sc); cost != 350 || !known {
		gt.Bool(t, false).Describe("evicted billing evidence could not be restored").True().Required()
	}
}

func TestDurableToolUsageKeepsIndependentEffectAttribution(t *testing.T) {
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	if err := kernel.LoadUsageForTest(context.Background(), p, "root"); err != nil {
		gt.NoError(t, err).Required()
	}
	calls := 0
	session, err := p.WrapClient("main", usageResponseClient(&gollem.Response{InputToken: 10, OutputToken: 5}, nil, &calls)).NewSession(context.Background())
	if err != nil {
		gt.NoError(t, err).Required()
	}
	h := kernel.DurableUsageToolMiddlewareForTest(p)(func(ctx context.Context, _ *agentkit.ToolCallRequest) (map[string]any, error) {
		_, err := session.Generate(ctx, nil)
		return nil, err
	})
	_, err = h(context.Background(), &agentkit.ToolCallRequest{Effect: agentkit.EffectContext{ProcessID: "root", RootID: "root", Agent: kernel.AgentJobSimple, Metadata: sc.Metadata()}, Call: gollem.FunctionCall{Name: "webfetch__fetch"}})
	if err != nil {
		gt.NoError(t, err).Required()
	}
	if total, known := p.UsageCost(sc); !known || total != 350 || calls != 1 {
		gt.Bool(t, false).Describef("tool usage=%d known=%t calls=%d", total, known, calls).True().Required()
	}
	events, _ := repo.ListEvents(context.Background(), "root", agentkit.EventQuery{})
	var c kernel.DurableUsageRecordForTest
	if err := json.Unmarshal(events[1].Payload, &c); err != nil {
		gt.NoError(t, err).Required()
	}
	if c.Kind != "internal" || c.ToolName != "webfetch__fetch" || c.StageRole != kernel.StageRoleMain {
		gt.Bool(t, false).Describef("tool attribution=%+v", c).True().Required()
	}
}

func TestDurableUsageWrapperIdempotencyAndPartialFailure(t *testing.T) {
	ctx := context.Background()
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	gt.NoError(t, kernel.LoadUsageForTest(ctx, p, "root")).Required()
	calls := 0
	providerErr := errors.New("provider partial failure")
	wrapped := p.WrapClient("main", usageResponseClient(&gollem.Response{InputToken: 100, OutputToken: 10, CacheReadInputToken: 20, CacheCreationInputToken: 10}, providerErr, &calls))
	gt.Bool(t, p.WrapClient("main", wrapped) == wrapped).True()
	gt.Value(t, p.WrapClient("main", nil)).Nil()
	session, err := wrapped.NewSession(ctx)
	gt.NoError(t, err).Required()
	h := kernel.DurableUsageToolMiddlewareForTest(p)(func(ctx context.Context, _ *agentkit.ToolCallRequest) (map[string]any, error) {
		_, err := session.Generate(ctx, []gollem.Input{gollem.Text("content must not be retained")})
		return nil, err
	})
	ctx = context.WithValue(ctx, kernel.UsagePhaseKeyForTest{}, "tool")
	_, err = h(ctx, &agentkit.ToolCallRequest{Effect: agentkit.EffectContext{ProcessID: "child", RootID: "root", Agent: kernel.AgentTask, StateSeq: 2, Metadata: sc.Metadata()}, Call: gollem.FunctionCall{Name: "webfetch__fetch"}})
	gt.Error(t, err).Is(providerErr)
	gt.Number(t, calls).Equal(1)
	events, err := repo.ListEvents(ctx, "root", agentkit.EventQuery{})
	gt.NoError(t, err).Required()
	gt.Array(t, events).Length(2).Required()
	var record kernel.DurableUsageRecordForTest
	gt.NoError(t, json.Unmarshal(events[1].Payload, &record)).Required()
	gt.Bool(t, record.Complete).True()
	gt.Bool(t, record.UsageKnown).True()
	gt.Bool(t, record.Failed).True()
	gt.Value(t, record.Kind).Equal("internal")
	gt.Value(t, record.ParentOrChild).Equal("child")
	gt.Value(t, record.Phase).Equal("tool")
	gt.Value(t, record.ToolName).Equal("webfetch__fetch")
	gt.Value(t, record.StateSeq).Equal(2)
	gt.Value(t, record.CostNanoUSD).Equal(pricing.NanoUSD(1200))
	gt.Value(t, record.CacheReadInputTokens).Equal(int64(20))
	gt.Value(t, record.CacheCreationInputTokens).Equal(int64(10))
	gt.Bool(t, strings.Contains(string(events[1].Payload), "content must not be retained")).False()
}

func TestDurableUsageAttributionAndMissingRunFailClosed(t *testing.T) {
	sc := kernel.Scope{JobRunID: "run"}
	record := kernel.UsageForEffectForTest(context.Background(), agentkit.EffectContext{ProcessID: "root", RootID: "root", Agent: kernel.AgentJob, Metadata: sc.Metadata(), StateSeq: 4}, "parent")
	gt.Value(t, record.Phase).Equal("unknown")
	gt.Value(t, record.RunID).Equal("run")
	gt.Value(t, record.ProcessID).Equal("root")
	gt.Value(t, record.ParentOrChild).Equal("parent")
	gt.Value(t, record.StateSeq).Equal(4)
	p, sc := durableAccountingFixture(t, procmemory.New())
	sc.JobRunID = ""
	called := false
	h := kernel.DurableUsageGenerateMiddlewareForTest(p)(func(context.Context, *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
		called = true
		return nil, nil
	})
	_, err := h(context.Background(), &agentkit.GenerateRequest{Effect: agentkit.EffectContext{Agent: kernel.AgentJob, Metadata: sc.Metadata()}})
	gt.Error(t, err)
	gt.Bool(t, called).False()
}

func TestScopedUsagePinsOverlapAndReleaseIndependently(t *testing.T) {
	ctx := context.Background()
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	first, err := p.LoadUsage(ctx, "root")
	gt.NoError(t, err).Required()
	calls := 0
	gt.NoError(t, generateUsage(t, p, sc, "main", "root", nil, usageResponseClient(&gollem.Response{InputToken: 10, OutputToken: 5}, nil, &calls))).Required()
	sibling, err := p.LoadUsage(ctx, "root")
	gt.NoError(t, err).Required()
	first()
	first()
	cost, known := p.UsageCost(sc)
	gt.Value(t, cost).Equal(pricing.NanoUSD(350))
	gt.Bool(t, known).True()
	gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).False()
	sibling()
	gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).True()
	_, known = p.UsageCost(sc)
	gt.Bool(t, known).False()
	// A completion callback on an instance with no active claim restores facts.
	callback, err := p.LoadUsage(ctx, "root")
	gt.NoError(t, err).Required()
	cost, known = p.UsageCost(sc)
	gt.Value(t, cost).Equal(pricing.NanoUSD(350))
	gt.Bool(t, known).True()
	callback()
	gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).True()
}

func TestScopedUsageEvictsAfterRefusalFailureAndRestoreError(t *testing.T) {
	ctx := context.Background()
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	release, err := p.LoadUsage(ctx, "missing-root")
	gt.Error(t, err)
	gt.Value(t, release).Nil()
	gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).True()
	claimErr := errors.New("claim failed")
	for _, expectedErr := range []error{nil, claimErr} {
		handler := kernel.DurableUsageClaimMiddlewareForTest(p)(func(context.Context, *agentkit.ClaimRequest) (agentkit.ClaimOutcome, error) {
			_, known := p.UsageCost(sc)
			gt.Bool(t, known).True()
			return agentkit.ClaimRefused, expectedErr
		})
		outcome, err := handler(ctx, &agentkit.ClaimRequest{Process: &agentkit.Process{ID: "root", RootID: "root", Agent: kernel.AgentJobCascade, Metadata: sc.Metadata()}})
		gt.Value(t, outcome).Equal(agentkit.ClaimRefused)
		if expectedErr != nil {
			gt.Error(t, err).Is(expectedErr)
		} else {
			gt.NoError(t, err)
		}
		gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).True()
	}
}

type panicUsageRepository struct{ agentkit.Repository }

func (r panicUsageRepository) GetProcess(context.Context, agentkit.ProcessID) (*agentkit.Process, error) {
	panic("repository panic")
}

func TestScopedUsageReleasesOnRestoreAndClaimPanics(t *testing.T) {
	p, _ := durableAccountingFixture(t, panicUsageRepository{Repository: procmemory.New()})
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = p.LoadUsage(context.Background(), "root")
	}()
	gt.Bool(t, panicked).True()
	gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).True()
	repo := procmemory.New()
	p, sc := durableAccountingFixture(t, repo)
	seedUsageRoot(t, repo, sc)
	handler := kernel.DurableUsageClaimMiddlewareForTest(p)(func(context.Context, *agentkit.ClaimRequest) (agentkit.ClaimOutcome, error) { panic("claim panic") })
	panicked = false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = handler(context.Background(), &agentkit.ClaimRequest{Process: &agentkit.Process{ID: "root", RootID: "root", Agent: kernel.AgentJobCascade, Metadata: sc.Metadata()}})
	}()
	gt.Bool(t, panicked).True()
	gt.Bool(t, kernel.UsageCacheEmptyForTest(p)).True()
}
