package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	goslack "github.com/slack-go/slack"
)

func TestNew(t *testing.T) {
	t.Run("returns error when token is empty", func(t *testing.T) {
		_, err := slack.New("")
		gt.Value(t, err).NotNil()
	})

	t.Run("creates service when token is provided", func(t *testing.T) {
		svc, err := slack.New("test-token")
		gt.NoError(t, err).Required()
		gt.Value(t, svc).NotNil()
	})
}

func TestIntegration(t *testing.T) {
	token := os.Getenv("TEST_SLACK_BOT_TOKEN")
	if token == "" {
		t.Skip("TEST_SLACK_BOT_TOKEN is not set")
	}

	ctx := context.Background()

	svc, err := slack.New(token)
	gt.NoError(t, err).Required()

	// Fetch channels and users once to avoid repeated API calls and rate limiting
	channels, err := svc.ListJoinedChannels(ctx, "")
	gt.NoError(t, err).Required()

	users, err := svc.ListUsers(ctx, "")
	gt.NoError(t, err).Required()

	t.Run("ListJoinedChannels returns channels", func(t *testing.T) {
		if len(channels) == 0 {
			t.Log("Warning: bot is not joined to any channels")
		}

		for _, ch := range channels {
			gt.String(t, ch.ID).NotEqual("")
			gt.String(t, ch.Name).NotEqual("")
			t.Logf("Found channel: %s (%s)", ch.Name, ch.ID)
		}
	})

	t.Run("GetChannelNames resolves channel names", func(t *testing.T) {
		if len(channels) == 0 {
			t.Skip("No channels available to test GetChannelNames")
		}

		channelIDs := []string{channels[0].ID}
		names, err := svc.GetChannelNames(ctx, channelIDs)
		gt.NoError(t, err).Required()

		gt.Map(t, names).HasKey(channels[0].ID)

		name := names[channels[0].ID]
		gt.String(t, name).NotEqual("")
		gt.Value(t, name).Equal(channels[0].Name)

		t.Logf("Resolved channel name: %s -> %s", channels[0].ID, name)
	})

	t.Run("GetChannelNames handles multiple channels", func(t *testing.T) {
		if len(channels) < 2 {
			t.Skip("Need at least 2 channels to test multiple channel resolution")
		}

		channelIDs := []string{channels[0].ID, channels[1].ID}
		names, err := svc.GetChannelNames(ctx, channelIDs)
		gt.NoError(t, err).Required()

		gt.Number(t, len(names)).Equal(2)

		for _, ch := range channels[:2] {
			gt.Map(t, names).HasKey(ch.ID)
			gt.Value(t, names[ch.ID]).Equal(ch.Name)
		}
	})

	t.Run("GetChannelNames handles non-existent channel gracefully", func(t *testing.T) {
		channelIDs := []string{"C00000FAKE"}
		names, err := svc.GetChannelNames(ctx, channelIDs)
		// This may or may not error depending on API behavior
		// The important thing is it doesn't panic
		t.Logf("GetChannelNames with fake ID: names=%v, err=%v", names, err)
	})

	t.Run("GetChannelNames with empty slice returns empty map", func(t *testing.T) {
		names, err := svc.GetChannelNames(ctx, []string{})
		gt.NoError(t, err).Required()
		gt.Number(t, len(names)).Equal(0)
	})

	t.Run("ListUsers returns users", func(t *testing.T) {
		gt.Number(t, len(users)).GreaterOrEqual(1)

		for _, u := range users {
			gt.String(t, u.ID).NotEqual("")
		}

		t.Logf("Total users retrieved: %d", len(users))
	})

	t.Run("GetUserInfo returns user info", func(t *testing.T) {
		if len(users) == 0 {
			t.Skip("No users available to test GetUserInfo")
		}

		user, err := svc.GetUserInfo(ctx, users[0].ID)
		gt.NoError(t, err).Required()

		gt.Value(t, user.ID).Equal(users[0].ID)
		t.Logf("Got user info: %s (%s)", user.RealName, user.ID)
	})
}

func TestWrapSlackViewError(t *testing.T) {
	t.Run("plain error only carries the trigger_id", func(t *testing.T) {
		base := errors.New("network blew up")
		wrapped := slack.WrapSlackViewErrorForTest(base, "failed to open Slack modal view", "trig-1")

		var ge *goerr.Error
		gt.Bool(t, errors.As(wrapped, &ge)).True()
		values := ge.Values()
		gt.Value(t, values["trigger_id"]).Equal("trig-1")
		// Plain errors must NOT pretend to surface Slack metadata.
		_, hasSlackError := values["slack_error"]
		gt.Bool(t, hasSlackError).False()
	})

	t.Run("SlackErrorResponse surfaces response metadata", func(t *testing.T) {
		errMsg := "validation error on element initial_value"
		se := goslack.SlackErrorResponse{
			Err: "invalid_arguments",
			Errors: []goslack.SlackResponseErrors{
				{Message: &errMsg},
			},
			ResponseMetadata: goslack.ResponseMetadata{
				Messages: []string{"[ERROR] failed to match schema [json-pointer:/view/blocks/1/element/initial_value]"},
				Warnings: []string{"deprecated_block_kit"},
			},
		}
		wrapped := slack.WrapSlackViewErrorForTest(fmt.Errorf("openView: %w", se), "failed to open Slack modal view", "trig-2")

		var ge *goerr.Error
		gt.Bool(t, errors.As(wrapped, &ge)).True()
		values := ge.Values()
		gt.Value(t, values["trigger_id"]).Equal("trig-2")
		gt.Value(t, values["slack_error"]).Equal("invalid_arguments")

		msgs, ok := values["slack_response_messages"].([]string)
		gt.Bool(t, ok).True()
		gt.Array(t, msgs).Length(1).Required()
		gt.String(t, msgs[0]).Contains("json-pointer:/view/blocks/1/element/initial_value")

		warns, ok := values["slack_response_warnings"].([]string)
		gt.Bool(t, ok).True()
		gt.Array(t, warns).Length(1).Required()
		gt.String(t, warns[0]).Equal("deprecated_block_kit")

		// The Errors slice itself is surfaced so the per-failure detail
		// (e.g. apps.manifest field paths) reaches Sentry without a replay.
		errs, ok := values["slack_response_errors"].([]goslack.SlackResponseErrors)
		gt.Bool(t, ok).True()
		gt.Array(t, errs).Length(1).Required()
		gt.Value(t, errs[0].Message).NotNil()
		gt.String(t, *errs[0].Message).Equal(errMsg)
	})
}

func TestResolveDisplayName(t *testing.T) {
	t.Run("prefers Profile.DisplayName when present", func(t *testing.T) {
		u := goslack.User{
			Name:     "alice",
			RealName: "Alice The Real",
			Profile: goslack.UserProfile{
				DisplayName: "Alice In Wonderland",
				RealName:    "Alice Profile Real",
			},
		}
		gt.String(t, slack.ResolveDisplayNameForTest(u)).Equal("Alice In Wonderland")
	})

	t.Run("falls back to Profile.RealName when DisplayName is empty", func(t *testing.T) {
		u := goslack.User{
			Name:     "alice",
			RealName: "Alice The Real",
			Profile: goslack.UserProfile{
				DisplayName: "",
				RealName:    "Alice Profile Real",
			},
		}
		gt.String(t, slack.ResolveDisplayNameForTest(u)).Equal("Alice Profile Real")
	})

	t.Run("falls back to top-level RealName when both profile fields are empty", func(t *testing.T) {
		u := goslack.User{
			Name:     "alice",
			RealName: "Alice The Real",
			Profile: goslack.UserProfile{
				DisplayName: "",
				RealName:    "",
			},
		}
		gt.String(t, slack.ResolveDisplayNameForTest(u)).Equal("Alice The Real")
	})

	t.Run("returns empty string when every name field is empty", func(t *testing.T) {
		u := goslack.User{
			Name:     "alice",
			RealName: "",
			Profile: goslack.UserProfile{
				DisplayName: "",
				RealName:    "",
			},
		}
		gt.String(t, slack.ResolveDisplayNameForTest(u)).Equal("")
	})
}

// inviteFake captures the user IDs the slack-go client tried to invite and
// returns a configurable per-user response. The fake speaks the
// conversations.invite contract: a JSON body with "ok" and optional
// "error" string. Slack's real endpoint is atomic across the supplied
// user list — we mimic that by failing the entire request if its single
// user_id appears in the failingUsers set.
type inviteFake struct {
	mu            sync.Mutex
	failingUsers  map[string]string // userID -> slack error code
	successfulIDs []string          // user IDs that received an "ok" response
	calls         int
}

func (f *inviteFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		users := r.FormValue("users")
		if errCode, bad := f.failingUsers[users]; bad {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": errCode})
			return
		}
		f.successfulIDs = append(f.successfulIDs, users)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "C1"}})
	}
}

// A channel the bot cannot see is an expected "no name"; any other failure
// must be reported, not silently dropped.
func TestGetChannelNames_ReportsFailuresOtherThanUnavailableChannels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		gt.NoError(t, r.ParseForm()).Required()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("channel") {
		case "C1":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C1","name":"general"}}`))
		case "C2":
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		case "C4":
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	var logs bytes.Buffer
	ctx := logging.With(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))

	names, err := svc.GetChannelNames(ctx, []string{"C1", "C2", "C3", "C4"})
	gt.NoError(t, err).Required()
	gt.Equal(t, names, map[string]string{"C1": "general"})

	// Only the unexpected failure (C3) is reported; channel_not_found (C2)
	// and not_in_channel (C4) are expected outcomes.
	out := logs.String()
	gt.Number(t, strings.Count(out, "failed to resolve Slack channel name")).Equal(1)
	gt.String(t, out).Contains(`"C3"`)
	gt.Bool(t, strings.Contains(out, `"C2"`)).False()
	gt.Bool(t, strings.Contains(out, `"C4"`)).False()
}

func TestListMemberChannels(t *testing.T) {
	var requests []url.Values
	rateLimitedOnce := false
	mux := http.NewServeMux()
	mux.HandleFunc("/users.conversations", func(w http.ResponseWriter, r *http.Request) {
		gt.NoError(t, r.ParseForm()).Required()
		if !rateLimitedOnce {
			rateLimitedOnce = true
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		requests = append(requests, r.Form)
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C1","name":"general","is_private":false}],"response_metadata":{"next_cursor":"next-page"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"G2","name":"incident-payroll","is_private":true}],"response_metadata":{"next_cursor":""}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	channels, err := svc.ListMemberChannels(context.Background(), "U_ACTOR", "T_TEAM")
	gt.NoError(t, err).Required()
	gt.Value(t, channels).Equal([]slack.Channel{
		{ID: "C1", Name: "general", IsPrivate: false},
		{ID: "G2", Name: "incident-payroll", IsPrivate: true},
	})

	// The rate-limited attempt is retried, then two pages are fetched.
	gt.Bool(t, rateLimitedOnce).True()
	gt.Array(t, requests).Length(2).Required()
	for _, form := range requests {
		gt.String(t, form.Get("user")).Equal("U_ACTOR")
		gt.String(t, form.Get("team_id")).Equal("T_TEAM")
		gt.String(t, form.Get("types")).Equal("public_channel,private_channel")
		gt.String(t, form.Get("exclude_archived")).Equal("true")
	}
	gt.String(t, requests[1].Get("cursor")).Equal("next-page")
}

func TestGetChannelInfo_ClassifiesUnavailableChannels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		gt.NoError(t, r.ParseForm()).Required()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("channel") {
		case "C_OK":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C_OK","name":"general","is_private":true,"is_member":true}}`))
		case "C_GONE":
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()
	ctx := context.Background()

	t.Run("maps is_private and is_member", func(t *testing.T) {
		info, err := svc.GetChannelInfo(ctx, "C_OK")
		gt.NoError(t, err).Required()
		gt.Bool(t, info.IsPrivate).True()
		gt.Bool(t, info.IsMember).True()
	})

	t.Run("channel_not_found is ErrChannelUnavailable", func(t *testing.T) {
		_, err := svc.GetChannelInfo(ctx, "C_GONE")
		gt.Error(t, err).Is(slack.ErrChannelUnavailable)
	})

	t.Run("other Slack errors are not ErrChannelUnavailable", func(t *testing.T) {
		_, err := svc.GetChannelInfo(ctx, "C_BROKEN")
		gt.Value(t, err).NotNil().Required()
		gt.Bool(t, errors.Is(err, slack.ErrChannelUnavailable)).False()
	})
}

func TestJoinChannel(t *testing.T) {
	var joined []string
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.join", func(w http.ResponseWriter, r *http.Request) {
		gt.NoError(t, r.ParseForm()).Required()
		channel := r.Form.Get("channel")
		joined = append(joined, channel)
		w.Header().Set("Content-Type", "application/json")
		switch channel {
		case "C_NEW":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C_NEW"}}`))
		case "C_ALREADY":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C_ALREADY"},"warning":"already_in_channel","response_metadata":{"warnings":["already_in_channel"]}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"method_not_supported_for_channel_type"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()
	ctx := context.Background()

	gt.NoError(t, svc.JoinChannel(ctx, "C_NEW"))
	gt.NoError(t, svc.JoinChannel(ctx, "C_ALREADY"))
	gt.Value(t, svc.JoinChannel(ctx, "G_PRIVATE")).NotNil()
	gt.Value(t, joined).Equal([]string{"C_NEW", "C_ALREADY", "G_PRIVATE"})
}

func TestInviteUsersToChannel_BadUserDoesNotBlockValidUsers(t *testing.T) {
	fake := &inviteFake{
		failingUsers: map[string]string{
			"U_BAD": "user_not_found",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.invite", fake.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	err = svc.InviteUsersToChannel(context.Background(), "C_TARGET", []string{"U_GOOD_1", "U_BAD", "U_GOOD_2"})

	// The function must return an error so errutil.Handle on the caller
	// records the failure, but the valid users MUST still have been
	// invited individually.
	gt.Value(t, err).NotNil()

	var ge *goerr.Error
	gt.Bool(t, errors.As(err, &ge)).True().Required()
	values := ge.Values()
	gt.Value(t, values["channel_id"]).Equal("C_TARGET")

	failed, ok := values["failed_user_ids"].([]string)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, failed).Length(1).Required()
	gt.String(t, failed[0]).Equal("U_BAD")

	reasons, ok := values["failure_reasons"].([]string)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, reasons).Length(1).Required()
	gt.String(t, reasons[0]).Contains("user_not_found")

	// Three invite calls were issued (one per user), not one batch.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	gt.Number(t, fake.calls).Equal(3)
	gt.Array(t, fake.successfulIDs).Length(2).Required()
	gt.String(t, fake.successfulIDs[0]).Equal("U_GOOD_1")
	gt.String(t, fake.successfulIDs[1]).Equal("U_GOOD_2")
}

func TestInviteUsersToChannel_AllSuccess_ReturnsNil(t *testing.T) {
	fake := &inviteFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.invite", fake.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	err = svc.InviteUsersToChannel(context.Background(), "C_TARGET", []string{"U_A", "U_B"})
	gt.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	gt.Number(t, fake.calls).Equal(2)
}

func TestInviteUsersToChannel_EmptyList_NoApiCall(t *testing.T) {
	fake := &inviteFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.invite", fake.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	err = svc.InviteUsersToChannel(context.Background(), "C_TARGET", nil)
	gt.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	gt.Number(t, fake.calls).Equal(0)
}

// formCaptor records, per Slack API path, the form values of the last request
// it received and answers "ok". It lets the unfurl test assert what actually
// went on the wire for each chat.* endpoint.
type formCaptor struct {
	mu     sync.Mutex
	values map[string]url.Values
}

func (c *formCaptor) handler(responseBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		c.mu.Lock()
		if c.values == nil {
			c.values = map[string]url.Values{}
		}
		// r.Form is repopulated per request; copy the path's values out.
		c.values[r.URL.Path] = r.Form
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}
}

func (c *formCaptor) get(path string) url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[path]
}

// TestBotPostsDisableUnfurl pins the invariant that every bot-originated post /
// update / ephemeral message suppresses Slack link & media unfurling. The bot
// embeds permalinks and URLs everywhere; preview cards would bury the content.
func TestBotPostsDisableUnfurl(t *testing.T) {
	captor := &formCaptor{}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", captor.handler(`{"ok":true,"channel":"C","ts":"1700000000.000100"}`))
	mux.HandleFunc("/chat.update", captor.handler(`{"ok":true,"channel":"C","ts":"1700000000.000100"}`))
	mux.HandleFunc("/chat.postEphemeral", captor.handler(`{"ok":true,"message_ts":"1700000000.000100"}`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	ctx := context.Background()
	blocks := []goslack.Block{goslack.NewSectionBlock(goslack.NewTextBlockObject(goslack.MarkdownType, "hello", false, false), nil, nil)}
	att := goslack.Attachment{Text: "att"}

	assertUnfurlDisabled := func(t *testing.T, path string) {
		t.Helper()
		vals := captor.get(path)
		gt.Value(t, vals).NotNil().Required()
		gt.String(t, vals.Get("unfurl_links")).Equal("false")
		gt.String(t, vals.Get("unfurl_media")).Equal("false")
	}

	t.Run("PostMessage", func(t *testing.T) {
		_, err := svc.PostMessage(ctx, "C", blocks, "fallback")
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postMessage")
	})

	t.Run("PostThreadReply", func(t *testing.T) {
		_, err := svc.PostThreadReply(ctx, "C", "1700000000.000001", "reply")
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postMessage")
	})

	t.Run("PostThreadMessage", func(t *testing.T) {
		_, err := svc.PostThreadMessage(ctx, "C", "1700000000.000001", blocks, "final")
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postMessage")
	})

	t.Run("PostMessageWithAttachments", func(t *testing.T) {
		_, err := svc.PostMessageWithAttachments(ctx, "C", "fallback", []goslack.Attachment{att})
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postMessage")
	})

	t.Run("PostMessageWithAttachment", func(t *testing.T) {
		_, err := svc.PostMessageWithAttachment(ctx, "C", "fallback", att)
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postMessage")
	})

	t.Run("UpdateMessage", func(t *testing.T) {
		err := svc.UpdateMessage(ctx, "C", "1700000000.000100", blocks, "")
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.update")
	})

	t.Run("UpdateMessageWithAttachments", func(t *testing.T) {
		err := svc.UpdateMessageWithAttachments(ctx, "C", "1700000000.000100", "", []goslack.Attachment{att})
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.update")
	})

	t.Run("PostEphemeral", func(t *testing.T) {
		err := svc.PostEphemeral(ctx, "C", "U", "ephemeral")
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postEphemeral")
	})

	t.Run("PostEphemeralBlocks", func(t *testing.T) {
		_, err := svc.PostEphemeralBlocks(ctx, "C", "U", blocks, "ephemeral")
		gt.NoError(t, err).Required()
		assertUnfurlDisabled(t, "/chat.postEphemeral")
	})
}

// An ephemeral message without thread_ts lands in the channel, where a user
// looking at a thread pane never sees it; WithEphemeralThread must place it
// in the thread, and omitting the option (or passing an empty ts) must not.
func TestPostEphemeral_Thread(t *testing.T) {
	captor := &formCaptor{}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postEphemeral", captor.handler(`{"ok":true,"message_ts":"1700000000.000100"}`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()
	ctx := context.Background()

	t.Run("with thread", func(t *testing.T) {
		gt.NoError(t, svc.PostEphemeral(ctx, "C1", "U1", "notice", slack.WithEphemeralThread("1700000000.000001"))).Required()
		vals := captor.get("/chat.postEphemeral")
		gt.Value(t, vals).NotNil().Required()
		gt.String(t, vals.Get("channel")).Equal("C1")
		gt.String(t, vals.Get("user")).Equal("U1")
		gt.String(t, vals.Get("text")).Equal("notice")
		gt.String(t, vals.Get("thread_ts")).Equal("1700000000.000001")
	})

	t.Run("without option", func(t *testing.T) {
		gt.NoError(t, svc.PostEphemeral(ctx, "C1", "U1", "notice")).Required()
		vals := captor.get("/chat.postEphemeral")
		gt.Value(t, vals).NotNil().Required()
		gt.Bool(t, vals.Has("thread_ts")).False()
	})

	t.Run("empty thread ts", func(t *testing.T) {
		gt.NoError(t, svc.PostEphemeral(ctx, "C1", "U1", "notice", slack.WithEphemeralThread(""))).Required()
		vals := captor.get("/chat.postEphemeral")
		gt.Value(t, vals).NotNil().Required()
		gt.Bool(t, vals.Has("thread_ts")).False()
	})
}

// conversations.replies / conversations.history must surface a message whose
// body lives entirely in attachments (how the GitHub Slack app posts PR
// notifications). Reading only the top-level "text" returned "" for those,
// which is how a Design Doc PR reached the agent as an empty message.
func TestGetConversationRepliesAndHistory_AttachmentOnlyBody(t *testing.T) {
	payload := `{
		"ok": true,
		"messages": [
			{
				"type": "message",
				"subtype": "bot_message",
				"user": "U01UD9VJXQB",
				"text": "",
				"ts": "1785673711.954009",
				"thread_ts": "1785673711.954009",
				"attachments": [
					{
						"pretext": "Pull request opened by octocat",
						"title": "#297 Add a Design Doc",
						"title_link": "https://github.com/example/design-doc/pull/297",
						"text": "Adds a Design Doc.",
						"footer": "example/design-doc"
					}
				]
			},
			{
				"type": "message",
				"user": "U058S2H241M",
				"text": "already implemented",
				"ts": "1785673800.000100",
				"thread_ts": "1785673711.954009"
			}
		]
	}`

	mux := http.NewServeMux()
	for _, path := range []string{"/conversations.replies", "/conversations.history"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := slack.NewWithAPIURLForTest("xoxb-test", srv.URL+"/")
	gt.NoError(t, err).Required()

	wantBody := strings.Join([]string{
		"Pull request opened by octocat",
		"#297 Add a Design Doc",
		"https://github.com/example/design-doc/pull/297",
		"Adds a Design Doc.",
		"example/design-doc",
	}, "\n")

	replies, err := svc.GetConversationReplies(context.Background(), "C05CZJVUS5N", "1785673711.954009", 32)
	gt.NoError(t, err).Required()
	gt.Array(t, replies).Length(2).Required()
	gt.String(t, replies[0].Text).Equal(wantBody)
	gt.String(t, replies[0].UserID).Equal("U01UD9VJXQB")
	gt.String(t, replies[1].Text).Equal("already implemented")

	history, err := svc.GetConversationHistory(context.Background(), "C05CZJVUS5N", time.Unix(0, 0), 32)
	gt.NoError(t, err).Required()
	gt.Array(t, history).Length(2).Required()
	gt.String(t, history[0].Text).Equal(wantBody)
	gt.String(t, history[1].Text).Equal("already implemented")
}
