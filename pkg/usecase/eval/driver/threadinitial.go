package driver

import (
	"context"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	slackmodel "github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/env"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/evaltype"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/eval/scenario"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
)

const (
	// defaultReporter is used when the scenario does not pin one.
	defaultReporter = "U-EVALUSER"
	evalTeamID      = "T-EVAL"
)

// ThreadInitial drives the thread-mode initial sequence: a top-level post
// starts the initialization (create) agent. The agent investigates and may ask
// the user a question instead of creating a case immediately; when it does, the
// simulator answers and the answer is injected as a thread reply that resumes
// the create agent, looping up to the persona's MaxAnswerTurns until the agent
// commits a case. The produced case is the artifact.
type ThreadInitial struct{}

// NewThreadInitial builds the driver.
func NewThreadInitial() *ThreadInitial { return &ThreadInitial{} }

// Kind implements WorkflowDriver.
func (*ThreadInitial) Kind() string { return "thread_mode_initial" }

// Run implements WorkflowDriver.
func (*ThreadInitial) Run(ctx context.Context, e *env.Env, sc *scenario.Scenario, sim evaltype.Simulator) (evaltype.Artifact, error) {
	logger := logging.From(ctx)
	if lang, err := i18n.ParseLang(sc.Meta.Language); err == nil && sc.Meta.Language != "" {
		ctx = i18n.ContextWithLang(ctx, lang)
	}

	wsID := e.Entry.Workspace.ID
	channel := e.MonitorChannel
	reporter := sc.Input.Reporter
	if reporter == "" {
		reporter = defaultReporter
	}

	now := e.Now()
	runThreadTS := e.Slack.NextTimestamp()
	e.Slack.Receive(channel, runThreadTS, runThreadTS, reporter, sc.Input.Text)
	first := slackmodel.NewMessageFromData(runThreadTS, channel, "", evalTeamID, reporter, reporter, sc.Input.Text, runThreadTS, now, nil)

	logger.Info("eval turn: create (case initialization)", "scenario", sc.Meta.ID, "channel", channel)
	if err := e.AgentUC.HandleThreadCaseCreation(ctx, first, e.Entry); err != nil {
		return nil, goerr.Wrap(err, "thread case creation")
	}
	async.Wait()
	// The turn is a durable run: the entry point returned once it was recorded,
	// and everything inspected below is written by its completion handler.
	if err := e.AwaitTurn(ctx, channel, runThreadTS, nil); err != nil {
		return nil, err
	}

	transcript := []evaltype.TurnRecord{{Turn: 1, Mode: "create", Input: sc.Input.Text}}

	for turn := 2; turn <= sc.Persona.MaxAnswerTurns+1; turn++ {
		session, err := e.Repo.Session().GetByThread(ctx, channel, runThreadTS)
		if err != nil {
			return nil, goerr.Wrap(err, "load session")
		}
		if session == nil || session.LastAction != model.SessionEndedWithQuestion {
			break // no pending question -> the agent created the case (or gave up)
		}

		if session.PendingQuestion == nil {
			return nil, goerr.New("question outcome is missing the pending question")
		}
		q := pendingQuestion(session.PendingQuestion)
		ans, err := sim.Answer(ctx, q)
		if err != nil {
			return nil, goerr.Wrap(err, "user simulator answer")
		}
		answerText := joinAnswers(ans)

		// The case does NOT exist yet (the agent deferred creation to ask).
		// Inject the answer as a thread reply that resumes the create agent.
		replyTS := e.Slack.NextTimestamp()
		e.Slack.Receive(channel, runThreadTS, replyTS, reporter, answerText)
		reply := slackmodel.NewMessageFromData(replyTS, channel, runThreadTS, evalTeamID, reporter, reporter, answerText, replyTS, e.Now(), nil)

		logger.Info("eval turn: resume create (answer injected)", "scenario", sc.Meta.ID, "turn", turn)
		if err := e.AgentUC.ResumeThreadCaseCreation(ctx, reply, e.Entry); err != nil {
			return nil, goerr.Wrap(err, "resume thread case creation")
		}
		async.Wait()
		// Wait for the resumed run to reach its own outcome, not the question this
		// turn is answering.
		if err := e.AwaitTurn(ctx, channel, runThreadTS, session); err != nil {
			return nil, err
		}

		transcript = append(transcript, evaltype.TurnRecord{
			Turn:     turn,
			Mode:     "resume",
			Input:    answerText,
			Question: &q,
			Answer:   &ans,
		})
	}

	finalCase, err := e.Repo.Case().GetBySlackThread(ctx, wsID, channel, runThreadTS)
	if err != nil {
		return nil, goerr.Wrap(err, "load final case")
	}
	if finalCase == nil {
		return nil, errNoCase
	}

	logger.Info("eval: artifact ready", "scenario", sc.Meta.ID, "tool_calls", len(e.Recorder.Records()))
	return &evaltype.CaseArtifact{
		Case:       finalCase,
		Transcript: transcript,
		ToolCalls:  e.Recorder.Records(),
		Posts:      e.Slack.Posts(),
	}, nil
}

func pendingQuestion(p *model.PendingQuestion) evaltype.Question {
	q := evaltype.Question{Reason: p.Reason, Items: make([]evaltype.QuestionItem, 0, len(p.Items))}
	for _, item := range p.Items {
		q.Items = append(q.Items, evaltype.QuestionItem{
			ID:      item.ID,
			Type:    evaltype.QuestionType(item.Type),
			Text:    item.Text,
			Options: item.Options,
		})
	}
	return q
}

func joinAnswers(ans evaltype.Answers) string {
	out := ""
	for _, a := range ans.Items {
		if a.Value != "" {
			out += a.Value + " "
		}
		for _, v := range a.Values {
			out += v + " "
		}
	}
	if out == "" {
		return "(no answer)"
	}
	return out
}
