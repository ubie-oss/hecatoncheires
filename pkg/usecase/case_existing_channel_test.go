package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

const (
	existingChWorkspace = "ws-existing"
	existingChOtherWS   = "ws-other"
	existingChThreadWS  = "ws-thread"
	existingChActor     = "U-ACTOR"
	existingChMonitor   = "C-MONITOR"
	existingChWSChannel = "C-WS-CHANNEL"
)

// existingChannelFixture is a channel-mode workspace with the feature enabled,
// a second channel-mode workspace, and a thread-mode workspace, sharing one
// repository and one fake Slack service.
type existingChannelFixture struct {
	repo     interfaces.Repository
	registry *model.WorkspaceRegistry
	slack    *mockSlackService
	uc       *usecase.CaseUseCase
	pub      *recordingCaseEventPublisher
	// channels is what conversations.info answers, keyed by channel ID. A
	// missing key answers ErrChannelUnavailable.
	channels map[string]*slack.ChannelInfo
	// members is what conversations.members answers, keyed by channel ID.
	members         map[string][]string
	createdChannels int
	membersCalls    int
}

func newExistingChannelFixture(t *testing.T) *existingChannelFixture {
	t.Helper()
	f := &existingChannelFixture{
		repo:     memory.New(),
		registry: model.NewWorkspaceRegistry(),
		channels: map[string]*slack.ChannelInfo{},
		members:  map[string][]string{},
	}
	f.registry.Register(&model.WorkspaceEntry{
		Workspace:                 model.Workspace{ID: existingChWorkspace},
		FieldSchema:               &config.FieldSchema{},
		CaseMode:                  model.CaseModeChannel,
		SlackWorkspaceChannelID:   existingChWSChannel,
		SlackTeamID:               "T-HOME",
		AllowExistingSlackChannel: true,
	})
	f.registry.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: existingChOtherWS},
		FieldSchema: &config.FieldSchema{},
		CaseMode:    model.CaseModeChannel,
	})
	f.registry.Register(&model.WorkspaceEntry{
		Workspace:             model.Workspace{ID: existingChThreadWS},
		FieldSchema:           &config.FieldSchema{},
		CaseMode:              model.CaseModeThread,
		SlackMonitorChannelID: existingChMonitor,
	})

	f.slack = &mockSlackService{
		getChannelInfoFn: func(_ context.Context, channelID string) (*slack.ChannelInfo, error) {
			info, ok := f.channels[channelID]
			if !ok {
				return nil, slack.ErrChannelUnavailable
			}
			return info, nil
		},
		getConversationMembersFn: func(_ context.Context, channelID string) ([]string, error) {
			f.membersCalls++
			return f.members[channelID], nil
		},
		createChannelFn: func(_ context.Context, _ int64, caseName string, _ string) (string, error) {
			f.createdChannels++
			return "C-NEW-" + caseName, nil
		},
	}
	f.uc = usecase.NewCaseUseCase(f.repo, f.registry, f.slack, nil, "https://hct.example.com")
	f.pub = &recordingCaseEventPublisher{}
	f.uc.SetEventPublisher(f.pub)

	gt.NoError(t, f.repo.SlackUser().SaveMany(context.Background(), []*model.SlackUser{
		{ID: existingChActor, Name: "actor", RealName: "Actor"},
		{ID: "U-OTHER", Name: "other", RealName: "Other"},
	})).Required()
	return f
}

func (f *existingChannelFixture) addChannel(id string, isPrivate, botIsMember bool, members ...string) {
	f.channels[id] = &slack.ChannelInfo{ID: id, Name: "name-" + id, IsPrivate: isPrivate, IsMember: botIsMember}
	f.members[id] = members
}

func (f *existingChannelFixture) caseCount(t *testing.T, workspaceID string) int {
	t.Helper()
	cases, err := f.repo.Case().List(context.Background(), workspaceID)
	gt.NoError(t, err).Required()
	return len(cases)
}

func actorContext() context.Context {
	return auth.ContextWithToken(context.Background(), &auth.Token{Sub: existingChActor})
}

func (f *existingChannelFixture) create(ctx context.Context, workspaceID string, isPrivate bool, channelID string) (*model.Case, error) {
	return f.uc.CreateCase(ctx, workspaceID, "Linked case", "desc", []string{"U-OTHER"}, nil, isPrivate, false, "", "",
		usecase.WithExistingSlackChannel(channelID))
}

func TestCaseUseCase_CreateCase_ExistingSlackChannel(t *testing.T) {
	t.Run("public channel the bot has joined is linked without creating a channel", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C1", false, true, existingChActor, "U-OTHER", "B-BOT")

		created, err := f.create(actorContext(), existingChWorkspace, false, "C1")
		gt.NoError(t, err).Required()

		gt.Value(t, created.Status).Equal(types.CaseStatusOpen)
		gt.String(t, created.SlackChannelID).Equal("C1")
		gt.Bool(t, created.IsPrivate).False()
		// B-BOT is not a known Slack user, so the human filter drops it.
		gt.Value(t, created.ChannelUserIDs).Equal([]string{existingChActor, "U-OTHER"})

		stored, err := f.repo.Case().Get(context.Background(), existingChWorkspace, created.ID)
		gt.NoError(t, err).Required()
		gt.String(t, stored.SlackChannelID).Equal("C1")
		gt.String(t, stored.ReporterID).Equal(existingChActor)

		gt.Number(t, f.createdChannels).Equal(0)
		gt.Array(t, f.slack.joinedChannelIDs).Length(0)
		gt.String(t, f.slack.invitedChannelID).Equal("C1")
		gt.Value(t, f.slack.invitedUserIDs).Equal([]string{existingChActor, "U-OTHER"})
		gt.String(t, f.slack.bookmarkChannelID).Equal("C1")

		gt.Array(t, f.pub.events).Length(1).Required()
		gt.Value(t, f.pub.events[0].lifecycle).Equal(model.CaseLifecycleCreated)
		gt.Value(t, f.pub.events[0].caseID).Equal(created.ID)
	})

	t.Run("private channel makes the case private", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("G1", true, true, existingChActor)

		created, err := f.create(actorContext(), existingChWorkspace, false, "G1")
		gt.NoError(t, err).Required()
		gt.Bool(t, created.IsPrivate).True()
		gt.String(t, created.SlackChannelID).Equal("G1")
	})

	t.Run("public channel without the bot is joined before the case is created", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C2", false, false, existingChActor)

		created, err := f.create(actorContext(), existingChWorkspace, false, "C2")
		gt.NoError(t, err).Required()
		gt.Value(t, f.slack.joinedChannelIDs).Equal([]string{"C2"})
		gt.String(t, created.SlackChannelID).Equal("C2")
	})

	t.Run("failed join creates no case and skips the membership check", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C3", false, false, existingChActor)
		f.slack.joinChannelFn = func(context.Context, string) error {
			return errors.New("missing_scope")
		}

		_, err := f.create(actorContext(), existingChWorkspace, false, "C3")
		gt.Error(t, err).Is(usecase.ErrSlackChannelJoinFailed)
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
		gt.Number(t, f.membersCalls).Equal(0)
	})

	t.Run("private channel without the bot is not linkable and is not joined", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("G2", true, false, existingChActor)

		_, err := f.create(actorContext(), existingChWorkspace, false, "G2")
		gt.Error(t, err).Is(usecase.ErrSlackChannelNotLinkable)
		gt.Array(t, f.slack.joinedChannelIDs).Length(0)
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
	})

	t.Run("disabled workspaces reject the request before calling Slack", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C1", false, true, existingChActor)

		for _, ws := range []string{existingChOtherWS, existingChThreadWS, "ws-unknown"} {
			_, err := f.create(actorContext(), ws, false, "C1")
			gt.Error(t, err).Is(usecase.ErrExistingSlackChannelDisabled)
		}
		gt.Number(t, f.membersCalls).Equal(0)
		gt.Number(t, f.caseCount(t, existingChOtherWS)).Equal(0)

		noSlack := usecase.NewCaseUseCase(f.repo, f.registry, nil, nil, "")
		_, err := noSlack.CreateCase(actorContext(), existingChWorkspace, "T", "", nil, nil, false, false, "", "",
			usecase.WithExistingSlackChannel("C1"))
		gt.Error(t, err).Is(usecase.ErrExistingSlackChannelDisabled)

		noRegistry := usecase.NewCaseUseCase(f.repo, nil, f.slack, nil, "")
		_, err = noRegistry.CreateCase(actorContext(), existingChWorkspace, "T", "", nil, nil, false, false, "", "",
			usecase.WithExistingSlackChannel("C1"))
		gt.Error(t, err).Is(usecase.ErrExistingSlackChannelDisabled)
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
	})

	t.Run("isPrivate and an empty channel ID are invalid arguments", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C1", false, true, existingChActor)

		_, err := f.create(actorContext(), existingChWorkspace, true, "C1")
		gt.Error(t, err).Is(usecase.ErrInvalidArgument)
		_, err = f.create(actorContext(), existingChWorkspace, false, "")
		gt.Error(t, err).Is(usecase.ErrInvalidArgument)

		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
		gt.Number(t, f.membersCalls).Equal(0)
		gt.Array(t, f.slack.joinedChannelIDs).Length(0)
	})

	t.Run("no auth token is access denied", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C1", false, true, existingChActor)

		_, err := f.create(context.Background(), existingChWorkspace, false, "C1")
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
	})

	t.Run("unlinkable channels are rejected without saving a case", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel(existingChMonitor, false, true, existingChActor)
		f.addChannel(existingChWSChannel, false, true, existingChActor)
		f.addChannel("C-ARCHIVED", false, true, existingChActor)
		f.channels["C-ARCHIVED"].IsArchived = true
		f.addChannel("C-NOT-MEMBER", false, true, "U-OTHER")

		for _, ch := range []string{existingChMonitor, existingChWSChannel, "C-MISSING", "C-ARCHIVED", "C-NOT-MEMBER"} {
			_, err := f.create(actorContext(), existingChWorkspace, false, ch)
			gt.Error(t, err).Is(usecase.ErrSlackChannelNotLinkable)
		}
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
		gt.Number(t, f.createdChannels).Equal(0)
	})

	t.Run("other conversations.info errors are returned as they are", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		infoErr := errors.New("internal_error")
		f.slack.getChannelInfoFn = func(context.Context, string) (*slack.ChannelInfo, error) {
			return nil, infoErr
		}

		_, err := f.create(actorContext(), existingChWorkspace, false, "C1")
		gt.Error(t, err).Is(infoErr)
		gt.Bool(t, errors.Is(err, usecase.ErrSlackChannelNotLinkable)).False()
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
	})

	t.Run("a channel used by a case in any workspace is already linked", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		ctx := context.Background()
		for _, ws := range []string{existingChWorkspace, existingChOtherWS, existingChThreadWS} {
			ch := "C-USED-" + ws
			// The bot is not a member, so a wrong order would join first.
			f.addChannel(ch, false, false, existingChActor)
			_, err := f.repo.Case().Create(ctx, ws, &model.Case{
				Title:          "Existing",
				Status:         types.CaseStatusOpen,
				ReporterID:     "U-REP",
				SlackChannelID: ch,
				CreatedAt:      time.Now().UTC(),
				UpdatedAt:      time.Now().UTC(),
			})
			gt.NoError(t, err).Required()

			_, err = f.create(actorContext(), existingChWorkspace, false, ch)
			gt.Error(t, err).Is(usecase.ErrSlackChannelAlreadyLinked)
		}
		gt.Array(t, f.slack.joinedChannelIDs).Length(0)
		// Only the seeded case exists in the requesting workspace.
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(1)
	})

	t.Run("a failed final update deletes the persisted case", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		f.addChannel("C1", false, true, existingChActor)
		failing := &updateFailingCaseRepo{Repository: f.repo}
		uc := usecase.NewCaseUseCase(failing, f.registry, f.slack, nil, "")

		_, err := uc.CreateCase(actorContext(), existingChWorkspace, "T", "", nil, nil, false, false, "", "",
			usecase.WithExistingSlackChannel("C1"))
		gt.Error(t, err).Is(errUpdateFailed)
		gt.Number(t, f.caseCount(t, existingChWorkspace)).Equal(0)
	})

	t.Run("without the option a new channel is created", func(t *testing.T) {
		f := newExistingChannelFixture(t)

		created, err := f.uc.CreateCase(actorContext(), existingChWorkspace, "Plain", "", nil, nil, false, false, "", "")
		gt.NoError(t, err).Required()
		gt.Number(t, f.createdChannels).Equal(1)
		gt.String(t, created.SlackChannelID).Equal("C-NEW-Plain")
		gt.Array(t, f.slack.joinedChannelIDs).Length(0)
	})
}

var errUpdateFailed = errors.New("update failed")

// updateFailingCaseRepo fails every Case().Update so the last step of channel
// activation fails after the case was persisted.
type updateFailingCaseRepo struct {
	interfaces.Repository
}

func (r *updateFailingCaseRepo) Case() interfaces.CaseRepository {
	return &updateFailingCases{CaseRepository: r.Repository.Case()}
}

type updateFailingCases struct {
	interfaces.CaseRepository
}

func (c *updateFailingCases) Update(context.Context, string, *model.Case) (*model.Case, error) {
	return nil, errUpdateFailed
}

func TestCaseUseCase_ListExistingSlackChannelCandidates(t *testing.T) {
	t.Run("returns the user's channels minus configured and linked ones, sorted by name", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		var gotUser, gotTeam string
		f.slack.listMemberChannelsFn = func(_ context.Context, userID, teamID string) ([]slack.Channel, error) {
			gotUser, gotTeam = userID, teamID
			return []slack.Channel{
				{ID: "C4", Name: "beta", IsPrivate: true},
				{ID: "C2", Name: "linked-elsewhere"},
				{ID: existingChMonitor, Name: "monitor"},
				{ID: "C1", Name: "alpha"},
			}, nil
		}
		_, err := f.repo.Case().Create(context.Background(), existingChOtherWS, &model.Case{
			Title:          "Uses C2",
			Status:         types.CaseStatusOpen,
			ReporterID:     "U-REP",
			SlackChannelID: "C2",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		})
		gt.NoError(t, err).Required()

		got, err := f.uc.ListExistingSlackChannelCandidates(actorContext(), existingChWorkspace)
		gt.NoError(t, err).Required()
		gt.Value(t, got).Equal([]usecase.SlackChannelCandidate{
			{ID: "C1", Name: "alpha", IsPrivate: false},
			{ID: "C4", Name: "beta", IsPrivate: true},
		})
		gt.String(t, gotUser).Equal(existingChActor)
		gt.String(t, gotTeam).Equal("T-HOME")
	})

	t.Run("disabled workspace is rejected without calling Slack", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		called := false
		f.slack.listMemberChannelsFn = func(context.Context, string, string) ([]slack.Channel, error) {
			called = true
			return nil, nil
		}

		_, err := f.uc.ListExistingSlackChannelCandidates(actorContext(), existingChOtherWS)
		gt.Error(t, err).Is(usecase.ErrExistingSlackChannelDisabled)
		gt.Bool(t, called).False()
	})

	t.Run("no auth token is access denied", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		_, err := f.uc.ListExistingSlackChannelCandidates(context.Background(), existingChWorkspace)
		gt.Error(t, err).Is(usecase.ErrAccessDenied)
	})

	t.Run("a Slack failure is returned", func(t *testing.T) {
		f := newExistingChannelFixture(t)
		slackErr := errors.New("ratelimited")
		f.slack.listMemberChannelsFn = func(context.Context, string, string) ([]slack.Channel, error) {
			return nil, slackErr
		}
		_, err := f.uc.ListExistingSlackChannelCandidates(actorContext(), existingChWorkspace)
		gt.Error(t, err).Is(slackErr)
	})
}

func TestCaseUseCase_ExistingSlackChannelAllowed(t *testing.T) {
	type combo struct {
		allow, channelMode, slackWired bool
	}
	for _, c := range []combo{
		{true, true, true},
		{true, true, false},
		{true, false, true},
		{true, false, false},
		{false, true, true},
		{false, true, false},
		{false, false, true},
		{false, false, false},
	} {
		registry := model.NewWorkspaceRegistry()
		mode := model.CaseModeThread
		if c.channelMode {
			mode = model.CaseModeChannel
		}
		registry.Register(&model.WorkspaceEntry{
			Workspace:                 model.Workspace{ID: "ws"},
			CaseMode:                  mode,
			SlackMonitorChannelID:     existingChMonitor,
			AllowExistingSlackChannel: c.allow,
		})
		var svc slack.Service
		if c.slackWired {
			svc = &mockSlackService{}
		}
		uc := usecase.NewCaseUseCase(memory.New(), registry, svc, nil, "")
		want := c.allow && c.channelMode && c.slackWired
		gt.Value(t, uc.ExistingSlackChannelAllowed("ws")).Equal(want)
		gt.Bool(t, uc.ExistingSlackChannelAllowed("ws-unknown")).False()
	}
}
