// Package evaltype holds the shared value types and small interfaces of the
// eval harness. It is a dependency-light leaf package so that env, driver,
// usersim, judge and report can all reference these types without forming an
// import cycle (notably: env collects ToolCallRecords and driver consumes
// *env.Env, so the records must live below both).
package evaltype

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

// Completer is the minimal LLM surface the eval components (judge, usersim,
// toolsim) need: produce a completion for a system+user prompt, optionally
// constrained to a JSON response schema. Keeping this behind an interface lets
// those components be unit-tested with a canned completer instead of driving
// the full gollem agent loop. The gollem-backed implementation lives in the
// llmrun package.
type Completer interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string, schema *gollem.Parameter) (string, error)
}

// QuestionType mirrors the planner's question item control type.
type QuestionType string

const (
	QuestionSelect      QuestionType = "select"
	QuestionMultiSelect QuestionType = "multi_select"
	QuestionFreeText    QuestionType = "free_text"
)

// QuestionItem is one item the agent asks the user.
type QuestionItem struct {
	ID      string
	Text    string
	Type    QuestionType
	Options []string
}

// Question is a clarification request surfaced by the agent during a turn.
type Question struct {
	Reason string
	Items  []QuestionItem
}

// Answer is the simulated user's reply to one QuestionItem. Value is used for
// select / free_text; Values for multi_select.
type Answer struct {
	ID     string
	Value  string
	Values []string
}

// Answers bundles the replies to all items of a Question.
type Answers struct {
	Items []Answer
}

// Simulator produces answers to an agent question. It is implemented by the
// usersim package; the persona it answers with is bound at construction time so
// this interface stays free of scenario types.
type Simulator interface {
	Answer(ctx context.Context, q Question) (Answers, error)
}

// ToolCallRecord captures one tool invocation for verification and diagnosis.
// Result holds the (simulated or real) returned content; it is rendered in full
// in diagnostic dumps and summarized for the judge.
type ToolCallRecord struct {
	Seq    int    // 1-based call order
	Tool   string // tool name (e.g. slack_search)
	Args   any    // arguments (query etc.)
	Mode   string // "sim" | "live"
	Result any    // returned content
}

// TurnRecord is one workflow turn in the transcript.
type TurnRecord struct {
	Turn     int
	Mode     string // "materialize" | "mention"
	Input    string
	Question *Question
	Answer   *Answers
	Decision string // terminal decision kind, if any
	Reply    string // reply text posted to the thread, if any
}

// Artifact is the workflow-type-specific produced result. Render returns a
// human-readable snapshot the judge evaluates each check against.
type Artifact interface {
	Kind() string
	Render() string
}

// CaseArtifact is the thread-mode produced result: the final case state plus
// the turn transcript and the tool-call trajectory.
type CaseArtifact struct {
	Case       *model.Case
	Transcript []TurnRecord
	ToolCalls  []ToolCallRecord
	// Posts are the Slack messages the turn produced (acknowledgement, progress,
	// the creation notice), in order.
	Posts []SlackPost
}

// Kind implements Artifact.
func (a *CaseArtifact) Kind() string { return "case" }

// Render implements Artifact: a structured, judge-readable snapshot. Field
// values and tool calls are surfaced explicitly so objectively-decidable checks
// (state grounding) do not require re-parsing prose.
func (a *CaseArtifact) Render() string {
	var b strings.Builder
	b.WriteString("# Produced case\n")
	if a.Case != nil {
		fmt.Fprintf(&b, "- Title: %s\n", a.Case.Title)
		fmt.Fprintf(&b, "- Description: %s\n", a.Case.Description)
		fmt.Fprintf(&b, "- Lifecycle status: %s\n", string(a.Case.Status))
		if a.Case.BoardStatus != "" {
			fmt.Fprintf(&b, "- Board status: %s\n", a.Case.BoardStatus)
		}
		if len(a.Case.FieldValues) > 0 {
			b.WriteString("- Field values:\n")
			ids := make([]string, 0, len(a.Case.FieldValues))
			for id := range a.Case.FieldValues {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				fmt.Fprintf(&b, "  - %s: %v\n", id, a.Case.FieldValues[id].Value)
			}
		}
	}

	if len(a.Transcript) > 0 {
		b.WriteString("\n# Transcript\n")
		for _, tr := range a.Transcript {
			fmt.Fprintf(&b, "## Turn %d (%s)\n", tr.Turn, tr.Mode)
			if tr.Input != "" {
				fmt.Fprintf(&b, "- input: %s\n", tr.Input)
			}
			if tr.Question != nil {
				fmt.Fprintf(&b, "- agent asked: %s\n", tr.Question.Reason)
				for _, it := range tr.Question.Items {
					fmt.Fprintf(&b, "  - [%s] %s\n", it.Type, it.Text)
				}
			}
			if tr.Answer != nil {
				for _, ans := range tr.Answer.Items {
					fmt.Fprintf(&b, "- user answered (%s): %s%s\n", ans.ID, ans.Value, strings.Join(ans.Values, ", "))
				}
			}
			if tr.Decision != "" {
				fmt.Fprintf(&b, "- decision: %s\n", tr.Decision)
			}
			if tr.Reply != "" {
				fmt.Fprintf(&b, "- reply: %s\n", tr.Reply)
			}
		}
	}

	if len(a.ToolCalls) > 0 {
		b.WriteString("\n# Tool calls\n")
		for _, tc := range a.ToolCalls {
			fmt.Fprintf(&b, "%d. %s [%s] args=%v\n", tc.Seq, tc.Tool, tc.Mode, tc.Args)
		}
	} else {
		b.WriteString("\n# Tool calls\n(none)\n")
	}

	renderSlackPosts(&b, a.Posts)
	return b.String()
}

// JobOutcome is the terminal result of a Job run.
type JobOutcome struct {
	Stage   string // "SUCCESS" | "FAILED" | "RUNNING"
	Error   string
	Summary string // final LLM response text
}

// JobArtifact is the job workflow's produced result: the job run outcome, the
// case state after the run, the actions present, and the tool-call trajectory
// (from the job run event timeline).
type JobArtifact struct {
	JobID     string
	Outcome   JobOutcome
	Case      *model.Case
	Actions   []*model.Action
	ToolCalls []ToolCallRecord
	// StartingDescription and StartingThread are the evidence seeded before the
	// first Job. They let the judge check whether the result is grounded.
	StartingDescription string
	StartingThread      []string
	// Posts are the Slack messages the run produced, in order: both the ones the
	// Job's agent posted through a tool and the ones the use case posted on its
	// own (e.g. the assign notice).
	Posts []SlackPost
}

// SlackPost is one outbound Slack message recorded during a run.
type SlackPost struct {
	Kind      string `json:"kind"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Text      string `json:"text"`
	TS        string `json:"ts"`
}

func renderSlackPosts(b *strings.Builder, posts []SlackPost) {
	chars, mentions := 0, 0
	for _, p := range posts {
		chars += utf8.RuneCountInString(p.Text)
		mentions += len(slackMentions(p.Text))
	}
	fmt.Fprintf(b, "\n# Slack posts (posts=%d chars=%d mentions=%d)\n", len(posts), chars, mentions)
	if len(posts) == 0 {
		b.WriteString("(none)\n")
	}
	for i, p := range posts {
		fmt.Fprintf(b, "## Post %d [%s] channel=%s thread_ts=%s mentions=%v\n%s\n", i+1, p.Kind, p.ChannelID, p.ThreadTS, slackMentions(p.Text), p.Text)
	}
}

// slackMentionPattern matches the Slack syntaxes that notify someone: a user,
// a user group, or a channel-wide broadcast.
var slackMentionPattern = regexp.MustCompile(`<@[UW][A-Z0-9]+(\|[^>]*)?>|<!subteam\^[A-Z0-9]+(\|[^>]*)?>|<!(here|channel|everyone)>`)

// slackMentions returns the notifying mentions found in text.
func slackMentions(text string) []string {
	return slackMentionPattern.FindAllString(text, -1)
}

// Kind implements Artifact.
func (a *JobArtifact) Kind() string { return "job" }

// Render implements Artifact: a judge-readable snapshot of what the job did.
func (a *JobArtifact) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Job run: %s\n", a.JobID)
	fmt.Fprintf(&b, "- Outcome: %s\n", a.Outcome.Stage)
	if a.Outcome.Error != "" {
		fmt.Fprintf(&b, "- Error: %s\n", a.Outcome.Error)
	}
	if a.Outcome.Summary != "" {
		fmt.Fprintf(&b, "- Summary: %s\n", a.Outcome.Summary)
	}

	if a.StartingDescription != "" || len(a.StartingThread) > 0 {
		b.WriteString("\n# Case before run\n")
		if a.StartingDescription != "" {
			fmt.Fprintf(&b, "- Description: %s\n", a.StartingDescription)
		}
		for _, message := range a.StartingThread {
			fmt.Fprintf(&b, "- Thread message: %s\n", message)
		}
	}

	b.WriteString("\n# Case after run\n")
	if a.Case != nil {
		fmt.Fprintf(&b, "- Title: %s\n", a.Case.Title)
		fmt.Fprintf(&b, "- Lifecycle status: %s\n", string(a.Case.Status))
		if a.Case.BoardStatus != "" {
			fmt.Fprintf(&b, "- Board status: %s\n", a.Case.BoardStatus)
		}
		if len(a.Case.FieldValues) > 0 {
			b.WriteString("- Field values:\n")
			ids := make([]string, 0, len(a.Case.FieldValues))
			for id := range a.Case.FieldValues {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				fmt.Fprintf(&b, "  - %s: %v\n", id, a.Case.FieldValues[id].Value)
			}
		}
	}

	if a.Case != nil {
		fmt.Fprintf(&b, "- Assignees: %s\n", strings.Join(a.Case.AssigneeIDs, ", "))
	}

	renderSlackPosts(&b, a.Posts)

	b.WriteString("\n# Actions\n")
	if len(a.Actions) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, ac := range a.Actions {
			fmt.Fprintf(&b, "- [%d] %s (status=%s)\n", ac.ID, ac.Title, string(ac.Status))
		}
	}

	b.WriteString("\n# Tool calls\n")
	if len(a.ToolCalls) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, tc := range a.ToolCalls {
			fmt.Fprintf(&b, "%d. %s args=%v\n", tc.Seq, tc.Tool, tc.Args)
		}
	}

	return b.String()
}

// CheckVerdict is the judge's assessment of one check.
type CheckVerdict struct {
	ID       string
	Question string
	Passed   bool
	Reason   string
}

// Score is the informational pass ratio for a scenario (not an automatic gate).
type Score struct {
	Passed int
	Total  int
}

// Status discriminates whether a scenario ran to completion or errored.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// ScenarioResult is the per-scenario outcome the reporter renders. The final
// OK/NG decision is left to a human reviewer; there is deliberately no overall
// pass/fail field.
type ScenarioResult struct {
	ScenarioID string
	EvalID     string
	Workflow   string
	Status     string // StatusOK | StatusError
	Err        string
	Score      Score
	Checks     []CheckVerdict
	Artifact   Artifact
	DumpDir    string
}
