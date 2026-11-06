package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	goslack "github.com/slack-go/slack"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slackpost"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	jobagent "github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/job"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
)

const testWorkspaceID = "test-ws"

func TestCaseUseCase_CreateCase(t *testing.T) {
	t.Run("create case with valid fields", func(t *testing.T) {
		repo := memory.New()
		fieldSchema := &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{
					ID:       "priority",
					Name:     "Priority",
					Type:     types.FieldTypeSelect,
					Required: true,
					Options: []config.FieldOption{
						{ID: "high", Name: "High"},
						{ID: "low", Name: "Low"},
					},
				},
			},
			Labels: config.EntityLabels{
				Case: "Case",
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: fieldSchema,
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
		seedSlackUsers(t, repo, "U001")

		fieldValues := map[string]model.FieldValue{
			"priority": {FieldID: "priority", Value: "high"},
		}

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"U001"}, fieldValues, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.Number(t, created.ID).NotEqual(0)
		gt.Value(t, created.Title).Equal("Test Case")
		gt.Value(t, created.Description).Equal("Description")

		// Verify field values are embedded in the case
		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(retrieved.FieldValues)).Equal(1)
		gt.Value(t, retrieved.FieldValues["priority"].Value).Equal("high")
		gt.Value(t, retrieved.FieldValues["priority"].Type).Equal(types.FieldTypeSelect)
	})

	t.Run("create case without title fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "", "Description", []string{}, nil, false, false, "", "")
		gt.Value(t, err).NotNil()
	})

	t.Run("create case with invalid field fails", func(t *testing.T) {
		repo := memory.New()
		fieldSchema := &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{
					ID:       "priority",
					Name:     "Priority",
					Type:     types.FieldTypeSelect,
					Required: true,
					Options: []config.FieldOption{
						{ID: "high", Name: "High"},
					},
				},
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: fieldSchema,
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		fieldValues := map[string]model.FieldValue{
			"priority": {FieldID: "priority", Value: "invalid"},
		}

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, fieldValues, false, false, "", "")
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})

	t.Run("create case with missing required field fails", func(t *testing.T) {
		repo := memory.New()
		fieldSchema := &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{
					ID:       "priority",
					Name:     "Priority",
					Type:     types.FieldTypeText,
					Required: true,
				},
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: fieldSchema,
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})
}

func TestCaseUseCase_UpdateCase_Drafts(t *testing.T) {
	t.Run("teammate can edit a public draft", func(t *testing.T) {
		// Public drafts are workspace-shared, so any teammate can iterate
		// on them via UpdateCase — including clearing the title back to
		// empty (the in-progress state is the whole point of DRAFT).
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Half written", "old body", nil, nil, false, false)
		gt.NoError(t, err).Required()

		peerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UPEER"})
		var patch usecase.CaseUpdate
		empty := ""
		newBody := "rewritten by teammate"
		patch.Title = &empty
		patch.Description = &newBody

		updated, err := uc.UpdateCase(peerCtx, testWorkspaceID, draft.ID, patch)
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Title).Equal("")
		gt.Value(t, updated.Description).Equal("rewritten by teammate")
		gt.Value(t, updated.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("non-reporter cannot edit a private draft", func(t *testing.T) {
		// Private drafts stay reporter-only — the ChannelUserIDs check
		// can't help yet (no channel), so UpdateCase falls back to the
		// reporter ID directly.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Hidden", "", nil, nil, true, false)
		gt.NoError(t, err).Required()

		peerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UPEER"})
		var patch usecase.CaseUpdate
		newTitle := "stolen"
		patch.Title = &newTitle
		_, err = uc.UpdateCase(peerCtx, testWorkspaceID, draft.ID, patch)
		gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	})

	t.Run("OPEN case still rejects empty title", func(t *testing.T) {
		// The empty-title gate only relaxes for drafts; once promoted to
		// OPEN, blanking the title would break Slack channel naming and
		// the listing UI.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		open, err := uc.CreateCase(ctx, testWorkspaceID, "Live", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		var patch usecase.CaseUpdate
		empty := ""
		patch.Title = &empty
		_, err = uc.UpdateCase(ctx, testWorkspaceID, open.ID, patch)
		gt.Error(t, err)
	})
}

func TestCaseUseCase_UpdateCase(t *testing.T) {
	t.Run("update case with valid fields", func(t *testing.T) {
		repo := memory.New()
		fieldSchema := &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{
					ID:       "priority",
					Name:     "Priority",
					Type:     types.FieldTypeSelect,
					Required: true,
					Options: []config.FieldOption{
						{ID: "high", Name: "High"},
						{ID: "low", Name: "Low"},
					},
				},
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: fieldSchema,
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
		seedSlackUsers(t, repo, "U001", "U002")

		// Create case first
		fieldValues := map[string]model.FieldValue{
			"priority": {FieldID: "priority", Value: "high"},
		}
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Original Title", "Original Description", []string{"U001"}, fieldValues, false, false, "", "")
		gt.NoError(t, err).Required()

		// Update case
		updatedFieldValues := map[string]model.FieldValue{
			"priority": {FieldID: "priority", Value: "low"},
		}
		updatedTitle := "Updated Title"
		updatedDesc := "Updated Description"
		patch := usecase.CaseUpdate{Title: &updatedTitle, Description: &updatedDesc, Fields: updatedFieldValues}
		updated, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, patch)
		gt.NoError(t, err).Required()

		gt.Value(t, updated.Title).Equal("Updated Title")
		gt.Value(t, updated.Description).Equal("Updated Description")

		// Verify field values were updated
		retrieved, err := uc.GetCase(ctx, testWorkspaceID, updated.ID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(retrieved.FieldValues)).Equal(1)
		gt.Value(t, retrieved.FieldValues["priority"].Value).Equal("low")
	})

	t.Run("partial update preserves untouched title, description, assignees and merges fields", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UORIG"})
		seedSlackUsers(t, repo, "U001", "U002", "U003")

		original := map[string]model.FieldValue{
			"stage":    {FieldID: "stage", Value: "screen"},
			"priority": {FieldID: "priority", Value: "low"},
		}
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Original", "OrigDesc", []string{"U001"}, original, false, false, "", "")
		gt.NoError(t, err).Required()

		// Empty patch: nothing should change.
		untouched, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{})
		gt.NoError(t, err).Required()
		gt.String(t, untouched.Title).Equal("Original")
		gt.String(t, untouched.Description).Equal("OrigDesc")
		gt.Array(t, untouched.AssigneeIDs).Length(1)
		gt.Value(t, untouched.FieldValues["stage"].Value).Equal("screen")
		gt.Value(t, untouched.FieldValues["priority"].Value).Equal("low")

		// Update only one field — others (including required ones, if any)
		// must not be touched and must not be re-validated.
		updated, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"priority": {FieldID: "priority", Value: "high"},
			},
		})
		gt.NoError(t, err).Required()
		gt.String(t, updated.Title).Equal("Original")
		gt.String(t, updated.Description).Equal("OrigDesc")
		gt.Array(t, updated.AssigneeIDs).Length(1)
		gt.Value(t, updated.FieldValues["stage"].Value).Equal("screen")
		gt.Value(t, updated.FieldValues["priority"].Value).Equal("high")

		// Update assignees only — title/description/fields preserved.
		// Assignees are now mutated exclusively via AssignCase (delta add).
		assignUpd, err := uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{"U002", "U003"})
		gt.NoError(t, err).Required()
		gt.String(t, assignUpd.Title).Equal("Original")
		gt.Array(t, assignUpd.AssigneeIDs).Length(3) // U001 (existing) + U002 + U003
		gt.Value(t, assignUpd.FieldValues["stage"].Value).Equal("screen")

		// Empty title is rejected.
		empty := ""
		_, err = uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{Title: &empty})
		gt.Value(t, err).NotNil()
	})

	t.Run("update non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		title := "Title"
		desc := "Description"
		_, err := uc.UpdateCase(ctx, testWorkspaceID, 999, usecase.CaseUpdate{Title: &title, Description: &desc})
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})
}

func TestCaseUseCase_IsTestFlag(t *testing.T) {
	t.Run("CreateCase persists the IsTest flag", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
		seedSlackUsers(t, repo, "U001")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test flagged", "Desc", []string{"U001"}, nil, false, true, "", "")
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsTest).True()

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, retrieved.IsTest).True()
	})

	t.Run("CreateCase without the flag defaults to false", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
		seedSlackUsers(t, repo, "U001")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Plain", "Desc", []string{"U001"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsTest).False()
	})

	t.Run("CreateDraft persists the IsTest flag", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
		seedSlackUsers(t, repo, "U001")

		created, err := uc.CreateDraft(ctx, testWorkspaceID, "Draft", "Desc", []string{"U001"}, nil, false, true)
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsTest).True()

		retrieved, err := uc.GetDraft(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, retrieved.IsTest).True()
	})

	t.Run("UpdateCase toggles IsTest only when the patch sets it", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
		seedSlackUsers(t, repo, "U001")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Toggle me", "Desc", []string{"U001"}, nil, false, true, "", "")
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsTest).True()

		// A patch with IsTest == nil must preserve the stored true value.
		preserved, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{})
		gt.NoError(t, err).Required()
		gt.Bool(t, preserved.IsTest).True()

		// A patch that sets IsTest=false must flip it off.
		off := false
		cleared, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{IsTest: &off})
		gt.NoError(t, err).Required()
		gt.Bool(t, cleared.IsTest).False()

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, retrieved.IsTest).False()

		// And back on.
		on := true
		reEnabled, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{IsTest: &on})
		gt.NoError(t, err).Required()
		gt.Bool(t, reEnabled.IsTest).True()
	})
}

// TestCaseUseCase_PrivateCaseThreadModeGuard verifies that isPrivate=true is
// rejected for thread-mode workspaces at the shared create chokepoint
// (persistCase), while it stays allowed for channel-mode workspaces. Private
// cases only make sense in channel mode (their sole effect is a dedicated
// private Slack channel); thread-mode cases reuse the monitored channel and
// have no equivalent, so the combination is a domain-invariant violation.
func TestCaseUseCase_PrivateCaseThreadModeGuard(t *testing.T) {
	newThreadRegistry := func() *model.WorkspaceRegistry {
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:             model.Workspace{ID: testWorkspaceID, Name: "Thread WS"},
			CaseMode:              model.CaseModeThread,
			SlackMonitorChannelID: "C-MONITOR",
		})
		return registry
	}
	newChannelRegistry := func() *model.WorkspaceRegistry {
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Channel WS"},
			CaseMode:  model.CaseModeChannel,
		})
		return registry
	}

	t.Run("CreateCase rejects private in thread mode", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, newThreadRegistry(), nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Private thread case", "body", nil, nil, true, false, "", "")
		gt.Error(t, err).Is(usecase.ErrCasePrivateThreadModeUnsupported)
		gt.Value(t, created).Nil()

		// The guard fires before any persistence, so no case was written.
		list, err := repo.Case().List(ctx, testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Array(t, list).Length(0)
	})

	t.Run("CreateDraft rejects private in thread mode", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, newThreadRegistry(), nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		created, err := uc.CreateDraft(ctx, testWorkspaceID, "Private thread draft", "body", nil, nil, true, false)
		gt.Error(t, err).Is(usecase.ErrCasePrivateThreadModeUnsupported)
		gt.Value(t, created).Nil()

		// The guard fires before any persistence, so no draft was written.
		drafts, err := repo.Case().ListDrafts(ctx, testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Array(t, drafts).Length(0)
	})

	t.Run("CreateDraft allows non-private in thread mode", func(t *testing.T) {
		// The guard rejects ONLY the private+thread combination — a normal
		// thread-mode draft must still go through.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, newThreadRegistry(), nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		created, err := uc.CreateDraft(ctx, testWorkspaceID, "Plain thread draft", "body", nil, nil, false, false)
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsPrivate).False()
		gt.Value(t, created.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("CreateDraft allows private in channel mode", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, newChannelRegistry(), nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		created, err := uc.CreateDraft(ctx, testWorkspaceID, "Private channel draft", "body", nil, nil, true, false)
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsPrivate).True()

		// Read back through the repository to confirm IsPrivate persisted.
		stored, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, stored.IsPrivate).True()
	})

	t.Run("private request fails closed when workspace mode is unknown", func(t *testing.T) {
		// A registry that is present but does not know the workspace must NOT
		// be treated as channel mode: "cannot determine the mode" is propagated
		// as an error, never silently allowed. Otherwise a misconfigured /
		// unknown workspace id would be a fail-open hole in the invariant.
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "some-other-ws", Name: "Other WS"},
			CaseMode:  model.CaseModeChannel,
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		created, err := uc.CreateDraft(ctx, "unregistered-ws", "Private draft", "body", nil, nil, true, false)
		gt.Error(t, err).Is(model.ErrWorkspaceNotFound)
		gt.Value(t, created).Nil()

		drafts, err := repo.Case().ListDrafts(ctx, "unregistered-ws")
		gt.NoError(t, err).Required()
		gt.Array(t, drafts).Length(0)
	})
}

// TestCaseUseCase_UpdateCase_PreservesUnpatchedFields is a regression guard for a
// production bug: UpdateCase rebuilt the Case via a field-by-field &model.Case{...}
// literal (PUT semantics) that omitted SlackThreadTS, BoardStatus, RequestKey,
// AgentAdditionalPrompt and AgentSourceIDs. Any edit therefore blanked those
// fields — a thread-mode Case lost its SlackThreadTS, so GetBySlackThread stopped
// finding it, the next mention created a DUPLICATE Case, and the on-closed Job
// posted to the channel root instead of the thread. UpdateCase now applies the
// patch onto the loaded Case (PATCH), so every field the caller did not touch
// must survive the write.
func TestCaseUseCase_UpdateCase_PreservesUnpatchedFields(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:             model.Workspace{ID: "support"},
		CaseMode:              model.CaseModeThread,
		SlackMonitorChannelID: "C-MONITOR",
	})
	uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	ctx := context.Background()

	threadTS := "1783560454.200959"
	created, err := repo.Case().Create(ctx, "support", &model.Case{
		Title:                 "Risk review",
		Description:           "original description",
		Status:                types.CaseStatusOpen,
		ReporterID:            "U014DL12P0U",
		AssigneeIDs:           []string{"U-A", "U-B"},
		SlackChannelID:        "C-MONITOR",
		SlackThreadTS:         threadTS,
		BoardStatus:           "in_review",
		RequestKey:            "req-key-xyz",
		AgentAdditionalPrompt: "focus on impersonation risk",
		AgentSourceIDs:        []model.SourceID{"src-1"},
		FieldValues:           map[string]model.FieldValue{},
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	})
	gt.NoError(t, err).Required()

	// A routine edit that only touches the description.
	newDesc := "edited description"
	updated, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{Description: &newDesc})
	gt.NoError(t, err).Required()

	// The patched field changed.
	gt.Value(t, updated.Description).Equal("edited description")
	// Every field the caller did NOT touch is preserved (these were dropped by
	// the old rebuild).
	gt.Value(t, updated.SlackThreadTS).Equal(threadTS)
	gt.Value(t, updated.BoardStatus).Equal("in_review")
	gt.Value(t, updated.RequestKey).Equal("req-key-xyz")
	gt.Value(t, updated.AgentAdditionalPrompt).Equal("focus on impersonation risk")
	gt.Value(t, updated.AgentSourceIDs).Equal([]model.SourceID{"src-1"})
	gt.Value(t, updated.ReporterID).Equal("U014DL12P0U")
	gt.Value(t, updated.SlackChannelID).Equal("C-MONITOR")
	gt.Value(t, updated.AssigneeIDs).Equal([]string{"U-A", "U-B"})
	gt.Value(t, updated.Title).Equal("Risk review")
	gt.Value(t, updated.Status).Equal(types.CaseStatusOpen)

	// The actual production symptom: after an edit the Case must still be
	// discoverable by its thread, so a follow-up mention resolves to it instead
	// of creating a duplicate.
	byThread, err := repo.Case().GetBySlackThread(ctx, "support", "C-MONITOR", threadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, byThread).NotNil().Required()
	gt.Value(t, byThread.ID).Equal(created.ID)
	gt.Value(t, byThread.SlackThreadTS).Equal(threadTS)
	gt.Value(t, byThread.Description).Equal("edited description")
}

func TestCaseUseCase_UpdateCase_TextSemantic(t *testing.T) {
	setup := func(t *testing.T) (*memory.Memory, *usecase.CaseUseCase, *model.Case) {
		t.Helper()
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "support"},
			CaseMode:  model.CaseModeChannel,
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{ID: "channel", Name: "Channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
					{ID: "note", Name: "Note", Type: types.FieldTypeText},
				},
			},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		created, err := repo.Case().Create(context.Background(), "support", &model.Case{
			Title:      "Case",
			Status:     types.CaseStatusOpen,
			ReporterID: "U-REP",
			// A value stored before the semantic was configured.
			FieldValues: map[string]model.FieldValue{
				"channel": {FieldID: "channel", Type: types.FieldTypeText, Value: "#legacy"},
			},
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		})
		gt.NoError(t, err).Required()
		return repo, uc, created
	}
	ctx := context.Background()

	t.Run("a channel name is rejected and nothing is written", func(t *testing.T) {
		repo, uc, created := setup(t)
		_, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{"channel": {FieldID: "channel", Value: "#general"}},
		})
		// UpdateCase accumulates violations into ErrCaseFieldValidation; the
		// rule reaches the caller (and an agent) through the message.
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
		gt.String(t, err.Error()).Contains(`Slack channel ID must be "C"`)

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.FieldValues["channel"].Value).Equal("#legacy")
	})

	t.Run("a channel ID is stored", func(t *testing.T) {
		repo, uc, created := setup(t)
		_, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{"channel": {FieldID: "channel", Value: "C0123ABCD"}},
		})
		gt.NoError(t, err).Required()

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.FieldValues["channel"].Value).Equal("C0123ABCD")
		gt.Value(t, reloaded.FieldValues["channel"].Type).Equal(types.FieldTypeText)
	})

	t.Run("updating another field leaves a legacy value untouched", func(t *testing.T) {
		repo, uc, created := setup(t)
		_, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{"note": {FieldID: "note", Value: "hello"}},
		})
		gt.NoError(t, err).Required()

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.FieldValues["note"].Value).Equal("hello")
		gt.Value(t, reloaded.FieldValues["channel"].Value).Equal("#legacy")
	})
}

func TestCaseUseCase_UpdateCase_TextPattern(t *testing.T) {
	setup := func(t *testing.T) (*memory.Memory, *usecase.CaseUseCase, *model.Case) {
		t.Helper()
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "support"},
			CaseMode:  model.CaseModeChannel,
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{ID: "ticket_id", Name: "Ticket ID", Type: types.FieldTypeText,
						Validation: config.FieldValidation{Pattern: "[A-Z]{2,5}-[0-9]+"}},
					{ID: "note", Name: "Note", Type: types.FieldTypeText},
				},
			},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		created, err := repo.Case().Create(context.Background(), "support", &model.Case{
			Title:      "Case",
			Status:     types.CaseStatusOpen,
			ReporterID: "U-REP",
			// A value stored before the pattern was configured.
			FieldValues: map[string]model.FieldValue{
				"ticket_id": {FieldID: "ticket_id", Type: types.FieldTypeText, Value: "legacy-1"},
			},
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		})
		gt.NoError(t, err).Required()
		return repo, uc, created
	}
	ctx := context.Background()

	t.Run("a mismatching value is rejected and nothing is written", func(t *testing.T) {
		repo, uc, created := setup(t)
		_, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{"ticket_id": {FieldID: "ticket_id", Value: "sec-1234"}},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
		gt.String(t, err.Error()).Contains(`field "ticket_id": text value must match the pattern [A-Z]{2,5}-[0-9]+`)

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.FieldValues["ticket_id"].Value).Equal("legacy-1")
	})

	t.Run("a matching value is stored", func(t *testing.T) {
		repo, uc, created := setup(t)
		_, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{"ticket_id": {FieldID: "ticket_id", Value: "SEC-1234"}},
		})
		gt.NoError(t, err).Required()

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.FieldValues["ticket_id"].Value).Equal("SEC-1234")
	})

	t.Run("updating another field leaves a legacy value untouched", func(t *testing.T) {
		repo, uc, created := setup(t)
		_, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{"note": {FieldID: "note", Value: "hello"}},
		})
		gt.NoError(t, err).Required()

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.FieldValues["note"].Value).Equal("hello")
		gt.Value(t, reloaded.FieldValues["ticket_id"].Value).Equal("legacy-1")
	})
}

// TestCaseUseCase_UpdateCase_NoRenameWhenFieldValidationFails guards the update
// ordering: the Slack channel rename is an external side effect that cannot be
// rolled back, so it must happen only after EVERY validation has passed. When a
// title change and an invalid field patch arrive together, the field validation
// must fail the whole update WITHOUT having renamed the channel — otherwise the
// channel name and the persisted title desync.
func TestCaseUseCase_UpdateCase_NoRenameWhenFieldValidationFails(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "support"},
		CaseMode:  model.CaseModeChannel,
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect, Options: []config.FieldOption{{ID: "high", Name: "High"}, {ID: "low", Name: "Low"}}},
			},
		},
	})

	renamed := false
	slackMock := &mockSlackService{
		renameChannelFn: func(_ context.Context, _ string, _ int64, _ string, _ string) error {
			renamed = true
			return nil
		},
	}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
	ctx := context.Background()

	// Channel-mode Case: it owns a dedicated Slack channel, so a title change
	// WOULD rename it — which is exactly what must be blocked when the same
	// update carries an invalid field patch.
	created, err := repo.Case().Create(ctx, "support", &model.Case{
		Title:          "Original title",
		Status:         types.CaseStatusOpen,
		ReporterID:     "U-REP",
		SlackChannelID: "C-DEDICATED",
		FieldValues:    map[string]model.FieldValue{},
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})
	gt.NoError(t, err).Required()

	// Title changes (would trigger a rename) AND fields are invalid ("critical"
	// is not an allowed severity option). The update must fail on validation.
	newTitle := "Renamed title"
	_, err = uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{
		Title:  &newTitle,
		Fields: map[string]model.FieldValue{"severity": {FieldID: "severity", Value: "critical"}},
	})
	gt.Error(t, err)

	// The channel was NOT renamed, and the persisted title is unchanged.
	gt.Bool(t, renamed).False()
	reloaded, err := repo.Case().Get(ctx, "support", created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, reloaded.Title).Equal("Original title")
}

// TestCaseUseCase_UpdateCase_SlackChannelRenameByMode guards the channel-rename
// side effect against Case mode. A thread-mode Case has no dedicated channel:
// its SlackChannelID points at the shared monitored channel that hosts many
// threads, so renaming it on a title change would rename that shared channel
// out from under every other thread (and fails in production with
// not_authorized — see Sentry ARGUS-82). Only channel-mode Cases, which own a
// dedicated channel, may be renamed.
func TestCaseUseCase_UpdateCase_SlackChannelRenameByMode(t *testing.T) {
	newTitle := "New title"

	t.Run("thread-mode title change does NOT rename the shared channel", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		set, err := model.NewActionStatusSet("triage", []string{"done"}, []model.ActionStatusDefinition{
			{ID: "triage", Name: "Triage"},
			{ID: "done", Name: "Done"},
		})
		gt.NoError(t, err).Required()
		registry.Register(&model.WorkspaceEntry{
			Workspace:             model.Workspace{ID: "support"},
			CaseMode:              model.CaseModeThread,
			SlackMonitorChannelID: "C-MONITOR",
			CaseStatusSet:         set,
		})

		renameCalls := 0
		slackMock := &mockSlackService{
			renameChannelFn: func(_ context.Context, _ string, _ int64, _ string, _ string) error {
				renameCalls++
				return nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, "support", &model.Case{
			Title:          "Original title",
			Status:         types.CaseStatusOpen,
			ReporterID:     "U-REP",
			SlackChannelID: "C-MONITOR",
			SlackThreadTS:  "1783560454.200959",
			BoardStatus:    "triage",
			FieldValues:    map[string]model.FieldValue{},
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		updated, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{Title: &newTitle})
		gt.NoError(t, err).Required()

		// No rename, but the title is still updated on the Case itself, and the
		// thread binding is preserved.
		gt.Number(t, renameCalls).Equal(0)
		gt.Value(t, updated.Title).Equal(newTitle)
		gt.Value(t, updated.SlackThreadTS).Equal("1783560454.200959")

		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.Title).Equal(newTitle)
	})

	t.Run("channel-mode title change renames the dedicated channel", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "support"},
			CaseMode:  model.CaseModeChannel,
		})

		var gotChannelID, gotTitle string
		renameCalls := 0
		slackMock := &mockSlackService{
			renameChannelFn: func(_ context.Context, channelID string, _ int64, title string, _ string) error {
				renameCalls++
				gotChannelID = channelID
				gotTitle = title
				return nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, "support", &model.Case{
			Title:          "Original title",
			Status:         types.CaseStatusOpen,
			ReporterID:     "U-REP",
			SlackChannelID: "C-DEDICATED",
			FieldValues:    map[string]model.FieldValue{},
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		updated, err := uc.UpdateCase(ctx, "support", created.ID, usecase.CaseUpdate{Title: &newTitle})
		gt.NoError(t, err).Required()

		gt.Number(t, renameCalls).Equal(1)
		gt.Value(t, gotChannelID).Equal("C-DEDICATED")
		gt.Value(t, gotTitle).Equal(newTitle)
		gt.Value(t, updated.Title).Equal(newTitle)
	})
}

func TestCaseUseCase_AssignCase(t *testing.T) {
	t.Run("adds users as a set union and persists", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		seedSlackUsers(t, repo, "UCREATOR", "U001", "U002")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", []string{"U001"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		updated, err := uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{"U002"})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U001", "U002"})

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AssigneeIDs).Equal([]string{"U001", "U002"})
	})

	t.Run("assigning an already-assigned user is idempotent", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		seedSlackUsers(t, repo, "UCREATOR", "U001")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", []string{"U001"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		updated, err := uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{"U001"})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U001"})
	})

	t.Run("assigning an empty list is a no-op returning the case unchanged", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		seedSlackUsers(t, repo, "UCREATOR", "U001")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", []string{"U001"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		updated, err := uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U001"})
	})

	t.Run("assigning an unknown user fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		seedSlackUsers(t, repo, "UCREATOR")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{"UGHOST"})
		gt.Value(t, err).NotNil()
	})

	t.Run("assigning on a non-existent case returns ErrCaseNotFound", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		_, err := uc.AssignCase(ctx, testWorkspaceID, 999, []string{"U001"})
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("assigning on a private case as non-member is denied", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		seedSlackUsers(t, repo, "UMEMBER", "USTRANGER", "U002")

		adminCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		created, err := repo.Case().Create(adminCtx, testWorkspaceID, &model.Case{
			ReporterID:     "UMEMBER",
			Title:          "Private",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
		})
		gt.NoError(t, err).Required()

		strangerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		_, err = uc.AssignCase(strangerCtx, testWorkspaceID, created.ID, []string{"U002"})
		gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	})
}

func TestCaseUseCase_UnassignCase(t *testing.T) {
	t.Run("removes a user and preserves the rest", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		seedSlackUsers(t, repo, "UCREATOR", "U001", "U002", "U003")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", []string{"U001", "U002", "U003"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		updated, err := uc.UnassignCase(ctx, testWorkspaceID, created.ID, []string{"U002"})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U001", "U003"})

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AssigneeIDs).Equal([]string{"U001", "U003"})
	})

	t.Run("unassigning an empty list is a no-op returning the case unchanged", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		seedSlackUsers(t, repo, "UCREATOR", "U001")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", []string{"U001"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		updated, err := uc.UnassignCase(ctx, testWorkspaceID, created.ID, []string{})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U001"})
	})

	t.Run("unassigning a since-deleted user still works without existence check", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		// Seed only the creator and one assignee; "UGONE" is intentionally not
		// a known Slack user yet still appears in the assignee list.
		seedSlackUsers(t, repo, "UCREATOR", "U001")
		created, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			ReporterID:  "UCREATOR",
			Title:       "Title",
			Status:      types.CaseStatusOpen,
			AssigneeIDs: []string{"U001", "UGONE"},
		})
		gt.NoError(t, err).Required()

		updated, err := uc.UnassignCase(ctx, testWorkspaceID, created.ID, []string{"UGONE"})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U001"})
	})

	t.Run("unassigning on a non-existent case returns ErrCaseNotFound", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		_, err := uc.UnassignCase(ctx, testWorkspaceID, 999, []string{"U001"})
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("unassigning on a private case as non-member is denied", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		adminCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		created, err := repo.Case().Create(adminCtx, testWorkspaceID, &model.Case{
			ReporterID:     "UMEMBER",
			Title:          "Private",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{"UMEMBER"},
		})
		gt.NoError(t, err).Required()

		strangerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		_, err = uc.UnassignCase(strangerCtx, testWorkspaceID, created.ID, []string{"UMEMBER"})
		gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	})
}

func TestCaseUseCase_DeleteCase(t *testing.T) {
	t.Run("delete case with actions", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create case
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Create action for the case
		_, err = actionUC.CreateAction(ctx, testWorkspaceID, created.ID, "Test Action", "Action Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Delete case
		gt.NoError(t, uc.DeleteCase(ctx, testWorkspaceID, created.ID)).Required()

		// Verify case is deleted
		_, err = uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.Value(t, err).NotNil()

		// Verify actions are deleted
		actions, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, created.ID, interfaces.ActionListOptions{ArchiveScope: interfaces.ActionArchiveScopeAll})
		gt.NoError(t, err).Required()
		gt.Array(t, actions).Length(0)
	})

	t.Run("delete non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		err := uc.DeleteCase(ctx, testWorkspaceID, 999)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})
}

func TestCaseUseCase_GetCase(t *testing.T) {
	t.Run("get existing case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()

		gt.Value(t, retrieved.ID).Equal(created.ID)
		gt.Value(t, retrieved.Title).Equal(created.Title)
	})

	t.Run("get non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.GetCase(ctx, testWorkspaceID, 999)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})
}

func TestCaseUseCase_GetCases(t *testing.T) {
	t.Run("returns requested cases in order, omits missing", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c1, err := uc.CreateCase(ctx, testWorkspaceID, "Case 1", "Desc 1", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		c2, err := uc.CreateCase(ctx, testWorkspaceID, "Case 2", "Desc 2", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		got, err := uc.GetCases(ctx, testWorkspaceID, []int64{c2.ID, 999999, c1.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(2)
		// Order follows the requested ids; the missing id is dropped.
		gt.Value(t, got[0].ID).Equal(c2.ID)
		gt.Value(t, got[0].Title).Equal("Case 2")
		gt.Value(t, got[1].ID).Equal(c1.ID)
		gt.Value(t, got[1].Title).Equal("Case 1")
	})

	t.Run("empty ids returns empty slice", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		got, err := uc.GetCases(context.Background(), testWorkspaceID, nil)
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(0)
	})

	t.Run("restricts private case for non-member", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		private := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, private)
		gt.NoError(t, err).Required()

		// Member gets the full case.
		memberGot, err := uc.GetCases(memberCtx, testWorkspaceID, []int64{created.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, memberGot).Length(1)
		gt.Value(t, memberGot[0].Title).Equal("Private")

		// Non-member gets a RestrictCase'd entry (title cleared, still private).
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		nonMemberGot, err := uc.GetCases(nonMemberCtx, testWorkspaceID, []int64{created.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, nonMemberGot).Length(1)
		gt.Bool(t, nonMemberGot[0].IsPrivate).True()
		gt.Value(t, nonMemberGot[0].Title).Equal("")
	})
}

func TestCaseUseCase_ListCases(t *testing.T) {
	t.Run("list cases", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create multiple cases
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Case 1", "Description 1", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.CreateCase(ctx, testWorkspaceID, "Case 2", "Description 2", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		cases, err := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()

		gt.Array(t, cases).Length(2)
	})

	t.Run("list cases with status filter", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create cases (all default to OPEN)
		case1, err := uc.CreateCase(ctx, testWorkspaceID, "Open Case 1", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.CreateCase(ctx, testWorkspaceID, "Open Case 2", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Close one case
		_, err = uc.CloseCase(ctx, testWorkspaceID, case1.ID)
		gt.NoError(t, err).Required()

		// Filter by OPEN status
		openStatus := types.CaseStatusOpen
		openCases, err := uc.ListCases(ctx, testWorkspaceID, &openStatus, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()
		gt.Array(t, openCases).Length(1)
		gt.Value(t, openCases[0].Title).Equal("Open Case 2")

		// Filter by CLOSED status
		closedStatus := types.CaseStatusClosed
		closedCases, err := uc.ListCases(ctx, testWorkspaceID, &closedStatus, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()
		gt.Array(t, closedCases).Length(1)
		gt.Value(t, closedCases[0].Title).Equal("Open Case 1")
	})

	t.Run("sorts newest-first by created date", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Persist cases out of chronological order so the result order cannot
		// accidentally match insertion order — only an explicit CreatedAt sort
		// produces newest-first.
		base := time.Now().UTC()
		seed := func(title string, createdAt time.Time) {
			_, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
				Title:      title,
				Status:     types.CaseStatusOpen,
				ReporterID: "UTESTUSER",
				CreatedAt:  createdAt,
				UpdatedAt:  createdAt,
			})
			gt.NoError(t, err).Required()
		}
		seed("middle", base.Add(-1*time.Hour))
		seed("newest", base)
		seed("oldest", base.Add(-2*time.Hour))

		cases, err := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()
		gt.Array(t, cases).Length(3)
		gt.Value(t, cases[0].Title).Equal("newest")
		gt.Value(t, cases[1].Title).Equal("middle")
		gt.Value(t, cases[2].Title).Equal("oldest")
	})
}

func TestCaseUseCase_CreateCase_DefaultStatus(t *testing.T) {
	repo := memory.New()
	uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

	created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	gt.Value(t, created.Status).Equal(types.CaseStatusOpen)

	// Verify through GetCase as well
	retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, retrieved.Status).Equal(types.CaseStatusOpen)
}

func TestCaseUseCase_CloseCase(t *testing.T) {
	t.Run("close an open case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.Status).Equal(types.CaseStatusOpen)

		closed, err := uc.CloseCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, closed.Status).Equal(types.CaseStatusClosed)
		gt.Value(t, closed.ID).Equal(created.ID)
		gt.Value(t, closed.Title).Equal("Test Case")
	})

	t.Run("close an already closed case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.CloseCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()

		_, err = uc.CloseCase(ctx, testWorkspaceID, created.ID)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseAlreadyClosed)
	})

	t.Run("close non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.CloseCase(ctx, testWorkspaceID, 999)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("close a draft case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:      "Some draft",
			Status:     types.CaseStatusDraft,
			ReporterID: "UTESTUSER",
		})
		gt.NoError(t, err).Required()

		_, err = uc.CloseCase(ctx, testWorkspaceID, created.ID)
		gt.Error(t, err).Is(usecase.ErrCaseIsDraft)

		// Status must remain DRAFT.
		got, err := repo.Case().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("closing a thread-mode case directly is rejected", func(t *testing.T) {
		// Thread-mode cases must close via UpdateCaseStatus (board status) so the
		// configurable BoardStatus and lifecycle Status stay in sync. CloseCase
		// rejects them at the boundary rather than producing the desynced state.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:          "Thread case",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C123",
			SlackThreadTS:  "1700000000.000100",
			BoardStatus:    "in_progress",
		})
		gt.NoError(t, err).Required()

		_, err = uc.CloseCase(ctx, testWorkspaceID, created.ID)
		gt.Error(t, err).Is(usecase.ErrCaseThreadModeUseStatus)

		// Status and BoardStatus must be untouched.
		got, err := repo.Case().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, got.BoardStatus).Equal("in_progress")
	})
}

func TestCaseUseCase_ReopenCase(t *testing.T) {
	t.Run("reopen a closed case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.CloseCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()

		reopened, err := uc.ReopenCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reopened.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, reopened.ID).Equal(created.ID)
		gt.Value(t, reopened.Title).Equal("Test Case")
	})

	t.Run("reopen an already open case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.ReopenCase(ctx, testWorkspaceID, created.ID)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseAlreadyOpen)
	})

	t.Run("reopen non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.ReopenCase(ctx, testWorkspaceID, 999)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("reopen a draft case fails", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:      "Some draft",
			Status:     types.CaseStatusDraft,
			ReporterID: "UTESTUSER",
		})
		gt.NoError(t, err).Required()

		_, err = uc.ReopenCase(ctx, testWorkspaceID, created.ID)
		gt.Error(t, err).Is(usecase.ErrCaseIsDraft)

		// Status must remain DRAFT.
		got, err := repo.Case().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("reopening a thread-mode case directly is rejected", func(t *testing.T) {
		// Thread-mode cases reopen by moving to a non-closed board status, not
		// through ReopenCase; rejecting it keeps BoardStatus and Status in sync.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:          "Thread case",
			Status:         types.CaseStatusClosed,
			SlackChannelID: "C123",
			SlackThreadTS:  "1700000000.000200",
			BoardStatus:    "closed",
		})
		gt.NoError(t, err).Required()

		_, err = uc.ReopenCase(ctx, testWorkspaceID, created.ID)
		gt.Error(t, err).Is(usecase.ErrCaseThreadModeUseStatus)

		// Status and BoardStatus must be untouched.
		got, err := repo.Case().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusClosed)
		gt.Value(t, got.BoardStatus).Equal("closed")
	})
}

func TestCaseUseCase_GetFieldConfiguration(t *testing.T) {
	t.Run("get field configuration with schema", func(t *testing.T) {
		repo := memory.New()
		fieldSchema := &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "priority", Name: "Priority", Type: types.FieldTypeText},
			},
			Labels: config.EntityLabels{
				Case: "Case",
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: fieldSchema,
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")

		cfg := uc.GetFieldConfiguration(testWorkspaceID)
		gt.Array(t, cfg.Fields).Length(1)
		gt.Value(t, cfg.Fields[0].ID).Equal("priority")
	})

	t.Run("get field configuration without schema", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		cfg := uc.GetFieldConfiguration(testWorkspaceID)
		gt.Array(t, cfg.Fields).Length(0)
		gt.Value(t, cfg.Labels.Case).Equal("Case")
	})
}

func TestCaseUseCase_CreateCase_SlackInvite(t *testing.T) {
	t.Run("invites creator and assignees to channel", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		seedSlackUsers(t, repo, "UASSIGNEE1", "UASSIGNEE2")

		token := auth.NewToken("UCREATOR", "creator@example.com", "Creator")
		ctx := auth.ContextWithToken(context.Background(), token)

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"UASSIGNEE1", "UASSIGNEE2"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")

		// Verify creator is first, followed by assignees
		gt.Array(t, mock.invitedUserIDs).Length(3)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UASSIGNEE1")
		gt.Value(t, mock.invitedUserIDs[2]).Equal("UASSIGNEE2")
		gt.Value(t, mock.invitedChannelID).Equal(created.SlackChannelID)
	})

	t.Run("deduplicates creator in assignees", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		seedSlackUsers(t, repo, "UCREATOR", "UOTHER")

		token := auth.NewToken("UCREATOR", "creator@example.com", "Creator")
		ctx := auth.ContextWithToken(context.Background(), token)

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"UCREATOR", "UOTHER"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// UCREATOR should appear only once
		gt.Array(t, mock.invitedUserIDs).Length(2)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UOTHER")
	})

	t.Run("invite failure does not fail case creation", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			inviteUsersToChannelFn: func(_ context.Context, _ string, _ []string) error {
				return errors.New("slack invite error")
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		seedSlackUsers(t, repo, "UASSIGNEE")

		token := auth.NewToken("UCREATOR", "creator@example.com", "Creator")
		ctx := auth.ContextWithToken(context.Background(), token)

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"UASSIGNEE"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")
	})

	t.Run("invites creator and assignees with auth token", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		seedSlackUsers(t, repo, "UASSIGNEE")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"UASSIGNEE"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Creator and assignee should be invited
		gt.Array(t, mock.invitedUserIDs).Length(2)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UTESTUSER")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UASSIGNEE")
	})

	t.Run("invites only creator when no assignees", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Only creator should be invited
		gt.Array(t, mock.invitedUserIDs).Length(1)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UTESTUSER")
	})
}

func TestCaseUseCase_CreateCase_BookmarkAndMapping(t *testing.T) {
	t.Run("adds bookmark and saves mapping when baseURL is set", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		i18n.Init(i18n.LangEN)
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "https://example.com")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Verify bookmark was added
		gt.Value(t, mock.bookmarkChannelID).Equal(created.SlackChannelID)
		gt.Value(t, mock.bookmarkTitle).Equal("Open Case")
		expectedURL := fmt.Sprintf("https://example.com/ws/%s/cases/%d", testWorkspaceID, created.ID)
		gt.Value(t, mock.bookmarkLink).Equal(expectedURL)
	})

	t.Run("skips bookmark when baseURL is empty", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Bookmark should not have been added
		gt.Value(t, mock.bookmarkChannelID).Equal("")
	})

	t.Run("bookmark failure does not fail case creation", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			addBookmarkFn: func(_ context.Context, _, _, _ string) error {
				return errors.New("bookmark error")
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "https://example.com")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")
	})

}

func TestCaseUseCase_CreateCase_WelcomeMessages(t *testing.T) {
	newRegistry := func(messages []string) *model.WorkspaceRegistry {
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:            model.Workspace{ID: testWorkspaceID, Name: "Test"},
			SlackWelcomeMessages: messages,
		})
		return registry
	}

	t.Run("posts rendered messages in declared order", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		registry := newRegistry([]string{
			"Hello {{.Case.Title}}",
			"Reporter: <@{{.Case.ReporterID}}>",
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Phishing", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.Array(t, mock.postedTexts).Length(2).Required()
		gt.Value(t, mock.postedTexts[0]).Equal("Hello Phishing")
		gt.Value(t, mock.postedTexts[1]).Equal("Reporter: <@UCREATOR>")
		gt.Value(t, mock.postedChannelIDs[0]).Equal(created.SlackChannelID)
		gt.Value(t, mock.postedChannelIDs[1]).Equal(created.SlackChannelID)
	})

	t.Run("template can reference custom Fields by ID and Name", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test"},
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{
						ID:   "severity",
						Name: "Severity",
						Type: types.FieldTypeSelect,
						Options: []config.FieldOption{
							{ID: "high", Name: "High"},
							{ID: "low", Name: "Low"},
						},
					},
				},
			},
			SlackWelcomeMessages: []string{
				"Severity: {{.Fields.severity.name}} ({{.Fields.severity.id}})",
			},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		fieldValues := map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
		}
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "desc", []string{}, fieldValues, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.Array(t, mock.postedTexts).Length(1).Required()
		gt.Value(t, mock.postedTexts[0]).Equal("Severity: High (high)")
	})

	t.Run("URL is exposed when baseURL is set", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		registry := newRegistry([]string{
			"Detail: {{.URL}}",
		})
		i18n.Init(i18n.LangEN)
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "https://example.com")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		expectedURL := fmt.Sprintf("https://example.com/ws/%s/cases/%d", testWorkspaceID, created.ID)
		gt.Array(t, mock.postedTexts).Length(1).Required()
		gt.Value(t, mock.postedTexts[0]).Equal("Detail: " + expectedURL)
	})

	t.Run("send failure does not abort case creation", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			postMessageFn: func(_ context.Context, _ string, _ []goslack.Block, _ string) (string, error) {
				return "", errors.New("post failed")
			},
		}
		registry := newRegistry([]string{
			"Hello",
			"World",
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")
		// Both messages were attempted even though the first failed.
		gt.Array(t, mock.postedTexts).Length(2)
	})

	t.Run("workspace without messages posts nothing", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Array(t, mock.postedTexts).Length(0)
	})

	t.Run("private case still receives welcome messages", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		registry := newRegistry([]string{
			"private welcome",
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})

		_, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "desc", []string{}, nil, true, false, "", "")
		gt.NoError(t, err).Required()
		gt.Array(t, mock.postedTexts).Length(1)
		gt.Value(t, mock.postedTexts[0]).Equal("private welcome")
	})
}

func TestCaseUseCase_PrivateCaseAccessControl(t *testing.T) {
	t.Run("create private case sets IsPrivate flag", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Private Case", "Secret", []string{}, nil, true, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.IsPrivate).Equal(true)
	})

	t.Run("get private case as member returns full case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		// Create case directly in repo with IsPrivate and ChannelUserIDs
		caseModel := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret desc",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER", "UOTHER"},
		}
		created, err := repo.Case().Create(ctx, testWorkspaceID, caseModel)
		gt.NoError(t, err).Required()

		// Get as member
		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.Title).Equal("Private Case")
		gt.Value(t, retrieved.Description).Equal("Secret desc")
		gt.Value(t, retrieved.AccessDenied).Equal(false)
	})

	t.Run("get private case as non-member returns restricted case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		// Create case as a different user
		adminCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UADMIN"})
		caseModel := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret desc",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UADMIN"},
		}
		created, err := repo.Case().Create(adminCtx, testWorkspaceID, caseModel)
		gt.NoError(t, err).Required()

		// Get as non-member
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		retrieved, err := uc.GetCase(nonMemberCtx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AccessDenied).Equal(true)
		gt.Value(t, retrieved.Title).Equal("")
		gt.Value(t, retrieved.Description).Equal("")
		gt.Value(t, retrieved.IsPrivate).Equal(true)
		gt.Value(t, retrieved.ID).Equal(created.ID)
	})

	t.Run("list cases restricts private cases for non-members", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		// Create public case
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Public Case", "Visible", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Create private case with UMEMBER as member
		privCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Visible",
			Description:    "Visible to member",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
		}
		_, err = repo.Case().Create(ctx, testWorkspaceID, privCase)
		gt.NoError(t, err).Required()

		// Create private case without UMEMBER
		privCase2 := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Hidden",
			Description:    "Not visible",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UOTHER"},
		}
		_, err = repo.Case().Create(ctx, testWorkspaceID, privCase2)
		gt.NoError(t, err).Required()

		// List cases
		cases, err := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()
		gt.Array(t, cases).Length(3)

		// Check that private hidden case is restricted
		var restrictedCount int
		for _, c := range cases {
			if c.AccessDenied {
				restrictedCount++
				gt.Value(t, c.Title).Equal("")
			}
		}
		gt.Value(t, restrictedCount).Equal(1)
	})

	t.Run("update private case as non-member returns access denied", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		adminCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UADMIN"})
		caseModel := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UADMIN"},
		}
		created, err := repo.Case().Create(adminCtx, testWorkspaceID, caseModel)
		gt.NoError(t, err).Required()

		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		hackTitle := "Hacked"
		hackDesc := "Hacked desc"
		hackPatch := usecase.CaseUpdate{Title: &hackTitle, Description: &hackDesc}
		_, err = uc.UpdateCase(nonMemberCtx, testWorkspaceID, created.ID, hackPatch)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	})

	t.Run("delete private case as non-member returns access denied", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		adminCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UADMIN"})
		caseModel := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UADMIN"},
		}
		created, err := repo.Case().Create(adminCtx, testWorkspaceID, caseModel)
		gt.NoError(t, err).Required()

		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		err = uc.DeleteCase(nonMemberCtx, testWorkspaceID, created.ID)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	})

	t.Run("close private case as non-member returns access denied", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		adminCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UADMIN"})
		caseModel := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			Status:         types.CaseStatusOpen,
			IsPrivate:      true,
			ChannelUserIDs: []string{"UADMIN"},
		}
		created, err := repo.Case().Create(adminCtx, testWorkspaceID, caseModel)
		gt.NoError(t, err).Required()

		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		_, err = uc.CloseCase(nonMemberCtx, testWorkspaceID, created.ID)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	})

	t.Run("public case is accessible without restrictions", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UANYONE"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Public Case", "Visible", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AccessDenied).Equal(false)
		gt.Value(t, retrieved.Title).Equal("Public Case")
	})

	t.Run("backward compatibility: existing case with nil ChannelUserIDs is public", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UANYONE"})

		// Create a case that simulates existing data (IsPrivate=false, ChannelUserIDs=nil)
		caseModel := &model.Case{
			ReporterID:  "U-TEST-DEFAULT",
			Title:       "Legacy Case",
			Description: "Old case",
			Status:      types.CaseStatusOpen,
			// IsPrivate defaults to false, ChannelUserIDs defaults to nil
		}
		created, err := repo.Case().Create(ctx, testWorkspaceID, caseModel)
		gt.NoError(t, err).Required()

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.AccessDenied).Equal(false)
		gt.Value(t, retrieved.Title).Equal("Legacy Case")
	})
}

func TestCaseUseCase_SyncCaseChannelUsers(t *testing.T) {
	t.Run("sync updates channel user IDs", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Seed SlackUser cache so filterHumanUsers can identify real users
		err := repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
			{ID: "U001", Name: "user1", RealName: "User One"},
			{ID: "U002", Name: "user2", RealName: "User Two"},
			{ID: "U003", Name: "user3", RealName: "User Three"},
		})
		gt.NoError(t, err).Required()

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")

		// Override GetConversationMembers to return specific members
		mock.getConversationMembersFn = func(_ context.Context, _ string) ([]string, error) {
			return []string{"U001", "U002", "U003"}, nil
		}

		synced, err := uc.SyncCaseChannelUsers(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Array(t, synced.ChannelUserIDs).Length(3)
	})

	t.Run("sync fails when case has no slack channel", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create case without slack service (no channel)
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.SyncCaseChannelUsers(ctx, testWorkspaceID, created.ID)
		gt.Value(t, err).NotNil()
	})

	t.Run("sync fails for non-existent case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := uc.SyncCaseChannelUsers(ctx, testWorkspaceID, 999)
		gt.Value(t, err).NotNil()
	})
}

func TestCaseUseCase_CreateCase_AutoInvite(t *testing.T) {
	t.Run("auto-invite users from workspace config", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:        model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackInviteUsers: []string{"UAUTO1", "UAUTO2"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Creator + auto-invite users
		gt.Array(t, mock.invitedUserIDs).Length(3)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UAUTO1")
		gt.Value(t, mock.invitedUserIDs[2]).Equal("UAUTO2")
	})

	t.Run("auto-invite deduplicates with creator and assignees", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:        model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackInviteUsers: []string{"UCREATOR", "UASSIGNEE", "UAUTO1"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")
		seedSlackUsers(t, repo, "UASSIGNEE")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"UASSIGNEE"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// UCREATOR and UASSIGNEE should appear only once each
		gt.Array(t, mock.invitedUserIDs).Length(3)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UASSIGNEE")
		gt.Value(t, mock.invitedUserIDs[2]).Equal("UAUTO1")
	})

	t.Run("auto-invite resolves group by ID", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			getUserGroupMembersFn: func(_ context.Context, groupID string) ([]string, error) {
				if groupID == "S0001" {
					return []string{"UGROUP1", "UGROUP2"}, nil
				}
				return nil, nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:         model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackInviteGroups: []string{"S0001"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Creator + group members
		gt.Array(t, mock.invitedUserIDs).Length(3)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UGROUP1")
		gt.Value(t, mock.invitedUserIDs[2]).Equal("UGROUP2")
	})

	t.Run("auto-invite resolves group by handle name with @ prefix", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			listUserGroupsFn: func(_ context.Context) ([]slack.UserGroup, error) {
				return []slack.UserGroup{
					{ID: "S1234", Handle: "security-team", Name: "Security Team"},
					{ID: "S5678", Handle: "dev-team", Name: "Dev Team"},
				}, nil
			},
			getUserGroupMembersFn: func(_ context.Context, groupID string) ([]string, error) {
				if groupID == "S1234" {
					return []string{"USEC1", "USEC2"}, nil
				}
				return nil, nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:         model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackInviteGroups: []string{"@security-team"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Creator + resolved group members
		gt.Array(t, mock.invitedUserIDs).Length(3)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("USEC1")
		gt.Value(t, mock.invitedUserIDs[2]).Equal("USEC2")
	})

	t.Run("group resolution failure does not block case creation", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			getUserGroupMembersFn: func(_ context.Context, _ string) ([]string, error) {
				return nil, errors.New("group API error")
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:         model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackInviteGroups: []string{"S0001"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")

		// Only creator should be invited (group members failed)
		gt.Array(t, mock.invitedUserIDs).Length(1)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
	})

	t.Run("group ID without @ prefix does not call ListUserGroups", func(t *testing.T) {
		repo := memory.New()
		listUserGroupsCalled := false
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
			listUserGroupsFn: func(_ context.Context) ([]slack.UserGroup, error) {
				listUserGroupsCalled = true
				return nil, nil
			},
			getUserGroupMembersFn: func(_ context.Context, groupID string) ([]string, error) {
				return []string{"UMEMBER"}, nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:         model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackInviteGroups: []string{"S0001"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.Bool(t, listUserGroupsCalled).False()
	})

	t.Run("empty auto-invite config does not affect behavior", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, mock, nil, "")
		seedSlackUsers(t, repo, "UASSIGNEE")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{"UASSIGNEE"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.Array(t, mock.invitedUserIDs).Length(2)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UCREATOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UASSIGNEE")
	})
}

func TestCaseUseCase_ReporterID(t *testing.T) {
	t.Run("create case sets reporter from auth token", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})
		seedSlackUsers(t, repo, "UASSIGNEE")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Reporter Test", "desc", []string{"UASSIGNEE"}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.String(t, created.ReporterID).Equal("UREPORTER")
	})

	t.Run("create case without auth token is rejected", func(t *testing.T) {
		// Before the persistence-boundary validation was added, the
		// usecase silently persisted an unattributable case when the
		// caller had no auth.Token in context (Slack interactivity
		// handler forgot to inject, NoAuthn mode misconfigured, etc).
		// Now Case.Validate refuses the write — callers must arrange
		// for an auth-context Token to exist before reaching CreateCase.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := context.Background()

		_, err := uc.CreateCase(ctx, testWorkspaceID, "No Reporter", "desc", []string{}, nil, false, false, "", "")
		gt.Error(t, err).Is(model.ErrCaseMissingReporter)
	})

	t.Run("update case preserves reporter", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})
		seedSlackUsers(t, repo, "UOTHER")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Reporter Preserved", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.String(t, created.ReporterID).Equal("UREPORTER")

		// Update with a different user context
		ctxOther := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		ut := "Updated Title"
		ud := "new desc"
		repPatch := usecase.CaseUpdate{Title: &ut, Description: &ud}
		updated, err := uc.UpdateCase(ctxOther, testWorkspaceID, created.ID, repPatch)
		gt.NoError(t, err).Required()
		gt.String(t, updated.ReporterID).Equal("UREPORTER") // Reporter should NOT change
	})

	t.Run("reporter is persisted and retrievable", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Persisted Reporter", "desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		retrieved, err := uc.GetCase(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.String(t, retrieved.ReporterID).Equal("UREPORTER")
	})
}

// mockAdminService is a mock implementation of slack.AdminService for testing
type mockAdminService struct {
	connectFn          func(ctx context.Context, channelID string, targetTeamIDs []string) error
	connectedChannelID string
	connectedTeamIDs   []string
}

func (m *mockAdminService) ConnectChannelToWorkspace(ctx context.Context, channelID string, targetTeamIDs []string) error {
	m.connectedChannelID = channelID
	m.connectedTeamIDs = targetTeamIDs
	if m.connectFn != nil {
		return m.connectFn(ctx, channelID, targetTeamIDs)
	}
	return nil
}

func TestCaseUseCase_CreateCase_CrossWorkspaceConnect(t *testing.T) {
	t.Run("connects channel to source workspace when admin service available", func(t *testing.T) {
		repo := memory.New()
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		adminMock := &mockAdminService{}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackTeamID: "TCONFIG",
		})
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, adminMock, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Cross WS Case", "Description", []string{}, nil, false, false, "TSOURCE", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")

		// Verify ConnectChannelToWorkspace was called with both team IDs
		gt.String(t, adminMock.connectedChannelID).Equal(created.SlackChannelID)
		gt.Array(t, adminMock.connectedTeamIDs).Length(2)
		gt.Value(t, adminMock.connectedTeamIDs[0]).Equal("TCONFIG")
		gt.Value(t, adminMock.connectedTeamIDs[1]).Equal("TSOURCE")
	})

	t.Run("skips connect when source team ID matches configured team ID", func(t *testing.T) {
		repo := memory.New()
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		adminMock := &mockAdminService{}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackTeamID: "TSAME",
		})
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, adminMock, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "Same WS Case", "Description", []string{}, nil, false, false, "TSAME", "")
		gt.NoError(t, err).Required()

		// ConnectChannelToWorkspace should NOT have been called
		gt.String(t, adminMock.connectedChannelID).Equal("")
	})

	t.Run("skips connect when source team ID is empty", func(t *testing.T) {
		repo := memory.New()
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		adminMock := &mockAdminService{}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackTeamID: "TCONFIG",
		})
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, adminMock, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateCase(ctx, testWorkspaceID, "No Source Team", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.String(t, adminMock.connectedChannelID).Equal("")
	})

	t.Run("skips connect when configured team ID is empty", func(t *testing.T) {
		// Regression for ARGUS-80: a workspace with no SlackTeamID configured
		// (teamID empty) previously still entered the cross-workspace connect
		// branch, passing ["", sourceTeamID] to admin.conversations.setTeams and
		// failing with not_an_enterprise. Without a home team id the connect must
		// be skipped entirely.
		repo := memory.New()
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		adminMock := &mockAdminService{}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			// SlackTeamID intentionally left empty.
		})
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, adminMock, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Unconfigured Home Team", "Description", []string{}, nil, false, false, "TSOURCE", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")

		// ConnectChannelToWorkspace must NOT be called.
		gt.String(t, adminMock.connectedChannelID).Equal("")
		gt.Array(t, adminMock.connectedTeamIDs).Length(0)
	})

	t.Run("connect failure does not fail case creation", func(t *testing.T) {
		repo := memory.New()
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		adminMock := &mockAdminService{
			connectFn: func(_ context.Context, _ string, _ []string) error {
				return errors.New("admin.conversations.setTeams failed")
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackTeamID: "TCONFIG",
		})
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, adminMock, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Connect Fail Case", "Description", []string{}, nil, false, false, "TSOURCE", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")
	})

	t.Run("skips connect when admin service is nil", func(t *testing.T) {
		repo := memory.New()
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			SlackTeamID: "TCONFIG",
		})
		// No admin service (nil)
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")

		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "No Admin Svc", "Description", []string{}, nil, false, false, "TSOURCE", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackChannelID).NotEqual("")
	})
}

func TestCaseUseCase_CreateCase_DuplicateRequestKey(t *testing.T) {
	t.Run("duplicate request key returns existing case without creating new one", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		requestKey := "test-idempotency-key-1"

		// First call creates the case
		created, err := uc.CreateCase(ctx, testWorkspaceID, "First Case", "Description", []string{}, nil, false, false, "", requestKey)
		gt.NoError(t, err).Required()
		gt.Value(t, created.Title).Equal("First Case")
		gt.String(t, created.RequestKey).Equal(requestKey)

		// Second call with same key returns existing case
		duplicate, err := uc.CreateCase(ctx, testWorkspaceID, "Different Title", "Different Desc", []string{}, nil, false, false, "", requestKey)
		gt.NoError(t, err).Required()
		gt.Value(t, duplicate.ID).Equal(created.ID)
		gt.Value(t, duplicate.Title).Equal("First Case")

		// Verify only one case exists
		cases, err := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()
		gt.Array(t, cases).Length(1)
	})

	t.Run("empty request key skips dedup check", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create two cases with empty key — both should succeed
		case1, err := uc.CreateCase(ctx, testWorkspaceID, "Case 1", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		case2, err := uc.CreateCase(ctx, testWorkspaceID, "Case 2", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		gt.Value(t, case1.ID).NotEqual(case2.ID)
	})

	t.Run("different request keys create separate cases", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		case1, err := uc.CreateCase(ctx, testWorkspaceID, "Case A", "Desc", []string{}, nil, false, false, "", "key-a")
		gt.NoError(t, err).Required()

		case2, err := uc.CreateCase(ctx, testWorkspaceID, "Case B", "Desc", []string{}, nil, false, false, "", "key-b")
		gt.NoError(t, err).Required()

		gt.Value(t, case1.ID).NotEqual(case2.ID)
		gt.Value(t, case1.Title).Equal("Case A")
		gt.Value(t, case2.Title).Equal("Case B")
	})
}

func TestCaseUseCase_CreateDraft(t *testing.T) {
	t.Run("create draft without side effects", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})
		seedSlackUsers(t, repo, "UASSIGN")

		got, err := uc.CreateDraft(ctx, testWorkspaceID, "Half-written", "Desc", []string{"UASSIGN"}, nil, true, false)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusDraft)
		gt.Value(t, got.Title).Equal("Half-written")
		gt.Value(t, got.Description).Equal("Desc")
		gt.Value(t, got.ReporterID).Equal("UAUTHOR")
		gt.Value(t, got.IsPrivate).Equal(true)
		gt.Number(t, len(got.AssigneeIDs)).Equal(1)
		gt.Value(t, got.AssigneeIDs[0]).Equal("UASSIGN")
		gt.Value(t, got.SlackChannelID).Equal("")

		// Slack channel creation MUST NOT have fired.
		gt.String(t, mock.invitedChannelID).Equal("")
		gt.Array(t, mock.invitedUserIDs).Length(0)

		// Persisted state matches what was returned.
		stored, err := repo.Case().Get(context.Background(), testWorkspaceID, got.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusDraft)
		gt.Value(t, stored.Title).Equal("Half-written")
		gt.Value(t, stored.ReporterID).Equal("UAUTHOR")
	})

	t.Run("create draft with empty title is allowed", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})

		got, err := uc.CreateDraft(ctx, testWorkspaceID, "", "Just a body", nil, nil, false, false)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusDraft)
		gt.Value(t, got.Title).Equal("")
	})

	t.Run("create draft skips required-field check", func(t *testing.T) {
		// Draft is by definition half-finished, so required fields that the
		// regular CreateCase path would reject must NOT block Save as Draft.
		// SubmitDraft is what enforces the full schema later.
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test"},
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{
						ID:       "category",
						Name:     "Category",
						Type:     types.FieldTypeSelect,
						Required: true,
						Options: []config.FieldOption{
							{ID: "a", Name: "A"},
							{ID: "b", Name: "B"},
						},
					},
				},
			},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})

		// No "category" supplied — required field missing.
		got, err := uc.CreateDraft(ctx, testWorkspaceID, "Half", "", nil, nil, false, false)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Status).Equal(types.CaseStatusDraft)
		gt.Number(t, len(got.FieldValues)).Equal(0)
	})
}

func TestCaseUseCase_ListDrafts(t *testing.T) {
	t.Run("returns every draft in the workspace regardless of reporter", func(t *testing.T) {
		// Drafts are surfaced workspace-wide so anyone on the team can pick
		// up an in-progress entry — reporter scoping was intentionally
		// removed once the feature graduated past the original Slack-only
		// design.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		mineCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		mineA, err := uc.CreateDraft(mineCtx, testWorkspaceID, "Mine A", "", nil, nil, false, false)
		gt.NoError(t, err).Required()
		mineB, err := uc.CreateDraft(mineCtx, testWorkspaceID, "Mine B", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		theirs, err := uc.CreateDraft(otherCtx, testWorkspaceID, "Theirs", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		// Open case by UMINE — must NOT appear in drafts.
		_, err = uc.CreateCase(mineCtx, testWorkspaceID, "Submitted", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		got, err := uc.ListDrafts(mineCtx, testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(3)
		ids := map[int64]bool{}
		for _, c := range got {
			ids[c.ID] = true
			gt.Value(t, c.Status).Equal(types.CaseStatusDraft)
		}
		gt.Bool(t, ids[mineA.ID]).True()
		gt.Bool(t, ids[mineB.ID]).True()
		gt.Bool(t, ids[theirs.ID]).True()
	})

	t.Run("private drafts stay reporter-only", func(t *testing.T) {
		// A draft has no Slack channel yet, so the usual ChannelUserIDs-based
		// access control locks everyone out of private drafts. To keep the
		// reporter from losing access to their own private entry while still
		// hiding it from teammates, ListDrafts filters private drafts by
		// reporter ID directly.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOWNER"})
		privateDraft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Secret", "", nil, nil, true, false)
		gt.NoError(t, err).Required()
		publicDraft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Public", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		// The owner sees both their public and their private draft.
		mine, err := uc.ListDrafts(ownerCtx, testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(mine)).Equal(2)

		// A different teammate sees the public one but NOT the private one.
		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UPEER"})
		theirs, err := uc.ListDrafts(otherCtx, testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(theirs)).Equal(1)
		gt.Value(t, theirs[0].ID).Equal(publicDraft.ID)

		// Sanity: the private draft still exists in the repo — it's just
		// hidden from the peer at the usecase layer.
		stored, err := repo.Case().Get(context.Background(), testWorkspaceID, privateDraft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.IsPrivate).Equal(true)
	})

	t.Run("no auth token surfaces public drafts only", func(t *testing.T) {
		// Bot / system contexts still get the workspace's public drafts —
		// hiding everything would mask legitimate background work — but
		// private drafts are filtered out (no reporter to match).
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOWNER"})
		_, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Public draft", "", nil, nil, false, false)
		gt.NoError(t, err).Required()
		_, err = uc.CreateDraft(ownerCtx, testWorkspaceID, "Private draft", "", nil, nil, true, false)
		gt.NoError(t, err).Required()

		got, err := uc.ListDrafts(context.Background(), testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Number(t, len(got)).Equal(1)
		gt.Value(t, got[0].Title).Equal("Public draft")
	})

	t.Run("sorts newest-first by created date", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOWNER"})

		base := time.Now().UTC()
		seed := func(title string, createdAt time.Time) {
			_, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
				Title:      title,
				Status:     types.CaseStatusDraft,
				ReporterID: "UOWNER",
				CreatedAt:  createdAt,
				UpdatedAt:  createdAt,
			})
			gt.NoError(t, err).Required()
		}
		seed("middle", base.Add(-1*time.Hour))
		seed("newest", base)
		seed("oldest", base.Add(-2*time.Hour))

		got, err := uc.ListDrafts(ctx, testWorkspaceID)
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(3)
		gt.Value(t, got[0].Title).Equal("newest")
		gt.Value(t, got[1].Title).Equal("middle")
		gt.Value(t, got[2].Title).Equal("oldest")
	})
}

func TestCaseUseCase_GetDraft(t *testing.T) {
	t.Run("reporter retrieves their own draft", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		created, err := uc.CreateDraft(ctx, testWorkspaceID, "Mine", "Body", nil, nil, false, false)
		gt.NoError(t, err).Required()

		got, err := uc.GetDraft(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ID).Equal(created.ID)
		gt.Value(t, got.Title).Equal("Mine")
	})

	t.Run("non-reporter can read a public draft", func(t *testing.T) {
		// Public drafts surface across the workspace so any teammate can
		// review an in-progress entry. The reporter check is only enforced
		// at the mutation layer (SubmitDraft / DiscardDraft).
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		created, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Shared", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		got, err := uc.GetDraft(otherCtx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ID).Equal(created.ID)
		gt.Value(t, got.Title).Equal("Shared")
	})

	t.Run("non-reporter cannot read a private draft", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		created, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Hidden", "", nil, nil, true, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		_, err = uc.GetDraft(otherCtx, testWorkspaceID, created.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("non-draft case returns ErrCaseNotDraft", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		open, err := uc.CreateCase(ctx, testWorkspaceID, "Open Case", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.GetDraft(ctx, testWorkspaceID, open.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotDraft)
	})

	t.Run("unknown id returns ErrCaseNotFound", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		_, err := uc.GetDraft(ctx, testWorkspaceID, 999)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})
}

func TestCaseUseCase_SubmitDraft(t *testing.T) {
	t.Run("promotes draft and runs activation side effects", func(t *testing.T) {
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})
		seedSlackUsers(t, repo, "UASSIGN")

		draft, err := uc.CreateDraft(ctx, testWorkspaceID, "Will Submit", "Body", []string{"UASSIGN"}, nil, false, false)
		gt.NoError(t, err).Required()
		gt.Value(t, draft.Status).Equal(types.CaseStatusDraft)

		submitted, err := uc.SubmitDraft(ctx, testWorkspaceID, draft.ID, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, submitted.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, submitted.ID).Equal(draft.ID)
		gt.Value(t, submitted.SlackChannelID).Equal(fmt.Sprintf("C%d", draft.ID))

		// Reporter + assignee both invited during activation.
		gt.Array(t, mock.invitedUserIDs).Length(2)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UAUTHOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UASSIGN")

		// Persisted state matches.
		stored, err := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, stored.SlackChannelID).Equal(fmt.Sprintf("C%d", draft.ID))
	})

	t.Run("submit-with-patch applies edits and still invites reporter", func(t *testing.T) {
		// Architecturally important: "save final edits and submit" is one
		// business operation, so it runs through a single SubmitDraft call.
		// Assignees are now mutated via AssignCase / UnassignCase before
		// calling SubmitDraft. We verify the resulting channel invite list
		// still includes the original reporter (UAUTHOR) even though the
		// title/description patch did not name them.
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})
		seedSlackUsers(t, repo, "UOLD", "UNEW")

		draft, err := uc.CreateDraft(ctx, testWorkspaceID, "Half-written", "old body", []string{"UOLD"}, nil, false, false)
		gt.NoError(t, err).Required()

		// Swap assignees before submitting: remove UOLD, add UNEW.
		_, err = uc.UnassignCase(ctx, testWorkspaceID, draft.ID, []string{"UOLD"})
		gt.NoError(t, err).Required()
		_, err = uc.AssignCase(ctx, testWorkspaceID, draft.ID, []string{"UNEW"})
		gt.NoError(t, err).Required()

		newTitle := "Final title"
		newBody := "polished body"
		var patch usecase.CaseUpdate
		patch.Title = &newTitle
		patch.Description = &newBody

		submitted, err := uc.SubmitDraft(ctx, testWorkspaceID, draft.ID, &patch)
		gt.NoError(t, err).Required()
		gt.Value(t, submitted.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, submitted.Title).Equal("Final title")
		gt.Value(t, submitted.Description).Equal("polished body")
		gt.Value(t, submitted.ReporterID).Equal("UAUTHOR")

		// Channel invites: reporter (UAUTHOR) + new assignee (UNEW).
		// UOLD was removed before submission.
		gt.Array(t, mock.invitedUserIDs).Length(2)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UAUTHOR")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UNEW")

		// The persisted case reflects the patch — i.e. the same value the
		// channel was activated against, not a stale draft snapshot.
		stored, err := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Title).Equal("Final title")
		gt.Value(t, stored.Description).Equal("polished body")
		gt.Number(t, len(stored.AssigneeIDs)).Equal(1)
		gt.Value(t, stored.AssigneeIDs[0]).Equal("UNEW")
	})

	t.Run("requires title to submit", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})

		draft, err := uc.CreateDraft(ctx, testWorkspaceID, "", "Body only", nil, nil, false, false)
		gt.NoError(t, err).Required()

		_, err = uc.SubmitDraft(ctx, testWorkspaceID, draft.ID, nil)
		gt.Error(t, err)

		stored, err := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("submitter (actor) is invited even when different from reporter", func(t *testing.T) {
		// When Bob promotes Alice's draft, both end up in the case channel:
		// Alice via ReporterID and Bob via the auth-context user that
		// triggered activateCase. Without this guarantee, the user who
		// hits Submit on the Web side would find themselves locked out of
		// the freshly-OPEN case channel.
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UALICE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Public draft", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		submitterCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UBOB"})
		submitted, err := uc.SubmitDraft(submitterCtx, testWorkspaceID, draft.ID, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, submitted.Status).Equal(types.CaseStatusOpen)
		// Reporter is preserved — promotion doesn't reassign ownership.
		gt.Value(t, submitted.ReporterID).Equal("UALICE")

		// Channel invite list contains both the reporter (UALICE) and the
		// submitter (UBOB). Ordering: reporter, actor, assignees.
		gt.Array(t, mock.invitedUserIDs).Length(2)
		gt.Value(t, mock.invitedUserIDs[0]).Equal("UALICE")
		gt.Value(t, mock.invitedUserIDs[1]).Equal("UBOB")
	})

	t.Run("any teammate can submit a public draft", func(t *testing.T) {
		// Drafts are team-shared — once a teammate visits a public draft
		// they can promote it to OPEN. The original reporter stays
		// recorded on the case, and channel activation invites them as
		// usual. Private drafts stay reporter-only because non-reporters
		// can't even see them.
		repo := memory.New()
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
				return fmt.Sprintf("C%d", caseID), nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Shared", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		submitted, err := uc.SubmitDraft(otherCtx, testWorkspaceID, draft.ID, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, submitted.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, submitted.ReporterID).Equal("UMINE")
	})

	t.Run("non-reporter cannot submit a private draft", func(t *testing.T) {
		// Private drafts are invisible to non-reporters, so SubmitDraft
		// reaches them as ErrCaseNotFound from GetDraft (same code path
		// as "draft does not exist").
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Secret", "", nil, nil, true, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		_, err = uc.SubmitDraft(otherCtx, testWorkspaceID, draft.ID, nil)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("rejects submit when required field is still missing", func(t *testing.T) {
		// CreateDraft skipped the required check, but SubmitDraft must enforce
		// it before flipping the case to OPEN — otherwise an incomplete draft
		// would slip past the workspace schema.
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test"},
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{
						ID:       "category",
						Name:     "Category",
						Type:     types.FieldTypeSelect,
						Required: true,
						Options: []config.FieldOption{
							{ID: "a", Name: "A"},
							{ID: "b", Name: "B"},
						},
					},
				},
			},
		})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})

		draft, err := uc.CreateDraft(ctx, testWorkspaceID, "Has Title", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		_, err = uc.SubmitDraft(ctx, testWorkspaceID, draft.ID, nil)
		gt.Error(t, err).Is(usecase.ErrMissingRequiredOnSubmit)
		// The error message must name the missing field so the UI can
		// point the user at it directly (rather than the legacy opaque
		// "required field is missing").
		gt.String(t, err.Error()).Contains("Category")

		// The draft survives the failed submit so the user can fill in the
		// missing field and retry.
		stored, err := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusDraft)
		gt.Value(t, stored.Title).Equal("Has Title")
	})

	t.Run("rolls back to DRAFT when activation fails", func(t *testing.T) {
		repo := memory.New()
		// CreateChannel returns error → activation fails. SubmitDraft's
		// rollback policy preserves the user's work: the case row stays,
		// status reverts to DRAFT, and the user can retry from the same
		// entry rather than starting over.
		mock := &mockSlackService{
			createChannelFn: func(_ context.Context, _ int64, _ string, _ string) (string, error) {
				return "", errors.New("channel creation rejected")
			},
		}
		uc := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})

		draft, err := uc.CreateDraft(ctx, testWorkspaceID, "Will Fail", "Body", nil, nil, false, false)
		gt.NoError(t, err).Required()

		_, err = uc.SubmitDraft(ctx, testWorkspaceID, draft.ID, nil)
		gt.Error(t, err)

		// Persisted case row survives; status is flipped back to DRAFT.
		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.NoError(t, getErr).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusDraft)
		gt.Value(t, stored.Title).Equal("Will Fail")
		gt.Value(t, stored.Description).Equal("Body")
	})

	t.Run("already-submitted case cannot be submitted again", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})

		open, err := uc.CreateCase(ctx, testWorkspaceID, "Already Open", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.SubmitDraft(ctx, testWorkspaceID, open.ID, nil)
		gt.Error(t, err).Is(usecase.ErrCaseNotDraft)
	})
}

func TestCaseUseCase_DiscardDraft(t *testing.T) {
	t.Run("reporter discards own draft", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		draft, err := uc.CreateDraft(ctx, testWorkspaceID, "Discard Me", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		err = uc.DiscardDraft(ctx, testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()

		_, getErr := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.Error(t, getErr)
	})

	t.Run("any teammate can discard a public draft", func(t *testing.T) {
		// Public drafts are workspace-shared so any teammate can clean one
		// up. Private drafts remain reporter-only because GetDraft hides
		// them from everyone else (covered in the next case).
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Shared", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		gt.NoError(t, uc.DiscardDraft(otherCtx, testWorkspaceID, draft.ID)).Required()

		_, getErr := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.Error(t, getErr)
	})

	t.Run("non-reporter cannot discard a private draft", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Hidden", "", nil, nil, true, false)
		gt.NoError(t, err).Required()

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		err = uc.DiscardDraft(otherCtx, testWorkspaceID, draft.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)

		// Draft must still exist after the failed discard.
		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, draft.ID)
		gt.NoError(t, getErr).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("non-draft case is rejected", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})

		open, err := uc.CreateCase(ctx, testWorkspaceID, "Open", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		err = uc.DiscardDraft(ctx, testWorkspaceID, open.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotDraft)
	})
}

func TestCaseUseCase_GetCase_DraftVisibility(t *testing.T) {
	t.Run("public draft is readable by anyone in the workspace", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Public draft", "", nil, nil, false, false)
		gt.NoError(t, err).Required()

		// Reporter sees their draft.
		got, err := uc.GetCase(ownerCtx, testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ID).Equal(draft.ID)

		// Stranger also sees the public draft (workspace-wide visibility).
		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		got, err = uc.GetCase(otherCtx, testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ID).Equal(draft.ID)
	})

	t.Run("private draft stays hidden from non-reporters", func(t *testing.T) {
		// Private drafts have no Slack channel yet, so the normal
		// ChannelUserIDs-based check would lock out the reporter too.
		// GetCase falls back to a reporter check for private drafts in
		// DRAFT status.
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		ownerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMINE"})
		draft, err := uc.CreateDraft(ownerCtx, testWorkspaceID, "Secret", "", nil, nil, true, false)
		gt.NoError(t, err).Required()

		got, err := uc.GetCase(ownerCtx, testWorkspaceID, draft.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.ID).Equal(draft.ID)

		otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		_, err = uc.GetCase(otherCtx, testWorkspaceID, draft.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})
}

// recordingCaseEventPublisher captures every PublishCaseLifecycle invocation
// so lifecycle-wiring tests can assert what fired and what did not.
type recordingCaseEventPublisher struct {
	events []recordedCaseEvent
}

type recordedCaseEvent struct {
	workspaceID string
	caseID      int64
	lifecycle   model.CaseLifecycle
	actor       string
}

func (r *recordingCaseEventPublisher) PublishCaseLifecycle(_ context.Context, workspaceID string, c *model.Case, lifecycle model.CaseLifecycle, actor string) {
	r.events = append(r.events, recordedCaseEvent{
		workspaceID: workspaceID,
		caseID:      c.ID,
		lifecycle:   lifecycle,
		actor:       actor,
	})
}

func TestCaseUseCase_PublishesCreatedLifecycle(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test"},
		FieldSchema: &config.FieldSchema{},
	})
	uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	pub := &recordingCaseEventPublisher{}
	uc.SetEventPublisher(pub)

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CALLER"})
	created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()

	gt.Array(t, pub.events).Length(1).Required()
	gt.Value(t, pub.events[0].lifecycle).Equal(model.CaseLifecycleCreated)
	gt.Value(t, pub.events[0].caseID).Equal(created.ID)
	gt.String(t, pub.events[0].workspaceID).Equal(testWorkspaceID)
	gt.String(t, pub.events[0].actor).Equal("U-CALLER")
}

func TestCaseUseCase_PublishesClosedLifecycle(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test"},
		FieldSchema: &config.FieldSchema{},
	})
	uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	pub := &recordingCaseEventPublisher{}
	uc.SetEventPublisher(pub)

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CALLER"})
	created, err := uc.CreateCase(ctx, testWorkspaceID, "T", "", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	gt.Array(t, pub.events).Length(1).Required()

	closed, err := uc.CloseCase(ctx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Array(t, pub.events).Length(2).Required()
	gt.Value(t, pub.events[1].lifecycle).Equal(model.CaseLifecycleClosed)
	gt.Value(t, pub.events[1].caseID).Equal(closed.ID)
}

func TestCaseUseCase_NoPublishWhenNotConfigured(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test"},
		FieldSchema: &config.FieldSchema{},
	})
	uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	// no SetEventPublisher

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CALLER"})
	_, err := uc.CreateCase(ctx, testWorkspaceID, "T", "", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	// Compiles and runs without panic.
}

// jobLifecycleSlackPostRecorder is a slackpost.Poster that records every
// PostMessage / PostThreadMessage call so the lifecycle test below can
// verify the agent actually reached the Slack tool.
type jobLifecycleSlackPostRecorder struct {
	mu     sync.Mutex
	posted []jobLifecycleSlackPost
}

type jobLifecycleSlackPost struct {
	channelID string
	threadTS  string
	text      string
}

func (r *jobLifecycleSlackPostRecorder) PostMessage(_ context.Context, channelID string, _ []goslack.Block, text string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posted = append(r.posted, jobLifecycleSlackPost{channelID: channelID, text: text})
	return "ts-1", nil
}

func (r *jobLifecycleSlackPostRecorder) PostThreadMessage(_ context.Context, channelID string, threadTS string, _ []goslack.Block, text string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posted = append(r.posted, jobLifecycleSlackPost{channelID: channelID, threadTS: threadTS, text: text})
	return "ts-2", nil
}

func (r *jobLifecycleSlackPostRecorder) snapshot() []jobLifecycleSlackPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]jobLifecycleSlackPost(nil), r.posted...)
	return out
}

// TestLifecycle_CreateCaseTriggersAgentJob wires the full event-driven Job
// pipeline (CaseUseCase publish -> JobUseCase match -> JobRunner ->
// SingleLoopJobExecutor -> slack_post tool) and verifies that a brand-new
// Case fires a Slack post via the agent.
func TestLifecycle_CreateCaseTriggersAgentJob(t *testing.T) {
	const wsID = "lifecycle-ws"
	caseJob := &model.Job{
		ID:     "summarize_on_create",
		Prompt: "Post a one-line summary of the case to Slack via slack__post_to_case_channel.",
		Events: model.JobEvents{
			Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
		},
	}
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: wsID, Name: "Lifecycle"},
		FieldSchema: &config.FieldSchema{},
		Jobs:        []*model.Job{caseJob},
	})

	repo := memory.New()
	caseUC := usecase.NewCaseUseCase(repo, registry, nil, nil, "")

	round := atomic.Int32{}
	llm := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					n := round.Add(1)
					switch n {
					case 1:
						return &gollem.Response{
							FunctionCalls: []*gollem.FunctionCall{{
								ID:   "call-1",
								Name: "slack__post_to_case_channel",
								Arguments: map[string]any{
									"text": "Lifecycle test (auto summary)",
								},
							}},
						}, nil
					default:
						return &gollem.Response{Texts: []string{"done"}}, nil
					}
				},
			}, nil
		},
	}

	postRec := &jobLifecycleSlackPostRecorder{}

	builder := job.ToolBuilderFunc(func(_ context.Context, c *model.Case, _ *model.WorkspaceEntry) []gollem.Tool {
		channelID := "C-LIFECYCLE"
		if c != nil && c.SlackChannelID != "" {
			channelID = c.SlackChannelID
		}
		return slackpost.New(slackpost.Deps{Poster: postRec, ChannelID: channelID})
	})

	runner := job.NewJobRunner(job.RunnerDeps{
		Repo:        repo,
		Registry:    registry,
		LLMClient:   llm,
		Executors:   map[model.JobStrategy]jobagent.JobExecutor{model.JobStrategySimple: jobagent.NewSingleLoopJobExecutor()},
		ToolBuilder: builder,
	})
	jobUC := job.NewUseCase(registry, runner)
	caseUC.SetEventPublisher(jobUC)

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CALLER"})
	created, err := caseUC.CreateCase(ctx, wsID, "Lifecycle test", "auto summary please", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()

	async.Wait()

	posts := postRec.snapshot()
	gt.Array(t, posts).Length(1).Required()
	gt.String(t, posts[0].text).Contains("Lifecycle test")

	run, err := repo.JobRun().Get(ctx, model.JobRunKey{
		WorkspaceID: wsID,
		CaseID:      created.ID,
		JobID:       caseJob.ID,
	})
	gt.NoError(t, err).Required()
	gt.Value(t, run.LastStatus).Equal(model.JobRunStatusSuccess)
}

func TestCaseUseCase_UpdateAgentSettings(t *testing.T) {
	setup := func(t *testing.T) (interfaces.Repository, *usecase.CaseUseCase, model.SourceID, model.SourceID, *model.Case) {
		t.Helper()
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		src1, err := repo.Source().Create(context.Background(), testWorkspaceID, &model.Source{
			ID: model.NewSourceID(), Name: "src1", SourceType: model.SourceTypeSlack, Enabled: true,
			SlackConfig: &model.SlackConfig{},
		})
		gt.NoError(t, err).Required()
		src2, err := repo.Source().Create(context.Background(), testWorkspaceID, &model.Source{
			ID: model.NewSourceID(), Name: "src2", SourceType: model.SourceTypeGitHub, Enabled: true,
			GitHubConfig: &model.GitHubConfig{},
		})
		gt.NoError(t, err).Required()

		c, err := uc.CreateCase(ctx, testWorkspaceID, "agent settings target", "desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		return repo, uc, src1.ID, src2.ID, c
	}

	t.Run("updates prompt and source list", func(t *testing.T) {
		_, uc, src1, src2, c := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		updated, err := uc.UpdateAgentSettings(ctx, testWorkspaceID, c.ID, "## case notes", []model.SourceID{src1, src2})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AgentAdditionalPrompt).Equal("## case notes")
		gt.Array(t, updated.AgentSourceIDs).Length(2).Required()
		gt.Value(t, updated.AgentSourceIDs[0]).Equal(src1)
		gt.Value(t, updated.AgentSourceIDs[1]).Equal(src2)

		// Read-back through GetCase to confirm persistence.
		got, err := uc.GetCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.AgentAdditionalPrompt).Equal("## case notes")
		gt.Array(t, got.AgentSourceIDs).Length(2).Required()
	})

	t.Run("clearing source list keeps empty selection", func(t *testing.T) {
		_, uc, src1, _, c := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		_, err := uc.UpdateAgentSettings(ctx, testWorkspaceID, c.ID, "first", []model.SourceID{src1})
		gt.NoError(t, err).Required()

		cleared, err := uc.UpdateAgentSettings(ctx, testWorkspaceID, c.ID, "", nil)
		gt.NoError(t, err).Required()
		gt.Value(t, cleared.AgentAdditionalPrompt).Equal("")
		gt.Number(t, len(cleared.AgentSourceIDs)).Equal(0)
	})

	t.Run("unknown source id is rejected", func(t *testing.T) {
		_, uc, src1, _, c := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		bogus := model.NewSourceID()
		_, err := uc.UpdateAgentSettings(ctx, testWorkspaceID, c.ID, "p", []model.SourceID{src1, bogus})
		gt.Error(t, err).Is(usecase.ErrInvalidArgument)
	})

	t.Run("duplicate source id is rejected", func(t *testing.T) {
		_, uc, src1, _, c := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		_, err := uc.UpdateAgentSettings(ctx, testWorkspaceID, c.ID, "p", []model.SourceID{src1, src1})
		gt.Error(t, err).Is(usecase.ErrInvalidArgument)
	})

	t.Run("private case rejects non-member", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})

		c, err := uc.CreateCase(ctx, testWorkspaceID, "private case", "", nil, nil, true, false, "", "")
		gt.NoError(t, err).Required()

		// Mark the case as private with explicit channel membership.
		raw, err := repo.Case().Get(context.Background(), testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		raw.IsPrivate = true
		raw.ChannelUserIDs = []string{"UREPORTER"}
		_, err = repo.Case().Update(context.Background(), testWorkspaceID, raw)
		gt.NoError(t, err).Required()

		// Member succeeds.
		_, err = uc.UpdateAgentSettings(ctx, testWorkspaceID, c.ID, "ok", nil)
		gt.NoError(t, err)

		// Non-member is denied.
		other := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "USTRANGER"})
		_, err = uc.UpdateAgentSettings(other, testWorkspaceID, c.ID, "blocked", nil)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	t.Run("missing case returns not found", func(t *testing.T) {
		_, uc, _, _, _ := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UREPORTER"})
		_, err := uc.UpdateAgentSettings(ctx, testWorkspaceID, 99999, "p", nil)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})
}

func TestCaseUseCase_UpdateCaseStatus(t *testing.T) {
	newThreadUC := func(t *testing.T) (interfaces.Repository, *usecase.CaseUseCase) {
		t.Helper()
		repo := memory.New()
		set, err := model.NewActionStatusSet("triage", []string{"done"}, []model.ActionStatusDefinition{
			{ID: "triage", Name: "Triage"},
			{ID: "in_review", Name: "In Review"},
			{ID: "done", Name: "Done"},
		})
		gt.NoError(t, err).Required()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:             model.Workspace{ID: "support"},
			CaseMode:              model.CaseModeThread,
			SlackMonitorChannelID: "C-MONITOR",
			CaseStatusSet:         set,
		})
		return repo, usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	}

	t.Run("moving to an open status keeps the case OPEN", func(t *testing.T) {
		repo, uc := newThreadUC(t)
		ctx := context.Background()
		c, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000100", "U-REP", "t", "b", nil, "")
		gt.NoError(t, err).Required()

		updated, _, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "in_review")
		gt.NoError(t, err).Required()
		gt.Value(t, updated.BoardStatus).Equal("in_review")
		gt.Value(t, updated.Status).Equal(types.CaseStatusOpen)
		_ = repo
	})

	t.Run("moving to a closed status closes the case", func(t *testing.T) {
		_, uc := newThreadUC(t)
		ctx := context.Background()
		c, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000200", "U-REP", "t", "b", nil, "")
		gt.NoError(t, err).Required()

		updated, _, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		gt.Value(t, updated.BoardStatus).Equal("done")
		gt.Value(t, updated.Status).Equal(types.CaseStatusClosed)
	})

	t.Run("invalid board status is rejected", func(t *testing.T) {
		_, uc := newThreadUC(t)
		ctx := context.Background()
		c, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000300", "U-REP", "t", "b", nil, "")
		gt.NoError(t, err).Required()

		_, _, err = uc.UpdateCaseStatus(ctx, "support", c.ID, "bogus")
		gt.Error(t, err)
	})

	t.Run("channel-mode workspace has no case status set", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "chan"}})
		uc := usecase.NewCaseUseCase(repo, registry, nil, nil, "")
		ctx := context.Background()
		created, err := repo.Case().Create(ctx, "chan", &model.Case{ReporterID: "U", Title: "x", SlackChannelID: "C1"})
		gt.NoError(t, err).Required()

		_, _, err = uc.UpdateCaseStatus(ctx, "chan", created.ID, "anything")
		gt.Error(t, err)
	})
}

func TestCaseUseCase_CreateThreadCaseWithFields(t *testing.T) {
	newThreadUC := func(t *testing.T) (interfaces.Repository, *usecase.CaseUseCase) {
		t.Helper()
		repo := memory.New()
		set, err := model.NewActionStatusSet("triage", []string{"done"}, []model.ActionStatusDefinition{
			{ID: "triage", Name: "Triage"},
			{ID: "done", Name: "Done"},
		})
		gt.NoError(t, err).Required()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:             model.Workspace{ID: "support"},
			CaseMode:              model.CaseModeThread,
			SlackMonitorChannelID: "C-MONITOR",
			CaseStatusSet:         set,
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect, Required: true, Options: []config.FieldOption{{ID: "high", Name: "High"}, {ID: "low", Name: "Low"}}},
					{ID: "summary", Name: "Summary", Type: types.FieldTypeText, Required: true},
				},
			},
		})
		return repo, usecase.NewCaseUseCase(repo, registry, nil, nil, "")
	}

	t.Run("full validation rejects missing required and bad option (aggregated)", func(t *testing.T) {
		_, uc := newThreadUC(t)
		ctx := context.Background()
		_, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000100", "U-REP", "Title",
			"desc", map[string]model.FieldValue{
				"severity": {FieldID: "severity", Value: "critical"}, // invalid option; summary missing
			}, "")
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
		gt.String(t, err.Error()).Contains("severity")
		gt.String(t, err.Error()).Contains("summary")
	})

	t.Run("valid fields create the case and round-trip", func(t *testing.T) {
		repo, uc := newThreadUC(t)
		ctx := context.Background()
		created, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000200", "U-REP", "Login outage",
			"Users cannot log in.", map[string]model.FieldValue{
				"severity": {FieldID: "severity", Value: "high"},
				"summary":  {FieldID: "summary", Value: "login broken"},
			}, "")
		gt.NoError(t, err).Required()
		gt.Value(t, created.Title).Equal("Login outage")
		gt.Value(t, created.BoardStatus).Equal("triage")
		gt.Value(t, created.ReporterID).Equal("U-REP")

		got, err := repo.Case().GetBySlackThread(ctx, "support", "C-MONITOR", "1700000000.000200")
		gt.NoError(t, err).Required()
		gt.Value(t, got).NotNil().Required()
		gt.Value(t, got.Title).Equal("Login outage")
		gt.Value(t, got.Description).Equal("Users cannot log in.")
		gt.Value(t, got.FieldValues["severity"].Value).Equal("high")
		gt.Value(t, got.FieldValues["summary"].Value).Equal("login broken")
	})

	t.Run("idempotent on existing thread", func(t *testing.T) {
		_, uc := newThreadUC(t)
		ctx := context.Background()
		first, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000300", "U-REP", "T",
			"d", map[string]model.FieldValue{
				"severity": {FieldID: "severity", Value: "low"},
				"summary":  {FieldID: "summary", Value: "s"},
			}, "")
		gt.NoError(t, err).Required()
		again, err := uc.CreateThreadBoundCaseForTest(ctx, "support", "C-MONITOR", "1700000000.000300", "U-REP", "T2",
			"d2", nil, "")
		gt.NoError(t, err).Required()
		gt.Number(t, again.ID).Equal(first.ID)
	})
}

// seedSlackUsers stores minimal SlackUser records so case writes that
// reference these ids (assignees / user-field values) pass the existence
// check that every case write now performs via CaseUseCase.validateCaseWrite.
// Shared across the usecase_test package.
func seedSlackUsers(t *testing.T, repo interfaces.Repository, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	users := make([]*model.SlackUser, 0, len(ids))
	for _, id := range ids {
		users = append(users, &model.SlackUser{ID: model.SlackUserID(id), Name: id})
	}
	gt.NoError(t, repo.SlackUser().SaveMany(context.Background(), users)).Required()
}

func TestCaseUseCase_AssignCase_ResolvesUnsyncedUsers(t *testing.T) {
	// setup creates a Case through a CaseUseCase without Slack, then returns a
	// second CaseUseCase over the same repository whose Slack service answers
	// users.info with lookup. Only "UCREATOR" is in the SlackUser store.
	setup := func(t *testing.T, lookup func(ctx context.Context, userID string) (*slack.User, error)) (*usecase.CaseUseCase, context.Context, int64, *[]string) {
		t.Helper()
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
		})
		seedSlackUsers(t, repo, "UCREATOR")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := usecase.NewCaseUseCase(repo, registry, nil, nil, "").
			CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		var asked []string
		slackMock := &mockSlackService{
			getUserInfoFn: func(ctx context.Context, userID string) (*slack.User, error) {
				asked = append(asked, userID)
				return lookup(ctx, userID)
			},
		}
		return usecase.NewCaseUseCase(repo, registry, slackMock, nil, ""), ctx, created.ID, &asked
	}

	t.Run("accepts a human user Slack knows but the store does not", func(t *testing.T) {
		uc, ctx, caseID, asked := setup(t, func(_ context.Context, userID string) (*slack.User, error) {
			return &slack.User{ID: userID, Name: "unsynced"}, nil
		})
		updated, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UUNSYNCED"})
		gt.NoError(t, err).Required()
		gt.Array(t, updated.AssigneeIDs).Equal([]string{"UUNSYNCED"})
		gt.Array(t, *asked).Equal([]string{"UUNSYNCED"})
	})

	t.Run("does not ask Slack about users in the store", func(t *testing.T) {
		uc, ctx, caseID, asked := setup(t, func(_ context.Context, userID string) (*slack.User, error) {
			return &slack.User{ID: userID}, nil
		})
		_, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UCREATOR"})
		gt.NoError(t, err).Required()
		gt.Array(t, *asked).Length(0)
	})

	t.Run("rejects a user Slack reports as not found", func(t *testing.T) {
		uc, ctx, caseID, _ := setup(t, func(_ context.Context, userID string) (*slack.User, error) {
			return nil, goerr.Wrap(slack.ErrUserNotFound, "users.info", goerr.V("user_id", userID))
		})
		_, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UGHOST"})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)
	})

	t.Run("rejects a bot user", func(t *testing.T) {
		uc, ctx, caseID, _ := setup(t, func(_ context.Context, userID string) (*slack.User, error) {
			return &slack.User{ID: userID, IsBot: true}, nil
		})
		_, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UBOT"})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)
	})

	t.Run("rejects a deleted user", func(t *testing.T) {
		uc, ctx, caseID, _ := setup(t, func(_ context.Context, userID string) (*slack.User, error) {
			return &slack.User{ID: userID, Deleted: true}, nil
		})
		_, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UGONE"})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)
	})

	t.Run("reports only the ids that stay unknown", func(t *testing.T) {
		uc, ctx, caseID, _ := setup(t, func(_ context.Context, userID string) (*slack.User, error) {
			if userID == "UGHOST" {
				return nil, goerr.Wrap(slack.ErrUserNotFound, "users.info")
			}
			return &slack.User{ID: userID}, nil
		})
		_, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UUNSYNCED", "UGHOST"})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)
		var ge *goerr.Error
		gt.Bool(t, errors.As(err, &ge)).True().Required()
		gt.Value(t, ge.Values()["missing_user_ids"]).Equal([]string{"UGHOST"})
	})

	t.Run("propagates other Slack failures", func(t *testing.T) {
		boom := goerr.New("slack is down")
		uc, ctx, caseID, _ := setup(t, func(_ context.Context, _ string) (*slack.User, error) {
			return nil, boom
		})
		_, err := uc.AssignCase(ctx, testWorkspaceID, caseID, []string{"UUNSYNCED"})
		gt.Error(t, err).Is(boom)
		gt.Bool(t, errors.Is(err, usecase.ErrUnknownUser)).False()
	})
}

func TestCaseUseCase_UpdateCase_UserExistence(t *testing.T) {
	newUC := func(t *testing.T) (*usecase.CaseUseCase, interfaces.Repository) {
		t.Helper()
		repo := memory.New()
		fieldSchema := &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "owner", Name: "Owner", Type: types.FieldTypeUser},
				{ID: "watchers", Name: "Watchers", Type: types.FieldTypeMultiUser},
			},
		}
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Test Workspace"},
			FieldSchema: fieldSchema,
		})
		return usecase.NewCaseUseCase(repo, registry, nil, nil, ""), repo
	}

	t.Run("rejects unknown assignee id", func(t *testing.T) {
		uc, repo := newUC(t)
		seedSlackUsers(t, repo, "UCREATOR")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Assignees are now mutated via AssignCase, not UpdateCase.
		_, err = uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{"UGHOST"})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)
	})

	t.Run("accepts assignee that exists in the SlackUser store", func(t *testing.T) {
		uc, repo := newUC(t)
		seedSlackUsers(t, repo, "UCREATOR", "UREAL")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Assignees are now mutated via AssignCase, not UpdateCase.
		updated, err := uc.AssignCase(ctx, testWorkspaceID, created.ID, []string{"UREAL"})
		gt.NoError(t, err).Required()
		gt.Array(t, updated.AssigneeIDs).Equal([]string{"UREAL"})
	})

	t.Run("rejects unknown user-field value", func(t *testing.T) {
		uc, repo := newUC(t)
		seedSlackUsers(t, repo, "UCREATOR")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"owner": {FieldID: "owner", Value: "UGHOST"},
			},
		})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)
	})

	t.Run("rejects unknown multi-user field value, accepts known", func(t *testing.T) {
		uc, repo := newUC(t)
		seedSlackUsers(t, repo, "UCREATOR", "UA", "UB")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"watchers": {FieldID: "watchers", Type: types.FieldTypeMultiUser, Value: []string{"UA", "UGHOST"}},
			},
		})
		gt.Error(t, err).Is(usecase.ErrUnknownUser)

		updated, err := uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"watchers": {FieldID: "watchers", Type: types.FieldTypeMultiUser, Value: []string{"UA", "UB"}},
			},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.FieldValues["watchers"].Value).Equal([]string{"UA", "UB"})
	})

	t.Run("rejects submitted unknown field id (strict)", func(t *testing.T) {
		uc, repo := newUC(t)
		seedSlackUsers(t, repo, "UCREATOR")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := uc.CreateCase(ctx, testWorkspaceID, "Title", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"ghost_field": {FieldID: "ghost_field", Value: "x"},
			},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})
}

// ----- case_ref fields -----

const refWorkspaceID = "ref-ws"

// newCaseRefUC builds a CaseUseCase whose main workspace (testWorkspaceID)
// defines a single case_ref field "related" and a multi_case_ref
// field "related_multi", both pointing at refWorkspaceID. The reference
// workspace itself needs no schema entry — referenceability is decided purely
// by the referenced case's IsPrivate / draft state.
func newCaseRefUC(t *testing.T) (*usecase.CaseUseCase, interfaces.Repository) {
	t.Helper()
	repo := memory.New()
	fieldSchema := &config.FieldSchema{
		Fields: []config.FieldDefinition{
			{ID: "related", Name: "Related", Type: types.FieldTypeCaseRef, ReferenceWorkspace: refWorkspaceID},
			{ID: "related_multi", Name: "Related Multi", Type: types.FieldTypeMultiCaseRef, ReferenceWorkspace: refWorkspaceID},
			{ID: "owner", Name: "Owner", Type: types.FieldTypeUser},
		},
	}
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Main Workspace"},
		FieldSchema: fieldSchema,
	})
	return usecase.NewCaseUseCase(repo, registry, nil, nil, ""), repo
}

// seedRefCase persists a case directly into a workspace and returns its ID.
// Used to populate the reference workspace with public / private / draft /
// closed cases the case_ref tests reference.
func seedRefCase(t *testing.T, repo interfaces.Repository, ws, title string, status types.CaseStatus, private bool) int64 {
	t.Helper()
	c := &model.Case{
		Title:      title,
		Status:     status,
		ReporterID: "URPT",
		IsPrivate:  private,
	}
	if private {
		c.ChannelUserIDs = []string{"URPT"}
	}
	created, err := repo.Case().Create(context.Background(), ws, c)
	gt.NoError(t, err).Required()
	return created.ID
}

func TestCaseUseCase_VerifyCaseRefsExist(t *testing.T) {
	ctxWith := func() context.Context {
		return auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
	}

	setup := func(t *testing.T) (*usecase.CaseUseCase, interfaces.Repository, int64) {
		uc, repo := newCaseRefUC(t)
		ctx := ctxWith()
		base, err := uc.CreateCase(ctx, testWorkspaceID, "Main", "Desc", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		return uc, repo, base.ID
	}

	t.Run("accepts reference to a public open case", func(t *testing.T) {
		uc, repo, baseID := setup(t)
		pub := seedRefCase(t, repo, refWorkspaceID, "DB outage", types.CaseStatusOpen, false)

		updated, err := uc.UpdateCase(ctxWith(), testWorkspaceID, baseID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"related": {FieldID: "related", Value: fmt.Sprintf("%d", pub)},
			},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.FieldValues["related"].Value).Equal(fmt.Sprintf("%d", pub))
	})

	t.Run("rejects reference to a private case", func(t *testing.T) {
		uc, repo, baseID := setup(t)
		priv := seedRefCase(t, repo, refWorkspaceID, "Secret", types.CaseStatusOpen, true)

		_, err := uc.UpdateCase(ctxWith(), testWorkspaceID, baseID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"related": {FieldID: "related", Value: fmt.Sprintf("%d", priv)},
			},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})

	t.Run("rejects reference to a draft case", func(t *testing.T) {
		uc, repo, baseID := setup(t)
		draft := seedRefCase(t, repo, refWorkspaceID, "WIP", types.CaseStatusDraft, false)

		_, err := uc.UpdateCase(ctxWith(), testWorkspaceID, baseID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"related": {FieldID: "related", Value: fmt.Sprintf("%d", draft)},
			},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})

	t.Run("rejects reference to a missing case", func(t *testing.T) {
		uc, _, baseID := setup(t)

		_, err := uc.UpdateCase(ctxWith(), testWorkspaceID, baseID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"related": {FieldID: "related", Value: "99999999"},
			},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})

	t.Run("multi rejects when any element is private, accepts all public", func(t *testing.T) {
		uc, repo, baseID := setup(t)
		a := seedRefCase(t, repo, refWorkspaceID, "A", types.CaseStatusOpen, false)
		b := seedRefCase(t, repo, refWorkspaceID, "B", types.CaseStatusOpen, false)
		priv := seedRefCase(t, repo, refWorkspaceID, "Secret", types.CaseStatusOpen, true)

		_, err := uc.UpdateCase(ctxWith(), testWorkspaceID, baseID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"related_multi": {FieldID: "related_multi", Value: []string{fmt.Sprintf("%d", a), fmt.Sprintf("%d", priv)}},
			},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)

		updated, err := uc.UpdateCase(ctxWith(), testWorkspaceID, baseID, usecase.CaseUpdate{
			Fields: map[string]model.FieldValue{
				"related_multi": {FieldID: "related_multi", Value: []string{fmt.Sprintf("%d", a), fmt.Sprintf("%d", b)}},
			},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.FieldValues["related_multi"].Value).Equal([]string{fmt.Sprintf("%d", a), fmt.Sprintf("%d", b)})
	})
}

func TestCaseUseCase_ListReferenceableCases(t *testing.T) {
	ctx := context.Background()

	t.Run("excludes private and draft, lists open first", func(t *testing.T) {
		uc, repo := newCaseRefUC(t)
		open := seedRefCase(t, repo, refWorkspaceID, "DB outage", types.CaseStatusOpen, false)
		closed := seedRefCase(t, repo, refWorkspaceID, "Old incident", types.CaseStatusClosed, false)
		seedRefCase(t, repo, refWorkspaceID, "Secret", types.CaseStatusOpen, true)
		seedRefCase(t, repo, refWorkspaceID, "WIP", types.CaseStatusDraft, false)

		refs, err := uc.ListReferenceableCases(ctx, refWorkspaceID, "", 50)
		gt.NoError(t, err).Required()
		gt.Array(t, refs).Length(2).Required()
		gt.Value(t, refs[0].ID).Equal(open)
		gt.Value(t, refs[0].Status).Equal(types.CaseStatusOpen)
		gt.Value(t, refs[1].ID).Equal(closed)
	})

	t.Run("query filters by title substring", func(t *testing.T) {
		uc, repo := newCaseRefUC(t)
		dbCase := seedRefCase(t, repo, refWorkspaceID, "DB outage", types.CaseStatusOpen, false)
		seedRefCase(t, repo, refWorkspaceID, "Network blip", types.CaseStatusOpen, false)

		refs, err := uc.ListReferenceableCases(ctx, refWorkspaceID, "outage", 50)
		gt.NoError(t, err).Required()
		gt.Array(t, refs).Length(1).Required()
		gt.Value(t, refs[0].ID).Equal(dbCase)
	})

	t.Run("query matches by case id", func(t *testing.T) {
		uc, repo := newCaseRefUC(t)
		seedRefCase(t, repo, refWorkspaceID, "Network blip", types.CaseStatusOpen, false)
		target := seedRefCase(t, repo, refWorkspaceID, "DB outage", types.CaseStatusOpen, false)

		refs, err := uc.ListReferenceableCases(ctx, refWorkspaceID, fmt.Sprintf("#%d", target), 50)
		gt.NoError(t, err).Required()
		gt.Array(t, refs).Length(1).Required()
		gt.Value(t, refs[0].ID).Equal(target)
	})
}

func TestCaseUseCase_ResolveCaseRefs(t *testing.T) {
	ctx := context.Background()
	uc, repo := newCaseRefUC(t)
	pub := seedRefCase(t, repo, refWorkspaceID, "Public", types.CaseStatusOpen, false)
	priv := seedRefCase(t, repo, refWorkspaceID, "Secret", types.CaseStatusOpen, true)

	refs, err := uc.ResolveCaseRefs(ctx, refWorkspaceID, []int64{pub, priv, 99999999})
	gt.NoError(t, err).Required()
	gt.Array(t, refs).Length(1).Required()
	gt.Value(t, refs[0].ID).Equal(pub)
	gt.Value(t, refs[0].Title).Equal("Public")
}

func TestCaseUseCase_GetReferenceableCases(t *testing.T) {
	ctx := context.Background()
	uc, repo := newCaseRefUC(t)
	pub := seedRefCase(t, repo, refWorkspaceID, "Public", types.CaseStatusOpen, false)
	priv := seedRefCase(t, repo, refWorkspaceID, "Secret", types.CaseStatusOpen, true)
	draft := seedRefCase(t, repo, refWorkspaceID, "WIP", types.CaseStatusDraft, false)

	cases, err := uc.GetReferenceableCases(ctx, refWorkspaceID, []int64{pub, priv, draft, 99999999})
	gt.NoError(t, err).Required()
	gt.Array(t, cases).Length(1).Required()
	gt.Value(t, cases[0].ID).Equal(pub)
	gt.Value(t, cases[0].Title).Equal("Public")
}

// A reference into a workspace the caller may not access is omitted like a
// private one, while a context without a token (a Job, the agent with no
// actor) still resolves it.
func TestCaseUseCase_GetReferenceableCases_DeniedWorkspace(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: testWorkspaceID, Name: "Main"}})
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: refWorkspaceID, Name: "Referenced"}})
	access, err := usecase.NewWorkspaceAccessUseCase(registry,
		map[string]interfaces.PolicyClient{refWorkspaceID: compileAuthzPolicy(t, policyDenyAll)},
		repo.SlackUser(), usecase.WorkspaceAccessCacheConfig{TTL: time.Minute, Size: 16})
	gt.NoError(t, err).Required()
	uc := usecase.New(repo, registry, usecase.WithWorkspaceAccess(access))

	pub := seedRefCase(t, repo, refWorkspaceID, "Public", types.CaseStatusOpen, false)

	userCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U0ALICE"})
	cases, err := uc.Case.GetReferenceableCases(userCtx, refWorkspaceID, []int64{pub})
	gt.NoError(t, err).Required()
	gt.Array(t, cases).Length(0)

	cases, err = uc.Case.GetReferenceableCases(context.Background(), refWorkspaceID, []int64{pub})
	gt.NoError(t, err).Required()
	gt.Array(t, cases).Length(1).Required()
	gt.Value(t, cases[0].ID).Equal(pub)
}

func TestCaseUseCase_ReferenceWorkspaceForField(t *testing.T) {
	uc, _ := newCaseRefUC(t)

	t.Run("resolves the configured reference workspace", func(t *testing.T) {
		ws, err := uc.ReferenceWorkspaceForField(testWorkspaceID, "related")
		gt.NoError(t, err).Required()
		gt.Value(t, ws).Equal(refWorkspaceID)
	})

	t.Run("errors for a non case_ref field", func(t *testing.T) {
		_, err := uc.ReferenceWorkspaceForField(testWorkspaceID, "owner")
		gt.Error(t, err)
	})

	t.Run("errors for an unknown field", func(t *testing.T) {
		_, err := uc.ReferenceWorkspaceForField(testWorkspaceID, "ghost")
		gt.Error(t, err)
	})
}

func TestCaseUseCase_RenderCaseFieldValues(t *testing.T) {
	ctx := context.Background()
	uc, repo := newCaseRefUC(t)
	pub := seedRefCase(t, repo, refWorkspaceID, "DB outage", types.CaseStatusOpen, false)
	priv := seedRefCase(t, repo, refWorkspaceID, "Secret", types.CaseStatusOpen, true)

	t.Run("renders non-reference fields raw and resolves references", func(t *testing.T) {
		out, err := uc.RenderCaseFieldValues(ctx, testWorkspaceID, map[string]model.FieldValue{
			"owner":   {FieldID: "owner", Type: types.FieldTypeUser, Value: "U1"},
			"related": {FieldID: "related", Type: types.FieldTypeCaseRef, Value: fmt.Sprintf("%d", pub)},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, out["owner"]).Equal("U1")
		resolved := out["related"].(map[string]any)
		gt.Value(t, resolved["id"]).Equal(pub)
		gt.Value(t, resolved["title"]).Equal("DB outage")
		gt.Value(t, resolved["status"]).Equal("OPEN")
	})

	t.Run("marks a private reference as unavailable on read drift", func(t *testing.T) {
		out, err := uc.RenderCaseFieldValues(ctx, testWorkspaceID, map[string]model.FieldValue{
			"related": {FieldID: "related", Type: types.FieldTypeCaseRef, Value: fmt.Sprintf("%d", priv)},
		})
		gt.NoError(t, err).Required()
		resolved := out["related"].(map[string]any)
		gt.Value(t, resolved["id"]).Equal(priv)
		gt.Value(t, resolved["available"]).Equal(false)
	})
}

// failCreateRepo wraps a real Repository but swaps the Case repository for one
// whose Create always fails, so a test can exercise the "root posted, then case
// persistence failed" branch without a real infrastructure fault.
type failCreateRepo struct {
	interfaces.Repository
	caseRepo interfaces.CaseRepository
}

func (r *failCreateRepo) Case() interfaces.CaseRepository { return r.caseRepo }

type failCreateCaseRepo struct {
	interfaces.CaseRepository
	err error
}

func (r *failCreateCaseRepo) Create(_ context.Context, _ string, _ *model.Case) (*model.Case, error) {
	return nil, r.err
}

// threadModeWorkspace builds a thread-mode WorkspaceEntry bound to C-MONITOR with
// a two-column board (initial "triage") and a single select field "severity".
func threadModeWorkspace(t *testing.T) *model.WorkspaceEntry {
	t.Helper()
	set, err := model.NewActionStatusSet("triage", []string{"done"}, []model.ActionStatusDefinition{
		{ID: "triage", Name: "Triage"},
		{ID: "done", Name: "Done"},
	})
	gt.NoError(t, err).Required()
	return &model.WorkspaceEntry{
		Workspace:             model.Workspace{ID: "support"},
		CaseMode:              model.CaseModeThread,
		SlackMonitorChannelID: "C-MONITOR",
		CaseStatusSet:         set,
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect, Options: []config.FieldOption{{ID: "high", Name: "High"}, {ID: "low", Name: "Low"}}},
			},
		},
	}
}

// TestCaseUseCase_CreateCase_ThreadMode covers the invariant that a thread-mode
// workspace never gets a dedicated Slack channel from the shared CreateCase
// funnel: instead the case is bound to a fresh root posted into the monitored
// channel (mirroring the reaction cross-channel path), and the web form's
// assignees / isPrivate / isTest are ignored.
func TestCaseUseCase_CreateCase_ThreadMode(t *testing.T) {
	t.Run("routes to monitored-channel thread and creates no dedicated channel", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))

		createChannelCalled := false
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, _ int64, _ string, _ string) (string, error) {
				createChannelCalled = true
				return "C-SHOULD-NOT-HAPPEN", nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "https://app.example.test")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		fields := map[string]model.FieldValue{"severity": {FieldID: "severity", Value: "high"}}
		// isPrivate is false here: private is rejected in thread mode (covered by
		// TestCaseUseCase_PrivateCaseThreadModeGuard). assignees + isTest are
		// passed to prove they are ignored, not persisted.
		created, err := uc.CreateCase(ctx, "support", "Phishing report", "user got a suspicious email",
			[]string{"U-ASSIGNEE"}, fields, false /*isPrivate*/, true /*isTest*/, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created).NotNil().Required()

		// Bound to the monitored channel thread; no dedicated channel.
		gt.Bool(t, createChannelCalled).False()
		gt.Value(t, created.SlackChannelID).Equal("C-MONITOR")
		gt.Value(t, created.SlackThreadTS).Equal("1234567890.123456")
		gt.Value(t, created.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, created.BoardStatus).Equal("triage")
		gt.Value(t, created.ReporterID).Equal("U-WEB-CREATOR")
		gt.Value(t, created.Title).Equal("Phishing report")
		gt.Value(t, created.Description).Equal("user got a suspicious email")
		gt.Value(t, created.FieldValues["severity"].Value).Equal("high")

		// assignees + isTest are silently ignored (thread cases do not carry them);
		// isPrivate stays false because a private thread-mode case is rejected, never
		// created (see TestCaseUseCase_PrivateCaseThreadModeGuard).
		gt.Array(t, created.AssigneeIDs).Length(0)
		gt.Bool(t, created.IsPrivate).False()
		gt.Bool(t, created.IsTest).False()

		// A placeholder root was posted to the monitored channel, then replaced in
		// place with the case summary (the summary carries the web link).
		gt.Array(t, slackMock.postedChannelIDs).Length(1)
		gt.Value(t, slackMock.postedChannelIDs[0]).Equal("C-MONITOR")
		gt.Array(t, slackMock.updatedChannelIDs).Length(1)
		gt.Value(t, slackMock.updatedChannelIDs[0]).Equal("C-MONITOR")

		// Persisted state matches what was returned (round-trip through the repo).
		reloaded, err := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, reloaded.SlackChannelID).Equal("C-MONITOR")
		gt.Value(t, reloaded.SlackThreadTS).Equal("1234567890.123456")
		gt.Value(t, reloaded.BoardStatus).Equal("triage")
		gt.Value(t, reloaded.ReporterID).Equal("U-WEB-CREATOR")
		gt.Value(t, reloaded.FieldValues["severity"].Value).Equal("high")

		// And it is discoverable by its thread binding.
		byThread, err := repo.Case().GetBySlackThread(ctx, "support", "C-MONITOR", "1234567890.123456")
		gt.NoError(t, err).Required()
		gt.Value(t, byThread).NotNil().Required()
		gt.Value(t, byThread.ID).Equal(created.ID)
	})

	t.Run("field validation failure returns error and posts no root", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		slackMock := &mockSlackService{}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		// "critical" is not an allowed severity option; the gate must reject it
		// BEFORE any Slack side effect, so the monitored channel stays clean.
		fields := map[string]model.FieldValue{"severity": {FieldID: "severity", Value: "critical"}}
		created, err := uc.CreateCase(ctx, "support", "Bad fields", "", nil, fields, false, false, "", "")
		gt.Error(t, err)
		gt.Value(t, created).Nil()
		gt.Array(t, slackMock.postedChannelIDs).Length(0)
		gt.Array(t, slackMock.updatedChannelIDs).Length(0)
	})

	t.Run("thread-mode without Slack falls through to a plain create", func(t *testing.T) {
		// A thread-mode workspace whose deployment has no Slack service (the e2e
		// harness, or local dev without a bot token) cannot post a monitored-channel
		// root. It falls through to the channel producer, but activation is a no-op
		// without Slack — so a plain OPEN case is persisted with NO dedicated channel
		// and NO thread binding. The "thread mode never provisions a channel"
		// invariant still holds, and these Slack-less environments keep working.
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		uc := usecase.NewCaseUseCase(repo, registry, nil /*no slack*/, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		created, err := uc.CreateCase(ctx, "support", "No slack here", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created).NotNil().Required()
		gt.Value(t, created.Status).Equal(types.CaseStatusOpen)
		gt.Value(t, created.SlackChannelID).Equal("")
		gt.Value(t, created.SlackThreadTS).Equal("")

		// Exactly one case was persisted; no dedicated channel was created.
		cases, listErr := repo.Case().List(ctx, "support")
		gt.NoError(t, listErr).Required()
		gt.Array(t, cases).Length(1)
	})

	t.Run("channel-mode still creates a dedicated channel (no thread routing)", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "support"},
			CaseMode:  model.CaseModeChannel,
		})

		createChannelCalled := false
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, _ int64, caseName string, _ string) (string, error) {
				createChannelCalled = true
				return "C-DEDICATED", nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		created, err := uc.CreateCase(ctx, "support", "Channel case", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, created).NotNil().Required()
		gt.Bool(t, createChannelCalled).True()
		gt.Value(t, created.SlackChannelID).Equal("C-DEDICATED")
		gt.Value(t, created.SlackThreadTS).Equal("")
	})

	t.Run("case persistence failure after root post replaces root with a fallback note", func(t *testing.T) {
		base := memory.New()
		repo := &failCreateRepo{
			Repository: base,
			caseRepo:   &failCreateCaseRepo{CaseRepository: base.Case(), err: errors.New("firestore write failed")},
		}
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		slackMock := &mockSlackService{}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		fields := map[string]model.FieldValue{"severity": {FieldID: "severity", Value: "high"}}
		created, err := uc.CreateCase(ctx, "support", "Will fail to persist", "", nil, fields, false, false, "", "")
		gt.Error(t, err)
		gt.Value(t, created).Nil()

		// The placeholder root was posted, then rewritten in place to an honest
		// failure note rather than left implying work is ongoing.
		gt.Array(t, slackMock.postedChannelIDs).Length(1)
		gt.Value(t, slackMock.postedChannelIDs[0]).Equal("C-MONITOR")
		gt.Array(t, slackMock.updatedChannelIDs).Length(1)
		gt.Value(t, slackMock.updatedChannelIDs[0]).Equal("C-MONITOR")
		gt.String(t, slackMock.updatedTexts[0]).Contains("⚠️")
	})

	t.Run("empty title is rejected before any Slack side effect", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		slackMock := &mockSlackService{}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		created, err := uc.CreateCase(ctx, "support", "" /*title*/, "desc", nil, nil, false, false, "", "")
		gt.Error(t, err)
		gt.Value(t, created).Nil()
		gt.Array(t, slackMock.postedChannelIDs).Length(0)
	})

	t.Run("same requestKey dedups a redelivered submission", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		slackMock := &mockSlackService{}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		first, err := uc.CreateCase(ctx, "support", "Duplicate submit", "", nil, nil, false, false, "", "req-1")
		gt.NoError(t, err).Required()
		gt.Value(t, first).NotNil().Required()

		// Second delivery of the same submission returns the same case and posts
		// no second root.
		again, err := uc.CreateCase(ctx, "support", "Duplicate submit", "", nil, nil, false, false, "", "req-1")
		gt.NoError(t, err).Required()
		gt.Value(t, again.ID).Equal(first.ID)
		gt.Array(t, slackMock.postedChannelIDs).Length(1)
	})

	t.Run("unknown workspace fails closed instead of creating a channel", func(t *testing.T) {
		repo := memory.New()
		// Registry is non-nil but does NOT contain "support": the mode cannot be
		// resolved, so creation must error rather than fall through to the
		// channel-creating path.
		registry := model.NewWorkspaceRegistry()
		createChannelCalled := false
		slackMock := &mockSlackService{
			createChannelFn: func(_ context.Context, _ int64, _ string, _ string) (string, error) {
				createChannelCalled = true
				return "C-NOPE", nil
			},
		}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		created, err := uc.CreateCase(ctx, "support", "Title", "", nil, nil, false, false, "", "")
		gt.Error(t, err)
		gt.Value(t, created).Nil()
		gt.Bool(t, createChannelCalled).False()
		gt.Array(t, slackMock.postedChannelIDs).Length(0)
	})

	t.Run("placeholder falls back to a thread reply when the update also fails", func(t *testing.T) {
		base := memory.New()
		repo := &failCreateRepo{
			Repository: base,
			caseRepo:   &failCreateCaseRepo{CaseRepository: base.Case(), err: errors.New("firestore write failed")},
		}
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		slackMock := &mockSlackService{
			updateMessageFn: func(_ context.Context, _ string, _ string, _ []goslack.Block, _ string) error {
				return errors.New("update failed too")
			},
		}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-WEB-CREATOR"})

		fields := map[string]model.FieldValue{"severity": {FieldID: "severity", Value: "high"}}
		created, err := uc.CreateCase(ctx, "support", "Will fail", "", nil, fields, false, false, "", "")
		gt.Error(t, err)
		gt.Value(t, created).Nil()

		// Root posted, update attempted and failed, so the failure note is delivered
		// as a threaded reply rather than leaving the placeholder stuck.
		gt.Array(t, slackMock.postedChannelIDs).Length(1)
		gt.Array(t, slackMock.updatedChannelIDs).Length(1)
		gt.Array(t, slackMock.threadReplyChannelIDs).Length(1)
		gt.Value(t, slackMock.threadReplyChannelIDs[0]).Equal("C-MONITOR")
		gt.String(t, slackMock.threadReplyTexts[0]).Contains("⚠️")
	})
}

// TestCaseUseCase_SubmitDraft_ThreadMode covers draft promotion in a thread-mode
// workspace: the promoted case binds to the monitored-channel thread (a root is
// posted there) and never provisions a dedicated Slack channel — the same
// invariant CreateCase honours through the shared openInWorkspaceMode funnel.
func TestCaseUseCase_SubmitDraft_ThreadMode(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(threadModeWorkspace(t))

	createChannelCalled := false
	slackMock := &mockSlackService{
		createChannelFn: func(_ context.Context, _ int64, _ string, _ string) (string, error) {
			createChannelCalled = true
			return "C-SHOULD-NOT-HAPPEN", nil
		},
	}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "https://app.example.test")
	reporter := fmt.Sprintf("U-REP-%d", time.Now().UnixNano())
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: reporter})

	draft, err := uc.CreateDraft(ctx, "support", "Draft to submit", "body text", nil, nil, false /*isPrivate*/, false /*isTest*/)
	gt.NoError(t, err).Required()
	gt.Value(t, draft.Status).Equal(types.CaseStatusDraft)

	submitted, err := uc.SubmitDraft(ctx, "support", draft.ID, nil)
	gt.NoError(t, err).Required()
	gt.Value(t, submitted).NotNil().Required()

	// No dedicated channel; the case is bound to the monitored-channel thread.
	gt.Bool(t, createChannelCalled).False()
	gt.Value(t, submitted.SlackChannelID).Equal("C-MONITOR")
	gt.Value(t, submitted.SlackThreadTS).Equal("1234567890.123456")
	gt.Value(t, submitted.Status).Equal(types.CaseStatusOpen)
	gt.Value(t, submitted.BoardStatus).Equal("triage")

	// A single root was posted to the monitored channel (then updated in place).
	gt.Array(t, slackMock.postedChannelIDs).Length(1)
	gt.Value(t, slackMock.postedChannelIDs[0]).Equal("C-MONITOR")
	// The placeholder root is replaced in place with the case summary — the only
	// part of this flow the user actually sees.
	gt.Array(t, slackMock.updatedChannelIDs).Length(1)
	gt.Value(t, slackMock.updatedChannelIDs[0]).Equal("C-MONITOR")

	// Persisted state round-trips through the repository.
	reloaded, err := repo.Case().Get(ctx, "support", draft.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, reloaded.Status).Equal(types.CaseStatusOpen)
	gt.Value(t, reloaded.SlackChannelID).Equal("C-MONITOR")
	gt.Value(t, reloaded.SlackThreadTS).Equal("1234567890.123456")
	gt.Value(t, reloaded.BoardStatus).Equal("triage")

	// Discoverable by its thread binding.
	byThread, err := repo.Case().GetBySlackThread(ctx, "support", "C-MONITOR", "1234567890.123456")
	gt.NoError(t, err).Required()
	gt.Value(t, byThread).NotNil().Required()
	gt.Value(t, byThread.ID).Equal(submitted.ID)
}

// TestCaseUseCase_SubmitDraft_ThreadMode_PrivateRejected covers a private draft
// that predates the workspace's thread mode: submitting it must fail closed
// (private cases are unsupported in thread mode) and roll the case back to
// DRAFT with no Slack binding, so the saved work survives for a retry.
func TestCaseUseCase_SubmitDraft_ThreadMode_PrivateRejected(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(threadModeWorkspace(t))

	createChannelCalled := false
	slackMock := &mockSlackService{
		createChannelFn: func(_ context.Context, _ int64, _ string, _ string) (string, error) {
			createChannelCalled = true
			return "C-SHOULD-NOT-HAPPEN", nil
		},
	}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")

	reporter := fmt.Sprintf("U-REP-%d", time.Now().UnixNano())
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: reporter})

	// Construct the private draft directly via the repository: CreateDraft would
	// itself reject a private request in a thread-mode workspace, so this
	// simulates a draft saved before the mode switch.
	created, err := repo.Case().Create(ctx, "support", &model.Case{
		Title:      "Private predates thread mode",
		ReporterID: reporter,
		Status:     types.CaseStatusDraft,
		IsPrivate:  true,
	})
	gt.NoError(t, err).Required()

	_, err = uc.SubmitDraft(ctx, "support", created.ID, nil)
	gt.Error(t, err).Is(usecase.ErrCasePrivateThreadModeUnsupported)
	gt.Error(t, err).Is(usecase.ErrActivationFailed)

	// Rolled back to DRAFT with no Slack binding.
	reloaded, err := repo.Case().Get(ctx, "support", created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, reloaded.Status).Equal(types.CaseStatusDraft)
	gt.Value(t, reloaded.SlackChannelID).Equal("")
	gt.Value(t, reloaded.SlackThreadTS).Equal("")

	// No dedicated channel and no monitored-channel root were created.
	gt.Bool(t, createChannelCalled).False()
	gt.Array(t, slackMock.postedChannelIDs).Length(0)
}

// TestCaseUseCase_SubmitDraft_ThreadMode_RollbackOnFailure covers a thread-mode
// activation failure (the monitored-channel root post fails): SubmitDraft must
// return an error and roll the draft back to DRAFT with no Slack binding.
func TestCaseUseCase_SubmitDraft_ThreadMode_RollbackOnFailure(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(threadModeWorkspace(t))
	slackMock := &mockSlackService{
		postMessageFn: func(_ context.Context, _ string, _ []goslack.Block, _ string) (string, error) {
			return "", errors.New("slack post failed")
		},
	}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")

	reporter := fmt.Sprintf("U-REP-%d", time.Now().UnixNano())
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: reporter})

	draft, err := uc.CreateDraft(ctx, "support", "Draft that fails activation", "body", nil, nil, false, false)
	gt.NoError(t, err).Required()

	_, err = uc.SubmitDraft(ctx, "support", draft.ID, nil)
	gt.Error(t, err).Is(usecase.ErrActivationFailed)

	reloaded, err := repo.Case().Get(ctx, "support", draft.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, reloaded.Status).Equal(types.CaseStatusDraft)
	gt.Value(t, reloaded.SlackChannelID).Equal("")
	gt.Value(t, reloaded.SlackThreadTS).Equal("")
}

// TestCaseUseCase_SubmitDraft_ChannelMode_StillCreatesChannel is the regression
// guard for the unchanged channel-mode submit path: it still provisions a
// dedicated Slack channel and binds the case to it.
func TestCaseUseCase_SubmitDraft_ChannelMode_StillCreatesChannel(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "support"},
		CaseMode:  model.CaseModeChannel,
	})
	createChannelCalled := false
	slackMock := &mockSlackService{
		createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
			createChannelCalled = true
			return fmt.Sprintf("C-DEDICATED-%d", caseID), nil
		},
	}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")

	reporter := fmt.Sprintf("U-REP-%d", time.Now().UnixNano())
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: reporter})

	draft, err := uc.CreateDraft(ctx, "support", "Channel draft", "body", nil, nil, false, false)
	gt.NoError(t, err).Required()

	submitted, err := uc.SubmitDraft(ctx, "support", draft.ID, nil)
	gt.NoError(t, err).Required()
	gt.Bool(t, createChannelCalled).True()
	gt.Value(t, submitted.Status).Equal(types.CaseStatusOpen)
	gt.Value(t, submitted.SlackChannelID).Equal(fmt.Sprintf("C-DEDICATED-%d", draft.ID))
	gt.Value(t, submitted.SlackThreadTS).Equal("")

	reloaded, err := repo.Case().Get(ctx, "support", draft.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, reloaded.SlackChannelID).Equal(fmt.Sprintf("C-DEDICATED-%d", draft.ID))
}

// ambiguousUpdateRepo wraps a Repository so the thread-binding Update (the one
// carrying a non-empty SlackThreadTS) persists to the underlying store but then
// returns an error — simulating a write that committed server-side while its
// response was lost. It is what makes the SubmitDraft rollback's snapshot
// restore observable: without it, the leaked thread binding would survive.
type ambiguousUpdateRepo struct {
	interfaces.Repository
	caseRepo interfaces.CaseRepository
}

func (r *ambiguousUpdateRepo) Case() interfaces.CaseRepository { return r.caseRepo }

type ambiguousUpdateCaseRepo struct {
	interfaces.CaseRepository
}

func (r *ambiguousUpdateCaseRepo) Update(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error) {
	updated, err := r.CaseRepository.Update(ctx, workspaceID, c)
	if err == nil && c.SlackThreadTS != "" {
		// The binding was written to the store; report a lost-response error.
		return updated, errors.New("update committed but response lost")
	}
	return updated, err
}

// TestCaseUseCase_SubmitDraft_ThreadMode_RollbackClearsLeakedBinding verifies the
// rollback restores the pre-activation snapshot when the thread-binding Update
// committed server-side but returned an error: the draft must return to DRAFT
// with NO Slack binding, so no thread-bound DRAFT is left behind for a retry.
func TestCaseUseCase_SubmitDraft_ThreadMode_RollbackClearsLeakedBinding(t *testing.T) {
	mem := memory.New()
	repo := &ambiguousUpdateRepo{Repository: mem, caseRepo: &ambiguousUpdateCaseRepo{CaseRepository: mem.Case()}}
	registry := model.NewWorkspaceRegistry()
	registry.Register(threadModeWorkspace(t))
	slackMock := &mockSlackService{}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")

	reporter := fmt.Sprintf("U-REP-%d", time.Now().UnixNano())
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: reporter})

	draft, err := uc.CreateDraft(ctx, "support", "Draft to submit", "body", nil, nil, false, false)
	gt.NoError(t, err).Required()

	_, err = uc.SubmitDraft(ctx, "support", draft.ID, nil)
	gt.Error(t, err).Is(usecase.ErrActivationFailed)

	// The thread-binding Update committed server-side despite the returned error;
	// the snapshot-based rollback must clear it, not leave a thread-bound DRAFT.
	reloaded, err := repo.Case().Get(ctx, "support", draft.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, reloaded.Status).Equal(types.CaseStatusDraft)
	gt.Value(t, reloaded.SlackChannelID).Equal("")
	gt.Value(t, reloaded.SlackThreadTS).Equal("")
}

// TestCaseUseCase_CreateCase_ThreadModeMonitorUnconfigured verifies the kept
// fail-closed branch: a thread-mode workspace that HAS a Slack service but no
// monitored channel cannot anchor a case, so it fails closed rather than falling
// through to the channel producer, which — with Slack present — would provision
// a dedicated channel and break the invariant. (The Slack-absent case instead
// falls through to a plain create; see the ThreadMode subtests above.)
func TestCaseUseCase_CreateCase_ThreadModeMonitorUnconfigured(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "support"},
		CaseMode:  model.CaseModeThread,
		// SlackMonitorChannelID intentionally left empty (misconfiguration).
	})
	createChannelCalled := false
	slackMock := &mockSlackService{
		createChannelFn: func(_ context.Context, _ int64, _ string, _ string) (string, error) {
			createChannelCalled = true
			return "C-SHOULD-NOT-HAPPEN", nil
		},
	}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CREATOR"})

	created, err := uc.CreateCase(ctx, "support", "No monitor channel", "", nil, nil, false, false, "", "")
	gt.Error(t, err).Is(usecase.ErrThreadModeSlackUnconfigured)
	gt.Value(t, created).Nil()
	gt.Bool(t, createChannelCalled).False()

	cases, listErr := repo.Case().List(ctx, "support")
	gt.NoError(t, listErr).Required()
	gt.Array(t, cases).Length(0)
}

// archiveTestCtx is the actor context every archive test runs under. Archiving
// is a Web-only operation, so the real path always carries an auth token.
func archiveTestCtx() context.Context {
	return auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-ARCHIVER"})
}

// seedCase creates a case directly through the repository so a test can put it
// in any lifecycle / archive state without walking the creation flow.
func seedArchiveCase(t *testing.T, repo *memory.Repository, c *model.Case) *model.Case {
	t.Helper()
	if c.ReporterID == "" {
		c.ReporterID = "U-REPORTER"
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = now
	}
	created, err := repo.Case().Create(context.Background(), testWorkspaceID, c)
	gt.NoError(t, err).Required()
	return created
}

func TestCaseUseCase_ArchiveCase(t *testing.T) {
	t.Run("archives a closed case without changing its status", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		before := time.Now().UTC().Add(-time.Hour)
		c := seedArchiveCase(t, repo, &model.Case{
			Title:     "Closed case",
			Status:    types.CaseStatusClosed,
			UpdatedAt: before,
		})

		archived, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived.IsArchived()).True()
		gt.Value(t, archived.Status).Equal(types.CaseStatusClosed)
		gt.Bool(t, archived.UpdatedAt.After(before)).True()

		stored, err := repo.Case().Get(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.ArchivedAt).NotNil().Required()
		gt.Bool(t, stored.ArchivedAt.Equal(*archived.ArchivedAt)).True()
		gt.Value(t, stored.Status).Equal(types.CaseStatusClosed)
	})

	t.Run("rejects an open case and leaves it untouched", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c := seedArchiveCase(t, repo, &model.Case{Title: "Open case", Status: types.CaseStatusOpen})

		_, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotClosed)

		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
		gt.Value(t, stored.Status).Equal(types.CaseStatusOpen)
	})

	t.Run("rejects a draft case and leaves it untouched", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c := seedArchiveCase(t, repo, &model.Case{Title: "Draft case", Status: types.CaseStatusDraft})

		_, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotClosed)

		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
		gt.Value(t, stored.Status).Equal(types.CaseStatusDraft)
	})

	t.Run("rejects an already archived case without overwriting the timestamp", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c := seedArchiveCase(t, repo, &model.Case{Title: "Closed case", Status: types.CaseStatusClosed})
		first, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()

		_, err = uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrCaseAlreadyArchived)

		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Value(t, stored.ArchivedAt).NotNil().Required()
		gt.Bool(t, stored.ArchivedAt.Equal(*first.ArchivedAt)).True()
	})

	t.Run("reports a missing case as not found", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		_, err := uc.ArchiveCase(archiveTestCtx(), testWorkspaceID, 4242)
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("denies a non-member of a private case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		c := seedArchiveCase(t, repo, &model.Case{
			Title:          "Private closed case",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
		})

		outsiderCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-OUTSIDER"})
		_, err := uc.ArchiveCase(outsiderCtx, testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	t.Run("allows a member of a private case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		c := seedArchiveCase(t, repo, &model.Case{
			Title:          "Private closed case",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
		})

		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-MEMBER"})
		archived, err := uc.ArchiveCase(memberCtx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived.IsArchived()).True()
	})

	// A context with no token is a system / agent context. Project policy is to
	// bypass the private-case check there rather than fail, so the GraphQL
	// resolver is what makes a token mandatory for the user-facing path.
	t.Run("bypasses the private check when the context carries no token", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		c := seedArchiveCase(t, repo, &model.Case{
			Title:          "Private closed case",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
		})

		archived, err := uc.ArchiveCase(context.Background(), testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived.IsArchived()).True()
	})
}

func TestCaseUseCase_UnarchiveCase(t *testing.T) {
	t.Run("restores an archived case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c := seedArchiveCase(t, repo, &model.Case{Title: "Closed case", Status: types.CaseStatusClosed})
		archived, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()

		restored, err := uc.UnarchiveCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, restored.ArchivedAt).Nil()
		gt.Bool(t, restored.IsArchived()).False()
		gt.Value(t, restored.Status).Equal(types.CaseStatusClosed)
		gt.Bool(t, restored.UpdatedAt.After(archived.UpdatedAt) ||
			restored.UpdatedAt.Equal(archived.UpdatedAt)).True()

		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Value(t, stored.ArchivedAt).Nil()
	})

	t.Run("rejects a case that is not archived", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		c := seedArchiveCase(t, repo, &model.Case{Title: "Closed case", Status: types.CaseStatusClosed})

		_, err := uc.UnarchiveCase(archiveTestCtx(), testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrCaseNotArchived)
	})

	t.Run("denies a non-member of a private case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		archivedAt := time.Now().UTC()
		c := seedArchiveCase(t, repo, &model.Case{
			Title:          "Private archived case",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
			ArchivedAt:     &archivedAt,
		})

		outsiderCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-OUTSIDER"})
		_, err := uc.UnarchiveCase(outsiderCtx, testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		stored, getErr := repo.Case().Get(context.Background(), testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).True()
	})
}

func TestCaseUseCase_ArchiveCase_SlackNotification(t *testing.T) {
	t.Run("posts one context line into the thread of a thread-mode case", func(t *testing.T) {
		ctx := archiveTestCtx()
		uc, slackMock, created, _ := newThreadNotifyCase(t, ctx)

		// A thread-mode case closes by moving to a closed board status; drive
		// the real path so the case reaches CLOSED the way production does.
		closed, _, err := uc.UpdateCaseStatus(ctx, "support", created.ID, "done")
		gt.NoError(t, err).Required()
		gt.Value(t, closed.Status).Equal(types.CaseStatusClosed)
		slackMock.threadCalls = nil

		archived, err := uc.ArchiveCase(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived.IsArchived()).True()

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		call := slackMock.threadCalls[0]
		gt.Value(t, call.channelID).Equal("C-MONITOR")
		gt.Value(t, call.threadTS).Equal(created.SlackThreadTS)
		gt.String(t, call.text).Contains("archived this case")
		gt.String(t, call.text).Contains("U-ARCHIVER")
	})

	t.Run("posts one context line on unarchive", func(t *testing.T) {
		ctx := archiveTestCtx()
		uc, slackMock, created, _ := newThreadNotifyCase(t, ctx)

		_, _, err := uc.UpdateCaseStatus(ctx, "support", created.ID, "done")
		gt.NoError(t, err).Required()
		_, err = uc.ArchiveCase(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		slackMock.threadCalls = nil

		restored, err := uc.UnarchiveCase(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, restored.IsArchived()).False()

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		gt.String(t, slackMock.threadCalls[0].text).Contains("unarchived this case")
	})

	// A channel-mode case has a dedicated channel but no thread, so it gets no
	// post — the same range as every other Case change notification.
	t.Run("posts nothing for a channel-mode case", func(t *testing.T) {
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: testWorkspaceID},
			CaseMode:  model.CaseModeChannel,
		})
		slackMock := &threadNotifySlackMock{}
		uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
		ctx := archiveTestCtx()

		c := seedArchiveCase(t, repo, &model.Case{
			Title:          "Channel-mode closed case",
			Status:         types.CaseStatusClosed,
			SlackChannelID: "C-CASE",
		})

		archived, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived.IsArchived()).True()
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	// The notification is best-effort: it is reported through errutil.Handle
	// and must never roll back or fail the archive that already persisted.
	t.Run("still archives when the Slack post fails", func(t *testing.T) {
		ctx := archiveTestCtx()
		uc, slackMock, created, repo := newThreadNotifyCase(t, ctx)

		_, _, err := uc.UpdateCaseStatus(ctx, "support", created.ID, "done")
		gt.NoError(t, err).Required()
		slackMock.threadCalls = nil
		slackMock.postThreadErr = errors.New("slack is down")

		archived, err := uc.ArchiveCase(ctx, "support", created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived.IsArchived()).True()
		gt.Array(t, slackMock.threadCalls).Length(1)

		stored, getErr := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).True()
	})
}

func TestCaseUseCase_BulkArchiveCases(t *testing.T) {
	t.Run("archives every closed case in the batch", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed})
		c2 := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed})

		archived, err := uc.BulkArchiveCases(ctx, testWorkspaceID, []int64{c1.ID, c2.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(2).Required()
		for _, c := range archived {
			gt.Bool(t, c.IsArchived()).True()
		}
	})

	t.Run("skips an id that is already archived", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed})
		c2 := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed})
		c3 := seedArchiveCase(t, repo, &model.Case{Title: "C", Status: types.CaseStatusClosed})
		_, err := uc.ArchiveCase(ctx, testWorkspaceID, c2.ID)
		gt.NoError(t, err).Required()

		archived, err := uc.BulkArchiveCases(ctx, testWorkspaceID, []int64{c1.ID, c2.ID, c3.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(2).Required()

		gotIDs := map[int64]bool{}
		for _, c := range archived {
			gotIDs[c.ID] = true
		}
		gt.Bool(t, gotIDs[c1.ID]).True()
		gt.Bool(t, gotIDs[c2.ID]).False()
		gt.Bool(t, gotIDs[c3.ID]).True()

		// The skipped case is still archived, just not newly so.
		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, c2.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).True()
	})

	// The selection is made against a snapshot of the Closed tab, so someone
	// else reopening one of the rows in between must not fail the whole batch.
	t.Run("skips an id that is no longer closed", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed})
		reopened := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusOpen})
		c3 := seedArchiveCase(t, repo, &model.Case{Title: "C", Status: types.CaseStatusClosed})

		archived, err := uc.BulkArchiveCases(ctx, testWorkspaceID, []int64{c1.ID, reopened.ID, c3.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(2).Required()

		gotIDs := map[int64]bool{}
		for _, c := range archived {
			gotIDs[c.ID] = true
		}
		gt.Bool(t, gotIDs[c1.ID]).True()
		gt.Bool(t, gotIDs[reopened.ID]).False()
		gt.Bool(t, gotIDs[c3.ID]).True()

		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, reopened.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).False()
	})

	// Only staleness is skipped. Access denial is the one outcome an operator
	// needs to see, so it must reach the caller rather than being swallowed.
	t.Run("propagates an access denial", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed})
		private := seedArchiveCase(t, repo, &model.Case{
			Title:          "Private",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
		})

		outsiderCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-OUTSIDER"})
		_, err := uc.BulkArchiveCases(outsiderCtx, testWorkspaceID, []int64{c1.ID, private.ID})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	// A row the user selected and someone else deleted in between is the list
	// going stale, like the two cases above — not a fault that should leave
	// the rest of a large batch unprocessed.
	t.Run("skips an id whose case no longer exists", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed})
		c2 := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed})

		archived, err := uc.BulkArchiveCases(ctx, testWorkspaceID, []int64{c1.ID, 9999, c2.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(2).Required()

		// The ids after the missing one are still processed.
		for _, id := range []int64{c1.ID, c2.ID} {
			stored, getErr := repo.Case().Get(ctx, testWorkspaceID, id)
			gt.NoError(t, getErr).Required()
			gt.Bool(t, stored.IsArchived()).True()
		}
	})

	t.Run("an empty batch returns an empty non-nil slice", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		archived, err := uc.BulkArchiveCases(archiveTestCtx(), testWorkspaceID, nil)
		gt.NoError(t, err).Required()
		gt.Bool(t, archived == nil).False()
		gt.Array(t, archived).Length(0)
	})
}

func TestCaseUseCase_BulkUnarchiveCases(t *testing.T) {
	t.Run("restores every archived case in the batch", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		archivedAt := time.Now().UTC()
		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed, ArchivedAt: &archivedAt})
		c2 := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed, ArchivedAt: &archivedAt})

		restored, err := uc.BulkUnarchiveCases(ctx, testWorkspaceID, []int64{c1.ID, c2.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, restored).Length(2).Required()
		for _, c := range restored {
			gt.Bool(t, c.IsArchived()).False()
		}
	})

	t.Run("skips an id that is not archived", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		archivedAt := time.Now().UTC()
		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed, ArchivedAt: &archivedAt})
		active := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed})

		restored, err := uc.BulkUnarchiveCases(ctx, testWorkspaceID, []int64{c1.ID, active.ID})
		gt.NoError(t, err).Required()
		gt.Array(t, restored).Length(1).Required()
		gt.Value(t, restored[0].ID).Equal(c1.ID)
	})

	t.Run("propagates an access denial", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		archivedAt := time.Now().UTC()
		private := seedArchiveCase(t, repo, &model.Case{
			Title:          "Private",
			Status:         types.CaseStatusClosed,
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-MEMBER"},
			ArchivedAt:     &archivedAt,
		})

		outsiderCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-OUTSIDER"})
		_, err := uc.BulkUnarchiveCases(outsiderCtx, testWorkspaceID, []int64{private.ID})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	t.Run("an empty batch returns an empty non-nil slice", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")

		restored, err := uc.BulkUnarchiveCases(archiveTestCtx(), testWorkspaceID, nil)
		gt.NoError(t, err).Required()
		gt.Bool(t, restored == nil).False()
		gt.Array(t, restored).Length(0)
	})
}

func TestCaseUseCase_BulkArchiveCasesAsync(t *testing.T) {
	t.Run("archives the whole batch after the async tail drains", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed})
		c2 := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed})

		uc.BulkArchiveCasesAsync(ctx, testWorkspaceID, []int64{c1.ID, c2.ID})
		async.Wait()

		for _, id := range []int64{c1.ID, c2.ID} {
			stored, err := repo.Case().Get(ctx, testWorkspaceID, id)
			gt.NoError(t, err).Required()
			gt.Bool(t, stored.IsArchived()).True()
		}
	})

	t.Run("restores the whole batch after the async tail drains", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		archivedAt := time.Now().UTC()
		c1 := seedArchiveCase(t, repo, &model.Case{Title: "A", Status: types.CaseStatusClosed, ArchivedAt: &archivedAt})
		c2 := seedArchiveCase(t, repo, &model.Case{Title: "B", Status: types.CaseStatusClosed, ArchivedAt: &archivedAt})

		uc.BulkUnarchiveCasesAsync(ctx, testWorkspaceID, []int64{c1.ID, c2.ID})
		async.Wait()

		for _, id := range []int64{c1.ID, c2.ID} {
			stored, err := repo.Case().Get(ctx, testWorkspaceID, id)
			gt.NoError(t, err).Required()
			gt.Bool(t, stored.IsArchived()).False()
		}
	})
}

func TestCaseUseCase_ArchivedCaseCannotReturnToOpen(t *testing.T) {
	t.Run("ReopenCase rejects an archived case", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		ctx := archiveTestCtx()

		c := seedArchiveCase(t, repo, &model.Case{Title: "Closed case", Status: types.CaseStatusClosed})
		_, err := uc.ArchiveCase(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, err).Required()

		_, err = uc.ReopenCase(ctx, testWorkspaceID, c.ID)
		gt.Error(t, err).Is(usecase.ErrCaseArchived)

		stored, getErr := repo.Case().Get(ctx, testWorkspaceID, c.ID)
		gt.NoError(t, getErr).Required()
		gt.Value(t, stored.Status).Equal(types.CaseStatusClosed)
		gt.Bool(t, stored.IsArchived()).True()
	})

	t.Run("UpdateCaseStatus rejects a move to an open column", func(t *testing.T) {
		ctx := archiveTestCtx()
		uc, _, created, repo := newThreadNotifyCase(t, ctx)

		_, _, err := uc.UpdateCaseStatus(ctx, "support", created.ID, "done")
		gt.NoError(t, err).Required()
		_, err = uc.ArchiveCase(ctx, "support", created.ID)
		gt.NoError(t, err).Required()

		_, _, err = uc.UpdateCaseStatus(ctx, "support", created.ID, "triage")
		gt.Error(t, err).Is(usecase.ErrCaseArchived)

		stored, getErr := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, getErr).Required()
		gt.Value(t, stored.BoardStatus).Equal("done")
		gt.Value(t, stored.Status).Equal(types.CaseStatusClosed)
	})

	// The guard is on "is archived", not on "is the target column open": an
	// archived case takes no board writes at all.
	t.Run("UpdateCaseStatus rejects a move between closed columns", func(t *testing.T) {
		ctx := archiveTestCtx()
		uc, _, created, repo := newThreadNotifyCase(t, ctx)

		_, _, err := uc.UpdateCaseStatus(ctx, "support", created.ID, "done")
		gt.NoError(t, err).Required()
		_, err = uc.ArchiveCase(ctx, "support", created.ID)
		gt.NoError(t, err).Required()

		_, _, err = uc.UpdateCaseStatus(ctx, "support", created.ID, "done")
		gt.Error(t, err).Is(usecase.ErrCaseArchived)

		stored, getErr := repo.Case().Get(ctx, "support", created.ID)
		gt.NoError(t, getErr).Required()
		gt.Bool(t, stored.IsArchived()).True()
	})
}

func TestCaseUseCase_ListCases_ArchiveScope(t *testing.T) {
	repo := memory.New()
	uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	ctx := archiveTestCtx()

	open := seedArchiveCase(t, repo, &model.Case{Title: "Open", Status: types.CaseStatusOpen})
	activeClosed := seedArchiveCase(t, repo, &model.Case{Title: "Closed active", Status: types.CaseStatusClosed})
	toArchive := seedArchiveCase(t, repo, &model.Case{Title: "Closed archived", Status: types.CaseStatusClosed})
	_, err := uc.ArchiveCase(ctx, testWorkspaceID, toArchive.ID)
	gt.NoError(t, err).Required()

	idsOf := func(cases []*model.Case) map[int64]bool {
		seen := make(map[int64]bool, len(cases))
		for _, c := range cases {
			seen[c.ID] = true
		}
		return seen
	}

	t.Run("active-only hides the archived case", func(t *testing.T) {
		got, listErr := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, listErr).Required()
		seen := idsOf(got)
		gt.Bool(t, seen[open.ID]).True()
		gt.Bool(t, seen[activeClosed.ID]).True()
		gt.Bool(t, seen[toArchive.ID]).False()
	})

	t.Run("archived-only returns just the archived case", func(t *testing.T) {
		got, listErr := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeArchivedOnly)
		gt.NoError(t, listErr).Required()
		gt.Array(t, got).Length(1).Required()
		gt.Value(t, got[0].ID).Equal(toArchive.ID)
		gt.Bool(t, got[0].IsArchived()).True()
	})

	t.Run("all returns every non-draft case", func(t *testing.T) {
		got, listErr := uc.ListCases(ctx, testWorkspaceID, nil, interfaces.CaseArchiveScopeAll)
		gt.NoError(t, listErr).Required()
		seen := idsOf(got)
		gt.Bool(t, seen[open.ID]).True()
		gt.Bool(t, seen[activeClosed.ID]).True()
		gt.Bool(t, seen[toArchive.ID]).True()
	})

	t.Run("the status filter combines with the scope", func(t *testing.T) {
		closedStatus := types.CaseStatusClosed
		got, listErr := uc.ListCases(ctx, testWorkspaceID, &closedStatus, interfaces.CaseArchiveScopeActiveOnly)
		gt.NoError(t, listErr).Required()
		gt.Array(t, got).Length(1).Required()
		gt.Value(t, got[0].ID).Equal(activeClosed.ID)
	})
}

// ListReferenceableCases feeds the case_ref picker. A case that was put away
// must stop being offered as a new reference.
func TestCaseUseCase_ListReferenceableCases_ExcludesArchived(t *testing.T) {
	repo := memory.New()
	uc := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	ctx := archiveTestCtx()

	visible := seedArchiveCase(t, repo, &model.Case{Title: "Still referenceable", Status: types.CaseStatusClosed})
	hidden := seedArchiveCase(t, repo, &model.Case{Title: "Put away", Status: types.CaseStatusClosed})
	_, err := uc.ArchiveCase(ctx, testWorkspaceID, hidden.ID)
	gt.NoError(t, err).Required()

	refs, err := uc.ListReferenceableCases(ctx, testWorkspaceID, "", 50)
	gt.NoError(t, err).Required()

	seen := map[int64]bool{}
	for _, ref := range refs {
		seen[ref.ID] = true
	}
	gt.Bool(t, seen[visible.ID]).True()
	gt.Bool(t, seen[hidden.ID]).False()
}

// threadNotifyCall captures one PostThreadMessage invocation.
type threadNotifyCall struct {
	channelID string
	threadTS  string
	blocks    []goslack.Block
	text      string
	broadcast bool
}

// threadNotifySlackMock records every PostThreadMessage call so the Case change
// notification tests can assert the destination, the block shape and the body
// rather than merely that no error was returned.
type threadNotifySlackMock struct {
	mockSlackService
	postThreadErr error
	threadCalls   []threadNotifyCall
}

func (m *threadNotifySlackMock) PostThreadMessage(_ context.Context, channelID string, threadTS string, blocks []goslack.Block, text string, opts ...slack.PostThreadOption) (string, error) {
	m.threadCalls = append(m.threadCalls, threadNotifyCall{
		channelID: channelID,
		threadTS:  threadTS,
		blocks:    blocks,
		text:      text,
		broadcast: slack.ApplyPostThreadOptions(opts...).Broadcast,
	})
	if m.postThreadErr != nil {
		return "", m.postThreadErr
	}
	return "1234567890.999999", nil
}

// failUpdateCaseRepo fails every Case Update while delegating reads, so a test
// can exercise "the write failed, therefore nothing is announced".
type failUpdateCaseRepo struct {
	interfaces.CaseRepository
	err error
}

func (r *failUpdateCaseRepo) Update(_ context.Context, _ string, _ *model.Case) (*model.Case, error) {
	return nil, r.err
}

// racedCaseRepo simulates losing a race to a concurrent assignee change: a plain
// Get still returns the snapshot from before that change, while Transact hands
// the closure the committed state that already contains it. A notification
// driven by the transaction's own view stays silent here; one driven by a Get
// taken outside the transaction would wrongly announce the other request's
// change as its own.
type racedCaseRepo struct {
	interfaces.CaseRepository
	stale     *model.Case
	committed *model.Case
}

func (r *racedCaseRepo) Get(_ context.Context, _ string, _ int64) (*model.Case, error) {
	return r.stale, nil
}

func (r *racedCaseRepo) Transact(_ context.Context, _ string, _ int64, fn func(*model.Case) error) (*model.Case, error) {
	if err := fn(r.committed); err != nil {
		return nil, err
	}
	return r.committed, nil
}

// newThreadNotifyCase builds a thread-mode workspace and creates one Case
// through the real CreateCase path, then clears the Slack calls made during
// creation so each test only sees the notifications it triggers itself.
func newThreadNotifyCase(t *testing.T, ctx context.Context) (*usecase.CaseUseCase, *threadNotifySlackMock, *model.Case, *memory.Repository) {
	t.Helper()
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(threadModeWorkspace(t))
	slackMock := &threadNotifySlackMock{}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "https://app.example.test")

	created, err := uc.CreateCase(ctx, "support", "Suspicious email", "reported by a user", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	gt.Value(t, created.SlackChannelID).Equal("C-MONITOR")
	gt.String(t, created.SlackThreadTS).NotEqual("")

	slackMock.threadCalls = nil
	return uc, slackMock, created, repo
}

// assertThreadContextLine asserts the recorded call is a single unobtrusive
// context block carrying wantText, posted into the Case thread without
// reply_broadcast.
func assertThreadContextLine(t *testing.T, call threadNotifyCall, wantChannel, wantThreadTS, wantText string) {
	t.Helper()
	gt.Value(t, call.channelID).Equal(wantChannel)
	gt.Value(t, call.threadTS).Equal(wantThreadTS)
	gt.Bool(t, call.broadcast).False()
	gt.Value(t, call.text).Equal(wantText)

	gt.Array(t, call.blocks).Length(1).Required()
	ctxBlock, ok := call.blocks[0].(*goslack.ContextBlock)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, ctxBlock.ContextElements.Elements).Length(1).Required()
	textObj, ok := ctxBlock.ContextElements.Elements[0].(*goslack.TextBlockObject)
	gt.Bool(t, ok).True().Required()
	gt.Value(t, textObj.Type).Equal(goslack.MarkdownType)
	gt.Value(t, textObj.Text).Equal(wantText)
}

// A workspace that configures a language announces its Case changes in that
// language even when the caller's context carries none, which is what a web UI
// request looks like. Every case write that posts to Slack is driven once.
func TestCaseUseCase_NotificationsFollowTheWorkspaceLanguage(t *testing.T) {
	i18n.Init(i18n.LangEN)
	const actor = "<@U-ALICE>"
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-ALICE"})
	jaCtx := i18n.ContextWithLang(ctx, i18n.LangJA)

	repo := memory.New()
	entry := threadModeWorkspace(t)
	entry.Language = i18n.LangJA
	registry := model.NewWorkspaceRegistry()
	registry.Register(entry)
	slackMock := &threadNotifySlackMock{}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "https://app.example.test")
	gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
		{ID: "U-BOB", Name: "bob", RealName: "Bob"},
	})).Required()

	c, err := uc.CreateCase(ctx, "support", "Suspicious email", "reported by a user", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	// The monitored-channel anchor CreateCase posts is the first output.
	gt.Array(t, slackMock.postedTexts).Length(1).Required()
	gt.String(t, slackMock.postedTexts[0]).Equal(i18n.T(jaCtx, i18n.MsgReactionCasePlaceholder))
	slackMock.threadCalls = nil

	title := "Phishing report"
	_, err = uc.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Title: &title})
	gt.NoError(t, err).Required()
	_, err = uc.AssignCase(ctx, "support", c.ID, []string{"U-BOB"})
	gt.NoError(t, err).Required()
	_, err = uc.UnassignCase(ctx, "support", c.ID, []string{"U-BOB"})
	gt.NoError(t, err).Required()
	_, _, err = uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
	gt.NoError(t, err).Required()
	_, err = uc.ArchiveCase(ctx, "support", c.ID)
	gt.NoError(t, err).Required()
	_, err = uc.UnarchiveCase(ctx, "support", c.ID)
	gt.NoError(t, err).Required()

	want := []string{
		i18n.T(jaCtx, i18n.MsgCaseChangeTitle, actor, "Suspicious email", title),
		i18n.T(jaCtx, i18n.MsgCaseChangeAssigneeAssigned, actor, "<@U-BOB>"),
		i18n.T(jaCtx, i18n.MsgCaseChangeAssigneeUnassigned, actor, "<@U-BOB>"),
		i18n.T(jaCtx, i18n.MsgCaseChangeStatus, actor, "Triage", "Done"),
		i18n.T(jaCtx, i18n.MsgCaseChangeArchived, actor),
		i18n.T(jaCtx, i18n.MsgCaseChangeUnarchived, actor),
	}
	gt.Array(t, slackMock.threadCalls).Length(len(want)).Required()
	for i, w := range want {
		gt.String(t, slackMock.threadCalls[i].text).Equal(w)
	}
}

// A channel-mode Case's bookmark title is localized too: it is the one piece of
// copy activation writes into the new channel.
func TestCaseUseCase_ChannelBookmarkFollowsTheWorkspaceLanguage(t *testing.T) {
	i18n.Init(i18n.LangEN)
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-ALICE"})
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-ja"}, Language: i18n.LangJA})
	slackMock := &mockSlackService{}
	uc := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "https://app.example.test")

	_, err := uc.CreateCase(ctx, "ws-ja", "Bookmark case", "", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	gt.String(t, slackMock.bookmarkTitle).Equal(
		i18n.T(i18n.ContextWithLang(ctx, i18n.LangJA), i18n.MsgBookmarkOpenCase))
}

// TestCaseUseCase_ThreadChangeNotification covers the thread-mode Case change
// notifications: title, assignees and board status are announced in the Case's
// Slack thread, and nothing is announced when there is no real change, when the
// Case is channel-mode, or when the write itself failed.
func TestCaseUseCase_ThreadChangeNotification(t *testing.T) {
	i18n.Init(i18n.LangEN)
	const actor = "<@U-ALICE>"
	tokenCtx := func() context.Context {
		return auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-ALICE"})
	}

	t.Run("title change is announced in the case thread", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, ctx)

		title := "Phishing report"
		updated, err := uc.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Title: &title})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Title).Equal(title)

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
			i18n.T(ctx, i18n.MsgCaseChangeTitle, actor, "Suspicious email", title))
	})

	t.Run("re-submitting the same title announces nothing", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, ctx)

		same := c.Title
		_, err := uc.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Title: &same})
		gt.NoError(t, err).Required()
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("a description-only edit announces nothing", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, ctx)

		desc := "rewritten description"
		updated, err := uc.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Description: &desc})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Description).Equal(desc)
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("a channel-mode case announces nothing and still renames its channel", func(t *testing.T) {
		ctx := tokenCtx()
		repo := memory.New()
		renamedTo := ""
		slackMock := &threadNotifySlackMock{}
		slackMock.renameChannelFn = func(_ context.Context, _ string, _ int64, caseName string, _ string) error {
			renamedTo = caseName
			return nil
		}
		uc := usecase.NewCaseUseCase(repo, nil, slackMock, nil, "")

		created, err := uc.CreateCase(ctx, testWorkspaceID, "Channel case", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.String(t, created.SlackThreadTS).Equal("")
		slackMock.threadCalls = nil

		title := "Renamed channel case"
		_, err = uc.UpdateCase(ctx, testWorkspaceID, created.ID, usecase.CaseUpdate{Title: &title})
		gt.NoError(t, err).Required()
		gt.Value(t, renamedTo).Equal(title)
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("assigning users announces the added members only", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, repo := newThreadNotifyCase(t, ctx)
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
			{ID: "U-BOB", Name: "bob", RealName: "Bob"},
			{ID: "U-CAROL", Name: "carol", RealName: "Carol"},
		})).Required()

		updated, err := uc.AssignCase(ctx, "support", c.ID, []string{"U-BOB", "U-CAROL"})
		gt.NoError(t, err).Required()
		gt.Array(t, updated.AssigneeIDs).Length(2)

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
			i18n.T(ctx, i18n.MsgCaseChangeAssigneeAssigned, actor, "<@U-BOB>, <@U-CAROL>"))

		// Re-assigning an existing member is a no-op union: nothing new to announce.
		slackMock.threadCalls = nil
		_, err = uc.AssignCase(ctx, "support", c.ID, []string{"U-BOB"})
		gt.NoError(t, err).Required()
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("assigning nobody announces nothing", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, ctx)

		_, err := uc.AssignCase(ctx, "support", c.ID, nil)
		gt.NoError(t, err).Required()
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("unassigning announces the removed member only", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, repo := newThreadNotifyCase(t, ctx)
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
			{ID: "U-BOB", Name: "bob", RealName: "Bob"},
		})).Required()
		_, err := uc.AssignCase(ctx, "support", c.ID, []string{"U-BOB"})
		gt.NoError(t, err).Required()
		slackMock.threadCalls = nil

		updated, err := uc.UnassignCase(ctx, "support", c.ID, []string{"U-BOB"})
		gt.NoError(t, err).Required()
		gt.Array(t, updated.AssigneeIDs).Length(0)

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
			i18n.T(ctx, i18n.MsgCaseChangeAssigneeUnassigned, actor, "<@U-BOB>"))

		// Removing someone who is not assigned changes nothing.
		slackMock.threadCalls = nil
		_, err = uc.UnassignCase(ctx, "support", c.ID, []string{"U-BOB"})
		gt.NoError(t, err).Required()
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("board status change is announced with the configured display names", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, ctx)

		updated, previous, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		gt.Value(t, updated.BoardStatus).Equal("done")
		gt.Value(t, updated.Status).Equal(types.CaseStatusClosed)
		// The reported previous status is the one the announcement was derived
		// from, so a caller can reconstruct the same decision the thread line made.
		gt.Value(t, previous).Equal("triage")

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
			i18n.T(ctx, i18n.MsgCaseChangeStatus, actor, "Triage", "Done"))

		// Setting the same status again is not a change. The returned previous
		// status now equals the written one, which is the only signal such a call
		// leaves — nothing is announced in the thread.
		slackMock.threadCalls = nil
		reset, previous, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		gt.Array(t, slackMock.threadCalls).Length(0)
		gt.Value(t, previous).Equal("done")
		gt.Value(t, reset.BoardStatus).Equal("done")
	})

	t.Run("re-setting the same board status leaves UpdatedAt where it was", func(t *testing.T) {
		// The home dashboard derives its stalled flag and two orderings from
		// UpdatedAt, so a scheduled Job re-setting the same status every morning
		// must not keep the case looking freshly worked on.
		ctx := tokenCtx()
		uc, _, c, _ := newThreadNotifyCase(t, ctx)

		moved, _, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		movedAt := moved.UpdatedAt
		gt.Bool(t, movedAt.After(c.UpdatedAt)).True()

		again, previous, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		gt.Value(t, previous).Equal("done")
		gt.Bool(t, again.UpdatedAt.Equal(movedAt)).True()

		// A real move after the no-op still advances it.
		reopened, _, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "triage")
		gt.NoError(t, err).Required()
		gt.Bool(t, reopened.UpdatedAt.After(movedAt)).True()
	})

	t.Run("a lifecycle status that drifted from the board status is repaired and stamped", func(t *testing.T) {
		// validate --check-db reports this state as lifecycle_mismatch and never
		// repairs it. Re-setting the board status the case already holds is a
		// real change here, so UpdatedAt moves even though the column did not.
		ctx := tokenCtx()
		uc, _, c, repo := newThreadNotifyCase(t, ctx)

		drifted, _, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		gt.Value(t, drifted.Status).Equal(types.CaseStatusClosed)
		stampedAt := drifted.UpdatedAt

		// Write the inconsistent state directly: no usecase path produces it.
		drifted.Status = types.CaseStatusOpen
		_, err = repo.Case().Update(ctx, "support", drifted)
		gt.NoError(t, err).Required()

		repaired, previous, err := uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
		gt.NoError(t, err).Required()
		gt.Value(t, previous).Equal("done")
		gt.Value(t, repaired.BoardStatus).Equal("done")
		gt.Value(t, repaired.Status).Equal(types.CaseStatusClosed)
		gt.Bool(t, repaired.UpdatedAt.After(stampedAt)).True()
	})

	t.Run("an empty board status renders as a dash and an unknown one as its raw id", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			from  string
			label string
		}{
			{name: "empty board status", from: "", label: "—"},
			{name: "board status dropped from the set", from: "retired_column", label: "retired_column"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx := tokenCtx()
				uc, slackMock, c, repo := newThreadNotifyCase(t, ctx)

				stored, err := repo.Case().Get(ctx, "support", c.ID)
				gt.NoError(t, err).Required()
				stored.BoardStatus = tc.from
				_, err = repo.Case().Update(ctx, "support", stored)
				gt.NoError(t, err).Required()

				_, _, err = uc.UpdateCaseStatus(ctx, "support", c.ID, "done")
				gt.NoError(t, err).Required()

				gt.Array(t, slackMock.threadCalls).Length(1).Required()
				assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
					i18n.T(ctx, i18n.MsgCaseChangeStatus, actor, tc.label, "Done"))
			})
		}
	})

	t.Run("a context without an auth token is attributed to the system", func(t *testing.T) {
		setupCtx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, setupCtx)

		// The agent tool path dispatches with no auth token.
		agentCtx := context.Background()
		_, _, err := uc.UpdateCaseStatus(agentCtx, "support", c.ID, "done")
		gt.NoError(t, err).Required()

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
			i18n.T(agentCtx, i18n.MsgCaseChangeStatus,
				i18n.T(agentCtx, i18n.MsgChangeActorSystem), "Triage", "Done"))
	})

	t.Run("a case-channel actor is attributed to the system, not mentioned as a user", func(t *testing.T) {
		setupCtx := tokenCtx()
		uc, slackMock, c, _ := newThreadNotifyCase(t, setupCtx)

		// An agent turn an app started acts as a case channel. It is not a
		// person, so it must never be rendered as "<@...>".
		appCtx := auth.ContextWithToken(context.Background(),
			&auth.Token{Sub: model.CaseChannelActorID("C0CASE01")})
		_, _, err := uc.UpdateCaseStatus(appCtx, "support", c.ID, "done")
		gt.NoError(t, err).Required()

		gt.Array(t, slackMock.threadCalls).Length(1).Required()
		assertThreadContextLine(t, slackMock.threadCalls[0], "C-MONITOR", c.SlackThreadTS,
			i18n.T(appCtx, i18n.MsgCaseChangeStatus,
				i18n.T(appCtx, i18n.MsgChangeActorSystem), "Triage", "Done"))
	})

	t.Run("a Slack failure does not roll back the update", func(t *testing.T) {
		ctx := tokenCtx()
		uc, slackMock, c, repo := newThreadNotifyCase(t, ctx)
		slackMock.postThreadErr = errors.New("slack outage")

		title := "Persisted despite Slack"
		updated, err := uc.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Title: &title})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Title).Equal(title)
		gt.Array(t, slackMock.threadCalls).Length(1)

		stored, err := repo.Case().Get(ctx, "support", c.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Title).Equal(title)
	})

	t.Run("a nil Slack service leaves the update intact", func(t *testing.T) {
		ctx := tokenCtx()
		_, _, c, repo := newThreadNotifyCase(t, ctx)

		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		ucNoSlack := usecase.NewCaseUseCase(repo, registry, nil, nil, "")

		title := "Updated without Slack"
		updated, err := ucNoSlack.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Title: &title})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Title).Equal(title)
	})

	// The assignee diff is computed inside the repository transaction, against
	// the same state the write lands on. These two cases pin that: a request
	// that loses the race to an identical concurrent change must stay silent
	// instead of announcing the winner's change a second time.
	t.Run("losing a race to an identical concurrent assign announces nothing", func(t *testing.T) {
		ctx := tokenCtx()
		_, slackMock, c, repo := newThreadNotifyCase(t, ctx)
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
			{ID: "U-BOB", Name: "bob", RealName: "Bob"},
		})).Required()

		stale := *c
		stale.AssigneeIDs = nil
		committed := *c
		committed.AssigneeIDs = []string{"U-BOB"} // a concurrent request already assigned U-BOB

		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		uc := usecase.NewCaseUseCase(&failCreateRepo{
			Repository: repo,
			caseRepo:   &racedCaseRepo{CaseRepository: repo.Case(), stale: &stale, committed: &committed},
		}, registry, slackMock, nil, "")
		slackMock.threadCalls = nil

		updated, err := uc.AssignCase(ctx, "support", c.ID, []string{"U-BOB"})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.AssigneeIDs).Equal([]string{"U-BOB"})
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("losing a race to an identical concurrent unassign announces nothing", func(t *testing.T) {
		ctx := tokenCtx()
		_, slackMock, c, repo := newThreadNotifyCase(t, ctx)

		stale := *c
		stale.AssigneeIDs = []string{"U-BOB"}
		committed := *c
		committed.AssigneeIDs = nil // a concurrent request already removed U-BOB

		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		uc := usecase.NewCaseUseCase(&failCreateRepo{
			Repository: repo,
			caseRepo:   &racedCaseRepo{CaseRepository: repo.Case(), stale: &stale, committed: &committed},
		}, registry, slackMock, nil, "")
		slackMock.threadCalls = nil

		updated, err := uc.UnassignCase(ctx, "support", c.ID, []string{"U-BOB"})
		gt.NoError(t, err).Required()
		gt.Array(t, updated.AssigneeIDs).Length(0)
		gt.Array(t, slackMock.threadCalls).Length(0)
	})

	t.Run("a failed write announces nothing", func(t *testing.T) {
		ctx := tokenCtx()
		_, slackMock, c, repo := newThreadNotifyCase(t, ctx)

		writeErr := errors.New("firestore unavailable")
		failing := &failCreateRepo{
			Repository: repo,
			caseRepo:   &failUpdateCaseRepo{CaseRepository: repo.Case(), err: writeErr},
		}
		registry := model.NewWorkspaceRegistry()
		registry.Register(threadModeWorkspace(t))
		ucFailing := usecase.NewCaseUseCase(failing, registry, slackMock, nil, "")

		title := "Never persisted"
		_, err := ucFailing.UpdateCase(ctx, "support", c.ID, usecase.CaseUpdate{Title: &title})
		gt.Error(t, err).Is(writeErr)
		gt.Array(t, slackMock.threadCalls).Length(0)
	})
}
