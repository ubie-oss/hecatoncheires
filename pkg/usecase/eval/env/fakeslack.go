package env

import (
	"context"
	"fmt"
	"sync"
	"time"

	goslack "github.com/slack-go/slack"

	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
)

// botUserID is the fixed bot identity the fake reports. Synthesized inputs use
// distinct user ids so bot-self filters never match the reporter.
const botUserID = "U-EVALBOT"

// Post is one recorded outbound message from the agent.
type Post struct {
	Kind      string // "thread_reply" | "thread_message" | "message"
	ChannelID string
	ThreadTS  string
	Text      string
	TS        string
}

// fakeSlack is an in-memory slack.Service used by the eval harness. It records
// outbound posts (so the driver can recover the agent's questions and replies)
// and never touches the network. It is safe for concurrent use because trace
// updates and parallel sub-agents may post concurrently within one turn.
type fakeSlack struct {
	mu    sync.Mutex
	seq   int
	posts []Post
}

var _ slacksvc.Service = (*fakeSlack)(nil)

func newFakeSlack() *fakeSlack { return &fakeSlack{} }

func (f *fakeSlack) record(kind, channelID, threadTS, text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	ts := fmt.Sprintf("eval.%d", f.seq)
	f.posts = append(f.posts, Post{Kind: kind, ChannelID: channelID, ThreadTS: threadTS, Text: text, TS: ts})
	return ts
}

// Posts returns a snapshot of all recorded posts in order.
func (f *fakeSlack) Posts() []Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Post, len(f.posts))
	copy(out, f.posts)
	return out
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

func (f *fakeSlack) GetConversationReplies(_ context.Context, _ string, _ string, _ int) ([]slacksvc.ConversationMessage, error) {
	return nil, nil
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
