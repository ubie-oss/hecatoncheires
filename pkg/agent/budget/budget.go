// Package budget turns an operator-configured ceiling into the
// agentkit.Limiter a Strategy answers Limit with.
//
// There are two tiers, and they are bounded by different quantities:
//
//   - Root is what an application entry point spawns. Its spend ceiling is
//     MONEY (see Root and RunLimit), because a token is not a unit of cost: one
//     run may execute on a model twenty times dearer than another's, so a token
//     figure that is right for one is wrong for the other. It keeps a step
//     ceiling, but only to stop a run that never terminates.
//   - Config is the sub-agent tier (one planned task). It is bounded by steps
//     and tokens AND by money, read from its own Process metadata. TODAY that
//     metadata is inherited from its parent unchanged, so each child is judged
//     against the ROOT figure measured on its OWN metrics: one child cannot
//     spend more than the whole run's budget, but a round of five of them
//     together still can. Carving a share of what is left out for each child is
//     a separate change; until it lands, this tier bounds one child, not a
//     round.
//
// MaxInputTokens and MaxOutputTokens bound the two token counts separately.
// Output tokens cost several times what input tokens do, so a single combined
// ceiling would let a large input allowance hide an output run-away — the
// expensive half — until the whole allowance was gone.
//
// # Money never stops a run; it only tells it to conclude
//
// A crossed MONEY ceiling answers LimitNotice, not LimitStop, and keeps
// answering it. agentkit reads a Stop at the transition boundary and fails the
// Process WITHOUT calling Step (worker.go, driveClaim), so a Stop is the one
// verdict after which a run can do nothing at all — it cannot perform the tool
// call its task was for, and it cannot say what it did. That is what a budget
// ceiling used to produce, and a run killed there had usually already paid for
// the work whose result it then threw away.
//
// So what ends a run out of money is the STRATEGY, not this package: both
// bundled strategies read the notice, make their two reserve moves (the final
// tool call, then the result written from it) and terminate.
//
// A budget is therefore what a run may spend BEFORE it must conclude, and NOT a
// bound on what it is charged. Three things are spent on top of it, and a caller
// sizing a budget has to allow for all three:
//
//   - the overshoot already committed when the notice was first observed. A
//     child's whole spend folds into its parent in one write, so the first
//     verdict a parent ever gets about its money can already be far past the
//     budget — the run this behaviour was written for observed $2.31 of $2.00.
//   - the reserve's two calls.
//   - everything up to the step or token ceiling, when the model keeps calling
//     tools after the reserve's one tool round. The calls after that round are
//     sent with tool calls disabled, so this takes a provider that returns a
//     call despite the setting; the step and token ceilings bound that case.
//
// # What still stops a run
//
// Steps and tokens do, and they must: with money answering only notices, they
// are the only thing left that can end a run whose strategy does not end
// itself. So Config's three ceilings and Root's step ceiling stay LimitStop.
// A run whose budget or price cannot be resolved is stopped too, for a
// different reason — see Root.Limiter.
//
// Every ceiling is cumulative over the whole Process tree: a child folds its
// metrics into its parent when it terminates, so the ceiling on a root run
// covers every sub-agent it spawned.
//
// It lives under pkg/agent rather than pkg/usecase because it is a policy value
// object the runtime evaluates on the transition hot path, not a business
// operation: it reaches nothing, decides nothing about a Case, and takes no
// request context.
package budget

import (
	"context"
	"fmt"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// Config is one agent kind's ceiling.
type Config struct {
	// MaxSteps is the greatest number of committed transitions this Process may
	// run. Must be positive.
	MaxSteps int64
	// MaxInputTokens is the greatest number of input tokens this Process may
	// consume. Must be positive.
	MaxInputTokens int64
	// MaxOutputTokens is the greatest number of output tokens this Process may
	// produce. Must be positive.
	MaxOutputTokens int64
	// NoticeRatio is the fraction of any ceiling a Process may spend before the
	// strategy is told to conclude. What is left is the reserve, which has to
	// cover the final tool call the strategy asks for AND the call that writes
	// the result from it — not one wrap-up call. Must be greater than 0 and less
	// than 1.
	//
	// It marks where a STEP or TOKEN ceiling starts its reserve, and the ceiling
	// itself then stops the run. The money ceiling never stops one, so past it
	// the reserve is not a fixed fraction: it is however much the strategy's two
	// remaining moves cost.
	NoticeRatio float64
}

// Validate enforces the required-field contract so a wiring mistake fails at
// startup rather than at the first mention.
func (c Config) Validate() error {
	if c.MaxSteps <= 0 {
		return goerr.New("max steps must be positive", goerr.V("max_steps", c.MaxSteps))
	}
	if c.MaxInputTokens <= 0 {
		return goerr.New("max input tokens must be positive",
			goerr.V("max_input_tokens", c.MaxInputTokens))
	}
	if c.MaxOutputTokens <= 0 {
		return goerr.New("max output tokens must be positive",
			goerr.V("max_output_tokens", c.MaxOutputTokens))
	}
	if c.NoticeRatio <= 0 || c.NoticeRatio >= 1 {
		return goerr.New("notice ratio must be between 0 and 1 exclusive",
			goerr.V("notice_ratio", c.NoticeRatio))
	}
	return nil
}

// Limiter returns the agentkit.Limiter that answers Strategy.Limit for a
// sub-agent run.
//
// resolve answers "what money is this sub-agent judged against". It is the same
// LimitResolver a root run uses, reading the same Process metadata key: a
// sub-agent's Process carries the allowance its parent gave it, so one resolver
// serves both tiers and a child is metered at the price of the model it
// actually generates through.
//
// The returned function is read-only, non-blocking and does no I/O, as the
// Limiter contract requires: agentkit calls it at every transition boundary and
// both before and after every LLM call, tool call and child spawn, so anything
// that acquires or waits here would be charged several times per effect and
// would turn a throttle into a lease expiry.
func (c Config) Limiter(resolve LimitResolver) agentkit.Limiter {
	return func(_ context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		spent, limit, priced := resolveSpend(resolve, proc, m)
		if !priced {
			return agentkit.LimitStop(unpricedRunMessage)
		}

		switch {
		// Every Stop is tested before any Notice. A notice threshold is always
		// the lower of the pair, so interleaving them would answer "nearly
		// exhausted" about one ceiling at the moment another was actually
		// reached, and let the run continue past it.
		case m.Steps >= c.MaxSteps:
			return agentkit.LimitStop(fmt.Sprintf("step budget exhausted (%d/%d)", m.Steps, c.MaxSteps))
		case m.InputTokens >= c.MaxInputTokens:
			return agentkit.LimitStop(fmt.Sprintf("input token budget exhausted (%d/%d)",
				m.InputTokens, c.MaxInputTokens))
		case m.OutputTokens >= c.MaxOutputTokens:
			return agentkit.LimitStop(fmt.Sprintf("output token budget exhausted (%d/%d)",
				m.OutputTokens, c.MaxOutputTokens))

		// The notices are ordered by how far gone the ceiling behind each one is.
		// A CROSSED ceiling outranks an approached one, so the money notice comes
		// first here and last below: crossed, it is the most advanced state this
		// verdict can report; merely approached, it is the least urgent, because
		// it is the one ceiling that will never stop the run by itself.
		case spent >= limit.Budget:
			return agentkit.LimitNotice(fmt.Sprintf("cost budget exhausted (%s/%s)",
				spent.USD(), limit.Budget.USD()))
		case atNotice(m.Steps, c.MaxSteps, c.NoticeRatio):
			return agentkit.LimitNotice(fmt.Sprintf("step budget nearly exhausted (%d/%d)",
				m.Steps, c.MaxSteps))
		case atNotice(m.InputTokens, c.MaxInputTokens, c.NoticeRatio):
			return agentkit.LimitNotice(fmt.Sprintf("input token budget nearly exhausted (%d/%d)",
				m.InputTokens, c.MaxInputTokens))
		case atNotice(m.OutputTokens, c.MaxOutputTokens, c.NoticeRatio):
			return agentkit.LimitNotice(fmt.Sprintf("output token budget nearly exhausted (%d/%d)",
				m.OutputTokens, c.MaxOutputTokens))
		case atNotice(int64(spent), int64(limit.Budget), c.NoticeRatio):
			return agentkit.LimitNotice(fmt.Sprintf("cost budget nearly exhausted (%s/%s)",
				spent.USD(), limit.Budget.USD()))
		}
		return agentkit.LimitPass()
	}
}

func atNotice(used, ceiling int64, ratio float64) bool {
	return float64(used) >= float64(ceiling)*ratio
}

// Root is the ceiling every Process an application entry point spawns runs
// under. Unlike Config it names no token allowance: what a root run may spend is
// an amount of money, resolved per run (see RunLimit), because the model a run
// generates through — and therefore what a token costs it — is configuration.
type Root struct {
	// MaxSteps is the greatest number of committed transitions this Process may
	// run. Must be positive.
	//
	// It is not a spend ceiling. It exists so a run that never terminates is
	// stopped even while it costs almost nothing — and, since the money ceiling
	// answers only notices, it is the ONLY thing that can end a root run whose
	// strategy does not end itself.
	MaxSteps int64
	// NoticeRatio is the fraction of any ceiling a Process may spend before the
	// strategy is told to conclude. What is left is the reserve, which has to
	// cover the final tool call the strategy asks for AND the call that writes
	// the result from it — not one wrap-up call. Must be greater than 0 and less
	// than 1.
	//
	// Crossing it is a hint, not the last warning: a child folds its whole spend
	// into its parent in one write, so a root run can pass from under this
	// fraction to past the budget between two of its own transitions and never
	// be told "nearly". That is why crossing the budget itself is a notice too —
	// it is the same instruction, arriving at the only moment this run got.
	NoticeRatio float64
}

// Validate enforces the required-field contract so a wiring mistake fails at
// startup rather than at the first run.
func (r Root) Validate() error {
	if r.MaxSteps <= 0 {
		return goerr.New("max steps must be positive", goerr.V("max_steps", r.MaxSteps))
	}
	if r.NoticeRatio <= 0 || r.NoticeRatio >= 1 {
		return goerr.New("notice ratio must be between 0 and 1 exclusive",
			goerr.V("notice_ratio", r.NoticeRatio))
	}
	return nil
}

// RunLimit is what ONE run is judged against: the money it may spend and the
// price of the model it spends it on.
//
// It is resolved per run rather than configured once because both halves come
// from the run itself — the Job that started it names its budget, and the model
// it named fixes the rate. The two travel together so a run can never be metered
// at one model's price while generating with another's.
type RunLimit struct {
	// Budget is the greatest amount this run may spend. Must be positive.
	Budget pricing.NanoUSD
	// Rate prices the run's model. Must price input and output above zero.
	Rate pricing.Rate
	// Spent optionally supplies money already measured at each call's actual
	// model rate. Mixed-model jobs use it because cumulative token counters
	// cannot be priced at one rate. Nil preserves the token-based policy.
	Spent *pricing.NanoUSD
}

// Validate reports whether this run can be bounded at all. A run that cannot is
// stopped rather than run unbounded (see Root.Limiter).
func (l RunLimit) Validate() error {
	if l.Budget <= 0 {
		return goerr.New("budget must be positive", goerr.V("budget", int64(l.Budget)))
	}
	if !l.Rate.IsPriced() {
		return goerr.New("model rate must price input and output above zero",
			goerr.V("input", int64(l.Rate.Input)), goerr.V("output", int64(l.Rate.Output)))
	}
	if l.Spent != nil && *l.Spent < 0 {
		return goerr.New("measured spend must not be negative")
	}
	return nil
}

// LimitResolver answers "what is this run judged against". The caller supplies
// it because resolving a run's model and budget means reading its metadata,
// which is the kernel's vocabulary, not this package's.
//
// It must be read-only, non-blocking and free of I/O for the same reason the
// Limiter is: it runs inside the Limiter.
type LimitResolver func(proc *agentkit.Process) RunLimit

// unpricedRunMessage is the Stop reason for a run that cannot be bounded at all.
// Both tiers use it: whichever tier reaches it, the situation is the same and so
// is the only safe answer.
const unpricedRunMessage = "this run has no priced budget"

// resolveSpend prices what this Process has consumed against the ceiling its run
// is judged by. priced is false when the run cannot be bounded at all, which is
// the one condition that stops a run outright rather than telling it to
// conclude.
func resolveSpend(resolve LimitResolver, proc *agentkit.Process, m agentkit.Metrics,
) (spent pricing.NanoUSD, limit RunLimit, priced bool) {
	// A run whose budget or price cannot be resolved is STOPPED, not run
	// unbounded. Startup validation makes this unreachable for a configured
	// deployment, so reaching it means the run's metadata says something this
	// build cannot price — and spending money on it is the one outcome that
	// cannot be undone. This is the one case where refusing to run beats letting
	// the run conclude, because there is no figure to conclude against.
	if resolve == nil {
		return 0, RunLimit{}, false
	}
	limit = resolve(proc)
	if err := limit.Validate(); err != nil {
		return 0, RunLimit{}, false
	}
	spent = limit.Rate.Cost(m.InputTokens, m.OutputTokens,
		m.CacheReadInputTokens, m.CacheCreationInputTokens)
	if limit.Spent != nil {
		spent = *limit.Spent
	}
	return spent, limit, true
}

// Limiter returns the agentkit.Limiter that answers Strategy.Limit for a root
// run.
//
// The returned function is read-only, non-blocking and does no I/O, as the
// Limiter contract requires: agentkit calls it at every transition boundary and
// both before and after every LLM call, tool call and child spawn, so anything
// that acquires or waits here would be charged several times per effect and
// would turn a throttle into a lease expiry.
func (r Root) Limiter(resolve LimitResolver) agentkit.Limiter {
	return func(_ context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		spent, limit, priced := resolveSpend(resolve, proc, m)
		if !priced {
			return agentkit.LimitStop(unpricedRunMessage)
		}

		switch {
		// The step ceiling is the only Stop here, and it is tested first for the
		// reason every Stop is: a notice threshold is the lower of its pair, so
		// answering "nearly exhausted" at the moment a ceiling was actually
		// reached would let the run continue past it.
		case m.Steps >= r.MaxSteps:
			return agentkit.LimitStop(fmt.Sprintf("step budget exhausted (%d/%d)", m.Steps, r.MaxSteps))

		// Spending the budget is a NOTICE, not a Stop. See the package comment:
		// a Stop is read before Step and leaves the run unable to perform the
		// call its turn was for or to say anything, and a child's spend folds in
		// one write, so this is routinely the first and only verdict a run gets
		// about its money.
		//
		// It outranks the step notice because a crossed ceiling is further gone
		// than an approached one, and is outranked by nothing except the Stop.
		case spent >= limit.Budget:
			return agentkit.LimitNotice(fmt.Sprintf("cost budget exhausted (%s/%s)",
				spent.USD(), limit.Budget.USD()))
		case atNotice(m.Steps, r.MaxSteps, r.NoticeRatio):
			return agentkit.LimitNotice(fmt.Sprintf("step budget nearly exhausted (%d/%d)",
				m.Steps, r.MaxSteps))
		case atNotice(int64(spent), int64(limit.Budget), r.NoticeRatio):
			return agentkit.LimitNotice(fmt.Sprintf("cost budget nearly exhausted (%s/%s)",
				spent.USD(), limit.Budget.USD()))
		}
		return agentkit.LimitPass()
	}
}
