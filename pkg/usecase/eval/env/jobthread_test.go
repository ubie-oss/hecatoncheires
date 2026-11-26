package env_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/env"

	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
)

func TestSeededThreadIsReadableThroughGetMessages(t *testing.T) {
	fake := env.NewFakeSlackForTest(time.Unix(1700000000, 0))
	fake.Receive("C1", "1700000000.000100", "1700000000.000100", "U1", "root post body")
	fake.Receive("C1", "1700000000.000100", "1700000000.000101", "U1", "known detail")
	var limits slacktool.Limits
	tools := slacktool.NewReadOnly(slacktool.Deps{Bot: fake, Retriever: fake, Limits: limits})
	for _, tool := range tools {
		if tool.Spec().Name != "slack__get_messages" {
			continue
		}
		out, err := tool.Run(context.Background(), map[string]any{
			"targets": []any{map[string]any{"channel_id": "C1", "ts": "1700000000.000100"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(collectText(out), "\n")
		if !strings.Contains(got, "root post body") || !strings.Contains(got, "known detail") {
			t.Fatalf("thread text missing from tool result: %#v", out)
		}
		return
	}
	t.Fatal("slack__get_messages not built")
}

func collectText(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, collectText(e)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, collectText(e)...)
		}
		return out
	case []map[string]any:
		var out []string
		for _, e := range x {
			out = append(out, collectText(e)...)
		}
		return out
	}
	return nil
}

func TestTurnFinishedWithConsecutiveQuestions(t *testing.T) {
	before := &model.Session{
		LastAction:      model.SessionEndedWithQuestion,
		PendingQuestion: &model.PendingQuestion{PostedMessageTS: "1.000100"},
	}
	current := *before
	current.LastMentionTS = "1.000200"
	if env.TurnFinishedForTest(&current, before) {
		t.Fatal("the previous question still owns the session")
	}
	current.PendingQuestion = nil
	if env.TurnFinishedForTest(&current, before) {
		t.Fatal("removing the previous question alone does not finish the turn")
	}
	current.PendingQuestion = &model.PendingQuestion{PostedMessageTS: "1.000300"}
	if !env.TurnFinishedForTest(&current, before) {
		t.Fatal("a new question should finish the turn")
	}
}

func TestFakeSlackShowsRepliesAsTheyArrive(t *testing.T) {
	ctx := context.Background()
	fake := env.NewFakeSlackForTest(time.Unix(1700000000, 0))
	threadTS := fake.NextTimestamp()
	fake.Receive("C1", threadTS, threadTS, "U1", "initial request")
	before, err := fake.GetConversationReplies(ctx, "C1", threadTS, 0)
	if err != nil || len(before) != 1 {
		t.Fatalf("before reply: %v, %v", before, err)
	}

	ts, err := fake.PostThreadReply(ctx, "C1", threadTS, "agent reply")
	if err != nil {
		t.Fatal(err)
	}
	fake.Receive("C1", threadTS, fake.NextTimestamp(), "U1", "user answer")

	after, err := fake.GetConversationReplies(ctx, "C1", threadTS, 0)
	if err != nil || len(after) != 3 || after[1].Text != "agent reply" || after[2].Text != "user answer" {
		t.Fatalf("after replies: %v, %v", after, err)
	}
	other, err := fake.GetConversationReplies(ctx, "C2", threadTS, 0)
	if err != nil || len(other) != 0 {
		t.Fatalf("other channel: %v, %v", other, err)
	}
	posts := fake.Posts()
	if len(posts) != 1 || posts[0].ThreadTS != threadTS || posts[0].TS != ts {
		t.Fatalf("outbound posts: %v", posts)
	}
}
