package slacktool_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	goslack "github.com/slack-go/slack"

	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	slackservice "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

// fakeSearchService is a SearchService where SearchMessages can be scripted
// to return a specific result or error.
type fakeSearchService struct {
	searchFn func(ctx context.Context, query string, opts slacktool.SearchOptions) (*slacktool.SearchResult, error)
}

func (f *fakeSearchService) SearchMessages(ctx context.Context, query string, opts slacktool.SearchOptions) (*slacktool.SearchResult, error) {
	return f.searchFn(ctx, query, opts)
}

// fakeBotService stubs slackservice.Service. The embedded interface is nil,
// so any method we don't explicitly override panics with a nil-pointer
// dereference if called — that surfaces unintended dependencies in tests.
type fakeBotService struct {
	slackservice.Service

	getPermalinkFn  func(ctx context.Context, channelID, ts string) (string, error)
	getRepliesFn    func(ctx context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error)
	postMessageFn   func(ctx context.Context, channelID string, blocks []goslack.Block, text string) (string, error)
	postThreadReply func(ctx context.Context, channelID, threadTS, text string) (string, error)
}

// fakeMessageRetriever stubs slacktool.MessageRetriever. The embedded
// interface is nil for the same panic-on-unexpected-call reason as
// fakeBotService.
type fakeMessageRetriever struct {
	slacktool.MessageRetriever

	getRepliesFn func(ctx context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error)
}

func (f *fakeMessageRetriever) GetConversationReplies(ctx context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error) {
	return f.getRepliesFn(ctx, channelID, threadTS, limit)
}

func (f *fakeBotService) GetPermalink(ctx context.Context, channelID, ts string) (string, error) {
	return f.getPermalinkFn(ctx, channelID, ts)
}

func (f *fakeBotService) GetConversationReplies(ctx context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error) {
	return f.getRepliesFn(ctx, channelID, threadTS, limit)
}

func (f *fakeBotService) PostMessage(ctx context.Context, channelID string, blocks []goslack.Block, text string) (string, error) {
	return f.postMessageFn(ctx, channelID, blocks, text)
}

func (f *fakeBotService) PostThreadReply(ctx context.Context, channelID, threadTS, text string) (string, error) {
	return f.postThreadReply(ctx, channelID, threadTS, text)
}

// ctxWithCapturingLogger returns a context whose logger writes JSON to buf.
// Used to assert that errutil.Handle records via the ctx-bound logger.
func ctxWithCapturingLogger(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logging.With(context.Background(), logger), &buf
}

// pickToolByName returns the tool whose Spec().Name matches name, or fails
// the test. Used so tests do not depend on the order of NewReadOnly /
// NewForAssist's return slice.
func pickToolByName(t *testing.T, tools []gollem.Tool, name string) gollem.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Spec().Name == name {
			return tool
		}
	}
	gt.Value(t, tools).Equal(nil) // forces failure with the slice in the message
	return nil
}

func TestSearchMessagesTool_ErrorRoutesThroughErrutilHandle(t *testing.T) {
	search := &fakeSearchService{
		// Mirror search_client.go's behaviour: the upstream client wraps
		// the API error with the query value, so the tool no longer needs
		// to re-wrap it.
		searchFn: func(_ context.Context, query string, _ slacktool.SearchOptions) (*slacktool.SearchResult, error) {
			return nil, goerr.New("upstream failure",
				goerr.V("slack_error", "missing_scope"),
				goerr.V("query", query),
			)
		},
	}

	tools := slacktool.NewReadOnly(slacktool.Deps{Search: search})
	gt.Array(t, tools).Length(1).Required()

	ctx, buf := ctxWithCapturingLogger(t)
	_, err := tools[0].Run(ctx, map[string]any{"query": "incident"})

	gt.Value(t, err).NotNil()

	var ge *goerr.Error
	gt.Bool(t, errors.As(err, &ge)).True().Required()
	values := ge.Values()
	gt.Value(t, values["query"]).Equal("incident")
	gt.Value(t, values["slack_error"]).Equal("missing_scope")

	logged := buf.String()
	gt.String(t, logged).Contains(`"msg":"slack search messages failed"`)
	gt.String(t, logged).Contains(`"slack_error":"missing_scope"`)
}

func TestSearchMessagesTool_SuccessReturnsConvertedResult(t *testing.T) {
	search := &fakeSearchService{
		searchFn: func(_ context.Context, query string, _ slacktool.SearchOptions) (*slacktool.SearchResult, error) {
			gt.String(t, query).Equal("incident")
			return &slacktool.SearchResult{
				Total: 1,
				Messages: []slacktool.SearchMessage{{
					ChannelID: "C111",
					Text:      "incident review",
					Timestamp: "1700000000.000100",
				}},
			}, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Search: search})

	ctx, buf := ctxWithCapturingLogger(t)
	out, err := tools[0].Run(ctx, map[string]any{"query": "incident"})
	gt.NoError(t, err).Required()

	gt.Number(t, out["total"].(int)).Equal(1)
	msgs, ok := out["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(1).Required()
	gt.Value(t, msgs[0]["channel_id"]).Equal("C111")
	gt.Value(t, msgs[0]["text"]).Equal("incident review")

	gt.Number(t, buf.Len()).Equal(0)
}

func TestGetMessagesTool_PermalinkErrorRoutesThroughErrutilHandle(t *testing.T) {
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, _, _ string) (string, error) {
			return "", goerr.New("permalink boom", goerr.V("slack_error", "channel_not_found"))
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot})
	// NewReadOnly returns getMessagesTool only (search is nil), pick last.
	gt.Array(t, tools).Length(1).Required()

	ctx, buf := ctxWithCapturingLogger(t)
	out, err := tools[0].Run(ctx, map[string]any{
		"targets": []any{
			map[string]any{"channel_id": "C111", "ts": "1700.0001"},
		},
	})

	// All targets failed so the tool returns an aggregate error rather
	// than per-target results.
	gt.Value(t, err).NotNil()
	gt.Value(t, out).Nil()

	logged := buf.String()
	gt.String(t, logged).Contains(`"msg":"slack get permalink failed"`)
	gt.String(t, logged).Contains(`"channel_id":"C111"`)
	gt.String(t, logged).Contains(`"slack_error":"channel_not_found"`)
}

func TestGetMessagesTool_PrefersRetrieverWhenSet(t *testing.T) {
	// When a MessageRetriever (User token) is wired alongside the Bot, the tool
	// must call retriever.GetConversationReplies, not bot.GetConversationReplies.
	// This is the whole point of the User-token route: a bot that has not been
	// invited to a public channel still gets messages back instead of
	// not_in_channel.
	retrieverCalled := 0
	botRepliesCalled := 0
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, _ string, _ int) ([]slackservice.ConversationMessage, error) {
			botRepliesCalled++
			return nil, errors.New("bot replies must not be called when retriever is set")
		},
	}
	retriever := &fakeMessageRetriever{
		getRepliesFn: func(_ context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error) {
			retrieverCalled++
			gt.String(t, channelID).Equal("C_PUB")
			gt.String(t, threadTS).Equal("t1")
			gt.Number(t, limit).Equal(20)
			return []slackservice.ConversationMessage{{UserID: "U1", Text: "via user token", Timestamp: threadTS}}, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot, Retriever: retriever})

	out, err := tools[0].Run(context.Background(), map[string]any{
		"targets": []any{map[string]any{"channel_id": "C_PUB", "ts": "t1"}},
	})
	gt.NoError(t, err).Required()
	gt.Number(t, retrieverCalled).Equal(1)
	gt.Number(t, botRepliesCalled).Equal(0)

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, results).Length(1).Required()
	gt.Value(t, results[0]["channel_id"]).Equal("C_PUB")
	msgs, ok := results[0]["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(1).Required()
	gt.Value(t, msgs[0]["text"]).Equal("via user token")
}

func TestGetMessagesTool_FallsBackToBotWhenRetrieverNil(t *testing.T) {
	// Backward-compat path: without a Retriever, the tool must still work via
	// the Bot token (the old behaviour). public/private channels the bot has
	// joined remain readable; channels it has not joined still fail with
	// not_in_channel (Slack-side constraint, not our concern here).
	botCalled := 0
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error) {
			botCalled++
			gt.String(t, channelID).Equal("C_BOT_MEMBER")
			gt.String(t, threadTS).Equal("t9")
			gt.Number(t, limit).Equal(20)
			return []slackservice.ConversationMessage{{UserID: "U9", Text: "via bot token", Timestamp: threadTS}}, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot})

	out, err := tools[0].Run(context.Background(), map[string]any{
		"targets": []any{map[string]any{"channel_id": "C_BOT_MEMBER", "ts": "t9"}},
	})
	gt.NoError(t, err).Required()
	gt.Number(t, botCalled).Equal(1)

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, results).Length(1).Required()
	msgs, ok := results[0]["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(1).Required()
	gt.Value(t, msgs[0]["text"]).Equal("via bot token")
}

func TestGetMessagesTool_PartialFailureLogsButReturnsResults(t *testing.T) {
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			if channelID == "C_BAD" {
				return "", goerr.New("not found")
			}
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, _ string, _ int) ([]slackservice.ConversationMessage, error) {
			return []slackservice.ConversationMessage{{
				UserID: "U1", Text: "hello", Timestamp: "1700.0001",
			}}, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot})

	ctx, buf := ctxWithCapturingLogger(t)
	out, err := tools[0].Run(ctx, map[string]any{
		"targets": []any{
			map[string]any{"channel_id": "C_BAD", "ts": "t1"},
			map[string]any{"channel_id": "C_OK", "ts": "t2"},
		},
	})
	gt.NoError(t, err).Required()

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, results).Length(2).Required()

	// First target failed: error string surfaced on the per-target map.
	gt.Value(t, results[0]["channel_id"]).Equal("C_BAD")
	gt.Value(t, results[0]["error"]).NotEqual(nil)

	// Second target succeeded.
	gt.Value(t, results[1]["channel_id"]).Equal("C_OK")
	_, hasErr := results[1]["error"]
	gt.Bool(t, hasErr).False()

	// errutil.Handle was invoked for the failure; logger captured it.
	gt.Bool(t, strings.Contains(buf.String(), "slack get permalink failed")).True()

	// Drain the small async sleep ticker many tools rely on; nothing
	// important here, but keeps lint-style waitgroup hygiene tidy.
	_ = time.Millisecond
}

// searchToolWith returns the slack__search_messages tool built over a scripted
// SearchResult and the given size bounds.
func searchToolWith(t *testing.T, limits slacktool.Limits, res *slacktool.SearchResult) gollem.Tool {
	t.Helper()
	search := &fakeSearchService{
		searchFn: func(_ context.Context, _ string, _ slacktool.SearchOptions) (*slacktool.SearchResult, error) {
			return res, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Search: search, Limits: limits})
	return pickToolByName(t, tools, "slack__search_messages")
}

func TestSearchMessagesTool_UnboundedByDefault(t *testing.T) {
	// The zero Limits is what a caller that wires no bounds gets. It must leave
	// the result exactly as it was before the bounds existed, so a deployment
	// that has not adopted the flags sees no behaviour change.
	long := strings.Repeat("a", 10000)
	tool := searchToolWith(t, slacktool.Limits{}, &slacktool.SearchResult{
		Total:    1,
		Messages: []slacktool.SearchMessage{{ChannelID: "C1", Text: long, Timestamp: "1700.0001"}},
	})

	out, err := tool.Run(context.Background(), map[string]any{"query": "incident"})
	gt.NoError(t, err).Required()

	msgs, ok := out["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(1).Required()
	gt.Value(t, msgs[0]["text"]).Equal(long)
	_, marked := msgs[0]["text_truncated"]
	gt.Bool(t, marked).False()
	gt.Value(t, out["truncated"]).Equal(false)
	gt.Value(t, out["returned"]).Equal(1)
	gt.Value(t, out["omitted"]).Equal(0)
}

func TestSearchMessagesTool_LongTextIsCutAndMarked(t *testing.T) {
	// A single long message is what drives an outlier call, and the model
	// cannot ask for a shorter one — only for fewer results. The cut must be
	// visible in the entry so the agent does not read a partial message as the
	// whole of what someone wrote.
	tool := searchToolWith(t, slacktool.Limits{MaxTextBytes: 10}, &slacktool.SearchResult{
		Total: 2,
		Messages: []slacktool.SearchMessage{
			{ChannelID: "C1", Text: "0123456789abcdef", Timestamp: "1700.0001"},
			{ChannelID: "C2", Text: "short", Timestamp: "1700.0002"},
		},
	})

	out, err := tool.Run(context.Background(), map[string]any{"query": "incident"})
	gt.NoError(t, err).Required()

	msgs, ok := out["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(2).Required()

	gt.Value(t, msgs[0]["text"]).Equal("0123456789")
	gt.Value(t, msgs[0]["text_truncated"]).Equal(true)

	// A message under the cap is untouched and carries no marker.
	gt.Value(t, msgs[1]["text"]).Equal("short")
	_, marked := msgs[1]["text_truncated"]
	gt.Bool(t, marked).False()

	// Cutting text is not dropping messages: the call is still complete.
	gt.Value(t, out["truncated"]).Equal(false)
	gt.Value(t, out["omitted"]).Equal(0)
}

func TestSearchMessagesTool_TextIsCutOnARuneBoundary(t *testing.T) {
	// The cut reaches the model, the run timeline and the trace archive, so a
	// broken rune would corrupt all three. 8 bytes lands mid-character in a
	// 3-byte-per-rune string, so the cut backs off to the last whole rune that
	// fits — 6 bytes, not a third character split across the boundary.
	tool := searchToolWith(t, slacktool.Limits{MaxTextBytes: 8}, &slacktool.SearchResult{
		Total:    1,
		Messages: []slacktool.SearchMessage{{ChannelID: "C1", Text: "ありがとう", Timestamp: "1700.0001"}},
	})

	out, err := tool.Run(context.Background(), map[string]any{"query": "incident"})
	gt.NoError(t, err).Required()

	msgs, ok := out["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	text, ok := msgs[0]["text"].(string)
	gt.Bool(t, ok).True().Required()
	gt.Bool(t, utf8.ValidString(text)).True()
	gt.String(t, text).Equal("あり")
	gt.Number(t, len(text)).LessOrEqual(8)
}

func TestSearchMessagesTool_ResultSizeCapDropsAndReportsMessages(t *testing.T) {
	// The per-message cap does not bound a call that returns many short
	// messages, which is the other half of what an operator has no control
	// over: `count` is a tool parameter the model fills in.
	msgs := make([]slacktool.SearchMessage, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, slacktool.SearchMessage{
			ChannelID: "C1",
			Text:      strings.Repeat("x", 100),
			Timestamp: "1700.0001",
		})
	}
	tool := searchToolWith(t, slacktool.Limits{MaxResultBytes: 600}, &slacktool.SearchResult{
		Total: 20, Messages: msgs,
	})

	out, err := tool.Run(context.Background(), map[string]any{"query": "incident"})
	gt.NoError(t, err).Required()

	got, ok := out["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()

	// Each encoded entry is over 100 bytes and none is over 600, so the budget
	// admits at least one and stops well short of all 20.
	gt.Number(t, len(got)).GreaterOrEqual(1)
	gt.Number(t, len(got)).LessOrEqual(4)

	gt.Value(t, out["truncated"]).Equal(true)
	gt.Value(t, out["returned"]).Equal(len(got))
	gt.Value(t, out["omitted"]).Equal(20 - len(got))
	// `total` keeps reporting what Slack matched, not what survived the cap.
	gt.Value(t, out["total"]).Equal(20)
}

func TestSearchMessagesTool_AnOversizedFirstMessageIsStillReturned(t *testing.T) {
	// With the per-message cap disabled and the per-call one on, one message can
	// exceed the whole budget. Dropping it would return an empty result the
	// agent cannot get past: asking for fewer results puts the same message
	// first again.
	tool := searchToolWith(t, slacktool.Limits{MaxResultBytes: 100}, &slacktool.SearchResult{
		Total: 2,
		Messages: []slacktool.SearchMessage{
			{ChannelID: "C1", Text: strings.Repeat("z", 500), Timestamp: "1700.0001"},
			{ChannelID: "C2", Text: "short", Timestamp: "1700.0002"},
		},
	})

	out, err := tool.Run(context.Background(), map[string]any{"query": "incident"})
	gt.NoError(t, err).Required()

	msgs, ok := out["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(1).Required()
	gt.Value(t, msgs[0]["channel_id"]).Equal("C1")
	gt.Value(t, msgs[0]["text"]).Equal(strings.Repeat("z", 500))

	// The floor is one entry, not a licence to ignore the budget: the second
	// message is still dropped and reported.
	gt.Value(t, out["returned"]).Equal(1)
	gt.Value(t, out["omitted"]).Equal(1)
	gt.Value(t, out["truncated"]).Equal(true)
}

func TestGetMessagesTool_LongTextIsCutAndMarked(t *testing.T) {
	// The same bound applies to slack__get_messages: its thread_limit bounds
	// the number of replies, never their length.
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, threadTS string, _ int) ([]slackservice.ConversationMessage, error) {
			return []slackservice.ConversationMessage{
				{UserID: "U1", Text: "0123456789abcdef", Timestamp: threadTS},
				{UserID: "U2", Text: "short", Timestamp: threadTS},
			}, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot, Limits: slacktool.Limits{MaxTextBytes: 10}})
	tool := pickToolByName(t, tools, "slack__get_messages")

	out, err := tool.Run(context.Background(), map[string]any{
		"targets": []any{map[string]any{"channel_id": "C1", "ts": "t1"}},
	})
	gt.NoError(t, err).Required()

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, results).Length(1).Required()

	msgs, ok := results[0]["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(2).Required()
	gt.Value(t, msgs[0]["text"]).Equal("0123456789")
	gt.Value(t, msgs[0]["text_truncated"]).Equal(true)
	gt.Value(t, msgs[1]["text"]).Equal("short")
	_, marked := msgs[1]["text_truncated"]
	gt.Bool(t, marked).False()

	gt.Value(t, out["truncated"]).Equal(false)
	gt.Value(t, out["omitted"]).Equal(0)
}

func TestGetMessagesTool_ResultSizeCapIsSharedAcrossTargets(t *testing.T) {
	// The bound is on what the CALL returns, so a first target that fills the
	// budget must leave the second one reporting what it lost rather than
	// silently returning an empty thread.
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, threadTS string, _ int) ([]slackservice.ConversationMessage, error) {
			out := make([]slackservice.ConversationMessage, 0, 5)
			for i := 0; i < 5; i++ {
				out = append(out, slackservice.ConversationMessage{
					UserID: "U1", Text: strings.Repeat("y", 100), Timestamp: threadTS,
				})
			}
			return out, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot, Limits: slacktool.Limits{MaxResultBytes: 400}})
	tool := pickToolByName(t, tools, "slack__get_messages")

	out, err := tool.Run(context.Background(), map[string]any{
		"targets": []any{
			map[string]any{"channel_id": "C1", "ts": "t1"},
			map[string]any{"channel_id": "C2", "ts": "t2"},
		},
	})
	gt.NoError(t, err).Required()

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, results).Length(2).Required()

	first, ok := results[0]["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	second, ok := results[1]["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()

	// 10 messages of >100 encoded bytes each against a 400-byte budget: some
	// are kept, and the budget runs out before the second target is served in
	// full.
	kept := len(first) + len(second)
	gt.Number(t, kept).GreaterOrEqual(1)
	gt.Number(t, kept).LessOrEqual(3)

	gt.Value(t, out["truncated"]).Equal(true)
	gt.Value(t, out["omitted"]).Equal(10 - kept)

	// The target that lost messages says so; the reporting is per target so
	// the agent knows WHICH thread it is missing.
	gt.Value(t, results[1]["omitted"]).Equal(5 - len(second))

	// The budget is charged in target order, so the same request returns the
	// same messages on every run.
	gt.Number(t, len(first)).GreaterOrEqual(len(second))
}

func TestGetMessagesTool_FailedTargetIsNotChargedToTheBudget(t *testing.T) {
	// A failed target carries "error" and no messages. The budget must skip it
	// rather than treat the absent list as an empty one it has to annotate.
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			if channelID == "C_BAD" {
				return "", goerr.New("not found")
			}
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, threadTS string, _ int) ([]slackservice.ConversationMessage, error) {
			return []slackservice.ConversationMessage{{UserID: "U1", Text: "hello", Timestamp: threadTS}}, nil
		},
	}
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot, Limits: slacktool.Limits{MaxResultBytes: 4096}})
	tool := pickToolByName(t, tools, "slack__get_messages")

	ctx, _ := ctxWithCapturingLogger(t)
	out, err := tool.Run(ctx, map[string]any{
		"targets": []any{
			map[string]any{"channel_id": "C_BAD", "ts": "t1"},
			map[string]any{"channel_id": "C_OK", "ts": "t2"},
		},
	})
	gt.NoError(t, err).Required()

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, results).Length(2).Required()

	gt.Value(t, results[0]["error"]).NotEqual(nil)
	_, hasOmitted := results[0]["omitted"]
	gt.Bool(t, hasOmitted).False()

	msgs, ok := results[1]["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, msgs).Length(1).Required()
	gt.Value(t, msgs[0]["text"]).Equal("hello")

	gt.Value(t, out["truncated"]).Equal(false)
	gt.Value(t, out["omitted"]).Equal(0)
}

// threadOf returns a thread of n messages whose texts are "m0" (the root)
// through "m<n-1>" (the newest reply), oldest-first as conversations.replies
// returns them.
func threadOf(n int) []slackservice.ConversationMessage {
	out := make([]slackservice.ConversationMessage, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, slackservice.ConversationMessage{
			UserID: "U1", Text: fmt.Sprintf("m%d", i), Timestamp: fmt.Sprintf("1700000000.%06d", i),
		})
	}
	return out
}

func textsOf(t *testing.T, result map[string]any) []string {
	t.Helper()
	msgs, ok := result["messages"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m["text"].(string))
	}
	return out
}

func TestGetMessagesTool_OrderLatestKeepsRootAndNewestReplies(t *testing.T) {
	// A thread longer than thread_limit: the default order would return its
	// first messages, order=latest returns the root and the last replies —
	// the part a progress check needs to see.
	var requested int
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, _ string, limit int) ([]slackservice.ConversationMessage, error) {
			requested = limit
			return threadOf(10), nil
		},
	}
	tool := pickToolByName(t, slacktool.NewReadOnly(slacktool.Deps{Bot: bot}), "slack__get_messages")

	out, err := tool.Run(context.Background(), map[string]any{
		"targets":      []any{map[string]any{"channel_id": "C1", "ts": "1700000000.000000"}},
		"thread_limit": 4,
		"order":        "latest",
	})
	gt.NoError(t, err).Required()

	// Slack returns oldest-first up to limit, so the tail can only be had by
	// asking for a wider window than thread_limit.
	gt.Number(t, requested).Equal(1000)

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Value(t, textsOf(t, results[0])).Equal([]string{"m0", "m7", "m8", "m9"})
	gt.Value(t, out["truncated"]).Equal(false)
}

func TestGetMessagesTool_OrderOldestIsTheDefault(t *testing.T) {
	var requested int
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, _ string, limit int) ([]slackservice.ConversationMessage, error) {
			requested = limit
			return threadOf(limit), nil
		},
	}
	tool := pickToolByName(t, slacktool.NewReadOnly(slacktool.Deps{Bot: bot}), "slack__get_messages")

	out, err := tool.Run(context.Background(), map[string]any{
		"targets":      []any{map[string]any{"channel_id": "C1", "ts": "1700000000.000000"}},
		"thread_limit": 3,
	})
	gt.NoError(t, err).Required()

	gt.Number(t, requested).Equal(3)
	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	gt.Value(t, textsOf(t, results[0])).Equal([]string{"m0", "m1", "m2"})
}

func TestGetMessagesTool_OrderLatestDropsOldestRepliesOverBudget(t *testing.T) {
	// Over the size bound, order=latest gives up the oldest replies rather
	// than the newest, keeps the root, and still reports what it dropped.
	bot := &fakeBotService{
		getPermalinkFn: func(_ context.Context, channelID, _ string) (string, error) {
			return "https://example.slack.com/archives/" + channelID + "/p1", nil
		},
		getRepliesFn: func(_ context.Context, _, _ string, _ int) ([]slackservice.ConversationMessage, error) {
			return threadOf(10), nil
		},
	}
	// Each entry encodes to roughly 90 bytes, so 300 bytes holds 3 of the 10.
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: bot, Limits: slacktool.Limits{MaxResultBytes: 300}})
	tool := pickToolByName(t, tools, "slack__get_messages")

	out, err := tool.Run(context.Background(), map[string]any{
		"targets":      []any{map[string]any{"channel_id": "C1", "ts": "1700000000.000000"}},
		"thread_limit": 200,
		"order":        "latest",
	})
	gt.NoError(t, err).Required()

	results, ok := out["results"].([]map[string]any)
	gt.Bool(t, ok).True().Required()
	got := textsOf(t, results[0])
	gt.Number(t, len(got)).GreaterOrEqual(2).Required()
	gt.Number(t, len(got)).Less(10).Required()

	gt.Value(t, got[0]).Equal("m0")
	gt.Value(t, got[len(got)-1]).Equal("m9")
	// What is kept is a contiguous tail, oldest-first.
	for i := 1; i < len(got); i++ {
		gt.Value(t, got[i]).Equal(fmt.Sprintf("m%d", 10-len(got)+i))
	}

	gt.Value(t, out["truncated"]).Equal(true)
	gt.Value(t, out["omitted"]).Equal(10 - len(got))
	gt.Value(t, results[0]["omitted"]).Equal(10 - len(got))
}

func TestGetMessagesTool_UnknownOrderIsRejected(t *testing.T) {
	bot := &fakeBotService{}
	tool := pickToolByName(t, slacktool.NewReadOnly(slacktool.Deps{Bot: bot}), "slack__get_messages")

	_, err := tool.Run(context.Background(), map[string]any{
		"targets": []any{map[string]any{"channel_id": "C1", "ts": "1700000000.000000"}},
		"order":   "newest",
	})
	gt.Value(t, err).NotNil()
}
