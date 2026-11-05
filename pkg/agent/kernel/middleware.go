package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/masq"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/agenttrace"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/runtrace"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

type traceHandlerKey struct{}

// withTraceHandler stashes the claim's trace handler so the effect middleware
// can find it. It is claim-scoped state living in a context, not cross-request
// state: it is created when a worker takes a Process and gone when it lets go.
func withTraceHandler(ctx context.Context, h trace.Handler) context.Context {
	return context.WithValue(ctx, traceHandlerKey{}, h)
}

func traceHandlerFrom(ctx context.Context) trace.Handler {
	h, _ := ctx.Value(traceHandlerKey{}).(trace.Handler)
	return h
}

// claimMiddleware brackets a worker's whole run on one Process. It is where the
// request-scoped context a transition needs is assembled — logger fields, the
// user's language, the access actor — and where the trace sinks are opened and
// flushed.
//
// agentkit calls it once per claim, not once per transition, which is exactly
// the lifetime a trace span and a language binding want.
func claimMiddleware(d Deps) agentkit.ClaimMiddleware {
	return func(next agentkit.ClaimHandler) agentkit.ClaimHandler {
		return func(ctx context.Context, req *agentkit.ClaimRequest) (agentkit.ClaimOutcome, error) {
			proc := req.Process
			sc := ScopeFrom(proc.Metadata)

			ctx = logging.With(ctx, logging.From(ctx).With(
				"process_id", string(proc.ID),
				"root_id", string(proc.RootID),
				"agent", string(proc.Agent),
				"state_seq", proc.StateSeq,
			))
			if sc.Lang != "" {
				ctx = i18n.ContextWithLang(ctx, i18n.Lang(sc.Lang))
			}
			// The actor was validated before Spawn (ValidateSpawn); this is where
			// it becomes the access scope every tool in the claim runs under.
			//
			// A Process that reaches here without one is NOT refused. Refusing a
			// claim puts the row back as pending with a backoff and never
			// consumes the retry budget, so a permanent metadata fault would
			// requeue forever and hold its Subject with it — no further turn on
			// that thread could ever start. The capability is withdrawn instead:
			// the tool factory hands such a run nothing (see NewToolFactory), so
			// it cannot act, and it ends on its own.
			if sc.ActorUserID != "" {
				ctx = auth.ContextWithToken(ctx, &auth.Token{Sub: sc.ActorUserID})
			}

			recorder := trace.New(
				trace.WithRepository(d.Trace),
				trace.WithTraceID(claimTraceID(proc)),
				trace.WithMetadata(claimTraceMetadata(proc, sc)),
			)
			// Two sinks: the Cloud Storage archive (full fidelity, one object per
			// claim) and the JobRunEvent timeline the run-detail page reads.
			//
			// A fresh timeline Handler per claim is correct BECAUSE the Sequence is
			// allocated by the repository inside each write. Several Handlers on one
			// run — this claim's, a later claim's on another instance, and the run
			// owner's RUN_ERROR — therefore append into a single ordered timeline
			// with nothing shared between them.
			handler := trace.Handler(recorder)
			if timeline := runTimeline(d, proc, sc); timeline != nil {
				handler = trace.Multi(recorder, timeline)
			}
			ctx = withTraceHandler(ctx, handler)

			// The root span has to be opened here, and it has to bracket the
			// whole claim. gollem's recorder only starts a trace on
			// StartAgentExecute, and a child span with no trace to hang off is
			// dropped — so without this the archive of every run would be empty.
			ctx = handler.StartAgentExecute(ctx)
			outcome, err := next(ctx, req)
			handler.EndAgentExecute(ctx, err)

			// context.WithoutCancel so the flush still reaches storage when the
			// claim ended because the caller's context was cancelled — which is
			// exactly when the trace is worth the most.
			flushCtx := context.WithoutCancel(ctx)
			if ferr := recorder.Finish(flushCtx); ferr != nil {
				errutil.Handle(flushCtx, goerr.Wrap(ferr, "persist agent claim trace",
					goerr.V("process", proc.ID)), "persist agent claim trace")
			}
			return outcome, err
		}
	}
}

// runTimeline builds the JobRunEvent sink for a run that keeps a run record, or
// nil for one that does not.
//
// Every identifier is required: an event without them cannot be attributed to a
// run, and the repository would reject it on every append. Returning nil instead
// leaves the archive as the run's only trace, which is the right outcome for a
// run that was never meant to appear on the case agent page.
//
// It takes the Process as well as the Scope because the Scope cannot tell one
// Process of a run from another: a sub-agent inherits its parent's metadata, so
// every Process of a run resolves to the SAME Scope. The Process id is what
// distinguishes them, and it is the only thing here that does.
func runTimeline(d Deps, proc *agentkit.Process, sc Scope) *runtrace.Handler {
	if d.Tools.Repo == nil || sc.WorkspaceID == "" || sc.CaseID == 0 ||
		sc.JobID == "" || sc.JobRunID == "" {
		return nil
	}
	var processID string
	if proc != nil {
		processID = string(proc.ID)
	}
	return runtrace.NewHandler(d.Tools.Repo.JobRunEvent(), runtrace.Routing{
		WorkspaceID: sc.WorkspaceID,
		CaseID:      sc.CaseID,
		JobID:       sc.JobID,
		RunID:       sc.JobRunID,
		TraceID:     sc.JobRunID,
		ProcessID:   processID,
	}, nil)
}

// claimTraceID names one claim's archive object. A Process runs over many
// claims, possibly on different instances, and trace.Repository.Save overwrites
// by id — so a per-Process id would let a later, partial claim replace the
// complete archive of an earlier one.
//
// The committed transition count alone is not enough: a claim that dies before
// committing anything is reclaimed at the SAME StateSeq, and the reclaim would
// overwrite the archive of the attempt that failed — which is the one worth
// keeping. The lease token is the identity that is unique per claim, so it goes
// in too; StateSeq stays because it is what orders the archives.
func claimTraceID(proc *agentkit.Process) string {
	return fmt.Sprintf("%s.%d.%s", proc.ID, proc.StateSeq, proc.LeaseToken)
}

func claimTraceMetadata(proc *agentkit.Process, sc Scope) trace.TraceMetadata {
	labels := map[string]string{
		// session_id is required by the memory trace repository and is what
		// existing trace consumers key on, so the root Process id takes that
		// slot: it is the identifier that spans a whole run.
		"session_id":   string(proc.RootID),
		"process_id":   string(proc.ID),
		"agent":        string(proc.Agent),
		"workspace_id": sc.WorkspaceID,
		"channel_id":   sc.ChannelID,
		"thread_ts":    sc.ThreadTS,
	}
	if sc.SessionID != "" {
		labels["slack_session_id"] = sc.SessionID
	}
	if sc.CaseID != 0 {
		labels["case_id"] = fmt.Sprintf("%d", sc.CaseID)
	}
	if sc.JobID != "" {
		labels["job_id"] = sc.JobID
	}
	if sc.JobRunID != "" {
		labels["job_run_id"] = sc.JobRunID
	}
	return trace.TraceMetadata{Labels: labels}
}

// modelRoleMiddleware points a run's LLM calls at the model its scope names.
//
// It is a middleware because that is the only seam where a per-RUN model choice
// can be made: agentkit binds a role to a client when the Kernel is built, and a
// Strategy is registered once at startup and then serves every run — so neither
// can carry a value that differs per Job. The role each strategy asks for
// (planexec's planner and finalizer roles) is left in place when the run names
// no model of its own, and those roles are deliberately unbound, so the call
// falls through to the Kernel's default client exactly as before.
//
// A reference name with no role — an unknown one, or the default model's — is
// left alone rather than refused. ModelPolicy prices such a run at the default
// rate for the same reason, so the model it generates with and the rate it is
// metered at stay the same model's.
func modelRoleMiddleware(policy ModelPolicy) agentkit.GenerateMiddleware {
	return func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(ctx context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			sc := executionScopeForAgent(ScopeFrom(req.Effect.Metadata), req.Effect.Agent)
			if sc.hasJobExecution() {
				if err := sc.JobExecution.Validate(); err != nil {
					return nil, goerr.Wrap(err, "invalid pinned job execution")
				}
				ref := sc.executionModelRef()
				if ref == "" {
					ref = policy.defaultRef
				}
				if _, ok := policy.defs[ref]; !ok {
					return nil, goerr.New("pinned job execution model is not defined", goerr.V("ref", ref))
				}
				if ref != policy.defaultRef && policy.roles[ref] == nil {
					return nil, goerr.New("pinned job execution model has no client", goerr.V("ref", ref))
				}
				// A nil role explicitly selects the default positional client.
				req.Role = policy.roles[ref]
				return next(ctx, req)
			}

			if role := policy.roles[sc.LLMModel]; role != nil {
				req.Role = role
			}
			return next(ctx, req)
		}
	}
}

// promptCacheMiddleware turns provider prompt caching on for every Generate in
// this application.
//
// It is registered once here rather than passed per call because agentkit's
// GenerateRequest is the only place the setting can be reached: agentkit builds
// the gollem session itself, so a strategy cannot hand gollem.WithPromptCache to
// a client the way the pre-agentkit hosts each did. Losing those per-host calls
// in the migration is what took the cache hit rate on scheduled Job runs from
// ~80% of input tokens to zero (issue #266); Claude bills a cache read at about
// a tenth of the base input rate, so this is a cost control, not a nicety.
//
// Only the Claude provider acts on the flag — OpenAI and Gemini cache on their
// own and ignore it — so it is safe to apply unconditionally.
func promptCacheMiddleware() agentkit.GenerateMiddleware {
	return func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(ctx context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			req.LLMSessionOptions = append(req.LLMSessionOptions, gollem.WithSessionPromptCache(true))
			return next(ctx, req)
		}
	}
}

// generateMiddleware records one LLM call onto the claim's trace handler.
//
// agentkit builds its own gollem session per Generate and never runs
// gollem.WithTrace, so the Start/End pair the trace consumers expect has to be
// driven from here.
//
// The model name comes from a ModelCapture installed in the gollem trace context
// for the duration of the call, because agentkit.GenerateResult does not report
// which model answered. A capture rather than the run's own handler: the provider
// drives the same Start/End pair, and giving it the real handler would record the
// call twice.
func generateMiddleware() agentkit.GenerateMiddleware {
	return func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(ctx context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			h := traceHandlerFrom(ctx)
			if h == nil {
				return next(ctx, req)
			}
			spanCtx := h.StartLLMCall(ctx)
			capture := &agenttrace.ModelCapture{}
			res, err := next(trace.WithHandler(spanCtx, capture), req)
			h.EndLLMCall(spanCtx, agenttrace.LLMCallData(req, res, capture.Model()), err)
			return res, err
		}
	}
}

// toolCallMiddleware records one tool execution onto the claim's trace handler.
//
// The claim's handler is also published in the gollem trace context for the
// duration of the tool, so a tool that talks to an LLM itself — the knowledge
// tools' embedding calls, webfetch's page analysis — appears on the timeline as
// an LLM call nested inside its tool span. The pre-agentkit hosts got that for
// free from gollem.WithTrace, which put the handler in the context for the whole
// Execute; nothing has done it since the migration (issue #266). No duplicate
// arises: an agentkit Generate never runs inside a tool.
func toolCallMiddleware() agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			h := traceHandlerFrom(ctx)
			if h == nil {
				return next(ctx, req)
			}
			spanCtx := h.StartToolExec(ctx, req.Call.Name, req.Call.Arguments)
			out, err := next(trace.WithHandler(spanCtx, h), req)
			h.EndToolExec(spanCtx, out, err)
			return out, err
		}
	}
}

// toolArgsFeedbackMiddleware states the shape of the arguments a rejected tool
// call actually carried, so the model can tell WHICH argument it got wrong.
//
// gollem rejects a call whose arguments do not match the ToolSpec, but the
// message it produces names only the tool and the expectation — not the
// offending parameter. The parameter name IS recorded, as a goerr value on a
// per-parameter error that gollem keeps in an unexported slice outside the
// Unwrap chain, so it is rendered by neither Error() nor goerr.Values: it is
// unreachable from here and from the model alike. A model told only
// "expected array type" for a tool whose creates / updates / archives are all
// arrays cannot tell which one to repair, and re-emits the same call.
//
// The shape of what was sent supplies the missing half — exactly one argument
// will contradict the expectation. Only the JSON shape is rendered, never a
// value: this error reaches the run timeline and the operator's Sentry as well
// as the model, and a tool call's arguments carry case content.
//
// It wraps rather than replaces, so errors.Is(err, gollem.ErrToolArgsValidation)
// still holds and a caller discriminating on it is unaffected.
func toolArgsFeedbackMiddleware() agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			out, err := next(ctx, req)
			if err == nil || !errors.Is(err, gollem.ErrToolArgsValidation) {
				return out, err
			}
			return out, &toolArgsFeedbackError{cause: err, shape: describeArgs(req.Call.Arguments)}
		}
	}
}

// toolArgsFeedbackError appends the received argument shape to a rejected tool
// call's error.
//
// goerr.Wrap cannot express this: it renders as "message: cause", which would
// put the shape BEFORE the expectation it exists to explain, and the whole
// string is read by a model.
type toolArgsFeedbackError struct {
	cause error
	shape string
}

func (e *toolArgsFeedbackError) Error() string {
	return e.cause.Error() + "\nThe arguments received were: " + e.shape
}

func (e *toolArgsFeedbackError) Unwrap() error { return e.cause }

// toolErrorValuesMiddleware states the goerr values a failed tool call carried,
// so the model can tell WHY the call failed rather than only that it did.
//
// goerr renders a chain as "message: message: message" and keeps everything
// attached with goerr.V outside that string, reachable only through
// goerr.Values. The diagnostic half of a tool failure lives there: a rejected
// Jira search reaches the model as "Jira API returned non-2xx", while the reason
// Jira gave — "Error in the JQL Query: Expecting either 'OR' or 'AND' but got
// '...'" — sits in a `body` value the model never sees (ARGUS-96). A model told
// only that its search failed re-emits the same broken query, exactly as the
// unattributed argument rejection did in ARGUS-8S.
//
// An argument rejection is deliberately left alone: toolArgsFeedbackMiddleware
// already supplies that error's missing half, and the only value agentkit
// attaches there is the tool name gollem's own message already states.
//
// It wraps rather than replaces, so errors.Is against a sentinel the caller
// discriminates on — gollem.ErrToolArgsValidation, agentkit.ErrLimitExceeded,
// which react's stepTool must still recognise to stop the run — keeps holding.
func toolErrorValuesMiddleware() agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			out, err := next(ctx, req)
			if err == nil || errors.Is(err, gollem.ErrToolArgsValidation) {
				return out, err
			}
			values := describeErrorValues(goerr.Values(err))
			if values == "" {
				return out, err
			}
			return out, &toolErrorValuesError{cause: err, values: values}
		}
	}
}

// toolErrorValuesError appends a failed tool call's goerr values to its message.
//
// goerr.Wrap cannot express this: it renders as "message: cause", which would
// put the values BEFORE the failure they explain, and the whole string is read
// by a model.
type toolErrorValuesError struct {
	cause  error
	values string
}

func (e *toolErrorValuesError) Error() string {
	return e.cause.Error() + "\nThe failure reported:\n" + e.values
}

func (e *toolErrorValuesError) Unwrap() error { return e.cause }

// errorValueMaxLen bounds one rendered value. The API errors this exists to
// surface state their reason in the first line or two, while the same field can
// also hold a proxy's HTML error page — the jira toolset caps its own body
// snippet at 4096 bytes, which is already far past anything a model can act on.
const errorValueMaxLen = 512

// errorValuesMaxLen bounds the whole rendered block. An error accumulates values
// from every goerr.Wrap on the way up, and a tool that walks a batch can attach
// one per item.
const errorValuesMaxLen = 2048

// secretValueKeyPattern names the value keys whose content must never be
// rendered. Unlike the logger's sink, this line is sent to the LLM provider and
// reproduced in the Slack thread, so a key that merely SOUNDS like a credential
// is redacted: a false positive costs one diagnostic, a false negative puts a
// credential at a third party. Case-insensitive because this codebase writes
// both snake_case and camelCase value keys.
var secretValueKeyPattern = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|api[-_]?key|authorization|cookie)`)

// errorValueRedactor applies the project's redaction policy plus the key-name
// rule above. The policy is shared with the logger (logging.RedactOptions) so a
// value marked secret for one sink is secret for the other; the key-name rule is
// additional because a goerr value key is not a struct field and carries no tag.
var errorValueRedactor = masq.New(append(logging.RedactOptions(),
	masq.WithCensor(func(fieldName string, _ any, _ string) bool {
		return secretValueKeyPattern.MatchString(fieldName)
	}))...)

// describeErrorValues renders the values attached to an error chain as indented
// "key=value" lines in sorted order, so the same failure always reads the same
// way. goerr.Values already merges the whole chain, an outer wrap overriding an
// inner one under the same key.
//
// One value per line rather than a comma-separated list: the values that matter
// here are API error bodies and queries, which contain commas and equals signs
// of their own, and a line break is the one separator they cannot forge.
func describeErrorValues(values map[string]any) string {
	if len(values) == 0 {
		return ""
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, "  "+key+"="+renderErrorValue(key, values[key]))
	}

	rendered := strings.Join(lines, "\n")
	if len(rendered) > errorValuesMaxLen {
		// runtrace.Truncate rather than a plain slice: cutting at a byte offset
		// splits a multi-byte character, and the broken rune reaches the model,
		// the run timeline and Sentry alike.
		rendered = runtrace.Truncate(rendered, errorValuesMaxLen) + "..."
	}
	return rendered
}

// renderErrorValue redacts v under its own key and renders what survives.
//
// A single-line string is rendered as itself. The alternative — JSON for
// everything — escapes the quotes of the JSON error body that is the whole point
// of surfacing these values, and hands the model an escaped document to unpick.
// A string carrying a line break would break the one-value-per-line layout
// instead, so that one is JSON-encoded, as is every non-string value.
func renderErrorValue(key string, v any) string {
	redacted := errorValueRedactor(nil, slog.Any(key, v)).Value.Any()

	out := ""
	if s, ok := redacted.(string); ok && !strings.ContainsAny(s, "\n\r") {
		out = s
	} else {
		encoded, err := json.Marshal(redacted)
		if err != nil {
			// A value that does not marshal (a channel, a func, a cyclic
			// structure) still says something about the failure, so it is
			// described by type rather than dropped. Never by %v: that would
			// render the very content the marshaler refused to look at.
			return fmt.Sprintf("<%T>", redacted)
		}
		out = string(encoded)
	}

	if len(out) > errorValueMaxLen {
		return runtrace.Truncate(out, errorValueMaxLen) + "..."
	}
	return out
}

// argShapeMaxDepth bounds how far describeValue descends. Six levels reach the
// deepest value a tool spec in this application declares — memo's creates[] ->
// object -> fields[] -> object -> values[] -> string (pkg/agent/tool/memo,
// fieldsParameter). gollem rejects a value at that position as readily as a
// top-level one, so a shape that stops short of it says nothing about what was
// refused.
const argShapeMaxDepth = 6

// argShapeMaxLen bounds the rendered shape. A batch tool takes up to 50 entries
// whose objects carry a field list each, and an unbounded rendering would push
// the real expectation out of the model's view.
const argShapeMaxLen = 1000

// describeArgs renders the shape of one tool call's arguments: each name against
// the JSON type of its value, in sorted order so the same call always reads the
// same way.
func describeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "no arguments"
	}

	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	slices.Sort(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+describeValue(args[name], argShapeMaxDepth))
	}

	shape := strings.Join(parts, ", ")
	if len(shape) > argShapeMaxLen {
		// runtrace.Truncate rather than a plain slice: cutting at a byte offset
		// splits a multi-byte argument name or object key, and the broken rune
		// reaches the model, the run timeline and Sentry alike.
		shape = runtrace.Truncate(shape, argShapeMaxLen) + "..."
	}
	return shape
}

// describeValue names the JSON shape of v and never its value. The numeric cases
// list the concrete types a decoded tool call can hold: gollem produces float64,
// or json.Number for a literal a float64 cannot hold exactly, and a hand-built
// call in a test may hold any of the others. json.Number must stay on the number
// case — this string is the shape reported back to the model, and a Go type name
// is not a JSON shape it can act on.
func describeValue(v any, depth int) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, float32, int, int64, json.Number:
		return "number"
	case []any:
		return describeArray(t, depth)
	case map[string]any:
		if depth <= 1 || len(t) == 0 {
			return "object"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fields := make([]string, 0, len(keys))
		for _, k := range keys {
			fields = append(fields, k+": "+describeValue(t[k], depth-1))
		}
		return "object{" + strings.Join(fields, ", ") + "}"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// describeArray renders an array's length and the shape of its elements.
//
// Uniform elements collapse to one rendering; a mixed array is listed per index
// instead. That distinction is the point of the function rather than a nicety:
// gollem stops at the FIRST element that fails validation and reports the index
// as a goerr value on an error this application cannot reach, so the index is
// lost. Collapsing a mixed array onto its first element would then state that
// every entry has the shape of the one entry that happens to be valid — the
// refused entry would not appear at all.
func describeArray(arr []any, depth int) string {
	length := "array[" + strconv.Itoa(len(arr)) + "]"
	if depth <= 1 || len(arr) == 0 {
		return length
	}

	shapes := make([]string, 0, len(arr))
	uniform := true
	for _, item := range arr {
		shape := describeValue(item, depth-1)
		if len(shapes) > 0 && shape != shapes[0] {
			uniform = false
		}
		shapes = append(shapes, shape)
	}
	if uniform {
		return length + " of " + shapes[0]
	}

	entries := make([]string, 0, len(shapes))
	for i, shape := range shapes {
		entries = append(entries, strconv.Itoa(i)+": "+shape)
	}
	return length + "{" + strings.Join(entries, ", ") + "}"
}
