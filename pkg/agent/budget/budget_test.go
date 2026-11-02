package budget_test

import (
	"context"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

func validConfig() budget.Config {
	return budget.Config{MaxSteps: 10, MaxInputTokens: 1000, MaxOutputTokens: 200, NoticeRatio: 0.8}
}

// opusRate is $5 / $25 / $0.50 / $6.25 per MTok, in NanoUSD per token. At this
// rate 200,000 input tokens or 40,000 output tokens cost exactly $1.00.
var opusRate = pricing.Rate{Input: 5000, Output: 25000, CacheRead: 500, CacheWrite: 6250}

// budgetOf is the resolver both tiers take. Every run it is asked about is
// judged against the same figure, which is all a limiter test needs — the
// per-run part is covered by TestRootLimiterReadsTheProcess.
func budgetOf(usd float64) budget.LimitResolver {
	return func(*agentkit.Process) budget.RunLimit {
		return budget.RunLimit{Budget: pricing.FromUSD(usd), Rate: opusRate}
	}
}

// roomyBudget is large enough that no token or step case in a test can reach it,
// so those cases exercise the ceiling they name and nothing else.
func roomyBudget() budget.LimitResolver { return budgetOf(1000) }

func TestConfigValidate(t *testing.T) {
	testCases := map[string]struct {
		mutate  func(budget.Config) budget.Config
		wantErr bool
	}{
		"valid": {
			mutate: func(c budget.Config) budget.Config { return c },
		},
		"zero steps": {
			mutate:  func(c budget.Config) budget.Config { c.MaxSteps = 0; return c },
			wantErr: true,
		},
		"negative steps": {
			mutate:  func(c budget.Config) budget.Config { c.MaxSteps = -1; return c },
			wantErr: true,
		},
		"zero input tokens": {
			mutate:  func(c budget.Config) budget.Config { c.MaxInputTokens = 0; return c },
			wantErr: true,
		},
		"zero output tokens": {
			mutate:  func(c budget.Config) budget.Config { c.MaxOutputTokens = 0; return c },
			wantErr: true,
		},
		"ratio at zero": {
			mutate:  func(c budget.Config) budget.Config { c.NoticeRatio = 0; return c },
			wantErr: true,
		},
		"ratio at one": {
			mutate:  func(c budget.Config) budget.Config { c.NoticeRatio = 1; return c },
			wantErr: true,
		},
		"ratio above one": {
			mutate:  func(c budget.Config) budget.Config { c.NoticeRatio = 1.5; return c },
			wantErr: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := tc.mutate(validConfig()).Validate()
			if tc.wantErr {
				gt.Value(t, err).NotNil()
				return
			}
			gt.NoError(t, err)
		})
	}
}

func TestConfigLimiter(t *testing.T) {
	limit := validConfig().Limiter(roomyBudget())
	ctx := context.Background()

	testCases := map[string]struct {
		metrics     agentkit.Metrics
		wantKind    agentkit.LimitKind
		wantMessage string
	}{
		"well within every ceiling": {
			metrics:  agentkit.Metrics{Steps: 1, InputTokens: 10, OutputTokens: 10},
			wantKind: agentkit.LimitKindPass,
		},
		"just below every notice threshold": {
			metrics:  agentkit.Metrics{Steps: 7, InputTokens: 799, OutputTokens: 159},
			wantKind: agentkit.LimitKindPass,
		},
		"step notice threshold reached": {
			metrics:     agentkit.Metrics{Steps: 8},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "step budget nearly exhausted (8/10)",
		},
		"input notice threshold reached": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 800},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "input token budget nearly exhausted (800/1000)",
		},
		"output notice threshold reached": {
			metrics:     agentkit.Metrics{Steps: 1, OutputTokens: 160},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "output token budget nearly exhausted (160/200)",
		},
		"step ceiling reached": {
			metrics:     agentkit.Metrics{Steps: 10},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "step budget exhausted (10/10)",
		},
		"input ceiling reached": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 1000},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "input token budget exhausted (1000/1000)",
		},
		"output ceiling reached": {
			metrics:     agentkit.Metrics{Steps: 1, OutputTokens: 200},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "output token budget exhausted (200/200)",
		},
		// The output ceiling is the expensive one. A run that is only near its
		// step and input thresholds but has spent its output allowance must be
		// stopped, not merely told to wrap up.
		"output stop outranks step and input notices": {
			metrics:     agentkit.Metrics{Steps: 8, InputTokens: 900, OutputTokens: 200},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "output token budget exhausted (200/200)",
		},
		"every ceiling crossed reports the step one first": {
			metrics:     agentkit.Metrics{Steps: 10, InputTokens: 1000, OutputTokens: 200},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "step budget exhausted (10/10)",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got := limit(ctx, nil, tc.metrics)
			gt.Value(t, got.Kind()).Equal(tc.wantKind)
			gt.String(t, got.Message()).Equal(tc.wantMessage)
		})
	}
}

// TestConfigLimiterMoney covers the sub-agent tier's money arm: the allowance a
// parent gave this child, judged against the child's own metrics.
//
// The token ceilings here are far out of reach so that each case exercises the
// money arm alone.
func TestConfigLimiterMoney(t *testing.T) {
	cfg := budget.Config{
		MaxSteps: 1000, MaxInputTokens: 10_000_000, MaxOutputTokens: 10_000_000, NoticeRatio: 0.8,
	}
	limit := cfg.Limiter(budgetOf(1))
	ctx := context.Background()

	testCases := map[string]struct {
		metrics     agentkit.Metrics
		wantKind    agentkit.LimitKind
		wantMessage string
	}{
		"well within the allowance": {
			metrics:  agentkit.Metrics{Steps: 1, InputTokens: 10_000},
			wantKind: agentkit.LimitKindPass,
		},
		"cost notice threshold reached": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 160_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget nearly exhausted ($0.80/$1.00)",
		},
		// The whole point of this change: a sub-agent out of money is told to
		// conclude, not killed before it can perform its task's tool call.
		"allowance spent is a notice, not a stop": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 200_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget exhausted ($1.00/$1.00)",
		},
		"far past the allowance is still a notice": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 1_000_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget exhausted ($5.00/$1.00)",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got := limit(ctx, nil, tc.metrics)
			gt.Value(t, got.Kind()).Equal(tc.wantKind)
			gt.String(t, got.Message()).Equal(tc.wantMessage)
		})
	}
}

// TestConfigLimiterRanksMoneyAgainstTokens pins the verdict order where the two
// arms overlap. A token ceiling can still stop this run and the money one cannot,
// so a token STOP outranks a spent allowance — but a spent allowance outranks a
// token threshold merely approached.
func TestConfigLimiterRanksMoneyAgainstTokens(t *testing.T) {
	limit := validConfig().Limiter(budgetOf(1))
	ctx := context.Background()

	// 200,000 input tokens is both $1.00 (spent) and past MaxInputTokens.
	tokenStop := agentkit.Metrics{Steps: 1, InputTokens: 200_000}
	got := limit(ctx, nil, tokenStop)
	gt.Value(t, got.Kind()).Equal(agentkit.LimitKindStop)
	gt.String(t, got.Message()).Equal("input token budget exhausted (200000/1000)")

	// Output tokens are dear enough to spend $1.00 while every token count is
	// only near its threshold: 40,000 output tokens is $1.00 exactly.
	spentButUnderCeilings := budget.Config{
		MaxSteps: 10, MaxInputTokens: 1_000_000, MaxOutputTokens: 50_000, NoticeRatio: 0.8,
	}.Limiter(budgetOf(1))
	got = spentButUnderCeilings(ctx, nil, agentkit.Metrics{Steps: 8, OutputTokens: 40_000})
	gt.Value(t, got.Kind()).Equal(agentkit.LimitKindNotice)
	gt.String(t, got.Message()).Equal("cost budget exhausted ($1.00/$1.00)")
}

// TestConfigLimiterStopsAnUnpricedRun pins that the sub-agent tier fails closed
// the same way the root tier does. A child that cannot be priced is not run
// unbounded just because its token ceilings would eventually catch it.
func TestConfigLimiterStopsAnUnpricedRun(t *testing.T) {
	ctx := context.Background()
	m := agentkit.Metrics{Steps: 1}

	testCases := map[string]budget.LimitResolver{
		"no resolver": nil,
		"zero budget": func(*agentkit.Process) budget.RunLimit {
			return budget.RunLimit{Rate: opusRate}
		},
		"unpriced model": func(*agentkit.Process) budget.RunLimit {
			return budget.RunLimit{Budget: pricing.FromUSD(1)}
		},
	}

	for name, resolve := range testCases {
		t.Run(name, func(t *testing.T) {
			got := validConfig().Limiter(resolve)(ctx, nil, m)
			gt.Value(t, got.Kind()).Equal(agentkit.LimitKindStop)
			gt.String(t, got.Message()).Equal("this run has no priced budget")
		})
	}
}

// TestOutputCeilingIsNotHiddenByInputHeadroom is the reason the two token counts
// are bounded separately. Under one combined ceiling this run would still look
// comfortable, and the expensive half would keep growing.
func TestOutputCeilingIsNotHiddenByInputHeadroom(t *testing.T) {
	limit := budget.Config{
		MaxSteps: 1000, MaxInputTokens: 500_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8,
	}.Limiter(roomyBudget())

	m := agentkit.Metrics{Steps: 5, InputTokens: 20_000, OutputTokens: 100_000}
	got := limit(context.Background(), nil, m)

	gt.Value(t, got.Kind()).Equal(agentkit.LimitKindStop)
	gt.String(t, got.Message()).Contains("output token budget exhausted")
}

// TestConfigLimiterCountsChildMetrics pins that the ceiling covers the whole
// tree. agentkit folds a child's metrics into its parent when the child
// terminates, so the parent's Limit sees them and must act on them.
func TestConfigLimiterCountsChildMetrics(t *testing.T) {
	limit := budget.Config{
		MaxSteps: 100, MaxInputTokens: 1000, MaxOutputTokens: 1000, NoticeRatio: 0.8,
	}.Limiter(roomyBudget())

	own := agentkit.Metrics{Steps: 3, InputTokens: 100, OutputTokens: 50}
	gt.Value(t, limit(context.Background(), nil, own).Kind()).Equal(agentkit.LimitKindPass)

	// Same Process after two children folded their usage into it.
	withChildren := agentkit.Metrics{Steps: 3, InputTokens: 1000, OutputTokens: 150}
	gt.Value(t, limit(context.Background(), nil, withChildren).Kind()).Equal(agentkit.LimitKindStop)
}

func validRoot() budget.Root {
	return budget.Root{MaxSteps: 100, NoticeRatio: 0.8}
}

func TestRootValidate(t *testing.T) {
	testCases := map[string]struct {
		mutate  func(budget.Root) budget.Root
		wantErr bool
	}{
		"valid": {
			mutate: func(r budget.Root) budget.Root { return r },
		},
		"zero steps": {
			mutate:  func(r budget.Root) budget.Root { r.MaxSteps = 0; return r },
			wantErr: true,
		},
		"negative steps": {
			mutate:  func(r budget.Root) budget.Root { r.MaxSteps = -1; return r },
			wantErr: true,
		},
		"ratio at zero": {
			mutate:  func(r budget.Root) budget.Root { r.NoticeRatio = 0; return r },
			wantErr: true,
		},
		"ratio at one": {
			mutate:  func(r budget.Root) budget.Root { r.NoticeRatio = 1; return r },
			wantErr: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := tc.mutate(validRoot()).Validate()
			if tc.wantErr {
				gt.Value(t, err).NotNil()
				return
			}
			gt.NoError(t, err)
		})
	}
}

func TestRunLimitValidate(t *testing.T) {
	testCases := map[string]struct {
		limit   budget.RunLimit
		wantErr bool
	}{
		"valid":       {limit: budget.RunLimit{Budget: pricing.FromUSD(1), Rate: opusRate}},
		"zero budget": {limit: budget.RunLimit{Rate: opusRate}, wantErr: true},
		"negative budget": {
			limit:   budget.RunLimit{Budget: -1, Rate: opusRate},
			wantErr: true,
		},
		"unpriced rate": {
			limit:   budget.RunLimit{Budget: pricing.FromUSD(1)},
			wantErr: true,
		},
		"input priced at nothing": {
			limit:   budget.RunLimit{Budget: pricing.FromUSD(1), Rate: pricing.Rate{Output: 25000}},
			wantErr: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := tc.limit.Validate()
			if tc.wantErr {
				gt.Value(t, err).NotNil()
				return
			}
			gt.NoError(t, err)
		})
	}
}

func TestRootLimiter(t *testing.T) {
	// $1.00 budget on the opus rate: 200,000 input tokens or 40,000 output
	// tokens would spend it exactly.
	limit := validRoot().Limiter(budgetOf(1))
	ctx := context.Background()

	testCases := map[string]struct {
		metrics     agentkit.Metrics
		wantKind    agentkit.LimitKind
		wantMessage string
	}{
		"well within every ceiling": {
			metrics:  agentkit.Metrics{Steps: 1, InputTokens: 1000, OutputTokens: 100},
			wantKind: agentkit.LimitKindPass,
		},
		"just below the notice thresholds": {
			metrics:  agentkit.Metrics{Steps: 79, InputTokens: 100_000, OutputTokens: 11_000},
			wantKind: agentkit.LimitKindPass,
		},
		"step notice threshold reached": {
			metrics:     agentkit.Metrics{Steps: 80},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "step budget nearly exhausted (80/100)",
		},
		"cost notice threshold reached": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 160_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget nearly exhausted ($0.80/$1.00)",
		},
		"step ceiling reached": {
			metrics:     agentkit.Metrics{Steps: 100},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "step budget exhausted (100/100)",
		},
		// The budget is spent, and the run is told so rather than killed: it
		// still has the tool call its turn was for to make, and a result to
		// write. Before this it was a LimitKindStop and the turn ended with the
		// work unperformed and nothing said.
		"budget spent by input is a notice": {
			metrics:     agentkit.Metrics{Steps: 1, InputTokens: 200_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget exhausted ($1.00/$1.00)",
		},
		"budget spent by output is a notice": {
			metrics:     agentkit.Metrics{Steps: 1, OutputTokens: 40_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget exhausted ($1.00/$1.00)",
		},
		"a spent budget outranks the step notice": {
			metrics:     agentkit.Metrics{Steps: 80, InputTokens: 200_000},
			wantKind:    agentkit.LimitKindNotice,
			wantMessage: "cost budget exhausted ($1.00/$1.00)",
		},
		"the step ceiling still stops a run that has also spent its budget": {
			metrics:     agentkit.Metrics{Steps: 100, InputTokens: 200_000},
			wantKind:    agentkit.LimitKindStop,
			wantMessage: "step budget exhausted (100/100)",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got := limit(ctx, nil, tc.metrics)
			gt.Value(t, got.Kind()).Equal(tc.wantKind)
			gt.String(t, got.Message()).Equal(tc.wantMessage)
		})
	}
}

// TestAnOverspentRunIsNeverStoppedByItsBudget is the regression test for the run
// that failed with "cost budget exhausted ($2.31/$2.00)".
//
// A child folds its whole spend into its parent in one write, so a root run can
// pass from under the notice threshold to well past the budget between two of
// its own transitions. Every one of those overshoots must still answer Notice:
// a Stop is read before Step and would leave the strategy no transition in which
// to write what its children already did.
func TestAnOverspentRunIsNeverStoppedByItsBudget(t *testing.T) {
	// $2.00 at the deployment's own notice ratio, which is the pair the failing run
	// was judged against.
	limit := budget.Root{MaxSteps: 100, NoticeRatio: 0.9}.Limiter(budgetOf(2))
	ctx := context.Background()

	// The jump that run actually made: $1.783 committed, then a round of four
	// children folded in and took it to $2.31.
	beforeTheFold := agentkit.Metrics{Steps: 40, InputTokens: 356_600}
	gt.Value(t, limit(ctx, nil, beforeTheFold).Kind()).Equal(agentkit.LimitKindPass)

	afterTheFold := agentkit.Metrics{Steps: 44, InputTokens: 462_000}
	got := limit(ctx, nil, afterTheFold)
	gt.Value(t, got.Kind()).Equal(agentkit.LimitKindNotice)
	gt.String(t, got.Message()).Equal("cost budget exhausted ($2.31/$2.00)")

	// And it stays a notice however far past the budget the fold lands.
	tenfold := agentkit.Metrics{Steps: 44, InputTokens: 4_000_000}
	gt.Value(t, limit(ctx, nil, tenfold).Kind()).Equal(agentkit.LimitKindNotice)
}

// TestRootLimiterPricesCacheSeparately is the reason the ceiling is money rather
// than tokens: the same token counts cost different amounts depending on how
// much of the input was served from cache.
func TestRootLimiterPricesCacheSeparately(t *testing.T) {
	limit := validRoot().Limiter(budgetOf(1))
	ctx := context.Background()

	// 200,000 input tokens spend the whole budget at the uncached rate...
	uncached := agentkit.Metrics{Steps: 1, InputTokens: 200_000}
	gt.Value(t, limit(ctx, nil, uncached).Kind()).Equal(agentkit.LimitKindNotice)

	// ...but the same count served from cache costs a tenth of that.
	cached := agentkit.Metrics{Steps: 1, InputTokens: 200_000, CacheReadInputTokens: 200_000}
	gt.Value(t, limit(ctx, nil, cached).Kind()).Equal(agentkit.LimitKindPass)
}

// TestRootLimiterStopsAnUnpricedRun pins the fail-closed behaviour: a run whose
// budget or price cannot be resolved is stopped rather than run unbounded. This
// is the one money-shaped condition that is still a Stop, because there is no
// figure for the run to conclude against.
func TestRootLimiterStopsAnUnpricedRun(t *testing.T) {
	ctx := context.Background()
	m := agentkit.Metrics{Steps: 1}

	testCases := map[string]budget.LimitResolver{
		"no resolver": nil,
		"zero budget": func(*agentkit.Process) budget.RunLimit {
			return budget.RunLimit{Rate: opusRate}
		},
		"unpriced model": func(*agentkit.Process) budget.RunLimit {
			return budget.RunLimit{Budget: pricing.FromUSD(1)}
		},
	}

	for name, resolve := range testCases {
		t.Run(name, func(t *testing.T) {
			got := validRoot().Limiter(resolve)(ctx, nil, m)
			gt.Value(t, got.Kind()).Equal(agentkit.LimitKindStop)
			gt.String(t, got.Message()).Equal("this run has no priced budget")
		})
	}
}

// TestRootLimiterCountsChildSpend pins that the money ceiling covers the whole
// tree. agentkit folds a child's metrics into its parent when the child
// terminates, so the parent's Limit sees them and must act on them.
func TestRootLimiterCountsChildSpend(t *testing.T) {
	limit := validRoot().Limiter(budgetOf(1))
	ctx := context.Background()

	own := agentkit.Metrics{Steps: 3, InputTokens: 10_000, OutputTokens: 500}
	gt.Value(t, limit(ctx, nil, own).Kind()).Equal(agentkit.LimitKindPass)

	// Same Process after two children folded their usage into it.
	withChildren := agentkit.Metrics{Steps: 3, InputTokens: 150_000, OutputTokens: 12_000}
	gt.Value(t, limit(ctx, nil, withChildren).Kind()).Equal(agentkit.LimitKindNotice)
}

// TestRootLimiterReadsTheProcess pins that the resolver is given the Process, so
// one registered limiter can judge each run against its own budget and model.
func TestRootLimiterReadsTheProcess(t *testing.T) {
	limit := validRoot().Limiter(func(proc *agentkit.Process) budget.RunLimit {
		if proc != nil && proc.Metadata["tier"] == "cheap" {
			return budget.RunLimit{Budget: pricing.FromUSD(0.10), Rate: opusRate}
		}
		return budget.RunLimit{Budget: pricing.FromUSD(10), Rate: opusRate}
	})
	ctx := context.Background()
	m := agentkit.Metrics{Steps: 1, InputTokens: 100_000} // $0.50

	cheap := &agentkit.Process{Metadata: map[string]string{"tier": "cheap"}}
	gt.Value(t, limit(ctx, cheap, m).Kind()).Equal(agentkit.LimitKindNotice)

	rich := &agentkit.Process{Metadata: map[string]string{"tier": "rich"}}
	gt.Value(t, limit(ctx, rich, m).Kind()).Equal(agentkit.LimitKindPass)
}

func TestMeasuredSpendOverridesFoldedTokenRate(t *testing.T) {
	spent := pricing.FromUSD(0.1)
	resolve := func(*agentkit.Process) budget.RunLimit {
		return budget.RunLimit{Budget: pricing.FromUSD(1), Rate: pricing.Rate{Input: 1000, Output: 5000}, Spent: &spent}
	}
	root := budget.Root{MaxSteps: 100, NoticeRatio: 0.8}.Limiter(resolve)
	// The combined tokens would cost $10 at the parent rate, but the ledger
	// already measured them on a mixture of models at $0.10.
	m := agentkit.Metrics{InputTokens: 10_000_000}
	if got := root(context.Background(), &agentkit.Process{}, m); got.Kind() != agentkit.LimitKindPass {
		gt.Bool(t, false).Describef("premature notice: %v", got).True().Required()
	}
	spent = pricing.FromUSD(0.9)
	if got := root(context.Background(), &agentkit.Process{}, m); got.Kind() != agentkit.LimitKindNotice {
		gt.Bool(t, false).Describef("missing actual-spend notice: %v", got).True().Required()
	}
	spent = -1
	if got := root(context.Background(), &agentkit.Process{}, m); got.Kind() != agentkit.LimitKindStop {
		gt.Bool(t, false).Describef("negative spend accepted: %v", got).True().Required()
	}
}
