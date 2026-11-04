package casebound_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	agentprocmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/slackfmt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/casebound"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// testModelPolicy is the one-model policy these turns are priced at. Its figures
// are what the run-record assertions expect: $1 / $5 per MTok.
func testModelPolicy(t *testing.T) agentkernel.ModelPolicy {
	t.Helper()
	p, err := agentkernel.NewModelPolicy(agentkernel.ModelPolicyInput{
		Defs: []agentkernel.ModelDef{{
			Ref:      "test",
			Provider: agentkernel.ProviderClaude,
			Model:    "test-model",
			Rate:     pricing.Rate{Input: 1000, Output: 5000},
		}},
		DefaultRef:    "test",
		DefaultBudget: pricing.FromUSD(100),
	})
	gt.NoError(t, err).Required()
	return p
}

// hostCall records one Slack-facing call the finished turn made.
type hostCall struct {
	Kind      string // "reply" or "failure"
	ChannelID string
	ThreadTS  string
	Text      string
}

// recordingHost captures every Host call so a test can assert what the user
// actually saw, not merely that the turn ended without an error.
type recordingHost struct {
	mu    sync.Mutex
	calls []hostCall
}

func (h *recordingHost) Reply(_ context.Context, channelID, threadTS, text string) error {
	h.record(hostCall{Kind: "reply", ChannelID: channelID, ThreadTS: threadTS, Text: text})
	return nil
}

func (h *recordingHost) ReportFailure(_ context.Context, channelID, threadTS, reason string) error {
	h.record(hostCall{Kind: "failure", ChannelID: channelID, ThreadTS: threadTS, Text: reason})
	return nil
}

func (h *recordingHost) record(c hostCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
}

func (h *recordingHost) Calls() []hostCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hostCall, len(h.calls))
	copy(out, h.calls)
	return out
}

// scriptedLLM answers with responses[i] on the i-th Generate. An extra call
// beyond the script fails rather than silently repeating the last answer.
func scriptedLLM(responses ...*gollem.Response) gollem.LLMClient {
	var n atomic.Int32
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					i := int(n.Add(1)) - 1
					if i >= len(responses) {
						return nil, goerr.New("unexpected extra generate call", goerr.V("call_index", i))
					}
					return responses[i], nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
}

// failingLLM refuses every Generate, which is how a turn reaches ProcessFailed.
func failingLLM() gollem.LLMClient {
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return nil, goerr.New("the model is unreachable")
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
}

const (
	testChannelID = "C-CASE"
	testThreadTS  = "1700000000.000001"
)

type harness struct {
	uc     *casebound.UseCase
	repo   *memory.Memory
	host   *recordingHost
	kernel *agentkit.Kernel
}

// newHarness wires the UseCase against a real agentkit Kernel with an
// in-process Process store. The Kernel is built directly rather than through
// agentkernel.Build so this test isolates the host's own contract — spawn,
// turn lock, completion — from the tool factory, which pkg/agent/kernel covers.
func newHarness(t *testing.T, llm gollem.LLMClient, tools ...gollem.Tool) *harness {
	t.Helper()

	repo := memory.New()
	host := &recordingHost{}
	procRepo := agentprocmemory.New()
	locator, err := agentkernel.NewLocator(procRepo)
	gt.NoError(t, err).Required()

	uc, err := casebound.New(repo, host, locator, testModelPolicy(t))
	gt.NoError(t, err).Required()

	reg := agentkit.NewRegistry()
	limiter := budget.Config{
		MaxSteps: 32, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8,
	}.Limiter(testModelPolicy(t).Resolve)
	gt.NoError(t, uc.Register(reg, limiter, agentarchive.NewMemoryHistoryStore())).Required()

	k, err := agentkit.New(procRepo, llm, reg,
		agentkit.WithToolFactory(func(context.Context, *agentkit.Process) ([]gollem.Tool, error) {
			return tools, nil
		}))
	gt.NoError(t, err).Required()
	uc.Bind(k)

	return &harness{uc: uc, repo: repo, host: host, kernel: k}
}

// session persists the Session a turn locks on and returns it.
func (h *harness) session(t *testing.T, ctx context.Context) *model.Session {
	t.Helper()
	ssn := &model.Session{
		ID:          "s-cb-1",
		ChannelID:   testChannelID,
		ThreadTS:    testThreadTS,
		WorkspaceID: "ws-1",
		CaseID:      55,
		Kind:        model.SessionKindCase,
	}
	gt.NoError(t, h.repo.Session().Put(ctx, ssn)).Required()
	return ssn
}

func (h *harness) request(ssn *model.Session, triggerTS string) casebound.TurnRequest {
	return casebound.TurnRequest{
		Session:       ssn,
		ChannelID:     testChannelID,
		ThreadTS:      testThreadTS,
		MentionTS:     triggerTS,
		MentionText:   "<@bot> what's up?",
		MentionUserID: "U-HUMAN",
		BotUserID:     "U-BOT",
		Workspace:     &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-1", Name: "WS"}},
		Case:          &model.Case{ID: 55, Title: "Case", Status: types.CaseStatusOpen, SlackChannelID: testChannelID},
		TriggerTS:     triggerTS,
	}
}

// jobIDOf reads the per-turn JobID a run recorded on its Process, which is the
// key its JobRunLog lives under.
func (h *harness) jobIDOf(t *testing.T, ctx context.Context, pid agentkit.ProcessID) string {
	t.Helper()
	proc, err := h.kernel.GetProcess(ctx, pid)
	gt.NoError(t, err).Required()
	return agentkernel.ScopeFrom(proc.Metadata).JobID
}

// drive runs the worker until pid reaches a terminal state and returns it.
func (h *harness) drive(t *testing.T, pid agentkit.ProcessID, opts ...agentkit.ServeOption) *agentkit.Process {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts = append([]agentkit.ServeOption{agentkit.WithPollInterval(5 * time.Millisecond)}, opts...)
	served := make(chan error, 1)
	go func() { served <- h.kernel.Serve(ctx, opts...) }()

	for {
		proc, err := h.kernel.GetProcess(ctx, pid)
		gt.NoError(t, err).Required()
		if proc.Status.Terminal() {
			cancel()
			<-served
			return proc
		}
		select {
		case <-ctx.Done():
			gt.NoError(t, ctx.Err()).Required()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A finished turn must post its answer to the thread it came from, close the
// JobRunLog the case agent page lists, and advance the session's mention
// position — all from the completion handler, since StartTurn returned long
// before the model answered.
func TestStartTurnPostsTheReplyAndRecordsTheRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, scriptedLLM(&gollem.Response{
		Texts:       []string{"Here is the answer."},
		InputToken:  120,
		OutputToken: 34,
	}))
	ssn := h.session(t, ctx)

	res, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000001.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, res.Status).Equal(casebound.StatusStarted)
	gt.Value(t, res.ProcessID).NotEqual(agentkit.ProcessID(""))

	// StartTurn only records the run. Nothing has been said to the user yet.
	gt.Array(t, h.host.Calls()).Length(0)

	proc := h.drive(t, res.ProcessID)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.String(t, calls[0].Kind).Equal("reply")
	gt.String(t, calls[0].ChannelID).Equal(testChannelID)
	gt.String(t, calls[0].ThreadTS).Equal(testThreadTS)
	gt.String(t, calls[0].Text).Equal("Here is the answer.")

	// The run is discovered through the same read path the case agent page uses:
	// a fresh per-turn JobID, not a fixed sentinel.
	runs, err := h.repo.JobRun().ListByCase(ctx, "ws-1", 55)
	gt.NoError(t, err).Required()
	gt.Array(t, runs).Length(1).Required()
	gt.String(t, runs[0].JobID).NotEqual("")
	gt.Value(t, runs[0].LastStatus).Equal(model.JobRunStatusSuccess)

	key := model.JobRunKey{WorkspaceID: "ws-1", CaseID: 55, JobID: runs[0].JobID}
	logs, err := h.repo.JobRunLog().List(ctx, key, 100)
	gt.NoError(t, err).Required()
	gt.Array(t, logs).Length(1).Required()
	log := logs[0]
	gt.Value(t, log.Stage).Equal(model.JobRunStageSuccess)
	gt.String(t, log.EventType).Equal(model.EventTypeMention)
	gt.String(t, log.ExecutorKind).Equal(model.ExecutorKindSingleLoop)
	gt.Number(t, log.CaseID).Equal(55)
	gt.String(t, log.Error).Equal("")
	// The usage comes off the Process, which is the only place a durable run's
	// total survives: its transitions span claims and possibly instances.
	gt.Value(t, log.InputTokens).Equal(int64(120))
	gt.Value(t, log.OutputTokens).Equal(int64(34))
	gt.Value(t, log.LLMCallCount).Equal(int64(1))
	// The same usage priced at testModelPolicy's rate: 120*1000 + 34*5000 nanoUSD.
	// A host that recorded no cost, or priced it at another model's rate, fails
	// here rather than showing an em dash on the run-detail page.
	gt.Value(t, log.CostNanoUSD).Equal(int64(290_000))
	gt.String(t, log.Model).Equal("test-model")

	stored, err := h.repo.Session().GetByThread(ctx, testChannelID, testThreadTS)
	gt.NoError(t, err).Required()
	gt.String(t, stored.LastMentionTS).Equal("1700000001.000001")
}

// The cursor write from the spawning side must not roll anything back. It races
// the turn it just started — agentkit frees the thread's subject at the terminal
// commit, before the completion handler runs — so a later turn's cursor and any
// field another path recorded meanwhile have to survive it. A full Session.Put
// here would restore this turn's stale copy of both.
func TestStartTurnNeverRollsTheSessionBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, scriptedLLM(&gollem.Response{Texts: []string{"Here is the answer."}}))
	ssn := h.session(t, ctx)

	// Stand in for what a concurrent turn / another path wrote after this turn's
	// request was built: a later cursor, and an end reason on the same row.
	ahead := &model.Session{
		ID: ssn.ID, ChannelID: testChannelID, ThreadTS: testThreadTS,
		WorkspaceID: "ws-1", CaseID: 55, Kind: model.SessionKindCase,
		LastMentionTS: "1700000009.000001",
		LastAction:    model.SessionEndedWithQuestion,
	}
	gt.NoError(t, h.repo.Session().Put(ctx, ahead)).Required()

	// The request still carries the earlier mention this turn is processing.
	res, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000001.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, res.Status).Equal(casebound.StatusStarted)

	stored, err := h.repo.Session().GetByThread(ctx, testChannelID, testThreadTS)
	gt.NoError(t, err).Required()
	gt.String(t, stored.LastMentionTS).Equal("1700000009.000001")
	gt.Value(t, stored.LastAction).Equal(model.SessionEndedWithQuestion)
}

// A failed turn must tell the user, and record the failure on the run so the
// case agent page shows why rather than a run that merely never finished.
func TestStartTurnReportsAFailedTurn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, failingLLM())
	ssn := h.session(t, ctx)

	res, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000002.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, res.Status).Equal(casebound.StatusStarted)

	proc := h.drive(t, res.ProcessID, agentkit.WithMaxStepAttempts(1))
	gt.Value(t, proc.Status).Equal(agentkit.ProcessFailed)

	calls := h.host.Calls()
	gt.Array(t, calls).Length(1).Required()
	gt.String(t, calls[0].Kind).Equal("failure")
	gt.String(t, calls[0].ChannelID).Equal(testChannelID)
	gt.String(t, calls[0].Text).Contains("the model is unreachable")

	runs, err := h.repo.JobRun().ListByCase(ctx, "ws-1", 55)
	gt.NoError(t, err).Required()
	gt.Array(t, runs).Length(1).Required()
	gt.Value(t, runs[0].LastStatus).Equal(model.JobRunStatusFailed)

	key := model.JobRunKey{WorkspaceID: "ws-1", CaseID: 55, JobID: runs[0].JobID}
	logs, err := h.repo.JobRunLog().List(ctx, key, 100)
	gt.NoError(t, err).Required()
	gt.Array(t, logs).Length(1).Required()
	gt.Value(t, logs[0].Stage).Equal(model.JobRunStageFailed)
	gt.String(t, logs[0].Error).Contains("the model is unreachable")
}

// A second mention arriving while a turn is still running must be refused, and
// the refusal must name the run holding the thread so the host can say what it
// is waiting on. The worker is deliberately not started here: the first run
// stays pending and therefore keeps the lock.
func TestStartTurnRefusesASecondTurnOnTheSameThread(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, scriptedLLM(&gollem.Response{Texts: []string{"unused"}}))
	ssn := h.session(t, ctx)

	first, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000003.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, first.Status).Equal(casebound.StatusStarted)

	second, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000004.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, second.Status).Equal(casebound.StatusBusy)
	gt.Value(t, second.Busy).NotNil().Required()
	gt.Value(t, second.Busy.ProcessID).Equal(first.ProcessID)
	gt.Bool(t, second.Busy.StartedAt.IsZero()).False()

	// The refused turn must leave no run record at all. A RUNNING log opened for
	// a turn that never started never reaches Finish, so it would sit in storage
	// forever without ever being listed.
	runs, err := h.repo.JobRun().ListByCase(ctx, "ws-1", 55)
	gt.NoError(t, err).Required()
	gt.Array(t, runs).Length(0)

	// The turn that DID start has its RUNNING log, under its own per-turn JobID.
	// (The refused turn's JobID is never used, so there is no key to look it up
	// by — its absence is what the JobRun assertion above stands for.)
	firstKey := model.JobRunKey{WorkspaceID: "ws-1", CaseID: 55, JobID: h.jobIDOf(t, ctx, first.ProcessID)}
	firstLogs, err := h.repo.JobRunLog().List(ctx, firstKey, 10)
	gt.NoError(t, err).Required()
	gt.Array(t, firstLogs).Length(1).Required()
	gt.Value(t, firstLogs[0].Stage).Equal(model.JobRunStageRunning)
}

// Slack re-delivers events. A re-delivery must resolve to the run the first
// delivery started — reported as a duplicate so the host drops it silently
// rather than posting a "busy" notice for the user's own single mention.
func TestStartTurnDropsARedeliveredTrigger(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, scriptedLLM(&gollem.Response{Texts: []string{"unused"}}))
	ssn := h.session(t, ctx)

	first, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000005.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, first.Status).Equal(casebound.StatusStarted)

	again, err := h.uc.StartTurn(ctx, h.request(ssn, "1700000005.000001"))
	gt.NoError(t, err).Required()
	gt.Value(t, again.Status).Equal(casebound.StatusDuplicate)
	gt.Value(t, again.ProcessID).Equal(first.ProcessID)
	gt.Value(t, again.Busy).Nil()
}

// An unbound UseCase must refuse rather than panic: the Kernel is handed over
// after registration, so a wiring order mistake is a real possibility.
func TestStartTurnRefusesWhenUnbound(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	locator, err := agentkernel.NewLocator(agentprocmemory.New())
	gt.NoError(t, err).Required()
	uc, err := casebound.New(repo, &recordingHost{}, locator, testModelPolicy(t))
	gt.NoError(t, err).Required()

	_, err = uc.StartTurn(ctx, casebound.TurnRequest{})
	gt.Error(t, err)
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	locator, err := agentkernel.NewLocator(agentprocmemory.New())
	gt.NoError(t, err).Required()
	models := testModelPolicy(t)

	_, err = casebound.New(nil, &recordingHost{}, locator, models)
	gt.Error(t, err)

	_, err = casebound.New(memory.New(), nil, locator, models)
	gt.Error(t, err)
}

// validateRequest is the gate that keeps an actor-less run from being spawned.
// A context with no actor is read by the usecase layer as a system context and
// BYPASSES private-case access control, so this must be a refusal, not a
// degraded run.
func TestValidateRequestRequiresAnActor(t *testing.T) {
	req := casebound.TurnRequest{
		Session:   &model.Session{ID: "s-1", ChannelID: testChannelID, ThreadTS: testThreadTS},
		MentionTS: "1700000006.000001",
		TriggerTS: "1700000006.000001",
		Case:      &model.Case{ID: 1},
		Workspace: &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-1"}},
	}
	gt.Error(t, casebound.ValidateRequestForTest(&req))

	req.MentionUserID = "U-HUMAN"
	gt.NoError(t, casebound.ValidateRequestForTest(&req))
}

func TestValidateRequestRequiresAPersistedSession(t *testing.T) {
	base := casebound.TurnRequest{
		MentionTS:     "1700000007.000001",
		TriggerTS:     "1700000007.000001",
		MentionUserID: "U-HUMAN",
		Case:          &model.Case{ID: 1},
		Workspace:     &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-1"}},
	}

	nilSession := base
	gt.Error(t, casebound.ValidateRequestForTest(&nilSession))

	unpersisted := base
	unpersisted.Session = &model.Session{ChannelID: testChannelID, ThreadTS: testThreadTS}
	gt.Error(t, casebound.ValidateRequestForTest(&unpersisted))
}

func TestBuildSystemPrompt_CaseAndFieldValues(t *testing.T) {
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "severity", Name: "Severity Level", Type: types.FieldTypeSelect},
			},
		},
	}
	c := &model.Case{
		Title:       "Important Case",
		Description: "This is very important",
		Status:      types.CaseStatusOpen,
		FieldValues: map[string]model.FieldValue{
			"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
		},
	}
	messages := []casebound.ConversationMessage{
		{UserID: "U001", UserName: "alice", Text: "Hello", Timestamp: "1234567890.000001"},
	}
	now := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, nil, nil, messages)

	gt.String(t, prompt).Contains("Important Case")
	gt.String(t, prompt).Contains("This is very important")
	gt.String(t, prompt).Contains("Severity Level")
	gt.String(t, prompt).Contains("high")
	gt.String(t, prompt).Contains("alice: Hello")
	// This agent's answer is posted to the thread verbatim, so the shared Slack
	// formatting rules have to reach it.
	gt.String(t, prompt).Contains(slackfmt.Section())
	gt.Bool(t, strings.Contains(prompt, "{{")).False()
}

// buildSystemPrompt swallows a template failure and returns a one-line fallback,
// so a broken template would silently drop the case context and the Slack
// formatting rules at once. Pin that the real template still renders, including
// with no workspace entry to read a schema from.
func TestBuildSystemPrompt_SlackFormatSurvivesWithoutWorkspaceEntry(t *testing.T) {
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, nil, "C0123ABC", "1700000000.000100", now, nil, nil, nil)

	gt.Bool(t, strings.HasPrefix(prompt, "You are an AI assistant. Case:")).False()
	gt.String(t, prompt).Contains(slackfmt.Section())
}

// A workspace that fixes its language overrides the "answer in the user's
// language" instruction; without one that instruction stays.
func TestBuildSystemPrompt_Language(t *testing.T) {
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)
	const userLanguage = "Respond in the same language as the user's message."

	ja := casebound.BuildSystemPromptForTest(c, &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-ja"}, Language: i18n.LangJA,
	}, "C0123ABC", "1700000000.000100", now, nil, nil, nil)
	gt.String(t, ja).Contains("Respond in **Japanese**, whatever language the user's message is written in.")
	gt.Bool(t, strings.Contains(ja, userLanguage)).False()

	unset := casebound.BuildSystemPromptForTest(c, &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-unset"},
	}, "C0123ABC", "1700000000.000100", now, nil, nil, nil)
	gt.String(t, unset).Contains(userLanguage)
	gt.Bool(t, strings.Contains(unset, "Respond in **")).False()

	noEntry := casebound.BuildSystemPromptForTest(c, nil, "C0123ABC", "1700000000.000100", now, nil, nil, nil)
	gt.String(t, noEntry).Contains(userLanguage)
}

func TestBuildSystemPrompt_ChannelIDAndTime(t *testing.T) {
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
	}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 45, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0123ABC", "1700000000.000100", now, nil, nil, nil)

	gt.String(t, prompt).Contains("## Slack Context")
	gt.String(t, prompt).Contains("Channel ID: C0123ABC")
	// The agent holds slack__get_messages, whose targets take a (channel_id, ts)
	// pair. Without the ts in the prompt it has to invent one.
	gt.String(t, prompt).Contains("Thread TS: 1700000000.000100")
	gt.String(t, prompt).Contains("slack__get_messages")
	gt.String(t, prompt).Contains("## Current Time")
	gt.String(t, prompt).Contains("2026-05-04T12:30:45Z")
}

func TestBuildSystemPrompt_FieldSemantic(t *testing.T) {
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "notify_channel", Name: "Notify", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
			{ID: "note", Name: "Note", Type: types.FieldTypeText},
		}},
	}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 45, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0123ABC", "", now, nil, nil, nil)

	gt.String(t, prompt).Contains("- id=`notify_channel` name=\"Notify\" type=text semantic=" + semantic.Label(types.SemanticSlackChannelID))
	gt.String(t, prompt).Contains("- id=`note` name=\"Note\" type=text\n")
	gt.Bool(t, strings.Contains(prompt, "## Memo Fields")).False()
}

func TestBuildSystemPrompt_FieldPattern(t *testing.T) {
	ticket := types.TextPattern("[A-Z]{2,5}-[0-9]+")
	cve := types.TextPattern("CVE-[0-9]{4}-[0-9]{4,}")
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "ticket_id", Name: "Ticket ID", Type: types.FieldTypeText,
				Description: "Ticket ID in the external tracker, e.g. SEC-1234",
				Validation:  config.FieldValidation{Pattern: ticket}},
			{ID: "note", Name: "Note", Type: types.FieldTypeText},
		}},
		MemoConfig: &config.MemoConfig{
			Description: "A memo records one observation.",
			FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
				{ID: "cve", Name: "CVE", Type: types.FieldTypeText, Validation: config.FieldValidation{Pattern: cve}},
			}},
		},
	}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 45, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0123ABC", "", now, nil, nil, nil)

	gt.String(t, prompt).Contains("- id=`ticket_id` name=\"Ticket ID\" type=text — Ticket ID in the external tracker, e.g. SEC-1234 pattern=" +
		"`[A-Z]{2,5}-[0-9]+` — the whole value must match this regular expression (Go RE2 syntax); an empty value is always accepted\n")
	gt.String(t, prompt).Contains("- id=`cve` name=\"CVE\" type=text pattern=" + cve.Label() + "\n")
	gt.String(t, prompt).Contains("- id=`note` name=\"Note\" type=text\n")
}

// The memo__* tools reach this agent whenever memos are enabled, and their
// `fields` parameter defers to the system prompt for ids and value shapes.
func TestBuildSystemPrompt_MemoFields(t *testing.T) {
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		MemoConfig: &config.MemoConfig{
			Description: "A memo records one observation.",
			FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
				{ID: "source_channel", Name: "Source", Type: types.FieldTypeText, Required: true, Semantic: types.SemanticSlackChannelID},
				{ID: "kind", Name: "Kind", Type: types.FieldTypeSelect, Options: []config.FieldOption{{ID: "fact", Name: "Fact"}}},
			}},
		},
	}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 45, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0123ABC", "", now, nil, nil, nil)

	gt.String(t, prompt).Contains("## Memo Fields\nMemos are this case's persistent memory, written with the `memo__*` tools. A memo records one observation.\n")
	gt.String(t, prompt).Contains("- id=`source_channel` name=\"Source\" type=text (required) semantic=" + semantic.Label(types.SemanticSlackChannelID) + "\n")
	gt.String(t, prompt).Contains("- id=`kind` name=\"Kind\" type=select\n  - option id=`fact` name=\"Fact\"\n")
}

// A turn with no thread must not render a dangling "Thread TS:" label the model
// could pass on as an id.
func TestBuildSystemPrompt_OmitsAbsentThreadTS(t *testing.T) {
	entry := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-test", Name: "Test"}}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 45, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0123ABC", "", now, nil, nil, nil)

	gt.String(t, prompt).Contains("Channel ID: C0123ABC")
	gt.Bool(t, strings.Contains(prompt, "Thread TS:")).False()
}

func TestBuildSystemPrompt_CaseWideActionsTitleOnly(t *testing.T) {
	entry := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-test", Name: "Test"}}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	actions := []*model.Action{
		{ID: 1, Title: "Investigate the issue", Status: types.ActionStatusInProgress, AssigneeID: "U001"},
		{ID: 2, Title: "Write report", Status: types.ActionStatusTodo},
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, nil, actions, nil)

	gt.String(t, prompt).Contains("## Actions")
	gt.String(t, prompt).Contains("Investigate the issue")
	gt.String(t, prompt).Contains("Write report")
	// Status / Assignee detail must NOT leak into the case-wide list.
	gt.Bool(t, strings.Contains(prompt, "U001")).False()
	gt.Bool(t, strings.Contains(prompt, "IN_PROGRESS")).False()
	gt.Bool(t, strings.Contains(prompt, "TODO")).False()
	// And the Current Action section must be absent.
	gt.Bool(t, strings.Contains(prompt, "## Current Action")).False()
}

func TestBuildSystemPrompt_CurrentActionInActionThread(t *testing.T) {
	entry := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-test", Name: "Test"}}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	due := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	currentAction := &model.Action{
		ID:          7,
		Title:       "Patch the vulnerable library",
		Description: "Bump dep to 1.2.3 and rerun integration tests.",
		Status:      types.ActionStatusInProgress,
		AssigneeID:  "U777",
		DueDate:     &due,
	}
	others := []*model.Action{{ID: 8, Title: "Sibling action"}}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, currentAction, others, nil)

	gt.String(t, prompt).Contains("## Current Action")
	gt.String(t, prompt).Contains("Patch the vulnerable library")
	gt.String(t, prompt).Contains("IN_PROGRESS")
	gt.String(t, prompt).Contains("Assignee: U777")
	gt.String(t, prompt).Contains("Bump dep to 1.2.3")
	gt.String(t, prompt).Contains("2026-06-01T09:00:00Z")
	// Case-wide actions must be suppressed in this mode.
	gt.Bool(t, strings.Contains(prompt, "## Actions")).False()
	gt.Bool(t, strings.Contains(prompt, "Sibling action")).False()
}

func TestBuildSystemPrompt_CurrentActionWithoutAssignee(t *testing.T) {
	entry := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-test", Name: "Test"}}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	currentAction := &model.Action{ID: 9, Title: "Triage", Status: types.ActionStatusTodo}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, currentAction, nil, nil)

	gt.String(t, prompt).Contains("Assignee: unassigned")
	gt.Bool(t, strings.Contains(prompt, "- Due:")).False()
	gt.Bool(t, strings.Contains(prompt, "### Description")).False()
}

func TestBuildSystemPrompt_NoActionsSection(t *testing.T) {
	entry := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-test", Name: "Test"}}
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, nil, nil, nil)

	gt.Bool(t, strings.Contains(prompt, "## Actions")).False()
	gt.Bool(t, strings.Contains(prompt, "## Current Action")).False()
}

func TestBuildUserInput_NoDelta(t *testing.T) {
	got := casebound.BuildUserInputForTest(nil, "@bot ping", "1700000000.000001")
	gt.String(t, got).Equal("@bot ping")
}

func TestBuildUserInput_WithDelta(t *testing.T) {
	delta := []casebound.ConversationMessage{
		{UserID: "U1", UserName: "alice", Text: "still here", Timestamp: "1700000005.000001"},
		{UserID: "U2", Text: "no name", Timestamp: "1700000006.000001"},
	}
	got := casebound.BuildUserInputForTest(delta, "@bot follow up", "1700000010.000001")
	gt.String(t, got).Contains("# Unprocessed thread messages since last mention")
	gt.String(t, got).Contains("alice: still here")
	gt.String(t, got).Contains("U2: no name")
	gt.String(t, got).Contains("# Current mention")
	gt.String(t, got).Contains("@bot follow up")
}

func TestBuildUserInput_SkipsCurrentMentionInDelta(t *testing.T) {
	currentTS := "1700000020.000001"
	delta := []casebound.ConversationMessage{
		{UserID: "U1", UserName: "alice", Text: "older", Timestamp: "1700000015.000001"},
		{UserID: "U1", UserName: "alice", Text: "current message text", Timestamp: currentTS},
	}
	got := casebound.BuildUserInputForTest(delta, "current message text", currentTS)
	// The delta line for the current mention TS must not be duplicated.
	occurrences := strings.Count(got, "current message text")
	gt.Number(t, occurrences).Equal(1)
}

func TestBuildSystemPrompt_EditableFieldsAndStatuses(t *testing.T) {
	statusSet, err := model.NewActionStatusSet("open", []string{"closed"}, []model.ActionStatusDefinition{
		{ID: "open", Name: "Open", Description: "Work has not started"},
		{ID: "closed", Name: "Closed", Description: "Work is fully resolved"},
	})
	gt.NoError(t, err).Required()

	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		FieldSchema: &config.FieldSchema{
			Fields: []config.FieldDefinition{
				{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect, Required: true, Description: "How urgent the case is", Options: []config.FieldOption{
					{ID: "high", Name: "High", Description: "Needs immediate attention"},
					{ID: "low", Name: "Low", Description: "Can wait"},
				}},
			},
		},
		CaseStatusSet: statusSet,
	}
	c := &model.Case{Title: "Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, nil, nil, nil)

	gt.String(t, prompt).Contains("Editable Custom Fields")
	gt.String(t, prompt).Contains("`severity`")
	// The field-level description must reach the agent.
	gt.String(t, prompt).Contains("How urgent the case is")
	gt.String(t, prompt).Contains("(required)")
	// Each select option must surface its id, name, and description.
	gt.String(t, prompt).Contains("`high`")
	gt.String(t, prompt).Contains(`name="High"`)
	gt.String(t, prompt).Contains("Needs immediate attention")
	gt.String(t, prompt).Contains("`low`")
	gt.String(t, prompt).Contains("Can wait")
	gt.String(t, prompt).Contains("Board Statuses")
	gt.String(t, prompt).Contains("`closed`")
	gt.String(t, prompt).Contains("(closed)")
	// Status descriptions must reach the agent so it knows when to pick one.
	gt.String(t, prompt).Contains("Work has not started")
	gt.String(t, prompt).Contains("Work is fully resolved")
}

// [workspace] description and [case.prompts].mention reach the case-channel
// agent: the description right after the opening as the workspace's own
// conventions, the mention prompt as the very last section so the operator's
// text has the final word (after the shared Slack formatting rules, too).
func TestBuildSystemPrompt_WorkspaceDescriptionAndMentionPrompt(t *testing.T) {
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)
	entry := &model.WorkspaceEntry{
		Workspace: model.Workspace{
			ID:          "ws-test",
			Description: "  Review actions are titled `[review] <item> (review #<parent action>)`.\n",
		},
		CaseMentionPrompt: "\nAnswer in at most five bullet points.\n",
	}

	prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, nil, nil, nil)

	gt.String(t, prompt).Contains("You are responding in a Slack channel dedicated to the following case.\n\n" +
		"## Workspace\n" +
		"This case belongs to a workspace that describes itself as follows. Follow the conventions it sets.\n\n" +
		"Review actions are titled `[review] <item> (review #<parent action>)`.\n\n" +
		"## Slack Context\n")
	gt.Bool(t, strings.HasSuffix(prompt, slackfmt.Section()+"\n\n"+
		"## Workspace-specific instructions\n"+
		"These instructions come from the workspace configuration. Follow them; where they conflict with the general instructions above (for example on reply length or format), they take precedence.\n\n"+
		"Answer in at most five bullet points.\n")).True()
}

// Without either value, neither heading is rendered and the prompt still ends
// on the shared Slack formatting section, exactly as before the feature.
func TestBuildSystemPrompt_NoWorkspaceDescriptionOrMentionPrompt(t *testing.T) {
	c := &model.Case{Title: "Test Case", Status: types.CaseStatusOpen}
	now := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)
	for name, entry := range map[string]*model.WorkspaceEntry{
		"nil entry":   nil,
		"unset":       {Workspace: model.Workspace{ID: "ws-test"}},
		"blank texts": {Workspace: model.Workspace{ID: "ws-test", Description: "  \n"}, CaseMentionPrompt: "\t\n"},
	} {
		t.Run(name, func(t *testing.T) {
			prompt := casebound.BuildSystemPromptForTest(c, entry, "C0TEST", "1700000000.000100", now, nil, nil, nil)
			gt.Bool(t, strings.Contains(prompt, "## Workspace\n")).False()
			gt.Bool(t, strings.Contains(prompt, "## Workspace-specific instructions")).False()
			gt.String(t, prompt).Contains("You are responding in a Slack channel dedicated to the following case.\n\n## Slack Context\n")
			gt.Bool(t, strings.HasSuffix(prompt, slackfmt.Section()+"\n")).True()
		})
	}
}

// The workspace's text has to reach the model on a real turn, not only the
// template: StartTurn builds the prompt from the request's WorkspaceEntry and
// the run hands it to the model as the session's system prompt.
func TestStartTurnGivesTheModelTheWorkspaceInstructions(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		prompts []string
	)
	llm := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, opts ...gollem.SessionOption) (gollem.Session, error) {
			cfg := gollem.NewSessionConfig(opts...)
			mu.Lock()
			prompts = append(prompts, cfg.SystemPrompt())
			mu.Unlock()
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					return &gollem.Response{Texts: []string{"- short answer"}}, nil
				},
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
	h := newHarness(t, llm)
	ssn := h.session(t, ctx)

	req := h.request(ssn, "1700000001.000001")
	req.Workspace = &model.WorkspaceEntry{
		Workspace:         model.Workspace{ID: "ws-1", Name: "WS", Description: "Every case tracks one vendor review."},
		CaseMentionPrompt: "Answer in at most five bullet points.",
	}
	res, err := h.uc.StartTurn(ctx, req)
	gt.NoError(t, err).Required()
	proc := h.drive(t, res.ProcessID)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	mu.Lock()
	defer mu.Unlock()
	gt.Array(t, prompts).Length(1).Required()
	gt.String(t, prompts[0]).Contains("## Workspace\n")
	gt.String(t, prompts[0]).Contains("Every case tracks one vendor review.")
	gt.String(t, prompts[0]).Contains("## Workspace-specific instructions\n")
	gt.String(t, prompts[0]).Contains("Answer in at most five bullet points.")
}
