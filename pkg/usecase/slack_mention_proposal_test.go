package usecase_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	agentprocmemory "github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/proposal"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	goslack "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// draftPlan is the opening plan every draft fixture uses: one investigation, so
// the turn walks plan → sub-agent → replan → final output rather than the
// degenerate single-call shape.
const draftPlan = `{"message":"gathering context","tasks":[{"id":"t-1","title":"Read the thread","description":"read the thread","acceptance_criteria":"summarised","tools":["slack_ro"],"budget_usd":0.01}]}`

// draftReplanDone terminates the planner loop; the draft itself is the final
// output that follows it.
const draftReplanDone = `{"message":"ready to draft","finalize":{"reason":"enough context"}}`

// draftFinal renders the terminal Draft JSON for the given workspace.
func draftFinal(workspaceID, title, description, fieldsJSON string) string {
	return `{"workspace_id":"` + workspaceID + `","title":"` + title +
		`","description":"` + description + `","fields":` + fieldsJSON + `}`
}

// stubDraftScriptTitled is one complete draft turn: investigate once, then
// finalize into the given draft.
func stubDraftScriptTitled(workspaceID, title, description, fieldsJSON string) []string {
	return []string{
		draftPlan,
		"summary: the thread describes an incident.",
		draftReplanDone,
		draftFinal(workspaceID, title, description, fieldsJSON),
	}
}

// stubDraftScript is stubDraftScriptTitled with placeholder content, for tests
// that assert on the flow rather than on what was drafted. It proposes no field
// values, so it fits any workspace's schema.
func stubDraftScript(workspaceID string) []string {
	return stubDraftScriptTitled(workspaceID,
		"AI suggested title", "AI suggested description", `[]`)
}

// bindDraftRuntime registers the durable case-draft agent, builds the Kernel it
// runs on, binds it to the usecase and runs the worker for the test's lifetime.
//
// It reproduces serve.go's ordering — register, build, bind — because a Kernel
// built before registration has no agent to spawn.
// It returns the run locator so a test can assert the negative case: an event
// the dispatcher was supposed to drop leaves no run behind.
func bindDraftRuntime(
	t *testing.T, uc *usecase.MentionProposalUseCase, repo interfaces.Repository,
	registry *model.WorkspaceRegistry, llm gollem.LLMClient, slackSvc slacksvc.Service,
) agentkernel.Locator {
	t.Helper()

	locator, k := bindDraftRuntimeWithoutWorker(t, uc, repo, registry, llm, slackSvc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Serve(ctx, agentkit.WithPollInterval(5*time.Millisecond)) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return locator
}

// bindDraftRuntimeWithoutWorker wires the runtime but never claims anything, so a
// spawned run stays exactly where the host left it. Use it to assert what the HOST
// did — the Session it wrote, the draft it claimed — with a turn still live and no
// worker racing to finish it.
func bindDraftRuntimeWithoutWorker(
	t *testing.T, uc *usecase.MentionProposalUseCase, repo interfaces.Repository,
	registry *model.WorkspaceRegistry, llm gollem.LLMClient, slackSvc slacksvc.Service,
) (agentkernel.Locator, *agentkit.Kernel) {
	t.Helper()

	procRepo := agentprocmemory.New()
	history := agentarchive.NewMemoryHistoryStore()
	reg := agentkit.NewRegistry()
	models := testAgentModelPolicy(t)
	taskAgent, err := agentkernel.RegisterTaskAgent(reg, testAgentBudget.Limiter(models.Resolve), history)
	gt.NoError(t, err).Required()
	locator, err := agentkernel.NewLocator(procRepo)
	gt.NoError(t, err).Required()

	d, err := proposal.NewDurable(repo, registry, usecase.MentionProposalWorkspaceAccessForTest(uc),
		uc.DurableDraftHost(), locator, models)
	gt.NoError(t, err).Required()
	gt.NoError(t, d.Register(reg, taskAgent, nil,
		testAgentRootBudget.Limiter(models.Resolve), history)).Required()

	k, err := agentkernel.Build(agentkernel.Deps{
		Repo:    procRepo,
		History: history,
		LLM:     llm,
		Trace:   agentarchive.NewMemoryTraceRepository(),
		Budgets: agentkernel.Budgets{Root: testAgentRootBudget, Task: testAgentBudget},
		Models:  models,
		Agents:  reg,
		Tools:   agentkernel.ToolDeps{Repo: repo, Registry: registry, SlackBot: slackSvc},
	})
	gt.NoError(t, err).Required()
	d.Bind(k, nil)
	uc.BindDurableDraft(d)
	return locator, k
}

// waitForDraftMaterialization blocks until the stored draft carries a finished
// materialization for the given workspace.
//
// A workspace switch ends on the same Session outcome as the turn before it, so
// the draft is what separates the two — and it must be the FINISHED draft: the
// switch stamps the destination workspace and clears the materialization before
// it spawns, so matching on the workspace alone would return the locked draft.
func waitForDraftMaterialization(t *testing.T, repo interfaces.Repository,
	id model.CaseProposalID, want string,
) *model.CaseProposal {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for {
		d, err := repo.CaseProposal().Get(ctx, id)
		gt.NoError(t, err).Required()
		if d != nil && d.SelectedWorkspaceID == want && d.Materialization != nil && !d.InferenceInProgress {
			return d
		}
		if time.Now().After(deadline) {
			gt.Value(t, d).NotNil().Required()
			gt.Value(t, d.SelectedWorkspaceID).Equal(want).Required()
			gt.Bool(t, d.InferenceInProgress).False().Required()
			gt.Value(t, d.Materialization).NotNil().Required()
			return d
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// waitForDraftSessionEnd blocks until the thread's Session records the given
// outcome. The turn runs on the agentkit worker, so there is nothing for the
// caller to join on — the stamped Session is the first durable evidence that the
// completion handler ran.
func waitForDraftSessionEnd(t *testing.T, repo interfaces.Repository,
	channelID, threadTS string, want model.SessionEndReason,
) *model.Session {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ssn, err := repo.Session().GetByThread(ctx, channelID, threadTS)
		gt.NoError(t, err).Required()
		if ssn != nil && ssn.LastAction == want {
			return ssn
		}
		if time.Now().After(deadline) {
			gt.Value(t, ssn).NotNil().Required()
			gt.Value(t, ssn.LastAction).Equal(want).Required()
			return ssn
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func newRegistryWithSchema(workspaceID, workspaceName string, schema *config.FieldSchema) *model.WorkspaceRegistry {
	r := model.NewWorkspaceRegistry()
	r.Register(&model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: workspaceID, Name: workspaceName},
		FieldSchema: schema,
	})
	return r
}

// TestBuildDraftUserInput_ChannelContext locks the planner's first-turn
// prompt content: mention text, channel descriptor (name / topic /
// purpose / privacy), and surrounding conversation. This is the seam
// where channel-level context is injected so the planner can anchor
// workspace inference without spending a tool call on it.
func TestBuildFieldPairSections_Semantic(t *testing.T) {
	fields := []config.FieldDefinition{
		{ID: "channel", Name: "Notify", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		{ID: "legacy", Name: "Legacy", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		{ID: "note", Name: "Note", Type: types.FieldTypeText},
	}
	values := map[string]model.FieldValue{
		"channel": {FieldID: "channel", Value: "C0123ABCD"},
		"legacy":  {FieldID: "legacy", Value: "#old"},
		"note":    {FieldID: "note", Value: "C0123ABCD"},
	}

	blocks := usecase.BuildFieldPairSectionsForTest(fields, values)
	gt.Array(t, blocks).Length(1).Required()
	section, ok := blocks[0].(*goslack.SectionBlock)
	gt.Bool(t, ok).True()
	// The channel link is not wrapped in a code span, where Slack would not
	// render it; values without a usable semantic keep the code span.
	gt.Value(t, section.Text.Text).Equal("*Notify:* <#C0123ABCD>\n*Legacy:* `#old`\n*Note:* `C0123ABCD`")
}

func TestBuildDraftUserInput_ChannelContext(t *testing.T) {
	d := &model.CaseProposal{
		RawMessages: []model.ProposalMessage{
			{TS: "1700000000.000100", UserID: "U001", Text: "first line"},
			{TS: "1700000001.000100", UserID: "U002", Text: "second\nline"},
		},
	}
	ci := &slacksvc.ChannelInfo{
		ID:         "C-RISK",
		Name:       "sec-risk-ops",
		Topic:      "Daily ops for the security risk team",
		Purpose:    "Triaging incoming risk reports\nand coordinating response",
		IsPrivate:  true,
		IsShared:   false,
		IsArchived: false,
		NumMembers: 17,
		Creator:    "U999",
	}

	got := usecase.BuildProposalUserInputForTest(d, "@bot please draft a case for Tanaka's issue", ci)

	gt.S(t, got).Contains("# User mention")
	gt.S(t, got).Contains("please draft a case for Tanaka's issue")
	gt.S(t, got).Contains("# Channel context")
	gt.S(t, got).Contains("- name: #sec-risk-ops")
	gt.S(t, got).Contains("- topic: Daily ops for the security risk team")
	// Newlines inside topic / purpose are flattened to single spaces so
	// the planner sees a single-row entry.
	gt.S(t, got).Contains("- description: Triaging incoming risk reports and coordinating response")
	gt.S(t, got).Contains("- privacy: private")
	gt.S(t, got).Contains("- members: 17")
	gt.S(t, got).Contains("- creator: U999")
	gt.S(t, got).Contains("# Surrounding conversation (chronological, oldest first)")
	gt.S(t, got).Contains("first line")
	gt.S(t, got).Contains("second line") // newline collapsed
}

// TestBuildDraftUserInput_NilChannelInfoOmitsSection confirms the host
// degrades gracefully when conversations.info fails: the planner still
// gets the mention text and the surrounding conversation, just no
// channel-level hints.
func TestBuildDraftUserInput_NilChannelInfoOmitsSection(t *testing.T) {
	d := &model.CaseProposal{}
	got := usecase.BuildProposalUserInputForTest(d, "@bot something", nil)

	gt.S(t, got).Contains("# User mention")
	gt.S(t, got).Contains("@bot something")
	gt.Bool(t, strings.Contains(got, "# Channel context")).False()
}

func TestMentionDraftUseCase_HandleAppMention_HappyPath(t *testing.T) {
	repo := memory.New()
	schema := &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "severity", Type: types.FieldTypeSelect,
			Options: []config.FieldOption{{ID: "low"}, {ID: "high"}}},
	}}
	registry := newRegistryWithSchema("ws-only", "OnlyWS", schema)

	slackMock := newCollectorOnlyMockSlack()
	uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	bindDraftRuntime(t, uc, repo, registry, newScriptedClient(stubDraftScript("ws-only")), slackMock)

	ev := &slackevents.AppMentionEvent{
		Channel:   "C-USER",
		User:      "U-USER",
		Text:      "<@BOT> please open a case",
		TimeStamp: "1700000010.000000",
	}

	gt.NoError(t, uc.HandleAppMention(context.Background(), ev)).Required()
	waitForDraftSessionEnd(t, repo, "C-USER", "1700000010.000000", model.SessionEndedWithMaterialize)

	// The mention path must (a) post the "⏳ Drafting…" placeholder
	// AND the preview as TWO distinct thread replies, with the preview
	// landing chronologically AFTER any planner trace messages, and
	// (b) collapse the placeholder into a completed-breadcrumb via
	// UpdateMessage so the user is pointed at the new preview.
	previewPost := waitForPreviewPost(t, slackMock)
	gt.Value(t, previewPost.channelID).Equal("C-USER")
	// Title + description markdown, divider, actions at minimum.
	gt.Number(t, len(previewPost.rawBlocks)).GreaterOrEqual(3)

	// The processing placeholder must end up as the completed
	// breadcrumb context block (block_id == mention_draft_processing_completed).
	updates := slackMock.updates()
	gt.Number(t, len(updates)).GreaterOrEqual(1).Required()
	last := updates[len(updates)-1]
	gt.Value(t, last.channelID).Equal("C-USER")
	gt.Array(t, last.rawBlocks).Length(1).Required()
	completedCtx, ok := last.rawBlocks[0].(*goslack.ContextBlock)
	gt.Bool(t, ok).True().Required()
	gt.String(t, completedCtx.BlockID).Equal("mention_draft_processing_completed")
}

// A second mention arriving while a draft turn is live must not take the thread's
// draft away from the run that is working on it.
//
// The mention path creates a draft before it knows whether the turn will be
// accepted. If it pointed the Session at that draft immediately, a refused second
// mention would leave the thread naming a draft nothing runs for — and the FIRST
// run's completion handler, which reloads the Session, would write its result into
// that refused draft instead of its own. So the association is made only after the
// spawn is accepted, and the run carries its own draft id.
func TestMentionDraftUseCase_BusySecondMentionKeepsTheLiveRunsDraft(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	registry := newRegistryWithSchema("ws-only", "OnlyWS", schemaWithSeverity())

	slackMock := newCollectorOnlyMockSlack()
	uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	// One scripted call: the first turn parks mid-plan so the thread stays held.
	bindDraftRuntimeWithoutWorker(t, uc, repo, registry,
		newScriptedClient([]string{draftPlan}), slackMock) //nolint:dogsled

	first := &slackevents.AppMentionEvent{
		Channel: "C-BUSY", User: "U-USER",
		Text: "<@BOT> please open a case", TimeStamp: "1700000010.000000",
	}
	gt.NoError(t, uc.HandleAppMention(ctx, first)).Required()

	ssn, err := repo.Session().GetByThread(ctx, "C-BUSY", "1700000010.000000")
	gt.NoError(t, err).Required()
	gt.Value(t, ssn).NotNil().Required()
	liveDraft := ssn.ProposalID
	gt.Value(t, liveDraft).NotEqual(model.CaseProposalID(""))

	// Second mention INSIDE the first one's thread — the same Session, and so the
	// same subject the live run holds. A top-level mention would open its own
	// thread and legitimately start its own turn.
	second := &slackevents.AppMentionEvent{
		Channel: "C-BUSY", User: "U-USER",
		Text: "<@BOT> and also this", TimeStamp: "1700000020.000000",
		ThreadTimeStamp: "1700000010.000000",
	}
	gt.NoError(t, uc.HandleAppMention(ctx, second)).Required()

	// The thread still names the draft the live run is writing into.
	ssn2, err := repo.Session().GetByThread(ctx, "C-BUSY", "1700000010.000000")
	gt.NoError(t, err).Required()
	gt.Value(t, ssn2.ProposalID).Equal(liveDraft)

	// And that draft is intact and still in flight, so the live run's completion
	// handler has somewhere correct to write.
	live, err := repo.CaseProposal().Get(ctx, liveDraft)
	gt.NoError(t, err).Required()
	gt.Value(t, live).NotNil().Required()
	gt.Bool(t, live.InferenceInProgress).True()
	gt.String(t, live.MentionText).Contains("please open a case")
}

// A draft turn has no workspace until its run picks one, so the first mention's
// turn keeps the user's locale even when the only workspace configures another
// language. Once a draft names its workspace, a reply's turn follows that
// workspace's language instead.
func TestMentionDraftUseCase_TurnLanguage(t *testing.T) {
	i18n.Init(i18n.LangEN)
	userCtx := i18n.ContextWithLang(context.Background(), i18n.LangEN)

	newDraftEnv := func(t *testing.T) (*usecase.MentionProposalUseCase, *memory.Memory, agentkernel.Locator, *agentkit.Kernel) {
		t.Helper()
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		registry.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws-ja", Name: "JA"}, FieldSchema: schemaWithSeverity(), Language: i18n.LangJA,
		})
		slackMock := newCollectorOnlyMockSlack()
		uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
		locator, k := bindDraftRuntimeWithoutWorker(t, uc, repo, registry, newScriptedClient(nil), slackMock)
		return uc, repo, locator, k
	}
	turnOf := func(t *testing.T, locator agentkernel.Locator, k *agentkit.Kernel, channelID, threadTS, triggerTS string) spawnedTurn {
		t.Helper()
		pid, err := locator.ByTrigger(context.Background(), agentkernel.TriggerKey(channelID, threadTS, triggerTS))
		gt.NoError(t, err).Required()
		gt.String(t, string(pid)).NotEqual("").Required()
		proc, err := k.GetProcess(context.Background(), pid)
		gt.NoError(t, err).Required()
		var state struct {
			Input struct {
				LanguageLabel string `json:"language_label"`
			} `json:"input"`
		}
		gt.NoError(t, json.Unmarshal(proc.State, &state)).Required()
		return spawnedTurn{Scope: agentkernel.ScopeFrom(proc.Metadata), LanguageLabel: state.Input.LanguageLabel}
	}

	t.Run("first mention keeps the user's locale", func(t *testing.T) {
		uc, _, locator, k := newDraftEnv(t)
		const ts = "1700000010.000000"
		gt.NoError(t, uc.HandleAppMention(userCtx, &slackevents.AppMentionEvent{
			Channel: "C-USER", User: "U-USER", Text: "<@BOT> please open a case", TimeStamp: ts,
		})).Required()

		turn := turnOf(t, locator, k, "C-USER", ts, ts)
		gt.String(t, turn.Scope.Lang).Equal("en")
		gt.String(t, turn.LanguageLabel).Equal("English")
	})

	t.Run("reply to a draft that names its workspace follows that workspace", func(t *testing.T) {
		uc, repo, locator, k := newDraftEnv(t)
		ctx := context.Background()
		d := model.NewCaseProposal(time.Now().UTC(), "U-USER")
		d.SelectedWorkspaceID = "ws-ja"
		gt.NoError(t, repo.CaseProposal().Save(ctx, d)).Required()
		const threadTS = "1700000020.000000"
		gt.NoError(t, repo.Session().Put(ctx, &model.Session{
			ID:            "s-draft-lang",
			ChannelID:     "C-USER",
			ThreadTS:      threadTS,
			CreatorUserID: "U-USER",
			ProposalID:    d.ID,
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		})).Required()

		const replyTS = "1700000030.000000"
		gt.NoError(t, uc.HandleThreadReply(userCtx, &slackevents.MessageEvent{
			Channel: "C-USER", User: "U-USER", Text: "it is the login page", TimeStamp: replyTS, ThreadTimeStamp: threadTS,
		})).Required()

		turn := turnOf(t, locator, k, "C-USER", threadTS, replyTS)
		gt.String(t, turn.Scope.Lang).Equal("ja")
		gt.String(t, turn.LanguageLabel).Equal("Japanese")
	})
}

// containsActionBlock reports whether the block slice contains an
// ActionBlock — used by tests to identify the preview post (which is the
// only block sequence in this flow that carries Submit/Edit/Cancel
// buttons) without depending on rendered text content.
func containsActionBlock(blocks []goslack.Block) bool {
	for _, b := range blocks {
		if _, ok := b.(*goslack.ActionBlock); ok {
			return true
		}
	}
	return false
}

func TestMentionDraftUseCase_HandleAppMention_NoWorkspace_PostsError(t *testing.T) {
	repo := memory.New()
	registry := model.NewWorkspaceRegistry() // empty
	slackMock := newCollectorOnlyMockSlack()
	uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	bindDraftRuntime(t, uc, repo, registry, newScriptedClient(nil), slackMock)

	ev := &slackevents.AppMentionEvent{
		Channel:   "C1",
		User:      "U1",
		Text:      "<@BOT> hi",
		TimeStamp: "1700000010.000000",
	}
	gt.NoError(t, uc.HandleAppMention(context.Background(), ev)).Required()

	// PostThreadMessage (text only) called for the no-workspace error. No turn
	// was spawned, so the empty script is never drawn on.
	gt.Array(t, slackMock.texts()).Length(1)
	gt.String(t, slackMock.texts()[0]).Contains("No workspace")
	// The processing block was posted then immediately UpdateMessage-cleared
	// by removeProcessingMessage; both calls show in the mock.
	gt.Number(t, len(slackMock.threadPosts())).Equal(1)
	gt.Number(t, len(slackMock.updates())).GreaterOrEqual(1)
}

// newTwoWorkspaceRegistry registers "ws-denied" (which denyingAccess refuses)
// and "ws-open" with the same schema.
func newTwoWorkspaceRegistry() *model.WorkspaceRegistry {
	registry := model.NewWorkspaceRegistry()
	for _, id := range []string{"ws-denied", "ws-open"} {
		registry.Register(&model.WorkspaceEntry{
			Workspace:   model.Workspace{ID: id, Name: "Name " + id},
			FieldSchema: schemaWithSeverity(),
		})
	}
	return registry
}

// The preview's workspace selector offers only what the mentioning user may
// access, so a denied workspace never reaches the choice.
func TestMentionDraftUseCase_HandleAppMention_OffersOnlyAccessibleWorkspaces(t *testing.T) {
	repo := memory.New()
	registry := newTwoWorkspaceRegistry()
	slackMock := newCollectorOnlyMockSlack()
	uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	usecase.SetMentionProposalWorkspaceAccessForTest(uc, denyingAccess(t, registry, "ws-denied"))
	bindDraftRuntime(t, uc, repo, registry, newScriptedClient(stubDraftScript("ws-open")), slackMock)

	ev := &slackevents.AppMentionEvent{
		Channel: "C-ACCESS", User: "U-USER", Text: "<@BOT> please open a case", TimeStamp: "1700000040.000000",
	}
	gt.NoError(t, uc.HandleAppMention(context.Background(), ev)).Required()
	waitForDraftSessionEnd(t, repo, "C-ACCESS", "1700000040.000000", model.SessionEndedWithMaterialize)

	preview := waitForPreviewPost(t, slackMock)
	raw, err := json.Marshal(preview.rawBlocks)
	gt.NoError(t, err).Required()
	gt.String(t, string(raw)).Contains("ws-open")
	gt.Bool(t, strings.Contains(string(raw), "ws-denied")).False()
}

// A user no workspace allows gets the no-workspace notice and no turn.
func TestMentionDraftUseCase_HandleAppMention_AllWorkspacesDenied(t *testing.T) {
	repo := memory.New()
	registry := newTwoWorkspaceRegistry()
	slackMock := newCollectorOnlyMockSlack()
	uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	usecase.SetMentionProposalWorkspaceAccessForTest(uc, denyingAccess(t, registry, "ws-denied", "ws-open"))
	locator := bindDraftRuntime(t, uc, repo, registry, newScriptedClient(nil), slackMock)

	ev := &slackevents.AppMentionEvent{
		Channel: "C-NONE", User: "U-USER", Text: "<@BOT> hi", TimeStamp: "1700000050.000000",
	}
	gt.NoError(t, uc.HandleAppMention(context.Background(), ev)).Required()

	gt.Array(t, slackMock.texts()).Length(1).Required()
	gt.String(t, slackMock.texts()[0]).Contains("No workspace is available to you")
	pid, err := locator.ByTrigger(context.Background(),
		agentkernel.TriggerKey("C-NONE", "1700000050.000000", "1700000050.000000"))
	gt.NoError(t, err).Required()
	gt.Value(t, pid).Equal(agentkit.ProcessID(""))
}

// A reply that would resume a draft turn re-resolves the user's workspaces; when
// none is left, the thread is told and no turn is started.
func TestDispatcher_ThreadReply_NoAccessibleWorkspace(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	registry := newRegistryWithSchema("ws-1", "ws", &config.FieldSchema{})
	usecase.SetMentionProposalWorkspaceAccessForTest(f.mentionProposal, denyingAccess(t, registry, "ws-1"))

	ev := newMessageEvent("C-OPEN", "U1", "user follow-up answer", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()

	f.assertNoTurn(t, "1700000020.000000")
	gt.Array(t, f.slackMock.texts()).Length(1).Required()
	gt.String(t, f.slackMock.texts()[0]).Contains("No workspace is available to you")
}

func TestSlackUseCases_AppMention_DispatchesToMentionProposal(t *testing.T) {
	repo := memory.New()
	schema := &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "severity", Type: types.FieldTypeSelect,
			Options: []config.FieldOption{{ID: "low"}, {ID: "high"}}},
	}}
	registry := newRegistryWithSchema("ws-1", "ws", schema)

	slackMock := newCollectorOnlyMockSlack()
	mentionProposal := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	bindDraftRuntime(t, mentionProposal, repo, registry, newScriptedClient(stubDraftScript("ws-1")), slackMock)

	slackUC := usecase.NewSlackUseCases(repo, registry, nil, mentionProposal, slackMock)

	// Channel is NOT bound to any Case.
	ev := &slackevents.EventsAPIEvent{
		Type:   slackevents.CallbackEvent,
		TeamID: "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: &slackevents.AppMentionEvent{
				Channel:   "C-NEW",
				User:      "U1",
				Text:      "<@BOT>",
				TimeStamp: "1700000010.000000",
			},
		},
	}

	gt.NoError(t, slackUC.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	waitForPreviewPost(t, slackMock)
}

func TestSlackUseCases_AppMention_CaseBoundChannelDoesNotInvokeProposal(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	registry := newRegistryWithSchema("ws-1", "ws", &config.FieldSchema{})

	// Pre-create a Case whose Slack channel matches the mention channel.
	_, err := repo.Case().Create(ctx, "ws-1", &model.Case{
		ReporterID:     "U-TEST-DEFAULT",
		Title:          "existing",
		Status:         types.CaseStatusOpen,
		SlackChannelID: "C-CASE",
	})
	gt.NoError(t, err).Required()

	slackMock := newCollectorOnlyMockSlack()
	// The draft script is never drawn on: reaching it would mean the case-bound
	// channel had been routed to the draft agent, which is what this test forbids.
	llm := newScriptedClient(stubDraftScript("ws-1"))
	mentionProposal := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	bindDraftRuntime(t, mentionProposal, repo, registry, llm, slackMock)
	agent := usecase.NewAgentUseCase(usecase.AgentDeps{
		Repo:         repo,
		Registry:     registry,
		LLM:          llm,
		EmbedClient:  llm,
		HistoryRepo:  agentarchive.NewMemoryHistoryRepository(),
		TraceRepo:    agentarchive.NewMemoryTraceRepository(),
		SlackService: slackMock,
	})
	slackUC := usecase.NewSlackUseCases(repo, registry, agent, mentionProposal, slackMock)

	ev := &slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: &slackevents.AppMentionEvent{
				Channel:   "C-CASE",
				User:      "U1",
				Text:      "<@BOT>",
				TimeStamp: "1700000010.000000",
			},
		},
	}

	gt.NoError(t, slackUC.HandleSlackEvent(ctx, ev)).Required()
	async.Wait()
	// MentionProposal must NOT have been invoked. Agent path posts a single
	// session-start block; the mentionProposal preview posts 4+ blocks.
	for _, post := range slackMock.threadPosts() {
		gt.Number(t, len(post.blocks)).LessOrEqual(1)
	}
}

// --- thread-reply dispatcher (F1-F8) tests ---

// dispatcherFixture wires a SlackUseCases for thread-reply tests with a
// pre-seeded Session in the requested state.
type dispatcherFixture struct {
	uc              *usecase.SlackUseCases
	mentionProposal *usecase.MentionProposalUseCase
	repo            interfaces.Repository
	registry        *model.WorkspaceRegistry
	slackMock       *collectorOnlyMockSlack
	locator         agentkernel.Locator
	channelID       string
	threadTS        string
}

// assertNoTurn asserts the dispatcher dropped the reply: the trigger started no
// run. Counting Slack posts cannot establish this — a leaked turn spawns
// synchronously but posts only once its worker gets to it, so an immediate
// count of zero would pass either way.
func (f *dispatcherFixture) assertNoTurn(t *testing.T, replyTS string) {
	t.Helper()
	pid, err := f.locator.ByTrigger(context.Background(),
		agentkernel.TriggerKey(f.channelID, f.threadTS, replyTS))
	gt.NoError(t, err).Required()
	gt.Value(t, pid).Equal(agentkit.ProcessID(""))
}

func newDispatcherWithOpenSession(t *testing.T, channelID, threadTS string, lastAction model.SessionEndReason) *dispatcherFixture {
	t.Helper()
	repo := memory.New()
	registry := newRegistryWithSchema("ws-1", "ws", &config.FieldSchema{})
	slackMock := newCollectorOnlyMockSlack()
	mentionProposal := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	locator := bindDraftRuntime(t, mentionProposal, repo, registry,
		newScriptedClient(stubDraftScript("ws-1")), slackMock)
	slackUC := usecase.NewSlackUseCases(repo, registry, nil, mentionProposal, slackMock)

	// The seeded session names a draft, as one left behind by an earlier mention
	// turn does: a resumed turn's result is written back into that draft.
	now := time.Now().UTC()
	d := model.NewCaseProposal(now, "U-CREATOR")
	d.Source = model.ProposalSource{ChannelID: channelID, ThreadTS: threadTS, MentionTS: threadTS}
	gt.NoError(t, repo.CaseProposal().Save(context.Background(), d)).Required()
	gt.NoError(t, repo.Session().Put(context.Background(), &model.Session{
		ID:            "ssn-disp",
		ChannelID:     channelID,
		ThreadTS:      threadTS,
		CreatorUserID: "U-CREATOR",
		ProposalID:    d.ID,
		LastAction:    lastAction,
		CreatedAt:     now,
		UpdatedAt:     now,
	})).Required()

	return &dispatcherFixture{
		uc: slackUC, mentionProposal: mentionProposal, repo: repo, registry: registry, slackMock: slackMock, locator: locator,
		channelID: channelID, threadTS: threadTS,
	}
}

func newMessageEvent(channel, user, text, ts, threadTS, subtype, botID string) *slackevents.EventsAPIEvent {
	return &slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				Channel:         channel,
				User:            user,
				Text:            text,
				TimeStamp:       ts,
				ThreadTimeStamp: threadTS,
				SubType:         subtype,
				BotID:           botID,
			},
		},
	}
}

func TestDispatcher_ThreadReply_F1_DropOnSubType(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	ev := newMessageEvent("C-OPEN", "U1", "hello", "1700000020.000000", "1700000010.000000", "message_changed", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	f.assertNoTurn(t, "1700000020.000000")
}

func TestDispatcher_ThreadReply_F2_DropOnBotSelfPost(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	ev := newMessageEvent("C-OPEN", "BOT", "hi", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	f.assertNoTurn(t, "1700000020.000000")
}

func TestDispatcher_ThreadReply_F3_DropOnBotID(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	ev := newMessageEvent("C-OPEN", "U1", "hi", "1700000020.000000", "1700000010.000000", "", "B999")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	f.assertNoTurn(t, "1700000020.000000")
}

func TestDispatcher_ThreadReply_F4_DropOnTopLevel(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	// thread_ts == ts means the parent post itself; drop.
	ev := newMessageEvent("C-OPEN", "U1", "hi", "1700000020.000000", "1700000020.000000", "", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	f.assertNoTurn(t, "1700000020.000000")
}

func TestDispatcher_ThreadReply_F5_DropOnMention(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	ev := newMessageEvent("C-OPEN", "U1", "<@BOT> hi", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	f.assertNoTurn(t, "1700000020.000000")
}

func TestDispatcher_ThreadReply_F6_DropOnNoSession(t *testing.T) {
	repo := memory.New()
	registry := newRegistryWithSchema("ws-1", "ws", &config.FieldSchema{})
	slackMock := newCollectorOnlyMockSlack()
	mentionProposal := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	locator := bindDraftRuntime(t, mentionProposal, repo, registry,
		newScriptedClient(stubDraftScript("ws-1")), slackMock)
	slackUC := usecase.NewSlackUseCases(repo, registry, nil, mentionProposal, slackMock)

	ev := newMessageEvent("C-NEW", "U1", "hi", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, slackUC.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()

	pid, err := locator.ByTrigger(context.Background(),
		agentkernel.TriggerKey("C-NEW", "1700000010.000000", "1700000020.000000"))
	gt.NoError(t, err).Required()
	gt.Value(t, pid).Equal(agentkit.ProcessID(""))
}

func TestDispatcher_ThreadReply_F7_DropCaseBound(t *testing.T) {
	repo := memory.New()
	registry := newRegistryWithSchema("ws-1", "ws", &config.FieldSchema{})
	slackMock := newCollectorOnlyMockSlack()
	mentionProposal := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	locator := bindDraftRuntime(t, mentionProposal, repo, registry,
		newScriptedClient(stubDraftScript("ws-1")), slackMock)
	slackUC := usecase.NewSlackUseCases(repo, registry, nil, mentionProposal, slackMock)

	now := time.Now().UTC()
	gt.NoError(t, repo.Session().Put(context.Background(), &model.Session{
		ID:         "ssn-cb",
		ChannelID:  "C-CB",
		ThreadTS:   "1700000010.000000",
		CaseID:     42, // case-bound → F7 drop
		LastAction: model.SessionEndedWithQuestion,
		CreatedAt:  now,
		UpdatedAt:  now,
	})).Required()

	ev := newMessageEvent("C-CB", "U1", "hi", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, slackUC.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()

	pid, err := locator.ByTrigger(context.Background(),
		agentkernel.TriggerKey("C-CB", "1700000010.000000", "1700000020.000000"))
	gt.NoError(t, err).Required()
	gt.Value(t, pid).Equal(agentkit.ProcessID(""))
}

func TestDispatcher_ThreadReply_F8_DropOnNonQuestionEnd(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithMessage)
	ev := newMessageEvent("C-OPEN", "U1", "hi", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	f.assertNoTurn(t, "1700000020.000000")
}

func TestDispatcher_ThreadReply_HappyPath_ResumesTurn(t *testing.T) {
	f := newDispatcherWithOpenSession(t, "C-OPEN", "1700000010.000000", model.SessionEndedWithQuestion)
	ev := newMessageEvent("C-OPEN", "U1", "user follow-up answer", "1700000020.000000", "1700000010.000000", "", "")
	gt.NoError(t, f.uc.HandleSlackEvent(context.Background(), ev)).Required()
	async.Wait()
	// The resumed turn runs to a draft and its preview replaces the thread's
	// existing surface.
	waitForDraftSessionEnd(t, f.repo, "C-OPEN", "1700000010.000000", model.SessionEndedWithMaterialize)
	waitForPreviewPost(t, f.slackMock)
}

// --- collector-only mock slack service ---

type ephemeralBlockPost struct {
	channelID string
	userID    string
	blocks    []slackBlockSnapshot
	// rawBlocks carries the actual Block Kit blocks the production code
	// passed in. Most assertions only need the count (recorded in `blocks`)
	// but tests that need to inspect rendered text/markdown can reach into
	// rawBlocks. Filled by UpdateMessage / PostThreadMessage / etc.
	rawBlocks []goslack.Block
}

// slackBlockSnapshot is intentionally opaque; we only check counts and
// presence rather than the deep Block Kit structure.
type slackBlockSnapshot struct{}

// collectorOnlyMockSlack records every Slack call the draft flow makes.
//
// mu guards the recorded slices: the draft turn posts from the agentkit worker
// goroutine, not from the one that started it, so a test reading while the
// worker runs must go through the accessor methods.
type collectorOnlyMockSlack struct {
	mu                 sync.Mutex
	thread             []slacksvc.ConversationMessage
	history            []slacksvc.ConversationMessage
	ephemeralText      string
	ephemeralChannelID string
	ephemeralUserID    string
	ephemeralPosts     []agentEphemeralMessage
	// ephemeralErr, when set, is returned by every PostEphemeral after the
	// attempt is recorded.
	ephemeralErr        error
	ephemeralBlockPosts []ephemeralBlockPost
	threadTexts         []string
	threadReplies       []string // texts posted via PostThreadReply
	threadBlockPosts    []ephemeralBlockPost
	updateBlockPosts    []ephemeralBlockPost
	openViewCalls       []openViewCall
}

func (m *collectorOnlyMockSlack) threadPosts() []ephemeralBlockPost {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ephemeralBlockPost(nil), m.threadBlockPosts...)
}

func (m *collectorOnlyMockSlack) updates() []ephemeralBlockPost {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ephemeralBlockPost(nil), m.updateBlockPosts...)
}

func (m *collectorOnlyMockSlack) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.threadTexts...)
}

// waitForPreviewPost blocks until a message carrying the preview's Submit /
// Edit / Cancel button row has been recorded, on either surface: a fresh mention
// posts it, a workspace switch updates the existing one in place.
func waitForPreviewPost(t *testing.T, m *collectorOnlyMockSlack) ephemeralBlockPost {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, p := range append(m.threadPosts(), m.updates()...) {
			if containsActionBlock(p.rawBlocks) {
				return p
			}
		}
		if time.Now().After(deadline) {
			gt.Value(t, "no preview post").Equal("a preview post").Required()
			return ephemeralBlockPost{}
		}
		time.Sleep(3 * time.Millisecond)
	}
}

type openViewCall struct {
	triggerID string
	view      goslack.ModalViewRequest
}

func newCollectorOnlyMockSlack() *collectorOnlyMockSlack {
	return &collectorOnlyMockSlack{}
}

// --- collector-required impls ---

func (m *collectorOnlyMockSlack) GetConversationReplies(_ context.Context, _ string, _ string, _ int) ([]slacksvc.ConversationMessage, error) {
	return m.thread, nil
}
func (m *collectorOnlyMockSlack) GetConversationHistory(_ context.Context, _ string, _ time.Time, _ int) ([]slacksvc.ConversationMessage, error) {
	return m.history, nil
}
func (m *collectorOnlyMockSlack) GetPermalink(_ context.Context, channelID, ts string) (string, error) {
	return "https://slack/" + channelID + "/" + ts, nil
}
func (m *collectorOnlyMockSlack) PostEphemeral(_ context.Context, channelID string, userID string, text string, opts ...slacksvc.PostEphemeralOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ephemeralText = text
	m.ephemeralChannelID = channelID
	m.ephemeralUserID = userID
	cfg := slacksvc.ApplyPostEphemeralOptions(opts...)
	m.ephemeralPosts = append(m.ephemeralPosts, agentEphemeralMessage{ChannelID: channelID, ThreadTS: cfg.ThreadTS, UserID: userID, Text: text})
	return m.ephemeralErr
}

// ephemerals returns every plain ephemeral message, in posting order.
func (m *collectorOnlyMockSlack) ephemerals() []agentEphemeralMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agentEphemeralMessage(nil), m.ephemeralPosts...)
}

// ephemeral returns the last plain ephemeral message: channel, user, text.
func (m *collectorOnlyMockSlack) ephemeral() (string, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ephemeralChannelID, m.ephemeralUserID, m.ephemeralText
}
func (m *collectorOnlyMockSlack) PostEphemeralBlocks(_ context.Context, channelID string, userID string, blocks []goslack.Block, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snaps := make([]slackBlockSnapshot, len(blocks))
	m.ephemeralBlockPosts = append(m.ephemeralBlockPosts, ephemeralBlockPost{
		channelID: channelID,
		userID:    userID,
		blocks:    snaps,
	})
	return "ts-eph", nil
}

// --- unused interface stubs ---

func (m *collectorOnlyMockSlack) ListJoinedChannels(context.Context, string) ([]slacksvc.Channel, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) ListMemberChannels(context.Context, string, string) ([]slacksvc.Channel, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) JoinChannel(context.Context, string) error {
	return nil
}
func (m *collectorOnlyMockSlack) GetChannelNames(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) GetUserInfo(context.Context, string) (*slacksvc.User, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) ListUsers(context.Context, string) ([]*slacksvc.User, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) CreateChannel(context.Context, int64, string, string, bool, string) (string, error) {
	// Return a deterministic synthetic channel ID so post-create assertions
	// have something to recognise as a Slack channel mention. Tests that do
	// not care about the value still see a non-empty string.
	return "C-CREATED", nil
}
func (m *collectorOnlyMockSlack) GetConversationMembers(context.Context, string) ([]string, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) GetChannelInfo(_ context.Context, channelID string) (*slacksvc.ChannelInfo, error) {
	return &slacksvc.ChannelInfo{
		ID:      channelID,
		Name:    "draft-test",
		Topic:   "drafting test cases",
		Purpose: "fixture channel for the draft mention flow",
	}, nil
}
func (m *collectorOnlyMockSlack) RenameChannel(context.Context, string, int64, string, string) error {
	return nil
}
func (m *collectorOnlyMockSlack) InviteUsersToChannel(context.Context, string, []string) error {
	return nil
}
func (m *collectorOnlyMockSlack) AddBookmark(context.Context, string, string, string) error {
	return nil
}
func (m *collectorOnlyMockSlack) GetTeamURL(context.Context) (string, error) { return "", nil }
func (m *collectorOnlyMockSlack) PostMessage(context.Context, string, []goslack.Block, string) (string, error) {
	return "", nil
}
func (m *collectorOnlyMockSlack) PostMessageWithAttachment(context.Context, string, string, goslack.Attachment) (string, error) {
	return "", nil
}
func (m *collectorOnlyMockSlack) PostMessageWithAttachments(context.Context, string, string, []goslack.Attachment) (string, error) {
	return "", nil
}
func (m *collectorOnlyMockSlack) UpdateMessageWithAttachments(context.Context, string, string, string, []goslack.Attachment) error {
	return nil
}
func (m *collectorOnlyMockSlack) UpdateMessageWithAttachment(context.Context, string, string, string, goslack.Attachment) error {
	return nil
}
func (m *collectorOnlyMockSlack) UpdateMessage(_ context.Context, channelID string, _ string, blocks []goslack.Block, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	snaps := make([]slackBlockSnapshot, len(blocks))
	m.updateBlockPosts = append(m.updateBlockPosts, ephemeralBlockPost{
		channelID: channelID,
		blocks:    snaps,
		rawBlocks: append([]goslack.Block(nil), blocks...),
	})
	return nil
}
func (m *collectorOnlyMockSlack) PostThreadReply(_ context.Context, _ string, _ string, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.threadReplies = append(m.threadReplies, text)
	return "ts-reply", nil
}
func (m *collectorOnlyMockSlack) PostThreadMessage(_ context.Context, channelID string, _ string, blocks []goslack.Block, text string, _ ...slacksvc.PostThreadOption) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(blocks) > 0 {
		snaps := make([]slackBlockSnapshot, len(blocks))
		m.threadBlockPosts = append(m.threadBlockPosts, ephemeralBlockPost{
			channelID: channelID,
			blocks:    snaps,
			rawBlocks: append([]goslack.Block(nil), blocks...),
		})
	} else {
		m.threadTexts = append(m.threadTexts, text)
	}
	return "ts-thread", nil
}
func (m *collectorOnlyMockSlack) GetBotUserID(context.Context) (string, error) { return "BOT", nil }
func (m *collectorOnlyMockSlack) OpenView(_ context.Context, triggerID string, view goslack.ModalViewRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openViewCalls = append(m.openViewCalls, openViewCall{triggerID: triggerID, view: view})
	return nil
}
func (m *collectorOnlyMockSlack) UpdateView(_ context.Context, _ goslack.ModalViewRequest, _, _, _ string) error {
	return nil
}
func (m *collectorOnlyMockSlack) ListUserGroups(context.Context, string) ([]slacksvc.UserGroup, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) GetUserGroupMembers(context.Context, string) ([]string, error) {
	return nil, nil
}
func (m *collectorOnlyMockSlack) ListTeams(context.Context) ([]slacksvc.Team, error) {
	return nil, nil
}

// --- Lifecycle (multi-turn integration) tests ---
//
// These tests drive the *whole* dispatcher path (HandleSlackEvent /
// HandleSelectWorkspace / HandleSubmit) across multiple turns to catch
// state-machine bugs that per-method tests cannot. They share two pieces
// of infrastructure:
//
//  1. lifecycleHarness — assembles MentionProposalUseCase + SlackUseCases and
//     the agentkit runtime that takes their turns, all against a single memory
//     repo and one scripted LLM client.
//  2. mention/messageEvent helpers — produce real Slack event shapes so the
//     dispatcher walks the same code path as production.
//
// A script runs a turn's calls in order: plan → each sub-agent → replan →
// terminal output. Overrunning it fails the turn, which is how these tests
// catch a runtime that loops more than it should.

// lifecycleHarness wires the host-side usecases against a shared memory repo
// and the supplied scripted LLM. Returns the SlackUseCases (the dispatcher)
// and the MentionProposalUseCase (so tests can drive interaction handlers).
type lifecycleHarness struct {
	repo            interfaces.Repository
	registry        *model.WorkspaceRegistry
	slackMock       *collectorOnlyMockSlack
	mentionProposal *usecase.MentionProposalUseCase
	slackUC         *usecase.SlackUseCases
	caseUC          *usecase.CaseUseCase
}

func newLifecycleHarness(t *testing.T, registry *model.WorkspaceRegistry, llm gollem.LLMClient) *lifecycleHarness {
	t.Helper()
	repo := memory.New()
	slackMock := newCollectorOnlyMockSlack()
	mentionProposal := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	bindDraftRuntime(t, mentionProposal, repo, registry, llm, slackMock)
	caseUC := usecase.NewCaseUseCase(repo, registry, slackMock, nil, "")
	slackUC := usecase.NewSlackUseCases(repo, registry, nil, mentionProposal, slackMock)
	return &lifecycleHarness{
		repo:            repo,
		registry:        registry,
		slackMock:       slackMock,
		mentionProposal: mentionProposal,
		slackUC:         slackUC,
		caseUC:          caseUC,
	}
}

func appMentionEvent(channel, user, text, ts string) *slackevents.EventsAPIEvent {
	return &slackevents.EventsAPIEvent{
		Type:   slackevents.CallbackEvent,
		TeamID: "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: &slackevents.AppMentionEvent{
				Channel:   channel,
				User:      user,
				Text:      text,
				TimeStamp: ts,
			},
		},
	}
}

// schemaWithSeverity builds a single-field FieldSchema with a `severity`
// select for use across lifecycle tests. Two option IDs (low/high) give
// schema-validation assertions something to match against.
func schemaWithSeverity() *config.FieldSchema {
	return &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "severity", Type: types.FieldTypeSelect, Required: true,
			Options: []config.FieldOption{{ID: "low", Name: "Low"}, {ID: "high", Name: "High"}}},
	}}
}

// --- Scenario A: mention → investigate → post_question → thread reply → materialize ---
func TestLifecycle_DraftFlow_InvestigateQuestionResumeMaterialize(t *testing.T) {
	const channelID = "C-LIFE-A"
	const mentionTS = "1700000010.000000"
	const replyTS = "1700000020.000000"
	registry := newRegistryWithSchema("ws-1", "WS-1", schemaWithSeverity())

	llm := newScriptedClient([]string{
		// Turn 1, round 1 (mention): investigate one task.
		`{"message":"Looking at the thread","tasks":[{"id":"inv-1","title":"thread scan","description":"scan thread","acceptance_criteria":"got summary","tools":["slack_ro"],"budget_usd":0.01}]}`,
		"summary: all messages mention an outage but never name a severity.",
		// Turn 1, round 2 (after the observation): ask the user.
		`{"message":"still missing severity","question":{"reason":"need severity to fill the schema","items":[{"id":"q-sev","text":"What is the severity?","type":"select","options":["low","high"]}]}}`,
		// Turn 2, round 1 (after the thread reply): no more investigation needed.
		`{"message":"user answered","tasks":[{"id":"inv-2","title":"confirm","description":"confirm the answer","acceptance_criteria":"confirmed","tools":["slack_ro"],"budget_usd":0.01}]}`,
		"summary: the user says the severity is high.",
		`{"message":"ready","finalize":{"reason":"severity known"}}`,
		draftFinal("ws-1", "Outage X", "Service degraded since morning.", `[{"field_id":"severity","value":"high"}]`),
	})

	h := newLifecycleHarness(t, registry, llm)

	// --- Turn 1: app_mention drives investigate → post_question.
	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U1", "<@BOT> case please", mentionTS))).Required()
	async.Wait()

	// Session is persisted with LastAction = post_question so the dispatcher
	// will treat the next thread reply as a resume signal. The pending
	// question snapshot is the canonical record of what was asked — assert
	// against it rather than parsing rendered Slack blocks.
	ssn1 := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithQuestion)
	gt.Value(t, ssn1.PendingQuestion).NotNil().Required()
	gt.Array(t, ssn1.PendingQuestion.Items).Length(1).Required()
	gt.Value(t, ssn1.PendingQuestion.Items[0].ID).Equal("q-sev")
	gt.String(t, ssn1.PendingQuestion.Items[0].Text).Contains("severity")
	gt.Array(t, ssn1.PendingQuestion.Items[0].Options).Length(2)
	gt.Value(t, ssn1.PendingQuestion.Items[0].Options[0]).Equal("low")
	gt.Value(t, ssn1.PendingQuestion.Items[0].Options[1]).Equal("high")

	// --- Turn 2: user replies in-thread without mentioning the bot.
	reply := &slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				Channel: channelID, User: "U1",
				Text:            "high",
				TimeStamp:       replyTS,
				ThreadTimeStamp: mentionTS,
			},
		},
	}
	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(), reply)).Required()
	async.Wait()

	// Materialization persisted on the draft.
	ssn2 := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)
	gt.Value(t, ssn2.ProposalID).NotEqual(model.CaseProposalID(""))

	d, err := h.repo.CaseProposal().Get(context.Background(), ssn2.ProposalID)
	gt.NoError(t, err).Required()
	gt.Value(t, d).NotNil().Required()
	gt.Value(t, d.Materialization).NotNil().Required()
	gt.Value(t, d.Materialization.Title).Equal("Outage X")
	gt.Value(t, d.Materialization.CustomFieldValues["severity"].Value).Equal("high")
}

// --- Scenario F: mention → question → Submit-button drives the resume ---
func TestLifecycle_DraftFlow_QuestionFormSubmitResumesPlanner(t *testing.T) {
	const channelID = "C-LIFE-F"
	const mentionTS = "1700000010.000000"
	const formTS = "ts-thread"
	const submitTS = "1700000020.000000"
	registry := newRegistryWithSchema("ws-1", "WS-1", schemaWithSeverity())

	llm := newScriptedClient([]string{
		// Turn 1 (mention): investigate, then ask the user.
		draftPlan,
		"summary: the thread does not name a severity.",
		`{"message":"need severity","question":{"reason":"need severity","items":[{"id":"q-sev","text":"What is the severity?","type":"select","options":["low","high"]}]}}`,
		// Turn 2 (after Submit): the answer is enough to draft from.
		draftPlan,
		"summary: the user says the severity is high.",
		draftReplanDone,
		draftFinal("ws-1", "Outage F", "Service degraded.", `[{"field_id":"severity","value":"high"}]`),
	})

	h := newLifecycleHarness(t, registry, llm)

	// --- Turn 1: mention → planner emits question.
	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U1", "<@BOT> case please", mentionTS))).Required()
	async.Wait()

	ssn1 := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithQuestion)
	gt.Value(t, ssn1.PendingQuestion).NotNil().Required()
	gt.Value(t, ssn1.PendingQuestion.PostedMessageTS).Equal(formTS)

	// --- Turn 2: user clicks Submit on the form.
	cb := &goslack.InteractionCallback{
		Type:    goslack.InteractionTypeBlockActions,
		User:    goslack.User{ID: "U1"},
		Channel: goslack.Channel{GroupConversation: goslack.GroupConversation{Conversation: goslack.Conversation{ID: channelID}}},
		Message: goslack.Message{Msg: goslack.Msg{Timestamp: formTS, ThreadTimestamp: mentionTS}},
		BlockActionState: &goslack.BlockActionStates{
			Values: map[string]map[string]goslack.BlockAction{
				usecase.BlockIDDraftQuestionItemPrefix + "q-sev": {
					usecase.ActionIDDraftQuestionChoice: {
						SelectedOption: goslack.OptionBlockObject{Value: "high"},
					},
				},
			},
		},
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{ActionID: usecase.ActionIDDraftQuestionSubmit, Value: string(ssn1.ProposalID)},
			},
		},
	}
	_ = submitTS // reserved for future per-submission ts attribution
	gt.NoError(t, h.mentionProposal.HandleQuestionSubmit(context.Background(), cb,
		cb.ActionCallback.BlockActions[0])).Required()
	async.Wait()

	// PendingQuestion is cleared and the planner advanced to materialize.
	ssn2 := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)
	gt.Value(t, ssn2.PendingQuestion).Nil()

	// Form was rewritten into the answered view (one UpdateMessage just for
	// the form swap; further updates may follow from the materialize path).
	gt.Number(t, len(h.slackMock.updates())).GreaterOrEqual(1)
	// A complete submit tells nobody about unanswered questions.
	gt.Array(t, h.slackMock.ephemerals()).Length(0)

	// Materialization landed with the user's answer baked into custom fields.
	d, err := h.repo.CaseProposal().Get(context.Background(), ssn2.ProposalID)
	gt.NoError(t, err).Required()
	gt.Value(t, d).NotNil().Required()
	gt.Value(t, d.Materialization).NotNil().Required()
	gt.Value(t, d.Materialization.Title).Equal("Outage F")
	gt.Value(t, d.Materialization.CustomFieldValues["severity"].Value).Equal("high")
}

// --- Scenario B: mention → materialize → ws-switch → re-materialize ---
func TestLifecycle_DraftFlow_MaterializeThenWorkspaceSwitch(t *testing.T) {
	const channelID = "C-LIFE-B"
	const mentionTS = "1700000010.000000"
	registry := model.NewWorkspaceRegistry()
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-A", Name: "WS-A"}, FieldSchema: schemaWithSeverity(),
	})
	registry.Register(&model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-B", Name: "WS-B"},
		FieldSchema: &config.FieldSchema{Fields: []config.FieldDefinition{
			{ID: "team", Type: types.FieldTypeText, Required: true},
		}},
	})

	llm := newScriptedClient([]string{
		// Turn 1 (mention) → draft for ws-A.
		draftPlan,
		"summary: an issue was reported.",
		draftReplanDone,
		draftFinal("ws-A", "Issue title", "Initial description", `[{"field_id":"severity","value":"low"}]`),
		// Turn 2 (ws-switch) → redraft for the ws-B schema.
		draftPlan,
		"summary: the same issue, seen through the ws-B schema.",
		draftReplanDone,
		draftFinal("ws-B", "Issue title", "Initial description", `[{"field_id":"team","value":"platform"}]`),
	})

	h := newLifecycleHarness(t, registry, llm)

	// Turn 1: mention.
	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U1", "<@BOT> please", mentionTS))).Required()
	async.Wait()

	ssn := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)
	d1, err := h.repo.CaseProposal().Get(context.Background(), ssn.ProposalID)
	gt.NoError(t, err).Required()
	gt.Value(t, d1.SelectedWorkspaceID).Equal("ws-A")
	gt.Value(t, d1.Materialization.CustomFieldValues["severity"].Value).Equal("low")

	// Turn 2: workspace switch via the preview's static_select.
	cb := &goslack.InteractionCallback{
		Type:        goslack.InteractionTypeBlockActions,
		ResponseURL: "http://example.invalid/responseurl",
		User:        goslack.User{ID: "U1"},
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{
					ActionID:       usecase.ActionIDDraftSelectWS,
					BlockID:        usecase.BlockIDDraftWSSelect + ":" + string(d1.ID),
					Value:          string(d1.ID),
					SelectedOption: goslack.OptionBlockObject{Value: "ws-B"},
				},
			},
		},
	}
	// The handler POSTs to ResponseURL — we use a local httptest server so
	// the lock-blocks render call succeeds (real HTTP would surface as a
	// non-fatal errutil.Handle but doesn't block the planner turn).
	respURL, _ := captureResponseURL(t)
	cb.ResponseURL = respURL

	// Count preview-shaped thread posts before turn 2 — the
	// invariant is that this number does NOT increase across the
	// workspace switch (a new preview at the bottom would break the
	// "same-position morph" UX). Plain trace / planning context
	// blocks are allowed to come and go.
	preSwitchPreviewThreadPosts := countPreviewThreadPosts(h.slackMock.threadPosts())

	wsErr := h.mentionProposal.HandleSelectWorkspace(context.Background(), cb, cb.ActionCallback.BlockActions[0])
	async.Wait()
	gt.NoError(t, wsErr).Required()

	// The switch spawns a fresh turn, so the redraft lands from the worker; the
	// stamped workspace is the first durable evidence that it did.
	d2 := waitForDraftMaterialization(t, h.repo, ssn.ProposalID, "ws-B")
	gt.Value(t, d2.Materialization).NotNil().Required()
	gt.Value(t, d2.Materialization.CustomFieldValues["team"].Value).Equal("platform")
	// The old severity field is no longer schema-relevant; the coercion
	// drops fields outside the active schema.
	_, hasSeverity := d2.Materialization.CustomFieldValues["severity"]
	gt.Bool(t, hasSeverity).False()

	// Workspace switch MUST rewrite the existing preview in place — the
	// flow does not post a fresh thread reply at the bottom (which
	// would leave two preview rows in the thread and obscure the
	// "same-position morph" UX). The post-switch preview must reach
	// Slack via UpdateMessage, not PostThreadMessage. (Plain trace /
	// planning context blocks may legitimately be posted by the
	// planner round and are not counted here.)
	gt.Number(t, countPreviewThreadPosts(h.slackMock.threadPosts())).Equal(preSwitchPreviewThreadPosts)
	sawWSSwitchPreview := false
	for _, post := range h.slackMock.updates() {
		if containsActionBlock(post.rawBlocks) {
			sawWSSwitchPreview = true
		}
	}
	gt.Bool(t, sawWSSwitchPreview).True()
}

// countPreviewThreadPosts counts the thread posts whose blocks include
// an ActionBlock (the Submit/Edit/Cancel button row). Plain trace lines
// and the processing placeholder are excluded — only the preview itself
// matches this shape.
func countPreviewThreadPosts(posts []ephemeralBlockPost) int {
	n := 0
	for _, p := range posts {
		if containsActionBlock(p.rawBlocks) {
			n++
		}
	}
	return n
}

// --- Scenario C: mention → 2 parallel investigations → materialize ---
func TestLifecycle_DraftFlow_ParallelInvestigationsThenMaterialize(t *testing.T) {
	const channelID = "C-LIFE-C"
	const mentionTS = "1700000030.000000"
	registry := newRegistryWithSchema("ws-1", "WS-1", schemaWithSeverity())

	// The two sub-agent answers are interchangeable on purpose: the tasks run
	// concurrently, so which one draws which entry is not fixed.
	llm := newScriptedClient([]string{
		`{"message":"Looking up two angles","tasks":[{"id":"inv-A","title":"thread","description":"scan thread A","acceptance_criteria":"a","tools":["slack_ro"],"budget_usd":0.01},{"id":"inv-B","title":"channel","description":"scan channel B","acceptance_criteria":"b","tools":["slack_ro"],"budget_usd":0.01}]}`,
		"summary: high signal",
		"summary: confirms",
		draftReplanDone,
		draftFinal("ws-1", "Combined finding", "From thread + channel.", `[{"field_id":"severity","value":"high"}]`),
	})

	h := newLifecycleHarness(t, registry, llm)

	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U1", "<@BOT> please", mentionTS))).Required()
	async.Wait()

	ssn := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)

	d, err := h.repo.CaseProposal().Get(context.Background(), ssn.ProposalID)
	gt.NoError(t, err).Required()
	gt.Value(t, d.Materialization.Title).Equal("Combined finding")
	gt.Value(t, d.Materialization.CustomFieldValues["severity"].Value).Equal("high")
}

// --- Scenario D: materialize terminal → thread reply must NOT resume (F8) ---
//
// Once the planner has produced a draft preview the conversation is over
// from the agent's perspective; further thread chatter without an explicit
// @mention should be ignored. Dispatcher F8 enforces this by checking
// session.ResumeOnReply() (true only when LastAction == post_question).
func TestLifecycle_DraftFlow_MaterializeEndsThenReplyIsDropped(t *testing.T) {
	const channelID = "C-LIFE-D"
	const mentionTS = "1700000040.000000"
	const replyTS = "1700000050.000000"
	registry := newRegistryWithSchema("ws-1", "WS-1", schemaWithSeverity())

	// Exactly ONE turn is scripted. The dispatcher must drop the follow-up
	// MessageEvent (F8: LastAction != post_question) so no second turn starts;
	// a leaked dispatch would overrun the script and fail its turn.
	llm := newScriptedClient(stubDraftScriptTitled("ws-1", "Case D", "Done.", `[{"field_id":"severity","value":"low"}]`))

	h := newLifecycleHarness(t, registry, llm)

	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U1", "<@BOT> hey", mentionTS))).Required()
	async.Wait()

	waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)

	// Thread reply: F8 must drop. No additional LLM calls.
	reply := &slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				Channel: channelID, User: "U1",
				Text:            "are you sure?",
				TimeStamp:       replyTS,
				ThreadTimeStamp: mentionTS,
			},
		},
	}
	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(), reply)).Required()
	async.Wait()

	// Session unchanged; LastAction still materialize.
	ssn2, err := h.repo.Session().GetByThread(context.Background(), channelID, mentionTS)
	gt.NoError(t, err).Required()
	gt.Value(t, ssn2.LastAction).Equal(model.SessionEndedWithMaterialize)
}

// The agent writes every field value as a string or a list of strings; the
// preview stores each in its field's own type. A value that cannot be coerced,
// or a field the workspace does not define, is left out while the rest of the
// draft is still stored.
func TestLifecycle_DraftFlow_FieldValuesAreStoredInTheirFieldTypes(t *testing.T) {
	const channelID = "C-LIFE-G"
	const mentionTS = "1700000070.000000"
	registry := newRegistryWithSchema("ws-1", "WS-1", &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "severity", Type: types.FieldTypeSelect,
			Options: []config.FieldOption{{ID: "low", Name: "Low"}, {ID: "high", Name: "High"}}},
		{ID: "score", Type: types.FieldTypeNumber},
		{ID: "impact", Type: types.FieldTypeNumber},
		{ID: "tags", Type: types.FieldTypeMultiSelect,
			Options: []config.FieldOption{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}},
	}})

	// The first terminal output holds a non-finite number and a field the schema
	// does not define; it is fed back, and the corrected one is what gets stored.
	script := stubDraftScriptTitled("ws-1", "Case G", "Typed fields.",
		`[{"field_id":"severity","value":"high"},`+
			`{"field_id":"score","value":"3"},`+
			`{"field_id":"impact","value":"NaN"},`+
			`{"field_id":"tags","values":["a","b"]},`+
			`{"field_id":"ghost","value":"x"}]`)
	script = append(script, draftFinal("ws-1", "Case G", "Typed fields.",
		`[{"field_id":"severity","value":"high"},`+
			`{"field_id":"score","value":"3"},`+
			`{"field_id":"tags","values":["a","b"]}]`))
	llm := newScriptedClient(script)

	h := newLifecycleHarness(t, registry, llm)
	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U1", "<@BOT> case please", mentionTS))).Required()
	async.Wait()

	ssn := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)
	d, err := h.repo.CaseProposal().Get(context.Background(), ssn.ProposalID)
	gt.NoError(t, err).Required()
	gt.Value(t, d.Materialization).NotNil().Required()
	gt.Value(t, d.Materialization.Title).Equal("Case G")

	gt.Value(t, d.Materialization.CustomFieldValues).Equal(map[string]model.FieldValue{
		"severity": {FieldID: "severity", Type: types.FieldTypeSelect, Value: "high"},
		"score":    {FieldID: "score", Type: types.FieldTypeNumber, Value: float64(3)},
		"tags":     {FieldID: "tags", Type: types.FieldTypeMultiSelect, Value: []string{"a", "b"}},
	})
}

// --- Scenario E: mention → materialize → HandleSubmit creates the Case ---
func TestLifecycle_DraftFlow_MaterializeThenSubmitCreatesCase(t *testing.T) {
	const channelID = "C-LIFE-E"
	const mentionTS = "1700000060.000000"
	registry := newRegistryWithSchema("ws-1", "WS-1", schemaWithSeverity())

	llm := newScriptedClient(stubDraftScriptTitled(
		"ws-1", "Quick incident", "Something broke briefly.", `[{"field_id":"severity","value":"high"}]`))

	h := newLifecycleHarness(t, registry, llm)
	seedSlackUsers(t, h.repo, "U-AUTHOR")

	gt.NoError(t, h.slackUC.HandleSlackEvent(context.Background(),
		appMentionEvent(channelID, "U-AUTHOR", "<@BOT> case", mentionTS))).Required()
	async.Wait()

	ssn := waitForDraftSessionEnd(t, h.repo, channelID, mentionTS, model.SessionEndedWithMaterialize)
	d, err := h.repo.CaseProposal().Get(context.Background(), ssn.ProposalID)
	gt.NoError(t, err).Required()

	// Submit via the preview's button — drives CreateCase end-to-end.
	respURL, _ := captureResponseURL(t)
	cb := &goslack.InteractionCallback{
		Type:        goslack.InteractionTypeBlockActions,
		ResponseURL: respURL,
		User:        goslack.User{ID: "U-AUTHOR"},
		Team:        goslack.Team{ID: "T1"},
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{ActionID: usecase.ActionIDDraftSubmit, Value: string(d.ID)},
			},
		},
	}
	gt.NoError(t, h.mentionProposal.HandleSubmit(context.Background(), h.caseUC, cb, cb.ActionCallback.BlockActions[0])).Required()
	async.Wait()

	// One case persisted with the materialized title and field value.
	cases, err := h.repo.Case().List(context.Background(), "ws-1")
	gt.NoError(t, err).Required()
	gt.Array(t, cases).Length(1).Required()
	gt.Value(t, cases[0].Title).Equal("Quick incident")
	gt.Value(t, cases[0].FieldValues["severity"].Value).Equal("high")
	gt.Array(t, cases[0].AssigneeIDs).Length(1).Required()
	gt.Value(t, cases[0].AssigneeIDs[0]).Equal("U-AUTHOR")
	gt.Value(t, cases[0].SlackChannelID).Equal("C-CREATED")

	// The post-create chat.update replaces the preview with a single
	// context block carrying a clickable mention of the case channel —
	// not a full re-render of the case body.
	updates := h.slackMock.updates()
	gt.Number(t, len(updates)).GreaterOrEqual(1).Required()
	finalUpdate := updates[len(updates)-1]
	gt.Array(t, finalUpdate.rawBlocks).Length(1).Required()
	finalCtx, ok := finalUpdate.rawBlocks[0].(*goslack.ContextBlock)
	gt.Bool(t, ok).True().Required()
	gt.Array(t, finalCtx.ContextElements.Elements).Length(1).Required()
	finalText, ok := finalCtx.ContextElements.Elements[0].(*goslack.TextBlockObject)
	gt.Bool(t, ok).True().Required()
	gt.Bool(t, strings.Contains(finalText.Text, "<#C-CREATED>")).True()
	gt.Bool(t, strings.Contains(finalText.Text, "Quick incident")).True()

	// Draft is deleted after Submit.
	_, err = h.repo.CaseProposal().Get(context.Background(), d.ID)
	gt.Value(t, err).NotNil()
}

// TestBuildCaseCreatedTailBlocks verifies that the post-create thread
// message renders as a single context block carrying a Slack channel
// mention that links the user to the case's dedicated channel.
func TestBuildCaseCreatedTailBlocks(t *testing.T) {
	t.Run("with SlackChannelID renders mrkdwn channel mention", func(t *testing.T) {
		created := &model.Case{
			ID:             42,
			Title:          "Tanaka incident",
			SlackChannelID: "C0123ABCD",
		}
		blocks, fallback := usecase.BuildCaseCreatedTailBlocksForTest(context.Background(), created)
		gt.Array(t, blocks).Length(1).Required()
		gt.String(t, fallback).Contains("42")
		gt.String(t, fallback).Contains("Tanaka incident")

		ctxBlock, ok := blocks[0].(*goslack.ContextBlock)
		gt.Bool(t, ok).True().Required()
		gt.Value(t, ctxBlock.Type).Equal(goslack.MBTContext)
		gt.Array(t, ctxBlock.ContextElements.Elements).Length(1).Required()

		text, ok := ctxBlock.ContextElements.Elements[0].(*goslack.TextBlockObject)
		gt.Bool(t, ok).True().Required()
		gt.Value(t, text.Type).Equal(goslack.MarkdownType)
		gt.String(t, text.Text).Contains("<#C0123ABCD>")
		gt.String(t, text.Text).Contains("42")
		gt.String(t, text.Text).Contains("Tanaka incident")
	})

	t.Run("without SlackChannelID falls back to plain created line", func(t *testing.T) {
		created := &model.Case{
			ID:    7,
			Title: "Solo incident",
		}
		blocks, _ := usecase.BuildCaseCreatedTailBlocksForTest(context.Background(), created)
		gt.Array(t, blocks).Length(1).Required()

		ctxBlock, ok := blocks[0].(*goslack.ContextBlock)
		gt.Bool(t, ok).True().Required()
		text, ok := ctxBlock.ContextElements.Elements[0].(*goslack.TextBlockObject)
		gt.Bool(t, ok).True().Required()
		gt.String(t, text.Text).Contains("7")
		gt.String(t, text.Text).Contains("Solo incident")
		// No channel mention when SlackChannelID is empty.
		gt.Bool(t, strings.Contains(text.Text, "<#")).False()
	})

	t.Run("nil case returns no blocks and a fallback", func(t *testing.T) {
		blocks, fallback := usecase.BuildCaseCreatedTailBlocksForTest(context.Background(), nil)
		gt.Array(t, blocks).Length(0)
		gt.String(t, fallback).NotEqual("")
	})

	t.Run("escapes markdown characters in title", func(t *testing.T) {
		// Title contains characters (`*`, `_`, `~`, backtick) that would
		// otherwise break the surrounding `*%s*` markdown slot.
		created := &model.Case{
			ID:             3,
			Title:          "*bold* _italic_ ~strike~ `code`",
			SlackChannelID: "C-X",
		}
		blocks, _ := usecase.BuildCaseCreatedTailBlocksForTest(context.Background(), created)
		gt.Array(t, blocks).Length(1).Required()
		ctxBlock, ok := blocks[0].(*goslack.ContextBlock)
		gt.Bool(t, ok).True().Required()
		text, ok := ctxBlock.ContextElements.Elements[0].(*goslack.TextBlockObject)
		gt.Bool(t, ok).True().Required()
		// Original markdown control chars are no longer present unescaped.
		// (escapeMarkdownInline prefixes them with `\` or strips them; the
		// exact escape form is its concern, but the raw characters must
		// not survive in a way that produces nested bold/italic spans.)
		gt.Bool(t, strings.Contains(text.Text, "*bold*")).False()
		gt.Bool(t, strings.Contains(text.Text, "_italic_")).False()
	})

	t.Run("empty title falls back to (untitled) placeholder", func(t *testing.T) {
		created := &model.Case{
			ID:             4,
			Title:          "   ",
			SlackChannelID: "C-Y",
		}
		blocks, fallback := usecase.BuildCaseCreatedTailBlocksForTest(context.Background(), created)
		gt.Array(t, blocks).Length(1).Required()
		ctxBlock, ok := blocks[0].(*goslack.ContextBlock)
		gt.Bool(t, ok).True().Required()
		text, ok := ctxBlock.ContextElements.Elements[0].(*goslack.TextBlockObject)
		gt.Bool(t, ok).True().Required()
		gt.String(t, text.Text).Contains("(untitled)")
		gt.String(t, fallback).Contains("(untitled)")
	})

	t.Run("fallback is localized via i18n", func(t *testing.T) {
		created := &model.Case{
			ID:    42,
			Title: "Tanaka incident",
		}
		enBlocks, enFallback := usecase.BuildCaseCreatedTailBlocksForTest(context.Background(), created)
		gt.Array(t, enBlocks).Length(1).Required()
		gt.Value(t, enFallback).Equal("Created case #42: Tanaka incident")

		jaCtx := i18n.ContextWithLang(context.Background(), i18n.LangJA)
		jaBlocks, jaFallback := usecase.BuildCaseCreatedTailBlocksForTest(jaCtx, created)
		gt.Array(t, jaBlocks).Length(1).Required()
		// Compare against the i18n source rather than a hardcoded Japanese
		// literal so translator tweaks do not break this test.
		gt.Value(t, jaFallback).Equal(i18n.T(jaCtx, i18n.MsgCaseCreatedFallback, created.ID, created.Title))
		gt.Value(t, jaFallback).NotEqual(enFallback)
	})
}

// TestBuildPreviewBlocks_Fallback locks the preview message's notification
// fallback to the i18n layer, including the draft title interpolation.
func TestBuildPreviewBlocks_Fallback(t *testing.T) {
	draft := &model.CaseProposal{
		ID: "draft-preview-fallback",
		Materialization: &model.WorkspaceMaterialization{
			Title:       "Broken auth",
			Description: "Login is failing for SSO users",
		},
	}
	selected := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws-1", Name: "Risk"},
	}

	t.Run("default context yields English fallback with title", func(t *testing.T) {
		blocks, fallback := usecase.BuildPreviewBlocksForTest(context.Background(), draft, selected, nil)
		gt.Number(t, len(blocks)).GreaterOrEqual(1)
		gt.Value(t, fallback).Equal("Case draft: Broken auth")
	})

	t.Run("Japanese context yields localized fallback with title", func(t *testing.T) {
		jaCtx := i18n.ContextWithLang(context.Background(), i18n.LangJA)
		blocks, fallback := usecase.BuildPreviewBlocksForTest(jaCtx, draft, selected, nil)
		gt.Number(t, len(blocks)).GreaterOrEqual(1)
		gt.Value(t, fallback).Equal(i18n.T(jaCtx, i18n.MsgMentionPreviewFallbackWithTitle, "Broken auth"))
		gt.Value(t, fallback).NotEqual("Case draft: Broken auth")
	})
}

// An integration that @-mentions the bot renders its payload into attachments
// and leaves Text empty. The persisted MentionText must carry that body,
// otherwise the planner is handed a blank mention.
func TestMentionDraftUseCase_HandleAppMention_AttachmentOnlyMentionText(t *testing.T) {
	repo := memory.New()
	schema := &config.FieldSchema{Fields: []config.FieldDefinition{
		{ID: "severity", Type: types.FieldTypeSelect,
			Options: []config.FieldOption{{ID: "low"}, {ID: "high"}}},
	}}
	registry := newRegistryWithSchema("ws-only", "OnlyWS", schema)

	slackMock := newCollectorOnlyMockSlack()
	// captured records every prompt handed to the model, so the test can assert
	// the mention body actually reached it.
	var (
		capturedMu sync.Mutex
		captured   []string
	)
	script := stubDraftScript("ws-only")
	var scriptIdx int
	llm := &mockLLMClient{
		newSessionFn: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mockLLMSession{
				generateContentFn: func(_ context.Context, input ...gollem.Input) (*gollem.Response, error) {
					capturedMu.Lock()
					defer capturedMu.Unlock()
					for _, i := range input {
						if txt, ok := i.(gollem.Text); ok {
							captured = append(captured, string(txt))
						}
					}
					if scriptIdx >= len(script) {
						return nil, errors.New("no more scripted responses")
					}
					out := script[scriptIdx]
					scriptIdx++
					return &gollem.Response{Texts: []string{out}}, nil
				},
			}, nil
		},
	}
	uc := usecase.NewMentionProposalUseCase(repo, registry, slackMock)
	bindDraftRuntime(t, uc, repo, registry, llm, slackMock)

	ev := &slackevents.AppMentionEvent{
		Channel:   "C-USER",
		User:      "U-USER",
		Text:      "",
		TimeStamp: "1700000010.000000",
		Attachments: []goslack.Attachment{{
			Title: "#297 Add a Design Doc",
			Text:  "Adds a Design Doc describing coding agent usage.",
		}},
	}

	gt.NoError(t, uc.HandleAppMention(context.Background(), ev)).Required()
	waitForDraftSessionEnd(t, repo, "C-USER", "1700000010.000000", model.SessionEndedWithMaterialize)

	capturedMu.Lock()
	defer capturedMu.Unlock()
	sawMentionBody := false
	for _, prompt := range captured {
		if strings.Contains(prompt, "#297 Add a Design Doc\nAdds a Design Doc describing coding agent usage.") {
			sawMentionBody = true
		}
	}
	gt.Bool(t, sawMentionBody).True()
}
