package model

import (
	"slices"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/robfig/cron/v3"

	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// CaseLifecycle enumerates the case lifecycle events that a Job can listen
// to via `events.case.on` in TOML. Each value names a distinct, observable
// transition.
type CaseLifecycle string

const (
	// CaseLifecycleCreated fires when a Case is newly created (regardless of
	// its initial status — DRAFT or OPEN both qualify).
	CaseLifecycleCreated CaseLifecycle = "created"
	// CaseLifecycleClosed fires when a Case's status transitions to CLOSED.
	CaseLifecycleClosed CaseLifecycle = "closed"
)

// AllCaseLifecycles returns every valid CaseLifecycle for validation and
// documentation purposes.
func AllCaseLifecycles() []CaseLifecycle {
	return []CaseLifecycle{
		CaseLifecycleCreated,
		CaseLifecycleClosed,
	}
}

// IsValid reports whether the lifecycle value is one of the recognised
// enum members.
func (l CaseLifecycle) IsValid() bool {
	switch l {
	case CaseLifecycleCreated, CaseLifecycleClosed:
		return true
	default:
		return false
	}
}

// String returns the string form for prompt rendering / logging.
func (l CaseLifecycle) String() string { return string(l) }

// JobEventDomain enumerates the event domains the dispatcher recognises.
// Each domain has a distinct filter schema (see CaseEventConfig /
// ScheduledEventConfig).
type JobEventDomain string

const (
	JobEventDomainCase      JobEventDomain = "case"
	JobEventDomainScheduled JobEventDomain = "scheduled"

	// JobEventDomainManual tags a run started by an explicit operator
	// request from the web UI. No Job ever *subscribes* to it — it is a
	// provenance tag only, so JobEvents carries no manual filter and the
	// dispatcher's event matching never matches it. The manual path
	// bypasses matching entirely and drives the runner directly.
	JobEventDomainManual JobEventDomain = "manual"
)

// CaseEventConfig is the listen filter for the `case` event domain. A Job
// fires when the published Case event's CaseLifecycle is contained in On.
type CaseEventConfig struct {
	// On lists the lifecycle events the Job is subscribed to. Always a
	// normalised, non-empty slice produced by the config loader from the
	// TOML "on" field. The domain layer never accepts a single-string form
	// — see config/job.go for the parsing rules.
	On []CaseLifecycle
}

// Matches reports whether the given lifecycle event matches this config's
// On filter.
func (c *CaseEventConfig) Matches(lc CaseLifecycle) bool {
	if c == nil {
		return false
	}
	return slices.Contains(c.On, lc)
}

// Validate enforces invariants for the case event filter:
// - On must be non-empty
// - every value must be a known CaseLifecycle
func (c *CaseEventConfig) Validate() error {
	if c == nil {
		return goerr.New("case event config is nil")
	}
	if len(c.On) == 0 {
		return goerr.New("events.case.on must not be empty")
	}
	seen := make(map[CaseLifecycle]struct{}, len(c.On))
	for _, lc := range c.On {
		if !lc.IsValid() {
			return goerr.New("invalid case lifecycle value",
				goerr.V("value", string(lc)))
		}
		if _, dup := seen[lc]; dup {
			return goerr.New("duplicate case lifecycle in on",
				goerr.V("value", string(lc)))
		}
		seen[lc] = struct{}{}
	}
	return nil
}

// ScheduledEventConfig is the listen filter for the `scheduled` event
// domain. Exactly one of Every / Cron must be populated.
type ScheduledEventConfig struct {
	// Every is the duration since the last run after which the Job becomes
	// due. Mutually exclusive with Cron.
	Every time.Duration

	// Cron is the parsed schedule for cron-style triggers. Mutually
	// exclusive with Every. Parsed at config load time so a malformed
	// expression fails loud before any Job dispatch.
	Cron cron.Schedule

	// CronExpr is the original cron expression preserved for display and
	// system-prompt rendering. Empty when Every is in use.
	CronExpr string
}

// Validate enforces the mutual exclusion: exactly one of Every / Cron.
func (s *ScheduledEventConfig) Validate() error {
	if s == nil {
		return goerr.New("scheduled event config is nil")
	}
	hasEvery := s.Every > 0
	hasCron := s.Cron != nil
	switch {
	case hasEvery && hasCron:
		return goerr.New("events.scheduled must specify exactly one of every or cron, not both")
	case !hasEvery && !hasCron:
		return goerr.New("events.scheduled must specify either every or cron")
	}
	return nil
}

// JobEvents collects every event filter a Job listens to. A nil pointer
// means the corresponding domain is not subscribed; a non-nil pointer
// indicates the Job listens to that domain with the given filter.
type JobEvents struct {
	Case      *CaseEventConfig
	Scheduled *ScheduledEventConfig
}

// Validate enforces invariants for the event map:
// - at least one of Case / Scheduled must be non-nil
// - each non-nil sub-config must itself be valid
func (e *JobEvents) Validate() error {
	if e == nil {
		return goerr.New("events is nil")
	}
	if e.Case == nil && e.Scheduled == nil {
		return goerr.New("job must subscribe to at least one event domain (case or scheduled)")
	}
	if e.Case != nil {
		if err := e.Case.Validate(); err != nil {
			return goerr.Wrap(err, "events.case is invalid")
		}
	}
	if e.Scheduled != nil {
		if err := e.Scheduled.Validate(); err != nil {
			return goerr.Wrap(err, "events.scheduled is invalid")
		}
	}
	return nil
}

// JobStrategy selects which execution runtime drives a Job. The default
// (zero value) is JobStrategySimple, which preserves the v1 single-loop
// behaviour. JobStrategyPlanexec opts the Job into the planexec
// (plan-and-execute) runtime shared with proposal — useful when the
// Job needs to investigate, replan, and produce a structured summary
// rather than emit a single ReAct turn.
type JobStrategy string

const (
	// JobStrategySimple drives the Job through the v1
	// SingleLoopJobExecutor (one gollem.Agent.Execute call with the
	// configured tool set).
	JobStrategySimple JobStrategy = "simple"

	// JobStrategyPlanexec drives the Job through the planexec runtime
	// (plan → parallel sub-agents → replan → final response). The host
	// disables Question because Jobs run unattended.
	JobStrategyPlanexec JobStrategy = "planexec"
)

// IsValid reports whether s is one of the recognised strategy values.
// The zero value (empty string) is NOT considered valid here — callers
// MUST normalise via NormaliseJobStrategy first, which substitutes
// JobStrategySimple for the empty input. This split keeps Validate
// strict (any non-empty unknown value is rejected) while letting TOML
// omit the field entirely.
func (s JobStrategy) IsValid() bool {
	switch s {
	case JobStrategySimple, JobStrategyPlanexec:
		return true
	default:
		return false
	}
}

// String returns the canonical string form for prompt rendering / logs.
func (s JobStrategy) String() string { return string(s) }

// NormaliseJobStrategy collapses the empty value to JobStrategySimple
// so callers downstream of TOML loading can treat every Job uniformly.
// Unknown non-empty values pass through unchanged and are then rejected
// by Validate, surfacing the typo at config-load time.
func NormaliseJobStrategy(s JobStrategy) JobStrategy {
	if s == "" {
		return JobStrategySimple
	}
	return s
}

// Job is a workspace-scoped, declaratively configured agent that runs in
// response to events. Jobs are loaded from workspace TOML and held in
// memory on the WorkspaceEntry — they are not persisted to a backend.
type Job struct {
	// ID is the workspace-unique identifier (kebab-case).
	ID string

	// Name and Description are human-facing labels used in logs and
	// system prompts.
	Name        string
	Description string

	// Prompt is the user prompt template (Go text/template). Job-specific
	// behaviour is fully expressed here; runtime context (Case / Workspace
	// / Event) is injected as template data.
	Prompt string

	// Disabled defaults to false (= active). Setting it true silently
	// excludes the Job from event matching without removing the TOML
	// entry.
	Disabled bool

	// Quiet defaults to false. When true, the Job runs without emitting
	// operational Slack notifications (the "starting..." marker, the
	// per-run session-log thread, and the completion/failure markers).
	// It does NOT silence the agent's own slack__post_message tool — that
	// is a deliberate agent action, not an operational log.
	Quiet bool

	// Strategy selects which execution runtime drives this Job. Empty
	// (the zero value) is equivalent to JobStrategySimple; the config
	// loader normalises before Validate runs.
	Strategy JobStrategy

	// Interactive enables mid-run user interaction. When true, the Job may
	// suspend a run to ask the user a question (planexec Question), persist
	// the pending interaction on its run state, post a Slack form, and
	// resume when the user answers. Defaults to false (= the run is fully
	// unattended). Only meaningful for JobStrategyPlanexec: the simple
	// single-loop runtime has no Question mechanism, so Validate rejects
	// interactive simple Jobs. Independent of Quiet — the question form is a
	// deliberate agent interaction, not an operational log, so Quiet does
	// not suppress it.
	Interactive bool

	// Reflection, when true, runs a post-execution reflection pass after a
	// successful run: a knowledge-only agent reviews the run's conversation
	// history and curates the workspace's shared Knowledge / Tags. Defaults
	// to false. Skipped for private cases and for failed runs.
	Reflection bool

	// LLMModel is the reference name of the model this Job's runs generate
	// through — an [[llm_model]] entry's alias in the global config, or its
	// model name when it declares none. Empty means the deployment's default
	// model.
	//
	// Whether the name is actually defined is NOT checked here: the definitions
	// live in a different configuration document, which this model knows
	// nothing about. config.ValidateJobModels checks it at startup and in
	// `validate`.
	LLMModel string

	// Execution is an optional job-local model and orchestration policy.
	Execution *JobExecution `json:",omitempty"`

	// Budget is the greatest amount one run of this Job may spend, in nano USD.
	// Zero means the deployment's default budget applies.
	Budget pricing.NanoUSD

	// Events maps the event domains this Job subscribes to. Validate
	// guarantees at least one non-nil entry.
	Events JobEvents
}

// ListensCase reports whether the Job subscribes to the given case lifecycle.
func (j *Job) ListensCase(lc CaseLifecycle) bool {
	if j == nil || j.Disabled {
		return false
	}
	return j.Events.Case.Matches(lc)
}

// ListensScheduled reports whether the Job subscribes to the scheduled domain.
func (j *Job) ListensScheduled() bool {
	if j == nil || j.Disabled {
		return false
	}
	return j.Events.Scheduled != nil
}

// Validate enforces the full Job invariant set: identity, prompt presence,
// event-map well-formedness, and strategy enum membership.
func (j *Job) Validate() error {
	if j == nil {
		return goerr.New("job is nil")
	}
	if j.ID == "" {
		return goerr.New("job id is empty")
	}
	if j.Prompt == "" {
		return goerr.New("job prompt is empty",
			goerr.V("job_id", j.ID))
	}
	if j.Strategy != "" && !j.Strategy.IsValid() {
		return goerr.New("job strategy is invalid",
			goerr.V("job_id", j.ID),
			goerr.V("strategy", string(j.Strategy)))
	}
	if err := j.Execution.Validate(); err != nil {
		return goerr.Wrap(err, "invalid job execution", goerr.V("job_id", j.ID))
	}
	if j.Execution != nil && j.Execution.AppliesTo(j.Strategy) && j.Interactive && j.Execution.Cascade {
		return goerr.New("interactive jobs do not support cascade execution", goerr.V("job_id", j.ID))
	}
	// Interactive relies on the planexec Question mechanism; the simple
	// single-loop runtime has no way to solicit input mid-run. Reject the
	// combination at config-load time rather than silently ignoring the flag.
	if j.Interactive && NormaliseJobStrategy(j.Strategy) != JobStrategyPlanexec {
		return goerr.New("interactive job requires strategy=planexec",
			goerr.V("job_id", j.ID),
			goerr.V("strategy", string(j.Strategy)))
	}
	// A negative budget would be read as "not specified" by the runtime and
	// silently hand the run the deployment default, which is the opposite of
	// what whoever wrote it meant.
	if j.Budget < 0 {
		return goerr.New("job budget must not be negative",
			goerr.V("job_id", j.ID),
			goerr.V("budget", int64(j.Budget)))
	}
	if err := j.Events.Validate(); err != nil {
		return goerr.Wrap(err, "job events invalid",
			goerr.V("job_id", j.ID))
	}
	return nil
}
