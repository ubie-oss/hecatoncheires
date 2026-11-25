package eval_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	eval "github.com/m-mizutani/hecatoncheires/pkg/usecase/eval"
)

func TestRun_ExternalHarnessAsksAndResumes(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var turns, simulatedReplies atomic.Int32
	worker := testHarness(func(_ context.Context, req harness.Request) (*harness.Result, error) {
		switch req.Role {
		case harness.RoleJudge:
			return &harness.Result{Text: bothPassVerdict}, nil
		case harness.RoleUserSim:
			simulatedReplies.Add(1)
			gt.String(t, req.Prompt).Contains("Who is affected?")
			return &harness.Result{Text: `{"answers":[{"id":"q","value":"Everyone using desktop"}]}`}, nil
		case "", harness.RoleAgent:
			gt.String(t, string(req.OutputSchema)).Contains("oneOf")
			if turns.Add(1) == 1 {
				return &harness.Result{Text: `{"question":{"reason":"Who is affected?","items":[{"id":"scope","type":"free_text","text":"Who is affected?"}]}}`}, nil
			}
			gt.True(t, strings.Contains(req.Prompt+"\n"+req.SystemPrompt, "Everyone using desktop"))
			return &harness.Result{Text: `{"answer":` + materializeJSON + `}`}, nil
		default:
			return nil, goerr.New("unexpected worker role", goerr.V("role", req.Role))
		}
	})

	var output bytes.Buffer
	code, err := eval.Run(t.Context(), []string{scenarioPath()}, eval.Config{
		LLM:         harness.RefusingLLM{ModelName: "test-model"},
		Harness:     worker,
		Models:      testModelPolicy(t),
		Concurrency: 1,
		Language:    "en",
		ReportPath:  reportPath,
	}, &output)
	gt.NoError(t, err)
	gt.Number(t, code).Equal(eval.ExitOK)
	gt.Value(t, turns.Load()).Equal(int32(2))
	gt.Value(t, simulatedReplies.Load()).Equal(int32(1))
	report := readReport(t, reportPath)
	gt.A(t, report.Scenarios).Length(1).Required()
	gt.String(t, report.Scenarios[0].ArtifactSnapshot).Contains("Portal login 503")
}
