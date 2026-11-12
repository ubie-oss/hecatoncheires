package job

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/slackfmt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
)

//go:embed prompts/system.md
var systemPromptTmplSource string

var (
	systemPromptOnce sync.Once
	systemPromptTmpl *template.Template
	systemPromptErr  error
)

// promptFuncs are the template helpers exposed to the system / user
// prompt templates. Kept minimal — prompts are trusted-author surfaces,
// but we still avoid handing them filesystem / network helpers.
var promptFuncs = template.FuncMap{
	"join": strings.Join,
}

func loadSystemPromptTemplate() (*template.Template, error) {
	systemPromptOnce.Do(func() {
		systemPromptTmpl, systemPromptErr = template.
			New("job-system").
			Funcs(promptFuncs).
			Parse(systemPromptTmplSource)
	})
	if systemPromptErr != nil {
		return nil, goerr.Wrap(systemPromptErr, "parse embedded job system prompt")
	}
	return systemPromptTmpl, nil
}

// PromptInputs bundles the runtime context needed to render the system
// prompt and the user prompt for a single Job run.
type PromptInputs struct {
	Job       *model.Job
	Workspace *model.WorkspaceEntry
	Case      *model.Case
	Actions   []*model.Action
	// Memos is the set of ACTIVE memos for the Case (the agent's working
	// memory). The system prompt embeds at most memoSystemPromptMax id+title
	// pairs and, when there are more, the total count; full content is fetched
	// on demand via the memo tools.
	Memos []*model.Memo

	// RecentMessages is the thread's recent Slack messages, oldest first,
	// already bounded by the caller to the last recentMessageWindow and at
	// most recentMessageMaxCount items. It is populated only for thread-mode
	// workspaces (channel-mode Jobs leave it nil); the system prompt renders
	// the section only when the workspace manages no Actions.
	RecentMessages []*slack.Message

	Event Event
	Now   time.Time

	// Sources is the resolved set of *model.Source the agent should be
	// aware of for this Case. It is always populated when source
	// listing succeeds — even when the operator has not narrowed the
	// selection — so the LLM has the full catalogue of investigation
	// sources at hand. The distinction "operator narrowed vs. workspace
	// default" is carried separately on SourcesNarrowed so the system
	// prompt can phrase the section accordingly.
	Sources []*model.Source

	// SourcesNarrowed reports whether the Sources slice reflects an
	// explicit operator selection from the Case Agent page
	// (Case.AgentSourceIDs non-empty). When true the system prompt
	// labels the list as a *preference*; when false it labels the list
	// as the full available catalogue and explicitly states no narrowing
	// is in effect. Either way the agent is never told to restrict its
	// search — selection is a hint, not a filter.
	SourcesNarrowed bool
}

// systemPromptData is the typed dot value passed into the system prompt
// template. All template branches read from this single struct so adding
// a new section is a focused change: add a field, add a `{{ if }}` block.
type systemPromptData struct {
	// Now is the turn's start time (RFC3339, UTC), rendered as the
	// agent's absolute "current time". Empty when PromptInputs.Now is
	// the zero value so the template can omit the section entirely.
	Now       string
	Workspace systemPromptWorkspace
	// BoardStatuses lists the workspace's case board statuses the agent may
	// move the case to via case__update_case_status. Populated only when the
	// workspace defines a case status set (the same condition under which the
	// status tool is wired); empty otherwise so the template omits the section.
	BoardStatuses []systemPromptStatus
	Case          *systemPromptCase
	// ManagesActions is false for thread-mode workspaces, which manage no
	// Actions. When false the template omits the Actions section and the
	// action-specific guardrails so the prompt never references a concept the
	// agent has no tools for.
	ManagesActions bool
	// ReadsActions is true wherever the Job can read the case's Actions: every
	// channel-mode workspace (ManagesActions), and a thread-mode workspace that
	// enables [case] actions, where the run is given the read tools only. The
	// template gates the Actions list on it, and on ManagesActions whatever
	// says the agent may change them.
	ReadsActions bool
	// ThreadMode is true for a thread-mode workspace. It gates the recent
	// thread messages section, which only a thread-bound case has.
	ThreadMode bool
	Actions    []systemPromptAction
	Memo       systemPromptMemoSection
	// RecentMessages is the thread's recent Slack messages, oldest first.
	// Only populated for thread-mode workspaces (ThreadMode);
	// the template gates the whole section on ThreadMode and shows
	// "(none)" when the slice is empty.
	RecentMessages []systemPromptMessage
	Trigger        systemPromptTrigger
	Reason         systemPromptReason
	Sources        systemPromptSourceSection
	// SlackFormat is the shared Slack message formatting section
	// (slackfmt.Section()). A Job reaches Slack only through
	// slack__post_to_case_channel, and it composes that text itself.
	SlackFormat string
	// Language is the English name of the workspace's configured language,
	// empty when the workspace configures none. The template then gives no
	// language instruction, which is how a Job ran before workspaces could
	// fix one.
	Language string
}

// recentMessageTruncateRunes bounds how many runes of each Slack message body
// the system prompt embeds. Bodies longer than this are truncated and their
// full rune count is annotated so the agent knows content was elided. Kept as
// a fixed feature parameter (not configurable) per the spec.
const recentMessageTruncateRunes = 140

// systemPromptMessage is one recent thread message, formatted for the system
// prompt. Text is already truncated to recentMessageTruncateRunes; FullRuneCount
// carries the original rune count only when truncation occurred (0 otherwise),
// so the template can append the "[N chars total]" annotation conditionally.
type systemPromptMessage struct {
	Timestamp     string
	Author        string
	Text          string
	FullRuneCount int
}

// memoSystemPromptMax bounds how many memo id+title pairs are embedded in the
// system prompt. Beyond this the prompt reports only the total count and the
// agent fetches details with the memo tools — this keeps the prompt bounded
// even for cases that accumulate many memories.
const memoSystemPromptMax = 20

// systemPromptMemoSection carries the workspace memo definition, the memo field
// schema, and a bounded preview of the case's active memos.
type systemPromptMemoSection struct {
	Enabled    bool
	Definition string
	Fields     []systemPromptField
	Items      []systemPromptMemoBrief
	TotalCount int
	Overflow   bool
}

type systemPromptMemoBrief struct {
	ID    string
	Title string
}

type systemPromptSourceSection struct {
	Items    []systemPromptSource
	Narrowed bool
}

type systemPromptSource struct {
	ID          string
	Name        string
	Type        string
	Description string
}

type systemPromptWorkspace struct {
	ID          string
	Name        string
	Description string
	Fields      []systemPromptField
}

// systemPromptStatus is one case board status the agent may move the case to
// via case__update_case_status. Closed marks a status configured as closing the
// case, mirrored from the workspace's case status set.
type systemPromptStatus struct {
	ID          string
	Name        string
	Description string
	Closed      bool
}

type systemPromptField struct {
	ID          string
	Name        string
	Type        string
	Required    bool
	Description string
	// Semantic is "<semantic id> — <hint>" for a text field that sets one,
	// empty otherwise.
	Semantic string
	// Pattern is "`<pattern>` — <hint>" for a text field that sets
	// validation.pattern, empty otherwise.
	Pattern string
	Options []systemPromptFieldOption
}

type systemPromptFieldOption struct {
	ID          string
	Name        string
	Description string
	Metadata    []systemPromptKV
}

type systemPromptKV struct {
	Key   string
	Value string
}

// emptyPlaceholder stands in for a value that is genuinely unset. The Job agent
// has no tool to read its case back, so a line omitted because the value is
// empty is indistinguishable to it from a line the prompt never renders; the
// placeholder keeps "no value" and "not shown" apart. Matches the placeholder
// the thread-mode mention prompt uses (pkg/usecase/agent/threadcase/prompt.go).
const emptyPlaceholder = "(empty)"

type systemPromptCase struct {
	ID          int64
	Title       string
	Description string
	// Status is the LIFECYCLE status (DRAFT / OPEN / CLOSED), not the board
	// column. BoardStatus below carries the column.
	Status string
	// BoardStatus is the case's current workflow column id, or emptyPlaceholder
	// when it has none. It is rendered only for a thread-bound case, gated on
	// IsThreadBound rather than on the value: a channel-bound case has no board,
	// so a placeholder there would name a concept that does not apply, while a
	// thread-bound case with no board status is a real and reportable state
	// (checkCaseStatuses in pkg/usecase/validate.go flags it) that must not
	// look like a line the prompt withheld.
	BoardStatus string
	// IsThreadBound gates the board_status line. See BoardStatus.
	IsThreadBound bool
	IsTest        bool
	IsPrivate     bool
	// ArchivedAt is the RFC3339 archive timestamp, or emptyPlaceholder when the
	// case is not archived.
	ArchivedAt  string
	ReporterID  string
	AssigneeIDs []string
	// ChannelUserIDs is the case's channel members, already joined into one
	// comma-separated line, or emptyPlaceholder when there are none.
	ChannelUserIDs        string
	SlackChannelID        string
	SlackThreadTS         string
	CreatedAt             string
	UpdatedAt             string
	FieldValues           []systemPromptFieldValue
	AgentAdditionalPrompt string
}

type systemPromptFieldValue struct {
	ID    string
	Value string
	// Resolved is populated when the field type is select / multi-select
	// and at least one raw element of Value matched a known option ID.
	// select produces at most one entry, multi-select may produce many.
	Resolved []systemPromptFieldValueOption
}

type systemPromptFieldValueOption struct {
	OptionID   string
	OptionName string
}

type systemPromptAction struct {
	ID         int64
	Title      string
	Status     string
	AssigneeID string
}

type systemPromptTrigger struct {
	CaseLifecycles []systemPromptLifecycle
	ScheduledEvery string
	ScheduledCron  string
}

type systemPromptLifecycle struct {
	Name        string
	Description string
}

type systemPromptReason struct {
	CaseCreated bool
	CaseClosed  bool
	// Manual marks a run an operator started from the web UI. Actor then
	// names the user who pressed Run, not the author of a case transition.
	Manual         bool
	ScheduledEvery string
	ScheduledCron  string
	CaseID         int64
	Actor          string
	Timestamp      string
	LastRunAt      string
	ScheduledFor   string
	Elapsed        string
}

// BuildSystemPrompt assembles the structured system prompt the Job agent
// receives. Section content is fixed by the embedded `prompts/system.md`
// template; this function only marshals PromptInputs into the typed
// template data.
func BuildSystemPrompt(in PromptInputs) (string, error) {
	tmpl, err := loadSystemPromptTemplate()
	if err != nil {
		return "", err
	}
	data := buildSystemPromptData(in)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", goerr.Wrap(err, "execute job system prompt template")
	}
	return buf.String(), nil
}

func buildSystemPromptData(in PromptInputs) systemPromptData {
	data := systemPromptData{SlackFormat: slackfmt.Section()}
	if in.Workspace != nil {
		data.Language = i18n.LanguageLabelOf(in.Workspace.Language)
	}

	// A Job writes Actions only in a channel-mode workspace. A thread-mode
	// workspace gives it no action tools unless it enables [case] actions, and
	// then only the read tools (agentkernel.Scope.ThreadModeReads), so the
	// prompt lists the Actions without advertising any action write. A nil
	// workspace defaults to channel-mode (actions on).
	data.ThreadMode = in.Workspace.IsThreadMode()
	data.ManagesActions = !data.ThreadMode
	data.ReadsActions = in.Workspace.ActionsEnabled()

	// Now is the turn's start time (runner.go injects r.clock()); a zero
	// value means the caller did not supply it, so the section is skipped
	// rather than rendering a bogus "0001-01-01" timestamp.
	if !in.Now.IsZero() {
		data.Now = in.Now.UTC().Format(time.RFC3339)
	}

	// fieldMetaByID lets the Case loop below look up field type and
	// option metadata when rendering field_values. It is built once
	// from the workspace schema and shared across both sections so the
	// custom-fields description and the field_values resolution stay
	// in sync.
	fieldMetaByID := map[string]fieldMeta{}

	// fieldOrder is the workspace schema's declaration order. The Case section
	// renders one field_values line per entry here, whether or not the case
	// carries a value for it, so the order is part of the rendered output and
	// not just a lookup key set.
	var fieldOrder []string

	if ws := in.Workspace; ws != nil {
		data.Workspace = systemPromptWorkspace{
			ID:          ws.Workspace.ID,
			Name:        ws.Workspace.Name,
			Description: ws.Workspace.Description,
		}
		if schema := ws.FieldSchema; schema != nil {
			for _, f := range schema.Fields {
				meta := fieldMeta{fieldType: f.Type}
				field := systemPromptField{
					ID:          f.ID,
					Name:        f.Name,
					Type:        string(f.Type),
					Required:    f.Required,
					Description: f.Description,
					Semantic:    semantic.Label(f.Semantic),
					Pattern:     f.Validation.Pattern.Label(),
				}
				if len(f.Options) > 0 {
					meta.options = make(map[string]config.FieldOption, len(f.Options))
				}
				for _, o := range f.Options {
					meta.options[o.ID] = o
					field.Options = append(field.Options, systemPromptFieldOption{
						ID:          o.ID,
						Name:        o.Name,
						Description: o.Description,
						Metadata:    fieldOptionMetadata(o.Metadata),
					})
				}
				fieldMetaByID[f.ID] = meta
				fieldOrder = append(fieldOrder, f.ID)
				data.Workspace.Fields = append(data.Workspace.Fields, field)
			}
		}

		// Board statuses: advertise the case status set so the agent can drive
		// case__update_case_status with valid ids, names, and the reasoning hint
		// each status's description carries. Rendered whenever a status set is
		// defined — the same condition under which casewriter wires the status
		// tool — so the prompt and the tool stay in sync.
		if css := ws.CaseStatusSet; css != nil {
			for _, s := range css.Statuses() {
				data.BoardStatuses = append(data.BoardStatuses, systemPromptStatus{
					ID:          s.ID,
					Name:        s.Name,
					Description: s.Description,
					Closed:      css.IsClosed(s.ID),
				})
			}
		}

		// Memo section: the workspace's strong definition + memo field schema.
		// Rendered only when the workspace enabled memos.
		if ws.MemoConfig.Enabled() {
			data.Memo.Enabled = true
			data.Memo.Definition = ws.MemoConfig.Description
			for _, f := range ws.MemoConfig.FieldSchema.Fields {
				field := systemPromptField{
					ID:          f.ID,
					Name:        f.Name,
					Type:        string(f.Type),
					Required:    f.Required,
					Description: f.Description,
					Semantic:    semantic.Label(f.Semantic),
					Pattern:     f.Validation.Pattern.Label(),
				}
				for _, o := range f.Options {
					field.Options = append(field.Options, systemPromptFieldOption{
						ID:          o.ID,
						Name:        o.Name,
						Description: o.Description,
						Metadata:    fieldOptionMetadata(o.Metadata),
					})
				}
				data.Memo.Fields = append(data.Memo.Fields, field)
			}
		}
	}

	// Memo preview: at most memoSystemPromptMax id+title pairs, plus the total
	// count when there are more (the agent reads the rest via the memo tools).
	if data.Memo.Enabled {
		data.Memo.TotalCount = len(in.Memos)
		for i, m := range in.Memos {
			if m == nil {
				continue
			}
			if i >= memoSystemPromptMax {
				data.Memo.Overflow = true
				break
			}
			data.Memo.Items = append(data.Memo.Items, systemPromptMemoBrief{
				ID:    string(m.ID),
				Title: m.Title,
			})
		}
	}

	// Recent thread messages: thread-mode only. The caller has already
	// bounded the slice to the last recentMessageWindow and recentMessageMaxCount
	// and ordered it oldest-first; here we only format and rune-truncate each
	// body. Channel-mode workspaces never reach this block, so the section is
	// absent from their prompt entirely.
	if data.ThreadMode {
		for _, m := range in.RecentMessages {
			if m == nil {
				continue
			}
			author := m.UserName()
			if author == "" {
				author = m.UserID()
			}
			ts := ""
			if t := m.CreatedAt(); !t.IsZero() {
				ts = t.UTC().Format(time.RFC3339)
			}
			text, fullCount := truncateRunes(m.Text(), recentMessageTruncateRunes)
			data.RecentMessages = append(data.RecentMessages, systemPromptMessage{
				Timestamp:     ts,
				Author:        author,
				Text:          text,
				FullRuneCount: fullCount,
			})
		}
	}

	if c := in.Case; c != nil {
		cs := &systemPromptCase{
			ID:                    c.ID,
			Title:                 c.Title,
			Description:           c.Description,
			Status:                c.Status.String(),
			BoardStatus:           orEmptyPlaceholder(c.BoardStatus),
			IsThreadBound:         c.IsThreadBound(),
			IsTest:                c.IsTest,
			IsPrivate:             c.IsPrivate,
			ArchivedAt:            emptyPlaceholder,
			ReporterID:            c.ReporterID,
			AssigneeIDs:           append([]string(nil), c.AssigneeIDs...),
			ChannelUserIDs:        orEmptyPlaceholder(strings.Join(c.ChannelUserIDs, ", ")),
			SlackChannelID:        c.SlackChannelID,
			SlackThreadTS:         c.SlackThreadTS,
			AgentAdditionalPrompt: c.AgentAdditionalPrompt,
			FieldValues:           caseFieldValues(c.FieldValues, fieldOrder, fieldMetaByID),
		}
		if c.ArchivedAt != nil {
			cs.ArchivedAt = c.ArchivedAt.UTC().Format(time.RFC3339)
		}
		if !c.CreatedAt.IsZero() {
			cs.CreatedAt = c.CreatedAt.UTC().Format(time.RFC3339)
		}
		if !c.UpdatedAt.IsZero() {
			cs.UpdatedAt = c.UpdatedAt.UTC().Format(time.RFC3339)
		}
		data.Case = cs
	}

	// Skip the Actions section entirely for workspaces whose Jobs cannot read
	// Actions (thread-mode without [case] actions); the template gates on
	// ReadsActions too, but leaving the slice empty keeps the rendered prompt
	// honest if that guard ever changes.
	if data.ReadsActions {
		for _, a := range in.Actions {
			if a == nil || a.IsArchived() {
				continue
			}
			data.Actions = append(data.Actions, systemPromptAction{
				ID:         a.ID,
				Title:      a.Title,
				Status:     a.Status.String(),
				AssigneeID: a.AssigneeID,
			})
		}
	}

	data.Sources.Narrowed = in.SourcesNarrowed
	for _, s := range in.Sources {
		if s == nil {
			continue
		}
		data.Sources.Items = append(data.Sources.Items, systemPromptSource{
			ID:          string(s.ID),
			Name:        s.Name,
			Type:        string(s.SourceType),
			Description: s.Description,
		})
	}

	if in.Job != nil {
		if cc := in.Job.Events.Case; cc != nil {
			for _, lc := range cc.On {
				data.Trigger.CaseLifecycles = append(data.Trigger.CaseLifecycles, systemPromptLifecycle{
					Name:        string(lc),
					Description: describeCaseLifecycle(lc),
				})
			}
		}
		if sc := in.Job.Events.Scheduled; sc != nil {
			switch {
			case sc.Every > 0:
				data.Trigger.ScheduledEvery = sc.Every.String()
			case sc.Cron != nil:
				data.Trigger.ScheduledCron = sc.CronExpr
			}
		}
	}

	switch in.Event.Domain {
	case model.JobEventDomainCase:
		actor := in.Event.ActorUserID
		if actor == "" {
			actor = "(unknown)"
		}
		data.Reason.Actor = actor
		data.Reason.CaseID = in.Event.CaseID
		data.Reason.Timestamp = in.Event.Timestamp.UTC().Format(time.RFC3339)
		switch in.Event.CaseLifecycle {
		case model.CaseLifecycleCreated:
			data.Reason.CaseCreated = true
		case model.CaseLifecycleClosed:
			data.Reason.CaseClosed = true
		}
	case model.JobEventDomainManual:
		actor := in.Event.ActorUserID
		if actor == "" {
			actor = "(unknown)"
		}
		data.Reason.Manual = true
		data.Reason.Actor = actor
		data.Reason.CaseID = in.Event.CaseID
		data.Reason.Timestamp = in.Event.Timestamp.UTC().Format(time.RFC3339)
	case model.JobEventDomainScheduled:
		data.Reason.Timestamp = in.Event.Timestamp.UTC().Format(time.RFC3339)
		data.Reason.LastRunAt = "(never)"
		if !in.Event.LastRunAt.IsZero() {
			data.Reason.LastRunAt = in.Event.LastRunAt.UTC().Format(time.RFC3339)
		}
		if in.Job != nil && in.Job.Events.Scheduled != nil {
			sc := in.Job.Events.Scheduled
			switch {
			case sc.Every > 0:
				data.Reason.ScheduledEvery = sc.Every.String()
				if !in.Event.LastRunAt.IsZero() {
					data.Reason.Elapsed = in.Event.Timestamp.Sub(in.Event.LastRunAt).String()
				} else {
					data.Reason.Elapsed = "?"
				}
			case sc.Cron != nil:
				data.Reason.ScheduledCron = sc.CronExpr
				if !in.Event.ScheduledFor.IsZero() {
					data.Reason.ScheduledFor = in.Event.ScheduledFor.UTC().Format(time.RFC3339)
				} else {
					data.Reason.ScheduledFor = "(none)"
				}
			}
		}
	}

	return data
}

// orEmptyPlaceholder substitutes emptyPlaceholder for an empty string so an
// always-rendered line never reads as a blank value.
func orEmptyPlaceholder(s string) string {
	if s == "" {
		return emptyPlaceholder
	}
	return s
}

// caseFieldValues renders one line per field the workspace schema declares, in
// schema order, followed by any stored value whose field id the schema no
// longer declares (left over from a configuration change), sorted by id so the
// prompt is stable across runs.
//
// A field with no stored value is rendered as emptyPlaceholder rather than
// skipped. Walking the stored map instead — which is what this used to do —
// emits no line at all for an unset field, and the agent has no tool to read
// the case back, so it cannot tell an unset field from one the prompt withheld.
func caseFieldValues(stored map[string]model.FieldValue, order []string, metaByID map[string]fieldMeta) []systemPromptFieldValue {
	out := make([]systemPromptFieldValue, 0, len(order)+len(stored))
	declared := make(map[string]struct{}, len(order))
	for _, id := range order {
		declared[id] = struct{}{}
		out = append(out, caseFieldValue(id, stored, metaByID))
	}

	orphans := make([]string, 0, len(stored))
	for id := range stored {
		if _, ok := declared[id]; !ok {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)
	for _, id := range orphans {
		out = append(out, caseFieldValue(id, stored, metaByID))
	}
	return out
}

func caseFieldValue(id string, stored map[string]model.FieldValue, metaByID map[string]fieldMeta) systemPromptFieldValue {
	v, ok := stored[id]
	if !ok {
		return systemPromptFieldValue{ID: id, Value: emptyPlaceholder}
	}
	meta := metaByID[id]
	return systemPromptFieldValue{
		ID:       id,
		Value:    orEmptyPlaceholder(formatFieldValue(meta.fieldType, v.Value)),
		Resolved: resolveFieldValueOptions(meta, v.Value),
	}
}

// fieldMeta is the small lookup shape buildSystemPromptData uses to
// bridge the workspace FieldSchema and the Case.FieldValues loop. It
// holds the field's declared type and an option index keyed by option
// ID. Kept package-private — internal scaffolding for prompt rendering.
type fieldMeta struct {
	fieldType types.FieldType
	options   map[string]config.FieldOption
}

// fieldOptionMetadata flattens the option's freeform metadata map into
// a stable, sorted slice of key/value pairs so the rendered prompt is
// deterministic across runs. Empty / nil input returns nil so the
// template's {{ if .Metadata }} guard short-circuits cleanly.
func fieldOptionMetadata(meta map[string]any) []systemPromptKV {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]systemPromptKV, 0, len(keys))
	for _, k := range keys {
		out = append(out, systemPromptKV{Key: k, Value: fmt.Sprint(meta[k])})
	}
	return out
}

// formatFieldValue renders the raw field value as a single human-
// readable string for the prompt. multi-select inputs (whether
// `[]string` or `[]any` carrying strings) are joined with ", " so the
// rendered line stays compact; everything else falls back to fmt.Sprint
// which already matches the previous behaviour.
func formatFieldValue(ft types.FieldType, raw any) string {
	if ft == types.FieldTypeMultiSelect {
		if parts, ok := multiSelectIDs(raw); ok {
			return strings.Join(parts, ", ")
		}
	}
	return fmt.Sprint(raw)
}

// resolveFieldValueOptions returns the option metadata for every
// element of the raw value that matched a known option ID. For
// non-select fields (or when no option matches) it returns nil so the
// template's {{ if .Resolved }} guard skips the parenthetical.
func resolveFieldValueOptions(meta fieldMeta, raw any) []systemPromptFieldValueOption {
	switch meta.fieldType {
	case types.FieldTypeSelect:
		s, ok := raw.(string)
		if !ok {
			return nil
		}
		opt, ok := meta.options[s]
		if !ok {
			return nil
		}
		return []systemPromptFieldValueOption{{OptionID: opt.ID, OptionName: opt.Name}}
	case types.FieldTypeMultiSelect:
		ids, ok := multiSelectIDs(raw)
		if !ok {
			return nil
		}
		var out []systemPromptFieldValueOption
		for _, id := range ids {
			opt, ok := meta.options[id]
			if !ok {
				continue
			}
			out = append(out, systemPromptFieldValueOption{OptionID: opt.ID, OptionName: opt.Name})
		}
		return out
	default:
		return nil
	}
}

// multiSelectIDs normalises the two on-the-wire shapes for a multi-
// select value (`[]string` from Go callers, `[]any` from Firestore
// JSON decoding) into a single `[]string`. The bool return signals
// whether the input matched a recognised multi-select shape so the
// caller can fall back to the generic fmt.Sprint path.
func multiSelectIDs(raw any) ([]string, bool) {
	switch v := raw.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}

// truncateRunes caps s at max runes (not bytes — the spec counts 文字), so a
// multi-byte body is never split mid-character the way a byte cap would. It
// returns the truncated string and, when truncation occurred, the original
// rune count so the caller can annotate the elision; when s already fits, the
// second return is 0 to signal "no annotation needed".
//
// Single pass: ranging over a string yields rune-boundary byte indices, so we
// record the cut point at the (max+1)-th rune and keep counting to the end —
// the full rune count falls out of the same loop, avoiding a separate
// utf8.RuneCountInString scan.
func truncateRunes(s string, max int) (string, int) {
	if max <= 0 {
		return "", 0
	}
	cut := -1
	count := 0
	for i := range s {
		if count == max {
			cut = i
		}
		count++
	}
	if cut < 0 {
		return s, 0
	}
	return s[:cut], count
}

func describeCaseLifecycle(lc model.CaseLifecycle) string {
	switch lc {
	case model.CaseLifecycleCreated:
		return "a new case is created"
	case model.CaseLifecycleClosed:
		return "the case status transitions to CLOSED"
	default:
		return string(lc)
	}
}

// userPromptCache memoises compiled user-prompt templates keyed on the prompt
// SOURCE, never on the Job ID. A Job ID is unique only *within* a workspace
// (`resolveJobs` in pkg/cli/config rejects duplicates per workspace), and
// placing the same standard Job — `assign_on_create`, `daily_progress_check` —
// in several workspaces is normal usage. This cache is process-global, so
// keying it on the ID handed whichever workspace was parsed first its compiled
// template to every same-named Job in the process, and the LLM received another
// workspace's user prompt. Keying on the source makes that mismatch structurally
// impossible: a compiled template is a pure function of its source text.
//
// Memory is bounded by the number of distinct prompts in the configuration, not
// by run count, and every prompt string is already retained by its *model.Job.
var userPromptCache sync.Map // key: prompt source string, value: *template.Template

// RenderUserPrompt renders the Job's `prompt` field as a Go template
// using PromptInputs as the dot value. Templates are cached per prompt
// source to avoid re-parsing on every invocation.
func RenderUserPrompt(in PromptInputs) (string, error) {
	if in.Job == nil {
		return "", goerr.New("job is nil")
	}
	tmpl, err := userTemplateFor(in.Job)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, in); err != nil {
		return "", goerr.Wrap(err, "render job prompt template",
			goerr.V("job_id", in.Job.ID))
	}
	return buf.String(), nil
}

func userTemplateFor(j *model.Job) (*template.Template, error) {
	if cached, ok := userPromptCache.Load(j.Prompt); ok {
		return cached.(*template.Template), nil
	}
	// The template name is deliberately job-independent: the cache entry is
	// shared by every Job whose prompt has this exact source, so embedding one
	// Job's ID in the name would misattribute another Job's execute error. The
	// ID reaches the caller through goerr.V instead.
	tmpl, err := template.New("job-user").Funcs(promptFuncs).Parse(j.Prompt)
	if err != nil {
		return nil, goerr.Wrap(err, "parse job prompt template",
			goerr.V("job_id", j.ID))
	}
	userPromptCache.Store(j.Prompt, tmpl)
	return tmpl, nil
}

// ParseUserPromptTemplate parses src as a Job user-prompt template using the
// exact same FuncMap the runtime renders with (promptFuncs) and returns the
// compiled template. Config load and the `validate` command call it (discarding
// the template) so a broken template — an unbalanced action, an unknown
// function — is caught up-front instead of only at the first Job run. It
// intentionally shares promptFuncs with userTemplateFor so validation and
// rendering can never accept a different template dialect.
//
// This is parse-only: it proves the template compiles, not that a particular
// dot value resolves — the latter needs a live Case/Workspace and is exercised
// at render time (RenderUserPrompt).
func ParseUserPromptTemplate(src string) (*template.Template, error) {
	tmpl, err := template.New("job-user-validate").Funcs(promptFuncs).Parse(src)
	if err != nil {
		return nil, goerr.Wrap(err, "parse job prompt template")
	}
	return tmpl, nil
}
