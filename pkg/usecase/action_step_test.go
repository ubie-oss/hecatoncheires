package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	goslack "github.com/slack-go/slack"
)

// stepSlackFake records every PostThreadMessage invocation so step tests can
// assert on the exact channel / thread / text that the usecase emitted.
type stepSlackFake struct {
	mockSlackService
	calls []stepSlackCall
}

type stepSlackCall struct {
	channelID string
	threadTS  string
	text      string
	blocks    []goslack.Block
	broadcast bool
}

func (f *stepSlackFake) PostThreadMessage(ctx context.Context, channelID, threadTS string, blocks []goslack.Block, text string, opts ...slacksvc.PostThreadOption) (string, error) {
	cfg := slacksvc.ApplyPostThreadOptions(opts...)
	f.calls = append(f.calls, stepSlackCall{
		channelID: channelID,
		threadTS:  threadTS,
		text:      text,
		blocks:    blocks,
		broadcast: cfg.Broadcast,
	})
	return "thread-ts", nil
}

type stepTestFixture struct {
	repo       *memory.Repository
	stepUC     *usecase.ActionStepUseCase
	slack      *stepSlackFake
	action     *model.Action
	caseModel  *model.Case
	caseUserID string
}

func newStepTestFixture(t *testing.T, channelUserIDs []string, isPrivate bool) *stepTestFixture {
	t.Helper()
	i18n.Init(i18n.LangEN)

	repo := memory.New()
	slackFake := &stepSlackFake{}
	caseUC := usecase.NewCaseUseCase(repo, nil, slackFake, nil, "")
	actionUC := usecase.NewActionUseCase(repo, nil, slackFake, "", nil)
	stepUC := usecase.NewActionStepUseCase(repo, nil, slackFake, nil)

	caseUserID := "UCASEMEMBER"
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: caseUserID})

	c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Step Case", "", nil, nil, false, false, "", "")
	gt.NoError(t, err).Required()

	// Pin a Slack channel + membership directly on the persisted Case so the
	// test does not rely on the Slack mock returning member listings. When
	// isPrivate is true the access-control branch is what we want to exercise.
	caseModel, err := repo.Case().Get(ctx, testWorkspaceID, c.ID)
	gt.NoError(t, err).Required()
	caseModel.SlackChannelID = "CSTEP"
	caseModel.IsPrivate = isPrivate
	caseModel.ChannelUserIDs = append([]string{caseUserID}, channelUserIDs...)
	_, err = repo.Case().Update(ctx, testWorkspaceID, caseModel)
	gt.NoError(t, err).Required()

	action, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Step Action", "", "", "1700000000.000100", types.ActionStatusTodo, nil)
	gt.NoError(t, err).Required()

	// Reset Slack call buffer after the setup posts so test assertions only
	// see step-driven calls.
	slackFake.calls = nil

	return &stepTestFixture{
		repo:       repo,
		stepUC:     stepUC,
		slack:      slackFake,
		action:     action,
		caseModel:  caseModel,
		caseUserID: caseUserID,
	}
}

func (f *stepTestFixture) ctx() context.Context {
	return auth.ContextWithToken(context.Background(), &auth.Token{Sub: f.caseUserID})
}

// stepAddedText adds one step through a usecase whose workspace configures
// wsLang, under the deployment default defaultLang, and returns the posted
// notice. The context carries no user language.
func stepAddedText(t *testing.T, wsLang, defaultLang i18n.Lang) string {
	t.Helper()
	f := newStepTestFixture(t, nil, false)
	i18n.Init(defaultLang)
	defer i18n.Init(i18n.LangEN)

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: testWorkspaceID}, Language: wsLang})
	uc := usecase.NewActionStepUseCase(f.repo, registry, f.slack, nil)

	_, err := uc.Add(f.ctx(), usecase.AddActionStepInput{
		WorkspaceID: testWorkspaceID,
		ActionID:    f.action.ID,
		Title:       "collect logs",
	})
	gt.NoError(t, err).Required()
	gt.Array(t, f.slack.calls).Length(1).Required()
	return f.slack.calls[0].text
}

func TestActionStepUseCase_NotificationFollowsTheWorkspaceLanguage(t *testing.T) {
	inJapanese := stepAddedText(t, "", i18n.LangJA)
	inEnglish := stepAddedText(t, "", i18n.LangEN)
	gt.String(t, inJapanese).NotEqual(inEnglish)

	gt.String(t, stepAddedText(t, i18n.LangJA, i18n.LangEN)).Equal(inJapanese)
	gt.String(t, stepAddedText(t, i18n.LangEN, i18n.LangJA)).Equal(inEnglish)
}

func TestActionStepUseCase_Add(t *testing.T) {
	t.Run("creates step and emits event + slack notification", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()

		step, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Title:       "  collect logs  ",
		})
		gt.NoError(t, err).Required()
		gt.String(t, step.Title).Equal("collect logs")
		gt.Bool(t, step.IsDone()).False()
		gt.Value(t, step.CreatedBy).Equal(f.caseUserID)
		gt.Value(t, step.ActionID).Equal(f.action.ID)

		stored, err := f.repo.ActionStep().Get(ctx, testWorkspaceID, f.action.ID, step.ID)
		gt.NoError(t, err).Required()
		gt.String(t, stored.Title).Equal("collect logs")

		events, _, err := f.repo.ActionEvent().List(ctx, testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, events).Length(2).Required() // CREATED + STEP_ADDED
		gt.Value(t, events[0].Kind).Equal(types.ActionEventStepAdded)
		gt.Value(t, events[0].NewValue).Equal("collect logs")

		gt.Array(t, f.slack.calls).Length(1).Required()
		gt.Value(t, f.slack.calls[0].channelID).Equal(f.caseModel.SlackChannelID)
		gt.Value(t, f.slack.calls[0].threadTS).Equal(f.action.SlackMessageTS)
		gt.String(t, f.slack.calls[0].text).Contains("collect logs")
		gt.String(t, f.slack.calls[0].text).Contains("added step")
		gt.Bool(t, f.slack.calls[0].broadcast).True()
	})

	t.Run("rejects empty title", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		_, err := f.stepUC.Add(f.ctx(), usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Title:       "   ",
		})
		gt.Value(t, err).NotNil()
	})

	t.Run("missing parent action returns ErrActionNotFound", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		_, err := f.stepUC.Add(f.ctx(), usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    99999,
			Title:       "x",
		})
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})

	t.Run("private case denies non-member", func(t *testing.T) {
		f := newStepTestFixture(t, nil, true)
		intruder := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UINTRUDER"})

		_, err := f.stepUC.Add(intruder, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Title:       "secret step",
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
		gt.Array(t, f.slack.calls).Length(0)
	})

	t.Run("system actor (no token) bypasses access control", func(t *testing.T) {
		f := newStepTestFixture(t, nil, true)
		ctx := context.Background() // no auth token

		step, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Title:       "system-driven",
			Actor:       usecase.ActorRef{Kind: usecase.ActorKindSystem},
		})
		gt.NoError(t, err).Required()
		gt.Value(t, step.CreatedBy).Equal("")
	})
}

func TestActionStepUseCase_SetDone(t *testing.T) {
	t.Run("toggles done and back; ActionEvent fires per transition", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()

		created, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "task",
		})
		gt.NoError(t, err).Required()
		f.slack.calls = nil

		done, err := f.stepUC.SetDone(ctx, usecase.SetActionStepDoneInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID, Done: true,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, done.IsDone()).True()
		gt.Value(t, done.DoneBy).Equal(f.caseUserID)
		gt.Array(t, f.slack.calls).Length(1).Required()
		gt.String(t, f.slack.calls[0].text).Contains("completed step")
		gt.Bool(t, f.slack.calls[0].broadcast).True()

		f.slack.calls = nil
		reopened, err := f.stepUC.SetDone(ctx, usecase.SetActionStepDoneInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID, Done: false,
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, reopened.IsDone()).False()
		gt.String(t, reopened.DoneBy).Equal("")
		gt.Value(t, reopened.DoneAt).Nil()
		gt.Array(t, f.slack.calls).Length(1).Required()
		gt.String(t, f.slack.calls[0].text).Contains("reopened step")
		gt.Bool(t, f.slack.calls[0].broadcast).True()
	})

	t.Run("no-op when state is unchanged", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()
		created, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "task",
		})
		gt.NoError(t, err).Required()
		f.slack.calls = nil

		_, err = f.stepUC.SetDone(ctx, usecase.SetActionStepDoneInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID, Done: false,
		})
		gt.NoError(t, err).Required()
		gt.Array(t, f.slack.calls).Length(0)
	})

	t.Run("missing step returns ErrActionStepNotFound", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		_, err := f.stepUC.SetDone(f.ctx(), usecase.SetActionStepDoneInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: "nope", Done: true,
		})
		gt.Error(t, err).Is(usecase.ErrActionStepNotFound)
	})
}

func TestActionStepUseCase_Rename(t *testing.T) {
	t.Run("updates title, fires event with old/new", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()
		created, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "original",
		})
		gt.NoError(t, err).Required()
		f.slack.calls = nil

		renamed, err := f.stepUC.Rename(ctx, usecase.RenameActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID, Title: "  updated  ",
		})
		gt.NoError(t, err).Required()
		gt.String(t, renamed.Title).Equal("updated")

		events, _, err := f.repo.ActionEvent().List(ctx, testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Value(t, events[0].Kind).Equal(types.ActionEventStepTitleChanged)
		gt.Value(t, events[0].OldValue).Equal("original")
		gt.Value(t, events[0].NewValue).Equal("updated")

		gt.Array(t, f.slack.calls).Length(1).Required()
		gt.String(t, f.slack.calls[0].text).Contains("renamed step")
		gt.String(t, f.slack.calls[0].text).Contains("original")
		gt.String(t, f.slack.calls[0].text).Contains("updated")
		gt.Bool(t, f.slack.calls[0].broadcast).True()
	})

	t.Run("no-op when title is unchanged after trim", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()
		created, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "same",
		})
		gt.NoError(t, err).Required()
		f.slack.calls = nil

		_, err = f.stepUC.Rename(ctx, usecase.RenameActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID, Title: "  same  ",
		})
		gt.NoError(t, err).Required()
		gt.Array(t, f.slack.calls).Length(0)
	})

	t.Run("rejects empty title", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()
		created, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "x",
		})
		gt.NoError(t, err).Required()

		_, err = f.stepUC.Rename(ctx, usecase.RenameActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID, Title: "   ",
		})
		gt.Value(t, err).NotNil()
	})
}

func TestActionStepUseCase_Delete(t *testing.T) {
	t.Run("removes step and emits event", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()
		created, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "to-delete",
		})
		gt.NoError(t, err).Required()
		f.slack.calls = nil

		err = f.stepUC.Delete(ctx, usecase.DeleteActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: created.ID,
		})
		gt.NoError(t, err).Required()

		_, err = f.repo.ActionStep().Get(ctx, testWorkspaceID, f.action.ID, created.ID)
		gt.Error(t, err)

		gt.Array(t, f.slack.calls).Length(1).Required()
		gt.String(t, f.slack.calls[0].text).Contains("removed step")
		gt.String(t, f.slack.calls[0].text).Contains("to-delete")
		gt.Bool(t, f.slack.calls[0].broadcast).True()
	})

	t.Run("missing step returns ErrActionStepNotFound", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		err := f.stepUC.Delete(f.ctx(), usecase.DeleteActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: "missing",
		})
		gt.Error(t, err)
		gt.Bool(t, errors.Is(err, usecase.ErrActionStepNotFound)).True()
	})
}

func TestActionStepUseCase_ListAndProgress(t *testing.T) {
	t.Run("returns ordered list and progress counts", func(t *testing.T) {
		f := newStepTestFixture(t, nil, false)
		ctx := f.ctx()

		s1, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "first",
		})
		gt.NoError(t, err).Required()
		s2, err := f.stepUC.Add(ctx, usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "second",
		})
		gt.NoError(t, err).Required()

		_, err = f.stepUC.SetDone(ctx, usecase.SetActionStepDoneInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, StepID: s1.ID, Done: true,
		})
		gt.NoError(t, err).Required()

		listed, err := f.stepUC.List(ctx, testWorkspaceID, f.action.ID)
		gt.NoError(t, err).Required()
		gt.Array(t, listed).Length(2).Required()
		gt.Value(t, listed[0].ID).Equal(s1.ID)
		gt.Value(t, listed[1].ID).Equal(s2.ID)

		done, total, err := f.stepUC.Progress(ctx, testWorkspaceID, f.action.ID)
		gt.NoError(t, err).Required()
		gt.Number(t, done).Equal(1)
		gt.Number(t, total).Equal(2)
	})

	t.Run("private case non-member sees empty list and 0/0 progress", func(t *testing.T) {
		f := newStepTestFixture(t, nil, true)
		_, err := f.stepUC.Add(f.ctx(), usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "private",
		})
		gt.NoError(t, err).Required()

		intruder := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UINTRUDER"})
		listed, err := f.stepUC.List(intruder, testWorkspaceID, f.action.ID)
		gt.NoError(t, err).Required()
		gt.Array(t, listed).Length(0)

		done, total, err := f.stepUC.Progress(intruder, testWorkspaceID, f.action.ID)
		gt.NoError(t, err).Required()
		gt.Number(t, done).Equal(0)
		gt.Number(t, total).Equal(0)
	})

	t.Run("system context sees full list", func(t *testing.T) {
		f := newStepTestFixture(t, nil, true)
		_, err := f.stepUC.Add(f.ctx(), usecase.AddActionStepInput{
			WorkspaceID: testWorkspaceID, ActionID: f.action.ID, Title: "system-visible",
		})
		gt.NoError(t, err).Required()

		listed, err := f.stepUC.List(context.Background(), testWorkspaceID, f.action.ID)
		gt.NoError(t, err).Required()
		gt.Array(t, listed).Length(1)
	})
}

func TestActionStepUseCase_ThreadModeCaseNotifiesInTheCaseThread(t *testing.T) {
	i18n.Init(i18n.LangEN)
	repo := memory.New()
	fake := &threadModeSlackFake{}
	slot := usecase.NewNotificationSlotCoordinatorForTest(repo.NotificationSlot(), fake, time.Hour, nil)
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UCREATOR"})
	c := newThreadModeCase(t, repo)

	actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "", nil)
	action, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Sample action", "", "", "", "", nil)
	gt.NoError(t, err).Required()
	fake.threadCalls = nil

	stepUC := usecase.NewActionStepUseCase(repo, nil, fake, slot)
	_, err = stepUC.Add(ctx, usecase.AddActionStepInput{
		WorkspaceID: testWorkspaceID,
		ActionID:    action.ID,
		Title:       "Example step",
		Actor:       usecase.ActorRef{Kind: usecase.ActorKindSlackUser, ID: "UDOER"},
	})
	gt.NoError(t, err).Required()

	// A step addition is broadcast in channel mode; in the case thread it is
	// a quiet reply and never reaches the channel root or the slot.
	gt.Array(t, fake.postedChannelIDs).Length(0)
	gt.Array(t, fake.threadCalls).Length(1).Required()
	assertQuietCaseThreadReply(t, fake.threadCalls[0])
	gt.Value(t, fake.threadCalls[0].text).Equal("📌 *Sample action*\n:heavy_plus_sign: <@UDOER> added step \"Example step\"")
}
