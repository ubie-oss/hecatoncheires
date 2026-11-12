package kernel_test

import (
	"testing"

	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// TestScopeRoundTrip pins that every field survives the trip through
// Process.Metadata. A field that silently fails to round-trip reappears as an
// agent with the wrong tools or no access actor, which is exactly the class of
// bug the typed Scope exists to prevent.
func TestScopeRoundTrip(t *testing.T) {
	want := kernel.Scope{
		WorkspaceID: "ws-1",
		CaseID:      42,
		ChannelID:   "C123",
		ThreadTS:    "1700000000.000100",
		UIChannelID: "C456",
		UIThreadTS:  "1700000000.000200",
		SessionID:   "ssn-1",
		ActorUserID: "U999",
		Lang:        "ja",
		ToolSets:    []string{"slack_ro", "notion"},
		PrivateCase: true,
		JobID:       "job-1",
		JobRunID:    "run-1",
		EventType:   "mention",
		SlotGated:   true,
		PreviewTS:   "1700000000.000300",
		LLMModel:    "cheap",
		Budget:      pricing.FromUSD(0.5),

		ThreadModeActions:     true,
		ThreadModeActionReads: true,
	}

	got := kernel.ScopeFrom(want.Metadata())
	gt.Value(t, got).Equal(want)
}

// A processing placeholder round-trips too. It is a separate case because it and
// PreviewTS are mutually exclusive, so one round-trip cannot carry both.
func TestScopeRoundTripProcessingPlaceholder(t *testing.T) {
	want := kernel.Scope{
		ToolSets:     []string{kernel.ToolSetsAll},
		ProcessingTS: "1700000000.000400",
	}

	got := kernel.ScopeFrom(want.Metadata())
	gt.Value(t, got).Equal(want)
}

// UITarget answers "where does the requester see this", falling back to the run's
// own thread when they are the same — which is every run except a case raised by
// a reaction in another channel.
func TestScopeUITarget(t *testing.T) {
	own := kernel.Scope{ChannelID: "C-CASE", ThreadTS: "1700000000.000100"}
	ch, ts := own.UITarget()
	gt.Value(t, ch).Equal("C-CASE")
	gt.Value(t, ts).Equal("1700000000.000100")

	split := kernel.Scope{
		ChannelID: "C-CASE", ThreadTS: "1700000000.000100",
		UIChannelID: "C-SOURCE", UIThreadTS: "1700000000.000200",
	}
	ch, ts = split.UITarget()
	gt.Value(t, ch).Equal("C-SOURCE")
	gt.Value(t, ts).Equal("1700000000.000200")
}

func TestScopeMetadataOmitsEmptyValues(t *testing.T) {
	sc := kernel.Scope{ToolSets: []string{kernel.ToolSetsAll}}
	m := sc.Metadata()

	gt.Map(t, m).HasKey("toolsets")
	for _, key := range []string{
		"workspace_id", "case_id", "channel_id", "thread_ts",
		"ui_channel_id", "ui_thread_ts", "processing_ts", "preview_ts",
		"session_id", "actor_user_id", "lang", "private_case",
		"job_id", "job_run_id", "event_type", "slot_gated",
		"llm_model", "budget_nano_usd",
	} {
		_, ok := m[key]
		gt.Bool(t, ok).False()
	}
}

// TestScopeFromMalformedBudget pins that a hand-edited budget value leaves the run
// on the deployment default rather than refusing to run: a zero budget reads as
// "not specified" everywhere downstream.
func TestScopeFromMalformedBudget(t *testing.T) {
	got := kernel.ScopeFrom(map[string]string{"budget_nano_usd": "one dollar"})
	gt.Value(t, got.Budget).Equal(pricing.NanoUSD(0))
}

// TestScopeFromMalformedValues pins that a hand-edited or older record still
// yields a usable Scope. Refusing to parse would strand the Process with no way
// to finish.
func TestScopeFromMalformedValues(t *testing.T) {
	got := kernel.ScopeFrom(map[string]string{
		"case_id":      "not-a-number",
		"private_case": "yes",
		"toolsets":     ",slack_ro,,notion,",
	})

	gt.Value(t, got.CaseID).Equal(int64(0))
	gt.Bool(t, got.PrivateCase).False() // only "1" means private
	gt.Array(t, got.ToolSets).Equal([]string{"slack_ro", "notion"})
}

func TestScopeFromEmptyMetadata(t *testing.T) {
	got := kernel.ScopeFrom(nil)
	gt.Value(t, got).Equal(kernel.Scope{})
}

func TestScopeValidate(t *testing.T) {
	valid := kernel.Scope{
		WorkspaceID: "ws-1",
		CaseID:      1,
		ChannelID:   "C1",
		ThreadTS:    "1.1",
		ToolSets:    []string{kernel.ToolSetsAll},
	}

	testCases := map[string]struct {
		mutate  func(kernel.Scope) kernel.Scope
		wantErr bool
	}{
		"valid": {
			mutate: func(s kernel.Scope) kernel.Scope { return s },
		},
		"a budget of its own": {
			mutate: func(s kernel.Scope) kernel.Scope { s.Budget = pricing.FromUSD(1); return s },
		},
		"negative budget": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.Budget = -1; return s },
			wantErr: true,
		},
		"thread without channel": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.ChannelID = ""; return s },
			wantErr: true,
		},
		"channel without thread": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.ThreadTS = ""; return s },
			wantErr: true,
		},
		"neither channel nor thread is fine": {
			mutate: func(s kernel.Scope) kernel.Scope { s.ChannelID = ""; s.ThreadTS = ""; return s },
		},
		"case without workspace": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.WorkspaceID = ""; return s },
			wantErr: true,
		},
		"negative case id": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.CaseID = -1; return s },
			wantErr: true,
		},
		"no toolsets": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.ToolSets = nil; return s },
			wantErr: true,
		},
		"empty toolset id": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.ToolSets = []string{"slack_ro", ""}; return s },
			wantErr: true,
		},
		"job run without job": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.JobRunID = "run-1"; return s },
			wantErr: true,
		},
		"ui thread without ui channel": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.UIThreadTS = "1.2"; return s },
			wantErr: true,
		},
		"ui channel without ui thread": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.UIChannelID = "C2"; return s },
			wantErr: true,
		},
		"both ui coordinates is fine": {
			mutate: func(s kernel.Scope) kernel.Scope { s.UIChannelID = "C2"; s.UIThreadTS = "1.2"; return s },
		},
		// The two name different lifecycles of the same slot, so a run carrying both
		// would have two answers to "where does the result go".
		"processing and preview together": {
			mutate: func(s kernel.Scope) kernel.Scope {
				s.ProcessingTS = "1.3"
				s.PreviewTS = "1.4"
				return s
			},
			wantErr: true,
		},
		"processing alone is fine": {
			mutate: func(s kernel.Scope) kernel.Scope { s.ProcessingTS = "1.3"; return s },
		},
		"preview alone is fine": {
			mutate: func(s kernel.Scope) kernel.Scope { s.PreviewTS = "1.4"; return s },
		},
		"job run with job": {
			mutate: func(s kernel.Scope) kernel.Scope { s.JobID = "job-1"; s.JobRunID = "run-1"; return s },
		},
		// A gated run whose identifiers are incomplete would be refused capacity on
		// every claim without ever spending its retry budget — it would wait for
		// ever. Catching it at Spawn makes that a readable error instead.
		"slot gated without a job": {
			mutate:  func(s kernel.Scope) kernel.Scope { s.SlotGated = true; return s },
			wantErr: true,
		},
		"slot gated without a case": {
			mutate: func(s kernel.Scope) kernel.Scope {
				s.SlotGated = true
				s.JobID = "job-1"
				s.CaseID = 0
				return s
			},
			wantErr: true,
		},
		"slot gated without a workspace": {
			mutate: func(s kernel.Scope) kernel.Scope {
				s.SlotGated = true
				s.JobID = "job-1"
				s.WorkspaceID = ""
				s.CaseID = 0
				return s
			},
			wantErr: true,
		},
		"slot gated fully identified": {
			mutate: func(s kernel.Scope) kernel.Scope { s.SlotGated = true; s.JobID = "job-1"; return s },
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := tc.mutate(valid).Validate()
			if tc.wantErr {
				gt.Value(t, err).NotNil()
				return
			}
			gt.NoError(t, err)
		})
	}
}

// TestWithToolSets pins the property a sub-agent spawn depends on: agentkit's
// SpawnChild REPLACES the parent's metadata map, so the child's map must carry
// the parent's scope forward and differ only in the toolset list.
func TestWithToolSets(t *testing.T) {
	parent := kernel.Scope{
		WorkspaceID: "ws-1",
		CaseID:      7,
		ChannelID:   "C1",
		ThreadTS:    "1.1",
		ActorUserID: "U1",
		ToolSets:    []string{kernel.ToolSetsAll},
	}.Metadata()

	child := kernel.WithToolSets(parent, []string{"slack_ro", "webfetch"})
	childScope := kernel.ScopeFrom(child)

	gt.String(t, childScope.WorkspaceID).Equal("ws-1")
	gt.Value(t, childScope.CaseID).Equal(int64(7))
	gt.String(t, childScope.ActorUserID).Equal("U1")
	gt.Array(t, childScope.ToolSets).Equal([]string{"slack_ro", "webfetch"})

	// The parent's own map must not have been rewritten.
	gt.Array(t, kernel.ScopeFrom(parent).ToolSets).Equal([]string{kernel.ToolSetsAll})
}

func TestWithToolSetsEmptyList(t *testing.T) {
	parent := kernel.Scope{WorkspaceID: "ws-1", ToolSets: []string{"slack_ro"}}.Metadata()
	child := kernel.WithToolSets(parent, nil)

	gt.String(t, kernel.ScopeFrom(child).WorkspaceID).Equal("ws-1")
	gt.Array(t, kernel.ScopeFrom(child).ToolSets).Length(0)
}

func TestWithToolSetsNilParent(t *testing.T) {
	child := kernel.WithToolSets(nil, []string{"slack_ro"})
	gt.Array(t, kernel.ScopeFrom(child).ToolSets).Equal([]string{"slack_ro"})
}

// TestWithBudget pins the same property for the other thing a spawn decides: the
// child's map carries the parent's scope forward and differs only in the spend
// ceiling. A rebuilt map would leave the child with no workspace and therefore no
// tools.
func TestWithBudget(t *testing.T) {
	parent := kernel.Scope{
		WorkspaceID: "ws-1",
		CaseID:      7,
		ChannelID:   "C1",
		ThreadTS:    "1.1",
		ActorUserID: "U1",
		LLMModel:    "cheap",
		ToolSets:    []string{"slack_ro"},
		Budget:      pricing.FromUSD(2),
	}.Metadata()

	child := kernel.WithBudget(parent, pricing.FromUSD(0.25))
	childScope := kernel.ScopeFrom(child)

	gt.Value(t, childScope.Budget).Equal(pricing.FromUSD(0.25))
	gt.String(t, childScope.WorkspaceID).Equal("ws-1")
	gt.Value(t, childScope.CaseID).Equal(int64(7))
	gt.String(t, childScope.ActorUserID).Equal("U1")
	// The model reference must survive: it is what the child's own spend is priced
	// at, so losing it would meter the child at the default model's rate.
	gt.String(t, childScope.LLMModel).Equal("cheap")
	gt.Array(t, childScope.ToolSets).Equal([]string{"slack_ro"})

	// The parent's own map must not have been rewritten.
	gt.Value(t, kernel.ScopeFrom(parent).Budget).Equal(pricing.FromUSD(2))
}

// TestWithBudgetNonPositiveRemovesTheKey pins that a zero or negative amount
// reads back as "not specified" rather than as a zero allowance. Scope.Metadata
// omits an unset budget the same way, so the two cannot disagree about what an
// absent key means — and a stored literal zero would be resolved as a budget of
// nothing, stopping the child as unpriced.
func TestWithBudgetNonPositiveRemovesTheKey(t *testing.T) {
	parent := kernel.Scope{
		WorkspaceID: "ws-1", ToolSets: []string{"slack_ro"}, Budget: pricing.FromUSD(2),
	}.Metadata()

	for name, amount := range map[string]pricing.NanoUSD{"zero": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			child := kernel.WithBudget(parent, amount)
			gt.Map(t, child).NotHasKey("budget_nano_usd")
			gt.Value(t, kernel.ScopeFrom(child).Budget).Equal(pricing.NanoUSD(0))
			// Everything else is still there.
			gt.String(t, kernel.ScopeFrom(child).WorkspaceID).Equal("ws-1")
		})
	}
}

func TestWithBudgetNilParent(t *testing.T) {
	child := kernel.WithBudget(nil, pricing.FromUSD(0.5))
	gt.Value(t, kernel.ScopeFrom(child).Budget).Equal(pricing.FromUSD(0.5))
}
