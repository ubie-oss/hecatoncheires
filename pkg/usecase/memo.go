package usecase

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
)

// ErrMemoNotEnabled is returned when a memo write targets a workspace that has
// not configured the memo feature (no [memo] field schema).
var ErrMemoNotEnabled = goerr.New("memo feature is not enabled for this workspace")

// ErrMemoNotFound is returned when a memo cannot be located within its Case.
var ErrMemoNotFound = goerr.New("memo not found")

// ErrMemoArchived is returned when UpdateMemo targets an archived memo. An
// archived memo is read-only; unarchive it first.
var ErrMemoArchived = goerr.New("memo is archived")

// MemoUseCase orchestrates Case-scoped memo operations. Every write funnels
// through the same field validation + access control gate so the GraphQL/WebUI
// path and the agent-tool path enforce identical rules.
type MemoUseCase struct {
	repo     interfaces.Repository
	registry *model.WorkspaceRegistry
}

// NewMemoUseCase constructs a MemoUseCase.
func NewMemoUseCase(repo interfaces.Repository, registry *model.WorkspaceRegistry) *MemoUseCase {
	return &MemoUseCase{repo: repo, registry: registry}
}

// CreateMemoInput is the unified input for MemoUseCase.CreateMemo.
type CreateMemoInput struct {
	CaseID      int64
	Title       string
	FieldValues map[string]model.FieldValue
}

// UpdateMemoInput is the unified input for MemoUseCase.UpdateMemo. Title is a
// pointer so an absent title means "no change". FieldValues is a patch merged
// over the existing values; the merged set is then validated in full.
type UpdateMemoInput struct {
	ID          model.MemoID
	CaseID      int64
	Title       *string
	FieldValues map[string]model.FieldValue
}

// MemoConfiguration describes a workspace's memo configuration for the WebUI:
// the strong definition and the custom field definitions used to render the
// memo form. Both are empty when the workspace has not enabled memos.
type MemoConfiguration struct {
	Enabled     bool
	Description string
	Fields      []config.FieldDefinition
}

// MemoConfiguration returns the workspace memo configuration for the WebUI.
// When the workspace has no memo schema it returns a disabled configuration
// (Enabled=false, empty fields) rather than an error, so the frontend can hide
// the Memos tab.
func (uc *MemoUseCase) MemoConfiguration(workspaceID string) (*MemoConfiguration, error) {
	if uc.registry == nil {
		return &MemoConfiguration{}, nil
	}
	entry, err := uc.registry.Get(workspaceID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to resolve workspace for memo configuration",
			goerr.V("workspace_id", workspaceID))
	}
	if entry.MemoConfig == nil || !entry.MemoConfig.Enabled() {
		return &MemoConfiguration{}, nil
	}
	return &MemoConfiguration{
		Enabled:     true,
		Description: entry.MemoConfig.Description,
		Fields:      entry.MemoConfig.FieldSchema.Fields,
	}, nil
}

// memoFieldValidator returns the field validator for the workspace's memo
// schema, or nil when the workspace has not enabled memos.
func (uc *MemoUseCase) memoFieldValidator(workspaceID string) *model.FieldValidator {
	if uc.registry == nil {
		return nil
	}
	entry, err := uc.registry.Get(workspaceID)
	if err != nil || entry.MemoConfig == nil || !entry.MemoConfig.Enabled() {
		return nil
	}
	return model.NewFieldValidator(entry.MemoConfig.FieldSchema)
}

// memoEnabled reports whether the workspace has the memo feature configured.
func (uc *MemoUseCase) memoEnabled(workspaceID string) bool {
	if uc.registry == nil {
		return false
	}
	entry, err := uc.registry.Get(workspaceID)
	if err != nil {
		return false
	}
	return entry.MemoConfig.Enabled()
}

// CreateMemo validates and persists a new memo within a Case. Field values are
// fully validated (required fields enforced, unknown field ids rejected).
func (uc *MemoUseCase) CreateMemo(ctx context.Context, workspaceID string, in CreateMemoInput) (*model.Memo, error) {
	if !uc.memoEnabled(workspaceID) {
		return nil, goerr.Wrap(ErrMemoNotEnabled, "memo create rejected", goerr.V("workspace_id", workspaceID))
	}
	parent, err := loadCaseForWrite(ctx, uc.repo, workspaceID, in.CaseID)
	if err != nil {
		return nil, err
	}
	if err := assertCaseNotArchived(parent); err != nil {
		return nil, goerr.Wrap(err, "memo create rejected")
	}

	enriched, err := uc.validateMemoFields(ctx, workspaceID, in.FieldValues)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	memo := &model.Memo{
		ID:          model.NewMemoID(),
		WorkspaceID: workspaceID,
		CaseID:      in.CaseID,
		Title:       strings.TrimSpace(in.Title),
		FieldValues: enriched,
		CreatorID:   creatorFromContext(ctx),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := memo.Validate(); err != nil {
		return nil, err
	}

	created, err := uc.repo.Memo().Create(ctx, workspaceID, memo)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to create memo", goerr.V(CaseIDKey, in.CaseID))
	}
	return created, nil
}

// UpdateMemo applies a partial patch to an existing memo. The patch field values
// are merged over the stored values and the merged set is validated in full.
func (uc *MemoUseCase) UpdateMemo(ctx context.Context, workspaceID string, in UpdateMemoInput) (*model.Memo, error) {
	if !uc.memoEnabled(workspaceID) {
		return nil, goerr.Wrap(ErrMemoNotEnabled, "memo update rejected", goerr.V("workspace_id", workspaceID))
	}
	parent, err := loadCaseForWrite(ctx, uc.repo, workspaceID, in.CaseID)
	if err != nil {
		return nil, err
	}
	if err := assertCaseNotArchived(parent); err != nil {
		return nil, goerr.Wrap(err, "memo update rejected", goerr.V("memo_id", in.ID))
	}

	memo, err := uc.repo.Memo().Get(ctx, workspaceID, in.CaseID, in.ID)
	if err != nil {
		return nil, goerr.Wrap(ErrMemoNotFound, "memo not found",
			goerr.V(CaseIDKey, in.CaseID), goerr.V("memo_id", in.ID))
	}
	// An archived memo is read-only. ArchiveMemo / UnarchiveMemo are the only
	// writes it accepts.
	if memo.IsArchived() {
		return nil, goerr.Wrap(ErrMemoArchived, "archived memo cannot be edited; unarchive it first",
			goerr.V(CaseIDKey, in.CaseID), goerr.V("memo_id", in.ID))
	}

	merged := mergeFieldValues(memo.FieldValues, in.FieldValues)
	enriched, err := uc.validateMemoFields(ctx, workspaceID, merged)
	if err != nil {
		return nil, err
	}

	if in.Title != nil {
		memo.Title = strings.TrimSpace(*in.Title)
	}
	memo.FieldValues = enriched
	memo.UpdatedAt = time.Now().UTC()
	if err := memo.Validate(); err != nil {
		return nil, err
	}

	updated, err := uc.repo.Memo().Update(ctx, workspaceID, memo)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update memo",
			goerr.V(CaseIDKey, in.CaseID), goerr.V("memo_id", in.ID))
	}
	return updated, nil
}

// ArchiveMemo soft-deletes a memo by setting ArchivedAt. Idempotent: archiving
// an already-archived memo is a no-op write that returns the current state.
func (uc *MemoUseCase) ArchiveMemo(ctx context.Context, workspaceID string, caseID int64, id model.MemoID) (*model.Memo, error) {
	return uc.setMemoArchived(ctx, workspaceID, caseID, id, true)
}

// UnarchiveMemo restores a soft-deleted memo by clearing ArchivedAt.
func (uc *MemoUseCase) UnarchiveMemo(ctx context.Context, workspaceID string, caseID int64, id model.MemoID) (*model.Memo, error) {
	return uc.setMemoArchived(ctx, workspaceID, caseID, id, false)
}

func (uc *MemoUseCase) setMemoArchived(ctx context.Context, workspaceID string, caseID int64, id model.MemoID, archived bool) (*model.Memo, error) {
	if _, err := loadCaseForWrite(ctx, uc.repo, workspaceID, caseID); err != nil {
		return nil, err
	}
	memo, err := uc.repo.Memo().Get(ctx, workspaceID, caseID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrMemoNotFound, "memo not found",
			goerr.V(CaseIDKey, caseID), goerr.V("memo_id", id))
	}

	if archived {
		now := time.Now().UTC()
		memo.ArchivedAt = &now
	} else {
		memo.ArchivedAt = nil
	}
	memo.UpdatedAt = time.Now().UTC()

	updated, err := uc.repo.Memo().Update(ctx, workspaceID, memo)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update memo archive state",
			goerr.V(CaseIDKey, caseID), goerr.V("memo_id", id))
	}
	return updated, nil
}

// GetMemo loads a single memo within a Case. Returns ErrAccessDenied for a
// private case the (token-bearing) caller cannot access.
func (uc *MemoUseCase) GetMemo(ctx context.Context, workspaceID string, caseID int64, id model.MemoID) (*model.Memo, error) {
	if _, err := loadCaseForWrite(ctx, uc.repo, workspaceID, caseID); err != nil {
		return nil, err
	}
	memo, err := uc.repo.Memo().Get(ctx, workspaceID, caseID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrMemoNotFound, "memo not found",
			goerr.V(CaseIDKey, caseID), goerr.V("memo_id", id))
	}
	return memo, nil
}

// ListMemosByCase returns the memos of a Case filtered by scope. For a private
// case the (token-bearing) caller cannot access, it returns an empty slice
// rather than an error so list surfaces degrade quietly (read-side restriction).
func (uc *MemoUseCase) ListMemosByCase(ctx context.Context, workspaceID string, caseID int64, scope interfaces.MemoArchiveScope) ([]*model.Memo, error) {
	caseModel, err := uc.repo.Case().Get(ctx, workspaceID, caseID)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, caseID))
	}
	if token, tokenErr := auth.TokenFromContext(ctx); tokenErr == nil && !model.IsCaseAccessible(caseModel, token.Sub) {
		return []*model.Memo{}, nil
	}

	memos, err := uc.repo.Memo().List(ctx, workspaceID, caseID, interfaces.MemoListOptions{ArchiveScope: scope})
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list memos", goerr.V(CaseIDKey, caseID))
	}
	return memos, nil
}

// validateMemoFields runs the workspace memo validator in full mode (required
// fields enforced, unknown ids rejected) and verifies every referenced user id
// exists. A nil validator (memos disabled) is unreachable here because callers
// guard with memoEnabled first; it is treated defensively as "no field checks".
func (uc *MemoUseCase) validateMemoFields(ctx context.Context, workspaceID string, fieldValues map[string]model.FieldValue) (map[string]model.FieldValue, error) {
	enriched := fieldValues
	if validator := uc.memoFieldValidator(workspaceID); validator != nil {
		v, err := validator.ValidateCaseFieldsAll(fieldValues)
		if err != nil {
			return nil, goerr.Wrap(err, "memo field validation failed", goerr.V("workspace_id", workspaceID))
		}
		enriched = v
	}
	if err := uc.verifyMemoUsersExist(ctx, enriched); err != nil {
		return nil, err
	}
	return enriched, nil
}

// verifyMemoUsersExist confirms every user id referenced by user / multi-user
// field values exists in the SlackUser store (Slack sync delay is treated as
// non-existence, mirroring the case write contract).
func (uc *MemoUseCase) verifyMemoUsersExist(ctx context.Context, fieldValues map[string]model.FieldValue) error {
	idSet := make(map[string]struct{})
	for _, fv := range fieldValues {
		switch fv.Type {
		case types.FieldTypeUser:
			if s, ok := fv.Value.(string); ok && s != "" {
				idSet[s] = struct{}{}
			}
		case types.FieldTypeMultiUser:
			for _, s := range coerceUserIDSlice(fv.Value) {
				if s != "" {
					idSet[s] = struct{}{}
				}
			}
		}
	}
	if len(idSet) == 0 {
		return nil
	}

	ids := make([]model.SlackUserID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, model.SlackUserID(id))
	}
	found, err := uc.repo.SlackUser().GetByIDs(ctx, ids)
	if err != nil {
		return goerr.Wrap(err, "failed to look up users for memo write")
	}

	var missing []string
	for id := range idSet {
		if _, ok := found[model.SlackUserID(id)]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return goerr.Wrap(ErrUnknownUser,
			"unknown user id(s): "+strings.Join(missing, ", "),
			goerr.V("missing_user_ids", missing))
	}
	return nil
}
