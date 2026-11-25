package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/evaltype"
)

// runDump is the run.json shape: everything needed to follow what happened from
// the scenario's point of view.
type runDump struct {
	ScenarioID       string                    `json:"scenario_id"`
	EvalID           string                    `json:"eval_id"`
	Workflow         string                    `json:"workflow"`
	Status           string                    `json:"status"`
	Score            evaltype.Score            `json:"score"`
	Checks           []evaltype.CheckVerdict   `json:"checks"`
	ArtifactKind     string                    `json:"artifact_kind,omitempty"`
	ArtifactSnapshot string                    `json:"artifact_snapshot,omitempty"`
	Transcript       []evaltype.TurnRecord     `json:"transcript,omitempty"`
	ToolCalls        []evaltype.ToolCallRecord `json:"tool_calls,omitempty"`
	Case             any                       `json:"case,omitempty"`
	Posts            []evaltype.SlackPost      `json:"posts,omitempty"`
}

// Dump writes the diagnostic bundle for one scenario under
// <dir>/<scenario-id>/<eval-id>/ and returns that directory. traces is the raw
// gollem trace snapshot (keyed sessionID/traceID).
func Dump(dir string, r evaltype.ScenarioResult, traces map[string]json.RawMessage, language string) (string, error) {
	target := filepath.Join(dir, sanitize(r.ScenarioID), r.EvalID)
	if err := os.MkdirAll(target, 0o750); err != nil {
		return "", goerr.Wrap(err, "create dump dir", goerr.V("dir", target))
	}

	if err := writeJSONFile(filepath.Join(target, "trace.json"), traces); err != nil {
		return "", err
	}

	rd := runDump{
		ScenarioID: r.ScenarioID,
		EvalID:     r.EvalID,
		Workflow:   r.Workflow,
		Status:     r.Status,
		Score:      r.Score,
		Checks:     r.Checks,
	}
	if r.Artifact != nil {
		rd.ArtifactKind = r.Artifact.Kind()
		rd.ArtifactSnapshot = r.Artifact.Render()
	}
	switch a := r.Artifact.(type) {
	case *evaltype.CaseArtifact:
		if a != nil {
			rd.Transcript = a.Transcript
			rd.ToolCalls = a.ToolCalls
			rd.Case = a.Case
			rd.Posts = a.Posts
		}
	case *evaltype.JobArtifact:
		if a != nil {
			rd.ToolCalls = a.ToolCalls
			rd.Case = a.Case
			rd.Posts = a.Posts
		}
	}
	if err := writeJSONFile(filepath.Join(target, "run.json"), rd); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(target, "analysis.md"), []byte(renderAnalysis(r, language)), 0o600); err != nil {
		return "", goerr.Wrap(err, "write analysis.md")
	}

	return target, nil
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return goerr.Wrap(err, "marshal dump", goerr.V("path", path))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return goerr.Wrap(err, "write dump file", goerr.V("path", path))
	}
	return nil
}

// renderAnalysis builds a ready-to-hand-off analysis request for a separate
// session. The template is English; languageDirective tells the analyst which
// language to respond in (so the harness keeps English source literals while
// still honoring --lang for the produced analysis).
func renderAnalysis(r evaltype.ScenarioResult, language string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Eval analysis request: %s\n\n", r.ScenarioID)
	fmt.Fprintf(&b, "Workflow: `%s` / score: %d/%d\n\n", r.Workflow, r.Score.Passed, r.Score.Total)
	if dir := languageDirective(language); dir != "" {
		fmt.Fprintf(&b, "**Respond in %s.**\n\n", dir)
	}
	b.WriteString("Below is an eval run. Given the failing checks, analyze and propose what to fix in the agent's prompts, tools, or behavior.\n")
	b.WriteString("See `run.json` (transcript / tool calls / final case) and `trace.json` (planner / sub-agent execution trace) in this directory for details.\n\n")

	b.WriteString("## Checks\n")
	for _, v := range r.Checks {
		mark := "PASS"
		if !v.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "- [%s] %s: %s\n  reason: %s\n", mark, v.ID, v.Question, v.Reason)
	}

	if r.Artifact != nil {
		b.WriteString("\n## Produced artifact\n```\n")
		b.WriteString(r.Artifact.Render())
		b.WriteString("\n```\n")
	}

	b.WriteString("\n## Task\nFor each failing check above, give a hypothesis for the cause and a concrete fix (prompt change, tool addition/adjustment, workflow change, etc.).\n")

	return b.String()
}

// languageDirective maps a language code to an English language name used in
// the "Respond in ..." directive. Empty means leave it to the analyst.
func languageDirective(language string) string {
	switch language {
	case "ja":
		return "Japanese"
	case "en":
		return "English"
	default:
		return ""
	}
}

// sanitize keeps a scenario id safe as a single path segment.
func sanitize(id string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	return strings.Map(repl, id)
}
