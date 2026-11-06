package usecase_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	agentprocmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/budget"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	slackmodel "github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/planexec"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
	goslack "github.com/slack-go/slack" //nolint:depguard
)

// agentTestSlackService is a mock Slack service for agent testing.
//
// postMu guards the recorded-call slices because the case-channel agent posts
// its reply from the agentkit worker goroutine, not from the goroutine that
// started the turn. Tests reading concurrently with a live worker must go
// through posts(); the single-goroutine tests still read the fields directly.
type agentTestSlackService struct {
	mockSlackService
	getConversationRepliesFn func(ctx context.Context, channelID string, threadTS string, limit int) ([]slack.ConversationMessage, error)
	getConversationHistoryFn func(ctx context.Context, channelID string, oldest time.Time, limit int) ([]slack.ConversationMessage, error)
	postThreadReplyFn        func(ctx context.Context, channelID string, threadTS string, text string) (string, error)
	getBotUserIDFn           func(ctx context.Context) (string, error)
	getPermalinkFn           func(ctx context.Context, channelID string, ts string) (string, error)
	// postEphemeralErr, when set, is returned by every PostEphemeral after
	// the attempt is recorded.
	postEphemeralErr  error
	postMu            sync.Mutex
	postID            atomic.Int32
	postedMessages    []agentPostedMessage
	updatedMessages   []agentUpdatedMessage
	ephemeralMessages []agentEphemeralMessage
	permalinkCalls    []agentPermalinkCall
}

// posts returns a snapshot of every posted message.
func (m *agentTestSlackService) posts() []agentPostedMessage {
	m.postMu.Lock()
	defer m.postMu.Unlock()
	out := make([]agentPostedMessage, len(m.postedMessages))
	copy(out, m.postedMessages)
	return out
}

func (m *agentTestSlackService) updates() []agentUpdatedMessage {
	m.postMu.Lock()
	defer m.postMu.Unlock()
	out := make([]agentUpdatedMessage, len(m.updatedMessages))
	copy(out, m.updatedMessages)
	return out
}

type agentPostedMessage struct {
	ChannelID string
	ThreadTS  string
	Text      string
	// Timestamp is the id this post was given, so a test can follow which of a
	// turn's messages a later UpdateMessage rewrote.
	Timestamp string
	Blocks    []goslack.Block
}

type agentPermalinkCall struct {
	ChannelID string
	MessageTS string
}

type agentUpdatedMessage struct {
	ChannelID string
	Timestamp string
	Text      string
	Blocks    []goslack.Block
}

func (m *agentTestSlackService) GetConversationReplies(ctx context.Context, channelID string, threadTS string, limit int) ([]slack.ConversationMessage, error) {
	if m.getConversationRepliesFn != nil {
		return m.getConversationRepliesFn(ctx, channelID, threadTS, limit)
	}
	return nil, nil
}

func (m *agentTestSlackService) GetConversationHistory(ctx context.Context, channelID string, oldest time.Time, limit int) ([]slack.ConversationMessage, error) {
	if m.getConversationHistoryFn != nil {
		return m.getConversationHistoryFn(ctx, channelID, oldest, limit)
	}
	return nil, nil
}

func (m *agentTestSlackService) PostThreadReply(ctx context.Context, channelID string, threadTS string, text string) (string, error) {
	m.postMu.Lock()
	m.postedMessages = append(m.postedMessages, agentPostedMessage{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Text:      text,
	})
	m.postMu.Unlock()
	if m.postThreadReplyFn != nil {
		return m.postThreadReplyFn(ctx, channelID, threadTS, text)
	}
	return "1234567890.trace01", nil
}

func (m *agentTestSlackService) PostThreadMessage(ctx context.Context, channelID string, threadTS string, blocks []goslack.Block, text string, opts ...slack.PostThreadOption) (string, error) {
	// A distinct id per post, so a test can tell which message a later update
	// rewrote — the whole point of a turn drawing its progress into ONE of them.
	ts := "1234567890.post" + strconv.Itoa(int(m.postID.Add(1)))
	m.postMu.Lock()
	m.postedMessages = append(m.postedMessages, agentPostedMessage{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Text:      text,
		Timestamp: ts,
		Blocks:    blocks,
	})
	m.postMu.Unlock()
	return ts, nil
}

func (m *agentTestSlackService) UpdateMessage(ctx context.Context, channelID string, timestamp string, blocks []goslack.Block, text string) error {
	m.postMu.Lock()
	m.updatedMessages = append(m.updatedMessages, agentUpdatedMessage{
		ChannelID: channelID,
		Timestamp: timestamp,
		Text:      text,
		Blocks:    blocks,
	})
	m.postMu.Unlock()
	return nil
}

func (m *agentTestSlackService) OpenView(ctx context.Context, triggerID string, view goslack.ModalViewRequest) error {
	return nil
}

func (m *agentTestSlackService) UpdateView(_ context.Context, _ goslack.ModalViewRequest, _, _, _ string) error {
	return nil
}

func (m *agentTestSlackService) PostEphemeral(_ context.Context, channelID string, userID string, text string, opts ...slack.PostEphemeralOption) error {
	m.postMu.Lock()
	defer m.postMu.Unlock()
	cfg := slack.ApplyPostEphemeralOptions(opts...)
	m.ephemeralMessages = append(m.ephemeralMessages, agentEphemeralMessage{ChannelID: channelID, ThreadTS: cfg.ThreadTS, UserID: userID, Text: text})
	return m.postEphemeralErr
}

// ephemerals returns a snapshot of every ephemeral message posted.
func (m *agentTestSlackService) ephemerals() []agentEphemeralMessage {
	m.postMu.Lock()
	defer m.postMu.Unlock()
	out := make([]agentEphemeralMessage, len(m.ephemeralMessages))
	copy(out, m.ephemeralMessages)
	return out
}

type agentEphemeralMessage struct {
	ChannelID string
	ThreadTS  string
	UserID    string
	Text      string
}

func (m *agentTestSlackService) PostEphemeralBlocks(_ context.Context, _ string, _ string, _ []goslack.Block, _ string) (string, error) {
	return "ts-eph", nil
}

func (m *agentTestSlackService) GetPermalink(ctx context.Context, channelID string, ts string) (string, error) {
	m.permalinkCalls = append(m.permalinkCalls, agentPermalinkCall{ChannelID: channelID, MessageTS: ts})
	if m.getPermalinkFn != nil {
		return m.getPermalinkFn(ctx, channelID, ts)
	}
	return "https://slack.test/" + channelID + "/" + ts, nil
}

func (m *agentTestSlackService) GetBotUserID(ctx context.Context) (string, error) {
	if m.getBotUserIDFn != nil {
		return m.getBotUserIDFn(ctx)
	}
	return "UBOT001", nil
}

// mockLLMSession is a mock gollem Session for testing
type mockLLMSession struct {
	generateContentFn func(ctx context.Context, input ...gollem.Input) (*gollem.Response, error)
}

func (s *mockLLMSession) Generate(ctx context.Context, input []gollem.Input, opts ...gollem.GenerateOption) (*gollem.Response, error) {
	if s.generateContentFn != nil {
		return s.generateContentFn(ctx, input...)
	}
	return &gollem.Response{
		Texts: []string{"This is a test response from the AI agent."},
	}, nil
}

func (s *mockLLMSession) Stream(ctx context.Context, input []gollem.Input, opts ...gollem.GenerateOption) (<-chan *gollem.Response, error) {
	return nil, nil
}

func (s *mockLLMSession) GenerateContent(ctx context.Context, input ...gollem.Input) (*gollem.Response, error) {
	return s.Generate(ctx, input)
}

func (s *mockLLMSession) GenerateStream(ctx context.Context, input ...gollem.Input) (<-chan *gollem.Response, error) {
	return s.Stream(ctx, input)
}

// History returns an empty-but-valid transcript. It must not be nil: the
// agentkit runtime persists the conversation after every transition, and a
// version with no gollem History.Version cannot be loaded back.
func (s *mockLLMSession) History() (*gollem.History, error) {
	return &gollem.History{LLType: gollem.LLMTypeOpenAI, Version: gollem.HistoryVersion}, nil
}

func (s *mockLLMSession) AppendHistory(*gollem.History) error {
	return nil
}

func (s *mockLLMSession) CountToken(ctx context.Context, input ...gollem.Input) (int, error) {
	return 0, nil
}

// mockLLMClient is a mock gollem LLMClient for testing
type mockLLMClient struct {
	newSessionFn func(ctx context.Context, options ...gollem.SessionOption) (gollem.Session, error)
}

func (c *mockLLMClient) NewSession(ctx context.Context, options ...gollem.SessionOption) (gollem.Session, error) {
	if c.newSessionFn != nil {
		return c.newSessionFn(ctx, options...)
	}
	return &mockLLMSession{}, nil
}

func (c *mockLLMClient) GenerateEmbedding(ctx context.Context, dimension int, input []string) ([][]float64, error) {
	return nil, nil
}

// agentRuntimeDeps is what startAgentRuntime needs to reproduce serve.go's
// wiring order: register the agents, build the Kernel from the filled registry,
// bind it, then run the worker.
type agentRuntimeDeps struct {
	UC       *usecase.AgentUseCase
	Repo     interfaces.Repository
	Registry *model.WorkspaceRegistry
	LLM      gollem.LLMClient
	Trace    trace.Repository
	// CaseUC wires the single-case writer tools (case__*). A test whose agent
	// mutates the case through a tool call needs it; one that only reads does not.
	CaseUC *usecase.CaseUseCase
}

// testAgentBudget is generous enough that no test turn ends on the ceiling; the
// budget's own behaviour is covered in pkg/agent/budget.
var testAgentBudget = budget.Config{
	MaxSteps: 32, MaxInputTokens: 100_000, MaxOutputTokens: 100_000, NoticeRatio: 0.8,
}

// testAgentRootBudget is the root tier of the same figures. Its spend ceiling is
// money, which testAgentModelPolicy supplies.
var testAgentRootBudget = budget.Root{MaxSteps: 32, NoticeRatio: 0.8}

// testAgentModelPolicy is the one-model policy every test kernel here runs under:
// a priced default model and a budget far above anything a fake LLM spends.
func testAgentModelPolicy(t *testing.T) agentkernel.ModelPolicy {
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

// startAgentRuntime wires and runs the agentkit runtime for the duration of the
// test. Without it a mention turn is never executed: HandleAgentMention only
// records the run, and the worker is what drives it to an answer.
func startAgentRuntime(t *testing.T, d agentRuntimeDeps) {
	t.Helper()

	k := bindAgentRuntimeWithoutWorker(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Serve(ctx, agentkit.WithPollInterval(5*time.Millisecond)) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// bindAgentRuntimeWithoutWorker wires the runtime but never claims anything, so a
// spawned run stays exactly where the host left it. Use it to assert what the
// HOST did — the Session it claimed, the record it opened — without racing the
// agent that would otherwise be finishing the run at the same time.
func bindAgentRuntimeWithoutWorker(t *testing.T, d agentRuntimeDeps) *agentkit.Kernel {
	t.Helper()
	k, _ := bindAgentRuntimeWithProcesses(t, d)
	return k
}

// bindAgentRuntimeWithProcesses is bindAgentRuntimeWithoutWorker that also
// returns the process store, so a test can read back what a host spawned.
func bindAgentRuntimeWithProcesses(t *testing.T, d agentRuntimeDeps) (*agentkit.Kernel, agentkit.Repository) {
	t.Helper()

	procRepo := agentprocmemory.New()
	history := agentarchive.NewMemoryHistoryStore()
	reg := agentkit.NewRegistry()
	models := testAgentModelPolicy(t)
	taskAgent, err := agentkernel.RegisterTaskAgent(reg, testAgentBudget.Limiter(models.Resolve), history)
	gt.NoError(t, err).Required()
	gt.NoError(t, d.UC.RegisterAgents(reg, testAgentRootBudget.Limiter(models.Resolve), models,
		history, procRepo, taskAgent)).Required()

	traceRepo := d.Trace
	if traceRepo == nil {
		traceRepo = agentarchive.NewMemoryTraceRepository()
	}
	k, err := agentkernel.Build(agentkernel.Deps{
		Repo:    procRepo,
		History: history,
		LLM:     d.LLM,
		Trace:   traceRepo,
		Budgets: agentkernel.Budgets{Root: testAgentRootBudget, Task: testAgentBudget},
		Models:  models,
		Agents:  reg,
		Tools: agentkernel.ToolDeps{
			Repo: d.Repo, Registry: d.Registry,
			CaseUC: usecase.NewCaseToolAdapter(d.CaseUC),
		},
	})
	gt.NoError(t, err).Required()
	d.UC.BindAgentKernel(k, nil)
	return k, procRepo
}

// spawnedTurn is what a Slack-thread host recorded when it spawned the turn
// for one thread: the run's scope and the language directive of its input.
type spawnedTurn struct {
	Scope         agentkernel.Scope
	LanguageLabel string
}

// threadTurn reads back the Process a Slack-thread host spawned for the
// trigger (channelID, threadTS, triggerTS). Every such host spawns with
// agentkernel.TriggerKey as its idempotency key, which is what finds it here.
func threadTurn(t *testing.T, k *agentkit.Kernel, procs agentkit.Repository, channelID, threadTS, triggerTS string) spawnedTurn {
	t.Helper()
	ctx := context.Background()
	locator, err := agentkernel.NewLocator(procs)
	gt.NoError(t, err).Required()
	pid, err := locator.ByTrigger(ctx, agentkernel.TriggerKey(channelID, threadTS, triggerTS))
	gt.NoError(t, err).Required()
	gt.String(t, string(pid)).NotEqual("").Required()
	proc, err := k.GetProcess(ctx, pid)
	gt.NoError(t, err).Required()

	var state struct {
		Input struct {
			LanguageLabel string `json:"language_label"`
		} `json:"input"`
	}
	gt.NoError(t, json.Unmarshal(proc.State, &state)).Required()
	return spawnedTurn{Scope: agentkernel.ScopeFrom(proc.Metadata), LanguageLabel: state.Input.LanguageLabel}
}

// waitForLLMFlag blocks until a probe LLM has been asked for a session, which is
// the signal that the worker picked the run up. The dispatch decision under test
// happens synchronously; the agent it dispatched to does not.
func waitForLLMFlag(t *testing.T, invoked *atomic.Bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !invoked.Load() {
		if time.Now().After(deadline) {
			gt.Bool(t, invoked.Load()).True().Required()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForPostContaining blocks until the mock has recorded a message carrying
// the given text, and returns it.
func waitForPostContaining(t *testing.T, m *agentTestSlackService, want string) agentPostedMessage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, p := range m.posts() {
			if strings.Contains(p.Text, want) {
				return p
			}
		}
		if time.Now().After(deadline) {
			gt.Value(t, "no post containing "+want).Equal("a matching post").Required()
			return agentPostedMessage{}
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// waitForPostIn blocks until the mock has recorded a message posted into the
// given thread, whichever position it lands in.
func waitForPostIn(t *testing.T, m *agentTestSlackService, channelID, threadTS string) agentPostedMessage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, p := range m.posts() {
			if p.ChannelID == channelID && p.ThreadTS == threadTS {
				return p
			}
		}
		if time.Now().After(deadline) {
			gt.Value(t, "no post in "+channelID+"/"+threadTS).Equal("a post").Required()
			return agentPostedMessage{}
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// waitForUpdate blocks until the mock has recorded an in-place update of the
// given message, which is how a durable run replaces a placeholder it posted
// before the run started.
func waitForUpdate(t *testing.T, m *agentTestSlackService, channelID, ts string) agentUpdatedMessage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, u := range m.updates() {
			if u.ChannelID == channelID && u.Timestamp == ts {
				return u
			}
		}
		if time.Now().After(deadline) {
			gt.Value(t, "no update for "+channelID+"/"+ts).Equal("an update").Required()
			return agentUpdatedMessage{}
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// waitForPosts blocks until the mock has recorded at least want messages. The
// reply arrives from the worker goroutine, so there is nothing to join on.
func waitForPosts(t *testing.T, m *agentTestSlackService, want int) []agentPostedMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := m.posts()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			gt.Array(t, got).Length(want).Required()
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAgentUseCase_HandleAgentMention(t *testing.T) {
	t.Run("responds to mention in channel with case", func(t *testing.T) {
		repo := memory.New()
		ctx := context.Background()

		// Create a case with a Slack channel
		_, err := repo.Case().Create(ctx, "ws-test", &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Security Incident",
			Description:    "A test security incident",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C-AGENT-001",
		})
		gt.NoError(t, err).Required()

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
			FieldSchema: &config.FieldSchema{
				Fields: []config.FieldDefinition{
					{ID: "severity", Name: "Severity", Type: types.FieldTypeSelect},
				},
			},
		})

		slackMock := &agentTestSlackService{}
		slackMock.getConversationHistoryFn = func(ctx context.Context, channelID string, oldest time.Time, limit int) ([]slack.ConversationMessage, error) {
			return []slack.ConversationMessage{
				{UserID: "U001", UserName: "alice", Text: "Something happened", Timestamp: "1234567890.000001"},
				{UserID: "U002", UserName: "bob", Text: "@bot what do you think?", Timestamp: "1234567890.000002"},
			}, nil
		}

		llmClient := &mockLLMClient{}

		agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
			Repo:         repo,
			Registry:     registry,
			LLM:          llmClient,
			EmbedClient:  llmClient,
			HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
			TraceRepo:    agentarchive.NewMemoryTraceRepository(),
			SlackService: slackMock,
		})
		startAgentRuntime(t, agentRuntimeDeps{
			UC: agentUC, Repo: repo, Registry: registry, LLM: llmClient,
		})

		msg := slackmodel.NewMessageFromData(
			"1234567890.000002",
			"C-AGENT-001",
			"", // no thread TS (channel mention)
			"T123",
			"U002",
			"bob",
			"@bot what do you think?",
			"1234567890.000002",
			time.Now(),
			nil,
		)

		gt.NoError(t, agentUC.HandleAgentMention(ctx, msg)).Required()

		// Session start posts synchronously; the answer arrives from the worker.
		posts := waitForPosts(t, slackMock, 2)
		gt.Array(t, posts).Length(2).Required()
		// First message: session start (via PostThreadMessage)
		gt.Value(t, posts[0].ChannelID).Equal("C-AGENT-001")
		gt.Value(t, posts[0].Text).NotEqual("") // session start (random label)
		// Second message: final response (via PostThreadReply)
		gt.Value(t, posts[1].ChannelID).Equal("C-AGENT-001")
		gt.Value(t, posts[1].Text).Equal("This is a test response from the AI agent.")

		// The run also records the per-call timeline the case agent page reads.
		// The Sequence numbers come from the repository, not from an in-process
		// counter, which is what lets a run that moves between instances keep one
		// ordered timeline.
		runs, err := repo.JobRun().ListByCase(ctx, "ws-test", 1)
		gt.NoError(t, err).Required()
		gt.Array(t, runs).Length(1).Required()
		events, err := repo.JobRunEvent().List(ctx,
			model.JobRunKey{WorkspaceID: "ws-test", CaseID: 1, JobID: runs[0].JobID},
			runs[0].LastRunID)
		gt.NoError(t, err).Required()
		// One LLM call, recorded as a request and a response.
		gt.Array(t, events).Length(2).Required()
		gt.Value(t, events[0].Kind).Equal(model.JobRunEventKindLLMRequest)
		gt.Value(t, events[1].Kind).Equal(model.JobRunEventKindLLMResponse)
		gt.Value(t, events[0].Sequence).Equal(int64(1))
		gt.Value(t, events[1].Sequence).Equal(int64(2))
	})

	// A Slack workflow or another app posts its mention with no user id. It used
	// to be refused before the turn was spawned, so the thread got an internal
	// error instead of an answer. It must be answered, acting as the case
	// channel (which may read even a private case bound to that channel).
	t.Run("responds to a mention posted by an app with no user", func(t *testing.T) {
		repo := memory.New()
		ctx := context.Background()

		created, err := repo.Case().Create(ctx, "ws-test", &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Sample case 1",
			Description:    "A private sample case",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C-AGENT-APP",
			IsPrivate:      true,
			ChannelUserIDs: []string{"U-TEST-DEFAULT"},
		})
		gt.NoError(t, err).Required()

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		})

		slackMock := &agentTestSlackService{}
		slackMock.getConversationHistoryFn = func(ctx context.Context, channelID string, oldest time.Time, limit int) ([]slack.ConversationMessage, error) {
			return []slack.ConversationMessage{
				{Text: "@bot please review this request", Timestamp: "1234567890.000020"},
			}, nil
		}

		llmClient := &mockLLMClient{}

		agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
			Repo:         repo,
			Registry:     registry,
			LLM:          llmClient,
			EmbedClient:  llmClient,
			HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
			TraceRepo:    agentarchive.NewMemoryTraceRepository(),
			SlackService: slackMock,
		})
		startAgentRuntime(t, agentRuntimeDeps{
			UC: agentUC, Repo: repo, Registry: registry, LLM: llmClient,
		})

		msg := slackmodel.NewMessageFromData(
			"1234567890.000020",
			"C-AGENT-APP",
			"",
			"T123",
			"", // an app-authored post names no user
			"",
			"@bot please review this request",
			"1234567890.000020",
			time.Now(),
			nil,
		)

		gt.NoError(t, agentUC.HandleAgentMention(ctx, msg)).Required()

		posts := waitForPosts(t, slackMock, 2)
		gt.Array(t, posts).Length(2).Required()
		gt.Value(t, posts[0].ChannelID).Equal("C-AGENT-APP")
		gt.Value(t, posts[1].ChannelID).Equal("C-AGENT-APP")
		gt.Value(t, posts[1].Text).Equal("This is a test response from the AI agent.")

		// The run was recorded against the case, so it really ran.
		runs, err := repo.JobRun().ListByCase(ctx, "ws-test", created.ID)
		gt.NoError(t, err).Required()
		gt.Array(t, runs).Length(1)
	})

	t.Run("responds to mention in thread", func(t *testing.T) {
		repo := memory.New()
		ctx := context.Background()

		_, err := repo.Case().Create(ctx, "ws-test", &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Bug Report",
			Description:    "A test bug",
			Status:         types.CaseStatusOpen,
			SlackChannelID: "C-AGENT-002",
		})
		gt.NoError(t, err).Required()

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		})

		slackMock := &agentTestSlackService{}
		slackMock.getConversationRepliesFn = func(ctx context.Context, channelID string, threadTS string, limit int) ([]slack.ConversationMessage, error) {
			gt.Value(t, threadTS).Equal("1234567890.000010")
			return []slack.ConversationMessage{
				{UserID: "U001", UserName: "alice", Text: "Found a bug", Timestamp: "1234567890.000010"},
				{UserID: "U002", UserName: "bob", Text: "@bot help", Timestamp: "1234567890.000011"},
			}, nil
		}

		llmClient := &mockLLMClient{}

		agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
			Repo:         repo,
			Registry:     registry,
			LLM:          llmClient,
			EmbedClient:  llmClient,
			HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
			TraceRepo:    agentarchive.NewMemoryTraceRepository(),
			SlackService: slackMock,
		})
		startAgentRuntime(t, agentRuntimeDeps{
			UC: agentUC, Repo: repo, Registry: registry, LLM: llmClient,
		})

		msg := slackmodel.NewMessageFromData(
			"1234567890.000011",
			"C-AGENT-002",
			"1234567890.000010", // thread TS
			"T123",
			"U002",
			"bob",
			"@bot help",
			"1234567890.000011",
			time.Now(),
			nil,
		)

		gt.NoError(t, agentUC.HandleAgentMention(ctx, msg)).Required()

		posts := waitForPosts(t, slackMock, 2)
		gt.Array(t, posts).Length(2).Required()
		// First message: session start (via PostThreadMessage)
		gt.Value(t, posts[0].ThreadTS).Equal("1234567890.000010")
		gt.Value(t, posts[0].Text).NotEqual("") // session start (random label)
		// Second message: final response (via PostThreadReply)
		gt.Value(t, posts[1].ThreadTS).Equal("1234567890.000010")
		gt.Value(t, posts[1].Text).Equal("This is a test response from the AI agent.")
	})

	t.Run("skips when no case found for channel", func(t *testing.T) {
		repo := memory.New()
		ctx := context.Background()

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		})

		slackMock := &agentTestSlackService{}
		llmClient := &mockLLMClient{}

		agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
			Repo:         repo,
			Registry:     registry,
			LLM:          llmClient,
			EmbedClient:  llmClient,
			HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
			TraceRepo:    agentarchive.NewMemoryTraceRepository(),
			SlackService: slackMock,
		})
		// The runtime is wired so the skip is genuinely exercised: without it
		// HandleAgentMention short-circuits before reaching the channel lookup.
		startAgentRuntime(t, agentRuntimeDeps{
			UC: agentUC, Repo: repo, Registry: registry, LLM: llmClient,
		})

		msg := slackmodel.NewMessageFromData(
			"1234567890.000100",
			"C-UNKNOWN",
			"",
			"T123",
			"U002",
			"bob",
			"@bot hello",
			"1234567890.000100",
			time.Now(),
			nil,
		)

		gt.NoError(t, agentUC.HandleAgentMention(ctx, msg)).Required()

		// No messages should be posted
		gt.Array(t, slackMock.postedMessages).Length(0)
	})

	t.Run("skips bot's own message", func(t *testing.T) {
		repo := memory.New()
		ctx := context.Background()

		_, err := repo.Case().Create(ctx, "ws-test", &model.Case{
			ReporterID:     "U-TEST-DEFAULT",
			Title:          "Test Case",
			SlackChannelID: "C-AGENT-003",
		})
		gt.NoError(t, err).Required()

		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws-test", Name: "Test"},
		})

		slackMock := &agentTestSlackService{}
		slackMock.getBotUserIDFn = func(ctx context.Context) (string, error) {
			return "UBOT001", nil
		}

		llmClient := &mockLLMClient{}

		agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
			Repo:         repo,
			Registry:     registry,
			LLM:          llmClient,
			EmbedClient:  llmClient,
			HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
			TraceRepo:    agentarchive.NewMemoryTraceRepository(),
			SlackService: slackMock,
		})
		startAgentRuntime(t, agentRuntimeDeps{
			UC: agentUC, Repo: repo, Registry: registry, LLM: llmClient,
		})

		msg := slackmodel.NewMessageFromData(
			"1234567890.000200",
			"C-AGENT-003",
			"",
			"T123",
			"UBOT001", // bot's own user ID
			"bot",
			"I already responded",
			"1234567890.000200",
			time.Now(),
			nil,
		)

		gt.NoError(t, agentUC.HandleAgentMention(ctx, msg)).Required()

		// No messages should be posted
		gt.Array(t, slackMock.postedMessages).Length(0)
	})

	// Note: system prompt assembly tests (case info / field values / channel
	// ID / actions / current action / due date / unassigned) live in
	// pkg/usecase/agent/casebound/casebound_test.go now that buildSystemPrompt
	// has moved into the casebound subpackage.
}

func TestParseAgentActionValue(t *testing.T) {
	t.Run("parses action with data", func(t *testing.T) {
		action, data, err := usecase.ParseAgentActionValue("show_session_info:abc-123-def")
		gt.NoError(t, err)
		gt.Value(t, action).Equal("show_session_info")
		gt.Value(t, data).Equal("abc-123-def")
	})

	t.Run("parses action without data", func(t *testing.T) {
		action, data, err := usecase.ParseAgentActionValue("send_feedback")
		gt.NoError(t, err)
		gt.Value(t, action).Equal("send_feedback")
		gt.Value(t, data).Equal("")
	})

	t.Run("parses action with multiple colons in data", func(t *testing.T) {
		action, data, err := usecase.ParseAgentActionValue("show_session_info:0193a7b0-7c3d-7e8f-9a1b-2c3d4e5f6a7b")
		gt.NoError(t, err)
		gt.Value(t, action).Equal("show_session_info")
		gt.Value(t, data).Equal("0193a7b0-7c3d-7e8f-9a1b-2c3d4e5f6a7b")
	})
}

// A milestone renders as one context block carrying one mrkdwn element. The
// compact, de-emphasised context rendering is what keeps a run's progress from
// reading like the agent's own reply.
func TestProgressBlock(t *testing.T) {
	block := usecase.ProgressBlockForTest("\U0001f527 `tool_a`")

	ctxBlock, ok := block.(*goslack.ContextBlock)
	gt.Bool(t, ok).True().Required()
	gt.Value(t, ctxBlock.Type).Equal(goslack.MBTContext)
	gt.Array(t, ctxBlock.ContextElements.Elements).Length(1).Required()

	text, ok := ctxBlock.ContextElements.Elements[0].(*goslack.TextBlockObject)
	gt.Bool(t, ok).True().Required()
	gt.Value(t, text.Type).Equal(goslack.MarkdownType)
	gt.String(t, text.Text).Equal("\U0001f527 `tool_a`")
}

func TestAgentUseCase_HandleSessionInfoRequest(t *testing.T) {
	t.Run("opens modal with session ID", func(t *testing.T) {
		repo := memory.New()
		slackMock := &agentTestSlackService{}
		mockWithCapture := &agentTestSlackServiceWithOpenView{
			agentTestSlackService: slackMock,
		}

		llmClient := &mockLLMClient{}
		i18n.Init(i18n.LangEN)
		agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
			Repo:         repo,
			LLM:          llmClient,
			EmbedClient:  llmClient,
			HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
			TraceRepo:    agentarchive.NewMemoryTraceRepository(),
			SlackService: mockWithCapture,
		})

		err := agentUC.HandleSessionInfoRequest(t.Context(), "trigger-123", "test-session-id", "", "")
		gt.NoError(t, err)

		gt.Value(t, mockWithCapture.openViewCalled).Equal(true)
		gt.Value(t, mockWithCapture.openViewTriggerID).Equal("trigger-123")
		gt.Value(t, mockWithCapture.openViewRequest.Title.Text).Equal("Session Info")
	})

	// The modal is written in the language of the workspace the thread's Session
	// belongs to; a thread whose Session is a different one leaves it alone.
	t.Run("follows the workspace language of the thread's session", func(t *testing.T) {
		i18n.Init(i18n.LangEN)
		open := func(t *testing.T, sessionID string) string {
			t.Helper()
			repo := memory.New()
			registry := model.NewWorkspaceRegistry()
			registry.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws-ja"}, Language: i18n.LangJA})
			gt.NoError(t, repo.Session().Put(t.Context(), &model.Session{
				ID: "ssn-info", ChannelID: "C-INFO", ThreadTS: "1700000000.000100", WorkspaceID: "ws-ja",
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			})).Required()
			mock := &agentTestSlackServiceWithOpenView{agentTestSlackService: &agentTestSlackService{}}
			agentUC := usecase.NewAgentUseCase(usecase.AgentDeps{
				Repo:         repo,
				Registry:     registry,
				LLM:          &mockLLMClient{},
				HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
				TraceRepo:    agentarchive.NewMemoryTraceRepository(),
				SlackService: mock,
			})
			gt.NoError(t, agentUC.HandleSessionInfoRequest(t.Context(), "trigger-1", sessionID, "C-INFO", "1700000000.000100")).Required()
			return mock.openViewRequest.Title.Text
		}

		gt.String(t, open(t, "ssn-info")).Equal(
			i18n.T(i18n.ContextWithLang(context.Background(), i18n.LangJA), i18n.MsgAgentSessionInfo))
		gt.String(t, open(t, "another-session")).Equal("Session Info")
	})
}

// agentTestSlackServiceWithOpenView wraps agentTestSlackService with OpenView capture
type agentTestSlackServiceWithOpenView struct {
	*agentTestSlackService
	openViewCalled    bool
	openViewTriggerID string
	openViewRequest   goslack.ModalViewRequest
}

func (m *agentTestSlackServiceWithOpenView) OpenView(ctx context.Context, triggerID string, view goslack.ModalViewRequest) error {
	m.openViewCalled = true
	m.openViewTriggerID = triggerID
	m.openViewRequest = view
	return nil
}

// TestLifecycle_AgentSession exercises the AgentSession + History/Trace
// pipeline across two consecutive mentions on the same Slack thread:
//
//  1. First mention creates a new AgentSession, records its ID, and seeds the
//     prompt with the thread's full context (no delta).
//  2. A non-bot user message arrives in the thread between mentions.
//  3. Second mention reuses the same session, surfaces the intervening
//     message as a delta in the user input, and bumps LastMentionTS.
//
// It also asserts that gollem received the same sessionID for WithHistoryRepository
// on both turns (so persisted history is actually reused) and that a trace
// blob was written for each turn.
func TestLifecycle_AgentSession(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	historyRepo := agentarchive.NewMemoryHistoryRepository()
	traceRepo := agentarchive.NewMemoryTraceRepository()

	_, err := repo.Case().Create(ctx, "ws-lifecycle", &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Thread session test",
		Description:    "lifecycle",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-LIFE",
	})
	gt.NoError(t, err).Required()

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-lifecycle", Name: "Lifecycle"},
	})

	threadParent := "1700000001.000001"
	firstMentionTS := threadParent
	intermediateTS := "1700000002.000001"
	secondMentionTS := "1700000003.000001"

	repliesAfterFirst := []slack.ConversationMessage{
		{UserID: "U001", UserName: "alice", Text: "context message", Timestamp: threadParent},
		{UserID: "U001", UserName: "alice", Text: "@bot kicking off", Timestamp: firstMentionTS},
	}
	repliesAfterSecond := append(repliesAfterFirst,
		slack.ConversationMessage{UserID: "U002", UserName: "bob", Text: "extra info", Timestamp: intermediateTS},
		slack.ConversationMessage{UserID: "UBOT001", UserName: "bot", Text: "previous bot reply", Timestamp: "1700000002.500000"},
		slack.ConversationMessage{UserID: "U001", UserName: "alice", Text: "@bot follow up", Timestamp: secondMentionTS},
	)

	stage := 0 // 0 = before first mention runs, 1 = before second
	slackMock := &agentTestSlackService{
		getConversationRepliesFn: func(_ context.Context, _ string, _ string, _ int) ([]slack.ConversationMessage, error) {
			if stage == 0 {
				return repliesAfterFirst, nil
			}
			return repliesAfterSecond, nil
		},
	}

	// generated records the user input of every LLM turn, in order. The worker
	// runs on its own goroutine, so it is mutex-guarded.
	var genMu sync.Mutex
	var generated []string
	captureInput := func(input ...gollem.Input) {
		genMu.Lock()
		defer genMu.Unlock()
		text := ""
		if len(input) > 0 {
			if txt, ok := input[0].(gollem.Text); ok {
				text = string(txt)
			}
		}
		generated = append(generated, text)
	}
	generatedInputs := func() []string {
		genMu.Lock()
		defer genMu.Unlock()
		out := make([]string, len(generated))
		copy(out, generated)
		return out
	}

	llm := &mockLLMClient{
		newSessionFn: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mockLLMSession{
				generateContentFn: func(_ context.Context, input ...gollem.Input) (*gollem.Response, error) {
					captureInput(input...)
					return &gollem.Response{Texts: []string{"ack"}}, nil
				},
			}, nil
		},
	}

	uc := usecase.NewAgentUseCase(usecase.AgentDeps{
		Repo:         repo,
		Registry:     registry,
		LLM:          llm,
		EmbedClient:  llm,
		HistoryRepo:  historyRepo,
		TraceRepo:    traceRepo,
		SlackService: slackMock,
	})
	startAgentRuntime(t, agentRuntimeDeps{
		UC: uc, Repo: repo, Registry: registry, LLM: llm, Trace: traceRepo,
	})

	// --- First mention -----------------------------------------------------
	first := slackmodel.NewMessageFromData(
		firstMentionTS,
		"C-LIFE",
		"", // top-level mention; threadTS will be derived from msg.ID()
		"T-life",
		"U001",
		"alice",
		"@bot kicking off",
		firstMentionTS,
		time.Unix(1700000001, 0).UTC(),
		nil,
	)
	gt.NoError(t, uc.HandleAgentMention(ctx, first)).Required()

	posts1 := waitForPosts(t, slackMock, 2)
	gt.Array(t, posts1).Length(2).Required()
	gt.Value(t, posts1[1].Text).Equal("ack")

	session1, err := repo.Session().GetByThread(ctx, "C-LIFE", threadParent)
	gt.NoError(t, err).Required()
	gt.Value(t, session1).NotNil().Required()
	gt.Value(t, session1.LastMentionTS).Equal(firstMentionTS)
	gt.Value(t, session1.ChannelID).Equal("C-LIFE")
	gt.String(t, session1.ID).NotEqual("")

	// First turn LLM input is just the mention text (no delta).
	inputs1 := generatedInputs()
	gt.Array(t, inputs1).Length(1).Required()
	gt.String(t, inputs1[0]).Equal("@bot kicking off")

	// One trace archive per turn. It is keyed on the Process id now, not the
	// Slack session id, so the count is read off the whole repository — the
	// archive has no read path in the application, only a write one.
	gt.Map(t, traceRepo.Snapshot()).Length(1)

	// --- Second mention ----------------------------------------------------
	stage = 1
	second := slackmodel.NewMessageFromData(
		secondMentionTS,
		"C-LIFE",
		threadParent, // explicit thread reply
		"T-life",
		"U001",
		"alice",
		"@bot follow up",
		secondMentionTS,
		time.Unix(1700000003, 0).UTC(),
		nil,
	)
	gt.NoError(t, uc.HandleAgentMention(ctx, second)).Required()

	posts2 := waitForPosts(t, slackMock, 4)
	gt.Array(t, posts2).Length(4).Required()
	gt.Value(t, posts2[3].Text).Equal("ack")

	session2, err := repo.Session().GetByThread(ctx, "C-LIFE", threadParent)
	gt.NoError(t, err).Required()
	gt.Value(t, session2).NotNil().Required()
	gt.Value(t, session2.ID).Equal(session1.ID) // same session reused
	gt.Value(t, session2.LastMentionTS).Equal(secondMentionTS)

	// The second turn's input carries the message that arrived between the two
	// mentions as a delta, which is what makes the reused session observable.
	inputs2 := generatedInputs()
	gt.Array(t, inputs2).Length(2).Required()
	gt.String(t, inputs2[1]).Contains("Unprocessed thread messages")
	gt.String(t, inputs2[1]).Contains("extra info")
	gt.String(t, inputs2[1]).Contains("@bot follow up")
	// The bot's own earlier reply must not come back as a delta.
	gt.Bool(t, strings.Contains(inputs2[1], "previous bot reply")).False()

	// One trace archive per turn, and they are distinct objects.
	snapshot := traceRepo.Snapshot()
	gt.Map(t, snapshot).Length(2)
}

// TestAgentUseCase_DeltaMessageInjection asserts the delta path explicitly:
// continuing-session mentions surface only post-lastMentionTS, non-bot
// thread messages, and pass them as user input rather than re-stuffing the
// system prompt.
func TestAgentUseCase_DeltaMessageInjection(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	historyRepo := agentarchive.NewMemoryHistoryRepository()
	traceRepo := agentarchive.NewMemoryTraceRepository()

	c, err := repo.Case().Create(ctx, "ws-delta", &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Delta test",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-DELTA",
	})
	gt.NoError(t, err).Required()

	// Pre-seed an existing AgentSession so the next mention takes the
	// continuing-session path.
	const (
		threadTS        = "1700100000.000001"
		previousMention = "1700100005.000001"
		newMention      = "1700100020.000001"
	)
	gt.NoError(t, repo.Session().Put(ctx, &model.Session{
		ID:            "session-delta",
		WorkspaceID:   "ws-delta",
		CaseID:        c.ID,
		ThreadTS:      threadTS,
		ChannelID:     "C-DELTA",
		LastMentionTS: previousMention,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	})).Required()

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-delta", Name: "Delta"},
	})

	slackMock := &agentTestSlackService{
		getConversationRepliesFn: func(_ context.Context, _ string, _ string, _ int) ([]slack.ConversationMessage, error) {
			return []slack.ConversationMessage{
				// before previous mention — must be excluded
				{UserID: "U001", UserName: "alice", Text: "old chatter", Timestamp: "1700100002.000000"},
				// previous mention itself — must be excluded (== previousMention)
				{UserID: "U001", UserName: "alice", Text: "@bot earlier", Timestamp: previousMention},
				// bot reply between mentions — must be excluded (bot user)
				{UserID: "UBOT001", UserName: "bot", Text: "earlier reply", Timestamp: "1700100006.000000"},
				// real delta — must be included
				{UserID: "U002", UserName: "bob", Text: "interim update", Timestamp: "1700100010.000000"},
				// current mention — must be excluded (handled separately)
				{UserID: "U001", UserName: "alice", Text: "@bot now what", Timestamp: newMention},
			}, nil
		},
	}

	var inputMu sync.Mutex
	var capturedInput string
	llm := &mockLLMClient{
		newSessionFn: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mockLLMSession{
				generateContentFn: func(_ context.Context, input ...gollem.Input) (*gollem.Response, error) {
					if len(input) > 0 {
						if txt, ok := input[0].(gollem.Text); ok {
							inputMu.Lock()
							capturedInput = string(txt)
							inputMu.Unlock()
						}
					}
					return &gollem.Response{Texts: []string{"ok"}}, nil
				},
			}, nil
		},
	}

	uc := usecase.NewAgentUseCase(usecase.AgentDeps{
		Repo:         repo,
		Registry:     registry,
		LLM:          llm,
		EmbedClient:  llm,
		HistoryRepo:  historyRepo,
		TraceRepo:    traceRepo,
		SlackService: slackMock,
	})
	startAgentRuntime(t, agentRuntimeDeps{
		UC: uc, Repo: repo, Registry: registry, LLM: llm, Trace: traceRepo,
	})

	msg := slackmodel.NewMessageFromData(
		newMention,
		"C-DELTA",
		threadTS,
		"T-delta",
		"U001",
		"alice",
		"@bot now what",
		newMention,
		time.Unix(1700100020, 0).UTC(),
		nil,
	)
	gt.NoError(t, uc.HandleAgentMention(ctx, msg)).Required()

	// Wait for the worker to answer: session start, then the reply.
	waitForPosts(t, slackMock, 2)
	inputMu.Lock()
	userInput := capturedInput
	inputMu.Unlock()

	// Verify exactly the interim update (and not the bot reply, nor older
	// messages, nor the current mention itself) was included as a delta.
	gt.String(t, userInput).Contains("Unprocessed thread messages")
	gt.String(t, userInput).Contains("interim update")
	gt.String(t, userInput).Contains("@bot now what")
	// The previous mention, the bot's own reply, and pre-lastMentionTS chatter
	// must all be excluded.
	gt.Bool(t, strings.Contains(userInput, "@bot earlier")).False()
	gt.Bool(t, strings.Contains(userInput, "earlier reply")).False()
	gt.Bool(t, strings.Contains(userInput, "old chatter")).False()

	// Session updated with the new mention TS.
	updated, err := repo.Session().GetByThread(ctx, "C-DELTA", threadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, updated.LastMentionTS).Equal(newMention)
}

// TestAgentUseCase_ActionLinkage asserts that when a mention starts a thread
// whose parent TS matches an Action's notification message, the new session
// records that ActionID.
func TestAgentUseCase_ActionLinkage(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	historyRepo := agentarchive.NewMemoryHistoryRepository()
	traceRepo := agentarchive.NewMemoryTraceRepository()

	c, err := repo.Case().Create(ctx, "ws-action", &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "Action linkage",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-ACT",
	})
	gt.NoError(t, err).Required()

	const actionThreadTS = "1700200000.000001"

	createdAction, err := repo.Action().Create(ctx, "ws-action", &model.Action{
		CaseID:         c.ID,
		Title:          "Investigate",
		Status:         "open",
		SlackMessageTS: actionThreadTS,
	})
	gt.NoError(t, err).Required()

	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-action", Name: "Action"},
	})

	slackMock := &agentTestSlackService{}
	llm := &mockLLMClient{}
	uc := usecase.NewAgentUseCase(usecase.AgentDeps{
		Repo:         repo,
		Registry:     registry,
		LLM:          llm,
		EmbedClient:  llm,
		HistoryRepo:  historyRepo,
		TraceRepo:    traceRepo,
		SlackService: slackMock,
	})
	startAgentRuntime(t, agentRuntimeDeps{
		UC: uc, Repo: repo, Registry: registry, LLM: llm, Trace: traceRepo,
	})

	msg := slackmodel.NewMessageFromData(
		"1700200005.000001",
		"C-ACT",
		actionThreadTS,
		"T-act",
		"U001",
		"alice",
		"@bot help with this action",
		"1700200005.000001",
		time.Unix(1700200005, 0).UTC(),
		nil,
	)
	gt.NoError(t, uc.HandleAgentMention(ctx, msg)).Required()

	session, err := repo.Session().GetByThread(ctx, "C-ACT", actionThreadTS)
	gt.NoError(t, err).Required()
	gt.Value(t, session).NotNil().Required()
	gt.Value(t, session.ActionID).Equal(createdAction.ID)
}

// progressCapture wraps the existing mockSlackService (defined in source_test.go)
// and records every PostThreadMessage / UpdateMessage call so the progress tests
// can assert on the rendered text and call sequence. All other Service methods
// fall through to mockSlackService. Set failPost / failUpdate to make the
// corresponding call fail, which is what a Slack outage looks like from the
// caller's side.
type progressCapture struct {
	mockSlackService
	mu         sync.Mutex
	posts      []progressCall
	postID     atomic.Int32
	failPost   bool
	failUpdate bool
}

type progressCall struct {
	method  string
	ts      string
	blocks  []goslack.Block
	text    string
	channel string
}

func (s *progressCapture) calls() []progressCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]progressCall, len(s.posts))
	copy(out, s.posts)
	return out
}

func (s *progressCapture) PostThreadMessage(_ context.Context, channelID, _ string, blocks []goslack.Block, text string, _ ...slack.PostThreadOption) (string, error) {
	if s.failPost {
		return "", goerr.New("slack is unreachable")
	}
	// Use a deterministic, monotonically-increasing TS per post so successive
	// messages can be distinguished without relying on wall-clock timing.
	id := s.postID.Add(1)
	ts := "ts-" + string(rune('0'+id))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, progressCall{method: "post", ts: ts, blocks: blocks, text: text, channel: channelID})
	return ts, nil
}

func (s *progressCapture) UpdateMessage(_ context.Context, channelID, ts string, blocks []goslack.Block, text string) error {
	if s.failUpdate {
		return goerr.New("slack is unreachable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, progressCall{method: "update", ts: ts, blocks: blocks, text: text, channel: channelID})
	return nil
}

// progressBlockTexts extracts the rendered markdown text of every context block
// in order, so progress tests can assert on the visible lines.
func progressBlockTexts(blocks []goslack.Block) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		cb, ok := b.(*goslack.ContextBlock)
		if !ok {
			continue
		}
		for _, el := range cb.ContextElements.Elements {
			if tb, ok := el.(*goslack.TextBlockObject); ok {
				out = append(out, tb.Text)
			}
		}
	}
	return out
}

// A run's progress is ONE context block holding ONE line. The first render posts
// the message; every later one replaces that line in place, so a long run
// occupies the same space in the thread as a short one.
func TestAgentProgress_DrawsOneBlockAndReplacesItsLine(t *testing.T) {
	ctx := context.Background()
	capture := &progressCapture{}
	uc := usecase.NewAgentUseCase(usecase.AgentDeps{
		Repo:         memory.New(),
		SlackService: capture,
	})
	progress := usecase.NewAgentProgressForTest(uc)
	target := planexec.ProgressTarget{ChannelID: "C-PROGRESS", ThreadTS: "1700000000.000001"}

	ts, err := progress.Render(ctx, target, "", "🧭 Planning")
	gt.NoError(t, err).Required()

	calls := capture.calls()
	gt.Array(t, calls).Length(1).Required()
	gt.Value(t, calls[0].method).Equal("post")
	gt.Value(t, calls[0].channel).Equal("C-PROGRESS")
	gt.Array(t, calls[0].blocks).Length(1)
	gt.Array(t, progressBlockTexts(calls[0].blocks)).Equal([]string{"🧭 Planning"})
	// The line is the whole message, so it is also the notification fallback.
	gt.Value(t, calls[0].text).Equal("🧭 Planning")
	gt.Value(t, ts).Equal(calls[0].ts)

	next, err := progress.Render(ctx, target, ts, "📝 Writing the answer")
	gt.NoError(t, err).Required()

	calls = capture.calls()
	gt.Array(t, calls).Length(2).Required()
	gt.Value(t, calls[1].method).Equal("update")
	gt.Value(t, calls[1].ts).Equal(ts)
	// Still one block: the milestone replaced the previous one rather than being
	// appended below it.
	gt.Array(t, calls[1].blocks).Length(1)
	gt.Array(t, progressBlockTexts(calls[1].blocks)).Equal([]string{"📝 Writing the answer"})
	gt.Value(t, calls[1].text).Equal("📝 Writing the answer")
	gt.Value(t, next).Equal(ts)
}

// A Slack failure is reported to the caller rather than swallowed: the strategy
// is what decides that a failed draw is non-fatal, and it cannot decide that
// about a failure it never sees.
func TestAgentProgress_ReportsSlackFailures(t *testing.T) {
	ctx := context.Background()
	target := planexec.ProgressTarget{ChannelID: "C-PROGRESS", ThreadTS: "1700000000.000001"}

	t.Run("post", func(t *testing.T) {
		capture := &progressCapture{failPost: true}
		uc := usecase.NewAgentUseCase(usecase.AgentDeps{Repo: memory.New(), SlackService: capture})

		ts, err := usecase.NewAgentProgressForTest(uc).Render(ctx, target, "", "🧭 Planning")

		gt.Error(t, err)
		gt.Value(t, ts).Equal("")
	})

	t.Run("update", func(t *testing.T) {
		capture := &progressCapture{failUpdate: true}
		uc := usecase.NewAgentUseCase(usecase.AgentDeps{Repo: memory.New(), SlackService: capture})

		ts, err := usecase.NewAgentProgressForTest(uc).Render(ctx, target, "ts-1", "🧭 Planning")

		gt.Error(t, err)
		// The id comes back unchanged so the next milestone retries the same
		// message rather than posting a second one.
		gt.Value(t, ts).Equal("ts-1")
	})
}

// The create path's acknowledgement is the same one-block progress message the
// run then updates, which is what keeps a create turn to a single message.
func TestPostProgressAck(t *testing.T) {
	ctx := context.Background()

	t.Run("posts one context block and returns its ts", func(t *testing.T) {
		capture := &progressCapture{}
		uc := usecase.NewAgentUseCase(usecase.AgentDeps{Repo: memory.New(), SlackService: capture})

		ts := usecase.PostProgressAckForTest(uc, ctx, "C-PROGRESS", "1700000000.000001")

		calls := capture.calls()
		gt.Array(t, calls).Length(1).Required()
		gt.Value(t, calls[0].method).Equal("post")
		gt.Value(t, calls[0].channel).Equal("C-PROGRESS")
		gt.Array(t, calls[0].blocks).Length(1)
		// The expected text comes from the same translation source the production
		// code reads, so a reworded translation does not break this.
		gt.Array(t, progressBlockTexts(calls[0].blocks)).
			Equal([]string{i18n.T(ctx, i18n.MsgThreadCaseCreating)})
		gt.Value(t, ts).Equal(calls[0].ts)
	})

	t.Run("returns empty when Slack refuses the post", func(t *testing.T) {
		capture := &progressCapture{failPost: true}
		uc := usecase.NewAgentUseCase(usecase.AgentDeps{Repo: memory.New(), SlackService: capture})

		// Empty leaves the run to post its own progress message, which is the
		// degradation every other host already lives with.
		gt.Value(t, usecase.PostProgressAckForTest(uc, ctx, "C-PROGRESS", "1700000000.000001")).Equal("")
		gt.Array(t, capture.calls()).Length(0)
	})

	t.Run("returns empty with no Slack service", func(t *testing.T) {
		uc := usecase.NewAgentUseCase(usecase.AgentDeps{Repo: memory.New()})

		gt.Value(t, usecase.PostProgressAckForTest(uc, ctx, "C-PROGRESS", "1700000000.000001")).Equal("")
	})
}
