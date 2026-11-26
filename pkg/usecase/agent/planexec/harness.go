package planexec

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/react"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
)

//go:embed prompts/harness.md
var harnessPromptTmpl string

var harnessPromptTemplate = template.Must(template.New("planexec_harness").Parse(harnessPromptTmpl))

// harnessPromptInput is the data fed into prompts/harness.md.
type harnessPromptInput struct {
	// HostPrompt is the host's base persona prompt, not the rendered planner
	// prompt: the planner protocol does not apply to a single-agent run.
	HostPrompt string
	Language   string
	Context    string
	// AllowWrites mirrors Input.AllowSubAgentWrites. The run holds the whole
	// palette, so this is what tells it whether the write tools are its to use.
	AllowWrites bool
	// Structured says the final answer is a JSON object rather than prose.
	Structured    bool
	AllowQuestion bool
}

// harnessTaskID labels the single child of a harness-driven run.
const harnessTaskID = "harness"

// launchHarness replaces the whole plan / tasks / replan / final sequence with
// ONE child that an external harness runs to completion.
//
// The child is the ordinary task agent, so it is spawned, budgeted and given its
// tools exactly as a planned task is; what differs is that it receives the whole
// palette and the host's own prompt, and that the react strategy hands its loop
// to the external harness. No LLM call is made by this Process at all.
//
// It is reached only while a harness is installed (see stepPlan), which is the
// eval command's simulation mode. The planner's judgement — what to investigate
// in parallel, when to stop — is exactly what such a run does not exercise.
func (s *strategy[T]) launchHarness(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[Output[T]], error) {
	var buf bytes.Buffer
	if err := harnessPromptTemplate.Execute(&buf, harnessPromptInput{
		HostPrompt:    st.Input.SystemPrompt,
		Language:      st.Input.LanguageLabel,
		Context:       st.Input.TaskContext,
		AllowWrites:   st.Input.AllowSubAgentWrites,
		Structured:    !s.cfg.TextOnly,
		AllowQuestion: st.Input.AllowQuestion,
	}); err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: render the harness prompt")
	}

	input := react.Input{SystemPrompt: buf.String(), Prompt: st.NextInput}
	if input.Prompt == "" {
		input.Prompt = st.Input.UserInput
	}
	schema, err := s.harnessSchema(st.Input.AllowQuestion)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: encode the external response schema")
	}
	input.OutputSchema = schema

	// The single child does all of the run's work, so it gets all of the run's
	// remaining allowance, as the direct path's child does.
	remaining, _, budgeted := s.remainingBudget(sys)
	pid, err := s.spawnChild(ctx, sys, harnessTaskID, input, st.Input.KnownToolIDs,
		budgetBound(remaining, budgeted))
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, err
	}

	st.Current = []taskRef{{TaskID: harnessTaskID, Title: "Single-agent run", ProcessID: pid}}
	st.Direct = true
	st.Harness = true
	// Per attempt, so a re-run after a rejected output opens a fresh await rather
	// than reusing a resolved one.
	st.RoundKey = agentkit.AwaitKey(fmt.Sprintf("%s:%d:%d", harnessTaskID, st.Round, st.FinalRetries))
	st.Phase = phaseCollect
	return st, agentkit.Suspend[Output[T]](agentkit.WaitChildren(st.RoundKey, pid)), nil
}

func (s *strategy[T]) harnessSchema(allowQuestion bool) (json.RawMessage, error) {
	var answer json.RawMessage
	if !s.cfg.TextOnly {
		var err error
		answer, err = harness.SchemaJSON(s.outputSchema)
		if err != nil {
			return nil, err
		}
	}
	if !allowQuestion {
		return answer, nil
	}
	if s.cfg.TextOnly {
		answer = json.RawMessage(`{"type":"string","minLength":1}`)
	}
	question, err := harness.SchemaJSON(questionSchema())
	if err != nil {
		return nil, err
	}
	variant := func(name string, schema json.RawMessage) map[string]any {
		return map[string]any{
			"type": "object", "properties": map[string]json.RawMessage{name: schema},
			"required": []string{name}, "additionalProperties": false,
		}
	}
	return json.Marshal(map[string]any{"oneOf": []any{variant("answer", answer), variant("question", question)}})
}

// collectHarness turns the harness child's answer into the run's output.
//
// A text host takes the answer as it is. A structured host decodes and validates
// it through the same steps stepFinal applies; a rejected output re-runs the
// child with the reason, but only for a host whose run performs no writes — a
// re-run repeats every tool call, and repeating a post or an assignment is worse
// than reporting that no conclusion was reached.
func (s *strategy[T]) collectHarness(ctx context.Context, sys agentkit.Syscalls, st state, results []agentkit.ChildResult) (state, agentkit.Decision[Output[T]], error) {
	text := directReplyText(st.Current, results)
	if text == "" {
		reason := "the harness run produced no output"
		for _, child := range results {
			if child.Failure != nil && child.Failure.Message != "" {
				reason = "the harness run failed: " + child.Failure.Message
				break
			}
		}
		return st, agentkit.Done(Output[T]{Kind: OutputFallback, FallbackReason: reason}), nil
	}

	var (
		out  *T
		derr error
	)
	if st.Input.AllowQuestion {
		var envelope struct {
			Answer   json.RawMessage `json:"answer"`
			Question *Question       `json:"question"`
		}
		derr = json.Unmarshal([]byte(text), &envelope)
		if derr == nil {
			switch {
			case envelope.Question != nil && len(envelope.Answer) == 0:
				derr = envelope.Question.Validate()
				if derr == nil {
					return s.handleQuestion(ctx, sys, st, envelope.Question)
				}
			case envelope.Question == nil && len(envelope.Answer) > 0:
				text = string(envelope.Answer)
				if s.cfg.TextOnly {
					derr = json.Unmarshal(envelope.Answer, &text)
					if derr == nil && strings.TrimSpace(text) == "" {
						derr = goerr.New("planexec: the external answer is empty")
					}
				}
			default:
				derr = goerr.New("planexec: return exactly one answer or question")
			}
		}
	}
	if derr == nil {
		if s.cfg.TextOnly {
			return st, agentkit.Done(Output[T]{Kind: OutputFinal, Text: text}), nil
		}
		out, derr = s.decodeFinal([]byte(text))
		if derr == nil {
			derr = (*out).Validate()
		}
		if derr == nil {
			derr = s.runFinalizers(ctx, sys.Metadata(), out)
		}
		if derr == nil {
			return st, agentkit.Done(Output[T]{Kind: OutputFinal, Data: out}), nil
		}
	}

	st.FinalRetries++
	if st.FinalRetries > finalOutputMaxRetry || st.Input.AllowSubAgentWrites {
		return st, agentkit.Done(Output[T]{Kind: OutputFallback, FallbackReason: derr.Error()}), nil
	}
	errutil.Handle(ctx, goerr.Wrap(derr, "planexec: harness output rejected",
		goerr.T(errutil.TagBenign)), "planexec: harness output rejected")
	retry, err := renderFinalRetryInput(derr)
	if err != nil {
		return st, agentkit.Decision[Output[T]]{}, goerr.Wrap(err, "planexec: build the harness output correction")
	}
	if st.NextInput == "" {
		st.NextInput = st.Input.UserInput
	}
	st.NextInput += "\n\n" + retry
	return s.launchHarness(ctx, sys, st)
}
