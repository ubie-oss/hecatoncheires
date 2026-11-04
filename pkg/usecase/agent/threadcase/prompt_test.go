package threadcase_test

import (
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/slackfmt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/threadcase"
)

// TestBuildSystemPrompt_GoldenMaterializeWithoutContext pins the whole
// rendered prompt for the smallest input, so any whitespace or wording change
// in prompts/system.md is caught.
func TestBuildSystemPrompt_GoldenMaterializeWithoutContext(t *testing.T) {
	want := "You are an investigation agent operating inside a Slack thread that represents a single case.\n" +
		"A new case was just created from the first message in this thread. Investigate the message (using the read-only tools when helpful) and emit a `materialize` decision that fills a concise title, a clear description, and any custom fields you are confident about.\n" +
		"You CANNOT create or manage Actions and you CANNOT create drafts — this is a thread-mode case. Sub-agent tools are read-only.\n" +
		"\n" +
		slackfmt.Section() + "\n\n"
	gt.Value(t, threadcase.BuildSystemPromptForTest(nil, nil, threadcase.ModeMaterialize, "")).Equal(want)
}

func TestBuildSystemPrompt_ThreadContext(t *testing.T) {
	ws := newThreadWorkspace()
	c := newThreadCase()
	c.Title = "Payment outage"

	prompt := threadcase.BuildSystemPromptForTest(c, ws, threadcase.ModeMention, "")
	gt.String(t, prompt).Contains("Payment outage")
	gt.String(t, prompt).Contains("severity")
	gt.String(t, prompt).Contains("DONE")
	gt.String(t, prompt).Contains("CANNOT create or manage Actions")
}

// TestBuildSystemPrompt_TemplateRendersEverySection drives every action of
// prompts/system.md with a fully populated input and pins the rendered
// blocks, so a template edit that drops or reshapes a section fails here.
func TestBuildSystemPrompt_TemplateRendersEverySection(t *testing.T) {
	ws := newThreadWorkspace()
	ws.FieldSchema = &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect, Required: true, Description: `pick "one"`, Options: []config.FieldOption{{ID: "high"}, {ID: "low"}}},
		{ID: "due", Name: "Due", Type: types.FieldTypeDate},
	}}
	ws.CaseCreatePrompt = "Operator create prompt"
	c := newThreadCase()
	c.Description = "desc"
	c.AssigneeIDs = []string{"U1", "U2"}
	c.FieldValues = map[string]model.FieldValue{
		"zeta":     {FieldID: "zeta", Value: "z"},
		"severity": {FieldID: "severity", Value: "high"},
	}

	prompt := threadcase.BuildSystemPromptForTest(c, ws, threadcase.ModeCreate, "reaction trigger")

	gt.String(t, prompt).Contains("You are an investigation agent operating inside a Slack thread that represents a single case.\nA message was posted in a monitored channel, but NO case exists yet.")
	gt.String(t, prompt).Contains("Sub-agent tools are read-only.\n\n# Current case\n")
	// Field values are listed in id order.
	gt.String(t, prompt).Contains("# Current case\n- Title: Initial title\n- Description: desc\n- Assignees (Slack user IDs): U1, U2\n- Current status: TRIAGE\n- Existing field values:\n  - severity: high\n  - zeta: z\n\n")
	gt.String(t, prompt).Contains("# Custom field schema (for materialize / create)\n" +
		"- Severity (id=severity, type=select) (required) description=\"pick \\\"one\\\"\" options=[high, low]\n" +
		"- Due (id=due, type=date) format=RFC3339 (e.g. 2026-07-14T00:00:00Z)\n\n")
	gt.String(t, prompt).Contains("# Closed status ids (for close): DONE\n\n" + slackfmt.Section() + "\n\n")
	gt.Bool(t, strings.HasSuffix(prompt, "# Workspace-specific instructions\nOperator create prompt\n# Trigger context\nreaction trigger\n")).True()
}

// With no case, no workspace and a mention turn, only the fixed sections are
// rendered — no empty headings.
func TestBuildSystemPrompt_TemplateOmitsEmptySections(t *testing.T) {
	prompt := threadcase.BuildSystemPromptForTest(nil, nil, threadcase.ModeMention, "ignored outside create")
	for _, heading := range []string{"# Current case", "# Custom field schema", "# Closed status ids", "# Workspace-specific instructions", "# Trigger context"} {
		gt.Bool(t, strings.Contains(prompt, heading)).False()
	}
	gt.String(t, prompt).Contains("A user mentioned you in this case thread.")
	gt.Bool(t, strings.HasSuffix(prompt, "never both.\n\n"+slackfmt.Section()+"\n\n")).True()
}

func TestBuildSystemPrompt_FieldSemantic(t *testing.T) {
	ws := newThreadWorkspace()
	ws.FieldSchema = &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "notify_channel", Name: "Notify", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		{ID: "note", Name: "Note", Type: types.FieldTypeText},
	}}

	prompt := threadcase.BuildSystemPromptForTest(newThreadCase(), ws, threadcase.ModeMention, "")
	gt.String(t, prompt).Contains("- Notify (id=notify_channel, type=text) semantic=slack_channel_id (" +
		semantic.PromptHint(types.SemanticSlackChannelID) + ")\n")
	gt.String(t, prompt).Contains("- Note (id=note, type=text)\n")
}

func TestBuildSystemPrompt_FieldPattern(t *testing.T) {
	ticket := types.TextPattern("[A-Z]{2,5}-[0-9]+")
	ws := newThreadWorkspace()
	ws.FieldSchema = &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "ticket_id", Name: "Ticket ID", Type: types.FieldTypeText, Validation: config.FieldValidation{Pattern: ticket}},
		{ID: "note", Name: "Note", Type: types.FieldTypeText},
	}}

	for _, mode := range []threadcase.Mode{threadcase.ModeCreate, threadcase.ModeMention} {
		prompt := threadcase.BuildSystemPromptForTest(newThreadCase(), ws, mode, "")
		gt.String(t, prompt).Contains("- Ticket ID (id=ticket_id, type=text) pattern=" + ticket.Label() + "\n")
		gt.String(t, prompt).Contains("- Note (id=note, type=text)\n")
	}
}

// A mention turn advertises the full case writer set, so the prompt must name
// every tool the sub-agents actually get — a prompt that omits one drives the
// model to tell the user it lacks the capability (the reported failure was a
// request to set assignees).
func TestBuildSystemPrompt_MentionModeAdvertisesEveryWriteTool(t *testing.T) {
	prompt := threadcase.BuildSystemPromptForTest(newThreadCase(), newThreadWorkspace(), threadcase.ModeMention, "")
	gt.String(t, prompt).Contains("case__update_case_status")
	gt.String(t, prompt).Contains("case__assign")
	gt.String(t, prompt).Contains("case__unassign")
	gt.String(t, prompt).Contains("case__update_case")
	// materialize replaces title/description wholesale, so the two content paths
	// must not be mixed within one turn.
	gt.String(t, prompt).Contains("pick one content path per turn")
}

// The agent has no tool to read the case back, so the current assignees must be
// in the snapshot — including when the list is empty, which is exactly the state
// a "set the assignees" request starts from.
func TestBuildSystemPrompt_RendersAssignees(t *testing.T) {
	ws := newThreadWorkspace()

	assigned := newThreadCase()
	assigned.AssigneeIDs = []string{"U-ONE", "U-TWO"}
	gt.String(t, threadcase.BuildSystemPromptForTest(assigned, ws, threadcase.ModeMention, "")).
		Contains("- Assignees (Slack user IDs): U-ONE, U-TWO")

	empty := newThreadCase()
	empty.AssigneeIDs = nil
	gt.String(t, threadcase.BuildSystemPromptForTest(empty, ws, threadcase.ModeMention, "")).
		Contains("- Assignees (Slack user IDs): (empty)")
}

func TestBuildSystemPrompt_CreateMode_WorkspacePrompt(t *testing.T) {
	ws := createTestWorkspace()
	ws.CaseCreatePrompt = "Always fill the severity field for security cases."

	// ModeCreate (no case yet) renders the schema and appends the workspace
	// instructions.
	prompt := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, "")
	gt.String(t, prompt).Contains("NO case exists yet")
	gt.String(t, prompt).Contains("severity")
	gt.String(t, prompt).Contains("Workspace-specific instructions")
	gt.String(t, prompt).Contains("Always fill the severity field")

	// Empty CaseCreatePrompt → no workspace-specific section.
	ws.CaseCreatePrompt = ""
	bare := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, "")
	gt.Bool(t, strings.Contains(bare, "Workspace-specific instructions")).False()
}

func TestBuildSystemPrompt_CreateInstruction(t *testing.T) {
	ws := createTestWorkspace()
	instruction := "Read the messages before and after the anchored message."

	// ModeCreate with an instruction renders the trigger-context section.
	prompt := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, instruction)
	gt.String(t, prompt).Contains("# Trigger context")
	gt.String(t, prompt).Contains(instruction)

	// Empty instruction → no trigger-context section.
	bare := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, "")
	gt.Bool(t, strings.Contains(bare, "# Trigger context")).False()

	// The instruction is create-only: a mention turn must not carry it.
	c := newThreadCase()
	mention := threadcase.BuildSystemPromptForTest(c, ws, threadcase.ModeMention, instruction)
	gt.Bool(t, strings.Contains(mention, "# Trigger context")).False()
	gt.Bool(t, strings.Contains(mention, instruction)).False()
}

// A reply, and every question this host asks, are posted to the Slack thread, so
// the shared Slack formatting rules have to reach the run in every mode. They sit
// before the operator-supplied sections, which stay the last word of the prompt.
func TestBuildSystemPrompt_SlackFormatInEveryMode(t *testing.T) {
	ws := newThreadWorkspace()
	ws.CaseCreatePrompt = "Ask for the affected region."

	for _, tc := range []struct {
		name string
		c    *model.Case
		mode threadcase.Mode
	}{
		{"mention", newThreadCase(), threadcase.ModeMention},
		{"materialize", newThreadCase(), threadcase.ModeMaterialize},
		{"create", nil, threadcase.ModeCreate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := threadcase.BuildSystemPromptForTest(tc.c, ws, tc.mode, "")
			gt.String(t, prompt).Contains(slackfmt.Section())
		})
	}

	// The workspace-supplied create instructions stay after the shared section.
	created := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, "")
	gt.Bool(t, strings.Index(created, slackfmt.Section()) < strings.Index(created, ws.CaseCreatePrompt)).True()
}

// The ModeCreate field-schema block must give the planner the hints it needs to
// fill fields correctly on the first attempt: which fields are required, and the
// exact RFC3339 format for date fields (whose bare-date value was the reported
// failure). The instruction text now promises feedback-and-retry rather than the
// old "NO retry" wording.
func TestBuildSystemPrompt_CreateMode_FieldSchemaHints(t *testing.T) {
	ws := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "support", Name: "Support"},
		CaseMode:  model.CaseModeThread,
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect, Required: true, Options: []config.FieldOption{{ID: "high", Name: "High"}}},
				{ID: "due_date", Name: "Due date", Type: types.FieldTypeDate},
			},
		},
	}
	prompt := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, "")
	// Required fields are marked so the planner knows which it must fill.
	gt.String(t, prompt).Contains("(required)")
	// Date fields spell out the exact RFC3339 format the validator enforces.
	gt.String(t, prompt).Contains("format=RFC3339")
	gt.String(t, prompt).Contains("2026-07-14T00:00:00Z")
	// The instruction promises feedback-and-retry, not the removed "NO retry".
	gt.String(t, prompt).Contains("fed back to you")
	gt.Bool(t, strings.Contains(prompt, "NO retry")).False()
}

func TestBuildUserInput_TemplateRendersEverySection(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	sys := []threadcase.ConversationMessage{
		{Timestamp: "1.1", UserID: "U1", UserName: "alice", Text: "hi"},
		{Timestamp: "1.2", UserID: "U2", Text: "the mention itself"},
	}
	delta := []threadcase.ConversationMessage{{Timestamp: "1.3", UserName: "bob", Text: "yo"}}
	mention := threadcase.ConversationMessage{Timestamp: "1.2", UserID: "U2", UserName: "carol", Text: "please assign me"}

	got, err := threadcase.BuildUserInputForTest(now, sys, delta, mention)
	gt.NoError(t, err).Required()
	// The mention is skipped from the thread listing and rendered last, and the
	// standing instruction is not added when there is something to send.
	gt.Bool(t, strings.HasSuffix(got, "# Thread so far\n[1.1] alice (U1): hi\n\n# New messages since last mention\n[1.3] bob: yo\n\n# Current mention\nFrom: carol (U2)\nplease assign me")).True()
	gt.Bool(t, strings.Contains(got, "the mention itself")).False()
	gt.Bool(t, strings.Contains(got, "Investigate this case")).False()
}

func TestBuildUserInput_FallsBackWhenEmpty(t *testing.T) {
	got, err := threadcase.BuildUserInputForTest(time.Time{}, nil, nil, threadcase.ConversationMessage{})
	gt.NoError(t, err).Required()
	gt.Value(t, got).Equal("Investigate this case and decide the next action.")
}

// The turn's absolute instant. Without it the planner resolves "today" against
// whatever date its training suggests — the create turn that produced this test
// was told "by today" and wrote a due date a month in the past.
//
// It goes in the USER message, not the system prompt, because a thread-mode turn
// continues the previous turn's conversation and the system prompt is not part of
// that history: a resumed turn would otherwise carry the earlier messages with
// nothing saying when they were written.
func TestBuildUserInput_StatesTheCurrentTime(t *testing.T) {
	now := time.Date(2026, 9, 7, 3, 35, 19, 0, time.UTC)

	t.Run("LeadsTheMessageWithTheInstant", func(t *testing.T) {
		got, err := threadcase.BuildUserInputForTest(now, nil, nil,
			threadcase.ConversationMessage{Text: "<@bot> due today"})
		gt.NoError(t, err).Required()
		gt.String(t, got).Contains("2026-09-07T03:35:19Z")
		gt.String(t, got).Contains("(UTC)")
		// Before the conversation it dates.
		gt.Bool(t, strings.Index(got, "# Current time") < strings.Index(got, "# Current mention")).True()
	})

	// A resumed turn inherits the previous turn's messages, each carrying its own
	// current time. The section must say which one is now.
	t.Run("SaysTheLatestSectionIsNow", func(t *testing.T) {
		got, err := threadcase.BuildUserInputForTest(now, nil, nil,
			threadcase.ConversationMessage{Text: "<@bot> due today"})
		gt.NoError(t, err).Required()
		gt.String(t, got).Contains("the latest one is now")
	})

	// A non-UTC instant is still rendered in UTC, so the stated zone and the value
	// can never disagree.
	t.Run("RendersInUTCWhateverZoneItIsGiven", func(t *testing.T) {
		jst := time.FixedZone("JST", 9*60*60)
		got, err := threadcase.BuildUserInputForTest(
			time.Date(2026, 9, 7, 12, 35, 19, 0, jst), nil, nil,
			threadcase.ConversationMessage{Text: "<@bot> due today"})
		gt.NoError(t, err).Required()
		gt.String(t, got).Contains("2026-09-07T03:35:19Z")
	})

	// The fallback instruction is what a materialize turn with no messages is
	// given; the time section must not stand in for it.
	t.Run("KeepsTheFallbackInstructionWhenThereIsNoConversation", func(t *testing.T) {
		got, err := threadcase.BuildUserInputForTest(now, nil, nil, threadcase.ConversationMessage{})
		gt.NoError(t, err).Required()
		gt.String(t, got).Contains("2026-09-07T03:35:19Z")
		gt.String(t, got).Contains("Investigate this case and decide the next action.")
	})

	t.Run("ZeroTimeOmitsTheSection", func(t *testing.T) {
		got, err := threadcase.BuildUserInputForTest(time.Time{}, nil, nil,
			threadcase.ConversationMessage{Text: "<@bot> due today"})
		gt.NoError(t, err).Required()
		gt.Bool(t, strings.Contains(got, "# Current time")).False()
		gt.Bool(t, strings.Contains(got, "0001-01-01")).False()
	})
}

// The mention-mode system prompt tells the agent to resolve a named person to a
// Slack user ID before calling case__assign, and no tool can look one up. Every
// conversation line must therefore carry the author's ID alongside the display
// name, or that instruction is unsatisfiable.
func TestBuildUserInput_RendersSpeakerIDs(t *testing.T) {
	msgs := []threadcase.ConversationMessage{
		{Timestamp: "1700000000.000100", UserID: "U-ALICE", UserName: "Alice", Text: "the DB is down"},
		{Timestamp: "1700000000.000200", UserID: "U-BOB", Text: "looking into it"},
		{Timestamp: "1700000000.000300", UserName: "Webhook", Text: "alert fired"},
	}

	got, err := threadcase.BuildUserInputForTest(time.Time{}, msgs, nil, threadcase.ConversationMessage{})
	gt.NoError(t, err).Required()
	gt.String(t, got).Contains("[1700000000.000100] Alice (U-ALICE): the DB is down")
	// Name unknown: the ID alone still reaches the model.
	gt.String(t, got).Contains("[1700000000.000200] U-BOB: looking into it")
	// ID unknown (e.g. a bot post): degrade to the name rather than printing "()".
	gt.String(t, got).Contains("[1700000000.000300] Webhook: alert fired")
}

// "assign me" is only actionable when the mention's own author is identified,
// so the current mention block names its speaker.
func TestBuildUserInput_RendersMentionAuthor(t *testing.T) {
	mention := threadcase.ConversationMessage{
		Timestamp: "1700000009.000001",
		UserID:    "U-CALLER",
		UserName:  "Caller",
		Text:      "<@bot> assign me",
	}

	got, err := threadcase.BuildUserInputForTest(time.Time{}, nil, nil, mention)
	gt.NoError(t, err).Required()
	gt.String(t, got).Contains("# Current mention")
	gt.String(t, got).Contains("From: Caller (U-CALLER)")
	gt.String(t, got).Contains("<@bot> assign me")

	// An unattributed mention still renders its text, with no dangling "From:".
	anon, err := threadcase.BuildUserInputForTest(time.Time{}, nil, nil,
		threadcase.ConversationMessage{Text: "<@bot> hello"})
	gt.NoError(t, err).Required()
	gt.String(t, anon).Contains("<@bot> hello")
	gt.Bool(t, strings.Contains(anon, "From:")).False()
}

// The mention itself is already rendered under "# Current mention", so it must
// not also appear in the thread transcript above it.
func TestBuildUserInput_SkipsTheMentionInTheTranscript(t *testing.T) {
	mention := threadcase.ConversationMessage{
		Timestamp: "1700000009.000001",
		UserID:    "U-CALLER",
		Text:      "<@bot> assign me",
	}
	msgs := []threadcase.ConversationMessage{
		{Timestamp: "1700000000.000100", UserID: "U-ALICE", Text: "earlier note"},
		mention,
	}

	got, err := threadcase.BuildUserInputForTest(time.Time{}, msgs, nil, mention)
	gt.NoError(t, err).Required()
	gt.String(t, got).Contains("earlier note")
	gt.Number(t, strings.Count(got, "<@bot> assign me")).Equal(1)
}

// A mention turn on an existing case carries [workspace] description and
// [case.prompts].mention; each phase gets only its own operator prompt, so a
// create turn never sees the mention prompt and vice versa, and a materialize
// turn sees neither.
func TestBuildSystemPrompt_MentionMode_WorkspaceInstructions(t *testing.T) {
	ws := newThreadWorkspace()
	ws.Workspace.Description = "  Every case tracks one vendor review.\n"
	ws.CaseCreatePrompt = "Operator create prompt"
	ws.CaseMentionPrompt = "Operator mention prompt"
	c := newThreadCase()

	mention := threadcase.BuildSystemPromptForTest(c, ws, threadcase.ModeMention, "")
	gt.String(t, mention).Contains("this is a thread-mode case. Sub-agents may read")
	gt.String(t, mention).Contains("\n\n# Workspace\n" +
		"This case belongs to a workspace that describes itself as follows. Follow the conventions it sets.\n\n" +
		"Every case tracks one vendor review.\n\n" +
		"# Current case\n")
	gt.Bool(t, strings.HasSuffix(mention, slackfmt.Section()+"\n\n# Workspace-specific instructions\nOperator mention prompt\n")).True()
	gt.Bool(t, strings.Contains(mention, "Operator create prompt")).False()

	created := threadcase.BuildSystemPromptForTest(nil, ws, threadcase.ModeCreate, "")
	gt.String(t, created).Contains("# Workspace-specific instructions\nOperator create prompt\n")
	gt.Bool(t, strings.Contains(created, "Operator mention prompt")).False()
	gt.Bool(t, strings.Contains(created, "# Workspace\n")).False()

	materialized := threadcase.BuildSystemPromptForTest(c, ws, threadcase.ModeMaterialize, "")
	gt.Bool(t, strings.Contains(materialized, "# Workspace-specific instructions")).False()
	gt.Bool(t, strings.Contains(materialized, "# Workspace\n")).False()
}

// Blank values render no heading at all.
func TestBuildSystemPrompt_MentionMode_BlankWorkspaceInstructions(t *testing.T) {
	ws := newThreadWorkspace()
	ws.Workspace.Description = " \n"
	ws.CaseMentionPrompt = "\t\n"

	prompt := threadcase.BuildSystemPromptForTest(newThreadCase(), ws, threadcase.ModeMention, "")
	gt.Bool(t, strings.Contains(prompt, "# Workspace\n")).False()
	gt.Bool(t, strings.Contains(prompt, "# Workspace-specific instructions")).False()
}
