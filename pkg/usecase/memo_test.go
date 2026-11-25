package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

const memoTestWorkspaceID = "memo-ws"
const memoDisabledWorkspaceID = "no-memo-ws"

// memoTestRegistry builds a registry with one memo-enabled workspace and one
// memo-disabled workspace.
func memoTestRegistry() *model.WorkspaceRegistry {
	reg := model.NewWorkspaceRegistry()
	reg.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: memoTestWorkspaceID, Name: "Memo WS"},
		MemoConfig: &config.MemoConfig{
			Description: "investigation memory",
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{ID: "memo_type", Name: "Type", Type: types.FieldTypeSelect, Required: true, Options: []config.FieldOption{
						{ID: "fact", Name: "Fact"},
						{ID: "hypothesis", Name: "Hypothesis"},
					}},
					{ID: "body", Name: "Body", Type: types.FieldTypeText},
					{ID: "owner", Name: "Owner", Type: types.FieldTypeUser},
				},
			},
		},
	})
	reg.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: memoDisabledWorkspaceID, Name: "No Memo WS"},
	})
	return reg
}

// seedCase creates a case directly in the repo and returns its ID.
func seedCase(t *testing.T, repo interfaces.Repository, ctx context.Context, c *model.Case) int64 {
	t.Helper()
	created, err := repo.Case().Create(ctx, memoTestWorkspaceID, c)
	gt.NoError(t, err).Required()
	return created.ID
}

func newMemoUC(t *testing.T) (*usecase.MemoUseCase, interfaces.Repository) {
	t.Helper()
	repo := memory.New()
	return usecase.NewMemoUseCase(repo, memoTestRegistry()), repo
}

func TestMemoUseCase_CreateMemo(t *testing.T) {
	t.Run("creates memo with valid fields and records creator from token", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

		created, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID: caseID,
			Title:  "first memo",
			FieldValues: map[string]model.FieldValue{
				"memo_type": {FieldID: "memo_type", Value: "fact"},
				"body":      {FieldID: "body", Value: "observed something"},
			},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, created.ID).NotEqual(model.MemoID(""))
		gt.Value(t, created.CaseID).Equal(caseID)
		gt.String(t, created.Title).Equal("first memo")
		gt.String(t, created.CreatorID).Equal("UCREATOR")
		gt.Bool(t, created.CreatedAt.IsZero()).False()

		// Read back from the repository and assert persisted field values.
		got, err := repo.Memo().Get(ctx, memoTestWorkspaceID, caseID, created.ID)
		gt.NoError(t, err).Required()
		gt.String(t, got.Title).Equal("first memo")
		gt.Value(t, got.FieldValues["memo_type"].Value).Equal("fact")
		gt.Value(t, got.FieldValues["memo_type"].Type).Equal(types.FieldTypeSelect)
		gt.Value(t, got.FieldValues["body"].Value).Equal("observed something")
	})

	t.Run("rejects a value that breaks the field semantic", func(t *testing.T) {
		reg := model.NewWorkspaceRegistry()
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: memoTestWorkspaceID, Name: "Memo WS"},
			MemoConfig: &config.MemoConfig{
				Description: "investigation memory",
				FieldSchema: &config.FieldSchema{
					Fields: []config.FieldDefinition{
						{ID: "channel", Name: "Channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
					},
				},
			},
		})
		repo := memory.New()
		uc := usecase.NewMemoUseCase(repo, reg)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

		_, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID:      caseID,
			Title:       "bad channel",
			FieldValues: map[string]model.FieldValue{"channel": {FieldID: "channel", Value: "#general"}},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
		gt.String(t, err.Error()).Contains(`Slack channel ID must be "C"`)

		memos, err := repo.Memo().List(ctx, memoTestWorkspaceID, caseID, interfaces.MemoListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, memos).Length(0)

		created, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID:      caseID,
			Title:       "good channel",
			FieldValues: map[string]model.FieldValue{"channel": {FieldID: "channel", Value: "C0123ABCD"}},
		})
		gt.NoError(t, err).Required()
		got, err := repo.Memo().Get(ctx, memoTestWorkspaceID, caseID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.FieldValues["channel"].Value).Equal("C0123ABCD")
	})

	t.Run("rejects missing required field", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

		_, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID:      caseID,
			Title:       "no type",
			FieldValues: map[string]model.FieldValue{"body": {FieldID: "body", Value: "x"}},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})

	t.Run("rejects unknown field id", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

		_, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID: caseID,
			Title:  "bad",
			FieldValues: map[string]model.FieldValue{
				"memo_type": {FieldID: "memo_type", Value: "fact"},
				"nope":      {FieldID: "nope", Value: "x"},
			},
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
	})

	t.Run("rejects write to memo-disabled workspace", func(t *testing.T) {
		repo := memory.New()
		uc := usecase.NewMemoUseCase(repo, memoTestRegistry())
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		created, err := repo.Case().Create(ctx, memoDisabledWorkspaceID, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})
		gt.NoError(t, err).Required()

		_, err = uc.CreateMemo(ctx, memoDisabledWorkspaceID, usecase.CreateMemoInput{CaseID: created.ID, Title: "x"})
		gt.Error(t, err).Is(usecase.ErrMemoNotEnabled)
	})

	t.Run("rejects non-existent case", func(t *testing.T) {
		uc, _ := newMemoUC(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		_, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: 99999, Title: "x"})
		gt.Error(t, err).Is(usecase.ErrCaseNotFound)
	})

	t.Run("agent context with no token records empty creator", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		seedCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, seedCtx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

		agentCtx := context.Background() // no auth token: agent/system context
		created, err := uc.CreateMemo(agentCtx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID:      caseID,
			Title:       "agent memo",
			FieldValues: map[string]model.FieldValue{"memo_type": {FieldID: "memo_type", Value: "hypothesis"}},
		})
		gt.NoError(t, err).Required()
		gt.String(t, created.CreatorID).Equal("")
	})
}

func TestMemoUseCase_UpdateMemo(t *testing.T) {
	t.Run("merges patch over existing and preserves untouched required field", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

		created, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID: caseID,
			Title:  "memo",
			FieldValues: map[string]model.FieldValue{
				"memo_type": {FieldID: "memo_type", Value: "fact"},
				"body":      {FieldID: "body", Value: "old"},
			},
		})
		gt.NoError(t, err).Required()

		newTitle := "memo edited"
		updated, err := uc.UpdateMemo(ctx, memoTestWorkspaceID, usecase.UpdateMemoInput{
			ID:          created.ID,
			CaseID:      caseID,
			Title:       &newTitle,
			FieldValues: map[string]model.FieldValue{"body": {FieldID: "body", Value: "new"}},
		})
		gt.NoError(t, err).Required()
		gt.String(t, updated.Title).Equal("memo edited")
		gt.Value(t, updated.FieldValues["body"].Value).Equal("new")
		// memo_type was not in the patch but must be preserved (and still valid).
		gt.Value(t, updated.FieldValues["memo_type"].Value).Equal("fact")
	})
}

func TestMemoUseCase_ArchivedIsReadOnly(t *testing.T) {
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
	fields := map[string]model.FieldValue{
		"memo_type": {FieldID: "memo_type", Value: "fact"},
		"body":      {FieldID: "body", Value: "old"},
	}

	t.Run("an archived memo cannot be edited until it is unarchived", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})
		created, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "memo", FieldValues: fields})
		gt.NoError(t, err).Required()
		_, err = uc.ArchiveMemo(ctx, memoTestWorkspaceID, caseID, created.ID)
		gt.NoError(t, err).Required()

		newTitle := "memo edited"
		in := usecase.UpdateMemoInput{
			ID:          created.ID,
			CaseID:      caseID,
			Title:       &newTitle,
			FieldValues: map[string]model.FieldValue{"body": {FieldID: "body", Value: "new"}},
		}
		_, err = uc.UpdateMemo(ctx, memoTestWorkspaceID, in)
		gt.Error(t, err).Is(usecase.ErrMemoArchived)

		stored, err := repo.Memo().Get(ctx, memoTestWorkspaceID, caseID, created.ID)
		gt.NoError(t, err).Required()
		gt.Bool(t, stored.IsArchived()).True()
		gt.String(t, stored.Title).Equal("memo")
		gt.Value(t, stored.FieldValues["body"].Value).Equal("old")

		_, err = uc.UnarchiveMemo(ctx, memoTestWorkspaceID, caseID, created.ID)
		gt.NoError(t, err).Required()
		updated, err := uc.UpdateMemo(ctx, memoTestWorkspaceID, in)
		gt.NoError(t, err).Required()
		gt.String(t, updated.Title).Equal("memo edited")
		gt.Value(t, updated.FieldValues["body"].Value).Equal("new")
	})

	t.Run("an archived case takes no new memo and no memo edit", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})
		created, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "memo", FieldValues: fields})
		gt.NoError(t, err).Required()

		c, err := repo.Case().Get(ctx, memoTestWorkspaceID, caseID)
		gt.NoError(t, err).Required()
		archivedAt := time.Now().UTC()
		c.Status = types.CaseStatusClosed
		c.ArchivedAt = &archivedAt
		_, err = repo.Case().Update(ctx, memoTestWorkspaceID, c)
		gt.NoError(t, err).Required()

		_, err = uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "another", FieldValues: fields})
		gt.Error(t, err).Is(usecase.ErrCaseArchived)

		newTitle := "memo edited"
		_, err = uc.UpdateMemo(ctx, memoTestWorkspaceID, usecase.UpdateMemoInput{ID: created.ID, CaseID: caseID, Title: &newTitle})
		gt.Error(t, err).Is(usecase.ErrCaseArchived)

		memos, err := repo.Memo().List(ctx, memoTestWorkspaceID, caseID, interfaces.MemoListOptions{ArchiveScope: interfaces.MemoArchiveScopeAll})
		gt.NoError(t, err).Required()
		gt.Array(t, memos).Length(1).Required()
		gt.Value(t, memos[0].ID).Equal(created.ID)
		gt.String(t, memos[0].Title).Equal("memo")
	})
}

func TestMemoUseCase_TextPattern(t *testing.T) {
	setup := func(t *testing.T) (*usecase.MemoUseCase, *memory.Memory, context.Context, int64) {
		t.Helper()
		reg := model.NewWorkspaceRegistry()
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: memoTestWorkspaceID, Name: "Memo WS"},
			MemoConfig: &config.MemoConfig{
				Description: "investigation memory",
				FieldSchema: &config.FieldSchema{
					Fields: []config.FieldDefinition{
						{ID: "cve", Name: "CVE", Type: types.FieldTypeText,
							Validation: config.FieldValidation{Pattern: "CVE-[0-9]{4}-[0-9]{4,}"}},
					},
				},
			},
		})
		repo := memory.New()
		uc := usecase.NewMemoUseCase(repo, reg)
		ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
		caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})
		return uc, repo, ctx, caseID
	}
	cve := func(value string) map[string]model.FieldValue {
		return map[string]model.FieldValue{"cve": {FieldID: "cve", Value: value}}
	}

	t.Run("a mismatching value is rejected and no memo is created", func(t *testing.T) {
		uc, repo, ctx, caseID := setup(t)
		_, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID: caseID, Title: "bad", FieldValues: cve("cve-2024-3094"),
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)
		gt.String(t, err.Error()).Contains("CVE-[0-9]{4}-[0-9]{4,}")

		memos, err := repo.Memo().List(ctx, memoTestWorkspaceID, caseID, interfaces.MemoListOptions{})
		gt.NoError(t, err).Required()
		gt.Array(t, memos).Length(0)
	})

	t.Run("a mismatching update is rejected and the memo keeps its value", func(t *testing.T) {
		uc, repo, ctx, caseID := setup(t)
		created, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID: caseID, Title: "good", FieldValues: cve("CVE-2024-3094"),
		})
		gt.NoError(t, err).Required()

		_, err = uc.UpdateMemo(ctx, memoTestWorkspaceID, usecase.UpdateMemoInput{
			ID: created.ID, CaseID: caseID, FieldValues: cve("CVE-24-3094"),
		})
		gt.Error(t, err).Is(model.ErrCaseFieldValidation)

		got, err := repo.Memo().Get(ctx, memoTestWorkspaceID, caseID, created.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, got.FieldValues["cve"].Value).Equal("CVE-2024-3094")
	})
}

func TestMemoUseCase_ArchiveAndList(t *testing.T) {
	uc, repo := newMemoUC(t)
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
	caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

	mk := func(title string) *model.Memo {
		m, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
			CaseID:      caseID,
			Title:       title,
			FieldValues: map[string]model.FieldValue{"memo_type": {FieldID: "memo_type", Value: "fact"}},
		})
		gt.NoError(t, err).Required()
		return m
	}
	keep := mk("keep")
	drop := mk("drop")

	archived, err := uc.ArchiveMemo(ctx, memoTestWorkspaceID, caseID, drop.ID)
	gt.NoError(t, err).Required()
	gt.Bool(t, archived.IsArchived()).True()

	active, err := uc.ListMemosByCase(ctx, memoTestWorkspaceID, caseID, interfaces.MemoArchiveScopeActiveOnly)
	gt.NoError(t, err).Required()
	gt.Array(t, active).Length(1).Required()
	gt.Value(t, active[0].ID).Equal(keep.ID)

	onlyArchived, err := uc.ListMemosByCase(ctx, memoTestWorkspaceID, caseID, interfaces.MemoArchiveScopeArchivedOnly)
	gt.NoError(t, err).Required()
	gt.Array(t, onlyArchived).Length(1).Required()
	gt.Value(t, onlyArchived[0].ID).Equal(drop.ID)

	all, err := uc.ListMemosByCase(ctx, memoTestWorkspaceID, caseID, interfaces.MemoArchiveScopeAll)
	gt.NoError(t, err).Required()
	gt.Array(t, all).Length(2)

	restored, err := uc.UnarchiveMemo(ctx, memoTestWorkspaceID, caseID, drop.ID)
	gt.NoError(t, err).Required()
	gt.Bool(t, restored.IsArchived()).False()

	activeAfter, err := uc.ListMemosByCase(ctx, memoTestWorkspaceID, caseID, interfaces.MemoArchiveScopeActiveOnly)
	gt.NoError(t, err).Required()
	gt.Array(t, activeAfter).Length(2)
}

func TestMemoUseCase_PrivateCaseAccessControl(t *testing.T) {
	newPrivateCase := func(t *testing.T, repo interfaces.Repository, ctx context.Context) int64 {
		t.Helper()
		created, err := repo.Case().Create(ctx, memoTestWorkspaceID, &model.Case{
			ReporterID:     "U-REP",
			Title:          "Private",
			IsPrivate:      true,
			ChannelUserIDs: []string{"UMEMBER"},
			AssigneeIDs:    []string{},
		})
		gt.NoError(t, err).Required()
		return created.ID
	}

	validFields := map[string]model.FieldValue{"memo_type": {FieldID: "memo_type", Value: "fact"}}

	t.Run("non-member create is denied, member create succeeds", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		caseID := newPrivateCase(t, repo, memberCtx)

		_, err := uc.CreateMemo(nonMemberCtx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "x", FieldValues: validFields})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		_, err = uc.CreateMemo(memberCtx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "ok", FieldValues: validFields})
		gt.NoError(t, err).Required()
	})

	t.Run("no-token agent context bypasses access control", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		caseID := newPrivateCase(t, repo, memberCtx)

		_, err := uc.CreateMemo(context.Background(), memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "agent", FieldValues: validFields})
		gt.NoError(t, err).Required()
	})

	t.Run("non-member list returns empty", func(t *testing.T) {
		uc, repo := newMemoUC(t)
		memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UMEMBER"})
		nonMemberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UOTHER"})
		caseID := newPrivateCase(t, repo, memberCtx)
		_, err := uc.CreateMemo(memberCtx, memoTestWorkspaceID, usecase.CreateMemoInput{CaseID: caseID, Title: "m", FieldValues: validFields})
		gt.NoError(t, err).Required()

		memos, err := uc.ListMemosByCase(nonMemberCtx, memoTestWorkspaceID, caseID, interfaces.MemoArchiveScopeActiveOnly)
		gt.NoError(t, err).Required()
		gt.Array(t, memos).Length(0)
	})
}

func TestMemoUseCase_UserFieldMustExist(t *testing.T) {
	uc, repo := newMemoUC(t)
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
	caseID := seedCase(t, repo, ctx, &model.Case{ReporterID: "UCREATOR", Title: "Case", AssigneeIDs: []string{}})

	_, err := uc.CreateMemo(ctx, memoTestWorkspaceID, usecase.CreateMemoInput{
		CaseID: caseID,
		Title:  "memo",
		FieldValues: map[string]model.FieldValue{
			"memo_type": {FieldID: "memo_type", Value: "fact"},
			"owner":     {FieldID: "owner", Value: "UGHOST"},
		},
	})
	gt.Error(t, err).Is(usecase.ErrUnknownUser)
}
