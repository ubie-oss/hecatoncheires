package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/llm/claude"
	"github.com/gollem-dev/gollem/llm/gemini"
	"github.com/gollem-dev/gollem/llm/openai"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	eval "github.com/m-mizutani/hecatoncheires/pkg/usecase/eval"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

func scenarioPath() string {
	return filepath.Join("scenario", "testdata", "valid_thread_initial.toml")
}

// testModelPolicy is the one-model policy a scenario runs under: a priced
// default model and a budget far above anything a scripted LLM spends, so a
// scenario never ends on the ceiling.
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

// --- scripted LLM building blocks -----------------------------------------

// newLLM wraps a Generate func into a mock client with the session plumbing the
// planexec / threadcase loops require.
func newLLM(gen func(text string) string) *mock.LLMClientMock {
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, input []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					var sb strings.Builder
					for _, in := range input {
						sb.WriteString(in.String())
						sb.WriteString("\n")
					}
					return &gollem.Response{Texts: []string{gen(sb.String())}}, nil
				},
				HistoryFunc:       func() (*gollem.History, error) { return &gollem.History{Version: gollem.HistoryVersion}, nil },
				AppendHistoryFunc: func(_ *gollem.History) error { return nil },
				CountTokenFunc:    func(_ context.Context, _ ...gollem.Input) (int, error) { return 0, nil },
			}, nil
		},
	}
}

const (
	// Thread-mode workspaces manage no Actions; the planner is offered no core
	// (action) toolset, so the read-only Slack toolset stands in here.
	planJSON        = `{"message":"go","tasks":[{"id":"t1","title":"look","description":"investigate","acceptance_criteria":"done","tools":["slack_ro"],"budget_usd":0.01}]}`
	replanDoneJSON  = `{"message":"done","finalize":{"reason":"goal met"}}`
	subAgentSummary = "summary: portal 503 login failure, severity high."
	materializeJSON = `{"kind":"materialize","title":"Portal login 503","description":"503 on login.","fields":[{"field_id":"severity","value":"high"}]}`
	bothPassVerdict = `{"verdicts":[{"id":"title-identifies-login-failure","passed":true,"reason":"title names a portal login failure"},{"id":"severity-high","passed":true,"reason":"severity is high"}]}`
)

// materializeLLM: plan -> sub-agent -> replan done -> materialize; judge passes both.
func materializeLLM() *mock.LLMClientMock {
	return newLLM(func(text string) string {
		switch {
		case strings.Contains(text, "Artifact snapshot"):
			return bothPassVerdict
		case strings.Contains(text, "investigation loop has finished"):
			return materializeJSON
		case strings.Contains(text, "Observations from prior"):
			return replanDoneJSON
		case text == "investigate\n":
			// The sub-agent task: its user turn is the task description the plan
			// above set, which is the one input that is exactly that string.
			return subAgentSummary
		default:
			// planner round 1 (creation or mention turn) — emit the plan.
			return planJSON
		}
	})
}

// --- report.json shape (read back to assert observable outcomes) ----------

type reportFile struct {
	Totals struct {
		Scenarios    int `json:"scenarios"`
		RunOK        int `json:"run_ok"`
		Errors       int `json:"errors"`
		ChecksPassed int `json:"checks_passed"`
		ChecksTotal  int `json:"checks_total"`
	} `json:"totals"`
	Scenarios []struct {
		ID       string `json:"id"`
		EvalID   string `json:"eval_id"`
		Workflow string `json:"workflow"`
		Status   string `json:"status"`
		Err      string `json:"error"`
		DumpDir  string `json:"dump_dir"`
		Score    struct {
			Passed int `json:"Passed"`
			Total  int `json:"Total"`
		} `json:"score"`
		Checks []struct {
			ID     string `json:"ID"`
			Passed bool   `json:"Passed"`
			Reason string `json:"Reason"`
		} `json:"checks"`
		ArtifactSnapshot string `json:"artifact_snapshot"`
	} `json:"scenarios"`
}

type runFile struct {
	Score struct {
		Passed int `json:"Passed"`
		Total  int `json:"Total"`
	} `json:"score"`
	Transcript []struct {
		Turn  int    `json:"Turn"`
		Mode  string `json:"Mode"`
		Input string `json:"Input"`
	} `json:"transcript"`
	Case struct {
		Title       string `json:"Title"`
		FieldValues map[string]struct {
			Value any `json:"Value"`
		} `json:"FieldValues"`
	} `json:"case"`
}

func readReport(t *testing.T, path string) reportFile {
	t.Helper()
	data, err := os.ReadFile(path)
	gt.NoError(t, err)
	var rep reportFile
	gt.NoError(t, json.Unmarshal(data, &rep))
	return rep
}

func readRun(t *testing.T, dumpDir string) runFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dumpDir, "run.json"))
	gt.NoError(t, err)
	var rf runFile
	gt.NoError(t, json.Unmarshal(data, &rf))
	return rf
}

// --- lifecycle tests -------------------------------------------------------

func TestRun_Lifecycle_Materialize(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	var out bytes.Buffer

	code, err := eval.Run(context.Background(), []string{scenarioPath()}, eval.Config{
		LLM:         materializeLLM(),
		Models:      testModelPolicy(t),
		Concurrency: 1,
		Language:    "en",
		ReportPath:  reportPath,
		DumpDir:     filepath.Join(dir, "dump"),
		DumpAll:     true,
	}, &out)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)

	rep := readReport(t, reportPath)
	gt.Number(t, rep.Totals.Scenarios).Equal(1)
	gt.Number(t, rep.Totals.RunOK).Equal(1)
	gt.Number(t, rep.Totals.Errors).Equal(0)
	gt.Number(t, rep.Totals.ChecksPassed).Equal(2)
	gt.Number(t, rep.Totals.ChecksTotal).Equal(2)

	gt.A(t, rep.Scenarios).Length(1)
	sc := rep.Scenarios[0]
	gt.V(t, sc.ID).Equal("thread-initial-login-issue")
	gt.V(t, sc.Status).Equal("ok")
	gt.V(t, sc.EvalID).NotEqual("")
	gt.Number(t, sc.Score.Passed).Equal(2)
	gt.Number(t, sc.Score.Total).Equal(2)

	// Per-check verdicts are present, mapped to the scenario's check ids, with
	// the judge's reasons carried through.
	gt.A(t, sc.Checks).Length(2)
	gt.V(t, sc.Checks[0].ID).Equal("title-identifies-login-failure")
	gt.B(t, sc.Checks[0].Passed).True()
	gt.V(t, sc.Checks[0].Reason).NotEqual("")
	gt.V(t, sc.Checks[1].ID).Equal("severity-high")
	gt.B(t, sc.Checks[1].Passed).True()

	// The produced (materialized) case is reflected in the snapshot.
	gt.String(t, sc.ArtifactSnapshot).Contains("Portal login 503")
	gt.String(t, sc.ArtifactSnapshot).Contains("severity: high")

	// The dump captured the produced case + transcript.
	gt.V(t, sc.DumpDir).NotEqual("")
	run := readRun(t, sc.DumpDir)
	gt.V(t, run.Case.Title).Equal("Portal login 503")
	sev, ok := run.Case.FieldValues["severity"]
	gt.B(t, ok).True()
	gt.V(t, sev.Value).Equal("high")
	gt.A(t, run.Transcript).Length(1)
	gt.V(t, run.Transcript[0].Mode).Equal("create")
}

// questionLLM asks the user a question on the creation turn (first replan),
// then materializes on the mention turn (second replan done). The simulator's
// answer is echoed back via the usersim completion.
func questionLLM() *mock.LLMClientMock {
	var replans atomic.Int32
	return newLLM(func(text string) string {
		switch {
		case strings.Contains(text, "Artifact snapshot"):
			return bothPassVerdict
		case strings.Contains(text, "Return an answer for every question id"):
			// usersim: answer the single free-text item id "q" the driver builds.
			return `{"answers":[{"id":"q","value":"high"}]}`
		case strings.Contains(text, "investigation loop has finished"):
			return materializeJSON
		case strings.Contains(text, "Observations from prior"):
			if replans.Add(1) == 1 {
				return `{"question":{"reason":"What is the severity?","items":[{"id":"q-1","text":"severity?","type":"select","options":["high","low"]}]}}`
			}
			return replanDoneJSON
		case text == "investigate\n":
			// The sub-agent task: its user turn is the task description the plan
			// above set, which is the one input that is exactly that string.
			return subAgentSummary
		default:
			// planner round 1 (creation or mention turn) — emit the plan.
			return planJSON
		}
	})
}

func TestRun_Lifecycle_QuestionAnswerLoop(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	var out bytes.Buffer

	code, err := eval.Run(context.Background(), []string{scenarioPath()}, eval.Config{
		LLM:         questionLLM(),
		Models:      testModelPolicy(t),
		Concurrency: 1,
		Language:    "en",
		ReportPath:  reportPath,
		DumpDir:     filepath.Join(dir, "dump"),
		DumpAll:     true,
	}, &out)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)

	sc := readReport(t, reportPath).Scenarios[0]
	gt.V(t, sc.Status).Equal("ok")
	gt.V(t, sc.DumpDir).NotEqual("")

	run := readRun(t, sc.DumpDir)
	// The agent asked a question, the simulator answered, and the answer was
	// injected as a mention: two turns, the second carrying the answer.
	gt.A(t, run.Transcript).Length(2)
	gt.V(t, run.Transcript[0].Mode).Equal("create")
	gt.V(t, run.Transcript[1].Mode).Equal("resume")
	gt.String(t, run.Transcript[1].Input).Contains("high")
	// The case still materialized after the follow-up.
	gt.V(t, run.Case.Title).Equal("Portal login 503")
}

// badJudgeLLM materializes normally but the judge omits a verdict, which must
// surface as a scenario execution error (status=error, exit 2), not a silent
// pass.
func badJudgeLLM() *mock.LLMClientMock {
	return newLLM(func(text string) string {
		switch {
		case strings.Contains(text, "Artifact snapshot"):
			return `{"verdicts":[{"id":"title-identifies-login-failure","passed":true,"reason":"ok"}]}`
		case strings.Contains(text, "investigation loop has finished"):
			return materializeJSON
		case strings.Contains(text, "Observations from prior"):
			return replanDoneJSON
		case text == "investigate\n":
			// The sub-agent task: its user turn is the task description the plan
			// above set, which is the one input that is exactly that string.
			return subAgentSummary
		default:
			// planner round 1 (creation or mention turn) — emit the plan.
			return planJSON
		}
	})
}

func TestRun_ScenarioError_ExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	var out bytes.Buffer

	code, err := eval.Run(context.Background(), []string{scenarioPath()}, eval.Config{
		LLM:         badJudgeLLM(),
		Models:      testModelPolicy(t),
		Concurrency: 1,
		ReportPath:  reportPath,
	}, &out)
	gt.NoError(t, err) // run completes; the per-scenario error is captured, not returned
	gt.Number(t, code).Equal(eval.ExitError)

	sc := readReport(t, reportPath).Scenarios[0]
	gt.V(t, sc.Status).Equal("error")
	gt.V(t, sc.Err).NotEqual("")
}

// --- dry-run / validation / catalog (no LLM) -------------------------------

// jobScriptLLM drives the job (create an action, then summarize) and answers
// the judge. Per-session counter sequences the job's two calls; the judge call
// is routed by the "Artifact snapshot" marker.
func jobScriptLLM() *mock.LLMClientMock {
	// Counted across sessions, not per session: the run is a durable Process whose
	// every transition opens its own gollem session, so a per-session counter would
	// restart at 1 each time and the model would ask for the same tool for as long
	// as the step budget allowed.
	var calls atomic.Int32
	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, input []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					var sb strings.Builder
					for _, in := range input {
						sb.WriteString(in.String())
					}
					if strings.Contains(sb.String(), "Artifact snapshot") {
						return &gollem.Response{Texts: []string{`{"verdicts":[
							{"id":"job-succeeded","passed":true,"reason":"outcome is SUCCESS"},
							{"id":"summary-mentions-login","passed":true,"reason":"addresses the 503 login issue"}
						]}`}}, nil
					}
					if calls.Add(1) == 1 {
						// Mirror a real model's shape (assistant text accompanying the
						// tool call). NOTE: the job trace handler still logs a non-fatal
						// "ToolCall must reference a parent LLMResponse" here because
						// gollem emits LLM-call trace hooks only from provider client
						// packages, never from mock.LLMClientMock — so lastLLMResponseSeq
						// stays 0 under a mock. That orphaned-trace path cannot occur
						// with a real provider and does not affect the observable
						// outcome asserted below (the action is created, run succeeds).
						return &gollem.Response{
							Texts: []string{"Creating a follow-up action for the portal 503 login issue."},
							FunctionCalls: []*gollem.FunctionCall{{
								ID:        "c1",
								Name:      "core__create_action",
								Arguments: map[string]any{"title": "Investigate portal 503 login"},
							}},
						}, nil
					}
					return &gollem.Response{Texts: []string{"Investigated the portal 503 login issue and created a follow-up action."}}, nil
				},
				HistoryFunc:       func() (*gollem.History, error) { return &gollem.History{Version: gollem.HistoryVersion}, nil },
				AppendHistoryFunc: func(_ *gollem.History) error { return nil },
				CountTokenFunc:    func(_ context.Context, _ ...gollem.Input) (int, error) { return 0, nil },
			}, nil
		},
	}
}

func jobScenarioPath() string {
	return filepath.Join("scenario", "testdata", "job_simple.toml")
}

func TestRun_Lifecycle_JobExecution(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	var out bytes.Buffer

	code, err := eval.Run(context.Background(), []string{jobScenarioPath()}, eval.Config{
		LLM:         jobScriptLLM(),
		Models:      testModelPolicy(t),
		Concurrency: 1,
		Language:    "en",
		ReportPath:  reportPath,
		DumpDir:     filepath.Join(dir, "dump"),
		DumpAll:     true,
	}, &out)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)

	rep := readReport(t, reportPath)
	gt.A(t, rep.Scenarios).Length(1)
	sc := rep.Scenarios[0]
	gt.V(t, sc.ID).Equal("job-triage-summary")
	gt.V(t, sc.Workflow).Equal("job")
	gt.V(t, sc.Status).Equal("ok")
	gt.Number(t, sc.Score.Passed).Equal(2)
	gt.Number(t, sc.Score.Total).Equal(2)
	// The job artifact snapshot reflects a successful run with the created action.
	gt.String(t, sc.ArtifactSnapshot).Contains("Outcome: SUCCESS")
	gt.String(t, sc.ArtifactSnapshot).Contains("Investigate portal 503 login")
}

type testHarness func(context.Context, harness.Request) (*harness.Result, error)

func (f testHarness) Run(ctx context.Context, req harness.Request) (*harness.Result, error) {
	return f(ctx, req)
}

func TestRun_ExternalHarnessExecutesJobToolsAndJudges(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	var agentCalls, judgeCalls atomic.Int32
	external := testHarness(func(ctx context.Context, req harness.Request) (*harness.Result, error) {
		if req.Role == harness.RoleJudge {
			judgeCalls.Add(1)
			gt.String(t, req.Prompt).Contains("Investigate portal 503 login")
			gt.String(t, req.Prompt).Contains("Created a follow-up for the login issue.")
			gt.String(t, req.Prompt).Contains("# Case before run")
			gt.String(t, req.Prompt).Contains("Users report 503 on login since this morning.")
			return &harness.Result{Text: `{"verdicts":[
				{"id":"job-succeeded","passed":true,"reason":"job succeeded"},
				{"id":"summary-mentions-login","passed":true,"reason":"addresses login"}
			]}`}, nil
		}
		agentCalls.Add(1)
		_, err := req.CallTool(ctx, gollem.FunctionCall{
			ID: "c1", Name: "core__create_action",
			Arguments: map[string]any{"title": "Investigate portal 503 login"},
		})
		if err != nil {
			return nil, err
		}
		return &harness.Result{Text: "Created a follow-up for the login issue."}, nil
	})

	var out bytes.Buffer
	code, err := eval.Run(t.Context(), []string{jobScenarioPath()}, eval.Config{
		LLM:         harness.RefusingLLM{ModelName: "test-model"},
		Harness:     external,
		Models:      testModelPolicy(t),
		Concurrency: 1,
		Language:    "en",
		ReportPath:  reportPath,
	}, &out)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)
	gt.Value(t, agentCalls.Load()).Equal(int32(1))
	gt.Value(t, judgeCalls.Load()).Equal(int32(1))

	rep := readReport(t, reportPath)
	gt.A(t, rep.Scenarios).Length(1).Required()
	gt.String(t, rep.Scenarios[0].ArtifactSnapshot).Contains("Outcome: SUCCESS")
	gt.String(t, rep.Scenarios[0].ArtifactSnapshot).Contains("Investigate portal 503 login")
	gt.String(t, rep.Scenarios[0].ArtifactSnapshot).Contains("Created a follow-up for the login issue.")
}

func TestRun_DryRunNoLLM(t *testing.T) {
	var out bytes.Buffer
	code, err := eval.Run(context.Background(), []string{scenarioPath()}, eval.Config{DryRun: true}, &out)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)
	gt.String(t, out.String()).Contains("ok  thread-initial-login-issue")
}

func TestRun_NoLLMWhenRunning(t *testing.T) {
	var out bytes.Buffer
	_, err := eval.Run(context.Background(), []string{scenarioPath()}, eval.Config{}, &out)
	gt.Error(t, err)
}

func TestRun_NoFiles(t *testing.T) {
	var out bytes.Buffer
	_, err := eval.Run(context.Background(), []string{t.TempDir()}, eval.Config{DryRun: true}, &out)
	gt.Error(t, err)
}

func TestRun_DryRunRejectsUnknownWorkflow(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	// Minimal scenario with an unknown workflow; workspace block reused from the
	// fixture is not needed because validation fails before workspace use... but
	// Load still parses the workspace, so copy a valid one.
	src, err := os.ReadFile(scenarioPath())
	gt.NoError(t, err)
	mutated := strings.Replace(string(src), `workflow    = "thread_mode_initial"`, `workflow    = "nope"`, 1)
	gt.NoError(t, os.WriteFile(bad, []byte(mutated), 0o600))

	var out bytes.Buffer
	_, err = eval.Run(context.Background(), []string{bad}, eval.Config{DryRun: true}, &out)
	gt.Error(t, err)
}

func TestToolCatalog(t *testing.T) {
	cat := eval.ToolCatalog()
	gt.A(t, cat).Length(8).Required()

	names := make(map[string]bool, len(cat))
	for _, e := range cat {
		names[e.Name] = true
	}
	gt.Bool(t, names["slack_search"]).True()
	gt.Bool(t, names["notion_search"]).True()
	gt.Bool(t, names["github_search"]).True()
	gt.Bool(t, names["jira_search"]).True()
	gt.Bool(t, names["webfetch"]).True()
	gt.Bool(t, names["knowledge__create_tag"]).True()
	gt.Bool(t, names["knowledge__update_tag"]).True()
	gt.Bool(t, names["knowledge__delete_tag"]).True()
}

// --- real-LLM end-to-end test ---------------------------------------------

// realLLMFromEnv builds a live gollem client from TEST_-prefixed env vars so the
// eval harness can be exercised against a real model. The test is skipped unless
// TEST_LLM_PROVIDER is set; only TEST_-prefixed variables are consulted (no
// .env* reading, no production env names).
//
//	TEST_LLM_PROVIDER          openai | claude | gemini   (gate; skip if unset)
//	TEST_LLM_MODEL             optional model override
//	TEST_LLM_OPENAI_API_KEY    required for openai (or claude direct)
//	TEST_LLM_CLAUDE_API_KEY    required for claude (direct API)
//	TEST_LLM_GEMINI_PROJECT_ID required for gemini / claude-on-vertex
//	TEST_LLM_GEMINI_LOCATION   required for gemini / claude-on-vertex
func realLLMFromEnv(t *testing.T) gollem.LLMClient {
	t.Helper()
	provider := os.Getenv("TEST_LLM_PROVIDER")
	if provider == "" {
		t.Skip("TEST_LLM_PROVIDER not set; skipping real-LLM eval test")
	}
	ctx := context.Background()
	model := os.Getenv("TEST_LLM_MODEL")

	switch provider {
	case "openai":
		key := os.Getenv("TEST_LLM_OPENAI_API_KEY")
		gt.Value(t, key).NotEqual("")
		var opts []openai.Option
		if model != "" {
			opts = append(opts, openai.WithModel(model))
		}
		client, err := openai.New(ctx, key, opts...)
		gt.NoError(t, err).Required()
		return client

	case "claude":
		key := os.Getenv("TEST_LLM_CLAUDE_API_KEY")
		project := os.Getenv("TEST_LLM_GEMINI_PROJECT_ID")
		switch {
		case key != "":
			var opts []claude.Option
			if model != "" {
				opts = append(opts, claude.WithModel(model))
			}
			client, err := claude.New(ctx, key, opts...)
			gt.NoError(t, err).Required()
			return client
		case project != "":
			location := os.Getenv("TEST_LLM_GEMINI_LOCATION")
			gt.Value(t, location).NotEqual("")
			var opts []claude.VertexOption
			if model != "" {
				opts = append(opts, claude.WithVertexModel(model))
			}
			client, err := claude.NewWithVertex(ctx, location, project, opts...)
			gt.NoError(t, err).Required()
			return client
		default:
			t.Skip("claude provider needs TEST_LLM_CLAUDE_API_KEY or TEST_LLM_GEMINI_PROJECT_ID")
			return nil
		}

	case "gemini":
		project := os.Getenv("TEST_LLM_GEMINI_PROJECT_ID")
		location := os.Getenv("TEST_LLM_GEMINI_LOCATION")
		gt.Value(t, project).NotEqual("")
		gt.Value(t, location).NotEqual("")
		var opts []gemini.Option
		if model != "" {
			opts = append(opts, gemini.WithModel(model))
		}
		client, err := gemini.New(ctx, project, location, opts...)
		gt.NoError(t, err).Required()
		return client

	default:
		t.Skipf("unsupported TEST_LLM_PROVIDER=%q", provider)
		return nil
	}
}

// TestRun_RealLLM_ThreadInitial runs the thread_mode_initial scenario end-to-end
// against a real model (planner -> sub-agents -> replan -> materialize -> judge)
// and asserts the harness's *plumbing* contract — the part that must hold no
// matter how well the model performs. It deliberately does NOT assert on the
// quality of the produced case (title wording, whether materialize succeeded,
// which checks passed): that is precisely what the eval measures, and pinning it
// would turn this into a flaky verdict on the model rather than on the harness.
//
// What it proves: a real planner/sub-agent/judge round-trip happened, every
// structured output parsed, the judge returned one verdict per check (mapped by
// id), and a case artifact + dump were produced.
func TestRun_RealLLM_ThreadInitial(t *testing.T) {
	llm := realLLMFromEnv(t)

	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	var out bytes.Buffer

	code, err := eval.Run(context.Background(), []string{scenarioPath()}, eval.Config{
		LLM:         llm,
		Models:      testModelPolicy(t),
		Concurrency: 1,
		Language:    "en",
		ReportPath:  reportPath,
		DumpDir:     filepath.Join(dir, "dump"),
		DumpAll:     true,
	}, &out)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)

	rep := readReport(t, reportPath)
	gt.A(t, rep.Scenarios).Length(1).Required()
	sc := rep.Scenarios[0]
	gt.V(t, sc.ID).Equal("thread-initial-login-issue")
	// status=ok means the harness drove the whole flow without a plumbing/judge
	// error — independent of whether the checks passed.
	gt.V(t, sc.Status).Equal("ok")
	gt.V(t, sc.DumpDir).NotEqual("")

	// A verdict for every check — proves the judge LLM ran, returned parseable
	// JSON, and the harness mapped each scenario check id to a verdict. (Whether a
	// check passed is the eval's finding, not this test's pass condition.)
	gt.Number(t, sc.Score.Total).Equal(2)
	gt.A(t, sc.Checks).Length(2)
	for _, ck := range sc.Checks {
		gt.V(t, ck.ID).NotEqual("")
		gt.V(t, ck.Reason).NotEqual("")
	}

	// A case artifact was produced and dumped: a case exists with a title and at
	// least the creation turn was recorded.
	run := readRun(t, sc.DumpDir)
	gt.V(t, run.Case.Title).NotEqual("")
	gt.Number(t, len(run.Transcript)).GreaterOrEqual(1)
	gt.V(t, run.Transcript[0].Mode).Equal("create")
}
