package usecase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
)

// ValidationIssueKind identifies which consistency check reported an issue.
type ValidationIssueKind string

const (
	// IssueKindFieldValue is a stored field value that violates its schema type
	// (a wrong value shape, an option id the definition no longer lists, a date
	// that is not RFC3339, ...).
	IssueKindFieldValue ValidationIssueKind = "field_value"
	// IssueKindFieldTypeMismatch is a stored FieldValue.Type that no longer
	// matches the schema type for that field.
	IssueKindFieldTypeMismatch ValidationIssueKind = "field_type_mismatch"
	// IssueKindCaseRefMissing is a case_ref / multi_case_ref value pointing at a
	// Case that does not exist in the field's configured reference_workspace.
	IssueKindCaseRefMissing ValidationIssueKind = "case_ref_missing"
	// IssueKindBoardStatus is a thread-mode Case whose BoardStatus is empty or
	// is not part of the workspace's configured case status set.
	IssueKindBoardStatus ValidationIssueKind = "board_status_invalid"
	// IssueKindLifecycleMismatch is a thread-mode Case whose lifecycle Status
	// disagrees with whether its BoardStatus is configured as closed.
	IssueKindLifecycleMismatch ValidationIssueKind = "lifecycle_status_mismatch"
	// IssueKindArchivedNotClosed is an archived Case whose lifecycle Status is
	// not CLOSED. Only a CLOSED case may be archived, and ReopenCase /
	// UpdateCaseStatus refuse to take an archived case back to OPEN, so this
	// state is unreachable through the usecase layer. It is reported anyway
	// because if it ever arises the Case appears in no list, no board, no
	// dashboard and no Job scan — an operator has no other way to find it.
	IssueKindArchivedNotClosed ValidationIssueKind = "archived_case_not_closed"
	// IssueKindActionStatus is an Action whose Status is not part of the
	// workspace's configured action status set.
	IssueKindActionStatus ValidationIssueKind = "action_status_invalid"
	// IssueKindActionsDisabled is an Action stored in a thread-mode workspace
	// whose configuration does not enable Actions ([case] actions is off, the
	// default). It arises when the option is turned off after Actions were
	// created. The Web UI then shows no Related Actions section and the thread-
	// mode board lists Cases, so the Action is visible nowhere in the UI.
	IssueKindActionsDisabled ValidationIssueKind = "action_actions_disabled"
)

// ValidationTargetKind names the kind of entity an issue was found on.
type ValidationTargetKind string

const (
	TargetKindCase   ValidationTargetKind = "case"
	TargetKindAction ValidationTargetKind = "action"
	TargetKindMemo   ValidationTargetKind = "memo"
)

// ValidationTarget identifies one persisted entity. CaseID is always set:
// Actions and Memos are both Case-scoped.
type ValidationTarget struct {
	Kind     ValidationTargetKind
	CaseID   int64
	ActionID int64        // set only when Kind is TargetKindAction
	MemoID   model.MemoID // set only when Kind is TargetKindMemo
}

// String renders the target for log output, e.g. "case:42" or "memo:42/01H...".
func (t ValidationTarget) String() string {
	switch t.Kind {
	case TargetKindAction:
		return fmt.Sprintf("action:%d/%d", t.CaseID, t.ActionID)
	case TargetKindMemo:
		return fmt.Sprintf("memo:%d/%s", t.CaseID, t.MemoID)
	default:
		return fmt.Sprintf("case:%d", t.CaseID)
	}
}

// compare orders targets so the sample an issue retains does not depend on
// repository iteration order.
func (t ValidationTarget) compare(other ValidationTarget) int {
	if c := strings.Compare(string(t.Kind), string(other.Kind)); c != 0 {
		return c
	}
	if t.CaseID != other.CaseID {
		if t.CaseID < other.CaseID {
			return -1
		}
		return 1
	}
	if t.ActionID != other.ActionID {
		if t.ActionID < other.ActionID {
			return -1
		}
		return 1
	}
	return strings.Compare(string(t.MemoID), string(other.MemoID))
}

// ValidationIssue is one GROUP of identical inconsistencies within a single
// workspace. A configuration change affects a whole workspace uniformly, so
// reporting every offending entity would bury the operator in output: Count is
// how many ENTITIES hit the same (Kind, FieldID) problem — never how many
// individual bad values they hold — and Sample together with Expected / Actual /
// Message describe only the lowest-ordered one.
//
// Actual carries the offending value of the sample. For IssueKindCaseRefMissing
// it is every missing case id of that one entity, comma-separated in ascending
// order, because a multi_case_ref value can point at several absent Cases.
type ValidationIssue struct {
	WorkspaceID string
	Kind        ValidationIssueKind
	FieldID     string // empty for the status kinds
	Count       int64
	Sample      ValidationTarget
	Expected    string
	Actual      string
	Message     string
}

// ValidationResult holds the results of a DB consistency check.
type ValidationResult struct {
	Issues []ValidationIssue
}

// HasIssues returns true if there are any validation issues.
func (r *ValidationResult) HasIssues() bool {
	return len(r.Issues) > 0
}

// TotalCount returns how many individual entities are inconsistent, across
// every issue group.
func (r *ValidationResult) TotalCount() int64 {
	var total int64
	for _, issue := range r.Issues {
		total += issue.Count
	}
	return total
}

// issueKey groups occurrences that should collapse into one reported issue.
type issueKey struct {
	kind    ValidationIssueKind
	fieldID string
}

// issueAccumulator folds occurrences into per-(kind, field) groups for one
// workspace, keeping a count and the lowest-ordered target as the sample.
type issueAccumulator struct {
	workspaceID string
	groups      map[issueKey]*ValidationIssue
}

func newIssueAccumulator(workspaceID string) *issueAccumulator {
	return &issueAccumulator{
		workspaceID: workspaceID,
		groups:      make(map[issueKey]*ValidationIssue),
	}
}

func (a *issueAccumulator) add(kind ValidationIssueKind, fieldID string, target ValidationTarget, expected, actual, message string) {
	key := issueKey{kind: kind, fieldID: fieldID}
	existing, ok := a.groups[key]
	if !ok {
		a.groups[key] = &ValidationIssue{
			WorkspaceID: a.workspaceID,
			Kind:        kind,
			FieldID:     fieldID,
			Count:       1,
			Sample:      target,
			Expected:    expected,
			Actual:      actual,
			Message:     message,
		}
		return
	}

	existing.Count++
	// Keep the lowest-ordered target so the reported sample is stable across
	// runs and identical between the memory and Firestore backends.
	if target.compare(existing.Sample) < 0 {
		existing.Sample = target
		existing.Expected = expected
		existing.Actual = actual
		existing.Message = message
	}
}

func (a *issueAccumulator) issues() []ValidationIssue {
	out := make([]ValidationIssue, 0, len(a.groups))
	for _, issue := range a.groups {
		out = append(out, *issue)
	}
	slices.SortFunc(out, compareIssues)
	return out
}

func compareIssues(a, b ValidationIssue) int {
	if c := strings.Compare(a.WorkspaceID, b.WorkspaceID); c != 0 {
		return c
	}
	if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
		return c
	}
	return strings.Compare(a.FieldID, b.FieldID)
}

// ValidateDB checks the persisted Cases, Actions and Memos of every workspace
// registered with this process against that workspace's configuration and
// reports the mismatches. It reads only; nothing is repaired or rewritten.
//
// What it deliberately does NOT report: a field id stored under a definition the
// config no longer has, and a required field with no stored value. Both are
// consequences of editing the configuration that the project accepts. See
// CLAUDE.md § "DB consistency checks".
func (uc *UseCases) ValidateDB(ctx context.Context) (*ValidationResult, error) {
	return uc.ValidateDBWithConfig(ctx, uc.workspaceRegistry)
}

// ValidateDBWithConfig runs the checks ValidateDB describes against a
// caller-supplied configuration instead of the one this process was started
// with. It exists for the HTTP check endpoint, where an operator submits
// candidate workspace configuration and asks whether the persisted data would
// still be consistent with it — the answer must not depend on what the running
// process happens to have loaded.
func (uc *UseCases) ValidateDBWithConfig(ctx context.Context, registry *model.WorkspaceRegistry) (*ValidationResult, error) {
	if registry == nil {
		return nil, goerr.New("workspace registry is required for the consistency check")
	}

	result := &ValidationResult{}

	for _, entry := range registry.List() {
		issues, err := uc.validateWorkspaceData(ctx, entry)
		if err != nil {
			return nil, err
		}
		result.Issues = append(result.Issues, issues...)
	}

	slices.SortFunc(result.Issues, compareIssues)
	return result, nil
}

func (uc *UseCases) validateWorkspaceData(ctx context.Context, entry *model.WorkspaceEntry) ([]ValidationIssue, error) {
	wsID := entry.Workspace.ID
	acc := newIssueAccumulator(wsID)

	caseIDs, refWanted, err := uc.validateCases(ctx, entry, acc)
	if err != nil {
		return nil, err
	}
	if err := uc.validateCaseRefs(ctx, wsID, refWanted, acc); err != nil {
		return nil, err
	}
	if err := uc.validateActionStatuses(ctx, entry, acc); err != nil {
		return nil, err
	}
	if err := uc.validateMemoFields(ctx, entry, caseIDs, acc); err != nil {
		return nil, err
	}

	return acc.issues(), nil
}

// caseRefWant records which Cases a reference workspace is expected to hold and
// which entities referenced them.
type caseRefWant struct {
	// fieldID -> referenced case id -> referring targets.
	byField map[string]map[int64][]ValidationTarget
}

// validateCases walks every Case in the workspace once, checking field values
// and the thread-mode status invariants. It returns the Case ids it saw (the
// Memo pass needs them) and the case references it collected: the existence
// lookups must happen after the scan, because ScanAll forbids calling back into
// the repository from its callback.
func (uc *UseCases) validateCases(ctx context.Context, entry *model.WorkspaceEntry, acc *issueAccumulator) ([]int64, map[string]*caseRefWant, error) {
	wsID := entry.Workspace.ID

	var validator *model.FieldValidator
	defByID := make(map[string]config.FieldDefinition)
	if entry.FieldSchema != nil {
		validator = model.NewFieldValidator(entry.FieldSchema)
		for _, fd := range entry.FieldSchema.Fields {
			defByID[fd.ID] = fd
		}
	}

	var caseIDs []int64
	refWanted := make(map[string]*caseRefWant)

	err := uc.repo.Case().ScanAll(ctx, wsID, func(c *model.Case) error {
		caseIDs = append(caseIDs, c.ID)
		target := ValidationTarget{Kind: TargetKindCase, CaseID: c.ID}

		if validator != nil {
			addFieldViolations(acc, target, defByID, c.FieldValues, validator.ValidateStored(c.FieldValues))
			collectCaseRefs(refWanted, target, defByID, c.FieldValues)
		}
		checkCaseStatuses(acc, entry, c, target)
		checkCaseArchiveState(acc, c, target)
		return nil
	})
	if err != nil {
		return nil, nil, goerr.Wrap(err, "failed to scan cases for consistency check",
			goerr.V("workspace_id", wsID))
	}

	return caseIDs, refWanted, nil
}

// addFieldViolations maps the validator's per-field violations onto issue kinds.
func addFieldViolations(
	acc *issueAccumulator,
	target ValidationTarget,
	defByID map[string]config.FieldDefinition,
	values map[string]model.FieldValue,
	violations []model.FieldViolation,
) {
	for _, v := range violations {
		kind := IssueKindFieldValue
		if errors.Is(v.Err, model.ErrStoredFieldTypeMismatch) {
			kind = IssueKindFieldTypeMismatch
		}

		expected := ""
		if fd, ok := defByID[v.FieldID]; ok {
			var qualifiers []string
			if fd.Semantic != "" {
				qualifiers = append(qualifiers, string(fd.Semantic))
			}
			if p := fd.Validation.Pattern; p != "" {
				qualifiers = append(qualifiers, "pattern="+string(p))
			}
			expected = string(fd.Type)
			if len(qualifiers) > 0 {
				expected = fmt.Sprintf("%s (%s)", fd.Type, strings.Join(qualifiers, ", "))
			}
		}
		actual := ""
		if fv, ok := values[v.FieldID]; ok {
			if kind == IssueKindFieldTypeMismatch {
				actual = string(fv.Type)
			} else {
				actual = fmt.Sprint(fv.Value)
			}
		}

		acc.add(kind, v.FieldID, target, expected, actual, v.Err.Error())
	}
}

// collectCaseRefs records every case reference so their targets can be looked up
// in one batch per reference workspace after the scan finishes.
func collectCaseRefs(
	refWanted map[string]*caseRefWant,
	target ValidationTarget,
	defByID map[string]config.FieldDefinition,
	values map[string]model.FieldValue,
) {
	for fieldID, fv := range values {
		fd, ok := defByID[fieldID]
		if !ok || !fd.Type.IsCaseRef() || fd.ReferenceWorkspace == "" {
			continue
		}

		// Extract according to the CURRENT schema type, not the stored one: a
		// stale stored Type is itself reported separately, and the references
		// must be checked against the configuration in force now.
		fv.Type = fd.Type
		ids, err := caseRefIDs(fv)
		if err != nil {
			// A malformed reference value is already reported as a field-value
			// violation; there is no id to look up here.
			continue
		}

		for _, id := range ids {
			want, ok := refWanted[fd.ReferenceWorkspace]
			if !ok {
				want = &caseRefWant{byField: make(map[string]map[int64][]ValidationTarget)}
				refWanted[fd.ReferenceWorkspace] = want
			}
			if want.byField[fieldID] == nil {
				want.byField[fieldID] = make(map[int64][]ValidationTarget)
			}
			want.byField[fieldID][id] = append(want.byField[fieldID][id], target)
		}
	}
}

// checkCaseStatuses validates the thread-mode board status and the lifecycle
// status derived from it.
//
// The gate is the workspace's configured CaseMode, not merely the presence of a
// case status set: config.resolveCaseStatusSet builds the set from the [case]
// section alone without consulting CaseMode, so a workspace switched to channel
// mode that still carries [case] in its TOML would otherwise have its old
// thread-bound Cases checked against a status set the workspace no longer uses.
//
// IsThreadBound stays in the condition deliberately. CaseUseCase.applyThreadBinding
// is the only place that populates BoardStatus and it runs on the thread path
// only, so a channel-bound Case in a thread-mode workspace carries an empty
// BoardStatus that no code path ever fills — reporting it would be a false
// positive rather than a finding.
func checkCaseStatuses(acc *issueAccumulator, entry *model.WorkspaceEntry, c *model.Case, target ValidationTarget) {
	set := entry.CaseStatusSet
	if !entry.IsThreadMode() || set == nil || !c.IsThreadBound() || c.IsDraft() {
		return
	}

	if c.BoardStatus == "" {
		acc.add(IssueKindBoardStatus, "", target,
			"one of the configured case status ids", "",
			"thread-mode case has no board status")
		return
	}
	if !set.IsValid(c.BoardStatus) {
		acc.add(IssueKindBoardStatus, "", target,
			"one of the configured case status ids", c.BoardStatus,
			"board status is not defined in the workspace configuration")
		return
	}

	// Mirror model.Case.SyncLifecycleFromBoardStatus: a closed board status maps
	// to CLOSED, anything else to OPEN. An empty stored Status predates DRAFT and
	// means OPEN, so normalize before comparing.
	expected := types.CaseStatusOpen
	if set.IsClosed(c.BoardStatus) {
		expected = types.CaseStatusClosed
	}
	if actual := c.Status.Normalize(); actual != expected {
		acc.add(IssueKindLifecycleMismatch, "", target,
			string(expected), string(actual),
			"lifecycle status disagrees with whether the board status is configured as closed")
	}
}

// checkCaseArchiveState reports an archived Case whose lifecycle Status is not
// CLOSED. The invariant is enforced on every write path (ArchiveCase refuses a
// non-closed case; ReopenCase and UpdateCaseStatus refuse an archived one), so
// a finding here means the document was written by something else — a data
// import, a manual edit, or a lost race between archive and reopen. It matters
// because such a Case is filtered out of every listing at once and cannot be
// found without this check.
//
// Unlike checkCaseStatuses this is not gated on CaseMode: the invariant holds
// for channel-mode and thread-mode Cases alike.
func checkCaseArchiveState(acc *issueAccumulator, c *model.Case, target ValidationTarget) {
	if !c.IsArchived() {
		return
	}
	if actual := c.Status.Normalize(); actual != types.CaseStatusClosed {
		acc.add(IssueKindArchivedNotClosed, "", target,
			string(types.CaseStatusClosed), string(actual),
			"archived case is not closed; unarchive it or close it")
	}
}

// validateCaseRefs confirms each collected reference points at a Case that
// exists in the field's configured reference workspace. Existence is the only
// check: whether a Case may be referenced (private / draft) is a write-time
// policy, and applying it here would flag references that were legitimate when
// they were written.
func (uc *UseCases) validateCaseRefs(ctx context.Context, wsID string, refWanted map[string]*caseRefWant, acc *issueAccumulator) error {
	for _, refWS := range slices.Sorted(maps.Keys(refWanted)) {
		want := refWanted[refWS]

		ids := make(map[int64]struct{})
		for _, byID := range want.byField {
			for id := range byID {
				ids[id] = struct{}{}
			}
		}
		if len(ids) == 0 {
			continue
		}

		missing, err := uc.findMissingCases(ctx, refWS, slices.Sorted(maps.Keys(ids)))
		if err != nil {
			return goerr.Wrap(err, "failed to load referenced cases for consistency check",
				goerr.V("workspace_id", wsID),
				goerr.V("reference_workspace", refWS))
		}
		if len(missing) == 0 {
			continue
		}

		for _, fieldID := range slices.Sorted(maps.Keys(want.byField)) {
			// One occurrence per referring entity, not per missing id: Count is
			// defined as the number of entities affected, and a single
			// multi_case_ref value can carry several missing ids (or the same id
			// twice).
			missingByTarget := make(map[ValidationTarget]map[int64]struct{})
			for id, targets := range want.byField[fieldID] {
				if _, ok := missing[id]; !ok {
					continue
				}
				for _, target := range targets {
					if missingByTarget[target] == nil {
						missingByTarget[target] = make(map[int64]struct{})
					}
					missingByTarget[target][id] = struct{}{}
				}
			}

			for _, target := range slices.SortedFunc(maps.Keys(missingByTarget), ValidationTarget.compare) {
				acc.add(IssueKindCaseRefMissing, fieldID, target,
					fmt.Sprintf("existing cases in workspace %q", refWS),
					joinCaseIDs(missingByTarget[target]),
					"referenced case does not exist in the configured reference workspace")
			}
		}
	}

	return nil
}

// caseRefLookupChunk bounds how many case ids go into one GetByIDs call.
// GetByIDs issues a single Firestore GetAll for whatever it is handed, and the
// id set here grows with the workspace rather than with a page of UI rows. 500
// mirrors the batch size the Firestore repositories already use for bulk work;
// it is a self-imposed bound, not a documented read limit.
const caseRefLookupChunk = 500

// findMissingCases returns the subset of ids that no Case in workspaceID has.
func (uc *UseCases) findMissingCases(ctx context.Context, workspaceID string, ids []int64) (map[int64]struct{}, error) {
	missing := make(map[int64]struct{})

	for start := 0; start < len(ids); start += caseRefLookupChunk {
		chunk := ids[start:min(start+caseRefLookupChunk, len(ids))]
		found, err := uc.repo.Case().GetByIDs(ctx, workspaceID, chunk)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to batch get referenced cases",
				goerr.V("chunk_size", len(chunk)))
		}
		for _, id := range chunk {
			if _, ok := found[id]; !ok {
				missing[id] = struct{}{}
			}
		}
	}

	return missing, nil
}

// joinCaseIDs renders a set of case ids as a comma-separated ascending list.
func joinCaseIDs(ids map[int64]struct{}) string {
	sorted := slices.Sorted(maps.Keys(ids))
	out := make([]string, len(sorted))
	for i, id := range sorted {
		out[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(out, ",")
}

// validateActionStatuses checks every Action, archived ones included: an
// archived Action still shows up in the Case history, so a status id the
// configuration dropped is just as visible there. In a thread-mode workspace
// that does not enable Actions ([case] actions), every Action is reported as
// well, since nothing in the UI shows it.
func (uc *UseCases) validateActionStatuses(ctx context.Context, entry *model.WorkspaceEntry, acc *issueAccumulator) error {
	set := entry.ActionStatusSet
	actionsEnabled := entry.ActionsEnabled()
	if set == nil && actionsEnabled {
		return nil
	}
	wsID := entry.Workspace.ID

	actions, err := uc.repo.Action().List(ctx, wsID, interfaces.ActionListOptions{
		ArchiveScope: interfaces.ActionArchiveScopeAll,
	})
	if err != nil {
		return goerr.Wrap(err, "failed to list actions for consistency check",
			goerr.V("workspace_id", wsID))
	}

	for _, a := range actions {
		if !actionsEnabled {
			acc.add(IssueKindActionsDisabled, "",
				ValidationTarget{Kind: TargetKindAction, CaseID: a.CaseID, ActionID: a.ID},
				"no actions ([case] actions is off)", "action exists",
				"action is stored in a thread-mode workspace that does not enable actions; set [case] actions = true to show it again")
		}
		if set == nil || set.IsValid(string(a.Status)) {
			continue
		}
		acc.add(IssueKindActionStatus, "",
			ValidationTarget{Kind: TargetKindAction, CaseID: a.CaseID, ActionID: a.ID},
			"one of the configured action status ids", string(a.Status),
			"action status is not defined in the workspace configuration")
	}

	return nil
}

// validateMemoFields checks memo field values against the memo schema. Memos
// live in a per-Case subcollection, so they are fetched Case by Case rather than
// through a workspace-wide query (which would need a new Firestore index).
func (uc *UseCases) validateMemoFields(ctx context.Context, entry *model.WorkspaceEntry, caseIDs []int64, acc *issueAccumulator) error {
	if !entry.MemoConfig.Enabled() {
		return nil
	}
	wsID := entry.Workspace.ID

	validator := model.NewFieldValidator(entry.MemoConfig.FieldSchema)
	defByID := make(map[string]config.FieldDefinition, len(entry.MemoConfig.FieldSchema.Fields))
	for _, fd := range entry.MemoConfig.FieldSchema.Fields {
		defByID[fd.ID] = fd
	}

	for _, caseID := range caseIDs {
		memos, err := uc.repo.Memo().List(ctx, wsID, caseID, interfaces.MemoListOptions{
			ArchiveScope: interfaces.MemoArchiveScopeAll,
		})
		if err != nil {
			return goerr.Wrap(err, "failed to list memos for consistency check",
				goerr.V("workspace_id", wsID),
				goerr.V("case_id", caseID))
		}

		for _, m := range memos {
			target := ValidationTarget{Kind: TargetKindMemo, CaseID: m.CaseID, MemoID: m.ID}
			addFieldViolations(acc, target, defByID, m.FieldValues, validator.ValidateStored(m.FieldValues))
		}
	}

	return nil
}
