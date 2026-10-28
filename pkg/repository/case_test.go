package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/firestore"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
)

func runCaseRepositoryTest(t *testing.T, newRepo func(t *testing.T) interfaces.Repository) {
	t.Helper()

	t.Run("Create creates case with auto-increment ID", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		case1 := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "SQL Injection Risk",
			Description: "Database vulnerable to SQL injection",
			AssigneeIDs: []string{"U123", "U456"},
		}

		created1, err := repo.Case().Create(ctx, wsID, case1)
		gt.NoError(t, err).Required()

		gt.Value(t, created1.ID).NotEqual(int64(0))
		gt.Value(t, created1.Title).Equal(case1.Title)
		gt.Value(t, created1.Description).Equal(case1.Description)
		gt.Array(t, created1.AssigneeIDs).Length(len(case1.AssigneeIDs))
		gt.Bool(t, created1.CreatedAt.IsZero()).False()
		gt.Bool(t, created1.UpdatedAt.IsZero()).False()

		// Create second case to test auto-increment
		case2 := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "XSS Risk",
			Description: "Cross-site scripting vulnerability",
		}

		created2, err := repo.Case().Create(ctx, wsID, case2)
		gt.NoError(t, err).Required()

		gt.Value(t, created2.ID).NotEqual(created1.ID)
	})

	t.Run("Get retrieves existing case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
			UpdatedAt:   time.Now().UTC().Truncate(time.Millisecond),
			Title:       "CSRF Risk",
			Description: "Cross-site request forgery",
			AssigneeIDs: []string{"U789"},
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		gt.Value(t, retrieved.ID).Equal(created.ID)
		gt.Value(t, retrieved.Title).Equal(created.Title)
		gt.Value(t, retrieved.Description).Equal(created.Description)
		gt.Array(t, retrieved.AssigneeIDs).Length(len(created.AssigneeIDs))
		gt.Bool(t, retrieved.CreatedAt.Equal(created.CreatedAt)).True()
		gt.Bool(t, retrieved.UpdatedAt.Equal(created.UpdatedAt)).True()
	})

	t.Run("Get returns error for non-existent case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		_, err := repo.Case().Get(ctx, wsID, time.Now().UnixNano())
		gt.Value(t, err).NotNil()
	})

	t.Run("GetByIDs returns cases for matching IDs", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		c1, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Case One",
			Description:    "First case",
			AssigneeIDs:    []string{"U111"},
			SlackChannelID: "C111",
		})
		gt.NoError(t, err).Required()

		c2, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Case Two",
			Description:    "Second case",
			AssigneeIDs:    []string{"U222", "U333"},
			SlackChannelID: "C222",
		})
		gt.NoError(t, err).Required()

		c3, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Case Three",
			Description: "Third case",
		})
		gt.NoError(t, err).Required()

		// Request a mix of existing and missing IDs. Missing IDs must
		// be silently absent from the returned map — they are not
		// errors at the repository layer.
		missingID := c3.ID + 999_999
		got, err := repo.Case().GetByIDs(ctx, wsID, []int64{c1.ID, c3.ID, missingID, c2.ID})
		gt.NoError(t, err).Required()
		gt.Map(t, got).HasKey(c1.ID)
		gt.Map(t, got).HasKey(c2.ID)
		gt.Map(t, got).HasKey(c3.ID)
		gt.Number(t, len(got)).Equal(3)

		gotC1 := got[c1.ID]
		gt.Value(t, gotC1.Title).Equal(c1.Title)
		gt.Value(t, gotC1.Description).Equal(c1.Description)
		gt.Value(t, gotC1.SlackChannelID).Equal(c1.SlackChannelID)
		gt.Array(t, gotC1.AssigneeIDs).Length(1)
		gt.Value(t, gotC1.AssigneeIDs[0]).Equal("U111")

		gotC2 := got[c2.ID]
		gt.Value(t, gotC2.Title).Equal(c2.Title)
		gt.Array(t, gotC2.AssigneeIDs).Length(2)
		gt.Value(t, gotC2.AssigneeIDs[0]).Equal("U222")
		gt.Value(t, gotC2.AssigneeIDs[1]).Equal("U333")

		gotC3 := got[c3.ID]
		gt.Value(t, gotC3.Title).Equal(c3.Title)

		_, ok := got[missingID]
		gt.Bool(t, ok).False()
	})

	t.Run("GetByIDs returns empty map for empty ID slice", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		got, err := repo.Case().GetByIDs(ctx, wsID, []int64{})
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(0)
	})

	t.Run("GetByIDs returns empty map for unknown workspace", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		got, err := repo.Case().GetByIDs(ctx, wsID, []int64{1, 2, 3})
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(0)
	})

	t.Run("Update updates existing case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Original Title",
			Description: "Original Description",
		})
		gt.NoError(t, err).Required()

		// Update the case
		originalUpdatedAt := created.UpdatedAt
		time.Sleep(time.Millisecond)
		created.Title = "Updated Title"
		created.Description = "Updated Description"
		created.AssigneeIDs = []string{"U111", "U222"}
		created.UpdatedAt = time.Now().UTC()

		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()

		gt.Value(t, updated.ID).Equal(created.ID)
		gt.Value(t, updated.Title).Equal("Updated Title")
		gt.Value(t, updated.Description).Equal("Updated Description")
		gt.Array(t, updated.AssigneeIDs).Length(2)
		gt.Bool(t, updated.UpdatedAt.Before(originalUpdatedAt) || updated.UpdatedAt.Equal(originalUpdatedAt)).False()
	})

	t.Run("Delete deletes existing case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "To be deleted",
		})
		gt.NoError(t, err).Required()

		err = repo.Case().Delete(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		// Verify it's deleted
		_, err = repo.Case().Get(ctx, wsID, created.ID)
		gt.Value(t, err).NotNil()
	})

	t.Run("Create and Get with FieldValues", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		fieldValues := map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "critical"},
			"score":    {FieldID: "score", Type: types.FieldTypeNumber, Value: 4.5},
			"tags":     {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []string{"data-breach", "compliance"}},
			"url":      {FieldID: "url", Type: types.FieldTypeURL, Value: "https://example.com"},
		}

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Case with fields",
			Description: "Testing field values",
			FieldValues: fieldValues,
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		gt.Number(t, len(retrieved.FieldValues)).Equal(4)

		gt.Value(t, retrieved.FieldValues["severity"].FieldID).Equal("severity")
		gt.Value(t, retrieved.FieldValues["severity"].Type).Equal(types.FieldTypeSelect)
		gt.Value(t, retrieved.FieldValues["severity"].Value).Equal("critical")

		gt.Value(t, retrieved.FieldValues["score"].FieldID).Equal("score")
		gt.Value(t, retrieved.FieldValues["score"].Type).Equal(types.FieldTypeNumber)
		gt.Value(t, retrieved.FieldValues["score"].Value).Equal(4.5)

		gt.Value(t, retrieved.FieldValues["tags"].FieldID).Equal("tags")
		gt.Value(t, retrieved.FieldValues["tags"].Type).Equal(types.FieldTypeMultiSelect)

		gt.Value(t, retrieved.FieldValues["url"].FieldID).Equal("url")
		gt.Value(t, retrieved.FieldValues["url"].Type).Equal(types.FieldTypeURL)
		gt.Value(t, retrieved.FieldValues["url"].Value).Equal("https://example.com")
	})

	t.Run("Create with nil FieldValues", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Case without fields",
			FieldValues: nil,
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		// nil or empty map is acceptable
		gt.Number(t, len(retrieved.FieldValues)).Equal(0)
	})

	t.Run("Create with empty FieldValues", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Case with empty fields",
			FieldValues: map[string]model.FieldValue{},
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		gt.Number(t, len(retrieved.FieldValues)).Equal(0)
	})

	t.Run("Update preserves and modifies FieldValues", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Case to update fields",
			FieldValues: map[string]model.FieldValue{
				"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "low"},
			},
		})
		gt.NoError(t, err).Required()

		// Update with new field values
		created.FieldValues = map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
			"notes":    {FieldID: "notes", Type: types.FieldTypeText, Value: "urgent"},
		}

		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()

		gt.Number(t, len(updated.FieldValues)).Equal(2)
		gt.Value(t, updated.FieldValues["severity"].Value).Equal("high")
		gt.Value(t, updated.FieldValues["notes"].Value).Equal("urgent")

		// Verify via Get as well
		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(retrieved.FieldValues)).Equal(2)
		gt.Value(t, retrieved.FieldValues["severity"].Value).Equal("high")
	})

	t.Run("FieldValues deep copy isolation", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		tags := []string{"tag1", "tag2"}
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Deep copy test",
			FieldValues: map[string]model.FieldValue{
				"tags": {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: tags},
			},
		})
		gt.NoError(t, err).Required()

		// Mutate the original slice
		tags[0] = "mutated"

		// Retrieve and verify the stored value is not affected.
		//
		// The two backends return different concrete slice types here:
		// memory stores the original `[]string` slice, while firestore
		// round-trips through `firestore.DataTo` and reconstitutes the
		// generic `Value any` as `[]interface{}`. The wire-format contract
		// on `FieldValue.Value` is "iterable of strings" (see
		// FieldValue.IsValueInSet which switches on both shapes), so the
		// test asserts on the iterable-of-strings invariant, not on the
		// concrete slice type.
		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		storedTags := toStringSlice(t, retrieved.FieldValues["tags"].Value)
		gt.Value(t, storedTags[0]).Equal("tag1")

		// Also verify that mutating the retrieved value doesn't affect the store
		storedTags[0] = "also-mutated"

		retrieved2, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		storedTags2 := toStringSlice(t, retrieved2.FieldValues["tags"].Value)
		gt.Value(t, storedTags2[0]).Equal("tag1")
	})

	t.Run("Delete removes case with FieldValues", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Case to delete",
			FieldValues: map[string]model.FieldValue{
				"priority": {FieldID: "priority", Type: types.FieldTypeSelect, Value: "high"},
			},
		})
		gt.NoError(t, err).Required()

		err = repo.Case().Delete(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()

		_, err = repo.Case().Get(ctx, wsID, created.ID)
		gt.Value(t, err).NotNil()
	})

	t.Run("GetBySlackChannelID returns matching case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Case with channel",
			SlackChannelID: "C-TEST-CHANNEL",
		})
		gt.NoError(t, err).Required()

		// Update to set SlackChannelID (Create may not persist it directly)
		created.SlackChannelID = "C-TEST-CHANNEL"
		_, err = repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()

		found, err := repo.Case().GetBySlackChannelID(ctx, wsID, "C-TEST-CHANNEL")
		gt.NoError(t, err).Required()
		gt.Value(t, found).NotNil()
		gt.Value(t, found.ID).Equal(created.ID)
		gt.Value(t, found.Title).Equal("Case with channel")
		gt.Value(t, found.SlackChannelID).Equal("C-TEST-CHANNEL")
	})

	t.Run("GetBySlackChannelID returns nil for non-existent channel", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		found, err := repo.Case().GetBySlackChannelID(ctx, wsID, "C-NONEXISTENT")
		gt.NoError(t, err)
		gt.Value(t, found).Nil()
	})

	t.Run("GetBySlackChannelID returns nil for non-existent workspace", func(t *testing.T) {
		repo := newRepo(t)
		ctx := context.Background()

		found, err := repo.Case().GetBySlackChannelID(ctx, "nonexistent-ws", "C-WHATEVER")
		gt.NoError(t, err)
		gt.Value(t, found).Nil()
	})

	t.Run("GetBySlackChannelIDs returns linked cases keyed by channel across query chunks", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		otherWsID := fmt.Sprintf("ws-other-%d", time.Now().UnixNano())
		ctx := context.Background()
		prefix := fmt.Sprintf("C%d-", time.Now().UnixNano())

		// 35 IDs exceed one Firestore `in` query (30 values), so the linked
		// channels are placed on both sides of the chunk boundary.
		channelIDs := make([]string, 35)
		for i := range channelIDs {
			channelIDs[i] = fmt.Sprintf("%s%02d", prefix, i)
		}
		linked := []string{channelIDs[2], channelIDs[29], channelIDs[33]}

		now := time.Now().UTC().Truncate(time.Millisecond)
		want := make(map[string]*model.Case, len(linked))
		for i, ch := range linked {
			created, err := repo.Case().Create(ctx, wsID, &model.Case{
				ReporterID:     "U-TEST-DEFAULT",
				CreatedAt:      now,
				UpdatedAt:      now,
				Title:          fmt.Sprintf("Linked case %d", i),
				Description:    "linked to an existing channel",
				AssigneeIDs:    []string{"U111"},
				SlackChannelID: ch,
				ChannelUserIDs: []string{"U111", "U222"},
				IsPrivate:      i == 1,
				Status:         types.CaseStatusOpen,
			})
			gt.NoError(t, err).Required()
			stored, err := repo.Case().Get(ctx, wsID, created.ID)
			gt.NoError(t, err).Required()
			want[ch] = stored
		}
		// A case in another workspace must not leak into this workspace's result.
		_, err := repo.Case().Create(ctx, otherWsID, &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			CreatedAt:      now,
			UpdatedAt:      now,
			Title:          "Other workspace",
			SlackChannelID: channelIDs[5],
		})
		gt.NoError(t, err).Required()

		got, err := repo.Case().GetBySlackChannelIDs(ctx, wsID, channelIDs)
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(len(linked)).Required()
		for _, ch := range linked {
			gt.Map(t, got).HasKey(ch).Required()
			gt.Value(t, got[ch]).Equal(want[ch])
		}
	})

	t.Run("GetBySlackChannelIDs returns an empty map for empty input", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())

		got, err := repo.Case().GetBySlackChannelIDs(context.Background(), wsID, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, got).NotNil()
		gt.Number(t, len(got)).Equal(0)
	})

	t.Run("thread-mode round-trip persists SlackThreadTS and BoardStatus", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		threadTS := fmt.Sprintf("%d.000100", time.Now().UnixNano())
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-THREAD-REPORTER",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Thread case",
			Description:    "Created from a monitored channel post",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  threadTS,
			BoardStatus:    "TRIAGE",
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.SlackThreadTS).Equal(threadTS)
		gt.Value(t, retrieved.BoardStatus).Equal("TRIAGE")
		gt.Value(t, retrieved.SlackChannelID).Equal("C-MONITOR")
		gt.Bool(t, retrieved.IsThreadBound()).True()
	})

	t.Run("IsTest round-trips on Create and toggles on Update", func(t *testing.T) {
		// IsTest is a persisted scalar; a Firestore Create that rebuilt the
		// struct field-by-field would silently drop it (the canonical bug this
		// suite guards against), so assert it both on Create and after Update.
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-FLAG",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Test-flagged case",
			Status:     types.CaseStatusOpen,
			IsTest:     true,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsTest).True()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, retrieved.IsTest).True()

		// Flip it off and confirm the false value persists (not just "unset").
		retrieved.IsTest = false
		updated, err := repo.Case().Update(ctx, wsID, retrieved)
		gt.NoError(t, err).Required()
		gt.Bool(t, updated.IsTest).False()

		reRetrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, reRetrieved.IsTest).False()
	})

	t.Run("IsTest defaults to false when omitted on Create", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Untagged case",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsTest).False()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, retrieved.IsTest).False()
	})

	t.Run("IsPrivate round-trips on Create and toggles on Update", func(t *testing.T) {
		// IsPrivate drives private-case access control; a Create/Update that
		// rebuilt the struct field-by-field and dropped it would silently
		// expose a private case. Assert it on Create, after Get, and after a
		// toggle-to-false Update. ChannelUserIDs is round-tripped alongside
		// because IsPrivate is meaningless without the member set it gates.
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		members := []string{"U-MEMBER-1", "U-MEMBER-2"}
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-OWNER",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Private case",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: members,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsPrivate).True()
		gt.Value(t, created.ChannelUserIDs).Equal(members)

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, retrieved.IsPrivate).True()
		gt.Value(t, retrieved.ChannelUserIDs).Equal(members)

		// Flip it off and confirm the false value persists (not just "unset").
		retrieved.IsPrivate = false
		updated, err := repo.Case().Update(ctx, wsID, retrieved)
		gt.NoError(t, err).Required()
		gt.Bool(t, updated.IsPrivate).False()

		reRetrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, reRetrieved.IsPrivate).False()
	})

	t.Run("thread-mode case may be created with an empty reporter", func(t *testing.T) {
		// A channel-root intake post relayed by an integration bot may name no
		// human, so a thread-mode Create must accept an empty ReporterID
		// (ValidateNew exempts thread-mode). Channel-mode still requires one.
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		threadTS := fmt.Sprintf("%d.000133", time.Now().UnixNano())
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Bot-relayed thread case",
			Description:    "No human reporter named in the form",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  threadTS,
			BoardStatus:    "TRIAGE",
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.ReporterID).Equal("")
		gt.Value(t, retrieved.SlackThreadTS).Equal(threadTS)
		gt.Value(t, retrieved.Title).Equal("Bot-relayed thread case")
		gt.Bool(t, retrieved.IsThreadBound()).True()
	})

	t.Run("channel-mode case still requires a reporter", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Channel case without reporter",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C-DEDICATED",
		})
		gt.Error(t, err).Required()
		gt.Bool(t, errors.Is(err, model.ErrCaseMissingReporter)).True()
	})

	t.Run("GetBySlackThread returns matching thread case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		threadA := fmt.Sprintf("%d.000111", time.Now().UnixNano())
		threadB := fmt.Sprintf("%d.000222", time.Now().UnixNano())

		caseA, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-A",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Thread A",
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  threadA,
			BoardStatus:    "TRIAGE",
		})
		gt.NoError(t, err).Required()

		_, err = repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-B",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Thread B",
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  threadB,
			BoardStatus:    "TRIAGE",
		})
		gt.NoError(t, err).Required()

		found, err := repo.Case().GetBySlackThread(ctx, wsID, "C-MONITOR", threadA)
		gt.NoError(t, err).Required()
		gt.Value(t, found).NotNil().Required()
		gt.Value(t, found.ID).Equal(caseA.ID)
		gt.Value(t, found.Title).Equal("Thread A")
		gt.Value(t, found.SlackThreadTS).Equal(threadA)
	})

	t.Run("GetBySlackThread returns nil for unknown thread", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		found, err := repo.Case().GetBySlackThread(ctx, wsID, "C-MONITOR", "9999999999.000000")
		gt.NoError(t, err)
		gt.Value(t, found).Nil()
	})

	t.Run("GetBySlackThread does not match when channel differs", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		threadTS := fmt.Sprintf("%d.000333", time.Now().UnixNano())
		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:     "U-C",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
			Title:          "Thread C",
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  threadTS,
			BoardStatus:    "TRIAGE",
		})
		gt.NoError(t, err).Required()

		found, err := repo.Case().GetBySlackThread(ctx, wsID, "C-OTHER", threadTS)
		gt.NoError(t, err)
		gt.Value(t, found).Nil()
	})

	t.Run("ScanAll streams every case including drafts", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		titleByStatus := map[types.CaseStatus]string{
			types.CaseStatusOpen:   "Open case",
			types.CaseStatusClosed: "Closed case",
			types.CaseStatusDraft:  "Draft case",
		}
		for _, status := range []types.CaseStatus{types.CaseStatusOpen, types.CaseStatusClosed, types.CaseStatusDraft} {
			_, err := repo.Case().Create(ctx, wsID, &model.Case{
				ReporterID: "U-SCAN",
				CreatedAt:  time.Now().UTC(),
				UpdatedAt:  time.Now().UTC(),
				Title:      titleByStatus[status],
				Status:     status,
			})
			gt.NoError(t, err).Required()
		}

		seen := map[string]types.CaseStatus{}
		gt.NoError(t, repo.Case().ScanAll(ctx, wsID, func(c *model.Case) error {
			seen[c.Title] = c.Status
			return nil
		})).Required()

		gt.Value(t, len(seen)).Equal(3)
		gt.Value(t, seen["Open case"]).Equal(types.CaseStatusOpen)
		gt.Value(t, seen["Closed case"]).Equal(types.CaseStatusClosed)
		gt.Value(t, seen["Draft case"]).Equal(types.CaseStatusDraft)

		// List / ListDrafts are status-filtered, which is why ScanAll exists.
		listed, err := repo.Case().List(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Array(t, listed).Length(2)
	})

	t.Run("ScanAll does not invoke fn for an empty workspace", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		calls := 0
		gt.NoError(t, repo.Case().ScanAll(ctx, wsID, func(_ *model.Case) error {
			calls++
			return nil
		})).Required()
		gt.Value(t, calls).Equal(0)
	})

	t.Run("ScanAll aborts and propagates the error returned by fn", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		for i := 0; i < 3; i++ {
			_, err := repo.Case().Create(ctx, wsID, &model.Case{
				ReporterID: "U-SCAN",
				CreatedAt:  time.Now().UTC(),
				UpdatedAt:  time.Now().UTC(),
				Title:      fmt.Sprintf("Case %d", i),
			})
			gt.NoError(t, err).Required()
		}

		stop := errors.New("stop scanning")
		calls := 0
		err := repo.Case().ScanAll(ctx, wsID, func(_ *model.Case) error {
			calls++
			return stop
		})
		gt.Error(t, err).Is(stop)
		gt.Value(t, calls).Equal(1)
	})

	t.Run("ScanAll preserves every field of the scanned case", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created := time.Now().UTC().Truncate(time.Second)
		archivedAt := created.Add(time.Hour)
		threadTS := fmt.Sprintf("%d.000777", time.Now().UnixNano())
		want := &model.Case{
			ReporterID:            "U-REPORTER",
			AssigneeIDs:           []string{"U-A1", "U-A2"},
			CreatedAt:             created,
			UpdatedAt:             created,
			ArchivedAt:            &archivedAt,
			Title:                 "Full case",
			Description:           "Every field populated",
			Status:                types.CaseStatusOpen,
			SlackChannelID:        "C-MONITOR",
			SlackThreadTS:         threadTS,
			BoardStatus:           "TRIAGE",
			IsPrivate:             true,
			IsTest:                true,
			ChannelUserIDs:        []string{"U-M1"},
			RequestKey:            fmt.Sprintf("req-%d", time.Now().UnixNano()),
			AgentAdditionalPrompt: "extra prompt",
			AgentSourceIDs:        []model.SourceID{"src-1"},
			FieldValues: map[string]model.FieldValue{
				"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
			},
		}
		createdCase, err := repo.Case().Create(ctx, wsID, want)
		gt.NoError(t, err).Required()

		var got *model.Case
		gt.NoError(t, repo.Case().ScanAll(ctx, wsID, func(c *model.Case) error {
			got = c
			return nil
		})).Required()
		gt.Value(t, got).NotNil().Required()

		gt.Value(t, got.ID).Equal(createdCase.ID)
		gt.Value(t, got.Title).Equal("Full case")
		gt.Value(t, got.Description).Equal("Every field populated")
		gt.Value(t, got.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, got.ReporterID).Equal("U-REPORTER")
		gt.Value(t, got.AssigneeIDs).Equal([]string{"U-A1", "U-A2"})
		gt.Value(t, got.SlackChannelID).Equal("C-MONITOR")
		gt.Value(t, got.SlackThreadTS).Equal(threadTS)
		gt.Value(t, got.BoardStatus).Equal("TRIAGE")
		gt.Bool(t, got.IsPrivate).True()
		gt.Bool(t, got.IsTest).True()
		gt.Value(t, got.ChannelUserIDs).Equal([]string{"U-M1"})
		gt.Value(t, got.RequestKey).Equal(want.RequestKey)
		gt.Value(t, got.AgentAdditionalPrompt).Equal("extra prompt")
		gt.Value(t, got.AgentSourceIDs).Equal([]model.SourceID{"src-1"})
		gt.Bool(t, got.CreatedAt.Equal(created)).True()
		gt.Bool(t, got.UpdatedAt.Equal(created)).True()
		gt.Value(t, got.ArchivedAt).NotNil().Required()
		gt.Bool(t, got.ArchivedAt.Equal(archivedAt)).True()
		gt.Bool(t, got.IsArchived()).True()

		fv, ok := got.FieldValues["severity"]
		gt.Bool(t, ok).True().Required()
		gt.Value(t, fv.Type).Equal(types.FieldTypeSelect)
		gt.Value(t, fv.Value).Equal("high")
	})

	t.Run("List retrieves all cases", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		// Create multiple cases
		for i := 0; i < 3; i++ {
			_, err := repo.Case().Create(ctx, wsID, &model.Case{
				ReporterID:  "U-TEST-DEFAULT",
				CreatedAt:   time.Now().UTC(),
				UpdatedAt:   time.Now().UTC(),
				Title:       "Case " + string(rune('A'+i)),
				Description: "Description " + string(rune('A'+i)),
			})
			gt.NoError(t, err).Required()
		}

		cases, err := repo.Case().List(ctx, wsID)
		gt.NoError(t, err).Required()

		gt.Number(t, len(cases)).GreaterOrEqual(3)
	})

	t.Run("List with status filter returns only matching cases", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		// Create open cases
		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Open Case 1",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()

		_, err = repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Open Case 2",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()

		// Create closed case
		_, err = repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Closed Case 1",
			Status:     types.CaseStatusClosed,
		})
		gt.NoError(t, err).Required()

		// Filter by OPEN
		openCases, err := repo.Case().List(ctx, wsID, interfaces.WithStatus(types.CaseStatusOpen))
		gt.NoError(t, err).Required()
		gt.Number(t, len(openCases)).Equal(2)
		for _, c := range openCases {
			gt.Value(t, c.Status).Equal(types.CaseStatusOpen)
		}

		// Filter by CLOSED
		closedCases, err := repo.Case().List(ctx, wsID, interfaces.WithStatus(types.CaseStatusClosed))
		gt.NoError(t, err).Required()
		gt.Number(t, len(closedCases)).Equal(1)
		gt.Value(t, closedCases[0].Status).Equal(types.CaseStatusClosed)
		gt.Value(t, closedCases[0].Title).Equal("Closed Case 1")

		// No filter returns all
		allCases, err := repo.Case().List(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(allCases)).Equal(3)
	})

	t.Run("Create and Get preserves status", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Status Test",
			Status:     types.CaseStatusClosed,
		})
		gt.NoError(t, err).Required()

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.Status).Equal(types.CaseStatusClosed)
	})

	t.Run("Update preserves status change", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Status Update Test",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()

		created.Status = types.CaseStatusClosed
		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Status).Equal(types.CaseStatusClosed)

		retrieved, err := repo.Case().Get(ctx, wsID, updated.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.Status).Equal(types.CaseStatusClosed)
	})

	t.Run("Create and retrieve case with ReporterID", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:       "Reporter Test",
			Description: "Testing reporter persistence",
			ReporterID:  "UREPORTER123",
			AssigneeIDs: []string{"UASSIGNEE"},
		})
		gt.NoError(t, err).Required()
		gt.String(t, created.ReporterID).Equal("UREPORTER123")

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.String(t, retrieved.ReporterID).Equal("UREPORTER123")
	})

	t.Run("Create case without ReporterID is rejected by validation", func(t *testing.T) {
		// Repositories now enforce model.Case.Validate at the write
		// boundary. Persisting a case without a reporter would render
		// the Cases / Drafts UI's Reporter column permanently empty
		// for that row — the only way the reporter ever lands is via
		// the auth-context Token at create time, so a missing value
		// here is always a usecase / handler bug. Refuse it.
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title: "No Reporter",
		})
		gt.Error(t, err).Is(model.ErrCaseMissingReporter)
	})

	t.Run("Update case without ReporterID is allowed for legacy data", func(t *testing.T) {
		// Legacy cases created before reporter validation was added may have
		// empty ReporterID. Update must succeed for these so membership sync
		// and other field updates do not fail on old data.
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Legacy Case",
			ReporterID: "UREPORTER_LEGACY",
		})
		gt.NoError(t, err).Required()

		// Simulate legacy case by clearing ReporterID in memory before Update.
		created.ReporterID = ""
		created.ChannelUserIDs = []string{"UMEMBER1", "UMEMBER2"}
		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()
		gt.Array(t, updated.ChannelUserIDs).Length(2)

		retrieved, err := repo.Case().Get(ctx, wsID, updated.ID)
		gt.NoError(t, err).Required()
		gt.String(t, retrieved.ReporterID).Equal("")
		gt.Array(t, retrieved.ChannelUserIDs).Length(2)
	})

	t.Run("Update preserves ReporterID", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Reporter Preserved",
			ReporterID: "UREPORTER456",
		})
		gt.NoError(t, err).Required()

		created.Title = "Updated Title"
		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()
		gt.String(t, updated.ReporterID).Equal("UREPORTER456")

		retrieved, err := repo.Case().Get(ctx, wsID, updated.ID)
		gt.NoError(t, err).Required()
		gt.String(t, retrieved.ReporterID).Equal("UREPORTER456")
	})

	t.Run("GetByRequestKey returns case with matching key", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		requestKey := fmt.Sprintf("test-key-%d", time.Now().UnixNano())
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Idempotent Case",
			RequestKey: requestKey,
		})
		gt.NoError(t, err).Required()

		found, err := repo.Case().GetByRequestKey(ctx, wsID, requestKey)
		gt.NoError(t, err).Required()
		gt.Value(t, found).NotNil()
		gt.Value(t, found.ID).Equal(created.ID)
		gt.String(t, found.Title).Equal("Idempotent Case")
		gt.String(t, found.RequestKey).Equal(requestKey)
	})

	t.Run("GetByRequestKey returns nil for non-existent key", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		found, err := repo.Case().GetByRequestKey(ctx, wsID, "non-existent-key")
		gt.NoError(t, err).Required()
		gt.Value(t, found).Nil()
	})

	t.Run("GetByRequestKey does not match cases with empty key", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		// Create a case without request key
		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "No Key Case",
		})
		gt.NoError(t, err).Required()

		// Search for empty key should not match
		found, err := repo.Case().GetByRequestKey(ctx, wsID, "some-key")
		gt.NoError(t, err).Required()
		gt.Value(t, found).Nil()
	})

	t.Run("List excludes drafts by default", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		open1, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Open Visible",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()

		_, err = repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Draft Hidden",
			Status:     types.CaseStatusDraft,
			ReporterID: "U-author",
		})
		gt.NoError(t, err).Required()

		closed, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Closed Visible",
			Status:     types.CaseStatusClosed,
		})
		gt.NoError(t, err).Required()

		cases, err := repo.Case().List(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(cases)).Equal(2)

		seen := map[int64]bool{}
		for _, c := range cases {
			seen[c.ID] = true
			gt.Bool(t, c.IsDraft()).False()
		}
		gt.Bool(t, seen[open1.ID]).True()
		gt.Bool(t, seen[closed.ID]).True()
	})

	t.Run("List with WithStatus(DRAFT) returns drafts", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-TEST-DEFAULT",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Open A",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()

		draft, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Draft A",
			Status:     types.CaseStatusDraft,
			ReporterID: "U-author",
		})
		gt.NoError(t, err).Required()

		got, err := repo.Case().List(ctx, wsID, interfaces.WithStatus(types.CaseStatusDraft))
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(1)
		gt.Value(t, got[0].ID).Equal(draft.ID)
		gt.Value(t, got[0].Title).Equal("Draft A")
	})

	// seedArchiveScopeCases creates one OPEN case, one active CLOSED case and
	// one archived CLOSED case in a fresh workspace. Only a CLOSED case can be
	// archived (the usecase enforces it), so the archived fixture is CLOSED.
	seedArchiveScopeCases := func(t *testing.T, repo interfaces.Repository, wsID string) (openID, activeClosedID, archivedID int64) {
		t.Helper()
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		archivedAt := now.Add(time.Minute)

		openCase, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-ARCHIVE-SCOPE",
			CreatedAt:  now,
			UpdatedAt:  now,
			Title:      "Open Active",
			Status:     types.CaseStatusOpen,
		})
		gt.NoError(t, err).Required()

		activeClosed, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-ARCHIVE-SCOPE",
			CreatedAt:  now,
			UpdatedAt:  now,
			Title:      "Closed Active",
			Status:     types.CaseStatusClosed,
		})
		gt.NoError(t, err).Required()

		archived, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-ARCHIVE-SCOPE",
			CreatedAt:  now,
			UpdatedAt:  now,
			Title:      "Closed Archived",
			Status:     types.CaseStatusClosed,
			ArchivedAt: &archivedAt,
		})
		gt.NoError(t, err).Required()

		return openCase.ID, activeClosed.ID, archived.ID
	}

	caseIDSet := func(cases []*model.Case) map[int64]bool {
		seen := make(map[int64]bool, len(cases))
		for _, c := range cases {
			seen[c.ID] = true
		}
		return seen
	}

	t.Run("List excludes archived cases by default", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		openID, activeClosedID, archivedID := seedArchiveScopeCases(t, repo, wsID)

		got, err := repo.Case().List(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(2)

		seen := caseIDSet(got)
		gt.Bool(t, seen[openID]).True()
		gt.Bool(t, seen[activeClosedID]).True()
		gt.Bool(t, seen[archivedID]).False()
		for _, c := range got {
			gt.Bool(t, c.IsArchived()).False()
		}
	})

	t.Run("List with the archived-only scope returns only archived cases", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		openID, activeClosedID, archivedID := seedArchiveScopeCases(t, repo, wsID)

		got, err := repo.Case().List(ctx, wsID,
			interfaces.WithArchiveScope(interfaces.CaseArchiveScopeArchivedOnly))
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(1).Required()
		gt.Value(t, got[0].ID).Equal(archivedID)
		gt.Value(t, got[0].Title).Equal("Closed Archived")
		gt.Bool(t, got[0].IsArchived()).True()

		seen := caseIDSet(got)
		gt.Bool(t, seen[openID]).False()
		gt.Bool(t, seen[activeClosedID]).False()
	})

	t.Run("List with the all scope returns active and archived cases", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		openID, activeClosedID, archivedID := seedArchiveScopeCases(t, repo, wsID)

		got, err := repo.Case().List(ctx, wsID,
			interfaces.WithArchiveScope(interfaces.CaseArchiveScopeAll))
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(3)

		seen := caseIDSet(got)
		gt.Bool(t, seen[openID]).True()
		gt.Bool(t, seen[activeClosedID]).True()
		gt.Bool(t, seen[archivedID]).True()
	})

	t.Run("List combines the status filter with the archive scope", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		_, activeClosedID, archivedID := seedArchiveScopeCases(t, repo, wsID)

		archivedClosed, err := repo.Case().List(ctx, wsID,
			interfaces.WithStatus(types.CaseStatusClosed),
			interfaces.WithArchiveScope(interfaces.CaseArchiveScopeArchivedOnly))
		gt.NoError(t, err).Required()
		gt.Number(t, len(archivedClosed)).Equal(1).Required()
		gt.Value(t, archivedClosed[0].ID).Equal(archivedID)

		// The same status filter without the scope must not leak the archived
		// case into the Closed tab.
		activeClosedList, err := repo.Case().List(ctx, wsID,
			interfaces.WithStatus(types.CaseStatusClosed))
		gt.NoError(t, err).Required()
		gt.Number(t, len(activeClosedList)).Equal(1).Required()
		gt.Value(t, activeClosedList[0].ID).Equal(activeClosedID)
	})

	t.Run("ListDrafts is unaffected by the archive scope", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		seedArchiveScopeCases(t, repo, wsID)

		draft, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Draft Untouched",
			Status:     types.CaseStatusDraft,
			ReporterID: "U-author",
		})
		gt.NoError(t, err).Required()

		drafts, err := repo.Case().ListDrafts(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(drafts)).Equal(1).Required()
		gt.Value(t, drafts[0].ID).Equal(draft.ID)
		gt.Bool(t, drafts[0].IsArchived()).False()
	})

	t.Run("ScanAll streams archived cases too", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		openID, activeClosedID, archivedID := seedArchiveScopeCases(t, repo, wsID)

		var scanned []*model.Case
		gt.NoError(t, repo.Case().ScanAll(ctx, wsID, func(c *model.Case) error {
			scanned = append(scanned, c)
			return nil
		})).Required()
		gt.Number(t, len(scanned)).Equal(3)

		seen := caseIDSet(scanned)
		gt.Bool(t, seen[openID]).True()
		gt.Bool(t, seen[activeClosedID]).True()
		gt.Bool(t, seen[archivedID]).True()

		archivedCount := 0
		for _, c := range scanned {
			if c.IsArchived() {
				archivedCount++
			}
		}
		gt.Number(t, archivedCount).Equal(1)
	})

	t.Run("a case stored without ArchivedAt reads back as active", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		// Documents written before the field existed simply carry no
		// ArchivedAt; both backends must decode that as "not archived" so no
		// backfill is needed.
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-LEGACY",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Title:      "Legacy Case",
			Status:     types.CaseStatusClosed,
		})
		gt.NoError(t, err).Required()

		got, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ArchivedAt).Nil()
		gt.Bool(t, got.IsArchived()).False()

		listed, err := repo.Case().List(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(listed)).Equal(1).Required()
		gt.Value(t, listed[0].ID).Equal(created.ID)
	})

	t.Run("Update clears ArchivedAt", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		now := time.Now().UTC().Truncate(time.Second)
		archivedAt := now
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID: "U-UNARCHIVE",
			CreatedAt:  now,
			UpdatedAt:  now,
			Title:      "To Restore",
			Status:     types.CaseStatusClosed,
			ArchivedAt: &archivedAt,
		})
		gt.NoError(t, err).Required()

		created.ArchivedAt = nil
		created.UpdatedAt = now.Add(time.Minute)
		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()
		gt.Value(t, updated.ArchivedAt).Nil()

		got, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ArchivedAt).Nil()
		gt.Bool(t, got.IsArchived()).False()
	})

	t.Run("ListDrafts returns every draft in the workspace regardless of reporter", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		mineA, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Mine A",
			Status:     types.CaseStatusDraft,
			ReporterID: "U-mine",
		})
		gt.NoError(t, err).Required()

		mineB, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Mine B",
			Status:     types.CaseStatusDraft,
			ReporterID: "U-mine",
		})
		gt.NoError(t, err).Required()

		theirs, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Theirs",
			Status:     types.CaseStatusDraft,
			ReporterID: "U-other",
		})
		gt.NoError(t, err).Required()

		// Non-draft cases (even by the same reporter) must NOT appear.
		_, err = repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "Mine but open",
			Status:     types.CaseStatusOpen,
			ReporterID: "U-mine",
		})
		gt.NoError(t, err).Required()

		all, err := repo.Case().ListDrafts(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(all)).Equal(3)

		ids := map[int64]bool{}
		for _, c := range all {
			ids[c.ID] = true
			gt.Value(t, c.Status).Equal(types.CaseStatusDraft)
		}
		gt.Bool(t, ids[mineA.ID]).True()
		gt.Bool(t, ids[mineB.ID]).True()
		gt.Bool(t, ids[theirs.ID]).True()
	})

	t.Run("ListDrafts returns empty when no drafts exist", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		_, err := repo.Case().Create(ctx, wsID, &model.Case{
			Title:      "An open case",
			Status:     types.CaseStatusOpen,
			ReporterID: "U-other",
		})
		gt.NoError(t, err).Required()

		drafts, err := repo.Case().ListDrafts(ctx, wsID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(drafts)).Equal(0)
	})

	t.Run("Create and Get round-trip agent settings", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		prompt := "## Per-case notes\n\n- Treat dataset X as read-only.\n- Cite `audit-log` source for every claim."
		sourceA := model.NewSourceID()
		sourceB := model.NewSourceID()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:            "U-TEST-DEFAULT",
			CreatedAt:             time.Now().UTC(),
			UpdatedAt:             time.Now().UTC(),
			Title:                 "Case with agent settings",
			AgentAdditionalPrompt: prompt,
			AgentSourceIDs:        []model.SourceID{sourceA, sourceB},
		})
		gt.NoError(t, err).Required()

		gt.Value(t, created.AgentAdditionalPrompt).Equal(prompt)
		gt.Array(t, created.AgentSourceIDs).Length(2).Required()
		gt.Value(t, created.AgentSourceIDs[0]).Equal(sourceA)
		gt.Value(t, created.AgentSourceIDs[1]).Equal(sourceB)

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AgentAdditionalPrompt).Equal(prompt)
		gt.Array(t, retrieved.AgentSourceIDs).Length(2).Required()
		gt.Value(t, retrieved.AgentSourceIDs[0]).Equal(sourceA)
		gt.Value(t, retrieved.AgentSourceIDs[1]).Equal(sourceB)
	})

	t.Run("Update modifies agent settings", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:            "U-TEST-DEFAULT",
			CreatedAt:             time.Now().UTC(),
			UpdatedAt:             time.Now().UTC(),
			Title:                 "Case agent update",
			AgentAdditionalPrompt: "initial",
			AgentSourceIDs:        []model.SourceID{model.NewSourceID()},
		})
		gt.NoError(t, err).Required()

		newPrompt := "updated **prompt** body"
		newSrc := model.NewSourceID()
		created.AgentAdditionalPrompt = newPrompt
		created.AgentSourceIDs = []model.SourceID{newSrc}

		updated, err := repo.Case().Update(ctx, wsID, created)
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AgentAdditionalPrompt).Equal(newPrompt)
		gt.Array(t, updated.AgentSourceIDs).Length(1).Required()
		gt.Value(t, updated.AgentSourceIDs[0]).Equal(newSrc)

		retrieved, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AgentAdditionalPrompt).Equal(newPrompt)
		gt.Array(t, retrieved.AgentSourceIDs).Length(1).Required()
		gt.Value(t, retrieved.AgentSourceIDs[0]).Equal(newSrc)

		// Clearing the source list back to empty must round-trip.
		retrieved.AgentSourceIDs = nil
		retrieved.AgentAdditionalPrompt = ""
		cleared, err := repo.Case().Update(ctx, wsID, retrieved)
		gt.NoError(t, err).Required()
		gt.Value(t, cleared.AgentAdditionalPrompt).Equal("")
		gt.Number(t, len(cleared.AgentSourceIDs)).Equal(0)
	})

	t.Run("Transact persists what the closure mutated", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC().Add(-time.Hour),
			Title:       "Assign target",
			AssigneeIDs: []string{"U1"},
		})
		gt.NoError(t, err).Required()

		stamp := time.Now().UTC().Truncate(time.Millisecond)
		added, err := repo.Case().Transact(ctx, wsID, created.ID, func(c *model.Case) error {
			c.AssignUsers([]string{"U2", "U3"})
			c.UpdatedAt = stamp
			return nil
		})
		gt.NoError(t, err).Required()
		gt.Value(t, added.AssigneeIDs).Equal([]string{"U1", "U2", "U3"})
		gt.Bool(t, added.UpdatedAt.Equal(stamp)).True()

		// Persisted state must match what was returned.
		got, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.AssigneeIDs).Equal([]string{"U1", "U2", "U3"})
		gt.Bool(t, got.UpdatedAt.Equal(stamp)).True()
	})

	t.Run("Transact observes the stored state so a no-op closure changes nothing", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		original := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   original,
			Title:       "Dup target",
			AssigneeIDs: []string{"U1", "U2"},
		})
		gt.NoError(t, err).Required()

		// The closure sees the already-assigned ids, so AssignUsers reports no
		// change and the caller leaves UpdatedAt alone.
		var changed bool
		res, err := repo.Case().Transact(ctx, wsID, created.ID, func(c *model.Case) error {
			changed = c.AssignUsers([]string{"U1", "U2"})
			if changed {
				c.UpdatedAt = time.Now().UTC()
			}
			return nil
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, changed).False()
		gt.Value(t, res.AssigneeIDs).Equal([]string{"U1", "U2"})
		gt.Bool(t, res.UpdatedAt.Equal(original)).True()

		got, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.AssigneeIDs).Equal([]string{"U1", "U2"})
		gt.Bool(t, got.UpdatedAt.Equal(original)).True()
	})

	t.Run("Transact removal drops ids and preserves order", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Remove target",
			AssigneeIDs: []string{"U1", "U2", "U3"},
		})
		gt.NoError(t, err).Required()

		stamp := time.Now().UTC().Truncate(time.Millisecond)
		removed, err := repo.Case().Transact(ctx, wsID, created.ID, func(c *model.Case) error {
			c.UnassignUsers([]string{"U2"})
			c.UpdatedAt = stamp
			return nil
		})
		gt.NoError(t, err).Required()
		gt.Value(t, removed.AssigneeIDs).Equal([]string{"U1", "U3"})
		gt.Bool(t, removed.UpdatedAt.Equal(stamp)).True()

		got, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.AssigneeIDs).Equal([]string{"U1", "U3"})
	})

	t.Run("Transact writes nothing when the closure fails", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, wsID, &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Title:       "Abort target",
			AssigneeIDs: []string{"U1"},
		})
		gt.NoError(t, err).Required()

		abort := errors.New("closure rejected the write")
		_, err = repo.Case().Transact(ctx, wsID, created.ID, func(c *model.Case) error {
			c.AssignUsers([]string{"U-SHOULD-NOT-PERSIST"})
			c.Title = "Should not persist"
			return abort
		})
		// The closure's error reaches the caller with its chain intact so the
		// usecase layer can discriminate it (e.g. ErrAccessDenied).
		gt.Error(t, err).Is(abort)

		got, err := repo.Case().Get(ctx, wsID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.AssigneeIDs).Equal([]string{"U1"})
		gt.Value(t, got.Title).Equal("Abort target")
	})

	t.Run("Transact on a missing case errors out", func(t *testing.T) {
		repo := newRepo(t)
		wsID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		ctx := context.Background()

		called := false
		// memory and firestore expose distinct ErrNotFound sentinels, so the
		// shared helper asserts only that the operation fails on a missing case.
		_, err := repo.Case().Transact(ctx, wsID, 99999, func(c *model.Case) error {
			called = true
			return nil
		})
		gt.Error(t, err)
		gt.Bool(t, called).False()
	})
}

func TestCaseRepository_Memory(t *testing.T) {
	t.Parallel()
	runCaseRepositoryTest(t, func(t *testing.T) interfaces.Repository {
		return memory.New()
	})
}

func TestCaseRepository_Firestore(t *testing.T) {
	t.Parallel()
	runCaseRepositoryTest(t, newFirestoreRepository)
}

// newFirestoreRepository constructs a Firestore-backed
// interfaces.Repository for tests. This is the single place that reads
// TEST_FIRESTORE_PROJECT_ID / TEST_FIRESTORE_DATABASE_ID — every
// TestXxxRepository_Firestore in this package must call it instead of
// inlining its own firestore.New (any divergence here re-creates the
// "half the Firestore tests silently skip" failure mode that hid the
// ReporterID drop bug).
//
// It never t.Skip's on a missing env var: the Firestore tests must
// always run so a missing backend fails loudly instead of passing as a
// no-op (that silent skip is exactly what let the ReporterID-drop bug
// through — issue #189). Resolution:
//   - TEST_FIRESTORE_PROJECT_ID set → real Firestore (e.g. `zenv task
//     test`); FIRESTORE_EMULATOR_HOST is left untouched.
//   - otherwise → default to a local emulator at 127.0.0.1:28615 (project
//     "test-project", database "(default)"). If FIRESTORE_EMULATOR_HOST
//     is already set (CI, `task test:firestore`) it wins; only when it is
//     unset do we default it so a bare `go test ./...` targets a local
//     emulator and fails with a connection error when none is running.
func newFirestoreRepository(t *testing.T) interfaces.Repository {
	t.Helper()

	projectID := os.Getenv("TEST_FIRESTORE_PROJECT_ID")
	databaseID := os.Getenv("TEST_FIRESTORE_DATABASE_ID")
	if databaseID == "" {
		databaseID = "(default)"
	}
	if projectID == "" {
		projectID = "test-project"
		// os.Setenv (not t.Setenv) because these tests use t.Parallel();
		// the value is identical for every caller so the repeated set is
		// idempotent, and os's env functions are safe for concurrent use.
		if _, ok := os.LookupEnv("FIRESTORE_EMULATOR_HOST"); !ok {
			// 28615 (not the emulator's stock 8080) because 8080 is the
			// single most contended dev port — the app server itself
			// defaults to :8080, so a bare `go test ./...` would silently
			// talk to whatever happens to be listening there.
			gt.NoError(t, os.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:28615")).Required()
		}
	}

	repo, err := firestore.New(context.Background(), projectID, databaseID)
	gt.NoError(t, err).Required()
	t.Cleanup(func() {
		gt.NoError(t, repo.Close())
	})
	return repo
}

// toStringSlice coerces a FieldValue.Value loaded from either backend into
// a `[]string` for assertion. Memory stores the original `[]string`;
// Firestore round-trips through `DataTo` and returns `[]interface{}`. Both
// shapes are part of the FieldValue.Value contract — see
// model.FieldValue.IsValueInSet which switches on both — so tests
// asserting on multi-select values must normalize first.
func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		out := make([]string, len(s))
		for i, elem := range s {
			str, ok := elem.(string)
			gt.Bool(t, ok).True()
			out[i] = str
		}
		return out
	default:
		t.Fatalf("FieldValue.Value is not a string slice: %T", v)
		return nil
	}
}
