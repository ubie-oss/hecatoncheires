package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
)

// actionToMap converts an Action to a map for tool response
func actionToMap(a *model.Action) map[string]any {
	m := map[string]any{
		"id":          a.ID,
		"case_id":     a.CaseID,
		"title":       a.Title,
		"description": a.Description,
		"status":      a.Status.String(),
		"assignee_id": a.AssigneeID,
		"archived":    a.IsArchived(),
		"created_at":  a.CreatedAt.String(),
		"updated_at":  a.UpdatedAt.String(),
	}
	if a.DueDate != nil {
		m["due_date"] = a.DueDate.Format(time.RFC3339)
	}
	if a.ArchivedAt != nil {
		m["archived_at"] = a.ArchivedAt.Format(time.RFC3339)
	}
	return m
}

// listActionsTool retrieves all actions for the current case
type listActionsTool struct {
	repo        interfaces.Repository
	workspaceID string
	caseID      int64
}

func (t *listActionsTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__list_actions",
		Description: "List all actions associated with the current case. By default archived actions are excluded; pass include_archived=true to include them.",
		Parameters: map[string]*gollem.Parameter{
			"include_archived": {
				Type:        gollem.TypeBoolean,
				Description: "When true, include archived actions in the result. Default false.",
			},
		},
	}
}

func (t *listActionsTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	tool.Update(ctx, "Listing actions...")
	scope := interfaces.ActionArchiveScopeActiveOnly
	if v, ok := args["include_archived"]; ok {
		if b, ok := v.(bool); ok && b {
			scope = interfaces.ActionArchiveScopeAll
		}
	}
	actions, err := t.repo.Action().GetByCase(ctx, t.workspaceID, t.caseID, interfaces.ActionListOptions{ArchiveScope: scope})
	if err != nil {
		return nil, goerr.Wrap(err, "failed to list actions",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("caseID", t.caseID),
		)
	}

	items := make([]map[string]any, len(actions))
	for i, a := range actions {
		item := map[string]any{
			"id":          a.ID,
			"title":       a.Title,
			"status":      a.Status.String(),
			"assignee_id": a.AssigneeID,
			"archived":    a.IsArchived(),
		}
		if a.DueDate != nil {
			item["due_date"] = a.DueDate.Format(time.RFC3339)
		}
		if a.ArchivedAt != nil {
			item["archived_at"] = a.ArchivedAt.Format(time.RFC3339)
		}
		items[i] = item
	}
	return map[string]any{"actions": items}, nil
}

// getActionTool retrieves action details by ID, within the case the run is
// pinned to.
type getActionTool struct {
	repo        interfaces.Repository
	workspaceID string
	// caseID is the case this run may read. An Action belonging to any other
	// case is reported as not found: an action id is a small integer an LLM can
	// arrive at by guessing or by being told to in injected text, and without
	// this check a run on one case could read another case's work — including a
	// private case the actor is not a member of, because the read goes straight
	// to the repository and never passes an access gate.
	//
	// Zero means "no case pinned", which only a run with no case at all has; such
	// a run gets no core toolset in the first place, so the tool never exists.
	caseID int64
}

func (t *getActionTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__get_action",
		Description: "Get details of a specific action by its ID",
		Parameters: map[string]*gollem.Parameter{
			"action_id": {
				Type:        gollem.TypeInteger,
				Description: "The ID of the action to retrieve",
				Required:    true,
			},
		},
	}
}

func (t *getActionTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	actionID, err := tool.ExtractInt64(args, "action_id")
	if err != nil {
		return nil, err
	}

	tool.Update(ctx, fmt.Sprintf("Getting action #%d...", actionID))
	a, err := t.repo.Action().Get(ctx, t.workspaceID, actionID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to get action",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("actionID", actionID),
		)
	}
	if a == nil {
		return nil, fmt.Errorf("action not found: id=%d", actionID)
	}
	// Same message as a genuinely missing action, deliberately: telling the model
	// that the id exists but belongs elsewhere would confirm the existence of a
	// case it may not read.
	if t.caseID != 0 && a.CaseID != t.caseID {
		return nil, fmt.Errorf("action not found: id=%d", actionID)
	}
	return actionToMap(a), nil
}

// statusParamDescription appends the workspace's action statuses — id, name,
// whether it closes the action, and the operator-written description — to a
// status parameter's description. The parameter's enum carries the ids alone,
// and a status set that encodes meaning in its ids (several kinds of action
// told apart by id prefix, say) cannot be used correctly from those.
func statusParamDescription(lead string, set *model.ActionStatusSet) string {
	var b strings.Builder
	b.WriteString(lead)
	b.WriteString(" Available statuses:")
	for _, d := range set.Statuses() {
		fmt.Fprintf(&b, "\n- `%s` (%s", d.ID, d.Name)
		if set.IsClosed(d.ID) {
			b.WriteString(", closed")
		}
		b.WriteString(")")
		if desc := strings.TrimSpace(d.Description); desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
	}
	return b.String()
}

// createActionTool creates a new action through the unified ActionUseCase
// entry point. Routing through the usecase (instead of poking the repository
// directly) is what triggers the Slack channel post, ActionEvent recording,
// and any future side-effects that CreateAction owns.
type createActionTool struct {
	actionUC    ActionMutator
	workspaceID string
	caseID      int64
	statusSet   *model.ActionStatusSet
}

func (t *createActionTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__create_action",
		Description: "Create a new action associated with the current case",
		Parameters: map[string]*gollem.Parameter{
			"title": {
				Type:        gollem.TypeString,
				Description: "Title of the action",
				Required:    true,
			},
			"description": {
				Type:        gollem.TypeString,
				Description: "Detailed description of the action",
				Required:    false,
			},
			"assignee_id": {
				Type:        gollem.TypeString,
				Description: "Slack user ID to assign to this action (omit or empty for unassigned)",
				Required:    false,
			},
			"status": {
				Type:        gollem.TypeString,
				Description: statusParamDescription(fmt.Sprintf("Initial status of the action (default: %s).", t.statusSet.InitialID()), t.statusSet),
				Required:    false,
				Enum:        t.statusSet.IDs(),
			},
			"due_date": {
				Type:        gollem.TypeString,
				Description: "Optional due date for the action in RFC3339 format (e.g. 2025-03-01T00:00:00Z)",
				Required:    false,
			},
		},
	}
}

func (t *createActionTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	title, _ := args["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}

	if t.actionUC == nil {
		return nil, goerr.New("create_action tool is not wired to an ActionUseCase",
			goerr.V("workspaceID", t.workspaceID))
	}

	description, _ := args["description"].(string)

	status := types.ActionStatus(t.statusSet.InitialID())
	if s, ok := args["status"].(string); ok && s != "" {
		if !t.statusSet.IsValid(s) {
			return nil, fmt.Errorf("invalid status %q: must be one of %v", s, t.statusSet.IDs())
		}
		status = types.ActionStatus(s)
	}

	var assigneeID string
	if v, ok := args["assignee_id"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("invalid assignee_id: expected string, got %T", v)
		}
		assigneeID = s
	}

	var dueDate *time.Time
	if dueDateStr, ok := args["due_date"].(string); ok && dueDateStr != "" {
		parsed, err := time.Parse(time.RFC3339, dueDateStr)
		if err != nil {
			return nil, fmt.Errorf("invalid due_date format %q: expected RFC3339 (e.g. 2025-03-01T00:00:00Z)", dueDateStr)
		}
		dueDate = &parsed
	}

	tool.Update(ctx, fmt.Sprintf("Creating action: %s", title))

	// Route through the unified usecase entry point so that Slack posting,
	// ActionEvent records, and any future side-effects fire identically to
	// the GraphQL and Slack-modal create paths. Initial SlackMessageTS is
	// empty; CreateAction itself fills it in once the channel post returns.
	created, err := t.actionUC.CreateAction(ctx, t.workspaceID, t.caseID, title, description, assigneeID, "", status, dueDate)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to create action",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("caseID", t.caseID),
		)
	}
	return actionToMap(created), nil
}

// updateActionTool updates the title, description, and/or assignee of an
// action through the unified ActionUseCase entry point. Routing through
// the usecase (instead of poking the repository directly) is what triggers
// the Slack message refresh, ActionEvent recording, and access checks.
type updateActionTool struct {
	actionUC    ActionMutator
	workspaceID string
}

func (t *updateActionTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__update_action",
		Description: "Update the title, description, and/or assignee of an existing action",
		Parameters: map[string]*gollem.Parameter{
			"action_id": {
				Type:        gollem.TypeInteger,
				Description: "The ID of the action to update",
				Required:    true,
			},
			"title": {
				Type:        gollem.TypeString,
				Description: "New title for the action (omit to keep current value)",
				Required:    false,
			},
			"description": {
				Type:        gollem.TypeString,
				Description: "New description for the action (omit or empty string to keep current value)",
				Required:    false,
			},
			"assignee_id": {
				Type:        gollem.TypeString,
				Description: "New Slack user ID to assign (replaces existing). Set to empty string to clear the assignee. Omit to keep current value.",
				Required:    false,
			},
			"due_date": {
				Type:        gollem.TypeString,
				Description: "New due date in RFC3339 format (e.g. 2025-03-01T00:00:00Z). Set to empty string to clear the due date. Omit to keep current value.",
				Required:    false,
			},
		},
	}
}

func (t *updateActionTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	actionID, err := tool.ExtractInt64(args, "action_id")
	if err != nil {
		return nil, err
	}

	if t.actionUC == nil {
		return nil, goerr.New("update_action tool is not wired to an ActionUseCase",
			goerr.V("workspaceID", t.workspaceID))
	}

	var params UpdateActionParams

	// title: tool contract says omit-or-empty means "no change". Only a
	// non-empty value is forwarded as a real edit.
	if title, ok := args["title"].(string); ok && title != "" {
		params.Title = &title
	}
	// description: same rule as title.
	if desc, ok := args["description"].(string); ok && desc != "" {
		params.Description = &desc
	}
	// assignee_id: present-but-empty is the explicit clear signal; a
	// non-empty value reassigns. Absent key means no change.
	if v, ok := args["assignee_id"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("invalid assignee_id: expected string, got %T", v)
		}
		if s == "" {
			params.ClearAssignee = true
		} else {
			params.AssigneeID = &s
		}
	}
	// due_date: present-but-empty clears, RFC3339 sets, absent leaves alone.
	if dueDateStr, ok := args["due_date"].(string); ok {
		if dueDateStr == "" {
			params.ClearDueDate = true
		} else {
			parsed, err := time.Parse(time.RFC3339, dueDateStr)
			if err != nil {
				return nil, fmt.Errorf("invalid due_date format %q: expected RFC3339 (e.g. 2025-03-01T00:00:00Z)", dueDateStr)
			}
			params.DueDate = &parsed
		}
	}

	tool.Update(ctx, fmt.Sprintf("Updating action #%d...", actionID))
	updated, err := t.actionUC.UpdateAction(ctx, t.workspaceID, actionID, params)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update action",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("actionID", actionID),
		)
	}
	return actionToMap(updated), nil
}

// updateActionStatusTool updates the status of an action through the
// unified ActionUseCase entry point.
type updateActionStatusTool struct {
	actionUC    ActionMutator
	workspaceID string
	statusSet   *model.ActionStatusSet
}

func (t *updateActionStatusTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__update_action_status",
		Description: "Update the status of an existing action",
		Parameters: map[string]*gollem.Parameter{
			"action_id": {
				Type:        gollem.TypeInteger,
				Description: "The ID of the action to update",
				Required:    true,
			},
			"status": {
				Type:        gollem.TypeString,
				Description: statusParamDescription("New status for the action.", t.statusSet),
				Required:    true,
				Enum:        t.statusSet.IDs(),
			},
		},
	}
}

func (t *updateActionStatusTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	actionID, err := tool.ExtractInt64(args, "action_id")
	if err != nil {
		return nil, err
	}

	statusStr, _ := args["status"].(string)
	if !t.statusSet.IsValid(statusStr) {
		return nil, fmt.Errorf("invalid status %q: must be one of %v", statusStr, t.statusSet.IDs())
	}
	status := types.ActionStatus(statusStr)

	if t.actionUC == nil {
		return nil, goerr.New("update_action_status tool is not wired to an ActionUseCase",
			goerr.V("workspaceID", t.workspaceID))
	}

	tool.Update(ctx, fmt.Sprintf("Updating action #%d status -> %s", actionID, status))
	updated, err := t.actionUC.UpdateAction(ctx, t.workspaceID, actionID, UpdateActionParams{
		Status: &status,
	})
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update action status",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("actionID", actionID),
		)
	}
	return actionToMap(updated), nil
}

// setActionAssigneeTool sets (or clears) the assignee of an action through
// the unified ActionUseCase entry point.
type setActionAssigneeTool struct {
	actionUC    ActionMutator
	workspaceID string
}

func (t *setActionAssigneeTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__set_action_assignee",
		Description: "Set or clear the assignee of an action. Pass an empty string to clear.",
		Parameters: map[string]*gollem.Parameter{
			"action_id": {
				Type:        gollem.TypeInteger,
				Description: "The ID of the action",
				Required:    true,
			},
			"assignee_id": {
				Type:        gollem.TypeString,
				Description: "Slack user ID of the assignee. Empty string clears the assignee.",
				Required:    true,
			},
		},
	}
}

func (t *setActionAssigneeTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	actionID, err := tool.ExtractInt64(args, "action_id")
	if err != nil {
		return nil, err
	}

	v, ok := args["assignee_id"]
	if !ok {
		return nil, fmt.Errorf("assignee_id is required")
	}
	assigneeID, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("assignee_id must be a string, got %T", v)
	}

	if t.actionUC == nil {
		return nil, goerr.New("set_action_assignee tool is not wired to an ActionUseCase",
			goerr.V("workspaceID", t.workspaceID))
	}

	var params UpdateActionParams
	if assigneeID == "" {
		params.ClearAssignee = true
		tool.Update(ctx, fmt.Sprintf("Clearing assignee on action #%d", actionID))
	} else {
		params.AssigneeID = &assigneeID
		tool.Update(ctx, fmt.Sprintf("Setting assignee %s on action #%d", assigneeID, actionID))
	}

	updated, err := t.actionUC.UpdateAction(ctx, t.workspaceID, actionID, params)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to update action assignee",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("actionID", actionID),
		)
	}
	return actionToMap(updated), nil
}

// archiveActionTool archives an action through the unified ActionUseCase entry
// point. Archived actions disappear from default Kanban / Case detail views
// but remain in storage so they can be unarchived later.
type archiveActionTool struct {
	actionUC    ActionMutator
	workspaceID string
}

func (t *archiveActionTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__archive_action",
		Description: "Archive an action so it disappears from active views. Archived actions can be unarchived later via core__unarchive_action.",
		Parameters: map[string]*gollem.Parameter{
			"action_id": {
				Type:        gollem.TypeInteger,
				Description: "The ID of the action to archive",
				Required:    true,
			},
		},
	}
}

func (t *archiveActionTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	actionID, err := tool.ExtractInt64(args, "action_id")
	if err != nil {
		return nil, err
	}

	if t.actionUC == nil {
		return nil, goerr.New("archive_action tool is not wired to an ActionUseCase",
			goerr.V("workspaceID", t.workspaceID))
	}

	tool.Update(ctx, fmt.Sprintf("Archiving action #%d", actionID))
	updated, err := t.actionUC.ArchiveAction(ctx, t.workspaceID, actionID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to archive action",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("actionID", actionID),
		)
	}
	return actionToMap(updated), nil
}

// unarchiveActionTool restores a previously archived action through the
// unified ActionUseCase entry point.
type unarchiveActionTool struct {
	actionUC    ActionMutator
	workspaceID string
}

func (t *unarchiveActionTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "core__unarchive_action",
		Description: "Restore a previously archived action back to active state.",
		Parameters: map[string]*gollem.Parameter{
			"action_id": {
				Type:        gollem.TypeInteger,
				Description: "The ID of the action to unarchive",
				Required:    true,
			},
		},
	}
}

func (t *unarchiveActionTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	actionID, err := tool.ExtractInt64(args, "action_id")
	if err != nil {
		return nil, err
	}

	if t.actionUC == nil {
		return nil, goerr.New("unarchive_action tool is not wired to an ActionUseCase",
			goerr.V("workspaceID", t.workspaceID))
	}

	tool.Update(ctx, fmt.Sprintf("Unarchiving action #%d", actionID))
	updated, err := t.actionUC.UnarchiveAction(ctx, t.workspaceID, actionID)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to unarchive action",
			goerr.V("workspaceID", t.workspaceID),
			goerr.V("actionID", actionID),
		)
	}
	return actionToMap(updated), nil
}
