package kernel

import (
	"maps"
	"slices"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// The providers a model definition may name. They are the providers
// config.LLM knows how to build a client for; anything else is a typo in the
// configuration document rather than a model this build can reach.
const (
	ProviderOpenAI = "openai"
	ProviderClaude = "claude"
	ProviderGemini = "gemini"
)

// ModelDef is one model an operator declared usable, with what it costs.
//
// It is the resolved form of a global config [[llm_model]] entry: the reference
// name has already been decided (the alias, or the model name when the entry
// declares no alias) and the prices have already been converted out of the
// dollars-per-million-tokens the operator writes.
type ModelDef struct {
	// Ref is the name a Job or the CLI names this model by. Unique across every
	// definition the deployment loaded.
	Ref string
	// Provider is which client builds it: openai, claude or gemini.
	Provider string
	// Model is the model name handed to that provider.
	Model string
	// Rate prices one token of each kind.
	Rate pricing.Rate

	// Thinking, ThinkingDisplay and Effort are sent as thinking.type,
	// thinking.display and output_config.effort on every request to a claude
	// model. Empty means not sent, so the model default applies. Which values a
	// model accepts is the API's to decide: an unsupported one is sent as given
	// and the API rejects the request.
	Thinking        string
	ThinkingDisplay string
	Effort          string
}

// ThinkingAdaptive is the one value ModelDef.Thinking accepts. It is the API's own
// value, as are the ThinkingDisplay and Effort values below, so a value the API
// adds later is one constant away.
const ThinkingAdaptive = "adaptive"

const (
	thinkingDisplaySummarized = "summarized"
	thinkingDisplayOmitted    = "omitted"

	effortLow    = "low"
	effortMedium = "medium"
	effortHigh   = "high"
	effortXHigh  = "xhigh"
	effortMax    = "max"
)

// ErrInvalidReasoningSetting is returned when a definition's Thinking,
// ThinkingDisplay or Effort is a value this build does not send, or is set where
// it cannot apply.
var ErrInvalidReasoningSetting = goerr.New("invalid thinking / thinking_display / effort setting")

// Validate enforces what a definition cannot be used without.
func (d ModelDef) Validate() error {
	if d.Ref == "" {
		return goerr.New("model reference name is required")
	}
	if d.Model == "" {
		return goerr.New("model name is required", goerr.V("ref", d.Ref))
	}
	switch d.Provider {
	case ProviderOpenAI, ProviderClaude, ProviderGemini:
	default:
		return goerr.New("model provider must be openai, claude or gemini",
			goerr.V("ref", d.Ref), goerr.V("provider", d.Provider))
	}
	if !d.Rate.IsPriced() {
		return goerr.New("a model priced at nothing would make its budget unbounded",
			goerr.V("ref", d.Ref),
			goerr.V("input", int64(d.Rate.Input)), goerr.V("output", int64(d.Rate.Output)))
	}
	if d.Rate.CacheRead < 0 || d.Rate.CacheWrite < 0 {
		return goerr.New("a model has a negative cache price",
			goerr.V("ref", d.Ref),
			goerr.V("cache_read", int64(d.Rate.CacheRead)),
			goerr.V("cache_write", int64(d.Rate.CacheWrite)))
	}
	return d.validateReasoning()
}

// validateReasoning checks Thinking, ThinkingDisplay and Effort. It checks the
// value set only, never which model accepts which value: that is the API's
// answer, and a table of it here would go stale with every model release.
func (d ModelDef) validateReasoning() error {
	if d.Thinking == "" && d.ThinkingDisplay == "" && d.Effort == "" {
		return nil
	}
	if d.Provider != ProviderClaude {
		return goerr.Wrap(ErrInvalidReasoningSetting,
			"thinking, thinking_display and effort can be set only for a claude model",
			goerr.V("ref", d.Ref), goerr.V("provider", d.Provider))
	}
	switch d.Thinking {
	case "", ThinkingAdaptive:
	default:
		return goerr.Wrap(ErrInvalidReasoningSetting, "thinking must be \"adaptive\"",
			goerr.V("ref", d.Ref), goerr.V("thinking", d.Thinking))
	}
	switch d.ThinkingDisplay {
	case "":
	case thinkingDisplaySummarized, thinkingDisplayOmitted:
		if d.Thinking == "" {
			return goerr.Wrap(ErrInvalidReasoningSetting, "thinking_display requires thinking",
				goerr.V("ref", d.Ref), goerr.V("thinking_display", d.ThinkingDisplay))
		}
	default:
		return goerr.Wrap(ErrInvalidReasoningSetting,
			"thinking_display must be \"summarized\" or \"omitted\"",
			goerr.V("ref", d.Ref), goerr.V("thinking_display", d.ThinkingDisplay))
	}
	switch d.Effort {
	case "", effortLow, effortMedium, effortHigh, effortXHigh, effortMax:
	default:
		return goerr.Wrap(ErrInvalidReasoningSetting,
			"effort must be \"low\", \"medium\", \"high\", \"xhigh\" or \"max\"",
			goerr.V("ref", d.Ref), goerr.V("effort", d.Effort))
	}
	return nil
}

// ModelPolicyInput is everything a ModelPolicy is built from.
type ModelPolicyInput struct {
	// Defs are every model the deployment declared. Reference names must be
	// unique, and every definition must be priced.
	Defs []ModelDef
	// DefaultRef is the reference name of the model a run that names none
	// generates through. Required, and must be one of Defs.
	DefaultRef string
	// Clients maps a reference name to the client that serves it. The default
	// reference needs no entry: it is the Kernel's positional client, which an
	// unbound role already resolves to.
	Clients map[string]gollem.LLMClient
	// DefaultBudget is what a run that names no budget of its own may spend.
	// Required and positive: a zero ceiling stops every run (see
	// budget.Root.Limiter), so it must be caught here rather than at the first
	// transition.
	DefaultBudget pricing.NanoUSD
}

// Validate enforces the required-field contract so a configuration mistake fails
// at startup rather than at the first run.
func (in ModelPolicyInput) Validate() error {
	if len(in.Defs) == 0 {
		return goerr.New("at least one model definition is required")
	}
	seen := make(map[string]struct{}, len(in.Defs))
	for _, d := range in.Defs {
		if err := d.Validate(); err != nil {
			return goerr.Wrap(err, "invalid model definition")
		}
		if _, dup := seen[d.Ref]; dup {
			return goerr.New("duplicate model reference name", goerr.V("ref", d.Ref))
		}
		seen[d.Ref] = struct{}{}
	}
	if in.DefaultRef == "" {
		return goerr.New("a default model reference name is required")
	}
	if _, ok := seen[in.DefaultRef]; !ok {
		return goerr.New("the default model is not defined",
			goerr.V("ref", in.DefaultRef), goerr.V("known", slices.Sorted(maps.Keys(seen))))
	}
	for ref, client := range in.Clients {
		if _, ok := seen[ref]; !ok {
			return goerr.New("a client was supplied for an undefined model",
				goerr.V("ref", ref), goerr.V("known", slices.Sorted(maps.Keys(seen))))
		}
		if client == nil {
			return goerr.New("a model has a nil client", goerr.V("ref", ref))
		}
	}
	if in.DefaultBudget <= 0 {
		return goerr.New("the default budget must be positive",
			goerr.V("default_budget", int64(in.DefaultBudget)))
	}

	return nil
}

// ModelPolicy answers, for one run, which model it generates through and what it
// is judged against.
//
// Both halves live in one value because they must agree: resolving the model
// from one place and the price from another is how a run ends up generating with
// a cheap model while being metered at an expensive one's rate, or the reverse.
// Everything here is decided at startup and never changes afterwards, which is
// also what makes it safe to read on the transition hot path.
type ModelPolicy struct {
	defs          map[string]ModelDef
	roles         map[string]agentkit.ModelRole
	clients       map[string]gollem.LLMClient
	defaultRef    string
	defaultBudget pricing.NanoUSD
	durableUsage  *durableUsageLedger
}

// NewModelPolicy builds the policy. One model role is defined per supplied
// client; the default model gets none, because an unbound role already resolves
// to the Kernel's positional client and defining one would only add a second
// name for it.
func NewModelPolicy(in ModelPolicyInput) (ModelPolicy, error) {
	if err := in.Validate(); err != nil {
		return ModelPolicy{}, goerr.Wrap(err, "invalid model policy")
	}
	p := ModelPolicy{
		defs:          make(map[string]ModelDef, len(in.Defs)),
		roles:         make(map[string]agentkit.ModelRole, len(in.Clients)),
		clients:       make(map[string]gollem.LLMClient, len(in.Clients)),
		defaultRef:    in.DefaultRef,
		defaultBudget: in.DefaultBudget,
		durableUsage:  newDurableUsageLedger(),
	}
	for _, d := range in.Defs {
		p.defs[d.Ref] = d
	}
	for _, ref := range slices.Sorted(maps.Keys(in.Clients)) {
		if ref == in.DefaultRef {
			continue
		}
		p.roles[ref] = agentkit.DefineModelRole("hecatoncheires.model." + ref)
		p.clients[ref] = in.Clients[ref]
	}
	return p, nil
}

// IsZero reports whether this is the empty policy, which a deployment with no
// LLM configured carries.
func (p ModelPolicy) IsZero() bool { return len(p.defs) == 0 }

// Refs lists every defined reference name, sorted. For error messages and tests.
func (p ModelPolicy) Refs() []string { return slices.Sorted(maps.Keys(p.defs)) }

// DefaultRef is the startup-resolved alias a fresh run inherits when no model
// was specified. Hosts pin it into execution scopes before persisting a run.
func (p ModelPolicy) DefaultRef() string { return p.defaultRef }

// Resolve returns what this run is judged against: its budget and the price of
// the model it generates through. It takes the Process because that is what the
// Limiter contract hands it.
func (p ModelPolicy) Resolve(proc *agentkit.Process) budget.RunLimit {
	var sc Scope
	if proc != nil {
		sc = executionScopeForAgent(ScopeFrom(proc.Metadata), proc.Agent)
	}
	limit := budget.RunLimit{Budget: p.defaultBudget, Rate: p.defOf(sc).Rate}

	if sc.Budget > 0 {
		limit.Budget = sc.Budget
	}
	if sc.hasJobExecution() && proc != nil {
		spent, known := p.durableUsage.processCost(string(proc.RootID), string(proc.ID))
		if !known {
			spent = -1 // RunLimit.Validate refuses unknown usage before another effect.
		}
		limit.Spent = &spent
		return limit
	}

	return limit
}

// Cost prices what a run spent, at the rate of the model its scope names. It is
// what the run record stores, so a later edit to the configured price cannot
// rewrite history.
//
// It takes the scope rather than the Process because every caller already holds
// one: a completion handler reads the scope to find the run's records, and
// re-deriving it here would parse the same metadata twice.
func (p ModelPolicy) Cost(sc Scope, m agentkit.Metrics) pricing.NanoUSD {
	if sc.hasJobExecution() {
		cost, _ := p.UsageCost(sc)
		return cost
	}

	def := p.defOf(sc)
	return def.Rate.Cost(m.InputTokens, m.OutputTokens,
		m.CacheReadInputTokens, m.CacheCreationInputTokens)
}

// Remaining reports what the run this metadata describes may still spend, and
// the total it was allowed. It is what a planner is shown so it can size the
// tasks it plans, and what a per-task allowance is carved out of.
//
// It takes the metadata rather than the Process because the caller that needs it
// is a strategy deciding how to split what is left, and a strategy holds a
// Syscalls, not a row.
//
// remaining is clamped at zero. A run whose children folded in more than its
// ceiling has nothing left to give away, not a negative amount — and handing a
// negative figure to a planner would invite it to allocate one.
func (p ModelPolicy) Remaining(m map[string]string, metrics agentkit.Metrics,
) (remaining, total pricing.NanoUSD) {
	sc := ScopeFrom(m)
	total = p.defaultBudget
	if sc.Budget > 0 {
		total = sc.Budget
	}
	if sc.hasJobExecution() {
		spent, known := p.UsageCost(sc)
		if known && spent < total {
			remaining = total - spent
		}
		return remaining, total
	}
	if spent := p.Cost(sc, metrics); spent < total {
		remaining = total - spent
	}
	return remaining, total
}

// RemainingFunc returns Remaining in the shape a plan-execute host hands to
// planexec, or nil for the empty policy.
//
// The nil is the point: a deployment with no LLM configured prices nothing, so
// Remaining would answer "$0.00 of $0.00" and every plan allocating a positive
// budget would be rejected. nil turns per-task budgets off instead, which is the
// correct reading of "there is no figure here". Keeping the check in one place
// stops each of the five hosts from having to remember it.
func (p ModelPolicy) RemainingFunc() func(map[string]string, agentkit.Metrics) (pricing.NanoUSD, pricing.NanoUSD) {
	if p.IsZero() {
		return nil
	}
	return p.Remaining
}

// ModelName names the model a run generated through — the provider's own model
// name, not the reference name, because that is the value an operator can match
// against a provider's billing.
func (p ModelPolicy) ModelName(sc Scope) string { return p.defOf(sc).Model }

// defOf is the run's model definition.
//
// Legacy scopes fall back to the default for an unknown reference, matching
// their role lookup. An explicit job execution snapshot instead stays unpriced
// when its pinned model was removed, so it cannot silently change providers.
func (p ModelPolicy) defOf(sc Scope) ModelDef {
	if sc.hasJobExecution() && sc.JobExecution.Validate() != nil {
		return ModelDef{}
	}
	ref := sc.executionModelRef()
	if def, ok := p.defs[ref]; ok {
		return def
	}
	if sc.hasJobExecution() && ref != "" {
		return ModelDef{}
	}
	return p.defs[p.defaultRef]
}

// kernelOptions binds each non-default model to its role. Build passes them to
// agentkit.New; nothing else may, because a role bound after the Kernel exists
// would never be resolved.
func (p ModelPolicy) kernelOptions() []agentkit.KernelOption {
	opts := make([]agentkit.KernelOption, 0, len(p.roles))
	for _, ref := range slices.Sorted(maps.Keys(p.roles)) {
		opts = append(opts, agentkit.WithModelRole(p.roles[ref], p.WrapClient(ref, p.clients[ref])))
	}
	return opts
}
