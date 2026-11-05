package kernel

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

const usagePendingEvent agentkit.EventType = "hecatoncheires.usage.pending"
const usageCompletedEvent agentkit.EventType = "hecatoncheires.usage.completed"

// durableUsageRecord contains billing evidence only. One attempt has two
// append-only records so a worker death after dispatch remains visibly unknown.
type durableUsageRecord struct {
	AttemptID   string    `json:"attempt_id"`
	Ancestors   []string  `json:"ancestors"`
	StageRole   StageRole `json:"stage_role"`
	StageOwner  StageRole `json:"stage_owner,omitempty"`
	WorkspaceID string    `json:"workspace_id"`
	JobID       string    `json:"job_id"`
	usageCall
	Complete bool `json:"complete"`
}

type durableUsageLedger struct {
	mu     sync.RWMutex
	repo   agentkit.Repository
	calls  map[string]map[string]durableUsageRecord
	loaded map[string]bool
	active map[string]int
	scopes map[string]usageScopeIdentity
}

type usageScopeIdentity struct {
	WorkspaceID string
	JobID       string
	RunID       string
}

func usageIdentity(sc Scope) usageScopeIdentity {
	return usageScopeIdentity{WorkspaceID: sc.WorkspaceID, JobID: sc.JobID, RunID: sc.JobRunID}
}

func newDurableUsageLedger() *durableUsageLedger {
	return &durableUsageLedger{calls: make(map[string]map[string]durableUsageRecord), loaded: make(map[string]bool), active: make(map[string]int), scopes: make(map[string]usageScopeIdentity)}
}

func (l *durableUsageLedger) bind(repo agentkit.Repository) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.repo = repo
}

func (l *durableUsageLedger) merge(root string, c durableUsageRecord) {
	if l.calls[root] == nil {
		l.calls[root] = make(map[string]durableUsageRecord)
	}
	// A concurrent load may return an older pending snapshot after completion.
	if prev, ok := l.calls[root][c.AttemptID]; ok && prev.Complete {
		return
	}
	l.calls[root][c.AttemptID] = c
}

func (l *durableUsageLedger) load(ctx context.Context, root string) error {
	l.mu.RLock()
	repo := l.repo
	l.mu.RUnlock()
	if repo == nil {
		return goerr.New("durable job usage repository is not bound")
	}
	proc, err := repo.GetProcess(ctx, agentkit.ProcessID(root))
	if err != nil {
		return goerr.Wrap(err, "restore durable job usage scope")
	}
	identity := usageIdentity(ScopeFrom(proc.Metadata))
	events, err := repo.ListEvents(ctx, agentkit.ProcessID(root), agentkit.EventQuery{})
	if err != nil {
		return goerr.Wrap(err, "restore durable job usage")
	}
	var records []durableUsageRecord
	for _, event := range events {
		if event.Type != usagePendingEvent && event.Type != usageCompletedEvent {
			continue
		}
		var c durableUsageRecord
		if err := json.Unmarshal(event.Payload, &c); err != nil {
			return goerr.Wrap(err, "decode durable job usage")
		}
		if c.AttemptID == "" || c.RootID != root || c.RunID != identity.RunID ||
			c.WorkspaceID != identity.WorkspaceID || c.JobID != identity.JobID ||
			c.Complete != (event.Type == usageCompletedEvent) {
			return goerr.New("invalid durable job usage attribution")
		}
		records = append(records, c)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range records {
		l.merge(root, c)
	}
	l.loaded[root] = true
	l.scopes[root] = identity
	return nil
}

func (l *durableUsageLedger) append(ctx context.Context, c durableUsageRecord) error {
	l.mu.RLock()
	identity, loaded := l.scopes[c.RootID]
	l.mu.RUnlock()
	if !loaded || identity.WorkspaceID != c.WorkspaceID || identity.JobID != c.JobID || identity.RunID != c.RunID {
		return goerr.New("durable job usage does not match its loaded run scope")
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	typ := usagePendingEvent
	if c.Complete {
		typ = usageCompletedEvent
	}
	event := &agentkit.Event{ID: agentkit.EventID(uuid.Must(uuid.NewV7()).String()), ProcessID: agentkit.ProcessID(c.RootID), Type: typ, Payload: payload, At: time.Now()}
	l.mu.RLock()
	repo := l.repo
	l.mu.RUnlock()
	if repo == nil {
		return goerr.New("durable job usage repository is not bound")
	}
	if err := repo.Apply(ctx, agentkit.ChangeSet{Events: []*agentkit.Event{event}}); err != nil {
		return goerr.Wrap(err, "persist durable job usage")
	}
	l.mu.Lock()
	l.merge(c.RootID, c)
	l.mu.Unlock()
	return nil
}

func (l *durableUsageLedger) processCost(root, pid string) (pricing.NanoUSD, bool) {
	if l == nil {
		return 0, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	known := l.repo != nil && l.loaded[root]
	var cost pricing.NanoUSD
	for _, c := range l.calls[root] {
		contains := c.ProcessID == pid
		for _, id := range c.Ancestors {
			contains = contains || id == pid
		}
		if !contains {
			continue
		}
		cost += c.CostNanoUSD
		known = known && c.Complete && c.UsageKnown
	}
	return cost, known
}

// UsageCost returns the known subtotal and whether the entire pinned stage is
// priced. A false result must never be interpreted as a free call. Main roots
// include the entire run; an advisor stage includes its own task descendants.
func (p ModelPolicy) UsageCost(sc Scope) (pricing.NanoUSD, bool) {
	if p.durableUsage == nil {
		return 0, false
	}
	l := p.durableUsage
	l.mu.RLock()
	defer l.mu.RUnlock()
	known := l.repo != nil
	foundRoot := false
	var cost pricing.NanoUSD
	for root, identity := range l.scopes {
		if identity == usageIdentity(sc) && l.loaded[root] {
			foundRoot = true
		}
	}
	for _, calls := range l.calls {
		for _, c := range calls {
			if c.RunID != sc.JobRunID || c.WorkspaceID != sc.WorkspaceID || c.JobID != sc.JobID {
				continue
			}
			if sc.StageRole == StageRoleAdvisor && c.StageOwner != StageRoleAdvisor && (c.StageOwner != "" || c.StageRole != StageRoleAdvisor) {
				continue
			}
			cost += c.CostNanoUSD
			known = known && c.Complete && c.UsageKnown
		}
	}
	return cost, known && foundRoot
}

// LoadUsage restores persisted billing evidence and pins it for a continuous
// claim or completion callback. The caller must defer release; the last active
// caller evicts this root's cache, including on claim refusal or failure.
func (p ModelPolicy) LoadUsage(ctx context.Context, rootID agentkit.ProcessID) (func(), error) {
	if p.durableUsage == nil {
		return nil, goerr.New("durable job usage is unavailable")
	}
	l := p.durableUsage
	root := string(rootID)
	l.mu.Lock()
	l.active[root]++
	l.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active[root]--
			if l.active[root] == 0 {
				delete(l.active, root)
				delete(l.calls, root)
				delete(l.loaded, root)
				delete(l.scopes, root)
			}
		})
	}
	loaded := false
	defer func() {
		if !loaded {
			release()
		}
	}()
	if err := l.load(ctx, root); err != nil {
		return nil, err
	}
	loaded = true
	return release, nil
}

type durableUsageContextKey struct{}
type durableAncestorsKey struct{}

func durableUsageClaimMiddleware(p ModelPolicy) agentkit.ClaimMiddleware {
	return func(next agentkit.ClaimHandler) agentkit.ClaimHandler {
		return func(ctx context.Context, req *agentkit.ClaimRequest) (agentkit.ClaimOutcome, error) {
			sc := executionScopeForAgent(ScopeFrom(req.Process.Metadata), req.Process.Agent)
			if !sc.hasJobExecution() {
				return next(ctx, req)
			}
			release, err := p.LoadUsage(ctx, req.Process.RootID)
			if err != nil {
				return "", err
			}
			defer release()
			var ancestors []string
			parent := req.Process.ParentID
			seen := map[agentkit.ProcessID]bool{req.Process.ID: true}
			for parent != nil {
				if seen[*parent] {
					return "", goerr.New("cycle in job usage process ancestry")
				}
				seen[*parent] = true
				ancestors = append(ancestors, string(*parent))
				proc, err := p.durableUsage.repo.GetProcess(ctx, *parent)
				if err != nil {
					return "", err
				}
				parent = proc.ParentID
			}
			return next(context.WithValue(ctx, durableAncestorsKey{}, ancestors), req)
		}
	}
}

func durableUsageStepMiddleware(p ModelPolicy) agentkit.StepMiddleware {
	return func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(ctx context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			if req.Process != nil && executionScopeForAgent(ScopeFrom(req.Process.Metadata), req.Process.Agent).hasJobExecution() {
				if err := p.durableUsage.load(ctx, string(req.Process.RootID)); err != nil {
					return nil, err
				}
				var state struct {
					Phase string `json:"phase"`
				}
				if len(req.Process.State) > 0 {
					if err := json.Unmarshal(req.Process.State, &state); err != nil {
						return nil, goerr.Wrap(err, "decode job usage checkpoint phase")
					}
				}
				ctx = context.WithValue(ctx, usagePhaseKey{}, state.Phase)
			}
			return next(ctx, req)
		}
	}
}

func durableUsageForEffect(ctx context.Context, effect agentkit.EffectContext, kind string) durableUsageRecord {
	sc := executionScopeForAgent(ScopeFrom(effect.Metadata), effect.Agent)
	c := durableUsageRecord{usageCall: usageForEffect(ctx, effect, kind), StageRole: sc.StageRole, StageOwner: stageOwner(sc), WorkspaceID: sc.WorkspaceID, JobID: sc.JobID}
	if c.StageRole == "" {
		c.StageRole = StageRoleMain
	}
	c.Ancestors, _ = ctx.Value(durableAncestorsKey{}).([]string)
	return c
}

func durableUsageGenerateMiddleware(p ModelPolicy) agentkit.GenerateMiddleware {
	return func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(ctx context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			sc := executionScopeForAgent(ScopeFrom(req.Effect.Metadata), req.Effect.Agent)
			if !sc.hasJobExecution() {
				return next(ctx, req)
			}
			if sc.JobRunID == "" {
				return nil, goerr.New("durable job usage requires a job run id")
			}
			c := durableUsageForEffect(ctx, req.Effect, "parent")
			if req.Effect.ProcessID != req.Effect.RootID {
				c.Kind = "child"
			}
			if req.Role != nil {
				c.Role = req.Role.String()
			}
			return next(context.WithValue(ctx, durableUsageContextKey{}, c), req)
		}
	}
}

func durableUsageToolMiddleware(p ModelPolicy) agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			if !executionScopeForAgent(ScopeFrom(req.Effect.Metadata), req.Effect.Agent).hasJobExecution() {
				return next(ctx, req)
			}
			c := durableUsageForEffect(ctx, req.Effect, "internal")
			c.ToolName = req.Call.Name
			return next(context.WithValue(ctx, durableUsageContextKey{}, c), req)
		}
	}
}

type durableUsageClient struct {
	gollem.LLMClient
	ledger *durableUsageLedger
	ref    string
	def    ModelDef
}

func (c *durableUsageClient) Model() string {
	if named, ok := c.LLMClient.(gollem.ModelNamer); ok {
		return named.Model()
	}
	return ""
}
func (c *durableUsageClient) NewSession(ctx context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
	s, err := c.LLMClient.NewSession(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &durableUsageSession{Session: s, client: c}, nil
}

type durableUsageSession struct {
	gollem.Session
	client *durableUsageClient
}

func (s *durableUsageSession) Generate(ctx context.Context, in []gollem.Input, opts ...gollem.GenerateOption) (*gollem.Response, error) {
	c, enabled := ctx.Value(durableUsageContextKey{}).(durableUsageRecord)
	if !enabled {
		return s.Session.Generate(ctx, in, opts...)
	}
	if n, ok := s.client.LLMClient.(gollem.ModelNamer); ok && n.Model() != "" && n.Model() != s.client.def.Model {
		return nil, goerr.New("durable usage rate does not match the client's model")
	}
	c.AttemptID = uuid.Must(uuid.NewV7()).String()
	c.ModelRef = s.client.ref
	c.Model = s.client.def.Model
	if err := s.client.ledger.append(ctx, c); err != nil {
		return nil, err
	}
	started := time.Now()
	r, err := s.Session.Generate(ctx, in, opts...)
	c.Complete = true
	c.DurationMS = time.Since(started).Milliseconds()
	c.Failed = err != nil
	if r != nil {
		c.InputTokens, c.OutputTokens = int64(r.InputToken), int64(r.OutputToken)
		c.CacheReadInputTokens, c.CacheCreationInputTokens = int64(r.CacheReadInputToken), int64(r.CacheCreationInputToken)
		c.UsageKnown = responseUsageKnown(r)
		c.FinishReason = r.FinishReason
		if c.UsageKnown {
			c.CostNanoUSD = s.client.def.Rate.Cost(c.InputTokens, c.OutputTokens, c.CacheReadInputTokens, c.CacheCreationInputTokens)
		}
	}
	// Persist even after the caller cancelled: a provider response is still a
	// billable fact. Keep this independent of the transition's own checkpoint.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if persistErr := s.client.ledger.append(persistCtx, c); persistErr != nil {
		return nil, persistErr
	}
	return r, err
}

// gollem has no usage-presence bit. All-zero counters can mean usage was
// omitted, so they cannot establish that a provider call was free.
func responseUsageKnown(r *gollem.Response) bool {
	return r != nil && r.InputToken >= 0 && r.OutputToken >= 0 &&
		r.CacheReadInputToken >= 0 && r.CacheCreationInputToken >= 0 &&
		r.CacheReadInputToken+r.CacheCreationInputToken <= r.InputToken &&
		(r.InputToken > 0 || r.OutputToken > 0)
}
func (s *durableUsageSession) Stream(ctx context.Context, in []gollem.Input, opts ...gollem.GenerateOption) (<-chan *gollem.Response, error) {
	if _, enabled := ctx.Value(durableUsageContextKey{}).(durableUsageRecord); enabled {
		return nil, goerr.New("streaming is not supported by durable job usage accounting")
	}
	return s.Session.Stream(ctx, in, opts...)
}

// usageCall is one SDK Generate attempt, without prompts, results, tool arguments
// or credentials. Costs use the configured client reference's rate, not the
// model of the Process whose cumulative Metrics may include other models.
type usageCall struct {
	ProcessID string `json:"process_id,omitempty"`
	RootID    string `json:"root_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Phase     string `json:"phase"`
	// ParentOrChild retains the calling Process's place in the tree even for an
	// internal tool call; empty when no Process context was supplied.
	ParentOrChild string `json:"parent_or_child,omitempty"`
	// Kind is parent, child, internal (a tool's own LLM), or unattributed.
	Kind                     string          `json:"kind"`
	ToolName                 string          `json:"tool_name,omitempty"`
	StateSeq                 int             `json:"state_seq"`
	AttemptErrors            int             `json:"attempt_errors,omitempty"`
	UncleanReclaims          int             `json:"unclean_reclaims,omitempty"`
	Role                     string          `json:"role,omitempty"`
	ModelRef                 string          `json:"model_ref"`
	Model                    string          `json:"model"`
	InputTokens              int64           `json:"input_tokens"`
	OutputTokens             int64           `json:"output_tokens"`
	CacheReadInputTokens     int64           `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64           `json:"cache_creation_input_tokens"`
	CostNanoUSD              pricing.NanoUSD `json:"cost_nano_usd"`
	DurationMS               int64           `json:"duration_ms"`
	Failed                   bool            `json:"failed"`
	UsageKnown               bool            `json:"usage_known"`
	FinishReason             string          `json:"finish_reason,omitempty"`
}

type usagePhaseKey struct{}

func usageForEffect(ctx context.Context, effect agentkit.EffectContext, kind string) usageCall {
	phase, _ := ctx.Value(usagePhaseKey{}).(string)
	if phase == "" {
		phase = "unknown"
	}
	parentOrChild := "parent"
	if effect.ProcessID != effect.RootID {
		parentOrChild = "child"
	}
	return usageCall{
		ProcessID: string(effect.ProcessID), RootID: string(effect.RootID),
		RunID: ScopeFrom(effect.Metadata).JobRunID,
		Agent: string(effect.Agent), Phase: phase, Kind: kind, ParentOrChild: parentOrChild, StateSeq: effect.StateSeq,
		AttemptErrors: effect.Attempt.Errors, UncleanReclaims: effect.Attempt.UncleanReclaims,
	}
}

// WrapClient records every Generate by this model, including calls inside tools
// when the same wrapper is supplied to their dependency wiring. Build wraps its
// default and role clients automatically. The caller may also pass its wrapped
// client to tool dependencies so their LLM calls are accounted for. Rewrapping the
// same (policy ledger, reference) is a no-op.
func (p ModelPolicy) WrapClient(ref string, client gollem.LLMClient) gollem.LLMClient {
	if client == nil {
		return client
	}
	if wrapped, ok := client.(*durableUsageClient); ok && wrapped.ledger == p.durableUsage && wrapped.ref == ref {
		return client
	}
	if p.durableUsage == nil {
		return client
	}
	return &durableUsageClient{LLMClient: client, ledger: p.durableUsage, ref: ref, def: p.defs[ref]}
}
