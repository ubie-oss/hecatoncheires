package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	jobruntime "github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/robfig/cron/v3"
)

// jobIDPattern matches snake_case identifiers. Job IDs are surfaced in
// system prompts and logging, so we keep them human-readable. Kebab-case
// (hyphens) is deliberately rejected so Job IDs stay valid identifiers.
var jobIDPattern = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// cronParser uses the standard 5-field schedule (minute hour dom month dow)
// in UTC. Anything more exotic (seconds, descriptors) is rejected at load
// time.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// JobSection is the TOML shape of a [[job]] entry. All event-shape
// validation runs at config load time: the runtime layer
// (pkg/usecase/job) trusts that every Job it receives has already been
// vetted.
type JobSection struct {
	ID          string `toml:"id"`
	Name        string `toml:"name"`
	Description string `toml:"description"`
	// Prompt is the inline prompt template. Exactly one of Prompt or
	// PromptFile must be set; supplying both, or neither, fails at config
	// load time.
	Prompt string `toml:"prompt"`
	// PromptFile points to a file holding the prompt template, resolved
	// relative to the config file's directory. It exists so long prompts can
	// live outside the TOML instead of being inlined. The file contents
	// replace Prompt once read (see resolvePrompt); the runtime layer only
	// ever sees the resolved model.Job.Prompt.
	PromptFile string `toml:"prompt_file"`
	Disabled   bool   `toml:"disabled"`
	// Quiet, when true, suppresses the operational Slack notifications a
	// Job run normally emits (the "starting..." marker, per-run session-log
	// thread, and completion/failure markers). Defaults to false.
	Quiet bool `toml:"quiet"`
	// Strategy selects the execution runtime for this Job. Empty falls
	// back to "simple" (the v1 SingleLoopJobExecutor); set to "planexec"
	// to drive the Job through the plan-and-execute runtime shared with
	// proposal. Unknown values fail loud at config load time.
	Strategy string `toml:"strategy"`
	// Reflection enables the post-execution reflection pass that curates
	// workspace Knowledge from a successful run's conversation history.
	// Defaults to false. Skipped for private cases and failed runs.
	Reflection bool `toml:"reflection"`
	// Interactive enables mid-run user interaction (planexec Question →
	// Slack form → resume). Defaults to false. Requires strategy="planexec";
	// the combination with simple is rejected at config load time by
	// model.Job.Validate.
	Interactive bool `toml:"interactive"`
	// LLMModel names which model this Job generates through: the reference name
	// of an [[llm_model]] entry in the global config (its alias, or its model
	// name when it declares no alias). Empty uses the deployment's default
	// model. Only the NAME's shape is checked here — whether it is defined
	// lives in another document, and is checked by ValidateJobModels.
	LLMModel  string               `toml:"llm_model"`
	Execution *JobExecutionSection `toml:"execution"`
	// BudgetUSD is the greatest amount in USD one run of this Job may spend,
	// sub-agents included. Zero (the default) uses the deployment's default
	// budget.
	BudgetUSD float64          `toml:"budget_usd"`
	Events    JobEventsSection `toml:"events"`
}

// JobEventsSection mirrors the `events.<domain> = { ... }` map. At least
// one sub-domain pointer must be non-nil; both may be set simultaneously.
type JobEventsSection struct {
	Case      *CaseEventSection      `toml:"case"`
	Scheduled *ScheduledEventSection `toml:"scheduled"`
}

// CaseEventSection is the filter for `events.case`. The TOML `on` field is
// always an array; we deliberately do not accept a single string so the
// schema stays type-safe.
type CaseEventSection struct {
	On []string `toml:"on"`
}

// ScheduledEventSection is the filter for `events.scheduled`. Exactly one
// of Every / Cron is required.
type ScheduledEventSection struct {
	Every string `toml:"every"`
	Cron  string `toml:"cron"`
}

// Validate parses and validates a single JobSection, returning a fully
// resolved model.Job on success. baseDir is the directory of the config
// file, used to resolve a relative prompt_file path. Returns an error
// wrapped with the job index so the caller can include it in its error
// message.
func (s *JobSection) Validate(baseDir string) (*model.Job, error) {
	if s == nil {
		return nil, goerr.New("job section is nil")
	}
	if s.ID == "" {
		return nil, goerr.New("job id is required")
	}
	if !jobIDPattern.MatchString(s.ID) {
		return nil, goerr.New("job id must be snake_case (^[a-z0-9]+(_[a-z0-9]+)*$)",
			goerr.V("job_id", s.ID))
	}

	prompt, deferred, err := s.resolvePrompt(baseDir)
	if err != nil {
		return nil, err
	}

	events, err := s.Events.toModel()
	if err != nil {
		return nil, goerr.Wrap(err, "invalid job events",
			goerr.V("job_id", s.ID))
	}

	strategy := model.NormaliseJobStrategy(model.JobStrategy(s.Strategy))
	if !strategy.IsValid() {
		return nil, goerr.New("invalid job strategy",
			goerr.V("job_id", s.ID),
			goerr.V("strategy", s.Strategy))
	}

	if s.LLMModel != "" && !llmModelRefPattern.MatchString(s.LLMModel) {
		return nil, goerr.Wrap(ErrInvalidLLMModelRef,
			"job llm_model must be a model reference name",
			goerr.V("job_id", s.ID), goerr.V("llm_model", s.LLMModel))
	}
	budget, err := budgetFromUSD(s.BudgetUSD)
	if err != nil {
		return nil, goerr.Wrap(err, "invalid job budget", goerr.V("job_id", s.ID))
	}
	execution, err := s.Execution.toModel(s.LLMModel)
	if err != nil {
		return nil, goerr.Wrap(err, "invalid job execution", goerr.V("job_id", s.ID))
	}
	if s.Interactive && execution != nil && execution.AppliesTo(strategy) && execution.Cascade {
		return nil, goerr.New("interactive jobs do not support cascade execution", goerr.V("job_id", s.ID))
	}

	job := &model.Job{
		ID:          s.ID,
		Name:        s.Name,
		Description: s.Description,
		Prompt:      prompt,
		Disabled:    s.Disabled,
		Quiet:       s.Quiet,
		Strategy:    strategy,
		Interactive: s.Interactive,
		Reflection:  s.Reflection,
		LLMModel:    s.LLMModel,
		Execution:   execution,
		Budget:      budget,
		Events:      *events,
	}
	// In structural-validation mode (baseDir == "") the prompt lives in a
	// prompt_file we deliberately did not read, so job.Prompt is empty and
	// the model invariant's prompt-non-empty arm would wrongly fail. Identity,
	// strategy, and events are already validated above; the complete
	// model.Job.Validate (including the prompt) runs once the file is read in
	// parseWorkspaceConfig.
	if deferred {
		return job, nil
	}
	if err := job.Validate(); err != nil {
		return nil, goerr.Wrap(err, "job invariant violation",
			goerr.V("job_id", s.ID))
	}
	// Parse the resolved prompt as a Go template with the runtime's FuncMap so a
	// broken template (unbalanced action, unknown function) fails at config-load
	// / `validate` time instead of silently passing here and only erroring at the
	// first Job run. Deferred (prompt_file, baseDir=="") sections skip this and
	// are re-validated once the file is read with a real baseDir.
	if _, err := jobruntime.ParseUserPromptTemplate(job.Prompt); err != nil {
		return nil, goerr.Wrap(err, "invalid job prompt template",
			goerr.V("job_id", s.ID))
	}
	return job, nil
}

// resolvePrompt determines the effective prompt for the job and enforces the
// prompt / prompt_file exclusivity. baseDir is the config file's directory,
// used to resolve a relative prompt_file path.
//
// baseDir == "" selects structural-validation mode: the caller
// (AppConfig.Validate) only checks that the section is well-formed and is in
// no position to read files. There a prompt_file reference is accepted
// without touching the filesystem and deferred is returned true, signalling
// that the prompt content is intentionally unresolved. The real read happens
// later in parseWorkspaceConfig, and only for a source that carries a BaseDir —
// a configuration document submitted over HTTP deliberately has none.
func (s *JobSection) resolvePrompt(baseDir string) (prompt string, deferred bool, err error) {
	hasInline := s.Prompt != ""
	hasFile := s.PromptFile != ""
	switch {
	case hasInline && hasFile:
		return "", false, goerr.New("job prompt and prompt_file are mutually exclusive",
			goerr.V("job_id", s.ID))
	case !hasInline && !hasFile:
		return "", false, goerr.New("job prompt or prompt_file is required",
			goerr.V("job_id", s.ID))
	case hasInline:
		return s.Prompt, false, nil
	}

	// hasFile: prompt content comes from a file resolved relative to baseDir.
	if baseDir == "" {
		return "", true, nil
	}

	// An absolute prompt_file is honoured as-is; only relative paths resolve
	// against the config file's directory (joining an absolute path with
	// baseDir would mangle it).
	path := s.PromptFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	// #nosec G304 -- prompt_file comes from the operator-supplied config file,
	// the same trust level as the config path itself (CLI argument).
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, goerr.Wrap(err, "failed to read job prompt file",
			goerr.V("job_id", s.ID), goerr.V("prompt_file", s.PromptFile))
	}
	// Trim trailing whitespace so an editor's trailing newline does not alter
	// the rendered prompt relative to an equivalent inline value.
	content := strings.TrimRight(string(data), " \t\r\n")
	if content == "" {
		return "", false, goerr.New("job prompt file is empty",
			goerr.V("job_id", s.ID), goerr.V("prompt_file", s.PromptFile))
	}
	return content, false, nil
}

func (e *JobEventsSection) toModel() (*model.JobEvents, error) {
	if e == nil {
		return nil, goerr.New("events section is nil")
	}
	if e.Case == nil && e.Scheduled == nil {
		return nil, goerr.New("events must subscribe to at least one domain (case or scheduled)")
	}

	out := &model.JobEvents{}

	if e.Case != nil {
		caseCfg, err := e.Case.toModel()
		if err != nil {
			return nil, goerr.Wrap(err, "invalid events.case")
		}
		out.Case = caseCfg
	}
	if e.Scheduled != nil {
		schedCfg, err := e.Scheduled.toModel()
		if err != nil {
			return nil, goerr.Wrap(err, "invalid events.scheduled")
		}
		out.Scheduled = schedCfg
	}
	return out, nil
}

func (c *CaseEventSection) toModel() (*model.CaseEventConfig, error) {
	if c == nil {
		return nil, goerr.New("case event section is nil")
	}
	if len(c.On) == 0 {
		return nil, goerr.New("events.case.on must not be empty")
	}
	seen := make(map[model.CaseLifecycle]struct{}, len(c.On))
	on := make([]model.CaseLifecycle, 0, len(c.On))
	for _, raw := range c.On {
		lc := model.CaseLifecycle(raw)
		if !lc.IsValid() {
			return nil, goerr.New("invalid value in events.case.on",
				goerr.V("value", raw))
		}
		if _, dup := seen[lc]; dup {
			return nil, goerr.New("duplicate value in events.case.on",
				goerr.V("value", raw))
		}
		seen[lc] = struct{}{}
		on = append(on, lc)
	}
	return &model.CaseEventConfig{On: on}, nil
}

func (s *ScheduledEventSection) toModel() (*model.ScheduledEventConfig, error) {
	if s == nil {
		return nil, goerr.New("scheduled event section is nil")
	}
	hasEvery := s.Every != ""
	hasCron := s.Cron != ""
	switch {
	case hasEvery && hasCron:
		return nil, goerr.New("events.scheduled must specify exactly one of every or cron, not both")
	case !hasEvery && !hasCron:
		return nil, goerr.New("events.scheduled must specify either every or cron")
	}

	out := &model.ScheduledEventConfig{}
	if hasEvery {
		d, err := time.ParseDuration(s.Every)
		if err != nil {
			return nil, goerr.Wrap(err, "invalid every duration",
				goerr.V("every", s.Every))
		}
		if d <= 0 {
			return nil, goerr.New("every must be positive",
				goerr.V("every", s.Every))
		}
		out.Every = d
	}
	if hasCron {
		sched, err := cronParser.Parse(s.Cron)
		if err != nil {
			return nil, goerr.Wrap(err, "invalid cron expression",
				goerr.V("cron", s.Cron))
		}
		out.Cron = sched
		out.CronExpr = s.Cron
	}
	return out, nil
}

// resolveJobs walks every [[job]] entry, validating it and collecting the
// resulting model.Jobs. baseDir is the config file's directory, used to
// resolve relative prompt_file paths; pass "" for structural-only validation
// that must not touch the filesystem. Duplicate IDs within the workspace
// surface as a loud failure.
func (a *AppConfig) resolveJobs(baseDir string) ([]*model.Job, error) {
	if len(a.Jobs) == 0 {
		return nil, nil
	}
	jobs := make([]*model.Job, 0, len(a.Jobs))
	seen := make(map[string]struct{}, len(a.Jobs))
	for idx := range a.Jobs {
		section := &a.Jobs[idx]
		job, err := section.Validate(baseDir)
		if err != nil {
			return nil, goerr.Wrap(err, "invalid job",
				goerr.V("job_index", idx))
		}
		if _, dup := seen[job.ID]; dup {
			return nil, goerr.New("duplicate job id within workspace",
				goerr.V("job_id", job.ID))
		}
		seen[job.ID] = struct{}{}
		jobs = append(jobs, job)
	}
	return jobs, nil
}
