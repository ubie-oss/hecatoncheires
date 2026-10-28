package slack_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	goslack "github.com/slack-go/slack"
)

// fakeSlackService implements slacksvc.Service with stub conversation/permalink
// methods. Other methods panic if accidentally invoked.
type fakeSlackService struct {
	thread        []slacksvc.ConversationMessage
	threadErr     error
	history       []slacksvc.ConversationMessage
	historyErr    error
	historyOldest time.Time
	historyLimit  int
	permalinkErr  error
}

func (f *fakeSlackService) GetConversationReplies(_ context.Context, _ string, _ string, limit int) ([]slacksvc.ConversationMessage, error) {
	if f.threadErr != nil {
		return nil, f.threadErr
	}
	out := f.thread
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSlackService) GetConversationHistory(_ context.Context, _ string, oldest time.Time, limit int) ([]slacksvc.ConversationMessage, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	f.historyOldest = oldest
	f.historyLimit = limit
	return f.history, nil
}

func (f *fakeSlackService) GetPermalink(_ context.Context, channelID, ts string) (string, error) {
	if f.permalinkErr != nil {
		return "", f.permalinkErr
	}
	return fmt.Sprintf("https://slack.test/%s/%s", channelID, ts), nil
}

// --- unused interface stubs (panic if called) ---

func (f *fakeSlackService) ListJoinedChannels(context.Context, string) ([]slacksvc.Channel, error) {
	panic("unexpected ListJoinedChannels")
}
func (f *fakeSlackService) ListMemberChannels(context.Context, string, string) ([]slacksvc.Channel, error) {
	panic("unexpected ListMemberChannels")
}
func (f *fakeSlackService) JoinChannel(context.Context, string) error {
	panic("unexpected JoinChannel")
}
func (f *fakeSlackService) GetChannelNames(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetChannelNames")
}
func (f *fakeSlackService) GetUserInfo(context.Context, string) (*slacksvc.User, error) {
	panic("unexpected GetUserInfo")
}
func (f *fakeSlackService) ListUsers(context.Context, string) ([]*slacksvc.User, error) {
	panic("unexpected ListUsers")
}
func (f *fakeSlackService) CreateChannel(context.Context, int64, string, string, bool, string) (string, error) {
	panic("unexpected CreateChannel")
}
func (f *fakeSlackService) GetConversationMembers(context.Context, string) ([]string, error) {
	panic("unexpected GetConversationMembers")
}
func (f *fakeSlackService) GetChannelInfo(context.Context, string) (*slacksvc.ChannelInfo, error) {
	panic("unexpected GetChannelInfo")
}
func (f *fakeSlackService) RenameChannel(context.Context, string, int64, string, string) error {
	panic("unexpected RenameChannel")
}
func (f *fakeSlackService) InviteUsersToChannel(context.Context, string, []string) error {
	panic("unexpected InviteUsersToChannel")
}
func (f *fakeSlackService) AddBookmark(context.Context, string, string, string) error {
	panic("unexpected AddBookmark")
}
func (f *fakeSlackService) GetTeamURL(context.Context) (string, error) {
	panic("unexpected GetTeamURL")
}
func (f *fakeSlackService) PostMessageWithAttachment(context.Context, string, string, goslack.Attachment) (string, error) {
	panic("unexpected PostMessageWithAttachment")
}
func (f *fakeSlackService) UpdateMessageWithAttachment(context.Context, string, string, string, goslack.Attachment) error {
	panic("unexpected UpdateMessageWithAttachment")
}
func (f *fakeSlackService) PostMessageWithAttachments(context.Context, string, string, []goslack.Attachment) (string, error) {
	panic("unexpected PostMessageWithAttachments")
}
func (f *fakeSlackService) UpdateMessageWithAttachments(context.Context, string, string, string, []goslack.Attachment) error {
	panic("unexpected UpdateMessageWithAttachments")
}
func (f *fakeSlackService) PostMessage(context.Context, string, []goslack.Block, string) (string, error) {
	panic("unexpected PostMessage")
}
func (f *fakeSlackService) UpdateMessage(context.Context, string, string, []goslack.Block, string) error {
	panic("unexpected UpdateMessage")
}
func (f *fakeSlackService) PostThreadReply(context.Context, string, string, string) (string, error) {
	panic("unexpected PostThreadReply")
}
func (f *fakeSlackService) PostThreadMessage(context.Context, string, string, []goslack.Block, string, ...slacksvc.PostThreadOption) (string, error) {
	panic("unexpected PostThreadMessage")
}
func (f *fakeSlackService) GetBotUserID(context.Context) (string, error) {
	panic("unexpected GetBotUserID")
}
func (f *fakeSlackService) OpenView(context.Context, string, goslack.ModalViewRequest) error {
	panic("unexpected OpenView")
}
func (f *fakeSlackService) UpdateView(context.Context, goslack.ModalViewRequest, string, string, string) error {
	panic("unexpected UpdateView")
}
func (f *fakeSlackService) ListUserGroups(context.Context, string) ([]slacksvc.UserGroup, error) {
	panic("unexpected ListUserGroups")
}
func (f *fakeSlackService) GetUserGroupMembers(context.Context, string) ([]string, error) {
	panic("unexpected GetUserGroupMembers")
}
func (f *fakeSlackService) ListTeams(context.Context) ([]slacksvc.Team, error) {
	panic("unexpected ListTeams")
}
func (f *fakeSlackService) PostEphemeral(context.Context, string, string, string, ...slacksvc.PostEphemeralOption) error {
	panic("unexpected PostEphemeral")
}
func (f *fakeSlackService) PostEphemeralBlocks(context.Context, string, string, []goslack.Block, string) (string, error) {
	panic("unexpected PostEphemeralBlocks")
}

// --- helpers ---

func makeMsgs(n int, prefix string) []slacksvc.ConversationMessage {
	out := make([]slacksvc.ConversationMessage, n)
	for i := range n {
		out[i] = slacksvc.ConversationMessage{
			UserID:    fmt.Sprintf("U%03d", i),
			Text:      fmt.Sprintf("%s msg %d", prefix, i),
			Timestamp: fmt.Sprintf("17000000%02d.000000", i),
		}
	}
	return out
}

func TestCollectThread_BasicChronological(t *testing.T) {
	fake := &fakeSlackService{thread: makeMsgs(5, "thread")}
	c := slacksvc.NewMessageCollector(fake)

	got, err := c.CollectThread(context.Background(), "C1", "1700000000.000000")
	gt.NoError(t, err).Required()
	gt.Array(t, got).Length(5)
	gt.Value(t, got[0].Text).Equal("thread msg 0")
	gt.Value(t, got[4].Text).Equal("thread msg 4")
	// Permalinks are filled in.
	gt.Value(t, got[0].Permalink).Equal("https://slack.test/C1/" + got[0].TS)
}

func TestCollectThread_TrimsToMax(t *testing.T) {
	// Simulate API mistakenly returning more than the limit.
	over := makeMsgs(slacksvc.MaxCollectedMessages+10, "t")
	fake := &fakeSlackService{thread: over}
	c := slacksvc.NewMessageCollector(fake)

	got, err := c.CollectThread(context.Background(), "C1", "1700000000.000000")
	gt.NoError(t, err).Required()
	gt.Array(t, got).Length(slacksvc.MaxCollectedMessages)
	// Must keep the most recent ones.
	gt.Value(t, got[0].Text).Equal("t msg 10")
	gt.Value(t, got[len(got)-1].Text).Equal(fmt.Sprintf("t msg %d", slacksvc.MaxCollectedMessages+10-1))
}

func TestCollectChannelRecent_Window(t *testing.T) {
	fake := &fakeSlackService{history: makeMsgs(3, "history")}
	c := slacksvc.NewMessageCollector(fake)
	mention := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	_, err := c.CollectChannelRecent(context.Background(), "C9", mention)
	gt.NoError(t, err).Required()

	gt.Bool(t, fake.historyOldest.Equal(mention.Add(-slacksvc.ChannelLookbackWindow))).True()
	gt.Number(t, fake.historyLimit).Equal(slacksvc.MaxCollectedMessages)
}

func TestCollectChannelRecent_SortsToChronological(t *testing.T) {
	// Simulate Slack's newest-first ordering.
	newestFirst := []slacksvc.ConversationMessage{
		{Text: "newest", Timestamp: "1700000300.000000"},
		{Text: "middle", Timestamp: "1700000200.000000"},
		{Text: "oldest", Timestamp: "1700000100.000000"},
	}
	fake := &fakeSlackService{history: newestFirst}
	c := slacksvc.NewMessageCollector(fake)

	got, err := c.CollectChannelRecent(context.Background(), "C1", time.Now().UTC())
	gt.NoError(t, err).Required()
	gt.Array(t, got).Length(3)
	gt.Value(t, got[0].Text).Equal("oldest")
	gt.Value(t, got[2].Text).Equal("newest")
}

func TestCollectChannelRecent_TrimsToMax(t *testing.T) {
	over := makeMsgs(slacksvc.MaxCollectedMessages+5, "h")
	fake := &fakeSlackService{history: over}
	c := slacksvc.NewMessageCollector(fake)

	got, err := c.CollectChannelRecent(context.Background(), "C1", time.Now().UTC())
	gt.NoError(t, err).Required()
	gt.Array(t, got).Length(slacksvc.MaxCollectedMessages)
}

func TestCollectThread_PermalinkFailureNonFatal(t *testing.T) {
	fake := &fakeSlackService{
		thread:       makeMsgs(2, "t"),
		permalinkErr: errors.New("transient"),
	}
	c := slacksvc.NewMessageCollector(fake)

	got, err := c.CollectThread(context.Background(), "C1", "1700000000.000000")
	gt.NoError(t, err).Required()
	gt.Array(t, got).Length(2)
	for _, m := range got {
		gt.Value(t, m.Permalink).Equal("")
	}
}

func TestCollectThread_APIError(t *testing.T) {
	fake := &fakeSlackService{threadErr: errors.New("boom")}
	c := slacksvc.NewMessageCollector(fake)

	_, err := c.CollectThread(context.Background(), "C1", "ts")
	gt.Value(t, err).NotNil().Required()
}

func TestCollectChannelRecent_APIError(t *testing.T) {
	fake := &fakeSlackService{historyErr: errors.New("boom")}
	c := slacksvc.NewMessageCollector(fake)

	_, err := c.CollectChannelRecent(context.Background(), "C1", time.Now().UTC())
	gt.Value(t, err).NotNil().Required()
}

func TestIncludesBotMessages(t *testing.T) {
	// Both human and bot messages should be passed through unchanged.
	msgs := []slacksvc.ConversationMessage{
		{UserID: "U_HUMAN", Text: "hi", Timestamp: "1.000001"},
		{UserID: "", UserName: "bot1", Text: "automated alert", Timestamp: "1.000002"},
		{UserID: "U_BOT", Text: "another from a bot user", Timestamp: "1.000003"},
	}
	fake := &fakeSlackService{thread: msgs}
	c := slacksvc.NewMessageCollector(fake)

	got, err := c.CollectThread(context.Background(), "C1", "ts")
	gt.NoError(t, err).Required()
	gt.Array(t, got).Length(3)
	gt.Value(t, got[1].Text).Equal("automated alert")
}
