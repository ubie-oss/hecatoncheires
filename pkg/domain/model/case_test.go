package model_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
)

func TestIsCaseAccessible(t *testing.T) {
	tests := []struct {
		name     string
		c        *model.Case
		userID   string
		expected bool
	}{
		{
			name:     "public case is always accessible",
			c:        &model.Case{IsPrivate: false},
			userID:   "U001",
			expected: true,
		},
		{
			name: "private case accessible to channel member",
			c: &model.Case{
				IsPrivate:      true,
				ChannelUserIDs: []string{"U001", "U002", "U003"},
			},
			userID:   "U002",
			expected: true,
		},
		{
			name: "private case not accessible to non-member",
			c: &model.Case{
				IsPrivate:      true,
				ChannelUserIDs: []string{"U001", "U002"},
			},
			userID:   "U999",
			expected: false,
		},
		{
			name: "private case with empty members is not accessible",
			c: &model.Case{
				IsPrivate:      true,
				ChannelUserIDs: []string{},
			},
			userID:   "U001",
			expected: false,
		},
		{
			name: "private case with nil members is not accessible",
			c: &model.Case{
				IsPrivate:      true,
				ChannelUserIDs: nil,
			},
			userID:   "U001",
			expected: false,
		},
		{
			name:     "public case with empty userID is accessible",
			c:        &model.Case{IsPrivate: false},
			userID:   "",
			expected: true,
		},
		{
			name: "private channel-mode case is accessible to its own channel actor",
			c: &model.Case{
				IsPrivate:      true,
				SlackChannelID: "C0CASE01",
				ChannelUserIDs: []string{"U001"},
			},
			userID:   model.CaseChannelActorID("ws-1", "C0CASE01"),
			expected: true,
		},
		{
			name: "private case is not accessible to another channel's actor",
			c: &model.Case{
				IsPrivate:      true,
				SlackChannelID: "C0CASE01",
				ChannelUserIDs: []string{"U001"},
			},
			userID:   model.CaseChannelActorID("ws-1", "C0OTHER9"),
			expected: false,
		},
		{
			name: "private thread-mode case is not accessible to the monitored channel's actor",
			c: &model.Case{
				IsPrivate:      true,
				SlackChannelID: "C0MONITOR",
				SlackThreadTS:  "1700000000.000100",
				ChannelUserIDs: []string{"U001"},
			},
			userID:   model.CaseChannelActorID("ws-1", "C0MONITOR"),
			expected: false,
		},
		{
			name: "private case without a channel is not accessible to an empty channel actor",
			c: &model.Case{
				IsPrivate:      true,
				ChannelUserIDs: []string{"U001"},
			},
			userID:   "@channel:ws-1/",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := model.IsCaseAccessible(tt.c, tt.userID)
			if result != tt.expected {
				t.Errorf("IsCaseAccessible() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestRestrictCase(t *testing.T) {
	now := time.Now()
	archivedAt := now.Add(-time.Hour)
	original := &model.Case{
		ID:             42,
		Title:          "Sensitive Title",
		Description:    "Secret description",
		Status:         types.CaseStatusOpen,
		AssigneeIDs:    []string{"U001"},
		SlackChannelID: "C001",
		IsPrivate:      true,
		ChannelUserIDs: []string{"U001", "U002"},
		FieldValues:    map[string]model.FieldValue{"f1": {FieldID: "f1"}},
		ArchivedAt:     &archivedAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	restricted := model.RestrictCase(original)

	// ArchivedAt survives the restriction: a restricted row still has to render
	// consistently with the archive slice it was returned in, so an archived
	// private case must not read as active on the Archived tab.
	gt.Value(t, restricted.ArchivedAt).NotNil().Required()
	gt.Bool(t, restricted.ArchivedAt.Equal(archivedAt)).True()
	gt.Bool(t, restricted.IsArchived()).True()

	// Preserved fields
	if restricted.ID != 42 {
		t.Errorf("ID = %d, want 42", restricted.ID)
	}
	if restricted.Status != types.CaseStatusOpen {
		t.Errorf("Status = %v, want %v", restricted.Status, types.CaseStatusOpen)
	}
	if !restricted.IsPrivate {
		t.Error("IsPrivate should be true")
	}
	if !restricted.AccessDenied {
		t.Error("AccessDenied should be true")
	}
	if !restricted.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", restricted.CreatedAt, now)
	}
	if !restricted.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want %v", restricted.UpdatedAt, now)
	}

	// Cleared fields
	if restricted.Title != "" {
		t.Errorf("Title = %q, want empty", restricted.Title)
	}
	if restricted.Description != "" {
		t.Errorf("Description = %q, want empty", restricted.Description)
	}
	if len(restricted.AssigneeIDs) != 0 {
		t.Errorf("AssigneeIDs = %v, want empty", restricted.AssigneeIDs)
	}
	if restricted.SlackChannelID != "" {
		t.Errorf("SlackChannelID = %q, want empty", restricted.SlackChannelID)
	}
	if len(restricted.ChannelUserIDs) != 0 {
		t.Errorf("ChannelUserIDs = %v, want empty", restricted.ChannelUserIDs)
	}
	if len(restricted.FieldValues) != 0 {
		t.Errorf("FieldValues = %v, want empty", restricted.FieldValues)
	}

	// Original should be unchanged
	if original.Title != "Sensitive Title" {
		t.Error("Original case should not be modified")
	}
}

func TestCase_IsDraft(t *testing.T) {
	gt.Bool(t, (&model.Case{Status: types.CaseStatusDraft}).IsDraft()).True()
	gt.Bool(t, (&model.Case{Status: types.CaseStatusOpen}).IsDraft()).False()
	gt.Bool(t, (&model.Case{Status: types.CaseStatusClosed}).IsDraft()).False()
	gt.Bool(t, (&model.Case{Status: types.CaseStatus("")}).IsDraft()).False()

	// Nil receiver is safe and reports false; this guards against accidental
	// nil dereference in higher layers that route Case lookups through
	// IsDraft as a guard.
	var nilCase *model.Case
	gt.Bool(t, nilCase.IsDraft()).False()
}

func TestCase_IsArchived(t *testing.T) {
	archivedAt := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	gt.Bool(t, (&model.Case{ArchivedAt: &archivedAt}).IsArchived()).True()
	gt.Bool(t, (&model.Case{ArchivedAt: nil}).IsArchived()).False()
	gt.Bool(t, (&model.Case{}).IsArchived()).False()

	// Nil receiver is safe and reports false, mirroring IsDraft: higher layers
	// route Case lookups through this as a guard.
	var nilCase *model.Case
	gt.Bool(t, nilCase.IsArchived()).False()
}

func TestCase_IsThreadBound(t *testing.T) {
	gt.Bool(t, (&model.Case{SlackThreadTS: "1700000000.000100"}).IsThreadBound()).True()
	gt.Bool(t, (&model.Case{SlackThreadTS: ""}).IsThreadBound()).False()
	gt.Bool(t, (&model.Case{SlackChannelID: "C001"}).IsThreadBound()).False()

	var nilCase *model.Case
	gt.Bool(t, nilCase.IsThreadBound()).False()
}

func TestCase_SyncLifecycleFromBoardStatus(t *testing.T) {
	set, err := model.NewActionStatusSet(
		"TRIAGE",
		[]string{"DONE"},
		[]model.ActionStatusDefinition{
			{ID: "TRIAGE", Name: "Triage"},
			{ID: "INVESTIGATING", Name: "Investigating"},
			{ID: "DONE", Name: "Done"},
		},
	)
	gt.NoError(t, err).Required()

	t.Run("open board status maps to OPEN", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusOpen, BoardStatus: "INVESTIGATING"}
		c.SyncLifecycleFromBoardStatus(set)
		gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
	})

	t.Run("closed board status maps to CLOSED", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusOpen, BoardStatus: "DONE"}
		c.SyncLifecycleFromBoardStatus(set)
		gt.Value(t, c.Status).Equal(types.CaseStatusClosed)
	})

	t.Run("reopening from closed maps back to OPEN", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusClosed, BoardStatus: "TRIAGE"}
		c.SyncLifecycleFromBoardStatus(set)
		gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
	})

	t.Run("draft is left untouched", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusDraft, BoardStatus: "DONE"}
		c.SyncLifecycleFromBoardStatus(set)
		gt.Value(t, c.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("nil set is a no-op", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusOpen, BoardStatus: "DONE"}
		c.SyncLifecycleFromBoardStatus(nil)
		gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
	})

	t.Run("empty board status is a no-op", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusOpen, BoardStatus: ""}
		c.SyncLifecycleFromBoardStatus(set)
		gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
	})
}

func TestCase_Validate(t *testing.T) {
	t.Run("nil case returns error", func(t *testing.T) {
		var c *model.Case
		gt.Error(t, c.Validate())
	})

	t.Run("case without ReporterID passes Validate", func(t *testing.T) {
		c := &model.Case{Title: "No Reporter"}
		gt.NoError(t, c.Validate())
	})

	t.Run("case with ReporterID passes Validate", func(t *testing.T) {
		c := &model.Case{Title: "Has Reporter", ReporterID: "UREPORTER123"}
		gt.NoError(t, c.Validate())
	})

	t.Run("agent additional prompt within limit passes Validate", func(t *testing.T) {
		c := &model.Case{
			Title:                 "Within limit",
			AgentAdditionalPrompt: strings.Repeat("a", model.AgentAdditionalPromptMaxLen),
		}
		gt.NoError(t, c.Validate())
	})

	t.Run("agent additional prompt over limit fails Validate", func(t *testing.T) {
		c := &model.Case{
			Title:                 "Over limit",
			AgentAdditionalPrompt: strings.Repeat("a", model.AgentAdditionalPromptMaxLen+1),
		}
		err := c.Validate()
		gt.Error(t, err)
		gt.Bool(t, errors.Is(err, model.ErrCaseAgentPromptTooLong)).True()
	})
}

func TestCase_ValidateNew(t *testing.T) {
	t.Run("nil case returns error", func(t *testing.T) {
		var c *model.Case
		gt.Error(t, c.ValidateNew())
	})

	t.Run("channel-mode case without ReporterID fails ValidateNew", func(t *testing.T) {
		// No SlackThreadTS => channel-mode; reporter is mandatory.
		c := &model.Case{Title: "No Reporter"}
		err := c.ValidateNew()
		gt.Error(t, err)
		gt.Bool(t, errors.Is(err, model.ErrCaseMissingReporter)).True()
	})

	t.Run("case with ReporterID passes ValidateNew", func(t *testing.T) {
		c := &model.Case{Title: "Has Reporter", ReporterID: "UREPORTER123"}
		gt.NoError(t, c.ValidateNew())
	})

	t.Run("thread-mode case without ReporterID passes ValidateNew", func(t *testing.T) {
		// SlackThreadTS set => thread-mode; an integration-bot intake post may
		// name no human, so an empty ReporterID is a legitimate state.
		c := &model.Case{
			Title:          "Bot-relayed, no reporter",
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  "1700000000.000900",
		}
		gt.NoError(t, c.ValidateNew())
	})

	t.Run("thread-mode case with ReporterID passes ValidateNew", func(t *testing.T) {
		c := &model.Case{
			Title:          "Thread case with reporter",
			ReporterID:     "UREPORTER123",
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  "1700000000.000901",
		}
		gt.NoError(t, c.ValidateNew())
	})
}

func TestCase_SubmitDraft(t *testing.T) {
	t.Run("draft transitions to open", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusDraft}
		gt.NoError(t, c.SubmitDraft()).Required()
		gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
	})

	t.Run("open case cannot be re-submitted", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusOpen}
		err := c.SubmitDraft()
		gt.Error(t, err)
		gt.Bool(t, errors.Is(err, model.ErrCaseNotDraft)).True()
		// Status must stay unchanged.
		gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
	})

	t.Run("closed case cannot be submitted", func(t *testing.T) {
		c := &model.Case{Status: types.CaseStatusClosed}
		err := c.SubmitDraft()
		gt.Error(t, err)
		gt.Bool(t, errors.Is(err, model.ErrCaseNotDraft)).True()
		gt.Value(t, c.Status).Equal(types.CaseStatusClosed)
	})

	t.Run("nil receiver returns error", func(t *testing.T) {
		var nilCase *model.Case
		err := nilCase.SubmitDraft()
		gt.Error(t, err)
		gt.Bool(t, errors.Is(err, model.ErrCaseNotDraft)).True()
	})
}

func TestCaseAssignUsers(t *testing.T) {
	t.Run("appends new ids in order and reports change", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1"}}
		changed := c.AssignUsers([]string{"U2", "U3"})
		gt.Bool(t, changed).True()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1", "U2", "U3"})
	})

	t.Run("ignores duplicates and reports no change", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1", "U2"}}
		changed := c.AssignUsers([]string{"U1", "U2"})
		gt.Bool(t, changed).False()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1", "U2"})
	})

	t.Run("adds only the genuinely new ids", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1"}}
		changed := c.AssignUsers([]string{"U1", "U2"})
		gt.Bool(t, changed).True()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1", "U2"})
	})

	t.Run("skips blank ids", func(t *testing.T) {
		c := &model.Case{}
		changed := c.AssignUsers([]string{"", "U1", ""})
		gt.Bool(t, changed).True()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1"})
	})

	t.Run("assigning into empty slice", func(t *testing.T) {
		c := &model.Case{}
		changed := c.AssignUsers([]string{"U1"})
		gt.Bool(t, changed).True()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1"})
	})

	t.Run("does not duplicate within a single input batch", func(t *testing.T) {
		c := &model.Case{}
		changed := c.AssignUsers([]string{"U1", "U1"})
		gt.Bool(t, changed).True()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1"})
	})
}

func TestCaseUnassignUsers(t *testing.T) {
	t.Run("removes ids and preserves remaining order", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1", "U2", "U3"}}
		changed := c.UnassignUsers([]string{"U2"})
		gt.Bool(t, changed).True()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1", "U3"})
	})

	t.Run("removing absent id is a no-op", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1"}}
		changed := c.UnassignUsers([]string{"U9"})
		gt.Bool(t, changed).False()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1"})
	})

	t.Run("removing from empty set is a no-op", func(t *testing.T) {
		c := &model.Case{}
		changed := c.UnassignUsers([]string{"U1"})
		gt.Bool(t, changed).False()
		gt.Array(t, c.AssigneeIDs).Length(0)
	})

	t.Run("empty input is a no-op", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1"}}
		changed := c.UnassignUsers(nil)
		gt.Bool(t, changed).False()
		gt.Value(t, c.AssigneeIDs).Equal([]string{"U1"})
	})

	t.Run("removes all matching ids", func(t *testing.T) {
		c := &model.Case{AssigneeIDs: []string{"U1", "U2"}}
		changed := c.UnassignUsers([]string{"U1", "U2"})
		gt.Bool(t, changed).True()
		gt.Array(t, c.AssigneeIDs).Length(0)
	})
}
