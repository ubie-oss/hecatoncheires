package casebound

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/slackfmt"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
)

//go:embed prompts/system.md
var systemPromptTmpl string

var systemPromptTemplate = template.Must(template.New("casebound_system").Parse(systemPromptTmpl))

// promptField represents a case field for template rendering.
type promptField struct {
	Name  string
	Value any
}

// promptMessage represents a conversation message for template rendering.
type promptMessage struct {
	Timestamp   string
	DisplayName string
	Text        string
}

// promptAction represents a single action in the case-wide action list
// (rendered when the agent is NOT in an action-bound thread).
type promptAction struct {
	ID    int64
	Title string
}

// promptCurrentAction represents the action that the current Slack thread
// is bound to (when Session.ActionID != 0). The full set of fields is
// inlined so the LLM can answer questions about it without a tool call.
type promptCurrentAction struct {
	ID          int64
	Title       string
	Status      string
	StatusEmoji string
	Assignee    string
	Description string
	DueDate     string
}

// promptFieldOption is one selectable option of a select / multi-select field.
type promptFieldOption struct {
	ID          string
	Name        string
	Description string
}

// promptFieldDef describes a custom field the agent may set via
// case__update_case. Surfaced so the LLM knows the valid field ids, types, and
// option ids before it tries to write them.
type promptFieldDef struct {
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
	Options []promptFieldOption
}

// promptStatus is one board status id the agent may move the case to via
// case__update_case_status.
type promptStatus struct {
	ID          string
	Name        string
	Description string
	Closed      bool
}

// promptData holds all data for the casebound system prompt template.
type promptData struct {
	ChannelID string
	// ThreadTS is the thread the agent was mentioned in. It is in the prompt
	// because the agent holds slack__get_messages, whose targets take a
	// (channel_id, ts) pair: without the ts it has to invent one, and the tool
	// rejects the call or looks up a message that does not exist.
	ThreadTS    string
	Now         string
	Case        *model.Case
	Fields      []promptField
	FieldSchema []promptFieldDef
	// MemoDefinition and MemoFieldSchema describe the workspace's memos. The
	// memo__* tools reach this agent whenever memos are enabled, and their
	// `fields` parameter points here for the field ids and value shapes.
	MemoDefinition  string
	MemoFieldSchema []promptFieldDef
	BoardStatuses   []promptStatus
	CurrentAction   *promptCurrentAction
	Actions         []promptAction
	Messages        []promptMessage
	// SlackFormat is the shared Slack message formatting section
	// (slackfmt.Section()). This agent's answer is posted to the thread verbatim.
	SlackFormat string
	// Language is the English name of the workspace's configured language.
	// Empty when the workspace configures none, in which case the agent
	// answers in the language of the user's message.
	Language string
	// WorkspaceDescription is [workspace] description: the workspace's own
	// account of what a case is and how work in it is organized. Empty when
	// unset.
	WorkspaceDescription string
	// MentionPrompt is [case.prompts].mention, rendered last as the
	// workspace-specific instructions so the operator's text has the final
	// word. Empty when unset.
	MentionPrompt string
}

// buildSystemPrompt renders the casebound system prompt.
//
// When currentAction is non-nil, the agent is responding inside a Slack
// thread bound to that action. In that mode the case-wide actions list is
// suppressed — only the current action's detail is surfaced, to avoid
// drowning the LLM in unrelated work items. Otherwise the case-wide
// actions list is rendered as a title-only summary.
func buildSystemPrompt(c *model.Case, entry *model.WorkspaceEntry, channelID, threadTS string, now time.Time, currentAction *model.Action, actions []*model.Action, messages []ConversationMessage) string {
	data := promptData{
		ChannelID:   channelID,
		ThreadTS:    threadTS,
		Now:         now.UTC().Format(time.RFC3339),
		Case:        c,
		SlackFormat: slackfmt.Section(),
	}
	if entry != nil {
		data.Language = i18n.LanguageLabelOf(entry.Language)
		data.WorkspaceDescription = strings.TrimSpace(entry.Workspace.Description)
		data.MentionPrompt = strings.TrimSpace(entry.CaseMentionPrompt)
	}

	if entry != nil && entry.FieldSchema != nil && len(c.FieldValues) > 0 {
		fieldNames := make(map[string]string)
		for _, fd := range entry.FieldSchema.Fields {
			fieldNames[fd.ID] = fd.Name
		}
		for fieldID, fv := range c.FieldValues {
			name := fieldNames[fieldID]
			if name == "" {
				name = fieldID
			}
			data.Fields = append(data.Fields, promptField{Name: name, Value: fv.Value})
		}
	}

	// Advertise the editable field schema and board statuses so the agent can
	// drive case__update_case / case__update_case_status with valid ids.
	if entry != nil && entry.FieldSchema != nil {
		data.FieldSchema = promptFieldDefs(entry.FieldSchema.Fields)
	}
	if entry != nil && entry.MemoConfig.Enabled() {
		data.MemoDefinition = entry.MemoConfig.Description
		data.MemoFieldSchema = promptFieldDefs(entry.MemoConfig.FieldSchema.Fields)
	}
	if entry != nil && entry.CaseStatusSet != nil {
		for _, s := range entry.CaseStatusSet.Statuses() {
			data.BoardStatuses = append(data.BoardStatuses, promptStatus{
				ID:          s.ID,
				Name:        s.Name,
				Description: s.Description,
				Closed:      entry.CaseStatusSet.IsClosed(s.ID),
			})
		}
	}

	if currentAction != nil {
		statusSet := model.DefaultActionStatusSet()
		if entry != nil && entry.ActionStatusSet != nil {
			statusSet = entry.ActionStatusSet
		}
		due := ""
		if currentAction.DueDate != nil {
			due = currentAction.DueDate.UTC().Format(time.RFC3339)
		}
		data.CurrentAction = &promptCurrentAction{
			ID:          currentAction.ID,
			Title:       currentAction.Title,
			Status:      currentAction.Status.String(),
			StatusEmoji: statusSet.Emoji(string(currentAction.Status)),
			Assignee:    currentAction.AssigneeID,
			Description: currentAction.Description,
			DueDate:     due,
		}
	} else {
		for _, a := range actions {
			data.Actions = append(data.Actions, promptAction{
				ID:    a.ID,
				Title: a.Title,
			})
		}
	}

	for _, msg := range messages {
		displayName := msg.UserName
		if displayName == "" {
			displayName = msg.UserID
		}
		data.Messages = append(data.Messages, promptMessage{
			Timestamp:   msg.Timestamp,
			DisplayName: displayName,
			Text:        msg.Text,
		})
	}

	var buf bytes.Buffer
	if err := systemPromptTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("You are an AI assistant. Case: %s", c.Title)
	}
	return buf.String()
}

// promptFieldDefs lists field definitions for the prompt, with the ids, types,
// option ids, semantic and pattern the agent needs to write a valid value.
func promptFieldDefs(fields []config.FieldDefinition) []promptFieldDef {
	defs := make([]promptFieldDef, 0, len(fields))
	for _, fd := range fields {
		def := promptFieldDef{
			ID:          fd.ID,
			Name:        fd.Name,
			Type:        string(fd.Type),
			Required:    fd.Required,
			Description: fd.Description,
			Semantic:    semantic.Label(fd.Semantic),
			Pattern:     fd.Validation.Pattern.Label(),
		}
		for _, o := range fd.Options {
			def.Options = append(def.Options, promptFieldOption{ID: o.ID, Name: o.Name, Description: o.Description})
		}
		defs = append(defs, def)
	}
	return defs
}
