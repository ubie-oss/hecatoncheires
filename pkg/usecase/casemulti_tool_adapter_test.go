package usecase_test

import (
	"context"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casemulti"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

func TestNewCaseMultiCaseAdapter_NilUseCase(t *testing.T) {
	gt.Value(t, usecase.NewCaseMultiCaseAdapter(nil)).Nil()
}

// TestCaseMultiCaseAdapter_CreateCase drives the adapter's CreateCase through
// a real CaseUseCase backed by memory.New() and reads the persisted Case back
// via the repository, verifying the fields the casemulti tool sets survive
// the round trip.
func TestCaseMultiCaseAdapter_CreateCase(t *testing.T) {
	repo := memory.New()
	caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	adapter := usecase.NewCaseMultiCaseAdapter(caseUC)
	gt.Value(t, adapter).NotNil().Required()

	// The workspace-agent host establishes the mentioning user as the ctx
	// auth token before any casemulti call (see wsagent.RunTurn); mirror that
	// here so CreateCase's reporter-from-auth-context logic behaves as it
	// does in production.
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CREATOR"})

	created, err := adapter.CreateCase(ctx, testWorkspaceID, "Cross-case title", "Cross-case description", nil, nil, true)
	gt.NoError(t, err).Required()

	stored, err := repo.Case().Get(ctx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.Title).Equal("Cross-case title")
	gt.Value(t, stored.Description).Equal("Cross-case description")
	gt.Bool(t, stored.IsPrivate).True()
	gt.Value(t, stored.ReporterID).Equal("U-CREATOR")
}

// TestCaseMultiCaseAdapter_AssignCase drives the adapter's delta assignee
// operations through a real CaseUseCase backed by memory.New() and reads the
// persisted Case back, so a regression in the adapter wiring (e.g. swapping
// AssignCase and UnassignCase, or dropping the user ids) is caught.
func TestCaseMultiCaseAdapter_AssignCase(t *testing.T) {
	repo := memory.New()
	caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	adapter := usecase.NewCaseMultiCaseAdapter(caseUC)
	gt.Value(t, adapter).NotNil().Required()

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CREATOR"})
	seedSlackUsers(t, repo, "U-CREATOR", "U-ONE", "U-TWO")

	created, err := caseUC.CreateCase(ctx, testWorkspaceID, "Assignable", "", []string{"U-ONE"}, nil, false, false, "", "")
	gt.NoError(t, err).Required()

	// Adding is a delta union: the pre-existing assignee survives.
	assigned, err := adapter.AssignCase(ctx, testWorkspaceID, created.ID, []string{"U-TWO"})
	gt.NoError(t, err).Required()
	gt.Value(t, assigned.AssigneeIDs).Equal([]string{"U-ONE", "U-TWO"})

	stored, err := repo.Case().Get(ctx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.AssigneeIDs).Equal([]string{"U-ONE", "U-TWO"})

	// Removing is a delta difference: only the named id goes.
	unassigned, err := adapter.UnassignCase(ctx, testWorkspaceID, created.ID, []string{"U-ONE"})
	gt.NoError(t, err).Required()
	gt.Value(t, unassigned.AssigneeIDs).Equal([]string{"U-TWO"})

	stored, err = repo.Case().Get(ctx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.AssigneeIDs).Equal([]string{"U-TWO"})
}

// The workspace agent runs every casemulti call under the mentioning user's
// auth token, so the adapter must inherit CaseUseCase's private-case gate
// rather than acting as a privileged bypass.
func TestCaseMultiCaseAdapter_AssignCase_PrivateCaseAccess(t *testing.T) {
	repo := memory.New()
	caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	adapter := usecase.NewCaseMultiCaseAdapter(caseUC)
	gt.Value(t, adapter).NotNil().Required()

	seedSlackUsers(t, repo, "U-MEMBER", "U-STRANGER", "U-TARGET")

	memberCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-MEMBER"})
	created, err := repo.Case().Create(memberCtx, testWorkspaceID, &model.Case{
		ReporterID:     "U-MEMBER",
		Title:          "Private",
		Status:         types.CaseStatusOpen,
		IsPrivate:      true,
		ChannelUserIDs: []string{"U-MEMBER"},
	})
	gt.NoError(t, err).Required()

	// A channel member may assign.
	assigned, err := adapter.AssignCase(memberCtx, testWorkspaceID, created.ID, []string{"U-TARGET"})
	gt.NoError(t, err).Required()
	gt.Value(t, assigned.AssigneeIDs).Equal([]string{"U-TARGET"})

	// A non-member is denied on both operations, and the case is unchanged.
	strangerCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-STRANGER"})
	_, err = adapter.AssignCase(strangerCtx, testWorkspaceID, created.ID, []string{"U-STRANGER"})
	gt.Error(t, err).Is(usecase.TestErrAccessDenied)
	_, err = adapter.UnassignCase(strangerCtx, testWorkspaceID, created.ID, []string{"U-TARGET"})
	gt.Error(t, err).Is(usecase.TestErrAccessDenied)

	stored, err := repo.Case().Get(memberCtx, testWorkspaceID, created.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.AssigneeIDs).Equal([]string{"U-TARGET"})
}

func TestNewCaseMultiActionAdapter_NilUseCase(t *testing.T) {
	repo := memory.New()
	actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
	stepUC := usecase.NewActionStepUseCase(repo, nil, nil, nil)

	gt.Value(t, usecase.NewCaseMultiActionAdapter(nil, nil)).Nil()
	gt.Value(t, usecase.NewCaseMultiActionAdapter(actionUC, nil)).Nil()
	gt.Value(t, usecase.NewCaseMultiActionAdapter(nil, stepUC)).Nil()
}

// casemultiActionFixture wires a real CaseUseCase / ActionUseCase /
// ActionStepUseCase against memory.New() and pre-creates one Case and one
// Action, so the casemulti action-adapter tests exercise real persistence
// rather than a hand-rolled double.
type casemultiActionFixture struct {
	repo     *memory.Memory
	actionUC *usecase.ActionUseCase
	stepUC   *usecase.ActionStepUseCase
	adapter  casemulti.ActionUsecase
	caseID   int64
	actionID int64
}

func newCasemultiActionFixture(t *testing.T) *casemultiActionFixture {
	t.Helper()

	repo := memory.New()
	caseUC := usecase.NewCaseUseCase(repo, nil, nil, nil, "")
	actionUC := usecase.NewActionUseCase(repo, nil, nil, "", nil)
	stepUC := usecase.NewActionStepUseCase(repo, nil, nil, nil)

	// Case creation needs a reporter (channel-mode Case), so the setup runs
	// under an authenticated ctx. The actual adapter calls under test use
	// their own explicit actorID argument instead.
	setupCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U-CREATOR"})
	c, err := caseUC.CreateCase(setupCtx, testWorkspaceID, "Cross-case action host", "", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()

	action, err := actionUC.CreateAction(setupCtx, testWorkspaceID, c.ID, "Original title", "original description", "", "", types.ActionStatusTodo, nil)
	gt.NoError(t, err).Required()

	adapter := usecase.NewCaseMultiActionAdapter(actionUC, stepUC)
	gt.Value(t, adapter).NotNil().Required()

	return &casemultiActionFixture{
		repo:     repo,
		actionUC: actionUC,
		stepUC:   stepUC,
		adapter:  adapter,
		caseID:   c.ID,
		actionID: action.ID,
	}
}

// TestCaseMultiActionAdapter_UpdateAction_AttributesToSlackUser verifies the
// adapter's documented contract (casemulti_tool_adapter.go): every
// workspace-agent write is attributed to the mentioning Slack user via
// ActorRef{Kind: ActorKindSlackUser, ID: actorID} — never ActorKindSystem.
// recordActionEvents (pkg/usecase/action.go) writes ActionEvent.ActorID from
// exactly that ActorRef, leaving it empty ("") only for ActorKindSystem, so a
// non-empty, matching ActorID on the persisted event is the observable proof
// of SlackUser attribution.
func TestCaseMultiActionAdapter_UpdateAction_AttributesToSlackUser(t *testing.T) {
	f := newCasemultiActionFixture(t)

	// Mirrors the real turn context (auth.ContextWithToken with the
	// mentioning user) plus the same id passed as actorID, exactly as
	// wsagent.RunTurn wires it.
	const actorID = "U-HUMAN"
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: actorID})

	newTitle := "Updated by workspace agent"
	newStatus := types.ActionStatusInProgress
	updated, err := f.adapter.UpdateAction(ctx, testWorkspaceID, f.actionID, casemulti.ActionUpdate{
		Title:  &newTitle,
		Status: &newStatus,
	}, actorID)
	gt.NoError(t, err).Required()
	gt.Value(t, updated.Title).Equal(newTitle)
	gt.Value(t, updated.Status).Equal(newStatus)

	// Persisted state reflects the change.
	stored, err := f.repo.Action().Get(ctx, testWorkspaceID, f.actionID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.Title).Equal(newTitle)
	gt.Value(t, stored.Status).Equal(newStatus)

	// The change history (ActionEvent) attributes both diffs to the Slack
	// user actorID — never the empty string ActorKindSystem would record.
	events, _, err := f.repo.ActionEvent().List(ctx, testWorkspaceID, f.actionID, 10, "")
	gt.NoError(t, err).Required()

	foundTitleChanged := false
	foundStatusChanged := false
	for _, ev := range events {
		switch ev.Kind {
		case types.ActionEventTitleChanged:
			foundTitleChanged = true
			gt.Value(t, ev.ActorID).Equal(actorID)
			gt.Value(t, ev.ActorID).NotEqual("") // contrast: ActorKindSystem would record ""
			gt.Value(t, ev.OldValue).Equal("Original title")
			gt.Value(t, ev.NewValue).Equal(newTitle)
		case types.ActionEventStatusChanged:
			foundStatusChanged = true
			gt.Value(t, ev.ActorID).Equal(actorID)
			gt.Value(t, ev.ActorID).NotEqual("")
		}
	}
	gt.Bool(t, foundTitleChanged).True()
	gt.Bool(t, foundStatusChanged).True()
}

// A turn an app started acts as the case channel. Its writes must go through
// (the channel actor may reach its own case, even a private one) and be
// recorded as the system's, never as a user id that is not a person.
func TestCaseMultiActionAdapter_CaseChannelActorIsAttributedToTheSystem(t *testing.T) {
	f := newCasemultiActionFixture(t)

	stored, err := f.repo.Case().Get(context.Background(), testWorkspaceID, f.caseID)
	gt.NoError(t, err).Required()
	stored.IsPrivate = true
	stored.SlackChannelID = "C0CASE01"
	stored.ChannelUserIDs = []string{"U-CREATOR"}
	_, err = f.repo.Case().Update(context.Background(), testWorkspaceID, stored)
	gt.NoError(t, err).Required()

	actorID := model.CaseChannelActorID("C0CASE01")
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: actorID})

	newTitle := "Updated by an app-started turn"
	updated, err := f.adapter.UpdateAction(ctx, testWorkspaceID, f.actionID, casemulti.ActionUpdate{Title: &newTitle}, actorID)
	gt.NoError(t, err).Required()
	gt.Value(t, updated.Title).Equal(newTitle)

	events, _, err := f.repo.ActionEvent().List(ctx, testWorkspaceID, f.actionID, 10, "")
	gt.NoError(t, err).Required()
	found := false
	for _, ev := range events {
		if ev.Kind == types.ActionEventTitleChanged {
			found = true
			gt.Value(t, ev.ActorID).Equal("")
		}
	}
	gt.Bool(t, found).True()

	step, err := f.adapter.AddActionStep(ctx, testWorkspaceID, f.actionID, "Follow up", actorID)
	gt.NoError(t, err).Required()
	gt.Value(t, step.CreatedBy).Equal("")

	created, err := f.adapter.CreateAction(ctx, testWorkspaceID, f.caseID, "Created by an app-started turn", "")
	gt.NoError(t, err).Required()
	createdEvents, _, err := f.repo.ActionEvent().List(ctx, testWorkspaceID, created.ID, 10, "")
	gt.NoError(t, err).Required()
	gt.Array(t, createdEvents).Length(1).Required()
	gt.Value(t, createdEvents[0].Kind).Equal(types.ActionEventCreated)
	gt.Value(t, createdEvents[0].ActorID).Equal("")

	// Another channel's actor is still refused on the private case.
	otherCtx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: model.CaseChannelActorID("C0OTHER9")})
	_, err = f.adapter.UpdateAction(otherCtx, testWorkspaceID, f.actionID, casemulti.ActionUpdate{Title: &newTitle}, model.CaseChannelActorID("C0OTHER9"))
	gt.Error(t, err).Is(usecase.ErrAccessDenied)
}

// TestCaseMultiActionAdapter_AddActionStep verifies the step is persisted and
// the actorID is recorded as the creator (ActionStep.CreatedBy), mirroring
// actionIdentifier's SlackUser-actor path.
func TestCaseMultiActionAdapter_AddActionStep(t *testing.T) {
	f := newCasemultiActionFixture(t)

	const actorID = "U-HUMAN"
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: actorID})

	step, err := f.adapter.AddActionStep(ctx, testWorkspaceID, f.actionID, "Investigate root cause", actorID)
	gt.NoError(t, err).Required()
	gt.Value(t, step.Title).Equal("Investigate root cause")
	gt.Value(t, step.CreatedBy).Equal(actorID)
	gt.Bool(t, step.IsDone()).False()

	stored, err := f.repo.ActionStep().Get(ctx, testWorkspaceID, f.actionID, step.ID)
	gt.NoError(t, err).Required()
	gt.Value(t, stored.Title).Equal("Investigate root cause")
	gt.Value(t, stored.CreatedBy).Equal(actorID)
	gt.Bool(t, stored.IsDone()).False()
}

// TestCaseMultiActionAdapter_SetActionStepDone verifies the adapter marks a
// step done/undone and records the actorID as DoneBy.
func TestCaseMultiActionAdapter_SetActionStepDone(t *testing.T) {
	f := newCasemultiActionFixture(t)

	const actorID = "U-HUMAN"
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: actorID})

	step, err := f.adapter.AddActionStep(ctx, testWorkspaceID, f.actionID, "Investigate root cause", actorID)
	gt.NoError(t, err).Required()

	done, err := f.adapter.SetActionStepDone(ctx, testWorkspaceID, f.actionID, step.ID, true, actorID)
	gt.NoError(t, err).Required()
	gt.Bool(t, done.IsDone()).True()
	gt.Value(t, done.DoneBy).Equal(actorID)

	stored, err := f.repo.ActionStep().Get(ctx, testWorkspaceID, f.actionID, step.ID)
	gt.NoError(t, err).Required()
	gt.Bool(t, stored.IsDone()).True()
	gt.Value(t, stored.DoneBy).Equal(actorID)

	// Toggling back to not-done clears DoneBy.
	reopened, err := f.adapter.SetActionStepDone(ctx, testWorkspaceID, f.actionID, step.ID, false, actorID)
	gt.NoError(t, err).Required()
	gt.Bool(t, reopened.IsDone()).False()
	gt.Value(t, reopened.DoneBy).Equal("")
}
