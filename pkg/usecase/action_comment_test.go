package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	goslack "github.com/slack-go/slack"
)

// commentSlackFake records every PostThreadMessage invocation so comment tests
// can assert on the exact channel / thread / text that the usecase emitted.
type commentSlackFake struct {
	mockSlackService
	threadCalls []commentSlackCall
}

type commentSlackCall struct {
	channelID string
	threadTS  string
	text      string
	blocks    []goslack.Block
	broadcast bool
}

func (f *commentSlackFake) PostThreadMessage(ctx context.Context, channelID, threadTS string, blocks []goslack.Block, text string, opts ...slacksvc.PostThreadOption) (string, error) {
	cfg := slacksvc.ApplyPostThreadOptions(opts...)
	f.threadCalls = append(f.threadCalls, commentSlackCall{
		channelID: channelID,
		threadTS:  threadTS,
		text:      text,
		blocks:    blocks,
		broadcast: cfg.Broadcast,
	})
	return "thread-ts", nil
}

// failingCommentRepo wraps a memory repository and replaces only the
// ActionComment accessor, so a persistence failure can be exercised without
// stubbing the whole Repository surface.
type failingCommentRepo struct {
	*memory.Repository
	comments interfaces.ActionCommentRepository
}

func (r *failingCommentRepo) ActionComment() interfaces.ActionCommentRepository {
	return r.comments
}

var (
	errCommentCreateFailed = goerr.New("comment create failed")
	errCommentGetFailed    = goerr.New("comment storage unavailable")
)

type failingActionCommentRepository struct {
	interfaces.ActionCommentRepository
}

func (r *failingActionCommentRepository) Create(context.Context, string, int64, *model.ActionComment) error {
	return errCommentCreateFailed
}

// brokenGetActionCommentRepository answers every Get with a storage failure
// that is NOT ErrActionCommentNotFound, so the usecase must surface it rather
// than reporting the comment as missing.
type brokenGetActionCommentRepository struct {
	interfaces.ActionCommentRepository
}

func (r *brokenGetActionCommentRepository) Get(context.Context, string, int64, string) (*model.ActionComment, error) {
	return nil, errCommentGetFailed
}

type commentTestFixture struct {
	repo       *memory.Repository
	commentUC  *usecase.ActionCommentUseCase
	slack      *commentSlackFake
	action     *model.Action
	caseModel  *model.Case
	caseUserID string
}

type commentFixtureOptions struct {
	isPrivate      bool
	channelUserIDs []string
	baseURL        string
	slotDuration   time.Duration
	noSlack        bool
	clearMessageTS bool
	clearChannelID bool
}

func newCommentTestFixture(t *testing.T, opts commentFixtureOptions) *commentTestFixture {
	t.Helper()
	i18n.Init(i18n.LangEN)

	repo := memory.New()
	slackFake := &commentSlackFake{}
	caseUC := usecase.NewCaseUseCase(repo, nil, slackFake, nil, "")
	actionUC := usecase.NewActionUseCase(repo, nil, slackFake, "", nil)

	caseUserID := "UCASEMEMBER"
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: caseUserID})

	c, err := caseUC.CreateCase(ctx, testWorkspaceID, "Comment Case", "", nil, nil, opts.isPrivate, false, "", "")
	gt.NoError(t, err).Required()

	caseModel, err := repo.Case().Get(ctx, testWorkspaceID, c.ID)
	gt.NoError(t, err).Required()
	caseModel.SlackChannelID = "CCOMMENT"
	if opts.clearChannelID {
		caseModel.SlackChannelID = ""
	}
	caseModel.IsPrivate = opts.isPrivate
	caseModel.ChannelUserIDs = append([]string{caseUserID}, opts.channelUserIDs...)
	_, err = repo.Case().Update(ctx, testWorkspaceID, caseModel)
	gt.NoError(t, err).Required()

	action, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Comment Action", "", "", "1700000000.000100", types.ActionStatusTodo, nil)
	gt.NoError(t, err).Required()

	if opts.clearMessageTS {
		action.SlackMessageTS = ""
		action, err = repo.Action().Update(ctx, testWorkspaceID, action)
		gt.NoError(t, err).Required()
	}

	var slotCoord *usecase.NotificationSlotCoordinatorForTest
	if opts.slotDuration > 0 {
		slotCoord = usecase.NewNotificationSlotCoordinatorForTest(repo.NotificationSlot(), slackFake, opts.slotDuration, nil)
	}

	var slackForUC slacksvc.Service
	if !opts.noSlack {
		slackForUC = slackFake
	}
	commentUC := usecase.NewActionCommentUseCase(repo, nil, slackForUC, opts.baseURL, slotCoord)

	// Drop the setup posts so assertions only see comment-driven calls.
	slackFake.threadCalls = nil
	slackFake.postedChannelIDs = nil
	slackFake.postedTexts = nil
	slackFake.updatedChannelIDs = nil
	slackFake.updatedTexts = nil

	return &commentTestFixture{
		repo:       repo,
		commentUC:  commentUC,
		slack:      slackFake,
		action:     action,
		caseModel:  caseModel,
		caseUserID: caseUserID,
	}
}

func (f *commentTestFixture) ctx() context.Context {
	return auth.ContextWithToken(context.Background(), &auth.Token{Sub: f.caseUserID})
}

func (f *commentTestFixture) ctxAs(userID string) context.Context {
	return auth.ContextWithToken(context.Background(), &auth.Token{Sub: userID})
}

// commentCreatedText posts one comment through a usecase whose workspace
// configures wsLang, under the deployment default defaultLang, and returns the
// thread notice. The context carries no user language.
func commentCreatedText(t *testing.T, registeredID string, wsLang, defaultLang i18n.Lang) string {
	t.Helper()
	f := newCommentTestFixture(t, commentFixtureOptions{})
	i18n.Init(defaultLang)
	defer i18n.Init(i18n.LangEN)

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: registeredID}, Language: wsLang})
	uc := usecase.NewActionCommentUseCase(f.repo, registry, f.slack, "", nil)

	_, err := uc.Create(f.ctx(), usecase.CreateActionCommentInput{
		WorkspaceID: testWorkspaceID,
		ActionID:    f.action.ID,
		Body:        "looks like maintenance",
	})
	gt.NoError(t, err).Required()
	gt.Array(t, f.slack.threadCalls).Length(1).Required()
	return f.slack.threadCalls[0].text
}

func TestActionCommentUseCase_NotificationFollowsTheWorkspaceLanguage(t *testing.T) {
	inJapanese := commentCreatedText(t, testWorkspaceID, "", i18n.LangJA)
	inEnglish := commentCreatedText(t, testWorkspaceID, "", i18n.LangEN)
	gt.String(t, inJapanese).NotEqual(inEnglish)

	gt.String(t, commentCreatedText(t, testWorkspaceID, i18n.LangJA, i18n.LangEN)).Equal(inJapanese)
	gt.String(t, commentCreatedText(t, testWorkspaceID, i18n.LangEN, i18n.LangJA)).Equal(inEnglish)
	// A workspace missing from the registry has no language: the notice keeps
	// the deployment default and the comment is still created.
	gt.String(t, commentCreatedText(t, "another-workspace", i18n.LangJA, i18n.LangEN)).Equal(inEnglish)
}

func TestActionCommentUseCase_Create(t *testing.T) {
	t.Run("persists the comment with the token author and a trimmed body", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{baseURL: "https://app.example.com"})
		ctx := f.ctx()

		comment, err := f.commentUC.Create(ctx, usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "  looks like maintenance  ",
		})
		gt.NoError(t, err).Required()
		gt.Value(t, comment.Body).Equal("looks like maintenance")
		gt.Value(t, comment.AuthorID).Equal(f.caseUserID)
		gt.Value(t, comment.ActionID).Equal(f.action.ID)
		gt.Value(t, comment.CreatedAt).Equal(comment.UpdatedAt)
		gt.Bool(t, comment.IsEdited()).False()

		stored, err := f.repo.ActionComment().Get(ctx, testWorkspaceID, f.action.ID, comment.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Body).Equal("looks like maintenance")
		gt.Value(t, stored.AuthorID).Equal(f.caseUserID)
	})

	t.Run("notifies the action thread with a link and an excerpt only", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{baseURL: "https://app.example.com"})
		ctx := f.ctx()

		longBody := strings.Repeat("x", 300)
		comment, err := f.commentUC.Create(ctx, usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        longBody,
		})
		gt.NoError(t, err).Required()

		gt.Array(t, f.slack.threadCalls).Length(1).Required()
		call := f.slack.threadCalls[0]
		gt.Value(t, call.channelID).Equal("CCOMMENT")
		gt.Value(t, call.threadTS).Equal(f.action.SlackMessageTS)
		gt.Array(t, call.blocks).Length(1)

		wantLink := "https://app.example.com/ws/" + testWorkspaceID +
			"/cases/" + strconv.FormatInt(f.action.CaseID, 10) +
			"/actions/" + strconv.FormatInt(f.action.ID, 10) +
			"?comment=" + comment.ID
		gt.Bool(t, strings.Contains(call.text, "<@"+f.caseUserID+">")).True()
		gt.Bool(t, strings.Contains(call.text, wantLink)).True()
		gt.Bool(t, strings.Contains(call.text, "\n> "+strings.Repeat("x", 100)+"…")).True()
		// The full body must never reach the channel.
		gt.Bool(t, strings.Contains(call.text, longBody)).False()
	})

	t.Run("omits the link when no base URL is configured", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		ctx := f.ctx()

		_, err := f.commentUC.Create(ctx, usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "no link here",
		})
		gt.NoError(t, err).Required()

		gt.Array(t, f.slack.threadCalls).Length(1).Required()
		call := f.slack.threadCalls[0]
		gt.Bool(t, strings.Contains(call.text, "Open comment")).False()
		gt.Bool(t, strings.Contains(call.text, "<@"+f.caseUserID+"> commented")).True()
		gt.Bool(t, strings.Contains(call.text, "\n> no link here")).True()
	})

	t.Run("broadcasts on the thread post when no slot is active", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{baseURL: "https://app.example.com"})

		_, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "channel should see this",
		})
		gt.NoError(t, err).Required()

		gt.Array(t, f.slack.threadCalls).Length(1).Required()
		gt.Bool(t, f.slack.threadCalls[0].broadcast).True()
		gt.Array(t, f.slack.postedChannelIDs).Length(0)
	})

	t.Run("folds the channel line into the notification slot when enabled", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{
			baseURL:      "https://app.example.com",
			slotDuration: time.Hour,
		})

		_, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "aggregated",
		})
		gt.NoError(t, err).Required()

		gt.Array(t, f.slack.threadCalls).Length(1).Required()
		gt.Bool(t, f.slack.threadCalls[0].broadcast).False()

		// The slot posts its own aggregated channel message.
		gt.Array(t, f.slack.postedChannelIDs).Length(1).Required()
		gt.Value(t, f.slack.postedChannelIDs[0]).Equal("CCOMMENT")

		slot, err := f.repo.NotificationSlot().GetActive(f.ctx(), "CCOMMENT", time.Now().UTC())
		gt.NoError(t, err).Required()
		gt.NotNil(t, slot)
		gt.Array(t, slot.Entries).Length(1).Required()
		gt.Value(t, slot.Entries[0].ActionMessageTS).Equal(f.action.SlackMessageTS)
		gt.Value(t, slot.Entries[0].ActionTitle).Equal(f.action.Title)
		gt.Value(t, slot.Entries[0].Body).Equal(f.slack.threadCalls[0].text)
	})

	t.Run("skips the notification when Slack is not wired", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{noSlack: true})

		comment, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "no slack",
		})
		gt.NoError(t, err).Required()
		gt.NotNil(t, comment)
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("skips the notification when the action has no Slack card", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{clearMessageTS: true})

		comment, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "no card",
		})
		gt.NoError(t, err).Required()
		gt.NotNil(t, comment)
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("skips the notification when the case has no Slack channel", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{clearChannelID: true})

		comment, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "no channel",
		})
		gt.NoError(t, err).Required()
		gt.NotNil(t, comment)
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("does not notify when the write fails", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{baseURL: "https://app.example.com"})
		broken := &failingCommentRepo{
			Repository: f.repo,
			comments:   &failingActionCommentRepository{ActionCommentRepository: f.repo.ActionComment()},
		}
		uc := usecase.NewActionCommentUseCase(broken, nil, f.slack, "https://app.example.com", nil)

		_, err := uc.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "never lands",
		})
		gt.Error(t, err).Is(errCommentCreateFailed)
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("rejects an unknown action", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})

		_, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID + 9999,
			Body:        "orphan",
		})
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})

	t.Run("rejects a non-member of a private case", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{isPrivate: true})

		_, err := f.commentUC.Create(f.ctxAs("UOUTSIDER"), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "should be denied",
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("allows a member of a private case", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{isPrivate: true, channelUserIDs: []string{"UFRIEND"}})

		comment, err := f.commentUC.Create(f.ctxAs("UFRIEND"), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "allowed",
		})
		gt.NoError(t, err).Required()
		gt.Value(t, comment.AuthorID).Equal("UFRIEND")
	})

	t.Run("rejects a context without an auth token", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})

		_, err := f.commentUC.Create(context.Background(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "no author",
		})
		gt.Error(t, err).Is(usecase.ErrInvalidArgument)
	})
}

func TestActionCommentUseCase_Update(t *testing.T) {
	seed := func(t *testing.T, f *commentTestFixture, body string) *model.ActionComment {
		t.Helper()
		comment, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        body,
		})
		gt.NoError(t, err).Required()
		f.slack.threadCalls = nil
		return comment
	}

	t.Run("the author can rewrite the body and it is marked edited", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f, "first draft")

		updated, err := f.commentUC.Update(f.ctx(), usecase.UpdateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
			Body:        "  second draft  ",
		})
		gt.NoError(t, err).Required()
		gt.Value(t, updated.Body).Equal("second draft")
		gt.Bool(t, updated.IsEdited()).True()

		stored, err := f.repo.ActionComment().Get(f.ctx(), testWorkspaceID, f.action.ID, comment.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Body).Equal("second draft")

		// Editing is silent on Slack.
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("an unchanged body is a no-op and does not mark the comment edited", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f, "stable")

		updated, err := f.commentUC.Update(f.ctx(), usecase.UpdateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
			Body:        "  stable  ",
		})
		gt.NoError(t, err).Required()
		gt.Bool(t, updated.IsEdited()).False()
		gt.Value(t, updated.UpdatedAt).Equal(comment.UpdatedAt)
	})

	t.Run("another user cannot edit someone else's comment", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f, "mine")

		_, err := f.commentUC.Update(f.ctxAs("UOTHER"), usecase.UpdateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
			Body:        "hijacked",
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		stored, err := f.repo.ActionComment().Get(f.ctx(), testWorkspaceID, f.action.ID, comment.ID)
		gt.NoError(t, err).Required()
		gt.Value(t, stored.Body).Equal("mine")
	})

	t.Run("an unknown comment id is reported as not found", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})

		_, err := f.commentUC.Update(f.ctx(), usecase.UpdateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   "no-such-comment",
			Body:        "ghost",
		})
		gt.Error(t, err).Is(usecase.ErrActionCommentNotFound)
	})

	t.Run("a storage failure is not reported as not found", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f, "will not be read")

		broken := &failingCommentRepo{
			Repository: f.repo,
			comments:   &brokenGetActionCommentRepository{ActionCommentRepository: f.repo.ActionComment()},
		}
		uc := usecase.NewActionCommentUseCase(broken, nil, f.slack, "", nil)

		_, err := uc.Update(f.ctx(), usecase.UpdateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
			Body:        "cannot be saved",
		})
		gt.Error(t, err).Is(errCommentGetFailed)
		// A storage outage must not masquerade as a 404.
		gt.Bool(t, errors.Is(err, usecase.ErrActionCommentNotFound)).False()
	})

	t.Run("an edit loses to a concurrent delete instead of resurrecting the comment", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f, "about to be deleted elsewhere")

		// Another tab of the same author deletes it after this edit was opened.
		gt.NoError(t, f.repo.ActionComment().Delete(f.ctx(), testWorkspaceID, f.action.ID, comment.ID)).Required()

		_, err := f.commentUC.Update(f.ctx(), usecase.UpdateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
			Body:        "edited after deletion",
		})
		gt.Error(t, err).Is(usecase.ErrActionCommentNotFound)

		got, _, err := f.commentUC.List(f.ctx(), testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(0)
	})
}

func TestActionCommentUseCase_Delete(t *testing.T) {
	seed := func(t *testing.T, f *commentTestFixture) *model.ActionComment {
		t.Helper()
		comment, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "to be removed",
		})
		gt.NoError(t, err).Required()
		f.slack.threadCalls = nil
		return comment
	}

	t.Run("the author can delete their own comment", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f)

		gt.NoError(t, f.commentUC.Delete(f.ctx(), usecase.DeleteActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
		})).Required()

		got, _, err := f.commentUC.List(f.ctx(), testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(0)

		// Deleting is silent on Slack.
		gt.Array(t, f.slack.threadCalls).Length(0)
	})

	t.Run("another user cannot delete someone else's comment", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})
		comment := seed(t, f)

		err := f.commentUC.Delete(f.ctxAs("UOTHER"), usecase.DeleteActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   comment.ID,
		})
		gt.Error(t, err).Is(usecase.ErrAccessDenied)

		got, _, err := f.commentUC.List(f.ctx(), testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(1)
	})

	t.Run("an unknown comment id is reported as not found", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})

		err := f.commentUC.Delete(f.ctx(), usecase.DeleteActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			CommentID:   "no-such-comment",
		})
		gt.Error(t, err).Is(usecase.ErrActionCommentNotFound)
	})
}

func TestActionCommentUseCase_List(t *testing.T) {
	t.Run("a non-member of a private case sees nothing", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{isPrivate: true})
		_, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "members only",
		})
		gt.NoError(t, err).Required()

		got, cursor, err := f.commentUC.List(f.ctxAs("UOUTSIDER"), testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(0)
		gt.Value(t, cursor).Equal("")
	})

	t.Run("a member of a private case sees the comments", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{isPrivate: true, channelUserIDs: []string{"UFRIEND"}})
		_, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "members only",
		})
		gt.NoError(t, err).Required()

		got, _, err := f.commentUC.List(f.ctxAs("UFRIEND"), testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(1)
	})

	t.Run("a context without an auth token reads freely", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{isPrivate: true})
		_, err := f.commentUC.Create(f.ctx(), usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    f.action.ID,
			Body:        "system readable",
		})
		gt.NoError(t, err).Required()

		got, _, err := f.commentUC.List(context.Background(), testWorkspaceID, f.action.ID, 10, "")
		gt.NoError(t, err).Required()
		gt.Array(t, got).Length(1)
	})

	t.Run("an unknown action is reported as not found", func(t *testing.T) {
		f := newCommentTestFixture(t, commentFixtureOptions{})

		_, _, err := f.commentUC.List(f.ctx(), testWorkspaceID, f.action.ID+9999, 10, "")
		gt.Error(t, err).Is(usecase.ErrActionNotFound)
	})
}

func TestActionCommentInput_Validate(t *testing.T) {
	t.Run("create rejects a missing workspace", func(t *testing.T) {
		in := usecase.CreateActionCommentInput{ActionID: 1, Body: "x"}
		gt.Error(t, in.Validate()).Is(usecase.ErrInvalidArgument)
	})

	t.Run("create rejects a missing action id", func(t *testing.T) {
		in := usecase.CreateActionCommentInput{WorkspaceID: testWorkspaceID, Body: "x"}
		gt.Error(t, in.Validate()).Is(usecase.ErrInvalidArgument)
	})

	t.Run("create rejects a whitespace-only body", func(t *testing.T) {
		in := usecase.CreateActionCommentInput{WorkspaceID: testWorkspaceID, ActionID: 1, Body: "   \n\t "}
		gt.Error(t, in.Validate()).Is(usecase.ErrInvalidArgument)
	})

	t.Run("create rejects a body over the cap", func(t *testing.T) {
		in := usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    1,
			Body:        strings.Repeat("a", model.ActionCommentBodyMaxLen+1),
		}
		gt.Error(t, in.Validate()).Is(usecase.ErrInvalidArgument)
	})

	t.Run("create accepts a body at the cap", func(t *testing.T) {
		in := usecase.CreateActionCommentInput{
			WorkspaceID: testWorkspaceID,
			ActionID:    1,
			Body:        strings.Repeat("a", model.ActionCommentBodyMaxLen),
		}
		gt.NoError(t, in.Validate())
	})

	t.Run("update rejects a missing comment id", func(t *testing.T) {
		in := usecase.UpdateActionCommentInput{WorkspaceID: testWorkspaceID, ActionID: 1, Body: "x"}
		gt.Error(t, in.Validate()).Is(usecase.ErrInvalidArgument)
	})

	t.Run("delete rejects a missing comment id", func(t *testing.T) {
		in := usecase.DeleteActionCommentInput{WorkspaceID: testWorkspaceID, ActionID: 1}
		gt.Error(t, in.Validate()).Is(usecase.ErrInvalidArgument)
	})

	t.Run("delete accepts a complete input", func(t *testing.T) {
		in := usecase.DeleteActionCommentInput{WorkspaceID: testWorkspaceID, ActionID: 1, CommentID: "c1"}
		gt.NoError(t, in.Validate())
	})
}

func TestCommentExcerpt(t *testing.T) {
	t.Run("empty input yields an empty excerpt", func(t *testing.T) {
		gt.Value(t, usecase.CommentExcerptForTest("   \n  ")).Equal("")
	})

	t.Run("collapses newlines and repeated spaces", func(t *testing.T) {
		gt.Value(t, usecase.CommentExcerptForTest("first\n\nsecond   third")).Equal("first second third")
	})

	t.Run("truncates on a rune boundary with an ellipsis", func(t *testing.T) {
		got := usecase.CommentExcerptForTest(strings.Repeat("あ", 101))
		gt.Value(t, got).Equal(strings.Repeat("あ", 100) + "…")
	})

	t.Run("keeps a body exactly at the cap intact", func(t *testing.T) {
		got := usecase.CommentExcerptForTest(strings.Repeat("b", 100))
		gt.Value(t, got).Equal(strings.Repeat("b", 100))
	})

	t.Run("escapes Slack mrkdwn control characters", func(t *testing.T) {
		gt.Value(t, usecase.CommentExcerptForTest("a & b < c > d | e")).
			Equal("a &amp; b &lt; c &gt; d ｜ e")
	})
}

func TestActionCommentBroadcasts(t *testing.T) {
	// Comments get the same channel-level attention as a status change.
	gt.Bool(t, usecase.ActionCommentBroadcastsForTest).True()
	gt.Bool(t, usecase.ShouldBroadcastActionEventForTest(types.ActionEventStatusChanged)).True()
}

func TestActionCommentUseCase_ThreadModeCaseNotifiesInTheCaseThread(t *testing.T) {
	i18n.Init(i18n.LangEN)
	repo := memory.New()
	fake := &threadModeSlackFake{}
	slot := usecase.NewNotificationSlotCoordinatorForTest(repo.NotificationSlot(), fake, time.Hour, nil)
	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "UAUTHOR"})
	c := newThreadModeCase(t, repo)

	actionUC := usecase.NewActionUseCase(repo, threadModeActionsRegistry(true), fake, "", nil)
	action, err := actionUC.CreateAction(ctx, testWorkspaceID, c.ID, "Sample action", "", "", "", "", nil)
	gt.NoError(t, err).Required()
	fake.threadCalls = nil

	commentUC := usecase.NewActionCommentUseCase(repo, nil, fake, "https://example.com", slot)
	comment, err := commentUC.Create(ctx, usecase.CreateActionCommentInput{
		WorkspaceID: testWorkspaceID,
		ActionID:    action.ID,
		Body:        "Example comment",
	})
	gt.NoError(t, err).Required()

	gt.Array(t, fake.postedChannelIDs).Length(0)
	gt.Array(t, fake.threadCalls).Length(1).Required()
	assertQuietCaseThreadReply(t, fake.threadCalls[0])
	actionURL := fmt.Sprintf("https://example.com/ws/%s/cases/%d/actions/%d", testWorkspaceID, c.ID, action.ID)
	gt.Value(t, fake.threadCalls[0].text).Equal(fmt.Sprintf(
		"📌 *<%s|Sample action>*\n:speech_balloon: <@UAUTHOR> commented: <%s?comment=%s|Open comment>\n> Example comment",
		actionURL, actionURL, comment.ID))
}
