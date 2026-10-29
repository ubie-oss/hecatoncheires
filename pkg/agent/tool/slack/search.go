package slacktool

import (
	"context"
	"fmt"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool"
	slackservice "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"golang.org/x/sync/errgroup"
)

// searchMessagesTool searches Slack messages workspace-wide via search.messages.
// Requires a User OAuth Token with the search:read scope (provided as SearchService).
type searchMessagesTool struct {
	search SearchService
	limits Limits
}

func (t *searchMessagesTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "slack__search_messages",
		Description: "Search Slack messages workspace-wide. Supports Slack search operators such as 'from:@user', 'in:#channel', 'before:YYYY-MM-DD', 'after:YYYY-MM-DD'. Requires a User OAuth Token with search:read scope. Results are size-bounded: a long message carries 'text_truncated', and 'truncated' / 'omitted' report messages dropped to keep the response within its size limit. Those fields say nothing about the 'count' cutoff — compare 'returned' with 'total' (Slack's full match count) to see how many matches were left unread.",
		Parameters: map[string]*gollem.Parameter{
			"query": {
				Type:        gollem.TypeString,
				Description: "Slack search query. Slack search operators are accepted.",
				Required:    true,
			},
			"count": {
				Type:        gollem.TypeInteger,
				Description: "Number of results to return (1-100, default 20).",
				Required:    false,
			},
			"sort": {
				Type:        gollem.TypeString,
				Description: "Sort order: 'score' (relevance, default) or 'timestamp' (newest first).",
				Required:    false,
				Enum:        []string{"score", "timestamp"},
			},
		},
	}
}

func (t *searchMessagesTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	opts := SearchOptions{}
	if v, err := tool.ExtractInt64(args, "count"); err == nil && v > 0 {
		opts.Count = int(v)
	}
	if s, ok := args["sort"].(string); ok {
		opts.Sort = s
	}

	tool.Update(ctx, fmt.Sprintf("Searching Slack: %s", query))

	res, err := t.search.SearchMessages(ctx, query, opts)
	if err != nil {
		errutil.Handle(ctx, err, "slack search messages failed")
		return nil, err
	}

	// Bound what reaches the model context. `count` is a tool parameter, so the
	// model chooses how many results it asks for — but not how long each one is,
	// and neither bound is something an operator could otherwise set.
	budget := newResultBudget(t.limits.MaxResultBytes)
	messages := make([]map[string]any, 0, len(res.Messages))
	for _, m := range res.Messages {
		text, textTruncated := truncateText(m.Text, t.limits.MaxTextBytes)
		entry := map[string]any{
			"channel_id":   m.ChannelID,
			"channel_name": m.ChannelName,
			"user_id":      m.UserID,
			"username":     m.Username,
			"text":         text,
			"ts":           m.Timestamp,
			"permalink":    m.Permalink,
		}
		if textTruncated {
			entry["text_truncated"] = true
		}
		fits, err := budget.admit(entry)
		if err != nil {
			return nil, err
		}
		if !fits {
			break
		}
		messages = append(messages, entry)
	}
	omitted := len(res.Messages) - len(messages)

	// `truncated` / `returned` / `omitted` are always reported, including when
	// nothing was dropped: a result that says so is one the agent can treat as
	// complete, whereas an absent field leaves it guessing.
	//
	// They describe the SIZE bounds only. `count` is a separate cutoff the model
	// itself chose, and `total` (Slack's full match count) is what reveals it —
	// so a call that returns 20 of 500 matches is `truncated: false`, because
	// nothing was dropped to fit a limit.
	return map[string]any{
		"total":     res.Total,
		"messages":  messages,
		"returned":  len(messages),
		"omitted":   omitted,
		"truncated": omitted > 0,
	}, nil
}

// getMessagesTool fetches multiple Slack messages and their thread context in
// parallel. Each target is processed independently; partial failures are returned
// per-target rather than aborting the whole call.
//
// When retriever is set, conversations.replies is called with the User token —
// bot membership is not required for public channels. Otherwise the call falls
// back to the Bot token via slack, which returns not_in_channel when the bot
// is not a member.
type getMessagesTool struct {
	slack     slackservice.Service
	retriever MessageRetriever
	limits    Limits
}

const (
	getMessagesMinTargets   = 1
	getMessagesMaxTargets   = 10
	getMessagesDefaultLimit = 20
	getMessagesMaxLimit     = 200

	// getMessagesLatestFetchCap is how many thread messages order=latest asks
	// Slack for. conversations.replies returns messages oldest-first up to
	// `limit`, so "the latest N" can only be had by fetching a wider window and
	// keeping its tail. 1000 matches pkg/service/slack's threadFetchHardCap,
	// which applies the same rule when collecting a thread for a case draft.
	getMessagesLatestFetchCap = 1000
)

// Thread read orders for slack__get_messages.
const (
	getMessagesOrderOldest = "oldest"
	getMessagesOrderLatest = "latest"
)

func (t *getMessagesTool) Spec() gollem.ToolSpec {
	minTargets := getMessagesMinTargets
	maxTargets := getMessagesMaxTargets
	return gollem.ToolSpec{
		Name:        "slack__get_messages",
		Description: "Fetch one or more Slack messages and their thread context in bulk (max 10 per call). Each target is fetched in parallel; per-target failures are reported in the response without aborting the whole call. Results are size-bounded: a long message carries 'text_truncated', and a target whose messages did not all fit carries 'omitted' alongside the response-level 'truncated'.",
		Parameters: map[string]*gollem.Parameter{
			"targets": {
				Type:        gollem.TypeArray,
				Description: "Array of message references. Each element must contain channel_id and ts.",
				Required:    true,
				MinItems:    &minTargets,
				MaxItems:    &maxTargets,
				Items: &gollem.Parameter{
					Type:        gollem.TypeObject,
					Description: "A Slack message reference.",
					Properties: map[string]*gollem.Parameter{
						"channel_id": {
							Type:        gollem.TypeString,
							Description: "Slack channel ID (e.g. C01234567).",
							Required:    true,
						},
						"ts": {
							Type:        gollem.TypeString,
							Description: "Slack message timestamp (e.g. 1700000000.000100).",
							Required:    true,
						},
					},
				},
			},
			"include_thread": {
				Type:        gollem.TypeBoolean,
				Description: "If true (default), return the full thread when ts is a thread root. If false, return only the message itself.",
				Required:    false,
			},
			"thread_limit": {
				Type:        gollem.TypeInteger,
				Description: "Max replies per thread (default 20, max 200).",
				Required:    false,
			},
			"order": {
				Type:        gollem.TypeString,
				Description: "Which end of a long thread to keep. 'oldest' (default) returns the first thread_limit messages and, when the size limit is hit, drops the newest. 'latest' returns the thread root plus the most recent replies, and drops the oldest replies instead — use it when what happened last matters more than how the thread began. Either way messages are returned oldest-first.",
				Required:    false,
				Enum:        []string{getMessagesOrderOldest, getMessagesOrderLatest},
			},
		},
	}
}

type messageTarget struct {
	channelID string
	ts        string
}

func (t *getMessagesTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	rawTargets, ok := args["targets"].([]any)
	if !ok {
		return nil, fmt.Errorf("targets is required and must be an array")
	}
	if len(rawTargets) < getMessagesMinTargets || len(rawTargets) > getMessagesMaxTargets {
		return nil, fmt.Errorf("targets must contain %d..%d elements, got %d",
			getMessagesMinTargets, getMessagesMaxTargets, len(rawTargets))
	}

	targets := make([]messageTarget, len(rawTargets))
	for i, raw := range rawTargets {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("targets[%d] must be an object", i)
		}
		cid, _ := m["channel_id"].(string)
		ts, _ := m["ts"].(string)
		if cid == "" || ts == "" {
			return nil, fmt.Errorf("targets[%d] requires both channel_id and ts", i)
		}
		targets[i] = messageTarget{channelID: cid, ts: ts}
	}

	includeThread := true
	if v, ok := args["include_thread"].(bool); ok {
		includeThread = v
	}
	threadLimit := getMessagesDefaultLimit
	if v, err := tool.ExtractInt64(args, "thread_limit"); err == nil && v > 0 {
		if v > getMessagesMaxLimit {
			v = getMessagesMaxLimit
		}
		threadLimit = int(v)
	}
	latest := false
	if v, ok := args["order"].(string); ok && v != "" {
		switch v {
		case getMessagesOrderOldest:
		case getMessagesOrderLatest:
			latest = true
		default:
			return nil, fmt.Errorf("order must be %q or %q, got %q",
				getMessagesOrderOldest, getMessagesOrderLatest, v)
		}
	}

	tool.Update(ctx, fmt.Sprintf("Fetching %d Slack message(s)...", len(targets)))

	results := make([]map[string]any, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(len(targets))
	for i, tgt := range targets {
		g.Go(func() error {
			results[i] = t.fetchOne(gctx, tgt, includeThread, threadLimit, latest)
			return nil
		})
	}
	_ = g.Wait()

	successCount := 0
	for _, r := range results {
		if _, hasErr := r["error"]; !hasErr {
			successCount++
		}
	}
	if successCount == 0 {
		return nil, goerr.New("all slack message fetches failed",
			goerr.V("count", len(targets)),
		)
	}

	omitted, err := t.applyResultBudget(results, latest)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"results":   results,
		"omitted":   omitted,
		"truncated": omitted > 0,
	}, nil
}

// applyResultBudget drops the messages that do not fit MaxResultBytes and
// returns how many were dropped across the whole call.
//
// It runs after the fan-out rather than inside fetchOne because the bound is on
// what the CALL returns: a budget shared by the goroutines would hand each
// target a share decided by which one happened to finish first, so the same
// request would return different messages on different runs. Charging the
// results in target order instead makes the outcome depend only on the request.
// A target that keeps none of its messages is reported as such rather than
// silently emptied.
//
// With latest set, each thread is charged root first and then from its newest
// reply backwards, so what runs out of budget is the oldest replies — the end
// order=latest asked to give up. The kept messages stay oldest-first.
func (t *getMessagesTool) applyResultBudget(results []map[string]any, latest bool) (int, error) {
	budget := newResultBudget(t.limits.MaxResultBytes)
	total := 0
	for _, r := range results {
		msgs, ok := r["messages"].([]map[string]any)
		if !ok {
			// A failed target carries "error" and no messages.
			continue
		}
		var kept []map[string]any
		var err error
		if latest {
			kept, err = admitLatest(budget, msgs)
		} else {
			kept, err = admitOldest(budget, msgs)
		}
		if err != nil {
			return 0, err
		}
		dropped := len(msgs) - len(kept)
		if dropped > 0 {
			r["messages"] = kept
			r["omitted"] = dropped
			total += dropped
		}
	}
	return total, nil
}

// admitOldest keeps messages from the start of the thread until the budget
// runs out.
func admitOldest(budget *resultBudget, msgs []map[string]any) ([]map[string]any, error) {
	kept := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		fits, err := budget.admit(m)
		if err != nil {
			return nil, err
		}
		if !fits {
			break
		}
		kept = append(kept, m)
	}
	return kept, nil
}

// admitLatest keeps the thread root, then the newest replies that still fit,
// and returns them oldest-first. The root goes first because it carries what
// the thread is about; without it the latest replies have no subject.
func admitLatest(budget *resultBudget, msgs []map[string]any) ([]map[string]any, error) {
	if len(msgs) == 0 {
		return msgs, nil
	}
	fits, err := budget.admit(msgs[0])
	if err != nil {
		return nil, err
	}
	if !fits {
		return []map[string]any{}, nil
	}
	start := len(msgs)
	for start > 1 {
		fits, err := budget.admit(msgs[start-1])
		if err != nil {
			return nil, err
		}
		if !fits {
			break
		}
		start--
	}
	kept := make([]map[string]any, 0, 1+len(msgs)-start)
	kept = append(kept, msgs[0])
	return append(kept, msgs[start:]...), nil
}

func (t *getMessagesTool) fetchOne(ctx context.Context, tgt messageTarget, includeThread bool, threadLimit int, latest bool) map[string]any {
	out := map[string]any{
		"channel_id": tgt.channelID,
		"ts":         tgt.ts,
	}

	permalink, err := t.slack.GetPermalink(ctx, tgt.channelID, tgt.ts)
	if err != nil {
		opts := []goerr.Option{
			goerr.V("channel_id", tgt.channelID),
			goerr.V("ts", tgt.ts),
		}
		opts = append(opts, slackErrorAttrs(err)...)
		wrapped := goerr.Wrap(err, "failed to get slack permalink", opts...)
		errutil.Handle(ctx, wrapped, "slack get permalink failed")
		out["error"] = err.Error()
		return out
	}
	out["permalink"] = permalink

	limit := threadLimit
	if !includeThread {
		limit = 1
	} else if latest {
		limit = getMessagesLatestFetchCap
	}
	msgs, err := t.fetchReplies(ctx, tgt.channelID, tgt.ts, limit)
	if err != nil {
		opts := []goerr.Option{
			goerr.V("channel_id", tgt.channelID),
			goerr.V("ts", tgt.ts),
			goerr.V("limit", limit),
		}
		opts = append(opts, slackErrorAttrs(err)...)
		wrapped := goerr.Wrap(err, "failed to get slack conversation replies", opts...)
		errutil.Handle(ctx, wrapped, "slack get conversation replies failed")
		out["error"] = err.Error()
		return out
	}

	if !includeThread && len(msgs) > 1 {
		msgs = msgs[:1]
	}
	if includeThread && latest && len(msgs) > threadLimit {
		// Keep the root and the newest threadLimit-1 replies, so order=latest
		// returns as many messages as the default order would.
		msgs = append(msgs[:1:1], msgs[len(msgs)-(threadLimit-1):]...)
	}

	out["messages"] = convertConversationMessages(msgs, t.limits.MaxTextBytes)
	return out
}

// fetchReplies routes the conversations.replies call to the User-token client
// when available (so public channels can be read without bot membership) and
// falls back to the Bot-token client otherwise.
func (t *getMessagesTool) fetchReplies(ctx context.Context, channelID, threadTS string, limit int) ([]slackservice.ConversationMessage, error) {
	if t.retriever != nil {
		return t.retriever.GetConversationReplies(ctx, channelID, threadTS, limit)
	}
	return t.slack.GetConversationReplies(ctx, channelID, threadTS, limit)
}

func convertConversationMessages(msgs []slackservice.ConversationMessage, maxTextBytes int) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		text, textTruncated := truncateText(m.Text, maxTextBytes)
		out[i] = map[string]any{
			"user_id":   m.UserID,
			"username":  m.UserName,
			"text":      text,
			"ts":        m.Timestamp,
			"thread_ts": m.ThreadTS,
		}
		if textTruncated {
			out[i]["text_truncated"] = true
		}
	}
	return out
}
