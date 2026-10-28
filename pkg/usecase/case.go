package usecase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	goslack "github.com/slack-go/slack"
)

// CaseEventPublisher is the narrow surface of pkg/usecase/job.UseCase that
// CaseUseCase calls into after a lifecycle transition. The interface is
// defined here so this package does not import pkg/usecase/job (which
// would create a cycle: job → usecase → job).
type CaseEventPublisher interface {
	PublishCaseLifecycle(ctx context.Context, workspaceID string, c *model.Case, lifecycle model.CaseLifecycle, actorUserID string)
}

type CaseUseCase struct {
	repo              interfaces.Repository
	workspaceRegistry *model.WorkspaceRegistry
	slackService      slack.Service
	slackAdminService slack.AdminService
	baseURL           string
	welcomeRenderers  map[string]*welcomeRenderer
	eventPublisher    CaseEventPublisher
	workspaceAccess   interfaces.WorkspaceAuthorizer
}

func NewCaseUseCase(repo interfaces.Repository, registry *model.WorkspaceRegistry, slackService slack.Service, slackAdminService slack.AdminService, baseURL string) *CaseUseCase {
	uc := &CaseUseCase{
		repo:              repo,
		workspaceRegistry: registry,
		slackService:      slackService,
		slackAdminService: slackAdminService,
		baseURL:           baseURL,
		welcomeRenderers:  make(map[string]*welcomeRenderer),
		workspaceAccess:   allowAllWorkspaces(),
	}

	// Pre-parse welcome message templates per workspace. Configuration loading
	// already validated each template, so a parse failure here is unexpected
	// but treated as non-fatal: the workspace simply gets no welcome messages.
	if registry != nil {
		for _, entry := range registry.List() {
			renderer, err := newWelcomeRenderer(entry.SlackWelcomeMessages)
			if err != nil {
				errutil.Handle(context.Background(), goerr.Wrap(err, "failed to build welcome renderer; skipping welcome messages",
					goerr.V("workspaceID", entry.Workspace.ID),
				), "failed to build welcome renderer")
				continue
			}
			uc.welcomeRenderers[entry.Workspace.ID] = renderer
		}
	}

	return uc
}

// SetEventPublisher wires the lifecycle event publisher. Called once at
// startup after the job dispatch UseCase has been constructed. nil is
// allowed (Job dispatch effectively disabled).
func (uc *CaseUseCase) SetEventPublisher(p CaseEventPublisher) {
	uc.eventPublisher = p
}

// publishLifecycle is a no-op when the publisher is unset. Suppression of
// self-firing (a Job actor mutation re-firing its own event) lives inside
// the publisher implementation; this method only forwards.
func (uc *CaseUseCase) publishLifecycle(ctx context.Context, workspaceID string, c *model.Case, lifecycle model.CaseLifecycle) {
	if uc == nil || uc.eventPublisher == nil || c == nil {
		return
	}
	actor := ""
	if tok, err := auth.TokenFromContext(ctx); err == nil {
		actor = tok.Sub
	}
	uc.eventPublisher.PublishCaseLifecycle(ctx, workspaceID, c, lifecycle, actor)
}

func (uc *CaseUseCase) fieldValidatorForWorkspace(workspaceID string) *model.FieldValidator {
	if uc.workspaceRegistry == nil {
		return nil
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil || entry.FieldSchema == nil {
		return nil
	}
	return model.NewFieldValidator(entry.FieldSchema)
}

// fieldSchemaForWorkspace returns the configured FieldSchema for the
// workspace, or nil when none is registered. Used when callers need the
// raw definitions (e.g. enumerating required fields) rather than a
// validator wrapper.
func (uc *CaseUseCase) fieldSchemaForWorkspace(workspaceID string) *config.FieldSchema {
	if uc.workspaceRegistry == nil {
		return nil
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return nil
	}
	return entry.FieldSchema
}

// fieldValidationMode selects how strictly validateCaseWrite treats the
// supplied custom-field values. The three modes mirror the three legal
// shapes a case write can take.
type fieldValidationMode int

const (
	// validatePartial type-checks the submitted fields, PRESERVES unknown
	// ids (forward-compat for fields removed from the schema), and does not
	// require missing required fields. Used by the draft paths.
	validatePartial fieldValidationMode = iota
	// validatePartialStrict type-checks the submitted fields, REJECTS unknown
	// ids, and does not require missing required fields. Used by the partial
	// edit paths whose input is untrusted (UpdateCase / MaterializeThreadCase).
	validatePartialStrict
	// validateAll type-checks every field, REJECTS unknown ids, and requires
	// every required field. Used by the open-create paths.
	validateAll
)

// validateCaseWrite is the single validation gate every case write funnels
// through so the agent (UpdateCase / MaterializeThreadCase), GraphQL, and
// Slack paths enforce identical rules. It (1) runs the workspace field
// validator in the requested mode, enriching each value with its config Type,
// and (2) verifies that every referenced user id — assignees plus the values
// of user / multi-user fields — exists in the SlackUser store. A missing user
// is rejected with ErrUnknownUser (Slack sync delay is treated as
// non-existence per project policy). A nil workspace validator skips the field
// checks (no schema configured) but the user-existence check still runs.
// Returns the enriched field values (nil-safe: a nil fieldValues yields nil).
func (uc *CaseUseCase) validateCaseWrite(
	ctx context.Context,
	workspaceID string,
	mode fieldValidationMode,
	fieldValues map[string]model.FieldValue,
	assigneeIDs []string,
) (map[string]model.FieldValue, error) {
	enriched := fieldValues
	// Skip the field validator only for partial modes with no submitted fields
	// (an assignee-only / status-adjacent update must not touch untouched
	// fields). validateAll always runs so missing required fields are caught
	// even when the caller supplied none.
	if fieldValues != nil || mode == validateAll {
		if validator := uc.fieldValidatorForWorkspace(workspaceID); validator != nil {
			var err error
			switch mode {
			case validateAll:
				enriched, err = validator.ValidateCaseFieldsAll(fieldValues)
			case validatePartialStrict:
				enriched, err = validator.ValidateCaseFieldsPartialStrict(fieldValues)
			default:
				enriched, err = validator.ValidateCaseFieldsPartial(fieldValues)
			}
			if err != nil {
				return nil, goerr.Wrap(err, "case field validation failed", goerr.V("workspace_id", workspaceID))
			}
		}
	}

	if err := uc.verifyUsersExist(ctx, assigneeIDs, enriched); err != nil {
		return nil, err
	}
	if err := uc.verifyCaseRefsExist(ctx, workspaceID, enriched); err != nil {
		return nil, err
	}
	return enriched, nil
}

// verifyCaseRefsExist confirms that every case_ref / multi-case-
// reference value points at a Case that exists in the field's configured
// reference_workspace and is neither private nor a draft. Private and draft
// Cases are never referenceable (project requirement), independent of the
// requesting user — so this check runs even in bot/system contexts that carry
// no auth token. Lookups are batched per reference_workspace (N+1-safe).
func (uc *CaseUseCase) verifyCaseRefsExist(ctx context.Context, workspaceID string, fieldValues map[string]model.FieldValue) error {
	if len(fieldValues) == 0 {
		return nil
	}
	schema := uc.fieldSchemaForWorkspace(workspaceID)
	if schema == nil {
		return nil
	}
	defByID := make(map[string]config.FieldDefinition, len(schema.Fields))
	for _, fd := range schema.Fields {
		defByID[fd.ID] = fd
	}

	// reference_workspace -> set of Case IDs to fetch.
	idsByWS := make(map[string]map[int64]struct{})
	// per (fieldID) -> its referenced (workspace, ids) for violation attribution.
	type fieldRefs struct {
		refWS string
		ids   []int64
	}
	var perField []struct {
		fieldID string
		refs    fieldRefs
	}

	for _, fv := range fieldValues {
		if !fv.Type.IsCaseRef() {
			continue
		}
		fd, ok := defByID[string(fv.FieldID)]
		if !ok || fd.ReferenceWorkspace == "" {
			// Unknown field id is reported by the field validator; a missing
			// reference_workspace is rejected at config load. Skip defensively.
			continue
		}
		ids, err := caseRefIDs(fv)
		if err != nil {
			return goerr.Wrap(err, "invalid case reference value",
				goerr.V("field_id", fv.FieldID))
		}
		if len(ids) == 0 {
			continue
		}
		if idsByWS[fd.ReferenceWorkspace] == nil {
			idsByWS[fd.ReferenceWorkspace] = make(map[int64]struct{})
		}
		for _, id := range ids {
			idsByWS[fd.ReferenceWorkspace][id] = struct{}{}
		}
		perField = append(perField, struct {
			fieldID string
			refs    fieldRefs
		}{fieldID: string(fv.FieldID), refs: fieldRefs{refWS: fd.ReferenceWorkspace, ids: ids}})
	}

	if len(idsByWS) == 0 {
		return nil
	}

	// Batch-fetch each reference workspace exactly once.
	fetched := make(map[string]map[int64]*model.Case, len(idsByWS))
	for refWS, idSet := range idsByWS {
		// A reference into a workspace the caller may not access is refused
		// rather than reported per id: its existence checks would otherwise
		// tell the caller which of that workspace's case ids exist.
		if err := uc.workspaceAccess.AuthorizeCurrentUser(ctx, refWS); err != nil {
			return goerr.Wrap(err, "case reference points into an inaccessible workspace",
				goerr.V("reference_workspace", refWS))
		}
		ids := make([]int64, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		found, err := uc.repo.Case().GetByIDs(ctx, refWS, ids)
		if err != nil {
			return goerr.Wrap(err, "failed to look up referenced cases",
				goerr.V("reference_workspace", refWS))
		}
		fetched[refWS] = found
	}

	var violations []string
	for _, pf := range perField {
		found := fetched[pf.refs.refWS]
		for _, id := range pf.refs.ids {
			c, ok := found[id]
			switch {
			case !ok:
				violations = append(violations, fmt.Sprintf(
					"field %q: referenced case #%d not found in workspace %q", pf.fieldID, id, pf.refs.refWS))
			case c.IsPrivate:
				violations = append(violations, fmt.Sprintf(
					"field %q: referenced case #%d is private and cannot be referenced", pf.fieldID, id))
			case c.Status == types.CaseStatusDraft:
				violations = append(violations, fmt.Sprintf(
					"field %q: referenced case #%d is a draft and cannot be referenced", pf.fieldID, id))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return goerr.Wrap(model.ErrCaseFieldValidation,
			"case reference validation failed:\n- "+strings.Join(violations, "\n- "),
			goerr.V("violations", violations))
	}
	return nil
}

// caseRefIDs extracts the numeric Case IDs from a case_ref value.
// Single values arrive as a string, multi values as []string / []any (depending
// on whether they went through typed coercion or a raw decode).
func caseRefIDs(fv model.FieldValue) ([]int64, error) {
	// A cleared optional case_ref arrives with a nil value; treat it as no
	// references rather than a type error.
	if fv.Value == nil {
		return nil, nil
	}
	parse := func(s string) (int64, bool, error) {
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false, goerr.Wrap(err, "case reference must be a numeric case ID",
				goerr.V("value", s))
		}
		return n, true, nil
	}

	switch fv.Type {
	case types.FieldTypeCaseRef:
		s, ok := fv.Value.(string)
		if !ok {
			return nil, goerr.New("case_ref value must be a string")
		}
		n, present, err := parse(s)
		if err != nil || !present {
			return nil, err
		}
		return []int64{n}, nil
	case types.FieldTypeMultiCaseRef:
		var out []int64
		for _, s := range coerceUserIDSlice(fv.Value) {
			n, present, err := parse(s)
			if err != nil {
				return nil, err
			}
			if present {
				out = append(out, n)
			}
		}
		return out, nil
	default:
		return nil, nil
	}
}

// verifyUsersExist collects every user id referenced by the write — the
// assignees plus the values of user / multi-user custom fields — and confirms
// each exists in the SlackUser store with a single batch lookup (N+1-safe).
// Reporter is intentionally NOT checked: it is set from the auth context /
// inbound Slack event, where the user provably exists even if the periodic
// SlackUser sync has not caught up yet. Unknown ids are reported together via
// ErrUnknownUser.
func (uc *CaseUseCase) verifyUsersExist(ctx context.Context, assigneeIDs []string, fieldValues map[string]model.FieldValue) error {
	idSet := make(map[string]struct{}, len(assigneeIDs))
	for _, id := range assigneeIDs {
		if id != "" {
			idSet[id] = struct{}{}
		}
	}
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
		return goerr.Wrap(err, "failed to look up users for case write")
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

// coerceUserIDSlice extracts string ids from a multi-user field value, which
// may arrive as []string (typed coercion) or []any (raw decode).
func coerceUserIDSlice(v any) []string {
	switch a := v.(type) {
	case []string:
		return a
	case []any:
		out := make([]string, 0, len(a))
		for _, item := range a {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// mergeFieldValues overlays the validated patch onto a copy of the existing
// field values. It is the shared merge behind UpdateCase / MaterializeThreadCase
// / SubmitDraft so the "preserve untouched fields, replace submitted ones"
// contract lives in one place. existing is never mutated.
func mergeFieldValues(existing, patch map[string]model.FieldValue) map[string]model.FieldValue {
	merged := make(map[string]model.FieldValue, len(existing)+len(patch))
	maps.Copy(merged, existing)
	maps.Copy(merged, patch)
	return merged
}

func (uc *CaseUseCase) slackTeamIDForWorkspace(workspaceID string) string {
	if uc.workspaceRegistry == nil {
		return ""
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return ""
	}
	return entry.SlackTeamID
}

func (uc *CaseUseCase) slackChannelPrefixForWorkspace(workspaceID string) string {
	if uc.workspaceRegistry == nil {
		return workspaceID
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return workspaceID
	}
	if entry.SlackChannelPrefix == "" {
		return workspaceID
	}
	return entry.SlackChannelPrefix
}

// resolveCaseMode resolves the workspace entry so the OPEN-case funnel can
// decide the Slack shape. A nil registry means the mode is undeterminable; this
// only occurs in tests / degraded setups (every production CLI entry point
// wires a registry via config.Configure), so it is treated as channel mode to
// preserve historical behaviour — a nil entry is returned with no error. A
// lookup failure for a wired registry still fails closed (propagated) so a
// mistyped workspace id cannot silently fall through to channel creation.
func (uc *CaseUseCase) resolveCaseMode(workspaceID string) (*model.WorkspaceEntry, error) {
	if uc.workspaceRegistry == nil {
		return nil, nil
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to resolve workspace mode",
			goerr.V("workspace_id", workspaceID))
	}
	return entry, nil
}

// assertThreadCaseVisibility rejects a private case in a thread-mode workspace.
// A private case's only effect is a dedicated private channel, which thread mode
// has no equivalent for. This is the single definition of the rule; both the
// OPEN-case funnel and persistCase (draft creation) call it. workspaceID is
// carried into the error so the (user-facing, BAD_USER_INPUT) rejection keeps
// the debugging context both call sites used to attach.
func assertThreadCaseVisibility(workspaceID string, isPrivate bool) error {
	if isPrivate {
		return goerr.Wrap(ErrCasePrivateThreadModeUnsupported,
			"private case requested in thread-mode workspace",
			goerr.V("workspace_id", workspaceID))
	}
	return nil
}

// applyThreadBinding sets the thread-mode Slack binding fields on a Case: the
// monitored channel, the root thread ts, the initial board status (only when
// unset or invalid), and the update timestamp. It is the single place the
// thread binding is applied, used by both the create path (before repo.Create)
// and the draft-promotion path (before repo.Update).
func (uc *CaseUseCase) applyThreadBinding(c *model.Case, workspaceID, channelID, rootTS string, now time.Time) {
	c.SlackChannelID = channelID
	c.SlackThreadTS = rootTS
	if set := uc.caseStatusSetForWorkspace(workspaceID); set != nil {
		if c.BoardStatus == "" || !set.IsValid(c.BoardStatus) {
			c.BoardStatus = set.InitialID()
		}
	}
	c.UpdatedAt = now
}

// openProducers supplies the two ways a case is brought to OPEN: Channel creates
// the dedicated-channel form, Thread binds it to the monitored-channel thread.
// openInWorkspaceMode picks exactly one based on the resolved workspace mode.
type openProducers struct {
	Channel func(ctx context.Context, entry *model.WorkspaceEntry) (*model.Case, error)
	Thread  func(ctx context.Context, entry *model.WorkspaceEntry) (*model.Case, error)
}

// openInWorkspaceMode is the shared funnel the OPEN-case entry points
// (CreateCase, SubmitDraft) pass through to honour the workspace-mode invariant:
// thread mode binds to the monitored channel and never provisions a dedicated
// channel; channel mode creates one. The mode decision, the private guard, and
// the thread-mode Slack-readiness check are defined here once, so every path
// routed through this funnel gets them consistently. (It does not by itself
// prevent a future caller from persisting + activating a case directly; it is
// the funnel all current OPEN-case paths use, not a hard gate.)
func (uc *CaseUseCase) openInWorkspaceMode(ctx context.Context, workspaceID string, isPrivate bool, p openProducers) (*model.Case, error) {
	entry, err := uc.resolveCaseMode(workspaceID)
	if err != nil {
		return nil, err
	}
	// A nil entry (registry not wired) is treated as channel mode — see
	// resolveCaseMode. The channel producer tolerates a nil entry.
	if entry == nil || !entry.IsThreadMode() {
		return p.Channel(ctx, entry)
	}
	if err := assertThreadCaseVisibility(workspaceID, isPrivate); err != nil {
		return nil, err
	}
	// No Slack service wired (e2e harness / local dev without a bot token): fall
	// through to the channel producer. activateChannelModeCase is a no-op without
	// Slack, so the thread-mode case is persisted with NO dedicated channel — the
	// "thread mode never provisions a channel" invariant still holds, and these
	// Slack-less environments keep working exactly as before.
	if uc.slackService == nil {
		return p.Channel(ctx, entry)
	}
	// Slack IS wired but the monitored channel is unset: a thread-mode case has
	// nowhere to anchor. Fail closed rather than fall through to the channel
	// producer, which — with Slack present — would create a dedicated channel and
	// violate the invariant. Startup config validation normally rejects this, so
	// it only guards direct WorkspaceEntry construction (tests / eval).
	if entry.SlackMonitorChannelID == "" {
		return nil, goerr.Wrap(ErrThreadModeSlackUnconfigured,
			"thread-mode workspace requires a monitor channel",
			goerr.V("workspace_id", workspaceID))
	}
	return p.Thread(ctx, entry)
}

// postMonitoredThreadAnchor posts a lightweight placeholder root into the
// workspace's monitored channel and returns its ts, which anchors the case
// thread until it is replaced with the case summary. Single Slack "start a
// monitored-channel thread" step shared by every thread-mode create / promotion.
//
// The Slack service is passed explicitly (not taken from a CaseUseCase field)
// so each caller supplies the same service it uses for the rest of that flow:
// the case-creation paths pass uc.slackService, the reaction path passes its
// own deps.SlackService. This keeps the placeholder, the summary replacement,
// and any failure note on one Slack backend and removes the hidden cross-object
// dependency (and nil-panic) a bound method would create.
func postMonitoredThreadAnchor(ctx context.Context, svc slack.Service, entry *model.WorkspaceEntry) (string, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, entry.Language)
	dest := entry.SlackMonitorChannelID
	rootTS, err := svc.PostMessage(ctx, dest, nil, i18n.T(ctx, i18n.MsgReactionCasePlaceholder))
	if err != nil {
		return "", goerr.Wrap(err, "failed to post thread-mode case root", goerr.V("dest_channel", dest))
	}
	if rootTS == "" {
		return "", goerr.New("slack returned empty thread ts for case root", goerr.V("dest_channel", dest))
	}
	return rootTS, nil
}

// failMonitoredThreadAnchor rewrites the placeholder root into an honest failure
// note when case persistence fails after the anchor was posted, so the monitored
// channel does not imply work is still ongoing. If the update itself fails it
// falls back to a threaded reply so the note is never lost. Both failures are
// non-fatal. The Slack service is passed explicitly for the same reason as
// postMonitoredThreadAnchor.
func failMonitoredThreadAnchor(ctx context.Context, svc slack.Service, entry *model.WorkspaceEntry, rootTS string) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, entry.Language)
	dest := entry.SlackMonitorChannelID
	failNote := "⚠️ " + i18n.T(ctx, i18n.MsgThreadCaseCreateFallback)
	if uErr := svc.UpdateMessage(ctx, dest, rootTS, nil, failNote); uErr != nil {
		errutil.Handle(ctx, goerr.Wrap(uErr, "update thread-mode case root to fallback",
			goerr.V("dest_channel", dest), goerr.V("root_ts", rootTS)), "update thread-mode case root to fallback")
		if _, rErr := svc.PostThreadReply(ctx, dest, rootTS, failNote); rErr != nil {
			errutil.Handle(ctx, goerr.Wrap(rErr, "post thread-mode case fallback reply",
				goerr.V("dest_channel", dest), goerr.V("root_ts", rootTS)), "post thread-mode case fallback reply")
		}
	}
}

// CreateCase persists a brand-new case in status=OPEN and runs the mode-
// appropriate activation (dedicated Slack channel in channel mode, monitored-
// channel thread in thread mode). It is the public entry point used by the
// createCase mutation and the slash-command "submit" flow. The mode decision is
// delegated to the shared funnel openInWorkspaceMode so every OPEN-case path
// honours the same invariant.
//
// WithExistingSlackChannel links the case to an existing channel instead of
// creating one; see createCaseInExistingSlackChannel.
func (uc *CaseUseCase) CreateCase(ctx context.Context, workspaceID string, title, description string, assigneeIDs []string, fieldValues map[string]model.FieldValue, isPrivate bool, isTest bool, sourceTeamID string, requestKey string, opts ...CreateCaseOption) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	var o createCaseOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.existingSlackChannelSet {
		return uc.createCaseInExistingSlackChannel(ctx, workspaceID, title, description, assigneeIDs, fieldValues, isPrivate, isTest, sourceTeamID, requestKey, o.existingSlackChannelID)
	}
	return uc.openInWorkspaceMode(ctx, workspaceID, isPrivate, openProducers{
		Channel: func(ctx context.Context, entry *model.WorkspaceEntry) (*model.Case, error) {
			return uc.createChannelModeCase(ctx, workspaceID, entry, title, description, assigneeIDs, fieldValues, isPrivate, isTest, sourceTeamID, requestKey, "")
		},
		// Thread-mode cases carry only title / description / fields (+ requestKey
		// for dedup); the caller's assigneeIDs / isTest / sourceTeamID are
		// intentionally ignored, exactly as reaction-created cases are.
		Thread: func(ctx context.Context, entry *model.WorkspaceEntry) (*model.Case, error) {
			return uc.createThreadModeCase(ctx, workspaceID, entry, title, description, fieldValues, requestKey)
		},
	})
}

// createChannelModeCase persists a brand-new OPEN case and runs channel-mode
// activation (dedicated Slack channel, invites, welcome, etc.). Extracted from
// CreateCase so the channel form is one named producer behind the shared funnel.
// existingSlackChannelID is passed through to activateChannelModeCase; empty
// creates a new channel.
func (uc *CaseUseCase) createChannelModeCase(ctx context.Context, workspaceID string, entry *model.WorkspaceEntry, title, description string, assigneeIDs []string, fieldValues map[string]model.FieldValue, isPrivate bool, isTest bool, sourceTeamID string, requestKey string, existingSlackChannelID string) (*model.Case, error) {
	created, err := uc.persistCase(ctx, workspaceID, persistCaseInput{
		Title:       title,
		Description: description,
		Status:      types.CaseStatusOpen,
		AssigneeIDs: assigneeIDs,
		IsPrivate:   isPrivate,
		IsTest:      isTest,
		FieldValues: fieldValues,
		RequestKey:  requestKey,
	})
	if err != nil {
		return nil, err
	}

	// `persistCase` returns early when an existing requestKey matched; that case
	// is already active and must not be re-activated.
	if created.Status != types.CaseStatusOpen || created.SlackChannelID != "" {
		return created, nil
	}

	activated, actErr := uc.activateChannelModeCase(ctx, workspaceID, entry, created, sourceTeamID, existingSlackChannelID)
	if actErr != nil {
		// Rollback policy: discard the half-formed case so the whole call appears
		// atomic. SubmitDraft uses a different policy (status flip back to DRAFT).
		if delErr := uc.repo.Case().Delete(ctx, workspaceID, created.ID); delErr != nil {
			return nil, goerr.Wrap(actErr, "case activation failed and rollback delete also failed",
				goerr.V("rollback_error", delErr),
				goerr.V(CaseIDKey, created.ID))
		}
		return nil, actErr
	}

	// Fire the case lifecycle event AFTER activation succeeded. Failure here must
	// not roll back the case — the Job dispatch is fire-and-forget by design.
	uc.publishLifecycle(ctx, workspaceID, activated, model.CaseLifecycleCreated)
	return activated, nil
}

// createThreadModeCase handles case creation for a thread-mode workspace. Unlike
// the channel-mode path (CreateCase → activateChannelModeCase), it does NOT create a
// dedicated Slack channel. It mirrors the reaction cross-channel path: it posts
// a fresh root message into the workspace's monitored channel, binds a new
// thread-mode Case to that message via createThreadBoundCase, then replaces
// the placeholder root with the shared case summary.
//
// Ordering is forced by the identity model: createThreadBoundCase requires
// the thread ts as a mandatory identity field, and a web-originated case has no
// pre-existing message — so the root must be posted first to mint the ts. The
// summary's web link needs the DB-assigned case id, which only exists after
// Create — so the root is posted as a placeholder and replaced afterwards.
//
// requestKey carries the caller's idempotency key (empty for the web path). It
// is honoured here — not only in the channel-mode persistCase — because the
// thread path posts a NEW root every call, so its (channel, threadTS) binding
// can never dedup a redelivery; requestKey is the only stable key across a
// Slack redelivery / double-submit of the slash-command or mention-proposal
// entry points.
func (uc *CaseUseCase) createThreadModeCase(ctx context.Context, workspaceID string, entry *model.WorkspaceEntry, title, description string, fieldValues map[string]model.FieldValue, requestKey string) (*model.Case, error) {
	// The caller (CreateCase) only routes here when the Slack service and the
	// monitored channel are both present, so both are safe to use.
	dest := entry.SlackMonitorChannelID

	// Title is required for an OPEN case, matching the channel-mode contract in
	// persistCase. Enforced here (before any Slack side effect) so the same
	// public CreateCase entry point does not silently accept an empty-title case
	// just because the workspace is thread mode.
	if title == "" {
		return nil, goerr.New("case title is required")
	}

	// Idempotency: a re-delivered submission with the same requestKey must return
	// the already-created case instead of posting a second root. The lookup is
	// best-effort (mirrors persistCase): a lookup error is reported but does not
	// block creation.
	if requestKey != "" {
		if existing, rkErr := uc.repo.Case().GetByRequestKey(ctx, workspaceID, requestKey); rkErr != nil {
			errutil.Handle(ctx, rkErr, "thread-mode case: check request key")
		} else if existing != nil {
			return existing, nil
		}
	}

	// Validate fields BEFORE posting the root. The monitored channel is a shared,
	// human-watched channel; posting an anchor for a case that then fails
	// validation and rewriting it to an error note would be noise on every
	// mistyped submission. Unlike the reaction path (whose only feedback channel
	// is Slack), the web / slash / mention callers return the error synchronously,
	// so we gate first and only touch Slack once the input is known good.
	// createThreadBoundCase re-validates authoritatively; this is a gate.
	if _, err := uc.validateCaseWrite(ctx, workspaceID, validateAll, fieldValues, nil); err != nil {
		return nil, goerr.Wrap(err, "thread-mode case field validation failed",
			goerr.V("workspace_id", workspaceID))
	}

	reporterID := ""
	if tok, err := auth.TokenFromContext(ctx); err == nil {
		reporterID = tok.Sub
	}

	// A lightweight placeholder root anchors the case thread; it is replaced in
	// place with the case summary once the case is committed.
	rootTS, err := postMonitoredThreadAnchor(ctx, uc.slackService, entry)
	if err != nil {
		return nil, err
	}

	created, err := uc.createThreadBoundCase(ctx, workspaceID, dest, rootTS, reporterID, title, description, fieldValues, requestKey)
	if err != nil {
		// The field gate above already ran, so reaching here means an
		// infrastructure failure (repo write). Replace the lingering placeholder
		// with an honest failure note so the monitored channel does not imply work
		// is still ongoing, then surface the error.
		failMonitoredThreadAnchor(ctx, uc.slackService, entry, rootTS)
		return nil, goerr.Wrap(err, "failed to create thread-mode case",
			goerr.V("dest_channel", dest), goerr.V("root_ts", rootTS))
	}

	replaceRootWithCaseSummary(ctx, uc.slackService, entry, created, dest, rootTS, uc.CaseURL(workspaceID, created.ID))
	return created, nil
}

// CreateDraft persists a case in status=DRAFT — i.e. an "in-progress" entry
// saved from the Slack creation modal's Save as Draft button. None of the
// activation side effects (Slack channel, invites, welcome, etc.) run; those
// fire only when the draft is later promoted via SubmitDraft.
//
// The reporter (auth-context Slack user) becomes the draft owner; the
// returned case carries the assigned ID so the caller can echo it back to
// the user.
func (uc *CaseUseCase) CreateDraft(ctx context.Context, workspaceID string, title, description string, assigneeIDs []string, fieldValues map[string]model.FieldValue, isPrivate bool, isTest bool) (*model.Case, error) {
	// Title is intentionally optional for drafts: half-written entries are
	// the whole point. We still validate field values to keep the draft
	// usable on Submit without surprise validation failures.
	return uc.persistCase(ctx, workspaceID, persistCaseInput{
		Title:       title,
		Description: description,
		Status:      types.CaseStatusDraft,
		AssigneeIDs: assigneeIDs,
		IsPrivate:   isPrivate,
		IsTest:      isTest,
		FieldValues: fieldValues,
	})
}

// persistCaseInput is the shared input for persistCase, used by both the
// "create open case" and "create draft" flows.
type persistCaseInput struct {
	Title       string
	Description string
	Status      types.CaseStatus
	AssigneeIDs []string
	IsPrivate   bool
	IsTest      bool
	FieldValues map[string]model.FieldValue
	RequestKey  string
}

// persistCase performs request-key deduplication, field validation, and
// repository write. It does NOT run any activation side effects — callers
// must invoke activateChannelModeCase separately when those should fire.
func (uc *CaseUseCase) persistCase(ctx context.Context, workspaceID string, in persistCaseInput) (*model.Case, error) {
	// Title is required for OPEN cases (the human flow needs a meaningful
	// listing entry); drafts may be saved with an empty title so a partial
	// entry can be picked up later.
	if !in.Status.IsDraft() && in.Title == "" {
		return nil, goerr.New("case title is required")
	}

	// Private cases are channel-mode-only: the sole effect of IsPrivate is a
	// dedicated private Slack channel, which thread-mode cases (bound to the
	// monitored channel) have no equivalent for. Reject the invalid combination
	// at this shared chokepoint so every create entry point is covered. The
	// mode is only consulted for a private request, so the common path is
	// unchanged; a lookup failure here fails closed (propagated), never open.
	if in.IsPrivate {
		threadMode, err := uc.workspaceIsThreadMode(workspaceID)
		if err != nil {
			return nil, err
		}
		if threadMode {
			return nil, assertThreadCaseVisibility(workspaceID, in.IsPrivate)
		}
	}

	// Check request key: if a case with this key already exists, return it.
	// RequestKey deduplication applies only to non-draft submissions; drafts
	// do not currently carry a request key.
	if in.RequestKey != "" {
		existing, err := uc.repo.Case().GetByRequestKey(ctx, workspaceID, in.RequestKey)
		if err != nil {
			errutil.Handle(ctx, err, "failed to check request key key")
		} else if existing != nil {
			return existing, nil
		}
	}

	// Validate and enrich custom fields with Type from config, and verify
	// every referenced user (assignees + user-field values) exists. Drafts use
	// the partial mode: supplied fields are type-checked, but missing required
	// fields do NOT fail — half-finished entries are the whole point of the
	// draft state. The full required-field check runs again in SubmitDraft
	// before promoting the case to OPEN.
	mode := validateAll
	if in.Status.IsDraft() {
		mode = validatePartial
	}
	enriched, err := uc.validateCaseWrite(ctx, workspaceID, mode, in.FieldValues, in.AssigneeIDs)
	if err != nil {
		return nil, goerr.Wrap(err, "case write validation failed")
	}
	in.FieldValues = enriched

	// Set reporter from auth context (immutable after creation).
	var reporterID string
	if token, tokenErr := auth.TokenFromContext(ctx); tokenErr == nil {
		reporterID = token.Sub
	}

	now := time.Now().UTC()
	caseModel := &model.Case{
		Title:       in.Title,
		Description: in.Description,
		Status:      in.Status,
		ReporterID:  reporterID,
		AssigneeIDs: in.AssigneeIDs,
		IsPrivate:   in.IsPrivate,
		IsTest:      in.IsTest,
		FieldValues: in.FieldValues,
		RequestKey:  in.RequestKey,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	created, err := uc.repo.Case().Create(ctx, workspaceID, caseModel)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to create case")
	}
	return created, nil
}

// activateChannelModeCase runs all post-persistence side effects required to
// bring a channel-mode OPEN case to life: Slack channel creation, optional
// cross-workspace connect, invites, bookmark, welcome messages, and
// channel-member sync. Returns the updated case (with SlackChannelID /
// ChannelUserIDs filled in).
//
// It is intentionally non-destructive: on Slack channel creation failure it
// returns the error without touching the persisted case. The caller decides the
// rollback policy:
//
//   - createChannelModeCase rolls back by deleting the just-persisted case so
//     the entire "create" call appears atomic to the user.
//   - SubmitDraft rolls back by flipping the case status back to DRAFT so the
//     user does not lose work they had saved.
//
// Activation is a no-op when no Slack service is configured. The validated
// entry (may be nil for callers that cannot supply it) is checked so that a
// mis-routed thread-mode case never reaches CreateChannel — the only place a
// dedicated channel is provisioned.
//
// existingSlackChannelID, when non-empty, is an already-validated channel the
// case links to instead of a new one: CreateChannel and the cross-workspace
// connect are skipped, and every later step runs unchanged.
func (uc *CaseUseCase) activateChannelModeCase(ctx context.Context, workspaceID string, entry *model.WorkspaceEntry, c *model.Case, sourceTeamID string, existingSlackChannelID string) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	if uc.slackService == nil {
		return c, nil
	}
	// Defend the invariant at the channel-creating boundary: with Slack wired, a
	// thread-mode case must never reach CreateChannel. Gated on slackService != nil
	// so the Slack-absent fall-through (a thread-mode workspace with no Slack, which
	// openInWorkspaceMode routes here as a no-op create) is not rejected. The funnel
	// already routes thread-mode-with-Slack to the thread path, so this only fires
	// on a wiring bug.
	if entry != nil && entry.IsThreadMode() {
		return nil, goerr.New("channel activation attempted on thread-mode workspace",
			goerr.V(CaseIDKey, c.ID), goerr.V("workspace_id", workspaceID))
	}

	channelID := existingSlackChannelID
	if channelID == "" {
		prefix := uc.slackChannelPrefixForWorkspace(workspaceID)
		teamID := uc.slackTeamIDForWorkspace(workspaceID)
		created, err := uc.slackService.CreateChannel(ctx, c.ID, c.Title, prefix, c.IsPrivate, teamID)
		if err != nil {
			return nil, goerr.Wrap(err, "failed to create Slack channel for case", goerr.V(CaseIDKey, c.ID))
		}
		channelID = created

		// Connect channel to the source workspace if it differs from the configured team.
		//
		// Both team IDs must be known: the source team (from the Slack payload) AND
		// the home team (workspace config, teamID). When the workspace has no
		// SlackTeamID configured, teamID is empty; without it we cannot build a valid
		// target_team_ids set, and Slack rejects a request carrying an empty team id
		// (surfacing as not_an_enterprise on non-Enterprise-Grid workspaces). Skip
		// rather than fire a call that can only fail.
		if teamID != "" && sourceTeamID != "" && sourceTeamID != teamID {
			if uc.slackAdminService != nil {
				if connectErr := uc.slackAdminService.ConnectChannelToWorkspace(ctx, channelID, []string{teamID, sourceTeamID}); connectErr != nil {
					errutil.Handle(ctx, connectErr, "failed to connect channel to source workspace")
				}
			}
		}
	}

	// Invite reporter, the actor (= auth-context user that triggered the
	// activation), assignees, and auto-invite users to the channel.
	//
	// The "actor" inclusion is what keeps the SubmitDraft path symmetric
	// with CreateCase: when Alice creates a draft and Bob promotes it,
	// the reporter (Alice) and the submitter (Bob) both need to be in
	// the channel — otherwise Bob, who just kicked off the case from
	// Web, would end up unable to follow it in Slack. For CreateCase the
	// actor and reporter are usually the same person, so the extra
	// append simply dedupes through uniqueStrings below.
	usersToInvite := make([]string, 0, len(c.AssigneeIDs)+2)
	if c.ReporterID != "" {
		usersToInvite = append(usersToInvite, c.ReporterID)
	}
	if token, tokenErr := auth.TokenFromContext(ctx); tokenErr == nil && token.Sub != "" {
		usersToInvite = append(usersToInvite, token.Sub)
	}
	usersToInvite = append(usersToInvite, c.AssigneeIDs...)
	autoInviteUsers := uc.resolveAutoInviteUsers(ctx, workspaceID)
	usersToInvite = append(usersToInvite, autoInviteUsers...)
	usersToInvite = uniqueStrings(usersToInvite)

	if len(usersToInvite) > 0 {
		if inviteErr := uc.slackService.InviteUsersToChannel(ctx, channelID, usersToInvite); inviteErr != nil {
			errutil.Handle(ctx, inviteErr, "failed to invite users to Slack channel")
		}
	}

	// Add bookmark to the Slack channel linking to the case WebUI.
	caseURL := ""
	if uc.baseURL != "" {
		caseURL = fmt.Sprintf("%s/ws/%s/cases/%d", uc.baseURL, workspaceID, c.ID)
		if bookmarkErr := uc.slackService.AddBookmark(ctx, channelID, i18n.T(ctx, i18n.MsgBookmarkOpenCase), caseURL); bookmarkErr != nil {
			errutil.Handle(ctx, bookmarkErr, "failed to add bookmark to Slack channel")
		}
	}

	// Post welcome messages defined in workspace configuration. The Case
	// passed to the renderer carries the freshly-assigned channel ID so
	// templates can reference it.
	c.SlackChannelID = channelID
	uc.postWelcomeMessages(ctx, workspaceID, c, channelID, caseURL)

	// Sync channel members (for both private and public cases).
	var channelUserIDs []string
	members, membersErr := uc.slackService.GetConversationMembers(ctx, channelID)
	if membersErr != nil {
		errutil.Handle(ctx, membersErr, "failed to get channel members during case creation")
	} else {
		channelUserIDs = filterHumanUsers(ctx, uc.repo, members)
	}

	c.ChannelUserIDs = channelUserIDs
	c.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Case().Update(ctx, workspaceID, c)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update case with Slack channel ID",
			goerr.V("orphaned_channel_id", channelID),
			goerr.V("existing_channel", existingSlackChannelID != ""),
			goerr.V(CaseIDKey, c.ID))
	}
	return updated, nil
}

// CaseUpdate represents a partial update to a Case. Each pointer/slice is
// nil-vs-set: nil means "preserve the existing value", a non-nil pointer
// means "set to this value (including empty string)". For Fields the nil
// case preserves all stored field values; a non-nil map merges the supplied
// entries on top of the existing ones (entries are not removed individually
// — clients should send the empty value to clear a field if needed).
//
// Assignees are intentionally NOT part of this patch. They are mutated only
// through the dedicated AssignCase / UnassignCase delta operations, so every
// entry point shares one race-free path and a full-list replace can never
// silently clobber a concurrent edit.
type CaseUpdate struct {
	Title       *string
	Description *string
	// IsTest, when non-nil, sets the test flag; nil preserves the stored value.
	// A pointer is required to distinguish "leave unchanged" from "set false".
	IsTest *bool
	// nil means "preserve all stored field values". A non-nil map merges its
	// entries on top of the existing values (callers cannot remove individual
	// entries via this API).
	Fields map[string]model.FieldValue
}

func (uc *CaseUseCase) UpdateCase(ctx context.Context, workspaceID string, id int64, patch CaseUpdate) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	// Load with the shared write access gate (draft-aware: private drafts fall
	// back to a reporter check since they have no Slack channel yet). The loaded
	// Case is preserved verbatim except for the fields the caller touches below.
	existingCase, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	// Apply the patch onto the loaded Case (PATCH, not PUT): only fields the
	// caller actually set are overwritten; everything else — SlackThreadTS,
	// BoardStatus, RequestKey, AgentAdditionalPrompt, AgentSourceIDs, assignees,
	// Status, timestamps — is left exactly as stored, then persisted verbatim.
	// Reconstructing a fresh &model.Case{...} (PUT) silently dropped every field
	// not re-listed: that is how a thread-mode Case lost its SlackThreadTS on any
	// edit, unbinding it from its Slack thread so GetBySlackThread stopped finding
	// it — the next mention then created a duplicate Case and the on-closed Job
	// posted to the channel root instead of the thread.
	// Drafts may carry an empty title — that's the whole point of the "save in
	// progress" state. The empty-title gate fires again at SubmitDraft time,
	// before promoting to OPEN.
	if patch.Title != nil && *patch.Title == "" && !existingCase.IsDraft() {
		return nil, goerr.New("case title cannot be empty", goerr.V(CaseIDKey, id))
	}

	if patch.Description != nil {
		existingCase.Description = *patch.Description
	}

	if patch.IsTest != nil {
		existingCase.IsTest = *patch.IsTest
	}

	// Validate the submitted fields through the shared gate, then merge the
	// enriched values onto the existing ones. Without a field patch, the map is
	// left untouched (no validator pass — stale option IDs from a prior config
	// must not cause an unrelated update to fail).
	if patch.Fields != nil {
		validated, err := uc.validateCaseWrite(ctx, workspaceID, validatePartialStrict, patch.Fields, nil)
		if err != nil {
			return nil, goerr.Wrap(err, "case write validation failed", goerr.V(CaseIDKey, id))
		}
		existingCase.FieldValues = mergeFieldValues(existingCase.FieldValues, validated)
	}

	// Everything above this line still sees the stored title; capture it here so
	// the change notification below can report the before/after pair.
	beforeTitle := existingCase.Title

	// Rename the Slack channel only after EVERY validation has passed. Renaming
	// is an external side effect that cannot be rolled back, so a later field
	// validation failure must not leave the channel renamed while the DB still
	// holds the old title (state desync). existingCase.Title is still the old
	// title here — it is assigned below.
	//
	// Channel-mode Cases own a dedicated Slack channel whose name mirrors the
	// title, so a title change renames it. Thread-mode Cases (IsThreadBound)
	// have NO dedicated channel: SlackChannelID points at the shared monitored
	// channel that hosts many threads, and renaming it would rename that shared
	// channel out from under every other thread (and fails with not_authorized
	// in practice). Skip the rename entirely for thread-mode Cases.
	if patch.Title != nil && uc.slackService != nil && !existingCase.IsThreadBound() && existingCase.SlackChannelID != "" && existingCase.Title != *patch.Title {
		prefix := uc.slackChannelPrefixForWorkspace(workspaceID)
		if err := uc.slackService.RenameChannel(ctx, existingCase.SlackChannelID, id, *patch.Title, prefix); err != nil {
			return nil, goerr.Wrap(err, "failed to rename Slack channel",
				goerr.V(CaseIDKey, id),
				goerr.V("channel_id", existingCase.SlackChannelID))
		}
	}

	if patch.Title != nil {
		existingCase.Title = *patch.Title
	}

	existingCase.UpdatedAt = time.Now().UTC()

	updated, err := uc.repo.Case().Update(ctx, workspaceID, existingCase)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update case", goerr.V(CaseIDKey, id))
	}

	// Tell the thread that the title moved. Only a real change is announced, so
	// a re-submit of the same title stays silent.
	if patch.Title != nil && beforeTitle != updated.Title {
		actor := i18n.T(ctx, i18n.MsgChangeActorSystem)
		if tok, terr := auth.TokenFromContext(ctx); terr == nil && tok.Sub != "" {
			actor = mentionUser(tok.Sub)
		}
		uc.postThreadContextLine(ctx, updated,
			i18n.T(ctx, i18n.MsgCaseChangeTitle, actor, beforeTitle, updated.Title))
	}

	return updated, nil
}

// AssignCase atomically adds the given Slack user IDs to the case's assignee
// set. Unlike UpdateCase — which replaces the whole assignee list and therefore
// loses a concurrent edit inside its read-modify-write window — the add is
// applied as a transactional set union in the repository, so two simultaneous
// "assign me" actions both land. IDs already assigned are ignored. New
// assignees must resolve to known Slack users. An empty userIDs slice is a
// no-op that returns the case unchanged.
func (uc *CaseUseCase) AssignCase(ctx context.Context, workspaceID string, id int64, userIDs []string) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	// Fail fast through the shared read gate: it is what turns a missing case
	// into ErrCaseNotFound and a private case into ErrAccessDenied, which
	// callers (GraphQL error mapping, agent tools) discriminate on. The
	// transaction below re-checks access on the state it actually writes, so
	// this read is never the basis for any decision that gets persisted or
	// announced.
	existingCase, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	// An empty input is a guaranteed no-op; short-circuit before the user
	// existence check and the transaction.
	if len(userIDs) == 0 {
		return existingCase, nil
	}

	// Verify the users BEFORE opening the transaction: it reads a different
	// collection (SlackUser), and Firestore re-runs the transaction body on
	// contention — a cross-collection read inside it would repeat on every
	// attempt for no gain.
	if err := uc.verifyUsersExist(ctx, userIDs, nil); err != nil {
		return nil, goerr.Wrap(err, "assignee verification failed", goerr.V(CaseIDKey, id))
	}

	actorID, checkAccess := tokenActor(ctx)
	var added []string
	updated, txErr := uc.repo.Case().Transact(ctx, workspaceID, id, func(c *model.Case) error {
		// Reset: Firestore may re-run this closure, and a leftover value from a
		// previous attempt would announce assignees this attempt did not make.
		added = nil
		if err := assertCaseWriteAccess(c, actorID, checkAccess); err != nil {
			return err
		}
		before := slices.Clone(c.AssigneeIDs)
		if c.AssignUsers(userIDs) {
			c.UpdatedAt = time.Now().UTC()
		}
		added, _ = diffAssignees(before, c.AssigneeIDs)
		return nil
	})
	if txErr != nil {
		return nil, goerr.Wrap(txErr, "failed to add assignees", goerr.V(CaseIDKey, id))
	}

	// added was computed inside the transaction against the very state that was
	// written, so a concurrent assign of the same user cannot make this announce
	// a change it did not make.
	if len(added) > 0 {
		actor := i18n.T(ctx, i18n.MsgChangeActorSystem)
		if tok, terr := auth.TokenFromContext(ctx); terr == nil && tok.Sub != "" {
			actor = mentionUser(tok.Sub)
		}
		mentions := make([]string, 0, len(added))
		for _, uid := range added {
			mentions = append(mentions, mentionUser(uid))
		}
		uc.postThreadContextLine(ctx, updated,
			i18n.T(ctx, i18n.MsgCaseChangeAssigneeAssigned, actor, strings.Join(mentions, ", ")))
	}

	return updated, nil
}

// UnassignCase atomically removes the given Slack user IDs from the case's
// assignee set. IDs not currently assigned are ignored. Removal needs no user
// existence check (a since-deleted user must still be removable). An empty
// userIDs slice is a no-op that returns the case unchanged.
func (uc *CaseUseCase) UnassignCase(ctx context.Context, workspaceID string, id int64, userIDs []string) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	// Same fail-fast read as AssignCase: it owns the ErrCaseNotFound /
	// ErrAccessDenied contract, never the diff.
	existingCase, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	if len(userIDs) == 0 {
		return existingCase, nil
	}

	actorID, checkAccess := tokenActor(ctx)
	var removed []string
	updated, txErr := uc.repo.Case().Transact(ctx, workspaceID, id, func(c *model.Case) error {
		removed = nil
		if err := assertCaseWriteAccess(c, actorID, checkAccess); err != nil {
			return err
		}
		before := slices.Clone(c.AssigneeIDs)
		if c.UnassignUsers(userIDs) {
			c.UpdatedAt = time.Now().UTC()
		}
		_, removed = diffAssignees(before, c.AssigneeIDs)
		return nil
	})
	if txErr != nil {
		return nil, goerr.Wrap(txErr, "failed to remove assignees", goerr.V(CaseIDKey, id))
	}

	// Mirror of AssignCase: the diff comes from inside the transaction, so
	// unassigning someone another request already removed stays silent.
	if len(removed) > 0 {
		actor := i18n.T(ctx, i18n.MsgChangeActorSystem)
		if tok, terr := auth.TokenFromContext(ctx); terr == nil && tok.Sub != "" {
			actor = mentionUser(tok.Sub)
		}
		mentions := make([]string, 0, len(removed))
		for _, uid := range removed {
			mentions = append(mentions, mentionUser(uid))
		}
		uc.postThreadContextLine(ctx, updated,
			i18n.T(ctx, i18n.MsgCaseChangeAssigneeUnassigned, actor, strings.Join(mentions, ", ")))
	}

	return updated, nil
}

// diffAssignees computes the delta needed to turn the current assignee set
// into the desired one: toAdd are ids in desired but not current, toRemove are
// ids in current but not desired. It lets a full-selection UI (e.g. a Slack
// multi-user select) reconcile through the delta AssignCase / UnassignCase
// path instead of a full-list replace. Blank ids in either input are ignored.
func diffAssignees(current, desired []string) (toAdd, toRemove []string) {
	currentSet := make(map[string]struct{}, len(current))
	for _, id := range current {
		if id != "" {
			currentSet[id] = struct{}{}
		}
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, id := range desired {
		if id == "" {
			continue
		}
		desiredSet[id] = struct{}{}
		if _, ok := currentSet[id]; !ok {
			toAdd = append(toAdd, id)
		}
	}
	for _, id := range current {
		if id == "" {
			continue
		}
		if _, ok := desiredSet[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}
	return toAdd, toRemove
}

// UpdateAgentSettings replaces the Case-specific agent additional prompt
// and the AgentSourceIDs whitelist. enabledSourceIDs == nil or empty
// resets the selection to "use every Source". Non-empty IDs are
// validated against the Workspace's Source list — any unknown ID makes
// the whole update fail with ErrInvalidArgument (we never silently
// drop an ID the caller meant to keep). Order is preserved exactly as
// supplied so the UI selection round-trips unchanged.
func (uc *CaseUseCase) UpdateAgentSettings(ctx context.Context, workspaceID string, caseID int64, additionalPrompt string, enabledSourceIDs []model.SourceID) (*model.Case, error) {
	existing, err := loadCaseForWrite(ctx, uc.repo, workspaceID, caseID)
	if err != nil {
		return nil, err
	}

	// Drafts cannot carry agent settings (no agent runs against an unsubmitted
	// draft anyway), so reject the call. The shared access gate already ran, so
	// a non-reporter on a private draft is denied before reaching this point.
	if existing.IsDraft() {
		return nil, goerr.Wrap(ErrCaseIsDraft,
			"agent settings are unavailable on drafts",
			goerr.V(CaseIDKey, caseID))
	}

	// Validate Source IDs against the workspace catalogue. We load the
	// full list (small per workspace) once instead of N parallel Gets
	// because callers typically pick a handful.
	if len(enabledSourceIDs) > 0 {
		sources, err := uc.repo.Source().List(ctx, workspaceID)
		if err != nil {
			return nil, goerr.Wrap(err, "list sources for agent settings",
				goerr.V("workspace_id", workspaceID))
		}
		known := make(map[model.SourceID]struct{}, len(sources))
		for _, s := range sources {
			known[s.ID] = struct{}{}
		}
		seen := make(map[model.SourceID]struct{}, len(enabledSourceIDs))
		for _, id := range enabledSourceIDs {
			if id == "" {
				return nil, goerr.Wrap(ErrInvalidArgument,
					"source id is empty", goerr.V(CaseIDKey, caseID))
			}
			if _, ok := known[id]; !ok {
				return nil, goerr.Wrap(ErrInvalidArgument,
					"unknown source id",
					goerr.V("source_id", string(id)),
					goerr.V(CaseIDKey, caseID))
			}
			if _, dup := seen[id]; dup {
				return nil, goerr.Wrap(ErrInvalidArgument,
					"duplicate source id",
					goerr.V("source_id", string(id)),
					goerr.V(CaseIDKey, caseID))
			}
			seen[id] = struct{}{}
		}
	}

	// Mutate the existing pointer rather than rebuilding the struct: the
	// rebuild pattern silently drops any new field added to model.Case
	// later. The repository's Update is a pure Set; whatever we hand
	// it is what lands in storage.
	existing.AgentAdditionalPrompt = additionalPrompt
	if len(enabledSourceIDs) == 0 {
		existing.AgentSourceIDs = nil
	} else {
		existing.AgentSourceIDs = append([]model.SourceID(nil), enabledSourceIDs...)
	}
	existing.UpdatedAt = time.Now().UTC()

	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update case agent settings",
			goerr.V(CaseIDKey, caseID))
	}
	return updated, nil
}

func (uc *CaseUseCase) DeleteCase(ctx context.Context, workspaceID string, id int64) error {
	// Load with the shared write access gate before the cascade delete.
	if _, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id); err != nil {
		return err
	}

	// Cascade-delete actions associated with this case. We pull every
	// action (archived included) because the case itself is being removed,
	// and orphaned action documents would otherwise leak. The repository's
	// Delete is INTERNAL to this cascade — public callers archive instead.
	actions, err := uc.repo.Action().GetByCase(ctx, workspaceID, id, interfaces.ActionListOptions{ArchiveScope: interfaces.ActionArchiveScopeAll})
	if err != nil {
		return goerr.Wrap(err, "failed to get actions for case", goerr.V(CaseIDKey, id))
	}

	for _, action := range actions {
		if err := uc.repo.Action().Delete(ctx, workspaceID, action.ID); err != nil {
			return goerr.Wrap(err, "failed to delete action",
				goerr.V(CaseIDKey, id),
				goerr.V(ActionIDKey, action.ID))
		}
	}

	// Delete case (field values are embedded, so they are deleted with the case)
	if err := uc.repo.Case().Delete(ctx, workspaceID, id); err != nil {
		return goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, id))
	}

	return nil
}

func (uc *CaseUseCase) GetCase(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	caseModel, err := uc.repo.Case().Get(ctx, workspaceID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, id))
	}

	token, tokenErr := auth.TokenFromContext(ctx)

	// Private drafts have no Slack channel yet, so the usual
	// ChannelUserIDs-based access check would lock out the reporter too.
	// For private drafts we restrict visibility to the reporter; public
	// drafts behave like any other case (workspace-wide listing).
	if caseModel.IsDraft() && caseModel.IsPrivate && tokenErr == nil && caseModel.ReporterID != token.Sub {
		return nil, goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, id))
	}

	// Access control for non-draft private cases.
	if tokenErr == nil && !caseModel.IsDraft() && !model.IsCaseAccessible(caseModel, token.Sub) {
		return model.RestrictCase(caseModel), nil
	}

	return caseModel, nil
}

// GetCases retrieves multiple cases by ID in a single batch, applying the same
// per-case access control as GetCase. It fetches all requested cases with one
// repository batch call (repo.Case().GetByIDs) to avoid N+1 round-trips, then:
//   - omits cases that do not exist;
//   - omits private drafts the caller did not author (their existence must not
//     leak);
//   - RestrictCases non-draft private cases the caller cannot access.
//
// Results preserve the order of ids; duplicate ids yield duplicate entries.
func (uc *CaseUseCase) GetCases(ctx context.Context, workspaceID string, ids []int64) ([]*model.Case, error) {
	if len(ids) == 0 {
		return []*model.Case{}, nil
	}

	found, err := uc.repo.Case().GetByIDs(ctx, workspaceID, ids)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to batch get cases")
	}

	token, tokenErr := auth.TokenFromContext(ctx)
	out := make([]*model.Case, 0, len(ids))
	for _, id := range ids {
		caseModel, ok := found[id]
		if !ok {
			continue
		}
		// Mirrors GetCase: a private draft is visible only to its reporter.
		if caseModel.IsDraft() && caseModel.IsPrivate && tokenErr == nil && caseModel.ReporterID != token.Sub {
			continue
		}
		if tokenErr == nil && !caseModel.IsDraft() && !model.IsCaseAccessible(caseModel, token.Sub) {
			out = append(out, model.RestrictCase(caseModel))
			continue
		}
		out = append(out, caseModel)
	}
	return out, nil
}

// ListCases returns the workspace's cases, optionally narrowed to one
// lifecycle status and to one archive slice. scope is an explicit parameter
// rather than an option with a default so every caller states which slice it
// wants: user-facing listings pass CaseArchiveScopeActiveOnly, the Archived
// tab passes CaseArchiveScopeArchivedOnly.
func (uc *CaseUseCase) ListCases(ctx context.Context, workspaceID string, status *types.CaseStatus, scope interfaces.CaseArchiveScope) ([]*model.Case, error) {
	opts := []interfaces.ListCaseOption{interfaces.WithArchiveScope(scope)}
	if status != nil {
		opts = append(opts, interfaces.WithStatus(*status))
	}

	cases, err := uc.repo.Case().List(ctx, workspaceID, opts...)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list cases")
	}

	// Access control for private cases
	token, tokenErr := auth.TokenFromContext(ctx)
	if tokenErr == nil {
		for i, c := range cases {
			if !model.IsCaseAccessible(c, token.Sub) {
				cases[i] = model.RestrictCase(c)
			}
		}
	}

	// The repositories return cases in an unspecified order (memory iterates a
	// map; Firestore orders by document ID). Sort newest-first here so every
	// caller gets a stable, intuitive listing without forcing a Firestore
	// composite index (Status filter + OrderBy would require one).
	sort.SliceStable(cases, func(i, j int) bool {
		return cases[i].CreatedAt.After(cases[j].CreatedAt)
	})

	return cases, nil
}

// referenceableCasesLimit caps how many candidate cases a case_ref picker
// / agent search returns in one call. It also serves as the default when the
// caller passes a non-positive limit.
const referenceableCasesLimit = 50

// ListReferenceableCases returns the non-private, non-draft Cases of
// workspaceID as case_ref candidates. Unlike ListCases, private Cases are
// dropped entirely (never RestrictCase'd) so a picker cannot even reveal that a
// private Case exists — the project rule that private Cases are not
// referenceable. With an empty query, OPEN cases sort first, then by most
// recently updated. With a query, results match the Case title (substring,
// case-insensitive) or the Case ID ("#42" / "42"), sorted by most recently
// updated. At most limit (clamped to referenceableCasesLimit) rows are
// returned. DRAFT cases are excluded because List excludes them by default,
// and archived cases for the same reason: passing no options selects the
// active-only archive scope, so a case that was put away stops being offered
// as a new reference (existing references resolve by id and are unaffected).
func (uc *CaseUseCase) ListReferenceableCases(ctx context.Context, workspaceID, query string, limit int) ([]model.CaseRef, error) {
	if limit <= 0 || limit > referenceableCasesLimit {
		limit = referenceableCasesLimit
	}

	cases, err := uc.repo.Case().List(ctx, workspaceID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list referenceable cases",
			goerr.V("reference_workspace", workspaceID))
	}

	q := strings.TrimSpace(query)
	lowerQ := strings.ToLower(q)
	var idQuery int64 = -1
	if n, perr := strconv.ParseInt(strings.TrimPrefix(q, "#"), 10, 64); perr == nil {
		idQuery = n
	}

	matched := make([]*model.Case, 0, len(cases))
	for _, c := range cases {
		// List excludes drafts by default, but enforce the "drafts are never
		// referenceable" rule explicitly so it does not depend on that default.
		if c.IsPrivate || c.Status.Normalize() == types.CaseStatusDraft {
			continue
		}
		if q != "" {
			if !strings.Contains(strings.ToLower(c.Title), lowerQ) && c.ID != idQuery {
				continue
			}
		}
		matched = append(matched, c)
	}

	openFirst := q == ""
	sort.SliceStable(matched, func(i, j int) bool {
		if openFirst {
			oi := matched[i].Status.Normalize() == types.CaseStatusOpen
			oj := matched[j].Status.Normalize() == types.CaseStatusOpen
			if oi != oj {
				return oi // OPEN cases sort before non-OPEN
			}
		}
		return matched[i].UpdatedAt.After(matched[j].UpdatedAt)
	})

	if len(matched) > limit {
		matched = matched[:limit]
	}

	refs := make([]model.CaseRef, len(matched))
	for i, c := range matched {
		refs[i] = model.NewCaseRef(workspaceID, c)
	}
	return refs, nil
}

// ResolveCaseRefs resolves the given Case IDs in workspaceID to their CaseRef
// summaries, dropping any ID that is missing, private, or a draft (the caller
// renders those as "unavailable"). Used to label existing case_ref values
// for display. A single batch lookup keeps it N+1-safe.
func (uc *CaseUseCase) ResolveCaseRefs(ctx context.Context, workspaceID string, ids []int64) ([]model.CaseRef, error) {
	cases, err := uc.GetReferenceableCases(ctx, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	refs := make([]model.CaseRef, len(cases))
	for i, c := range cases {
		refs[i] = model.NewCaseRef(workspaceID, c)
	}
	return refs, nil
}

// GetReferenceableCases batch-fetches the full Cases for the given IDs in
// workspaceID, returning only those that are referenceable (non-private,
// non-draft, existing). IDs that are missing / private / draft are omitted, so
// the caller can diff the input against the result to report them. Results
// preserve the input ID order. This backs the agent's detail-fetch tool.
func (uc *CaseUseCase) GetReferenceableCases(ctx context.Context, workspaceID string, ids []int64) ([]*model.Case, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// References into a workspace the caller may not access are omitted, the
	// same way private and draft cases are: they render as unavailable.
	if err := uc.workspaceAccess.AuthorizeCurrentUser(ctx, workspaceID); err != nil {
		if isWorkspaceAccessDenied(err) {
			return nil, nil
		}
		return nil, goerr.Wrap(err, "authorize reference workspace",
			goerr.V("reference_workspace", workspaceID))
	}
	found, err := uc.repo.Case().GetByIDs(ctx, workspaceID, ids)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to fetch referenceable cases",
			goerr.V("reference_workspace", workspaceID))
	}
	out := make([]*model.Case, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		c, ok := found[id]
		if !ok || c.IsPrivate || c.Status.Normalize() == types.CaseStatusDraft {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ReferenceWorkspaceForField returns the reference_workspace configured for the
// case_ref field fieldID in workspaceID. It errors when the field is
// unknown or is not a case_ref type, so an agent tool that was handed a
// wrong field id fails loudly instead of querying the wrong workspace.
func (uc *CaseUseCase) ReferenceWorkspaceForField(workspaceID, fieldID string) (string, error) {
	schema := uc.fieldSchemaForWorkspace(workspaceID)
	if schema == nil {
		return "", goerr.Wrap(ErrCaseNotFound, "workspace has no field schema",
			goerr.V("workspace_id", workspaceID))
	}
	for _, fd := range schema.Fields {
		if fd.ID == fieldID {
			if !fd.Type.IsCaseRef() {
				return "", goerr.New("field is not a case_ref field",
					goerr.V("field_id", fieldID), goerr.V("field_type", fd.Type))
			}
			return fd.ReferenceWorkspace, nil
		}
	}
	return "", goerr.New("field not defined in the workspace schema",
		goerr.V("field_id", fieldID), goerr.V("workspace_id", workspaceID))
}

// RenderCaseFieldValues flattens a Case's field values into a plain map for an
// agent tool response. Non-reference fields render their raw stored value (as
// the existing tool responses do). Case-reference fields are resolved one level
// — each referenced Case ID becomes {id, title, status} — using the field's
// configured reference_workspace. An ID that no longer resolves (the referenced
// Case was deleted, made private, or turned into a draft after it was stored)
// renders as {id, available:false}; this read-time drift is the only place an
// "unavailable" reference can appear, since writes are gated by
// verifyCaseRefsExist. A repository lookup failure is propagated, never
// swallowed.
func (uc *CaseUseCase) RenderCaseFieldValues(ctx context.Context, workspaceID string, fieldValues map[string]model.FieldValue) (map[string]any, error) {
	out := make(map[string]any, len(fieldValues))
	schema := uc.fieldSchemaForWorkspace(workspaceID)
	defByID := make(map[string]config.FieldDefinition)
	if schema != nil {
		for _, fd := range schema.Fields {
			defByID[fd.ID] = fd
		}
	}

	for id, fv := range fieldValues {
		if !fv.Type.IsCaseRef() {
			out[id] = fv.Value
			continue
		}
		fd, ok := defByID[id]
		if !ok || fd.ReferenceWorkspace == "" {
			// Field removed from the schema (drift) or with no target workspace:
			// fall back to the raw stored value instead of failing the whole
			// field_values render with a doomed cross-workspace lookup.
			out[id] = fv.Value
			continue
		}
		ids, err := caseRefIDs(fv)
		if err != nil {
			return nil, goerr.Wrap(err, "invalid stored case reference value",
				goerr.V("field_id", id))
		}
		resolved, err := uc.ResolveCaseRefs(ctx, fd.ReferenceWorkspace, ids)
		if err != nil {
			return nil, err
		}
		byID := make(map[int64]model.CaseRef, len(resolved))
		for _, r := range resolved {
			byID[r.ID] = r
		}
		render := func(refID int64) map[string]any {
			if r, ok := byID[refID]; ok {
				return map[string]any{"id": r.ID, "title": r.Title, "status": r.Status.String()}
			}
			return map[string]any{"id": refID, "available": false}
		}
		if fv.Type == types.FieldTypeCaseRef {
			if len(ids) == 0 {
				out[id] = nil
			} else {
				out[id] = render(ids[0])
			}
			continue
		}
		arr := make([]any, 0, len(ids))
		for _, refID := range ids {
			arr = append(arr, render(refID))
		}
		out[id] = arr
	}
	return out, nil
}

func (uc *CaseUseCase) CloseCase(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	existing, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	// Thread-mode cases close by moving to a closed board status (UpdateCaseStatus),
	// which keeps BoardStatus and the lifecycle Status in sync. Closing one
	// directly here would desync them, so reject it at the boundary — this is the
	// usecase-level counterpart of UpdateCaseStatus rejecting channel-mode cases.
	if existing.IsThreadBound() {
		return nil, goerr.Wrap(ErrCaseThreadModeUseStatus, "thread-mode case cannot be closed directly", goerr.V(CaseIDKey, id))
	}

	status := existing.Status.Normalize()
	if status == types.CaseStatusDraft {
		return nil, goerr.Wrap(ErrCaseIsDraft, "draft case cannot be closed", goerr.V(CaseIDKey, id))
	}
	if status == types.CaseStatusClosed {
		return nil, goerr.Wrap(ErrCaseAlreadyClosed, "case is already closed", goerr.V(CaseIDKey, id))
	}

	existing.Status = types.CaseStatusClosed
	existing.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to close case", goerr.V(CaseIDKey, id))
	}

	uc.publishLifecycle(ctx, workspaceID, updated, model.CaseLifecycleClosed)
	return updated, nil
}

func (uc *CaseUseCase) ReopenCase(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	existing, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	// Thread-mode cases reopen by moving to a non-closed board status
	// (UpdateCaseStatus); reopening directly here would desync BoardStatus and
	// the lifecycle Status. Reject at the boundary, mirroring CloseCase.
	if existing.IsThreadBound() {
		return nil, goerr.Wrap(ErrCaseThreadModeUseStatus, "thread-mode case cannot be reopened directly", goerr.V(CaseIDKey, id))
	}

	// An archived case is CLOSED by construction, so this check must come
	// before the DRAFT one to report the specific reason rather than falling
	// through. Reopening it would produce an OPEN case that appears in no
	// list, no board, no dashboard and no Job scan.
	if existing.IsArchived() {
		return nil, goerr.Wrap(ErrCaseArchived, "archived case cannot be reopened; unarchive it first", goerr.V(CaseIDKey, id))
	}

	status := existing.Status.Normalize()
	if status == types.CaseStatusDraft {
		return nil, goerr.Wrap(ErrCaseIsDraft, "draft case cannot be reopened", goerr.V(CaseIDKey, id))
	}
	if status == types.CaseStatusOpen {
		return nil, goerr.Wrap(ErrCaseAlreadyOpen, "case is already open", goerr.V(CaseIDKey, id))
	}

	existing.Status = types.CaseStatusOpen
	existing.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to reopen case", goerr.V(CaseIDKey, id))
	}

	return updated, nil
}

// ArchiveCase marks a CLOSED case as archived so it disappears from the
// default Cases list and the Case board. The case document is preserved and
// can be restored via UnarchiveCase; deleteCase remains the only permanent
// removal.
//
// Only a CLOSED case may be archived: archiving hides the case from the list,
// the board, the dashboard and the Job scan at once, and an OPEN case
// vanishing from all of them leaves an operator no way to see why.
//
// Idempotency: archiving an already-archived case returns
// ErrCaseAlreadyArchived rather than silently succeeding, so a caller can
// distinguish "already done" from "now done". BulkArchiveCases is what treats
// that as a skip.
func (uc *CaseUseCase) ArchiveCase(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	existing, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	if status := existing.Status.Normalize(); status != types.CaseStatusClosed {
		return nil, goerr.Wrap(ErrCaseNotClosed, "only a closed case can be archived",
			goerr.V(CaseIDKey, id), goerr.V("status", string(status)))
	}
	if existing.IsArchived() {
		return nil, goerr.Wrap(ErrCaseAlreadyArchived, "case is already archived", goerr.V(CaseIDKey, id))
	}

	now := time.Now().UTC()
	existing.ArchivedAt = &now
	existing.UpdatedAt = now

	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to archive case", goerr.V(CaseIDKey, id))
	}

	uc.postThreadContextLine(ctx, updated, i18n.T(ctx, i18n.MsgCaseChangeArchived, uc.changeActor(ctx)))
	return updated, nil
}

// UnarchiveCase restores a previously archived case.
//
// Unlike ArchiveCase this checks no lifecycle status. An archived case is
// CLOSED by construction, and if a document ever violated that invariant (see
// the archived_case_not_closed consistency check), making it visible again is
// the correct repair rather than a further obstacle.
func (uc *CaseUseCase) UnarchiveCase(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	existing, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id)
	if err != nil {
		return nil, err
	}

	if !existing.IsArchived() {
		return nil, goerr.Wrap(ErrCaseNotArchived, "case is not archived", goerr.V(CaseIDKey, id))
	}

	existing.ArchivedAt = nil
	existing.UpdatedAt = time.Now().UTC()

	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to unarchive case", goerr.V(CaseIDKey, id))
	}

	uc.postThreadContextLine(ctx, updated, i18n.T(ctx, i18n.MsgCaseChangeUnarchived, uc.changeActor(ctx)))
	return updated, nil
}

// changeActor renders the acting user for a case-change thread notification:
// a Slack mention when the context carries an auth token, the localized
// "system" label otherwise. Matches what UpdateCase / UpdateCaseStatus do.
func (uc *CaseUseCase) changeActor(ctx context.Context) string {
	if tok, err := auth.TokenFromContext(ctx); err == nil && tok.Sub != "" {
		return mentionUser(tok.Sub)
	}
	return i18n.T(ctx, i18n.MsgChangeActorSystem)
}

// BulkArchiveCases archives the given cases by delegating each id to
// ArchiveCase, so bulk and single archive share the exact same path (access
// control, status check, persistence, Slack notification). The archive body
// lives only in ArchiveCase and is never duplicated here.
//
// Staleness is skipped rather than failing the batch, because the selection is
// made against a snapshot of the list and the rows can move between the click
// and the write: an id already archived by someone else, an id no longer
// CLOSED because someone reopened it, and an id whose case was deleted. See
// isStaleForBulkArchive. Every other error — access denied, a persistence
// failure — is propagated, because a bulk operation must not silently swallow
// a real failure. The returned slice contains only the cases that were newly
// archived.
func (uc *CaseUseCase) BulkArchiveCases(ctx context.Context, workspaceID string, ids []int64) ([]*model.Case, error) {
	archived := make([]*model.Case, 0, len(ids))
	for _, id := range ids {
		updated, err := uc.ArchiveCase(ctx, workspaceID, id)
		if err != nil {
			if isStaleForBulkArchive(err) {
				continue
			}
			return nil, goerr.Wrap(err, "failed to bulk archive case", goerr.V(CaseIDKey, id))
		}
		archived = append(archived, updated)
	}
	return archived, nil
}

// isStaleForBulkArchive reports whether err means the selected row no longer
// describes the case as it is now, rather than that the operation failed.
//
// ErrCaseNotFound is in this set for the same reason as the other two: a row
// the user selected and someone else deleted in between is the list going
// stale, not a fault to abort the remaining ids for. Aborting there would
// leave the rest of a large batch unprocessed while the resolver has already
// reported every id as accepted.
//
// ErrAccessDenied is deliberately NOT in this set. It is the one outcome an
// operator needs to see, and treating it as routine would let a permission
// problem pass unnoticed through a bulk call.
func isStaleForBulkArchive(err error) bool {
	return errors.Is(err, ErrCaseAlreadyArchived) ||
		errors.Is(err, ErrCaseNotClosed) ||
		errors.Is(err, ErrCaseNotFound)
}

// BulkUnarchiveCases restores the given cases, mirroring BulkArchiveCases.
// Ids that are not archived, and ids whose case was deleted in the meantime,
// are skipped for the same staleness reason; every other error is propagated.
func (uc *CaseUseCase) BulkUnarchiveCases(ctx context.Context, workspaceID string, ids []int64) ([]*model.Case, error) {
	restored := make([]*model.Case, 0, len(ids))
	for _, id := range ids {
		updated, err := uc.UnarchiveCase(ctx, workspaceID, id)
		if err != nil {
			if errors.Is(err, ErrCaseNotArchived) || errors.Is(err, ErrCaseNotFound) {
				continue
			}
			return nil, goerr.Wrap(err, "failed to bulk unarchive case", goerr.V(CaseIDKey, id))
		}
		restored = append(restored, updated)
	}
	return restored, nil
}

// BulkArchiveCasesAsync runs BulkArchiveCases in the background via
// async.Dispatch so the operation outlives the request that triggered it.
// Clearing a Closed tab issues one archive (DB write + Slack post) per case;
// on a workspace with hundreds of closed cases that can take longer than the
// client is willing to wait, and a mid-flight disconnect would otherwise
// cancel the request context and leave the list half-archived. async.Dispatch
// hands the work a context with the auth token / logger intact but the
// cancellation severed. Per-case failures surface through async.Dispatch's
// error reporting rather than to the caller, which has already returned.
func (uc *CaseUseCase) BulkArchiveCasesAsync(ctx context.Context, workspaceID string, ids []int64) {
	async.Dispatch(ctx, func(bgCtx context.Context) error {
		_, err := uc.BulkArchiveCases(bgCtx, workspaceID, ids)
		return err
	})
}

// BulkUnarchiveCasesAsync mirrors BulkArchiveCasesAsync for restore.
func (uc *CaseUseCase) BulkUnarchiveCasesAsync(ctx context.Context, workspaceID string, ids []int64) {
	async.Dispatch(ctx, func(bgCtx context.Context) error {
		_, err := uc.BulkUnarchiveCases(bgCtx, workspaceID, ids)
		return err
	})
}

// caseStatusSetForWorkspace returns the configurable Case status set for the
// workspace (thread mode), or nil when the workspace is channel mode / has no
// case status set.
func (uc *CaseUseCase) caseStatusSetForWorkspace(workspaceID string) *model.ActionStatusSet {
	if uc.workspaceRegistry == nil {
		return nil
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return nil
	}
	return entry.CaseStatusSet
}

// workspaceIsThreadMode reports whether the workspace binds cases to Slack
// threads (thread mode) rather than dedicated channels (channel mode).
//
// A missing registry (uc.workspaceRegistry == nil) resolves to (false, nil):
// with no registry configured there are no thread-mode workspaces, and many
// channel-only call sites (and tests) run without one. A registry lookup
// FAILURE, however, is propagated rather than swallowed — treating "could not
// determine the mode" as "channel mode" would let the private-case invariant
// fail open for an unknown / misconfigured workspace ID.
func (uc *CaseUseCase) workspaceIsThreadMode(workspaceID string) (bool, error) {
	if uc.workspaceRegistry == nil {
		return false, nil
	}
	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		return false, goerr.Wrap(err, "failed to resolve workspace for thread-mode check",
			goerr.V("workspace_id", workspaceID))
	}
	return entry.IsThreadMode(), nil
}

// createThreadBoundCase creates a thread-mode Case bound to a monitored-channel
// thread (channelID, threadTS), with the supplied title / description / custom
// fields, running FULL validation before persisting. All field violations are
// aggregated (not fail-fast) so the caller — the thread-mode create agent — can
// be told everything wrong in one shot. It is idempotent: a re-delivered Slack
// message mapping to an existing thread returns the existing Case unchanged.
// CaseLifecycleCreated is published so Jobs fire exactly as in channel mode.
//
// requestKey is stored on the case for idempotency (empty for the reaction /
// Slack-thread creation paths, which dedup by their existing message ts). Web /
// slash / mention creation passes it so a redelivered submission can be deduped
// via GetByRequestKey before a second thread root is posted.
func (uc *CaseUseCase) createThreadBoundCase(ctx context.Context, workspaceID, channelID, threadTS, reporterID, title, description string, fieldValues map[string]model.FieldValue, requestKey string) (*model.Case, error) {
	if channelID == "" || threadTS == "" {
		return nil, goerr.New("channelID and threadTS are required for thread case")
	}

	existing, err := uc.repo.Case().GetBySlackThread(ctx, workspaceID, channelID, threadTS)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to check existing thread case",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
	}
	if existing != nil {
		return existing, nil
	}

	enriched, vErr := uc.validateCaseWrite(ctx, workspaceID, validateAll, fieldValues, nil)
	if vErr != nil {
		return nil, goerr.Wrap(vErr, "thread case field validation failed",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
	}
	fieldValues = enriched

	now := time.Now().UTC()
	c := &model.Case{
		Title:       title,
		Description: description,
		Status:      types.CaseStatusOpen,
		ReporterID:  reporterID,
		FieldValues: fieldValues,
		RequestKey:  requestKey,
		CreatedAt:   now,
	}
	// Apply the monitored-channel thread binding (channel, thread ts, initial
	// board status, UpdatedAt) via the shared helper so the create and the
	// draft-promotion paths bind identically.
	uc.applyThreadBinding(c, workspaceID, channelID, threadTS, now)
	created, err := uc.repo.Case().Create(ctx, workspaceID, c)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to create thread case with fields",
			goerr.V("channel_id", channelID), goerr.V("thread_ts", threadTS))
	}

	uc.publishLifecycle(ctx, workspaceID, created, model.CaseLifecycleCreated)
	return created, nil
}

// MaterializeThreadCase applies the LLM-materialized title / description /
// custom field values onto a thread-mode Case. Empty title / description are
// ignored (the placeholder set at creation is kept). Field values are
// type-checked via the workspace validator before write.
func (uc *CaseUseCase) MaterializeThreadCase(ctx context.Context, workspaceID string, id int64, title, description string, fieldValues map[string]model.FieldValue) (*model.Case, error) {
	existing, err := uc.repo.Case().Get(ctx, workspaceID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, id))
	}

	if title != "" {
		existing.Title = title
	}
	if description != "" {
		existing.Description = description
	}
	if len(fieldValues) > 0 {
		validated, vErr := uc.validateCaseWrite(ctx, workspaceID, validatePartialStrict, fieldValues, nil)
		if vErr != nil {
			return nil, goerr.Wrap(vErr, "thread case field validation failed", goerr.V(CaseIDKey, id))
		}
		existing.FieldValues = mergeFieldValues(existing.FieldValues, validated)
	}

	existing.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to materialize thread case", goerr.V(CaseIDKey, id))
	}
	return updated, nil
}

// UpdateCaseStatus sets the configurable board status of a thread-mode Case
// and synchronises the lifecycle Status (a closed board status closes the
// case). It is the single entry point for both the Kanban drag-and-drop and
// the agent's `close` decision; CaseLifecycleClosed is published only on the
// open→closed edge so Jobs fire once.
//
// The second return value is the board status the case held immediately before
// the write. It is computed inside the transaction against the very state the
// write is applied to, so it describes exactly what was persisted — which is
// what lets a caller tell a real move from a re-assignment of the status the
// case already had, a distinction the returned *model.Case cannot carry since
// it holds only the post-write value.
func (uc *CaseUseCase) UpdateCaseStatus(ctx context.Context, workspaceID string, id int64, boardStatus string) (*model.Case, string, error) {
	ctx = i18n.ContextWithWorkspaceLang(ctx, uc.workspaceRegistry.Language(workspaceID))
	set := uc.caseStatusSetForWorkspace(workspaceID)
	if set == nil {
		return nil, "", goerr.New("workspace has no case status set (not thread mode)",
			goerr.V("workspace_id", workspaceID))
	}
	if !set.IsValid(boardStatus) {
		return nil, "", goerr.New("invalid board status id",
			goerr.V("workspace_id", workspaceID), goerr.V("board_status", boardStatus))
	}

	// Fail fast through the shared read gate, the same way AssignCase does: it is
	// what turns a missing case into ErrCaseNotFound and a private case into
	// ErrAccessDenied, which callers (GraphQL error mapping, agent tools)
	// discriminate on. The transaction below re-checks access on the state it
	// actually writes, so this read is never the basis for anything persisted or
	// announced.
	if _, err := loadCaseForWrite(ctx, uc.repo, workspaceID, id); err != nil {
		return nil, "", err
	}

	actorID, checkAccess := tokenActor(ctx)
	var beforeStatus string
	var wasClosed bool
	updated, err := uc.repo.Case().Transact(ctx, workspaceID, id, func(c *model.Case) error {
		// Reset: Firestore may re-run this closure, and leftover values from a
		// previous attempt would announce a transition this attempt did not make.
		beforeStatus = ""
		wasClosed = false
		if err := assertCaseWriteAccess(c, actorID, checkAccess); err != nil {
			return err
		}
		// The guard is on "is archived", not on "is the target column open": an
		// archived case must not take board writes at all, so moving it between
		// closed columns is rejected too. Unarchive it first.
		if c.IsArchived() {
			return goerr.Wrap(ErrCaseArchived, "archived case cannot change board status; unarchive it first", goerr.V(CaseIDKey, id))
		}

		beforeStatus = c.BoardStatus
		wasClosed = c.Status.Normalize() == types.CaseStatusClosed
		beforeLifecycle := c.Status
		c.BoardStatus = boardStatus
		c.SyncLifecycleFromBoardStatus(set)

		// UpdatedAt moves only when this write actually changed the case.
		// Re-assigning the status a case already holds must leave it looking
		// untouched: the home dashboard derives its "stalled" flag and two
		// orderings from this field, so a scheduled Job re-setting the same status
		// every morning would otherwise keep the case looking freshly worked on
		// forever. The lifecycle half of the condition covers the repair case — a
		// case whose board status is right but whose lifecycle status drifted
		// (what validate --check-db reports as lifecycle_mismatch) is genuinely
		// changed by this write, so it is stamped.
		if beforeStatus != c.BoardStatus || beforeLifecycle != c.Status {
			c.UpdatedAt = time.Now().UTC()
		}
		return nil
	})
	if err != nil {
		return nil, "", goerr.Wrap(err, "failed to update case status", goerr.V(CaseIDKey, id))
	}

	if !wasClosed && updated.Status.Normalize() == types.CaseStatusClosed {
		uc.publishLifecycle(ctx, workspaceID, updated, model.CaseLifecycleClosed)
	}

	// beforeStatus was captured inside the transaction against the very state
	// that was written, so a concurrent move of the same case cannot make this
	// announce a transition it did not make.
	if beforeStatus != updated.BoardStatus {
		actor := i18n.T(ctx, i18n.MsgChangeActorSystem)
		if tok, terr := auth.TokenFromContext(ctx); terr == nil && tok.Sub != "" {
			actor = mentionUser(tok.Sub)
		}
		// Report the configured display name, not the raw board status id. An id
		// the set no longer defines (left over from a config change) falls back to
		// the id itself, and an unset board status to an em dash.
		label := func(statusID string) string {
			if def, ok := set.Get(statusID); ok {
				return def.Name
			}
			return orDash(statusID)
		}
		uc.postThreadContextLine(ctx, updated,
			i18n.T(ctx, i18n.MsgCaseChangeStatus, actor, label(beforeStatus), label(updated.BoardStatus)))
	}

	return updated, beforeStatus, nil
}

// ListDrafts returns every draft case in the workspace. Drafts are
// workspace-wide so any team member can pick one up; private drafts are
// the exception and remain visible only to their reporter (a draft has
// no Slack channel yet, so the usual IsCaseAccessible check via
// ChannelUserIDs would lock everyone out — we use ReporterID instead).
func (uc *CaseUseCase) ListDrafts(ctx context.Context, workspaceID string) ([]*model.Case, error) {
	drafts, err := uc.repo.Case().ListDrafts(ctx, workspaceID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list drafts")
	}

	// Apply private-draft access control. Callers without an auth token
	// (bots / system contexts) only see public drafts.
	var requesterID string
	if token, tokenErr := auth.TokenFromContext(ctx); tokenErr == nil {
		requesterID = token.Sub
	}

	visible := make([]*model.Case, 0, len(drafts))
	for _, d := range drafts {
		if d.IsPrivate && d.ReporterID != requesterID {
			continue
		}
		visible = append(visible, d)
	}

	// Match ListCases: newest-first, repository-order-independent.
	sort.SliceStable(visible, func(i, j int) bool {
		return visible[i].CreatedAt.After(visible[j].CreatedAt)
	})
	return visible, nil
}

// GetDraft returns a single draft case. Public drafts are visible
// workspace-wide so any team member can preview (and act on) an
// in-progress entry; private drafts remain reporter-only (the usual
// ChannelUserIDs check can't help yet — the draft has no Slack channel).
// Non-draft cases return ErrCaseNotDraft so callers cannot reuse the
// draft resolver to peek at submitted cases.
//
// Mutating actions (SubmitDraft, DiscardDraft) reach the draft through
// this method, so private-draft access control automatically extends to
// them: a non-reporter cannot even discover a private draft, let alone
// modify it. Public drafts are deliberately open — the team owns them.
func (uc *CaseUseCase) GetDraft(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	c, err := uc.repo.Case().Get(ctx, workspaceID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "draft not found", goerr.V(CaseIDKey, id))
	}
	if !c.IsDraft() {
		return nil, goerr.Wrap(ErrCaseNotDraft, "case is not a draft", goerr.V(CaseIDKey, id))
	}

	if c.IsPrivate {
		token, tokenErr := auth.TokenFromContext(ctx)
		if tokenErr != nil || c.ReporterID != token.Sub {
			return nil, goerr.Wrap(ErrCaseNotFound, "draft not found", goerr.V(CaseIDKey, id))
		}
	}
	return c, nil
}

// SubmitDraft promotes a draft case to OPEN and triggers the same activation
// side effects (Slack channel, invites, welcome, etc.) as a fresh CreateCase.
// The optional `patch` carries last-minute edits the caller wants to apply
// atomically before the promotion — passing them to this single usecase
// method (rather than separate UpdateCase + SubmitDraft calls from the
// controller) keeps the "save final edits and submit" business operation
// atomic: required-field validation, channel creation, and invites all see
// the same set of values, and a failure path leaves the draft consistent.
//
// If activation fails the draft is kept in DRAFT so the user can retry
// without losing the saved entry.
func (uc *CaseUseCase) SubmitDraft(ctx context.Context, workspaceID string, id int64, patch *CaseUpdate) (*model.Case, error) {
	c, err := uc.GetDraft(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}

	// Apply pre-submit edits in memory and persist them before strict
	// validation runs. We route through repo.Case().Update directly rather
	// than UpdateCase() so the activation path further down sees the
	// freshly-stored case (and so private-draft / draft-empty-title quirks
	// stay scoped to SubmitDraft instead of leaking into the generic
	// UpdateCase contract).
	if patch != nil {
		if patch.Title != nil {
			c.Title = *patch.Title
		}
		if patch.Description != nil {
			c.Description = *patch.Description
		}
		if patch.IsTest != nil {
			c.IsTest = *patch.IsTest
		}
		// Assignees are not part of the submit-draft patch; they are managed
		// via AssignCase / UnassignCase on the draft before promotion.
		if patch.Fields != nil {
			validated := patch.Fields
			// Drafts keep the lenient partial validation (preserve unknown ids,
			// no required check) and the ErrFieldValidationFailed wrapping so
			// the GraphQL FIELD_VALIDATION_FAILED code is preserved on the
			// pre-submit edit path. The strict / required enforcement happens
			// below, just before the draft is promoted to OPEN.
			if validator := uc.fieldValidatorForWorkspace(workspaceID); validator != nil {
				enriched, vErr := validator.ValidateCaseFieldsPartial(validated)
				if vErr != nil {
					return nil, goerr.Wrap(ErrFieldValidationFailed, vErr.Error(), goerr.V(CaseIDKey, id))
				}
				validated = enriched
			}
			c.FieldValues = mergeFieldValues(c.FieldValues, validated)
		}
		// Verify every referenced user exists (assignees + user-field values),
		// consistent with every other case write.
		if err := uc.verifyUsersExist(ctx, c.AssigneeIDs, c.FieldValues); err != nil {
			return nil, goerr.Wrap(err, "case write validation failed", goerr.V(CaseIDKey, id))
		}
		c.UpdatedAt = time.Now().UTC()
		persistedPatch, pErr := uc.repo.Case().Update(ctx, workspaceID, c)
		if pErr != nil {
			return nil, goerr.Wrap(pErr, "failed to persist pre-submit edits", goerr.V(CaseIDKey, id))
		}
		c = persistedPatch
	}

	// Drafts cannot be Submitted with an empty title — Slack channel naming
	// and listing both need at least a few chars. The Save as Draft path
	// allowed empty titles for partial entries; require one on Submit.
	if c.Title == "" {
		return nil, goerr.Wrap(ErrDraftTitleRequired,
			"draft title is required before submit",
			goerr.V(CaseIDKey, id))
	}

	// Re-run strict field validation now that the draft is being promoted
	// to OPEN — Save as Draft skipped the required-field check, so this is
	// the first time the workspace's full schema is enforced. Bail out
	// before flipping the status so the user can finish filling required
	// fields on the draft entry and resubmit. We collect *every* missing
	// required field so the UI can list them in one message instead of
	// surfacing them one redirect at a time.
	if schema := uc.fieldSchemaForWorkspace(workspaceID); schema != nil {
		var missingNames []string
		var missingIDs []string
		for _, fd := range schema.Fields {
			if !fd.Required {
				continue
			}
			if _, ok := c.FieldValues[fd.ID]; ok {
				continue
			}
			missingIDs = append(missingIDs, fd.ID)
			name := fd.Name
			if name == "" {
				name = fd.ID
			}
			missingNames = append(missingNames, name)
		}
		if len(missingNames) > 0 {
			return nil, goerr.Wrap(ErrMissingRequiredOnSubmit,
				fmt.Sprintf("required field(s) not filled: %s", strings.Join(missingNames, ", ")),
				goerr.V(CaseIDKey, id),
				goerr.V(MissingFieldIDsKey, missingIDs),
				goerr.V(MissingFieldNamesKey, missingNames),
			)
		}
	}

	if err := c.SubmitDraft(); err != nil {
		return nil, goerr.Wrap(err, "cannot submit draft",
			goerr.V(CaseIDKey, id),
			goerr.V(CurrentStatusKey, string(c.Status)),
		)
	}

	c.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Case().Update(ctx, workspaceID, c)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to flip draft to open", goerr.V(CaseIDKey, id))
	}

	// Snapshot the pre-activation Slack binding so a rollback restores exactly
	// what the draft had, instead of blindly clearing (which would drop a
	// pre-existing board status). The thread producer mutates `updated` in place
	// via applyThreadBinding, so this must be captured before the funnel runs.
	preChannelID, preThreadTS, preBoardStatus := updated.SlackChannelID, updated.SlackThreadTS, updated.BoardStatus

	// Bring the promoted case to OPEN through the same mode funnel CreateCase
	// uses: channel mode creates a dedicated channel; thread mode binds the case
	// to a freshly-posted monitored-channel thread and never creates a channel.
	activated, actErr := uc.openInWorkspaceMode(ctx, workspaceID, updated.IsPrivate, openProducers{
		Channel: func(ctx context.Context, entry *model.WorkspaceEntry) (*model.Case, error) {
			return uc.activateChannelModeCase(ctx, workspaceID, entry, updated, "", "")
		},
		Thread: func(ctx context.Context, entry *model.WorkspaceEntry) (*model.Case, error) {
			rootTS, err := postMonitoredThreadAnchor(ctx, uc.slackService, entry)
			if err != nil {
				return nil, err
			}
			uc.applyThreadBinding(updated, workspaceID, entry.SlackMonitorChannelID, rootTS, time.Now().UTC())
			saved, err := uc.repo.Case().Update(ctx, workspaceID, updated)
			if err != nil {
				failMonitoredThreadAnchor(ctx, uc.slackService, entry, rootTS)
				return nil, goerr.Wrap(err, "failed to bind draft to monitored thread", goerr.V(CaseIDKey, id))
			}
			replaceRootWithCaseSummary(ctx, uc.slackService, entry, saved, entry.SlackMonitorChannelID, rootTS, uc.CaseURL(workspaceID, saved.ID))
			return saved, nil
		},
	})
	if actErr != nil {
		// SubmitDraft's rollback policy is "preserve the saved work": restore the
		// draft to its pre-activation snapshot (DRAFT status, and the exact Slack
		// binding / board status it had before promotion) and keep the row so the
		// user can retry. Restoring the snapshot rather than clearing matters on
		// the thread path: a repo Update that persisted the binding server-side but
		// returned an error to us would otherwise leave a thread-bound DRAFT.
		if rolled, getErr := uc.repo.Case().Get(ctx, workspaceID, id); getErr == nil {
			rolled.Status = types.CaseStatusDraft
			rolled.SlackChannelID = preChannelID
			rolled.SlackThreadTS = preThreadTS
			rolled.BoardStatus = preBoardStatus
			rolled.UpdatedAt = time.Now().UTC()
			if _, undoErr := uc.repo.Case().Update(ctx, workspaceID, rolled); undoErr != nil {
				errutil.Handle(ctx, goerr.Wrap(undoErr, "failed to roll status back to draft after activation failure",
					goerr.V(CaseIDKey, id),
				), "failed to roll status back to draft after activation failure")
			}
		} else {
			errutil.Handle(ctx, goerr.Wrap(getErr, "draft case missing during rollback",
				goerr.V(CaseIDKey, id),
			), "draft case missing during rollback")
		}
		// errors.Join keeps both the ErrActivationFailed marker (existing callers /
		// GraphQL rely on it) and the underlying sentinel (e.g.
		// ErrCasePrivateThreadModeUnsupported) reachable via errors.Is.
		return nil, goerr.Wrap(errors.Join(ErrActivationFailed, actErr), "draft activation failed", goerr.V(CaseIDKey, id))
	}

	// A DRAFT-promoted-to-OPEN case is the first time the entity is
	// "real" — fire the created lifecycle event so Jobs that listen for
	// new cases run uniformly whether they came from CreateCase or
	// SubmitDraft.
	uc.publishLifecycle(ctx, workspaceID, activated, model.CaseLifecycleCreated)
	return activated, nil
}

// DiscardDraft permanently deletes a draft. Public drafts are team-wide
// shared so any workspace member may discard one; private drafts are
// hidden from non-reporters at the GetDraft layer, which naturally keeps
// them owner-only. Non-draft cases are rejected so callers cannot pivot
// this method into a "delete any case" shortcut.
func (uc *CaseUseCase) DiscardDraft(ctx context.Context, workspaceID string, id int64) error {
	c, err := uc.GetDraft(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	if err := uc.repo.Case().Delete(ctx, workspaceID, c.ID); err != nil {
		return goerr.Wrap(err, "failed to discard draft", goerr.V(CaseIDKey, id))
	}
	return nil
}

// draftURL returns the web-UI URL for a specific draft, or an empty
// string when no baseURL has been configured. The URL format mirrors
// what the React app's router expects for the draft detail page.
func (uc *CaseUseCase) draftURL(workspaceID string, caseID int64) string {
	if uc.baseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/ws/%s/drafts/%d", uc.baseURL, workspaceID, caseID)
}

// CaseURL returns the web-UI URL for a specific case detail page, or an empty
// string when no baseURL has been configured.
func (uc *CaseUseCase) CaseURL(workspaceID string, caseID int64) string {
	if uc.baseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/ws/%s/cases/%d", uc.baseURL, workspaceID, caseID)
}

// SyncCaseChannelUsers synchronizes channel members from Slack API to the case
func (uc *CaseUseCase) SyncCaseChannelUsers(ctx context.Context, workspaceID string, caseID int64) (*model.Case, error) {
	existing, err := loadCaseForWrite(ctx, uc.repo, workspaceID, caseID)
	if err != nil {
		return nil, err
	}

	if existing.SlackChannelID == "" {
		return nil, goerr.New("case has no Slack channel", goerr.V(CaseIDKey, caseID))
	}

	if uc.slackService == nil {
		return nil, goerr.New("Slack service is not available")
	}

	members, err := uc.slackService.GetConversationMembers(ctx, existing.SlackChannelID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get channel members",
			goerr.V(CaseIDKey, caseID),
			goerr.V("channel_id", existing.SlackChannelID))
	}

	existing.ChannelUserIDs = filterHumanUsers(ctx, uc.repo, members)
	existing.UpdatedAt = time.Now().UTC()
	updated, err := uc.repo.Case().Update(ctx, workspaceID, existing)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update case with channel members",
			goerr.V(CaseIDKey, caseID))
	}

	return updated, nil
}

// resolveAutoInviteUsers resolves auto-invite users from workspace config.
// It collects direct user IDs and resolves user group members.
// Errors during group resolution are logged but do not stop the process.
func (uc *CaseUseCase) resolveAutoInviteUsers(ctx context.Context, workspaceID string) []string {
	if uc.workspaceRegistry == nil || uc.slackService == nil {
		return nil
	}

	entry, err := uc.workspaceRegistry.Get(workspaceID)
	if err != nil {
		errutil.Handle(ctx, err, "failed to get workspace entry for auto-invite")
		return nil
	}

	if len(entry.SlackInviteUsers) == 0 && len(entry.SlackInviteGroups) == 0 {
		return nil
	}

	users := make([]string, 0, len(entry.SlackInviteUsers))
	users = append(users, entry.SlackInviteUsers...)

	// Resolve group members
	if len(entry.SlackInviteGroups) > 0 {
		groupMembers := uc.resolveGroupMembers(ctx, entry.SlackInviteGroups, entry.SlackTeamID)
		users = append(users, groupMembers...)
	}

	return users
}

// resolveGroupMembers resolves user group identifiers (IDs or handle names) to member user IDs.
// Handle names are prefixed with "@" (e.g., "@security-team"); everything else is treated as a group ID.
// teamID is passed to ListUserGroups for org-level app support (empty string for WS-level apps).
func (uc *CaseUseCase) resolveGroupMembers(ctx context.Context, groups []string, teamID string) []string {
	var groupIDs []string
	var handleNames []string

	for _, g := range groups {
		if handle, ok := strings.CutPrefix(g, "@"); ok {
			handleNames = append(handleNames, handle)
		} else {
			groupIDs = append(groupIDs, g)
		}
	}

	// Resolve handle names to group IDs via full group list
	if len(handleNames) > 0 {
		allGroups, err := uc.slackService.ListUserGroups(ctx, teamID)
		if err != nil {
			errutil.Handle(ctx, err, "failed to list user groups for handle resolution")
		} else {
			handleToID := make(map[string]string, len(allGroups))
			for _, g := range allGroups {
				if g.Handle != "" {
					handleToID[g.Handle] = g.ID
				}
			}
			for _, handle := range handleNames {
				if id, ok := handleToID[handle]; ok {
					groupIDs = append(groupIDs, id)
				} else {
					// Unknown handle: usually a workspace config typo or a
					// handle that was renamed/deleted in Slack. Surface so
					// the operator can fix the configuration.
					errutil.Handle(ctx, goerr.New("user group handle not found", goerr.V("handle", handle)), "user group handle not found")
				}
			}
		}
	}

	// Resolve group IDs to member user IDs
	groupIDs = uniqueStrings(groupIDs)
	var members []string
	for _, gid := range groupIDs {
		m, err := uc.slackService.GetUserGroupMembers(ctx, gid)
		if err != nil {
			errutil.Handle(ctx, err, "failed to get user group members")
			continue
		}
		members = append(members, m...)
	}

	return members
}

// filterHumanUsers filters out bot/unknown user IDs by checking against the SlackUser DB cache.
// Only IDs that exist in the cache (i.e., real human users synced via ListUsers) are returned.
// This avoids additional Slack API calls since ListUsers already excludes bots.
func filterHumanUsers(ctx context.Context, repo interfaces.Repository, userIDs []string) []string {
	if len(userIDs) == 0 {
		return userIDs
	}

	slackUserIDs := make([]model.SlackUserID, len(userIDs))
	for i, id := range userIDs {
		slackUserIDs[i] = model.SlackUserID(id)
	}

	known, err := repo.SlackUser().GetByIDs(ctx, slackUserIDs)
	if err != nil {
		// On error, return all IDs to avoid data loss; report so the
		// degraded mode is visible.
		errutil.Handle(ctx, goerr.Wrap(err, "failed to get slack users for bot filtering, returning all IDs",
			goerr.V("userIDs", userIDs),
		), "failed to get slack users for bot filtering")
		return userIDs
	}

	filtered := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		if _, ok := known[model.SlackUserID(id)]; ok {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

// uniqueStrings removes duplicate strings while preserving order
func uniqueStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	result := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			result = append(result, v)
		}
	}
	return result
}

func (uc *CaseUseCase) GetFieldConfiguration(workspaceID string) *config.FieldSchema {
	if uc.workspaceRegistry != nil {
		entry, err := uc.workspaceRegistry.Get(workspaceID)
		if err == nil && entry.FieldSchema != nil {
			return entry.FieldSchema
		}
	}
	return &config.FieldSchema{
		Fields: []config.FieldDefinition{},
		Labels: config.EntityLabels{
			Case: "Case",
		},
	}
}

// GetActionStatusSet returns the resolved ActionStatusSet for the workspace,
// falling back to the legacy default when the workspace is unknown or has no
// custom configuration. This is the canonical accessor for any layer that
// needs to render or validate action statuses outside ActionUseCase.
func (uc *CaseUseCase) GetActionStatusSet(workspaceID string) *model.ActionStatusSet {
	return resolveActionStatusSet(uc.workspaceRegistry, workspaceID)
}

// GetCaseStatusSet returns the configurable Case status set (the Kanban
// columns) for a thread-mode workspace, or nil for channel-mode workspaces.
func (uc *CaseUseCase) GetCaseStatusSet(workspaceID string) *model.ActionStatusSet {
	return uc.caseStatusSetForWorkspace(workspaceID)
}

// postThreadContextLine posts body as a single context-block reply in the
// Case's Slack thread. Thread-mode only: a channel-mode Case owns a dedicated
// channel and has no thread to reply into, and its title change already renames
// that channel. No PostThreadOption is passed, so the reply stays inside the
// thread — reply_broadcast would push it into the monitored channel that every
// other thread-mode Case shares.
//
// Best-effort: a Slack failure is reported through errutil.Handle and never
// rolls back the write that produced the change.
func (uc *CaseUseCase) postThreadContextLine(ctx context.Context, c *model.Case, body string) {
	if uc.slackService == nil || c == nil || body == "" {
		return
	}
	if !c.IsThreadBound() || c.SlackChannelID == "" {
		return
	}

	blocks := []goslack.Block{
		goslack.NewContextBlock("", goslack.NewTextBlockObject(goslack.MarkdownType, body, false, false)),
	}
	if _, err := uc.slackService.PostThreadMessage(ctx, c.SlackChannelID, c.SlackThreadTS, blocks, body); err != nil {
		errutil.Handle(ctx, err, "failed to post case change notification")
	}
}
