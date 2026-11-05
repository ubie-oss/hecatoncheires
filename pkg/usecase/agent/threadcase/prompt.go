package threadcase

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/slackfmt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
)

// Mode discriminates the purpose of a thread-mode turn.
type Mode int

const (
	// ModeMention is a user @-mention in a case thread. The planner may
	// respond, update fields (materialize), or close the case.
	ModeMention Mode = iota
	// ModeMaterialize runs right after a case is auto-created from a
	// monitored-channel post. The planner investigates the message and emits
	// a materialize decision to fill title / description / fields.
	ModeMaterialize
	// ModeCreate runs when a monitored-channel post arrives but NO case exists
	// yet. The planner investigates / asks the user, and only commits a new
	// case (final create decision) once it can satisfy validation.
	ModeCreate
)

//go:embed prompts/system.md
var systemPromptTemplateText string

//go:embed prompts/user_input.md
var userInputTemplateText string

var (
	systemPromptTemplate = template.Must(template.New("threadcase_system").Parse(systemPromptTemplateText))
	userInputTemplate    = template.Must(template.New("threadcase_user_input").Parse(userInputTemplateText))
)

// systemPromptInput is the data prompts/system.md renders.
type systemPromptInput struct {
	// Mode is "create", "materialize" or "mention".
	Mode string
	// Case is nil when no case exists yet (ModeCreate).
	Case            *systemPromptCase
	Fields          []systemPromptField
	ClosedStatusIDs string
	SlackFormat     string
	// WorkspaceDescription is [workspace] description, rendered in ModeMention
	// only: it is the workspace's account of what a case is and how work in it
	// is organized, which a turn acting on an existing case has to follow.
	WorkspaceDescription string
	// WorkspacePrompt is the operator-supplied prompt for the turn's phase:
	// [case.prompts].create in ModeCreate, [case.prompts].mention in
	// ModeMention, empty in ModeMaterialize.
	WorkspacePrompt string
	// TriggerContext is rendered in ModeCreate only.
	TriggerContext string
}

type systemPromptCase struct {
	Title       string
	Description string
	// Assignees is always rendered, "(empty)" when unset: the agent has no
	// tool to read the case back, so an omitted line would leave it unable to
	// tell "no assignees" from "not shown" before calling case__assign.
	Assignees   string
	BoardStatus string
	FieldValues []systemPromptFieldValue
}

type systemPromptFieldValue struct {
	ID    string
	Value any
}

type systemPromptField struct {
	Name        string
	ID          string
	Type        string
	Required    bool
	Description string
	// Options is the comma-joined option ids.
	Options string
	// IsDate spells the RFC3339 format out: the validator rejects a bare date
	// like "2026-07-14".
	IsDate bool
	// Semantic and SemanticHint tell the planner the expected value shape up
	// front, since the validator rejects a value that does not fit.
	Semantic     string
	SemanticHint string
	// Pattern is "`<pattern>` — <hint>" for a text field that sets
	// validation.pattern, empty otherwise.
	Pattern string
}

// userInputTemplateInput is the data prompts/user_input.md renders.
type userInputTemplateInput struct {
	// HasSystemMessages / HasDeltaMessages keep a section heading even when
	// every message in it is the current mention and is skipped.
	HasSystemMessages bool
	SystemMessages    []userInputMessage
	HasDeltaMessages  bool
	DeltaMessages     []userInputMessage
	MentionSpeaker    string
	MentionText       string
}

type userInputMessage struct {
	Timestamp string
	Speaker   string
	Text      string
}

func modeName(mode Mode) string {
	switch mode {
	case ModeCreate:
		return "create"
	case ModeMaterialize:
		return "materialize"
	default:
		return "mention"
	}
}

// buildSystemPrompt renders the planner system prompt for a thread-mode turn.
// It inlines the case snapshot, the workspace field schema, and the board
// status vocabulary so the planner can fill fields and pick a close status.
func buildSystemPrompt(c *model.Case, ws *model.WorkspaceEntry, mode Mode, createInstruction string) (string, error) {
	in := systemPromptInput{
		Mode: modeName(mode),
		// How to write anything that goes to Slack. Placed before the
		// operator-supplied sections so the operator's text stays the last
		// word, as it is for every other host. It is rendered in every mode:
		// a create or materialize turn asks the user questions through Slack
		// too (AllowQuestion is unconditional in Durable.StartTurn), and the
		// section states its own scope — the case title, description and field
		// values it produces are stored records and are excluded from it.
		SlackFormat: slackfmt.Section(),
	}

	if c != nil {
		pc := &systemPromptCase{
			Title:       orPlaceholder(c.Title),
			Description: orPlaceholder(c.Description),
			Assignees:   orPlaceholder(strings.Join(c.AssigneeIDs, ", ")),
			BoardStatus: c.BoardStatus,
		}
		for _, id := range slices.Sorted(maps.Keys(c.FieldValues)) {
			pc.FieldValues = append(pc.FieldValues, systemPromptFieldValue{ID: id, Value: c.FieldValues[id].Value})
		}
		in.Case = pc
	}

	if ws != nil && ws.FieldSchema != nil {
		for _, f := range ws.FieldSchema.Fields {
			opts := make([]string, 0, len(f.Options))
			for _, o := range f.Options {
				opts = append(opts, o.ID)
			}
			in.Fields = append(in.Fields, systemPromptField{
				Name:         f.Name,
				ID:           f.ID,
				Type:         string(f.Type),
				Required:     f.Required,
				Description:  f.Description,
				Options:      strings.Join(opts, ", "),
				IsDate:       f.Type == types.FieldTypeDate,
				Semantic:     string(f.Semantic),
				SemanticHint: semantic.PromptHint(f.Semantic),
				Pattern:      f.Validation.Pattern.Label(),
			})
		}
	}

	if ws != nil && ws.CaseStatusSet != nil {
		in.ClosedStatusIDs = strings.Join(ws.CaseStatusSet.ClosedIDs(), ", ")
	}

	if mode == ModeCreate {
		// Workspace-specific instructions from TOML [case.prompts].create.
		if ws != nil && strings.TrimSpace(ws.CaseCreatePrompt) != "" {
			in.WorkspacePrompt = ws.CaseCreatePrompt
		}
		// Host-supplied trigger context (e.g. reaction-initiated creation):
		// per-turn, not per-workspace, and only meaningful while initializing.
		if strings.TrimSpace(createInstruction) != "" {
			in.TriggerContext = createInstruction
		}
	}

	if mode == ModeMention && ws != nil {
		in.WorkspaceDescription = strings.TrimSpace(ws.Workspace.Description)
		// Workspace-specific instructions from TOML [case.prompts].mention.
		if strings.TrimSpace(ws.CaseMentionPrompt) != "" {
			in.WorkspacePrompt = ws.CaseMentionPrompt
		}
	}

	var b strings.Builder
	if err := systemPromptTemplate.Execute(&b, in); err != nil {
		return "", goerr.Wrap(err, "failed to render thread-case system prompt",
			goerr.V("mode", in.Mode))
	}
	return b.String(), nil
}

// buildUserInput assembles the first user message handed to the planner. The
// turn's current time comes first; the system / delta conversation messages
// follow; the current mention is appended last (when it carries text). The
// mention is passed as a ConversationMessage so its author is rendered exactly
// like every other speaker — the agent needs the author's Slack user ID to
// satisfy a request like "assign me".
//
// The time is stated HERE rather than in the system prompt because a thread-mode
// turn continues the previous turn's conversation
// (Durable.inheritOpts → agentkit.WithInheritedHistory) and the system prompt is
// not part of that history. See agent.PlannerMessage.
func buildUserInput(now time.Time, systemMessages, deltaMessages []ConversationMessage, mention ConversationMessage) (string, error) {
	in := userInputTemplateInput{
		HasSystemMessages: len(systemMessages) > 0,
		SystemMessages:    toUserInputMessages(systemMessages, mention.Timestamp),
		HasDeltaMessages:  len(deltaMessages) > 0,
		DeltaMessages:     toUserInputMessages(deltaMessages, mention.Timestamp),
		MentionText:       mention.Text,
		MentionSpeaker:    speakerLabel(mention),
	}
	// The template falls back to a standing instruction when there is nothing
	// else to send: planexec rejects an empty user input at Validate, and a
	// materialize turn may have no mention text. The fallback lives in the
	// body rather than relying on the time section, so the time cannot stand
	// in for the instruction that belongs here.
	var b strings.Builder
	if err := userInputTemplate.Execute(&b, in); err != nil {
		return "", goerr.Wrap(err, "failed to render thread-case user input")
	}
	return agent.PlannerMessage{Now: now, Body: b.String()}.Render()
}

// toUserInputMessages drops the current mention (it is rendered on its own)
// and resolves each author label.
func toUserInputMessages(msgs []ConversationMessage, skipTS string) []userInputMessage {
	out := make([]userInputMessage, 0, len(msgs))
	for _, m := range msgs {
		if skipTS != "" && m.Timestamp == skipTS {
			continue
		}
		out = append(out, userInputMessage{Timestamp: m.Timestamp, Speaker: speakerLabel(m), Text: m.Text})
	}
	return out
}

// speakerLabel renders a message author as "Display Name (U123)", degrading to
// whichever half is known. Both halves are emitted because the mention-mode
// system prompt tells the agent to resolve a named person to a Slack user ID
// before calling case__assign / case__unassign: a display name on its own
// cannot satisfy that, and there is no user-directory tool to look one up.
func speakerLabel(m ConversationMessage) string {
	switch {
	case m.UserName != "" && m.UserID != "":
		return fmt.Sprintf("%s (%s)", m.UserName, m.UserID)
	case m.UserName != "":
		return m.UserName
	default:
		return m.UserID
	}
}

func orPlaceholder(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
