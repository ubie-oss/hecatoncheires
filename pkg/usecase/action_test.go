package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	goslack "github.com/slack-go/slack"
)

func TestActionUseCase_CreateAction(t *testing.T) {
	t.Run("create action with valid case", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create case first
		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Create action
		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Action Description", "U001", "msg-123", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		gt.Number(t, created.ID).NotEqual(0)
		gt.Value(t, created.CaseID).Equal(c.ID)
		gt.Value(t, created.Title).Equal("Test Action")
		gt.Value(t, created.SlackMessageTS).Equal("msg-123")
		gt.Value(t, created.Status).Equal(types.ActionStatusTodo)
	})

	t.Run("create action without title fails", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "", "Description", "", "", types.ActionStatusTodo, nil)
		gt.Value(t, err).NotNil()
	})

	t.Run("create action with non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := actionUC.CreateAction(ctx, testWorkspaceID, 999, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("create action with invalid status fails", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", "invalid-status", nil)
		gt.Value(t, err).NotNil()
	})

	t.Run("create action with default status", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", "", nil)
		gt.NoError(t, err).Required()

		// Default workspace falls back to model.DefaultActionStatusSet whose
		// initial id is BACKLOG.
		gt.Value(t, created.Status).Equal(types.ActionStatusBacklog)
	})

	t.Run("create action in a thread-mode case is rejected by default", func(t *testing.T) {
		// [case] actions defaults to off, so an existing thread-mode workspace
		// keeps having no actions. With no registry the case's thread binding
		// stands in for the mode and the default applies too.
		for name, registry := range map[string]*model.WorkspaceRegistry{
			"actions off": threadModeActionsRegistry(false),
			"no registry": nil,
		} {
			t.Run(name, func(t *testing.T) {
				repo := memory.New()
				slackFake := &threadModeSlackFake{}
				actionUC := usecase.NewActionUseCase(repo, registry, slackFake, "", nil)
				ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})
				c := newThreadModeCase(t, repo)

				_, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "", "", "", "", nil)
				gt.Error(t, err).Is(usecase.ErrCaseActionsDisabled)

				actions, err := repo.Action().GetByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{})
				gt.NoError(t, err).Required()
				gt.Array(t, actions).Length(0)
				gt.Array(t, slackFake.threadCalls).Length(0)
			})
		}
	})

	t.Run("create action in a thread-mode case is accepted when the workspace enables actions", func(t *testing.T) {
		// With [case] actions on, a thread-mode case takes actions like any
		// other case.
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		threadCase, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:          "Thread case",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C123",
			SlackThreadTS:  "1700000000.000300",
			BoardStatus:    "in_progress",
		})
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, threadCase.ID, "Test Action", "Description", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		actions, err := repo.Action().GetByCase(ctx, testWorkspaceID, threadCase.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, actions).Length(1).Required()
		gt.Value(t, actions[0].ID).Equal(created.ID)
		gt.Value(t, actions[0].Title).Equal("Test Action")
		gt.Value(t, actions[0].Description).Equal("Description")
		gt.Value(t, actions[0].AssigneeID).Equal("U001")
		gt.Value(t, actions[0].Status).Equal(types.ActionStatusTodo)
	})
}

// A Slack actor the workspace's policy denies cannot update an action (the
// Slack status / assignee selects reach UpdateAction without the GraphQL
// field middleware), while a system actor is not checked.
// actionChangeTexts drives a status change and an archive on one Action and
// returns the thread notifications they posted. wsLang is the workspace's
// configured language and defaultLang the deployment default; the context
// carries no user language, as a web UI request does not.
func actionChangeTexts(t *testing.T, wsLang, defaultLang i18n.Lang) []string {
	t.Helper()
	i18n.Init(defaultLang)
	defer i18n.Init(i18n.LangEN)

	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: testWorkspaceID}, Language: wsLang})
	slackFake := &stepSlackFake{}
	caseUC := usecase.NewCaseUseCase(repo, registry, slackFake, nil, "")
	actionUC := usecase.NewActionUseCase(repo, registry, slackFake, "", nil)
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCASEMEMBER"})

	c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Lang Case", "", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()
	stored, err := repo.Case().Get(ctx, testWorkspaceID, c.ID)
	gt.NoError(t, err).Required()
	stored.SlackChannelID = "CLANG"
	stored.ChannelUserIDs = []string{"UCASEMEMBER"}
	_, err = repo.Case().Update(ctx, testWorkspaceID, stored)
	gt.NoError(t, err).Required()

	action, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Lang Action", "", "", "1700000000.000100", types.ActionStatusTodo, nil)
	gt.NoError(t, err).Required()
	async.Wait()
	slackFake.calls = nil

	actor := usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UCASEMEMBER"}
	done := types.ActionStatusCompleted
	_, err = actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
		ID: action.ID, Status: &done, Actor: actor,
	})
	gt.NoError(t, err).Required()
	_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, action.ID, actor)
	gt.NoError(t, err).Required()
	async.Wait()

	texts := make([]string, 0, len(slackFake.calls))
	for _, call := range slackFake.calls {
		texts = append(texts, call.text)
	}
	return texts
}

// The Action change and archive notices follow the workspace language, which
// wins over the deployment default; without one they keep the default.
func TestActionUseCase_NotificationsFollowTheWorkspaceLanguage(t *testing.T) {
	inJapanese := actionChangeTexts(t, "", i18n.LangJA)
	inEnglish := actionChangeTexts(t, "", i18n.LangEN)
	gt.Array(t, inJapanese).Length(2).Required()
	gt.Value(t, inJapanese).NotEqual(inEnglish)

	gt.Value(t, actionChangeTexts(t, i18n.LangJA, i18n.LangEN)).Equal(inJapanese)
	gt.Value(t, actionChangeTexts(t, i18n.LangEN, i18n.LangJA)).Equal(inEnglish)
}

func TestActionUseCase_UpdateAction_WorkspaceAccess(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: testWorkspaceID, Name: "Test"}})
	uc := usecase.New(repo, registry, usecase.WithWorkspaceAccess(denyingAccess(t, registry, testWorkspaceID)))

	c, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{ReporterID: "U-REPORTER", Title: "Case"})
	gt.NoError(t, err).Required()
	created, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{CaseID: c.ID, Title: "Action", Status: types.ActionStatusTodo})
	gt.NoError(t, err).Required()

	newStatus := types.ActionStatusInProgress
	_, err = uc.Action.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
		ID: created.ID, Status: &newStatus, SlackSync: usecase.SlackSyncSkip,
		Actor: usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "U-DENIED"},
	})
	gt.Error(t, err).Is(model.ErrWorkspaceAccessDenied)
	stored, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.Status).Equal(types.ActionStatusTodo)

	updated, err := uc.Action.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
		ID: created.ID, Status: &newStatus, SlackSync: usecase.SlackSyncSkip,
		Actor: usecase.ActorRef{Kind: usecase.ActorKindSystem},
	})
	gt.NoError(t, err).Required()
	gt.Value(t, updated.Status).Equal(types.ActionStatusInProgress)
}

func TestActionUseCase_UpdateAction(t *testing.T) {
	t.Run("update action title and status", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Original Title", "Original Description", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		newTitle := "Updated Title"
		newStatus := types.ActionStatusInProgress
		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, Title: &newTitle, Status: &newStatus, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		gt.Value(t, updated.Title).Equal("Updated Title")
		gt.Value(t, updated.Status).Equal(types.ActionStatusInProgress)
		gt.Value(t, updated.CaseID).Equal(c.ID)
	})

	t.Run("update action caseID", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c1, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case 1", "Description 1", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		c2, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case 2", "Description 2", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c1.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, CaseID: &c2.ID, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		gt.Value(t, updated.CaseID).Equal(c2.ID)
	})

	t.Run("update action with non-existent case fails", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		newCaseID := int64(999)
		_, err = actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, CaseID: &newCaseID, SlackSync: usecase.SlackSyncSkip,
		})
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("update non-existent action fails", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: 999, SlackSync: usecase.SlackSyncSkip,
		})
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})

	t.Run("reparenting an action into a thread-mode case is rejected by default", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		channelCase, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title: "Channel case", Status: types.CaseStatusOpen, ReporterID: "UTESTUSER",
		})
		gt.NoError(t, err).Required()
		threadCase := newThreadModeCase(t, repo)

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, channelCase.ID, "Test Action", "", "", "", "", nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, CaseID: &threadCase.ID, SlackSync: usecase.SlackSyncSkip,
		})
		gt.Error(t, err).Is(usecase.ErrCaseActionsDisabled)

		got, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.CaseID).Equal(channelCase.ID)
	})

	t.Run("reparenting an action into a thread-mode case is accepted when the workspace enables actions", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		channelCase, err := caseUC.CreateCase(ctx, testWorkspaceID, "Channel Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		threadCase, err := repo.Case().Create(ctx, testWorkspaceID, &model.Case{
			Title:          "Thread case",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C123",
			SlackThreadTS:  "1700000000.000400",
			BoardStatus:    "in_progress",
		})
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, channelCase.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, CaseID: &threadCase.ID, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		got, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.CaseID).Equal(threadCase.ID)
	})

	t.Run("updating an archived action preserves ArchivedAt", func(t *testing.T) {
		// Regression for #188: UpdateAction rebuilt the persisted Action from a
		// field-by-field literal that omitted ArchivedAt, so a single update on
		// an archived Action silently un-archived it (the Firestore Update is a
		// full Set overwrite). Same class as the SlackThreadTS loss fixed in #185
		// for Case. The update below must leave the action archived.
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Original Title", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		archived, err := actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Value(t, archived.ArchivedAt).NotNil().Required()
		archivedAt := *archived.ArchivedAt

		newTitle := "Updated Title"
		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, Title: &newTitle, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		// The returned value must still be archived, at the same instant.
		gt.Value(t, updated.Title).Equal("Updated Title")
		gt.Value(t, updated.ArchivedAt).NotNil().Required()
		gt.Bool(t, updated.IsArchived()).True()
		gt.Bool(t, updated.ArchivedAt.Equal(archivedAt)).True()

		// And the persisted state must agree — this is what a fresh reader (UI,
		// list query) sees. A dropped ArchivedAt would re-surface the action.
		fetched, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, fetched.ArchivedAt).NotNil().Required()
		gt.Bool(t, fetched.IsArchived()).True()
		gt.Bool(t, fetched.ArchivedAt.Equal(archivedAt)).True()
		gt.Value(t, fetched.Title).Equal("Updated Title")
	})

	t.Run("updating a non-archived action keeps ArchivedAt nil", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Original Title", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		newTitle := "Updated Title"
		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, Title: &newTitle, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.ArchivedAt).Nil()
		gt.Bool(t, updated.IsArchived()).False()

		fetched, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, fetched.ArchivedAt).Nil()
	})

	t.Run("reparenting an archived action preserves ArchivedAt", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c1, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case 1", "Description 1", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		c2, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case 2", "Description 2", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c1.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()

		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, CaseID: &c2.ID, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.CaseID).Equal(c2.ID)
		gt.Value(t, updated.ArchivedAt).NotNil()
		gt.Bool(t, updated.IsArchived()).True()
	})

	t.Run("update records change history events (existing snapshot preserved)", func(t *testing.T) {
		// The value-copy fix must keep `existing` as an untouched pre-update
		// snapshot so recordActionEvents can diff before vs after. An in-place
		// mutation would collapse the diff and silently drop all history.
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Original Title", "Description", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		newTitle := "Updated Title"
		newStatus := types.ActionStatusInProgress
		newAssignee := "U002"
		_, err = actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, Title: &newTitle, Status: &newStatus, AssigneeID: &newAssignee, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		events, _, err := repo.ActionEvent().List(ctx, testWorkspaceID, created.ID, 100, "")
		gt.NoError(t, err).Required()
		kinds := map[types.ActionEventKind]int{}
		for _, e := range events {
			kinds[e.Kind]++
		}
		gt.Number(t, kinds[types.ActionEventTitleChanged]).Equal(1)
		gt.Number(t, kinds[types.ActionEventStatusChanged]).Equal(1)
		gt.Number(t, kinds[types.ActionEventAssigneeChanged]).Equal(1)
	})
}

func TestActionUseCase_ArchiveAction(t *testing.T) {
	t.Run("archive action sets ArchivedAt and hides from default list", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		archived, err := actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Value(t, archived.ArchivedAt).NotNil()
		gt.Bool(t, archived.IsArchived()).True()

		// Default list excludes archived
		got, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(0)

		// ArchiveScopeAll returns archived action too
		gotAll, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{ArchiveScope: interfaces.ActionArchiveScopeAll})
		gt.NoError(t, err).Required()
		gt.Array(t, gotAll).Length(1).Required()
		gt.Value(t, gotAll[0].ID).Equal(created.ID)

		// ArchiveScopeArchivedOnly returns just the archived action
		gotArchived, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{ArchiveScope: interfaces.ActionArchiveScopeArchivedOnly})
		gt.NoError(t, err).Required()
		gt.Array(t, gotArchived).Length(1).Required()
		gt.Value(t, gotArchived[0].ID).Equal(created.ID)

		// Get still returns archived action
		fetched, err := actionUC.GetAction(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, fetched.IsArchived()).True()

		// ActionEvent recorded
		events, _, err := repo.ActionEvent().List(ctx, testWorkspaceID, created.ID, 100, "")
		gt.NoError(t, err).Required()
		archiveEventCount := 0
		for _, e := range events {
			if e.Kind == types.ActionEventArchived {
				archiveEventCount++
			}
		}
		gt.Number(t, archiveEventCount).Equal(1)
	})

	t.Run("archive already archived action returns ErrActionAlreadyArchived", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()

		_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.Error(t, err).Is(usecase.ErrActionAlreadyArchived)
	})

	t.Run("archive non-existent action returns ErrActionNotFound", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := actionUC.ArchiveAction(ctx, testWorkspaceID, 999, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})
}

func TestActionUseCase_BulkArchiveActions(t *testing.T) {
	t.Run("archives every given action and records an event for each", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		a1, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 1", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		a2, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 2", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		a3, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 3", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		archived, err := actionUC.BulkArchiveActions(ctx, testWorkspaceID, []int64{a1.ID, a2.ID, a3.ID}, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(3).Required()

		// Every requested action must come back archived. Verify each id
		// individually (not just the count) and that ArchivedAt is set.
		byID := map[int64]*model.Action{}
		for _, a := range archived {
			byID[a.ID] = a
		}
		for _, want := range []int64{a1.ID, a2.ID, a3.ID} {
			got := byID[want]
			gt.Value(t, got).NotNil().Required()
			gt.Bool(t, got.IsArchived()).True()
			gt.Value(t, got.ArchivedAt).NotNil()
		}

		// Default listing now excludes all three.
		remaining, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, remaining).Length(0)

		// Each action got exactly one ARCHIVED event, proving bulk reused the
		// single-archive path rather than skipping event recording.
		for _, id := range []int64{a1.ID, a2.ID, a3.ID} {
			events, _, listErr := repo.ActionEvent().List(ctx, testWorkspaceID, id, 100, "")
			gt.NoError(t, listErr).Required()
			archiveEventCount := 0
			for _, e := range events {
				if e.Kind == types.ActionEventArchived {
					archiveEventCount++
				}
			}
			gt.Number(t, archiveEventCount).Equal(1)
		}
	})

	t.Run("skips already-archived actions and returns only newly archived", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		a1, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Already", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		a2, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Fresh", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Pre-archive a1 so the bulk call must skip it.
		_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, a1.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()

		archived, err := actionUC.BulkArchiveActions(ctx, testWorkspaceID, []int64{a1.ID, a2.ID}, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(1).Required()
		gt.Value(t, archived[0].ID).Equal(a2.ID)

		// a1 must not have a second ARCHIVED event from the skipped bulk pass.
		events, _, err := repo.ActionEvent().List(ctx, testWorkspaceID, a1.ID, 100, "")
		gt.NoError(t, err).Required()
		archiveEventCount := 0
		for _, e := range events {
			if e.Kind == types.ActionEventArchived {
				archiveEventCount++
			}
		}
		gt.Number(t, archiveEventCount).Equal(1)
	})

	t.Run("empty ids returns empty slice without error", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		archived, err := actionUC.BulkArchiveActions(ctx, testWorkspaceID, nil, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(0)
	})

	t.Run("non-member cannot bulk archive actions in private case", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		action, err := actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Member Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Non-member's bulk archive must be denied and leave the action active.
		_, err = actionUC.BulkArchiveActions(nonMemberCtx, testWorkspaceID, []int64{action.ID}, usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UOTHER"})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		fetched, err := actionUC.GetAction(memberCtx, testWorkspaceID, action.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, fetched.IsArchived()).False()
	})

	t.Run("system context without auth token bypasses access control", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		// No auth token in ctx: mirrors agent/bot system flows.
		ctx := context.Background()

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(ctx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		action, err := actionUC.CreateAction(ctx, testWorkspaceID, created.ID, "System Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		archived, err := actionUC.BulkArchiveActions(ctx, testWorkspaceID, []int64{action.ID}, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Array(t, archived).Length(1).Required()
		gt.Value(t, archived[0].ID).Equal(action.ID)
		gt.Bool(t, archived[0].IsArchived()).True()
	})
}

func TestActionUseCase_BulkArchiveActionsAsync(t *testing.T) {
	t.Run("archives every action in the background after Wait", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		a1, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Async 1", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		a2, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Async 2", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Fire-and-forget: the call returns immediately and the archiving
		// runs on a background goroutine.
		actionUC.BulkArchiveActionsAsync(ctx, testWorkspaceID, []int64{a1.ID, a2.ID}, usecase.ActorRef{Kind: usecase.ActorKindSystem})

		// Deterministically wait for the dispatched goroutine to finish.
		async.Wait()

		for _, id := range []int64{a1.ID, a2.ID} {
			got, getErr := actionUC.GetAction(ctx, testWorkspaceID, id)
			gt.NoError(t, getErr).Required()
			gt.Bool(t, got.IsArchived()).True()
		}

		// Default listing is now empty.
		remaining, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, remaining).Length(0)
	})

	t.Run("survives cancellation of the originating context", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		baseCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(baseCtx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		action, err := actionUC.CreateAction(baseCtx, testWorkspaceID, c.ID, "Cancelled Ctx", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Cancel the originating context immediately, mimicking the HTTP
		// request being torn down right after the resolver dispatched.
		cancelCtx, cancel := context.WithCancel(baseCtx)
		actionUC.BulkArchiveActionsAsync(cancelCtx, testWorkspaceID, []int64{action.ID}, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		cancel()

		async.Wait()

		// The archive still completed because async.Dispatch severs the
		// cancellation signal from the background work.
		got, err := actionUC.GetAction(baseCtx, testWorkspaceID, action.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, got.IsArchived()).True()
	})
}

func TestActionUseCase_UnarchiveAction(t *testing.T) {
	t.Run("unarchive restores active state and records event", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()

		restored, err := actionUC.UnarchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.NoError(t, err).Required()
		gt.Value(t, restored.ArchivedAt).Nil()
		gt.Bool(t, restored.IsArchived()).False()

		// Default list now includes it again
		got, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(1).Required()
		gt.Value(t, got[0].ID).Equal(created.ID)

		events, _, err := repo.ActionEvent().List(ctx, testWorkspaceID, created.ID, 100, "")
		gt.NoError(t, err).Required()
		unarchiveCount := 0
		for _, e := range events {
			if e.Kind == types.ActionEventUnarchived {
				unarchiveCount++
			}
		}
		gt.Number(t, unarchiveCount).Equal(1)
	})

	t.Run("unarchive non-archived action returns ErrActionNotArchived", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.UnarchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.Error(t, err).Is(usecase.ErrActionNotArchived)
	})
}

func TestActionUseCase_GetAction(t *testing.T) {
	t.Run("get existing action", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		retrieved, err := actionUC.GetAction(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()

		gt.Value(t, retrieved.ID).Equal(created.ID)
		gt.Value(t, retrieved.Title).Equal(created.Title)
	})

	t.Run("get non-existent action fails", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := actionUC.GetAction(ctx, testWorkspaceID, 999)
		gt.Value(t, err).NotNil()
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})
}

func TestActionUseCase_ListActions(t *testing.T) {
	t.Run("list actions", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 1", "Description 1", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 2", "Description 2", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		actions, err := actionUC.ListActions(ctx, testWorkspaceID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()

		gt.Array(t, actions).Length(2)
	})
}

func TestActionUseCase_GetActionsByCase(t *testing.T) {
	t.Run("get actions by case", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c1, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case 1", "Description 1", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		c2, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case 2", "Description 2", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c1.ID, "Action 1-1", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c1.ID, "Action 1-2", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c2.ID, "Action 2-1", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		actions, err := actionUC.GetActionsByCase(ctx, testWorkspaceID, c1.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()

		gt.Array(t, actions).Length(2)

		for _, action := range actions {
			gt.Value(t, action.CaseID).Equal(c1.ID)
		}
	})
}

func TestActionUseCase_GetActions(t *testing.T) {
	t.Run("returns requested actions in order, omits missing", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Case", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		a1, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 1", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		a2, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action 2", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		got, err := actionUC.GetActions(ctx, testWorkspaceID, []int64{a2.ID, 999999, a1.ID}, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(2)
		gt.Value(t, got[0].ID).Equal(a2.ID)
		gt.Value(t, got[0].Title).Equal("Action 2")
		gt.Value(t, got[1].ID).Equal(a1.ID)
		gt.Value(t, got[1].Title).Equal("Action 1")
	})

	t.Run("empty ids returns empty slice", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		got, err := actionUC.GetActions(context.Background(), testWorkspaceID, nil, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(0)
	})

	t.Run("ExcludePrivateCaseActions omits private-case actions even for members", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		pubCase, err := caseUC.CreateCase(memberCtx, testWorkspaceID, "Public", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		pubAct, err := actionUC.CreateAction(memberCtx, testWorkspaceID, pubCase.ID, "Public Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		privCreated, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()
		privAct, err := actionUC.CreateAction(memberCtx, testWorkspaceID, privCreated.ID, "Private Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		opts := interfaces.ActionListOptions{ExcludePrivateCaseActions: true}
		got, err := actionUC.GetActions(memberCtx, testWorkspaceID, []int64{pubAct.ID, privAct.ID}, opts)
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(1)
		gt.Value(t, got[0].ID).Equal(pubAct.ID)
	})
}

// actionTestSlackMock tracks the Slack calls Action use-case makes.
// Action card posts go through the *WithAttachment variants; the plain
// PostMessage / UpdateMessage stubs remain so other tests sharing this
// embedding still satisfy the Service interface.
type actionTestSlackMock struct {
	mockSlackService
	postMessageCalled   bool
	postMessageChannel  string
	postMessageBlocks   []goslack.Block
	postMessageText     string
	postMessageTS       string
	postMessageErr      error
	updateMessageCalled bool
	updateMessageTS     string
	updateMessageBlocks []goslack.Block
	updateMessageErr    error

	postWithAttachmentCalled     bool
	postWithAttachmentChannel    string
	postWithAttachmentText       string
	postWithAttachmentAttachment goslack.Attachment

	updateWithAttachmentCalled     bool
	updateWithAttachmentTS         string
	updateWithAttachmentText       string
	updateWithAttachmentAttachment goslack.Attachment
}

func (m *actionTestSlackMock) PostMessage(ctx context.Context, channelID string, blocks []goslack.Block, text string) (string, error) {
	m.postMessageCalled = true
	m.postMessageChannel = channelID
	m.postMessageBlocks = blocks
	m.postMessageText = text
	if m.postMessageErr != nil {
		return "", m.postMessageErr
	}
	return m.postMessageTS, nil
}

func (m *actionTestSlackMock) UpdateMessage(ctx context.Context, channelID string, timestamp string, blocks []goslack.Block, text string) error {
	m.updateMessageCalled = true
	m.updateMessageTS = timestamp
	m.updateMessageBlocks = blocks
	return m.updateMessageErr
}

func (m *actionTestSlackMock) PostMessageWithAttachment(ctx context.Context, channelID string, text string, attachment goslack.Attachment) (string, error) {
	m.postWithAttachmentCalled = true
	m.postWithAttachmentChannel = channelID
	m.postWithAttachmentText = text
	m.postWithAttachmentAttachment = attachment
	if m.postMessageErr != nil {
		return "", m.postMessageErr
	}
	return m.postMessageTS, nil
}

func (m *actionTestSlackMock) UpdateMessageWithAttachment(ctx context.Context, channelID string, timestamp string, text string, attachment goslack.Attachment) error {
	m.updateWithAttachmentCalled = true
	m.updateWithAttachmentTS = timestamp
	m.updateWithAttachmentText = text
	m.updateWithAttachmentAttachment = attachment
	return m.updateMessageErr
}

func TestActionUseCase_CreateAction_SlackNotification(t *testing.T) {
	t.Run("posts Slack message when case has channel", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageTS: "1234567890.123456",
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Create case with Slack channel
		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, c.SlackChannelID).NotEqual("")

		// Create action
		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Action Desc", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Verify Slack message was posted via the attachment-shaped path
		// so the channel-view broadcast preview will see the parent text.
		gt.Value(t, mock.postWithAttachmentCalled).Equal(true)
		gt.Value(t, mock.postWithAttachmentChannel).Equal(c.SlackChannelID)
		// Title must carry the fixed action-card prefix emoji and the
		// linked title; no "Action:" literal.
		gt.String(t, mock.postWithAttachmentText).Contains("📌")
		gt.String(t, mock.postWithAttachmentText).Contains("Test Action")
		gt.String(t, mock.postWithAttachmentText).NotEqual("")
		// The attachment must carry the Block Kit content (description +
		// Actions block with selects).
		gt.Array(t, mock.postWithAttachmentAttachment.Blocks.BlockSet).Length(2)

		// Verify SlackMessageTS was saved
		gt.Value(t, created.SlackMessageTS).Equal("1234567890.123456")

		// Verify the saved action has the timestamp
		retrieved, err := actionUC.GetAction(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.SlackMessageTS).Equal("1234567890.123456")
	})

	t.Run("escapes mrkdwn control characters in title", func(t *testing.T) {
		// Slack mrkdwn treats `&` `<` `>` as control characters; `|` inside
		// `<URL|label>` terminates the link label. User-supplied titles
		// must therefore be escaped before they reach the top-level text
		// or the link label, or Slack mis-renders / mis-parses the card.
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageTS: "1234567890.123456",
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Title with every problematic character.
		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "a&b<c>d|e", "", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		gt.String(t, mock.postWithAttachmentText).Contains("a&amp;b&lt;c&gt;d｜e")
		// The raw forms must not survive into the text.
		gt.String(t, mock.postWithAttachmentText).NotEqual("")
		gt.Bool(t, strings.Contains(mock.postWithAttachmentText, "a&b<c>d|e")).False()
	})

	t.Run("does not post when case has no channel", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			postMessageTS: "1234567890.123456",
		}
		// Create case without Slack service (no channel)
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, c.SlackChannelID).Equal("")

		_, err = actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Slack message should NOT have been posted
		gt.Value(t, mock.postWithAttachmentCalled).Equal(false)
	})

	t.Run("does not post when slack service is nil", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackMessageTS).Equal("")
	})

	t.Run("action creation succeeds even when Slack posting fails", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageErr: errors.New("slack API error"),
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		// Action creation should still succeed
		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Test Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, created.Title).Equal("Test Action")
		gt.Value(t, created.SlackMessageTS).Equal("") // No TS because posting failed
	})
}

func TestActionUseCase_PostSlackMessageToAction(t *testing.T) {
	t.Run("posts message and persists timestamp on unposted action", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageTS: "9999.123456",
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, c.SlackChannelID).NotEqual("")

		// Simulate an action created via the legacy tool path: directly via
		// the repository, bypassing CreateAction. SlackMessageTS stays empty.
		raw, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: c.ID,
			Title:  "Unsent Action",
			Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()
		gt.Value(t, raw.SlackMessageTS).Equal("")

		// Reset mock so we observe only the PostSlackMessageToAction call.
		mock.postWithAttachmentCalled = false

		updated, err := actionUC.PostSlackMessageToAction(ctx, testWorkspaceID, raw.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, mock.postWithAttachmentCalled).Equal(true)
		gt.Value(t, mock.postWithAttachmentChannel).Equal(c.SlackChannelID)
		gt.Value(t, updated.SlackMessageTS).Equal("9999.123456")

		retrieved, err := actionUC.GetAction(ctx, testWorkspaceID, raw.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.SlackMessageTS).Equal("9999.123456")
	})

	t.Run("returns ErrSlackMessageAlreadyPosted for action with existing TS", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageTS: "1.1",
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Posted Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, created.SlackMessageTS).NotEqual("")

		mock.postWithAttachmentCalled = false
		_, err = actionUC.PostSlackMessageToAction(ctx, testWorkspaceID, created.ID)
		gt.Error(t, err).Is(usecase.ErrSlackMessageAlreadyPosted)
		gt.Value(t, mock.postWithAttachmentCalled).Equal(false)
	})

	t.Run("returns ErrCaseHasNoSlackChannel when parent has no channel", func(t *testing.T) {
		repo := memory.New()
		// Build a Slack mock that records calls but returns a TS — to confirm
		// that PostMessage was NEVER invoked when channel is missing.
		mock := &actionTestSlackMock{postMessageTS: "should-not-be-used"}
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Value(t, c.SlackChannelID).Equal("")

		raw, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: c.ID,
			Title:  "Unsent Action",
			Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		_, err = actionUC.PostSlackMessageToAction(ctx, testWorkspaceID, raw.ID)
		gt.Error(t, err).Is(usecase.ErrCaseHasNoSlackChannel)
		gt.Value(t, mock.postWithAttachmentCalled).Equal(false)
	})

	t.Run("returns ErrActionNotFound for unknown action", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{}
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := actionUC.PostSlackMessageToAction(ctx, testWorkspaceID, 99999)
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})

	t.Run("denies access to non-member of private case", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageTS: "should-not-post",
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		seedSlackUsers(t, repo, "UMEMBER")

		c, err := caseUC.CreateCase(memberCtx, testWorkspaceID, "Private Case", "Desc",
			[]string{"UMEMBER"}, nil, true, false, "", "")
		gt.NoError(t, err).Required()

		raw, err := repo.Action().Create(memberCtx, testWorkspaceID, &model.Action{
			CaseID: c.ID,
			Title:  "Private Action",
			Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		outsiderCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOUTSIDER"})
		_, err = actionUC.PostSlackMessageToAction(outsiderCtx, testWorkspaceID, raw.ID)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
		gt.Value(t, mock.postWithAttachmentCalled).Equal(false)
	})

	t.Run("returns wrapped error when Slack PostMessage fails", func(t *testing.T) {
		repo := memory.New()
		mock := &actionTestSlackMock{
			mockSlackService: mockSlackService{
				createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
					return fmt.Sprintf("C%d", caseID), nil
				},
			},
			postMessageErr: errors.New("slack outage"),
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		raw, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: c.ID,
			Title:  "Unsent",
			Status: types.ActionStatusTodo,
		})
		gt.NoError(t, err).Required()

		_, err = actionUC.PostSlackMessageToAction(ctx, testWorkspaceID, raw.ID)
		gt.Value(t, err).NotNil()
		// Slack failure must NOT be silently swallowed for the strict path.
		gt.String(t, err.Error()).Contains("slack outage")

		retrieved, err := actionUC.GetAction(ctx, testWorkspaceID, raw.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.SlackMessageTS).Equal("")
	})
}

// actionTestSlackMockExt extends actionTestSlackMock to also capture
// PostThreadMessage calls used by change-notification tests.
type actionTestSlackMockExt struct {
	actionTestSlackMock
	postThreadCalled    bool
	postThreadChannel   string
	postThreadTS        string
	postThreadBlocks    []goslack.Block
	postThreadText      string
	postThreadBroadcast bool
}

func (m *actionTestSlackMockExt) PostThreadMessage(ctx context.Context, channelID string, threadTS string, blocks []goslack.Block, text string, opts ...slacksvc.PostThreadOption) (string, error) {
	cfg := slacksvc.ApplyPostThreadOptions(opts...)
	m.postThreadCalled = true
	m.postThreadChannel = channelID
	m.postThreadTS = threadTS
	m.postThreadBlocks = blocks
	m.postThreadText = text
	m.postThreadBroadcast = cfg.Broadcast
	return "thread-reply-ts", nil
}

func TestActionUseCase_UpdateAction_SlackSync(t *testing.T) {
	i18n.Init(i18n.LangEN)
	setup := func(t *testing.T) (*memory.Repository, *usecase.ActionUseCase, *actionTestSlackMockExt, *model.Action) {
		t.Helper()
		repo := memory.New()
		mock := &actionTestSlackMockExt{
			actionTestSlackMock: actionTestSlackMock{
				mockSlackService: mockSlackService{
					createChannelFn: func(_ context.Context, caseID int64, _ string, _ string) (string, error) {
						return fmt.Sprintf("C%d", caseID), nil
					},
				},
				postMessageTS: "1234567890.123456",
			},
		}
		caseUC := usecase.NewCaseUseCase(repo, nil, mock, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, mock, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Register the human users that the bot-rejection guard will look up.
		gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
			{ID: "U001", Name: "alice", RealName: "Alice"},
			{ID: "U999", Name: "carol", RealName: "Carol"},
		})).Required()

		_, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		c, err := repo.Case().List(ctx, testWorkspaceID)
		gt.NoError(t, err).Required()

		action, err := actionUC.CreateAction(ctx, testWorkspaceID, c[0].ID, "Test Action", "Desc", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Reset call tracking after creation
		mock.updateWithAttachmentCalled = false
		mock.postThreadCalled = false
		return repo, actionUC, mock, action
	}

	// listChangeEvents pulls the ActionEvent stream for the action and drops the
	// CREATED record so each test only sees diffs produced by UpdateAction.
	listChangeEvents := func(t *testing.T, repo *memory.Repository, ctx context.Context, actionID int64) []*model.ActionEvent {
		t.Helper()
		events, _, err := repo.ActionEvent().List(ctx, testWorkspaceID, actionID, 100, "")
		gt.NoError(t, err).Required()
		var diffs []*model.ActionEvent
		for _, e := range events {
			if e.Kind == types.ActionEventCreated {
				continue
			}
			diffs = append(diffs, e)
		}
		return diffs
	}

	t.Run("status change with SlackSyncFull updates message and records status event", func(t *testing.T) {
		repo, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newStatus := types.ActionStatusInProgress
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()

		updated, err := repo.Action().Get(ctx, testWorkspaceID, action.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Status).Equal(types.ActionStatusInProgress)
		gt.Bool(t, mock.updateWithAttachmentCalled).True()
		// Attachment color tracks the new status so the channel side-bar
		// follows status transitions. Default IN_PROGRESS uses the
		// "active" preset which resolves to a non-empty hex.
		gt.String(t, mock.updateWithAttachmentAttachment.Color).NotEqual("")
		// SlackSyncFull both refreshes the message AND posts a thread
		// summary so channel watchers can see the change without opening
		// the WebUI. The ingest path drops these context-block posts so
		// they don't double-count in the ActionEvent feed.
		gt.Bool(t, mock.postThreadCalled).True()
		gt.String(t, mock.postThreadText).Contains("<@UDOER>")

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(1).Required()
		gt.Value(t, diffs[0].Kind).Equal(types.ActionEventStatusChanged)
		gt.Value(t, diffs[0].ActorID).Equal("UDOER")
		gt.Value(t, diffs[0].OldValue).Equal(types.ActionStatusTodo.String())
		gt.Value(t, diffs[0].NewValue).Equal(types.ActionStatusInProgress.String())
	})

	t.Run("assignee replacement records assignee event", func(t *testing.T) {
		repo, actionUC, _, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newAssignee := "U999"
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:         action.ID,
			AssigneeID: &newAssignee,
			SlackSync:  usecase.SlackSyncFull,
			Actor:      usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(1).Required()
		gt.Value(t, diffs[0].Kind).Equal(types.ActionEventAssigneeChanged)
		gt.Value(t, diffs[0].OldValue).Equal("U001")
		gt.Value(t, diffs[0].NewValue).Equal("U999")
	})

	t.Run("assignee clear records empty newValue", func(t *testing.T) {
		repo, actionUC, _, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:            action.ID,
			ClearAssignee: true,
			SlackSync:     usecase.SlackSyncFull,
			Actor:         usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(1).Required()
		gt.Value(t, diffs[0].Kind).Equal(types.ActionEventAssigneeChanged)
		gt.Value(t, diffs[0].OldValue).Equal("U001")
		gt.Value(t, diffs[0].NewValue).Equal("")
	})

	t.Run("SlackSyncMessageOnly refreshes message and still records event", func(t *testing.T) {
		repo, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newStatus := types.ActionStatusInProgress
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncMessageOnly,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.updateWithAttachmentCalled).True()
		gt.Bool(t, mock.postThreadCalled).False()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(1).Required()
	})

	t.Run("SlackSyncSkip skips Slack but still records event", func(t *testing.T) {
		repo, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newStatus := types.ActionStatusInProgress
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.updateWithAttachmentCalled).False()
		gt.Bool(t, mock.postThreadCalled).False()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(1).Required()
	})

	t.Run("no observable change records no event", func(t *testing.T) {
		repo, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		// Set status to its current value: no diff in title/status/assignee.
		current := action.Status
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &current,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.postThreadCalled).False()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(0)
	})

	t.Run("system actor records event with empty actorID", func(t *testing.T) {
		repo, actionUC, _, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newTitle := "Renamed"
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Title:     &newTitle,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSystem},
		})
		gt.NoError(t, err).Required()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(1).Required()
		gt.Value(t, diffs[0].Kind).Equal(types.ActionEventTitleChanged)
		// System-driven changes leave ActorID empty; the WebUI renders a
		// neutral "system" label in that case.
		gt.Value(t, diffs[0].ActorID).Equal("")
		gt.Value(t, diffs[0].NewValue).Equal("Renamed")
	})

	t.Run("multiple field changes share a single CreatedAt", func(t *testing.T) {
		repo, actionUC, _, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newTitle := "Renamed"
		newStatus := types.ActionStatusInProgress
		newAssignee := "UASSIGNEE"
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:         action.ID,
			Title:      &newTitle,
			Status:     &newStatus,
			AssigneeID: &newAssignee,
			SlackSync:  usecase.SlackSyncSkip,
			Actor:      usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()

		diffs := listChangeEvents(t, repo, ctx, action.ID)
		gt.Array(t, diffs).Length(3).Required()
		// All sibling diffs from one UpdateAction call must share the same
		// CreatedAt so the activity feed groups them deterministically; any
		// drift would let the sort flip per call.
		ts := diffs[0].CreatedAt
		for _, d := range diffs[1:] {
			gt.Bool(t, d.CreatedAt.Equal(ts)).True()
		}
	})

	t.Run("status change is broadcast to parent channel", func(t *testing.T) {
		_, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newStatus := types.ActionStatusInProgress
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.postThreadCalled).True()
		gt.Bool(t, mock.postThreadBroadcast).True()
	})

	t.Run("assignee change is broadcast to parent channel", func(t *testing.T) {
		_, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newAssignee := "U999"
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:         action.ID,
			AssigneeID: &newAssignee,
			SlackSync:  usecase.SlackSyncFull,
			Actor:      usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.postThreadCalled).True()
		gt.Bool(t, mock.postThreadBroadcast).True()
	})

	t.Run("title-only change is not broadcast", func(t *testing.T) {
		_, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newTitle := "Renamed action"
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Title:     &newTitle,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.postThreadCalled).True()
		gt.Bool(t, mock.postThreadBroadcast).False()
	})

	t.Run("mixed diff containing a broadcast kind is broadcast", func(t *testing.T) {
		_, actionUC, mock, action := setup(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		newTitle := "Renamed"
		newStatus := types.ActionStatusInProgress
		_, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Title:     &newTitle,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, mock.postThreadCalled).True()
		gt.Bool(t, mock.postThreadBroadcast).True()
	})
}

func TestParseSlackStatusSelectValue(t *testing.T) {
	t.Run("valid value", func(t *testing.T) {
		ws, id, status, err := usecase.ParseSlackStatusSelectValue("test-ws:42:IN_PROGRESS")
		gt.NoError(t, err).Required()
		gt.Value(t, ws).Equal("test-ws")
		gt.Value(t, id).Equal(int64(42))
		gt.Value(t, status).Equal(types.ActionStatusInProgress)
	})

	t.Run("workspace with hyphen", func(t *testing.T) {
		ws, id, status, err := usecase.ParseSlackStatusSelectValue("my-workspace:123:COMPLETED")
		gt.NoError(t, err).Required()
		gt.Value(t, ws).Equal("my-workspace")
		gt.Value(t, id).Equal(int64(123))
		gt.Value(t, status).Equal(types.ActionStatusCompleted)
	})

	t.Run("unknown-but-non-empty status passes through", func(t *testing.T) {
		// Status validity is decided by the workspace's ActionStatusSet at the
		// callsite, not by this parser. Any non-empty value is parsed.
		ws, id, status, err := usecase.ParseSlackStatusSelectValue("ws:1:CUSTOM_STATUS")
		gt.NoError(t, err).Required()
		gt.Value(t, ws).Equal("ws")
		gt.Value(t, id).Equal(int64(1))
		gt.Value(t, status).Equal(types.ActionStatus("CUSTOM_STATUS"))
	})

	t.Run("empty status is rejected", func(t *testing.T) {
		_, _, _, err := usecase.ParseSlackStatusSelectValue("ws:1:")
		gt.Value(t, err).NotNil()
	})

	t.Run("invalid action id", func(t *testing.T) {
		_, _, _, err := usecase.ParseSlackStatusSelectValue("ws:notnum:TODO")
		gt.Value(t, err).NotNil()
	})

	t.Run("missing fields", func(t *testing.T) {
		_, _, _, err := usecase.ParseSlackStatusSelectValue("only-one-field")
		gt.Value(t, err).NotNil()
	})
}

func TestParseSlackAssigneeBlockID(t *testing.T) {
	t.Run("round-trip", func(t *testing.T) {
		blockID := usecase.SlackActionAssigneeBlockID("test-ws", 42)
		ws, id, err := usecase.ParseSlackAssigneeBlockID(blockID)
		gt.NoError(t, err).Required()
		gt.Value(t, ws).Equal("test-ws")
		gt.Value(t, id).Equal(int64(42))
	})

	t.Run("invalid prefix", func(t *testing.T) {
		_, _, err := usecase.ParseSlackAssigneeBlockID("not_our_block_id")
		gt.Value(t, err).NotNil()
	})

	t.Run("missing action id", func(t *testing.T) {
		_, _, err := usecase.ParseSlackAssigneeBlockID("hc_action_assignee_block:onlyws")
		gt.Value(t, err).NotNil()
	})
}

func TestActionUseCase_DueDate(t *testing.T) {
	t.Run("create action with due date", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		dueDate := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action with due date", "Description", "", "", types.ActionStatusTodo, &dueDate)
		gt.NoError(t, err).Required()

		gt.Value(t, created.DueDate).NotNil()
		gt.Value(t, created.DueDate.Year()).Equal(2026)
		gt.Value(t, created.DueDate.Month()).Equal(time.March)
		gt.Value(t, created.DueDate.Day()).Equal(15)

		// Verify persistence
		retrieved, err := actionUC.GetAction(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, retrieved.DueDate).NotNil()
		gt.Value(t, retrieved.DueDate.Equal(dueDate)).Equal(true)
	})

	t.Run("create action without due date", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action without due date", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		gt.Value(t, created.DueDate == nil).Equal(true)
	})

	t.Run("update action to set due date", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action", "Description", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, created.DueDate == nil).Equal(true)

		dueDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, DueDate: &dueDate, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		gt.Value(t, updated.DueDate).NotNil()
		gt.Value(t, updated.DueDate.Equal(dueDate)).Equal(true)
	})

	t.Run("update action to clear due date", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		dueDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action", "Description", "", "", types.ActionStatusTodo, &dueDate)
		gt.NoError(t, err).Required()
		gt.Value(t, created.DueDate).NotNil()

		// Clear due date using clearDueDate=true
		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, ClearDueDate: true, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		gt.Value(t, updated.DueDate == nil).Equal(true)
	})

	t.Run("update action without changing due date preserves it", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UTESTUSER"})

		c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Test Case", "Description", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()

		dueDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Action", "Description", "", "", types.ActionStatusTodo, &dueDate)
		gt.NoError(t, err).Required()

		// Update only title, leave dueDate unchanged (nil, false)
		newTitle := "Updated Title"
		updated, err := actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID: created.ID, Title: &newTitle, SlackSync: usecase.SlackSyncSkip,
		})
		gt.NoError(t, err).Required()

		gt.Value(t, updated.Title).Equal("Updated Title")
		gt.Value(t, updated.DueDate).NotNil()
		gt.Value(t, updated.DueDate.Equal(dueDate)).Equal(true)
	})
}

func TestActionUseCase_PrivateCaseAccessControl(t *testing.T) {
	t.Run("create action in private case as non-member returns access denied", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})

		// Create private case directly via repo with specific members
		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		// Member can create action
		action, err := actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Member Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		gt.Value(t, action.Title).Equal("Member Action")

		// Non-member cannot create action
		_, err = actionUC.CreateAction(nonMemberCtx, testWorkspaceID, created.ID, "Non-member Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	t.Run("update action in private case as non-member returns access denied", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		// Member creates action
		action, err := actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Non-member cannot update action
		newTitle := "Updated"
		_, err = actionUC.UpdateAction(nonMemberCtx, testWorkspaceID, usecase.UpdateActionInput{
			ID: action.ID, Title: &newTitle, SlackSync: usecase.SlackSyncSkip,
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	// Slack interactivity routes UpdateAction through async.Dispatch, which
	// hands the usecase a fresh background context with no auth token. The
	// access check must therefore fall back to the supplied Actor.ID when
	// the Slack actor is not a channel member.
	t.Run("update action in private case as Slack non-member is denied without token", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		// Simulate the post-async.Dispatch context: no auth token attached.
		bgCtx := context.Background()

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		action, err := actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		newStatus := types.ActionStatusInProgress

		// Slack non-member: should be rejected via Actor.ID fallback.
		_, err = actionUC.UpdateAction(bgCtx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncSkip,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UOTHER"},
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		// Slack member: should succeed even though the context has no token.
		updated, err := actionUC.UpdateAction(bgCtx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncSkip,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UMEMBER"},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Status).Equal(types.ActionStatusInProgress)

		// System actor with no token: backward-compat path stays open.
		newStatus2 := types.ActionStatusCompleted
		updated, err = actionUC.UpdateAction(bgCtx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus2,
			SlackSync: usecase.SlackSyncSkip,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSystem},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Status).Equal(types.ActionStatusCompleted)

		// Slack actor with empty ID (malformed callback): must be denied
		// rather than silently bypassing access control.
		newStatus3 := types.ActionStatusBlocked
		_, err = actionUC.UpdateAction(bgCtx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        action.ID,
			Status:    &newStatus3,
			SlackSync: usecase.SlackSyncSkip,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: ""},
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	t.Run("delete action in private case as non-member returns access denied", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		// Member creates action
		action, err := actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Non-member cannot archive action
		_, err = actionUC.ArchiveAction(nonMemberCtx, testWorkspaceID, action.ID, usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	t.Run("list actions filters out private case actions for non-members", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})

		// Create public case with action
		pubCase, err := caseUC.CreateCase(memberCtx, testWorkspaceID, "Public Case", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		_, err = actionUC.CreateAction(memberCtx, testWorkspaceID, pubCase.ID, "Public Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Create private case with action (directly via repo)
		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		privCreated, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()
		_, err = actionUC.CreateAction(memberCtx, testWorkspaceID, privCreated.ID, "Private Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Member sees both actions
		memberActions, err := actionUC.ListActions(memberCtx, testWorkspaceID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, memberActions).Length(2)

		// Non-member sees only public action
		nonMemberActions, err := actionUC.ListActions(nonMemberCtx, testWorkspaceID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, nonMemberActions).Length(1)
		gt.Value(t, nonMemberActions[0].Title).Equal("Public Action")
	})

	t.Run("get actions by private case as non-member returns empty", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()

		_, err = actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// Member sees actions
		memberActions, err := actionUC.GetActionsByCase(memberCtx, testWorkspaceID, created.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, memberActions).Length(1)

		// Non-member gets empty list
		nonMemberActions, err := actionUC.GetActionsByCase(nonMemberCtx, testWorkspaceID, created.ID, interfaces.ActionListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, nonMemberActions).Length(0)
	})

	t.Run("ExcludePrivateCaseActions drops private-case actions even for members", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		pubCase, err := caseUC.CreateCase(memberCtx, testWorkspaceID, "Public Case", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		_, err = actionUC.CreateAction(memberCtx, testWorkspaceID, pubCase.ID, "Public Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		privCreated, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()
		_, err = actionUC.CreateAction(memberCtx, testWorkspaceID, privCreated.ID, "Private Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		// A member normally sees both; with ExcludePrivateCaseActions the
		// private-case action is dropped regardless of membership.
		opts := interfaces.ActionListOptions{ExcludePrivateCaseActions: true}
		actions, err := actionUC.ListActions(memberCtx, testWorkspaceID, opts)
		gt.NoError(t, err).Required()
		gt.Array(t, actions).Length(1)
		gt.Value(t, actions[0].Title).Equal("Public Action")

		// Same result with no auth token at all (system/MCP context).
		actionsNoToken, err := actionUC.ListActions(context.Background(), testWorkspaceID, opts)
		gt.NoError(t, err).Required()
		gt.Array(t, actionsNoToken).Length(1)
		gt.Value(t, actionsNoToken[0].Title).Equal("Public Action")
	})

	t.Run("ExcludePrivateCaseActions: GetActionsByCase on private case returns empty for members", func(t *testing.T) {
		repo := memory.New()
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		created, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()
		_, err = actionUC.CreateAction(memberCtx, testWorkspaceID, created.ID, "Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		opts := interfaces.ActionListOptions{ExcludePrivateCaseActions: true}
		actions, err := actionUC.GetActionsByCase(memberCtx, testWorkspaceID, created.ID, opts)
		gt.NoError(t, err).Required()
		gt.Array(t, actions).Length(0)
	})

	t.Run("ExcludePrivateCaseActions: GetAction denies private-case action, allows public", func(t *testing.T) {
		repo := memory.New()
		caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
		actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})

		pubCase, err := caseUC.CreateCase(memberCtx, testWorkspaceID, "Public Case", "Desc", []string{}, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		pubAction, err := actionUC.CreateAction(memberCtx, testWorkspaceID, pubCase.ID, "Public Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		privateCase := &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Private Case",
			Description:    "Secret",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		}
		privCreated, err := repo.Case().Create(memberCtx, testWorkspaceID, privateCase)
		gt.NoError(t, err).Required()
		privAction, err := actionUC.CreateAction(memberCtx, testWorkspaceID, privCreated.ID, "Private Action", "Desc", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		opts := interfaces.ActionListOptions{ExcludePrivateCaseActions: true}

		// Public-case action is returned.
		got, err := actionUC.GetAction(memberCtx, testWorkspaceID, pubAction.ID, opts)
		gt.NoError(t, err).Required()
		gt.Value(t, got.Title).Equal("Public Action")

		// Private-case action is denied even though the caller is a member.
		_, err = actionUC.GetAction(memberCtx, testWorkspaceID, privAction.ID, opts)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})
}

func TestActorForAccess(t *testing.T) {
	t.Run("auth token is preferred over Slack actor", func(t *testing.T) {
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-token"})
		id, check := usecase.ActorForAccessForTest(ctx, usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "U-slack"})
		gt.String(t, id).Equal("U-token")
		gt.Bool(t, check).True()
	})

	t.Run("falls back to Slack actor when no token", func(t *testing.T) {
		id, check := usecase.ActorForAccessForTest(context.Background(), usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "U-slack"})
		gt.String(t, id).Equal("U-slack")
		gt.Bool(t, check).True()
	})

	t.Run("system actor without token bypasses the check", func(t *testing.T) {
		id, check := usecase.ActorForAccessForTest(context.Background(), usecase.ActorRef{Kind: usecase.ActorKindSystem})
		gt.String(t, id).Equal("")
		gt.Bool(t, check).False()
	})
}

// threadModeSlackCall is one PostThreadMessage recorded by threadModeSlackFake.
type threadModeSlackCall struct {
	channelID string
	threadTS  string
	text      string
	blocks    []goslack.Block
	broadcast bool
}

// threadModeSlackFake records every thread reply, channel-root post and card
// update so the thread-mode tests can assert where each notification went.
type threadModeSlackFake struct {
	mockSlackService
	threadCalls     []threadModeSlackCall
	cardPosts       []string // channel IDs passed to PostMessageWithAttachment
	cardUpdates     []string // message timestamps passed to UpdateMessageWithAttachment
	nextThreadReply int
}

func (f *threadModeSlackFake) PostThreadMessage(_ context.Context, channelID, threadTS string, blocks []goslack.Block, text string, opts ...slacksvc.PostThreadOption) (string, error) {
	cfg := slacksvc.ApplyPostThreadOptions(opts...)
	f.threadCalls = append(f.threadCalls, threadModeSlackCall{
		channelID: channelID, threadTS: threadTS, text: text, blocks: blocks, broadcast: cfg.Broadcast,
	})
	f.nextThreadReply++
	return fmt.Sprintf("1700000001.%06d", f.nextThreadReply), nil
}

func (f *threadModeSlackFake) PostMessageWithAttachment(_ context.Context, channelID, _ string, _ goslack.Attachment) (string, error) {
	f.cardPosts = append(f.cardPosts, channelID)
	return "1700000002.000001", nil
}

func (f *threadModeSlackFake) UpdateMessageWithAttachment(_ context.Context, _, timestamp, _ string, _ goslack.Attachment) error {
	f.cardUpdates = append(f.cardUpdates, timestamp)
	return nil
}

// threadModeActionsRegistry registers testWorkspaceID as a thread-mode
// workspace with [case] actions on (enabled) or left at its default (off).
func threadModeActionsRegistry(enabled bool) *model.WorkspaceRegistry {
	r := model.NewWorkspaceRegistry()
	r.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: testWorkspaceID, Name: "Thread workspace"},
		CaseMode:    model.CaseModeThread,
		CaseActions: enabled,
	})
	return r
}

const (
	threadModeChannelID = "CTHREADMODE"
	threadModeThreadTS  = "1700000000.000500"
)

// newThreadModeCase persists an OPEN thread-mode case bound to
// threadModeChannelID / threadModeThreadTS.
func newThreadModeCase(t *testing.T, repo *memory.Repository) *model.Case {
	t.Helper()
	c, err := repo.Case().Create(context.Background(), testWorkspaceID, &model.Case{
		Title:          "Thread case",
		Status:         types.CaseStatusOpen,
		SlackChannelID: threadModeChannelID,
		SlackThreadTS:  threadModeThreadTS,
		BoardStatus:    "in_progress",
	})
	gt.NoError(t, err).Required()
	return c
}

// assertQuietCaseThreadReply checks that call is a single context block posted
// as a reply in the thread-mode case's thread and not broadcast to the channel.
func assertQuietCaseThreadReply(t *testing.T, call threadModeSlackCall) {
	t.Helper()
	gt.Value(t, call.channelID).Equal(threadModeChannelID)
	gt.Value(t, call.threadTS).Equal(threadModeThreadTS)
	gt.Bool(t, call.broadcast).False()
	gt.Array(t, call.blocks).Length(1).Required()
	gt.Value(t, call.blocks[0].BlockType()).Equal(goslack.MBTContext)
}

func TestActionUseCase_ThreadModeSlackNotifications(t *testing.T) {
	i18n.Init(i18n.LangEN)

	t.Run("creating an action posts a one-line notice in the case thread instead of a card", func(t *testing.T) {
		repo := memory.New()
		fake := &threadModeSlackFake{}
		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "https://example.com", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		c := newThreadModeCase(t, repo)

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Sample action", "Long description", "U001", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()

		gt.Array(t, fake.cardPosts).Length(0)
		gt.Array(t, fake.postedChannelIDs).Length(0)
		gt.Array(t, fake.threadCalls).Length(1).Required()
		call := fake.threadCalls[0]
		assertQuietCaseThreadReply(t, call)
		wantURL := fmt.Sprintf("https://example.com/ws/%s/cases/%d/actions/%d", testWorkspaceID, c.ID, created.ID)
		gt.Value(t, call.text).Equal(fmt.Sprintf(":heavy_plus_sign: <@UCREATOR> created action 📌 *<%s|Sample action>*", wantURL))
		// The description stays out of the notice: it is a single line.
		gt.Bool(t, strings.Contains(call.text, "Long description")).False()

		// The notice timestamp is recorded so the action counts as posted.
		gt.Value(t, created.SlackMessageTS).Equal("1700000001.000001")
		stored, err := repo.Action().Get(ctx, testWorkspaceID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.SlackMessageTS).Equal("1700000001.000001")
	})

	t.Run("an action created without a user token is attributed to the system", func(t *testing.T) {
		repo := memory.New()
		fake := &threadModeSlackFake{}
		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "", nil)
		c := newThreadModeCase(t, repo)

		_, err := actionUC.CreateAction(context.Background(), testWorkspaceID, c.ID, "a&b<c>", "", "", "", "", nil)
		gt.NoError(t, err).Required()

		gt.Array(t, fake.threadCalls).Length(1).Required()
		gt.Value(t, fake.threadCalls[0].text).Equal(":heavy_plus_sign: system created action 📌 *a&amp;b&lt;c&gt;*")
	})

	t.Run("updating an action posts a quiet notice naming the action and refreshes no card", func(t *testing.T) {
		repo := memory.New()
		fake := &threadModeSlackFake{}
		slot := usecase.NewNotificationSlotCoordinatorForTest(repo.NotificationSlot(), fake, time.Hour, nil)
		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "https://example.com", slot)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		c := newThreadModeCase(t, repo)

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Sample action", "", "", "", types.ActionStatusTodo, nil)
		gt.NoError(t, err).Required()
		fake.threadCalls = nil

		newStatus := types.ActionStatusInProgress
		_, err = actionUC.UpdateAction(ctx, testWorkspaceID, usecase.UpdateActionInput{
			ID:        created.ID,
			Status:    &newStatus,
			SlackSync: usecase.SlackSyncFull,
			Actor:     usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
		})
		gt.NoError(t, err).Required()

		// No card exists to refresh, and nothing reaches the channel root —
		// not even the notification slot, which a status change uses in
		// channel mode.
		gt.Array(t, fake.cardUpdates).Length(0)
		gt.Array(t, fake.cardPosts).Length(0)
		gt.Array(t, fake.postedChannelIDs).Length(0)
		gt.Array(t, fake.threadCalls).Length(1).Required()
		call := fake.threadCalls[0]
		assertQuietCaseThreadReply(t, call)
		wantURL := fmt.Sprintf("https://example.com/ws/%s/cases/%d/actions/%d", testWorkspaceID, c.ID, created.ID)
		gt.Value(t, call.text).Equal(fmt.Sprintf("📌 *<%s|Sample action>*\n:arrows_counterclockwise: <@UDOER> changed status: TODO -> IN_PROGRESS", wantURL))
	})

	t.Run("archiving an action posts the archive notice in the case thread", func(t *testing.T) {
		repo := memory.New()
		fake := &threadModeSlackFake{}
		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "", nil)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		c := newThreadModeCase(t, repo)

		created, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Sample action", "", "", "", "", nil)
		gt.NoError(t, err).Required()
		fake.threadCalls = nil

		_, err = actionUC.ArchiveAction(ctx, testWorkspaceID, created.ID, usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"})
		gt.NoError(t, err).Required()

		gt.Array(t, fake.cardUpdates).Length(0)
		gt.Array(t, fake.threadCalls).Length(1).Required()
		assertQuietCaseThreadReply(t, fake.threadCalls[0])
		gt.Value(t, fake.threadCalls[0].text).Equal(`:file_cabinet: <@UDOER> archived action "Sample action"`)
	})

	t.Run("re-posting an unsent action posts the creation notice", func(t *testing.T) {
		repo := memory.New()
		fake := &threadModeSlackFake{}
		ctx := context.Background()
		c := newThreadModeCase(t, repo)
		unsent, err := repo.Action().Create(ctx, testWorkspaceID, &model.Action{
			CaseID: c.ID, Title: "Sample action", Status: types.ActionStatusTodo,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "", nil)
		posted, err := actionUC.PostSlackMessageToAction(ctx, testWorkspaceID, unsent.ID)
		gt.NoError(t, err).Required()

		gt.Array(t, fake.cardPosts).Length(0)
		gt.Array(t, fake.threadCalls).Length(1).Required()
		assertQuietCaseThreadReply(t, fake.threadCalls[0])
		gt.Value(t, fake.threadCalls[0].text).Equal(":heavy_plus_sign: system created action 📌 *Sample action*")
		gt.Value(t, posted.SlackMessageTS).Equal("1700000001.000001")
	})
}

func TestActionUseCase_ActionsEnabled(t *testing.T) {
	channel := model.NewWorkspaceRegistry()
	channel.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: testWorkspaceID}})

	for name, tc := range map[string]struct {
		registry *model.WorkspaceRegistry
		want     bool
	}{
		"channel mode always has actions":    {channel, true},
		"thread mode with actions on":        {threadModeActionsRegistry(true), true},
		"thread mode defaults to no actions": {threadModeActionsRegistry(false), false},
		"no registry is the channel default": {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := usecase.NewActionUseCase(memory.New(), tc.registry, nil, "", nil).ActionsEnabled(context.Background(), testWorkspaceID)
			gt.NoError(t, err).Required()
			gt.Value(t, got).Equal(tc.want)
		})
	}

	t.Run("an unknown workspace is an error", func(t *testing.T) {
		_, err := usecase.NewActionUseCase(memory.New(), threadModeActionsRegistry(true), nil, "", nil).ActionsEnabled(context.Background(), "unknown-ws")
		gt.Error(t, err).Is(model.ErrWorkspaceNotFound)
	})
}
