package job_test

import (
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/robfig/cron/v3"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/slackfmt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
)

func newWorkspace(id, name string) *model.WorkspaceEntry {
	return &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: id, Name: name, Description: "test"},
	}
}

func newCase(id int64) *model.Case {
	return &model.Case{
		ID:             id,
		Title:          "Sample",
		Description:    "desc",
		Status:         types.CaseStatusOpen,
		ReporterID:     "U-REPORTER",
		AssigneeIDs:    []string{"U-A1", "U-A2"},
		SlackChannelID: "C-CASE",
		CreatedAt:      time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC),
	}
}

func TestBuildSystemPrompt_CaseCreatedEvent(t *testing.T) {
	j := &model.Job{
		ID:     "summarize",
		Prompt: "{{.Case.Title}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	ev := job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        42,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-CALLER",
		CaseLifecycle: model.CaseLifecycleCreated,
	}
	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(42), Event: ev,
	})
	gt.NoError(t, err).Required()

	mustContain(t, got, "# Role")
	mustContain(t, got, "# Workspace")
	mustContain(t, got, "- id: ws")
	mustContain(t, got, "# Case")
	mustContain(t, got, "- title: Sample")
	mustContain(t, got, "# Actions (existing, non-archived)")
	mustContain(t, got, "(none)")
	mustContain(t, got, "# Trigger condition")
	mustContain(t, got, "a new case is created")
	mustContain(t, got, "# Trigger reason (this invocation)")
	mustContain(t, got, "Case #42 was created by U-CALLER at 2026-05-23T12:00:00Z.")
	mustContain(t, got, "# Guardrails")
	mustContain(t, got, "Do not duplicate work")
	mustContain(t, got, "cannot close the case")
}

func TestBuildSystemPrompt_ThreadModeOmitsActions(t *testing.T) {
	j := &model.Job{
		ID:     "summarize",
		Prompt: "{{.Case.Title}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	ev := job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        42,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-CALLER",
		CaseLifecycle: model.CaseLifecycleCreated,
	}
	ws := newWorkspace("ws", "WS")
	ws.CaseMode = model.CaseModeThread

	// Even if actions were somehow passed in, a thread-mode prompt must not
	// surface them: the agent has no action tools there.
	actions := []*model.Action{{ID: 7, Title: "stray", Status: types.ActionStatusTodo}}

	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: ws, Case: newCase(42), Actions: actions, Event: ev,
	})
	gt.NoError(t, err).Required()

	// Action section and action-specific guardrails are gone.
	mustNotContain(t, got, "# Actions (existing, non-archived)")
	mustNotContain(t, got, "stray")
	mustNotContain(t, got, "archive actions")
	mustNotContain(t, got, "action list")
	mustContain(t, got, "you do not manage Actions")

	// Non-action sections still render.
	mustContain(t, got, "# Case")
	mustContain(t, got, "# Guardrails")
	mustContain(t, got, "Do not duplicate work")
	mustContain(t, got, "cannot close the case")
}

// A thread-mode workspace that enables [case] actions gives its Jobs the action
// read tools only. The prompt lists the case's Actions and says they may be read
// but not changed, keeps the thread-mode sections, and carries none of the
// channel-mode guidance that presumes action writes.
func TestBuildSystemPrompt_ThreadModeWithActionsListsActionsReadOnly(t *testing.T) {
	ws := newWorkspace("ws", "WS")
	ws.CaseMode = model.CaseModeThread
	ws.CaseActions = true

	archivedAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	actions := []*model.Action{
		{ID: 7, Title: "Sample action", Status: types.ActionStatusTodo, AssigneeID: "U-ALICE"},
		{ID: 8, Title: "Archived action", Status: types.ActionStatusTodo, ArchivedAt: &archivedAt},
	}

	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job:            threadModeJob(),
		Workspace:      ws,
		Case:           newCase(42),
		Actions:        actions,
		RecentMessages: nil,
		Event:          threadModeEvent(),
	})
	gt.NoError(t, err).Required()

	mustContain(t, got, "# Actions (existing, non-archived)")
	mustContain(t, got, "You may read these Actions (`core__list_actions` / `core__get_action`) to\ninform your work, but you cannot create, update, or archive them.")
	mustContain(t, got, "- #7 Sample action [TODO] assignee=U-ALICE")
	mustNotContain(t, got, "Archived action")
	mustContain(t, got, "- You cannot delete cases. This is a thread-mode workspace: you may read its Actions, but you cannot create or change them.")
	mustContain(t, got, "Determine idempotency from the current case state, action list, and Slack history.")
	mustNotContain(t, got, "you do not manage Actions")
	// The write-side guidance stays channel-mode only.
	mustNotContain(t, got, "if an equivalent action or Slack message already exists")
	mustNotContain(t, got, "archive actions, or delete action steps")
	// Thread-mode sections still render.
	mustContain(t, got, "# Recent thread messages (last 24h, up to 32)")
}

// A Job reaches Slack only through slack__post_to_case_channel, and it composes
// that text itself, so the shared Slack formatting rules belong in its system
// prompt regardless of whether the workspace manages Actions.
func TestBuildSystemPrompt_SlackFormat(t *testing.T) {
	j := &model.Job{
		ID:     "summarize",
		Prompt: "{{.Case.Title}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	ev := job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        42,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-CALLER",
		CaseLifecycle: model.CaseLifecycleCreated,
	}

	channel := newWorkspace("ws", "WS")
	thread := newWorkspace("ws", "WS")
	thread.CaseMode = model.CaseModeThread

	for name, ws := range map[string]*model.WorkspaceEntry{"channel_mode": channel, "thread_mode": thread} {
		t.Run(name, func(t *testing.T) {
			got, err := job.BuildSystemPrompt(job.PromptInputs{
				Job: j, Workspace: ws, Case: newCase(42), Event: ev,
			})
			gt.NoError(t, err).Required()
			mustContain(t, got, slackfmt.Section())
			mustNotContain(t, got, "{{")
		})
	}
}

// A workspace that fixes its language tells the Job agent so, on both the simple
// strategy (which has no other language directive) and planexec; a workspace with
// none adds no instruction.
func TestBuildSystemPrompt_WorkspaceLanguage(t *testing.T) {
	j := &model.Job{
		ID:     "summarize",
		Prompt: "x",
		Events: model.JobEvents{Scheduled: &model.ScheduledEventConfig{Every: time.Hour}},
	}
	ev := job.Event{
		Domain:      model.JobEventDomainScheduled,
		WorkspaceID: "ws",
		CaseID:      42,
		Timestamp:   time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}
	const directive = "in **Japanese**. This workspace fixes its language."

	ja := newWorkspace("ws", "WS")
	ja.Language = i18n.LangJA
	got, err := job.BuildSystemPrompt(job.PromptInputs{Job: j, Workspace: ja, Case: newCase(42), Event: ev})
	gt.NoError(t, err).Required()
	mustContain(t, got, directive)

	got, err = job.BuildSystemPrompt(job.PromptInputs{Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(42), Event: ev})
	gt.NoError(t, err).Required()
	mustNotContain(t, got, "This workspace fixes its language.")
}

func TestBuildSystemPrompt_SlackThreadTS(t *testing.T) {
	j := &model.Job{
		ID:     "daily-progress",
		Prompt: "x",
		Events: model.JobEvents{
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		},
	}
	ev := job.Event{
		Domain:      model.JobEventDomainScheduled,
		WorkspaceID: "ws",
		CaseID:      42,
		Timestamp:   time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}

	t.Run("thread-mode case exposes slack_thread_ts so the agent can read its thread", func(t *testing.T) {
		ws := newWorkspace("ws", "WS")
		ws.CaseMode = model.CaseModeThread
		c := newCase(42)
		c.SlackThreadTS = "1700000000.000100"
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: j, Workspace: ws, Case: c, Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- slack_channel_id: C-CASE")
		mustContain(t, got, "- slack_thread_ts: 1700000000.000100")
	})

	t.Run("channel-mode case (no thread) omits the slack_thread_ts line", func(t *testing.T) {
		c := newCase(42) // newCase leaves SlackThreadTS empty
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: j, Workspace: newWorkspace("ws", "WS"), Case: c, Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- slack_channel_id: C-CASE")
		mustNotContain(t, got, "slack_thread_ts:")
	})
}

func TestBuildSystemPrompt_CaseClosedEvent(t *testing.T) {
	j := &model.Job{
		ID:     "on-close",
		Prompt: "x",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleClosed}},
		},
	}
	ev := job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        99,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-OPS",
		CaseLifecycle: model.CaseLifecycleClosed,
	}
	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(99), Event: ev,
	})
	gt.NoError(t, err).Required()
	mustContain(t, got, "Case #99 status was transitioned to CLOSED by U-OPS at 2026-05-23T12:00:00Z.")
	mustContain(t, got, "the case status transitions to CLOSED")
}

func TestBuildSystemPrompt_ScheduledEvery(t *testing.T) {
	j := &model.Job{
		ID:     "stale",
		Prompt: "x",
		Events: model.JobEvents{
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		},
	}
	ev := job.Event{
		Domain:      model.JobEventDomainScheduled,
		WorkspaceID: "ws",
		CaseID:      1,
		Timestamp:   time.Date(2026, 5, 23, 10, 5, 0, 0, time.UTC),
		LastRunAt:   time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC),
	}
	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(1), Event: ev,
	})
	gt.NoError(t, err).Required()
	mustContain(t, got, "the time since the last run reaches 1h0m0s")
	mustContain(t, got, "every=1h0m0s")
	mustContain(t, got, "last_run_at=2026-05-23T09:00:00Z")
	mustContain(t, got, "now=2026-05-23T10:05:00Z")
}

func TestBuildSystemPrompt_ScheduledCron(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse("0 9 * * *")
	gt.NoError(t, err).Required()

	j := &model.Job{
		ID:     "daily",
		Prompt: "x",
		Events: model.JobEvents{
			Scheduled: &model.ScheduledEventConfig{Cron: sched, CronExpr: "0 9 * * *"},
		},
	}
	ev := job.Event{
		Domain:       model.JobEventDomainScheduled,
		WorkspaceID:  "ws",
		CaseID:       1,
		Timestamp:    time.Date(2026, 5, 23, 9, 5, 0, 0, time.UTC),
		LastRunAt:    time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
		ScheduledFor: time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC),
	}
	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(1), Event: ev,
	})
	gt.NoError(t, err).Required()
	mustContain(t, got, "a cron tick of `0 9 * * *` arrives (UTC)")
	mustContain(t, got, "scheduled_for=2026-05-23T09:00:00Z")
}

func TestBuildSystemPrompt_ManualEvent(t *testing.T) {
	j := &model.Job{
		ID:     "daily",
		Prompt: "x",
		Events: model.JobEvents{
			Scheduled: &model.ScheduledEventConfig{Every: time.Hour},
		},
	}
	base := job.Event{
		Domain:      model.JobEventDomainManual,
		WorkspaceID: "ws",
		CaseID:      7,
		Timestamp:   time.Date(2026, 5, 23, 11, 30, 0, 0, time.UTC),
	}

	t.Run("names the operator who pressed Run", func(t *testing.T) {
		ev := base
		ev.ActorUserID = "U-OPERATOR"
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(7), Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "Manually triggered by U-OPERATOR for case #7 at 2026-05-23T11:30:00Z.")
		mustContain(t, got, "This run was requested on demand, not by one of the trigger conditions above.")
		// The Job's own scheduled trigger is still described in the
		// "Trigger condition" section; only the reason differs.
		mustContain(t, got, "the time since the last run reaches 1h0m0s")
	})

	t.Run("falls back to (unknown) without an actor", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: j, Workspace: newWorkspace("ws", "WS"), Case: newCase(7), Event: base,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "Manually triggered by (unknown) for case #7 at 2026-05-23T11:30:00Z.")
	})
}

func TestRenderUserPrompt_TemplateExpansion(t *testing.T) {
	j := &model.Job{
		ID:     "demo",
		Prompt: "case {{.Case.Title}} created by {{.Event.ActorUserID}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	out, err := job.RenderUserPrompt(job.PromptInputs{
		Job: j, Case: newCase(7),
		Event: job.Event{ActorUserID: "U-X", CaseLifecycle: model.CaseLifecycleCreated},
	})
	gt.NoError(t, err).Required()
	gt.String(t, out).Equal("case Sample created by U-X")
}

// Job IDs are only unique within a workspace (config.resolveJobs rejects
// duplicates per workspace, not globally), so the same id — e.g. a standard
// `assign_on_create` placed in every workspace — legitimately names different
// prompts. A template cache keyed on the job id alone served whichever
// workspace's prompt happened to be parsed first, to every workspace.
func TestRenderUserPrompt_SameJobIDAcrossWorkspaces(t *testing.T) {
	const sharedID = "assign_on_create"

	first := &model.Job{
		ID:     sharedID,
		Prompt: "workspace-a: assign {{.Case.Title}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	second := &model.Job{
		ID:     sharedID,
		Prompt: "workspace-b: review {{.Case.Title}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}

	gotFirst, err := job.RenderUserPrompt(job.PromptInputs{
		Job: first, Workspace: newWorkspace("workspace-a", "A"), Case: newCase(1),
	})
	gt.NoError(t, err).Required()
	gt.String(t, gotFirst).Equal("workspace-a: assign Sample")

	gotSecond, err := job.RenderUserPrompt(job.PromptInputs{
		Job: second, Workspace: newWorkspace("workspace-b", "B"), Case: newCase(2),
	})
	gt.NoError(t, err).Required()
	gt.String(t, gotSecond).Equal("workspace-b: review Sample")

	// Re-render the first one to prove the second render did not evict or
	// overwrite it either — both must stay independently addressable.
	gotFirstAgain, err := job.RenderUserPrompt(job.PromptInputs{
		Job: first, Workspace: newWorkspace("workspace-a", "A"), Case: newCase(1),
	})
	gt.NoError(t, err).Required()
	gt.String(t, gotFirstAgain).Equal("workspace-a: assign Sample")
}

func TestRenderUserPrompt_TemplateError(t *testing.T) {
	j := &model.Job{
		ID:     "bad",
		Prompt: "{{.Missing.Field",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	_, err := job.RenderUserPrompt(job.PromptInputs{Job: j, Case: newCase(1)})
	gt.Error(t, err)
}

// threadModeJob and threadModeEvent are small fixtures shared by the recent-
// message tests below: a case-created Job on a thread-mode workspace.
func threadModeJob() *model.Job {
	return &model.Job{
		ID:     "summarize",
		Prompt: "{{.Case.Title}}",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
}

func threadModeEvent() job.Event {
	return job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        42,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-CALLER",
		CaseLifecycle: model.CaseLifecycleCreated,
	}
}

func recentMsg(text, userID, userName string, createdAt time.Time) *slack.Message {
	return slack.NewMessageFromData(createdAt.Format("20060102.150405"), "C-CASE", "1700000000.0001", "T1", userID, userName, text, "", createdAt, nil)
}

func TestBuildSystemPrompt_ThreadModeRecentMessages(t *testing.T) {
	ws := newWorkspace("ws", "WS")
	ws.CaseMode = model.CaseModeThread

	short := recentMsg("hello there", "U-1", "Alice", time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC))
	// A body of 200 ASCII runes must be truncated to 140 with the original
	// rune count annotated. No display name → author falls back to user ID.
	long := recentMsg(strings.Repeat("x", 200), "U-2", "", time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC))

	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job:            threadModeJob(),
		Workspace:      ws,
		Case:           newCase(42),
		RecentMessages: []*slack.Message{short, long},
		Event:          threadModeEvent(),
	})
	gt.NoError(t, err).Required()

	mustContain(t, got, "# Recent thread messages (last 24h, up to 32)")
	// Oldest-first ordering and author rendering (name, then user-id fallback).
	mustContain(t, got, "[2026-05-23T09:00:00Z] Alice: hello there")
	mustContain(t, got, "[2026-05-23T10:00:00Z] U-2: "+strings.Repeat("x", 140))
	// The long body is truncated and annotated; the short body is not.
	mustContain(t, got, "[200 chars total]")
	mustNotContain(t, got, strings.Repeat("x", 141))
	mustNotContain(t, got, "(none)")

	// Oldest-first: the 09:00 line must precede the 10:00 line.
	gt.Number(t, strings.Index(got, "Alice: hello there")).LessOrEqual(strings.Index(got, "U-2: "))
}

func TestBuildSystemPrompt_ThreadModeRecentMessagesEmpty(t *testing.T) {
	ws := newWorkspace("ws", "WS")
	ws.CaseMode = model.CaseModeThread

	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job:       threadModeJob(),
		Workspace: ws,
		Case:      newCase(42),
		Event:     threadModeEvent(),
	})
	gt.NoError(t, err).Required()

	// Thread-mode always renders the section header; an empty window shows
	// "(none)" so the agent is explicitly told there is no recent traffic.
	mustContain(t, got, "# Recent thread messages (last 24h, up to 32)")
	mustContain(t, got, "(none)")
}

func TestBuildSystemPrompt_ChannelModeOmitsRecentMessages(t *testing.T) {
	// Channel-mode workspace: even if messages are passed in, the section
	// must be absent (the agent there has no thread to reason about).
	msg := recentMsg("should not appear", "U-1", "Alice", time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC))

	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job:            threadModeJob(),
		Workspace:      newWorkspace("ws", "WS"), // default channel mode
		Case:           newCase(42),
		RecentMessages: []*slack.Message{msg},
		Event:          threadModeEvent(),
	})
	gt.NoError(t, err).Required()

	mustNotContain(t, got, "# Recent thread messages")
	mustNotContain(t, got, "should not appear")
}

func TestTruncateRunes(t *testing.T) {
	// Under the cap: returned verbatim, no annotation.
	out, full := job.TruncateRunesForTest("hello", 140)
	gt.String(t, out).Equal("hello")
	gt.Number(t, full).Equal(0)

	// Exactly at the cap: still no annotation.
	exact := strings.Repeat("a", 140)
	out, full = job.TruncateRunesForTest(exact, 140)
	gt.String(t, out).Equal(exact)
	gt.Number(t, full).Equal(0)

	// One over the cap: truncated to 140 runes, full count reported.
	over := strings.Repeat("a", 141)
	out, full = job.TruncateRunesForTest(over, 140)
	gt.String(t, out).Equal(strings.Repeat("a", 140))
	gt.Number(t, full).Equal(141)

	// Multibyte: counts runes (文字), not bytes, and never splits a rune.
	// 5 CJK runes (15 bytes) capped at 3 → 3 runes, full count 5.
	out, full = job.TruncateRunesForTest("一二三四五", 3)
	gt.String(t, out).Equal("一二三")
	gt.Number(t, full).Equal(5)

	// Non-positive cap yields empty, no annotation.
	out, full = job.TruncateRunesForTest("anything", 0)
	gt.String(t, out).Equal("")
	gt.Number(t, full).Equal(0)
}

func TestBuildSystemPrompt_FieldSemantic(t *testing.T) {
	ws := newWorkspace("ws", "WS")
	ws.FieldSchema = &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "notify_channel", Name: "Notify", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		{ID: "note", Name: "Note", Type: types.FieldTypeText},
	}}
	ws.MemoConfig = &config.MemoConfig{
		Description: "memo",
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "memo_channel", Name: "Memo channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		}},
	}
	j := &model.Job{ID: "j", Prompt: "do it", Events: model.JobEvents{
		Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
	}}
	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: ws, Case: newCase(1),
		Event: job.Event{Domain: model.JobEventDomainCase, WorkspaceID: "ws", CaseID: 1, CaseLifecycle: model.CaseLifecycleCreated},
	})
	gt.NoError(t, err).Required()

	label := semantic.Label(types.SemanticSlackChannelID)
	mustContain(t, got, "  - notify_channel (text): Notify\n    semantic: "+label)
	mustContain(t, got, "  - memo_channel (text): Memo channel\n    semantic: "+label)
	// A text field without a semantic renders no semantic line.
	mustNotContain(t, got, "  - note (text): Note\n    semantic:")
}

func TestBuildSystemPrompt_FieldPattern(t *testing.T) {
	ticket := types.TextPattern("[A-Z]{2,5}-[0-9]+")
	cve := types.TextPattern("CVE-[0-9]{4}-[0-9]{4,}")
	ws := newWorkspace("ws", "WS")
	ws.FieldSchema = &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "ticket_id", Name: "Ticket ID", Type: types.FieldTypeText, Validation: config.FieldValidation{Pattern: ticket}},
		{ID: "note", Name: "Note", Type: types.FieldTypeText},
	}}
	ws.MemoConfig = &config.MemoConfig{
		Description: "memo",
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "cve", Name: "CVE", Type: types.FieldTypeText, Validation: config.FieldValidation{Pattern: cve}},
		}},
	}
	j := &model.Job{ID: "j", Prompt: "do it", Events: model.JobEvents{
		Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
	}}
	got, err := job.BuildSystemPrompt(job.PromptInputs{
		Job: j, Workspace: ws, Case: newCase(1),
		Event: job.Event{Domain: model.JobEventDomainCase, WorkspaceID: "ws", CaseID: 1, CaseLifecycle: model.CaseLifecycleCreated},
	})
	gt.NoError(t, err).Required()

	mustContain(t, got, "  - ticket_id (text): Ticket ID\n    pattern: "+ticket.Label())
	mustContain(t, got, "  - cve (text): CVE\n    pattern: "+cve.Label())
	// A text field without a pattern renders no pattern line.
	mustNotContain(t, got, "  - note (text): Note\n    pattern:")
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("expected prompt to contain %q\n---\n%s\n---", sub, s)
	}
}

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("expected prompt NOT to contain %q\n---\n%s\n---", sub, s)
	}
}

func TestBuildSystemPrompt_CaseAgentAdditionalPrompt(t *testing.T) {
	j := &model.Job{
		ID:     "incident-rca",
		Prompt: "x",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	ev := job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        7,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-CALLER",
		CaseLifecycle: model.CaseLifecycleCreated,
	}

	t.Run("renders Per-case operator notes when prompt is set", func(t *testing.T) {
		c := newCase(7)
		c.AgentAdditionalPrompt = "### Custom guidance\n- always cite source IDs\n- escalate to #incident-7"
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: j, Workspace: newWorkspace("ws", "WS"), Case: c, Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Per-case operator notes")
		mustContain(t, got, "### Custom guidance")
		mustContain(t, got, "always cite source IDs")
		mustContain(t, got, "escalate to #incident-7")
		// Sanity: section comes before Actions so the operator notes
		// are read in the context of the Case body.
		idxNotes := strings.Index(got, "# Per-case operator notes")
		idxActions := strings.Index(got, "# Actions")
		gt.Number(t, idxNotes).LessOrEqual(idxActions - 1)
	})

	t.Run("section is omitted when prompt is empty", func(t *testing.T) {
		c := newCase(7)
		c.AgentAdditionalPrompt = ""
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: j, Workspace: newWorkspace("ws", "WS"), Case: c, Event: ev,
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "# Per-case operator notes")
	})
}

func TestBuildSystemPrompt_SourcesSection(t *testing.T) {
	j := &model.Job{
		ID:     "incident-rca",
		Prompt: "x",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	ev := job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        7,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		CaseLifecycle: model.CaseLifecycleCreated,
	}

	mkSource := func(id, name, desc string, t model.SourceType) *model.Source {
		return &model.Source{
			ID:          model.SourceID(id),
			Name:        name,
			SourceType:  t,
			Description: desc,
			Enabled:     true,
		}
	}

	t.Run("operator-narrowed phrasing when SourcesNarrowed is true", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       j,
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     ev,
			Sources: []*model.Source{
				mkSource("src-1", "AWS CloudTrail", "Audit log for AWS", model.SourceTypeSlack),
				mkSource("src-2", "Datadog Logs", "", model.SourceTypeGitHub),
			},
			SourcesNarrowed: true,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Sources")
		mustContain(t, got, "operator explicitly preferred")
		mustContain(t, got, "NOT a hard")
		mustContain(t, got, "src-1")
		mustContain(t, got, "AWS CloudTrail")
		mustContain(t, got, "Audit log for AWS")
		mustContain(t, got, "src-2")
		mustContain(t, got, "Datadog Logs")
	})

	t.Run("full-catalogue phrasing when SourcesNarrowed is false", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       j,
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     ev,
			Sources: []*model.Source{
				mkSource("src-1", "AWS CloudTrail", "Audit log for AWS", model.SourceTypeSlack),
			},
			SourcesNarrowed: false,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Sources")
		mustContain(t, got, "No per-case Source selection")
		mustContain(t, got, "src-1")
		mustContain(t, got, "AWS CloudTrail")
		mustNotContain(t, got, "operator explicitly preferred")
	})

	t.Run("section is omitted when Sources slice is empty", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       j,
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     ev,
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "# Sources")
	})
}

// fieldSchemaFixture is the workspace FieldSchema used by the
// custom-fields / field_values tests below. It exercises required,
// description, select / multi-select options, option descriptions, and
// option metadata so a single fixture covers every template branch.
func fieldSchemaFixture() *config.FieldSchema {
	return &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{
				ID:          "severity",
				Name:        "Severity",
				Type:        types.FieldTypeSelect,
				Required:    true,
				Description: "How severe the case is.",
				Options: []config.FieldOption{
					{ID: "low", Name: "Low", Description: "Minor impact", Metadata: map[string]any{"score": 1}},
					{ID: "high", Name: "High", Description: "Severe impact", Metadata: map[string]any{"score": 4}},
				},
			},
			{
				ID:   "affected_systems",
				Name: "Affected systems",
				Type: types.FieldTypeMultiSelect,
				Options: []config.FieldOption{
					{ID: "prod", Name: "Production"},
					{ID: "staging", Name: "Staging"},
				},
			},
			{
				ID:   "notes",
				Name: "Notes",
				Type: types.FieldTypeText,
			},
		},
	}
}

func newCustomFieldsWorkspace() *model.WorkspaceEntry {
	ws := newWorkspace("ws", "WS")
	ws.FieldSchema = fieldSchemaFixture()
	return ws
}

func caseCreatedEvent() job.Event {
	return job.Event{
		Domain:        model.JobEventDomainCase,
		WorkspaceID:   "ws",
		CaseID:        7,
		Timestamp:     time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ActorUserID:   "U-CALLER",
		CaseLifecycle: model.CaseLifecycleCreated,
	}
}

func caseCreatedJob() *model.Job {
	return &model.Job{
		ID:     "j",
		Prompt: "x",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
}

func TestBuildSystemPrompt_CustomFieldsSchema(t *testing.T) {
	t.Run("required marker and description are rendered", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- custom fields:")
		mustContain(t, got, "- severity (select): Severity [required]")
		mustContain(t, got, "description: How severe the case is.")
		// Non-required field has no [required] marker.
		mustContain(t, got, "- affected_systems (multi-select): Affected systems")
		mustNotContain(t, got, "Affected systems [required]")
		// Text field with no description, no options: just the header
		// line is emitted, no trailing metadata lines.
		mustContain(t, got, "- notes (text): Notes")
	})

	t.Run("select options carry name, description, and metadata", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "options:")
		mustContain(t, got, "- low — Low (Minor impact) [score=1]")
		mustContain(t, got, "- high — High (Severe impact) [score=4]")
		// Multi-select option with no description / metadata renders
		// only the ID and Name.
		mustContain(t, got, "- prod — Production")
		mustNotContain(t, got, "Production (")
		mustNotContain(t, got, "Production [")
	})

	t.Run("custom fields section is omitted when schema is nil", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "- custom fields:")
	})

	t.Run("custom fields section is omitted when schema has no fields", func(t *testing.T) {
		ws := newWorkspace("ws", "WS")
		ws.FieldSchema = &config.FieldSchema{}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: ws,
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "- custom fields:")
	})
}

func TestBuildSystemPrompt_BoardStatuses(t *testing.T) {
	t.Run("statuses render id, name, closed marker, and description", func(t *testing.T) {
		statusSet, err := model.NewActionStatusSet("triage", []string{"resolved"}, []model.ActionStatusDefinition{
			{ID: "triage", Name: "Triage", Description: "Awaiting first assessment"},
			{ID: "resolved", Name: "Resolved", Description: "Investigation is complete"},
		})
		gt.NoError(t, err).Required()
		ws := newWorkspace("ws", "WS")
		ws.CaseStatusSet = statusSet

		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: ws,
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Board Statuses")
		mustContain(t, got, "case__update_case_status")
		// The list is a vocabulary, not a current value. Saying so here is what
		// keeps it from being read as the case's present column.
		mustContain(t, got, "NOT the case's current status")
		mustContain(t, got, "`board_status` line in the `# Case` section")
		// Each status surfaces id, name, and the reasoning-hint description.
		mustContain(t, got, "- triage — Triage: Awaiting first assessment")
		mustContain(t, got, "- resolved — Resolved (closed): Investigation is complete")
		// The first status item must start on its own line, not glued onto the
		// trailing instruction sentence ({{- range}} trims the blank line before
		// it, but the range body's own leading newline still separates them).
		mustContain(t, got, "you would move it to.\n- triage")
		mustNotContain(t, got, "move it to.- triage")
	})

	t.Run("board statuses section is omitted when no status set is defined", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "# Board Statuses")
	})
}

// newThreadModeWorkspace returns a thread-mode workspace with a two-status case
// board, the configuration under which a Case carries a BoardStatus.
func newThreadModeWorkspace(t *testing.T) *model.WorkspaceEntry {
	t.Helper()
	statusSet, err := model.NewActionStatusSet("triage", []string{"resolved"}, []model.ActionStatusDefinition{
		{ID: "triage", Name: "Triage", Description: "Awaiting first assessment"},
		{ID: "resolved", Name: "Resolved", Description: "Investigation is complete"},
	})
	gt.NoError(t, err).Required()
	ws := newWorkspace("ws", "WS")
	ws.CaseMode = model.CaseModeThread
	ws.CaseStatusSet = statusSet
	return ws
}

func TestBuildSystemPrompt_CaseState(t *testing.T) {
	t.Run("a thread-bound case reports the board status it currently sits on", func(t *testing.T) {
		c := newCase(7)
		c.SlackThreadTS = "1748000000.000100"
		c.BoardStatus = "triage"
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newThreadModeWorkspace(t),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- board_status: triage")
		// The lifecycle status is a separate line and keeps its own value; the
		// two must not be conflated.
		mustContain(t, got, "- status: OPEN")
	})

	t.Run("a thread-bound case with no board status still renders the line", func(t *testing.T) {
		// A thread-bound case whose board status is empty is a real state —
		// validate --check-db reports it as an inconsistency — so the line must
		// say so rather than disappear, which would read as "not shown".
		c := newCase(7)
		c.SlackThreadTS = "1748000000.000100"
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newThreadModeWorkspace(t),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- board_status: (empty)")
	})

	t.Run("a channel-bound case omits the board status line entirely", func(t *testing.T) {
		// A channel-bound Case has no board, so there is no column to name and a
		// placeholder would invent one.
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "- board_status:")
		mustContain(t, got, "- status: OPEN")
	})

	t.Run("is_test and is_private are rendered even when false", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- is_test: false")
		mustContain(t, got, "- is_private: false")
	})

	t.Run("is_test and is_private report true when set", func(t *testing.T) {
		c := newCase(7)
		c.IsTest = true
		c.IsPrivate = true
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- is_test: true")
		mustContain(t, got, "- is_private: true")
	})

	t.Run("an archived case is distinguishable from a live one", func(t *testing.T) {
		live, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, live, "- archived_at: (empty)")

		c := newCase(7)
		archivedAt := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC)
		c.ArchivedAt = &archivedAt
		archived, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, archived, "- archived_at: 2026-06-01T08:30:00Z")
	})

	t.Run("channel members are joined onto one line", func(t *testing.T) {
		c := newCase(7)
		c.ChannelUserIDs = []string{"U-M1", "U-M2"}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- channel_user_ids: U-M1, U-M2")
	})

	t.Run("a case with no channel members still renders the line", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- channel_user_ids: (empty)")
	})

	t.Run("runtime-only and bookkeeping fields stay out of the prompt", func(t *testing.T) {
		c := newCase(7)
		c.AccessDenied = true
		c.RequestKey = "8e0b1f2c-request-key"
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "access_denied")
		mustNotContain(t, got, "8e0b1f2c-request-key")
	})
}

func TestBuildSystemPrompt_CaseFieldValueCoverage(t *testing.T) {
	t.Run("every schema field gets a line, in schema order, whether or not it holds a value", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- field_values:\n"+
			"  - severity: high (High)\n"+
			"  - affected_systems: (empty)\n"+
			"  - notes: (empty)\n")
	})

	t.Run("the section renders for a case with no values at all", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- field_values:\n"+
			"  - severity: (empty)\n"+
			"  - affected_systems: (empty)\n"+
			"  - notes: (empty)\n")
	})

	t.Run("a stored value whose field the schema no longer declares is still rendered", func(t *testing.T) {
		// Left over from a configuration change: dropping the line would hide
		// data the case still carries.
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"retired_owner": {FieldID: "retired_owner", Type: types.FieldTypeText, Value: "U-OWNER"},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		// Schema fields come first, in schema order; the orphan follows.
		mustContain(t, got, "- field_values:\n"+
			"  - severity: (empty)\n"+
			"  - affected_systems: (empty)\n"+
			"  - notes: (empty)\n"+
			"  - retired_owner: U-OWNER\n")
	})

	t.Run("a workspace with no field schema renders only the stored values", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"b_field": {FieldID: "b_field", Type: types.FieldTypeText, Value: "second"},
			"a_field": {FieldID: "a_field", Type: types.FieldTypeText, Value: "first"},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- field_values:\n"+
			"  - a_field: first\n"+
			"  - b_field: second\n")
	})

	t.Run("the section is omitted when there is neither a schema nor a stored value", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "- field_values:")
	})
}

func TestBuildSystemPrompt_CaseFieldValuesResolution(t *testing.T) {
	t.Run("select value resolves to option name", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- severity: high (High)")
	})

	t.Run("multi-select []string is joined and resolved", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"affected_systems": {
				FieldID: "affected_systems",
				Type:    types.FieldTypeMultiSelect,
				Value:   []string{"prod", "staging"},
			},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- affected_systems: prod, staging (Production, Staging)")
	})

	t.Run("multi-select []any (from Firestore decode) is joined and resolved", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"affected_systems": {
				FieldID: "affected_systems",
				Type:    types.FieldTypeMultiSelect,
				Value:   []any{"staging", "prod"},
			},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- affected_systems: staging, prod (Staging, Production)")
	})

	t.Run("unknown option id falls back to raw value with no parenthetical", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "ghost"},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- severity: ghost")
		// No parenthetical because the option ID did not resolve.
		mustNotContain(t, got, "severity: ghost (")
	})

	t.Run("text field renders verbatim with no resolution", func(t *testing.T) {
		c := newCase(7)
		c.FieldValues = map[string]model.FieldValue{
			"notes": {FieldID: "notes", Type: types.FieldTypeText, Value: "free text here"},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newCustomFieldsWorkspace(),
			Case:      c,
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "- notes: free text here")
		mustNotContain(t, got, "notes: free text here (")
	})
}

func TestBuildSystemPrompt_CurrentTime(t *testing.T) {
	t.Run("renders the turn start time normalised to UTC RFC3339", func(t *testing.T) {
		// Deliberately non-UTC input to prove the section is normalised.
		jst := time.FixedZone("JST", 9*60*60)
		now := time.Date(2026, 5, 23, 21, 30, 0, 0, jst)
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
			Now:       now,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Current time")
		mustContain(t, got, "The current time (this turn's execution start) is 2026-05-23T12:30:00Z (UTC).")
	})

	t.Run("omits the section when Now is the zero value", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job:       caseCreatedJob(),
			Workspace: newWorkspace("ws", "WS"),
			Case:      newCase(7),
			Event:     caseCreatedEvent(),
		})
		gt.NoError(t, err).Required()
		mustNotContain(t, got, "# Current time")
	})
}

func newWorkspaceWithMemo(id, name string) *model.WorkspaceEntry {
	ws := newWorkspace(id, name)
	ws.MemoConfig = &config.MemoConfig{
		Description: "Investigation memory for this case.",
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "memo_type", Name: "Type", Type: types.FieldTypeSelect, Required: true, Options: []config.FieldOption{
				{ID: "fact", Name: "Fact"},
			}},
			{ID: "body", Name: "Body", Type: types.FieldTypeText},
		}},
	}
	return ws
}

func TestBuildSystemPrompt_MemoSection(t *testing.T) {
	ev := job.Event{Domain: model.JobEventDomainCase, WorkspaceID: "ws", CaseID: 42}

	t.Run("renders definition, fields and active memos", func(t *testing.T) {
		now := time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC)
		memos := []*model.Memo{
			{ID: "m-1", WorkspaceID: "ws", CaseID: 42, Title: "first memory", CreatedAt: now, UpdatedAt: now},
			{ID: "m-2", WorkspaceID: "ws", CaseID: 42, Title: "second memory", CreatedAt: now, UpdatedAt: now},
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: &model.Job{ID: "j"}, Workspace: newWorkspaceWithMemo("ws", "WS"), Case: newCase(42), Memos: memos, Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Memos (case-scoped memory)")
		mustContain(t, got, "Investigation memory for this case.")
		mustContain(t, got, "memo_type (select): Type [required]")
		mustContain(t, got, "Current memos (2 total)")
		mustContain(t, got, "`m-1` first memory")
		mustContain(t, got, "`m-2` second memory")
		// The batching instruction must ship with the batch tool: without it
		// the model falls back to one write call per memo.
		mustContain(t, got, "memo__apply_memo_changes")
		mustContain(t, got, "submit them in a single call")
	})

	t.Run("caps preview at 20 and reports overflow", func(t *testing.T) {
		now := time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC)
		var memos []*model.Memo
		for i := range 25 {
			memos = append(memos, &model.Memo{ID: model.MemoID("m-" + string(rune('a'+i))), WorkspaceID: "ws", CaseID: 42, Title: "memory", CreatedAt: now, UpdatedAt: now})
		}
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: &model.Job{ID: "j"}, Workspace: newWorkspaceWithMemo("ws", "WS"), Case: newCase(42), Memos: memos, Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "Current memos (25 total, showing first 20)")
		mustContain(t, got, "more memos exist; use memo__list_memos")
	})

	t.Run("empty active memos render none-yet", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: &model.Job{ID: "j"}, Workspace: newWorkspaceWithMemo("ws", "WS"), Case: newCase(42), Event: ev,
		})
		gt.NoError(t, err).Required()
		mustContain(t, got, "# Memos (case-scoped memory)")
		mustContain(t, got, "(none yet)")
	})

	t.Run("memo-disabled workspace omits the section", func(t *testing.T) {
		got, err := job.BuildSystemPrompt(job.PromptInputs{
			Job: &model.Job{ID: "j"}, Workspace: newWorkspace("ws", "WS"), Case: newCase(42), Event: ev,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, strings.Contains(got, "# Memos (case-scoped memory)")).False()
	})
}

func TestParseUserPromptTemplate(t *testing.T) {
	t.Run("well-formed template parses", func(t *testing.T) {
		_, err := job.ParseUserPromptTemplate("Summarize case {{ .Case.Title }}")
		gt.NoError(t, err)
	})

	t.Run("template using the shared join func parses", func(t *testing.T) {
		// join is registered in promptFuncs; a parser that ran without the
		// runtime FuncMap would reject this template as an unknown function,
		// so this guards against validation/runtime dialect drift.
		_, err := job.ParseUserPromptTemplate(`{{ join .Case.AssigneeIDs ", " }}`)
		gt.NoError(t, err)
	})

	t.Run("empty source parses (emptiness is a separate invariant)", func(t *testing.T) {
		// An empty template is a syntactically valid template; the non-empty
		// prompt requirement is enforced by model.Job.Validate, not here.
		_, err := job.ParseUserPromptTemplate("")
		gt.NoError(t, err)
	})

	t.Run("unbalanced action is rejected", func(t *testing.T) {
		_, err := job.ParseUserPromptTemplate("Summarize {{ .Case.Title")
		gt.Error(t, err)
	})

	t.Run("unknown function is rejected", func(t *testing.T) {
		_, err := job.ParseUserPromptTemplate("{{ definitelyNotAFunc .Case }}")
		gt.Error(t, err)
	})

	t.Run("a template that parses here also renders at runtime", func(t *testing.T) {
		// The two paths must agree: anything ParseUserPromptTemplate accepts
		// must be renderable by RenderUserPrompt (same FuncMap), and anything it
		// rejects must fail to render.
		const good = `{{ join .Case.AssigneeIDs "," }} / {{ .Case.Title }}`
		_, err := job.ParseUserPromptTemplate(good)
		gt.NoError(t, err).Required()
		rendered, err := job.RenderUserPrompt(job.PromptInputs{Job: &model.Job{ID: "render_check", Prompt: good}, Case: newCase(7)})
		gt.NoError(t, err).Required()
		gt.String(t, rendered).Equal("U-A1,U-A2 / Sample")

		const bad = "{{ .Case.Title"
		_, err = job.ParseUserPromptTemplate(bad)
		gt.Error(t, err)
		_, err = job.RenderUserPrompt(job.PromptInputs{Job: &model.Job{ID: "render_check_bad", Prompt: bad}, Case: newCase(8)})
		gt.Error(t, err)
	})
}
