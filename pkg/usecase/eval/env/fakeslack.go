package env

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	goslack "github.com/slack-go/slack"

	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/evaltype"
)

// botUserID is the fixed bot identity the fake reports. Synthesized inputs use
// distinct user ids so bot-self filters never match the reporter.
const botUserID = "U-EVALBOT"

type threadKey struct{ channelID, threadTS string }

// fakeSlack is an in-memory slack.Service used by the eval harness. It records
// outbound posts (so the driver can recover the agent's questions and replies)
// and never touches the network. It is safe for concurrent use because trace
// updates and parallel sub-agents may post concurrently within one turn.
type fakeSlack struct {
	mu    sync.Mutex
	seq   int
	start time.Time
	posts []evaltype.SlackPost
	// threads holds the seeded thread messages keyed by channel and thread ts, so the Slack
	// read tools return the conversation a scenario declared.
	threads map[threadKey][]slacksvc.ConversationMessage
}

// Receive makes a message visible through Slack reads once its turn arrives.
func (f *fakeSlack) Receive(channelID, threadTS, ts, user, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addReply(channelID, threadTS, ts, user, text)
}

func (f *fakeSlack) addReply(channelID, threadTS, ts, user, text string) {
	key := threadKey{channelID, threadTS}
	f.threads[key] = append(f.threads[key], slacksvc.ConversationMessage{
		UserID: user, UserName: user, Text: text, Timestamp: ts, ThreadTS: threadTS,
	})
}

var _ slacksvc.Service = (*fakeSlack)(nil)

func newFakeSlack(start time.Time) *fakeSlack {
	return &fakeSlack{start: start, threads: make(map[threadKey][]slacksvc.ConversationMessage)}
}

func slackTimestamp(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// NextTimestamp reserves the next timestamp on the scenario's Slack clock.
func (f *fakeSlack) NextTimestamp() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextTimestamp()
}

func (f *fakeSlack) nextTimestamp() string {
	f.seq++
	return slackTimestamp(f.start.Add(time.Second + time.Duration(f.seq)*time.Microsecond))
}

func (f *fakeSlack) record(kind, channelID, threadTS, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ts := f.nextTimestamp()
	f.posts = append(f.posts, evaltype.SlackPost{Kind: kind, ChannelID: channelID, ThreadTS: threadTS, Text: text, TS: ts})
	if threadTS != "" {
		f.addReply(channelID, threadTS, ts, botUserID, text)
	}
	return ts
}

// Posts returns a snapshot of all recorded posts in order.
func (f *fakeSlack) Posts() []evaltype.SlackPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

// --- slack.Service implementation -----------------------------------------

func (f *fakeSlack) PostThreadReply(_ context.Context, channelID, threadTS, text string) (string, error) {
	return f.record("thread_reply", channelID, threadTS, text), nil
}

func (f *fakeSlack) PostThreadMessage(_ context.Context, channelID, threadTS string, _ []goslack.Block, text string, _ ...slacksvc.PostThreadOption) (string, error) {
	return f.record("thread_message", channelID, threadTS, text), nil
}

func (f *fakeSlack) PostMessage(_ context.Context, channelID string, _ []goslack.Block, text string) (string, error) {
	return f.record("message", channelID, "", text), nil
}

func (f *fakeSlack) UpdateMessage(_ context.Context, _ string, _ string, _ []goslack.Block, _ string) error {
	return nil
}

func (f *fakeSlack) GetBotUserID(_ context.Context) (string, error) { return botUserID, nil }

func (f *fakeSlack) GetUserInfo(_ context.Context, userID string) (*slacksvc.User, error) {
	return &slacksvc.User{ID: userID, Name: userID}, nil
}

func (f *fakeSlack) GetConversationMembers(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeSlack) GetConversationReplies(_ context.Context, channelID, threadTS string, limit int) ([]slacksvc.ConversationMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.threads[threadKey{channelID, threadTS}]
	if limit > 0 && limit < len(msgs) {
		msgs = msgs[:limit]
	}
	return slices.Clone(msgs), nil
}

func (f *fakeSlack) GetConversationHistory(_ context.Context, _ string, _ time.Time, _ int) ([]slacksvc.ConversationMessage, error) {
	return nil, nil
}

func (f *fakeSlack) ListJoinedChannels(_ context.Context, _ string) ([]slacksvc.Channel, error) {
	return nil, nil
}

func (f *fakeSlack) ListMemberChannels(_ context.Context, _, _ string) ([]slacksvc.Channel, error) {
	return nil, nil
}

func (f *fakeSlack) JoinChannel(_ context.Context, _ string) error {
	return nil
}

func (f *fakeSlack) GetChannelNames(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = id
	}
	return out, nil
}

func (f *fakeSlack) ListUsers(_ context.Context, _ string) ([]*slacksvc.User, error) { return nil, nil }

func (f *fakeSlack) CreateChannel(_ context.Context, _ int64, caseName, _ string, _ bool, _ string) (string, error) {
	return "C" + caseName, nil
}

func (f *fakeSlack) GetChannelInfo(_ context.Context, channelID string) (*slacksvc.ChannelInfo, error) {
	return &slacksvc.ChannelInfo{ID: channelID, Name: channelID}, nil
}

func (f *fakeSlack) RenameChannel(_ context.Context, _ string, _ int64, _, _ string) error {
	return nil
}

func (f *fakeSlack) InviteUsersToChannel(_ context.Context, _ string, _ []string) error { return nil }

func (f *fakeSlack) AddBookmark(_ context.Context, _, _, _ string) error { return nil }

func (f *fakeSlack) GetTeamURL(_ context.Context) (string, error) { return "https://slack.test/", nil }

func (f *fakeSlack) PostMessageWithAttachment(_ context.Context, channelID, text string, _ goslack.Attachment) (string, error) {
	return f.record("message", channelID, "", text), nil
}

func (f *fakeSlack) PostMessageWithAttachments(_ context.Context, channelID, text string, _ []goslack.Attachment) (string, error) {
	return f.record("message", channelID, "", text), nil
}

func (f *fakeSlack) UpdateMessageWithAttachment(_ context.Context, _, _, _ string, _ goslack.Attachment) error {
	return nil
}

func (f *fakeSlack) UpdateMessageWithAttachments(_ context.Context, _, _, _ string, _ []goslack.Attachment) error {
	return nil
}

func (f *fakeSlack) OpenView(_ context.Context, _ string, _ goslack.ModalViewRequest) error {
	return nil
}

func (f *fakeSlack) UpdateView(_ context.Context, _ goslack.ModalViewRequest, _, _, _ string) error {
	return nil
}

func (f *fakeSlack) ListUserGroups(_ context.Context, _ string) ([]slacksvc.UserGroup, error) {
	return nil, nil
}

func (f *fakeSlack) GetUserGroupMembers(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeSlack) ListTeams(_ context.Context) ([]slacksvc.Team, error) { return nil, nil }

func (f *fakeSlack) PostEphemeral(_ context.Context, _, _, _ string, _ ...slacksvc.PostEphemeralOption) error {
	return nil
}

func (f *fakeSlack) PostEphemeralBlocks(_ context.Context, _, _ string, _ []goslack.Block, _ string) (string, error) {
	return "eval.eph", nil
}

func (f *fakeSlack) GetPermalink(_ context.Context, channelID, ts string) (string, error) {
	return "https://slack.test/" + channelID + "/" + ts, nil
}
