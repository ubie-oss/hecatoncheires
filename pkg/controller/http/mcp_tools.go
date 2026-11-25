package http

import (
	"context"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires the read-only MCP tool surface. All tools enforce the
// project rule that private Cases — and the Actions beneath them — are never
// exposed via MCP, regardless of channel membership.
func (h *mcpHandler) registerTools(s *mcp.Server) {
	registerTool(s, h, toolListWorkspaces,
		"List accessible workspaces with their configuration details (case mode, status sets, custom field schema).",
		h.runListWorkspaces)
	registerTool(s, h, toolListCases,
		"List cases in a workspace. Private cases are never returned. Optionally filter by status (DRAFT, OPEN, CLOSED).",
		h.runListCases)
	registerTool(s, h, toolGetCases,
		"Get full details for multiple cases by ID in a workspace. Private cases are silently omitted from the result.",
		h.runGetCases)
	registerTool(s, h, toolListActions,
		"List actions in a workspace, optionally scoped to a single case. Actions of private cases are never returned.",
		h.runListActions)
	registerTool(s, h, toolGetActions,
		"Get details for multiple actions by ID in a workspace. Actions belonging to private cases are silently omitted.",
		h.runGetActions)
}

// --- list_workspaces ---

type listWorkspacesInput struct{}

type fieldDef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Semantic string `json:"semantic,omitempty"`
	// SemanticHint describes the expected value so a client can fill the
	// field without knowing the semantic by name.
	SemanticHint string `json:"semantic_hint,omitempty"`
	// Pattern is the regular expression the whole value must match, and
	// PatternHint states that matching rule.
	Pattern     string `json:"pattern,omitempty"`
	PatternHint string `json:"pattern_hint,omitempty"`
}

type workspaceDetail struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Description    string     `json:"description,omitempty"`
	Emoji          string     `json:"emoji,omitempty"`
	Color          string     `json:"color,omitempty"`
	CaseMode       string     `json:"case_mode"`
	ActionStatuses []string   `json:"action_statuses"`
	CaseStatuses   []string   `json:"case_statuses,omitempty"`
	FieldSchema    []fieldDef `json:"field_schema"`
}

type listWorkspacesOutput struct {
	Workspaces []workspaceDetail `json:"workspaces"`
}

func (h *mcpHandler) runListWorkspaces(ctx context.Context, _ listWorkspacesInput) (listWorkspacesOutput, error) {
	entries := h.registry.List()
	if h.identity != nil {
		filtered, err := h.identity.WorkspaceAccess.FilterAccessibleForCurrentUser(ctx, entries)
		if err != nil {
			return listWorkspacesOutput{}, goerr.Wrap(err, "failed to filter accessible MCP workspaces")
		}
		entries = filtered
	}
	out := listWorkspacesOutput{Workspaces: make([]workspaceDetail, 0, len(entries))}
	for _, e := range entries {
		wd := workspaceDetail{
			ID:             e.Workspace.ID,
			Name:           e.Workspace.Name,
			Description:    e.Workspace.Description,
			Emoji:          e.Workspace.Emoji,
			Color:          e.Workspace.Color,
			CaseMode:       string(e.CaseMode.Normalize()),
			ActionStatuses: []string{},
			FieldSchema:    []fieldDef{},
		}
		if e.ActionStatusSet != nil {
			wd.ActionStatuses = e.ActionStatusSet.IDs()
		}
		if e.CaseStatusSet != nil {
			wd.CaseStatuses = e.CaseStatusSet.IDs()
		}
		if e.FieldSchema != nil {
			for _, f := range e.FieldSchema.Fields {
				wd.FieldSchema = append(wd.FieldSchema, fieldDef{
					ID:           f.ID,
					Name:         f.Name,
					Type:         string(f.Type),
					Semantic:     string(f.Semantic),
					SemanticHint: semantic.PromptHint(f.Semantic),
					Pattern:      string(f.Validation.Pattern),
					PatternHint:  f.Validation.Pattern.PromptHint(),
				})
			}
		}
		out.Workspaces = append(out.Workspaces, wd)
	}
	return out, nil
}

// --- shared case / action views ---

type caseSummary struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	BoardStatus string   `json:"board_status,omitempty"`
	ReporterID  string   `json:"reporter_id,omitempty"`
	AssigneeIDs []string `json:"assignee_ids"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

type fieldValue struct {
	FieldID string `json:"field_id"`
	Type    string `json:"type"`
	Value   any    `json:"value"`
}

type caseDetail struct {
	caseSummary
	Description    string       `json:"description,omitempty"`
	SlackChannelID string       `json:"slack_channel_id,omitempty"`
	SlackThreadTS  string       `json:"slack_thread_ts,omitempty"`
	FieldValues    []fieldValue `json:"field_values"`
	AgentSourceIDs []string     `json:"agent_source_ids"`
}

type actionDetail struct {
	ID             int64  `json:"id"`
	CaseID         int64  `json:"case_id"`
	Title          string `json:"title"`
	Description    string `json:"description,omitempty"`
	AssigneeID     string `json:"assignee_id,omitempty"`
	Status         string `json:"status"`
	DueDate        string `json:"due_date,omitempty"`
	ArchivedAt     string `json:"archived_at,omitempty"`
	SlackMessageTS string `json:"slack_message_ts,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

func toCaseSummary(c *model.Case) caseSummary {
	assignees := c.AssigneeIDs
	if assignees == nil {
		assignees = []string{}
	}
	return caseSummary{
		ID:          c.ID,
		Title:       c.Title,
		Status:      string(c.Status),
		BoardStatus: c.BoardStatus,
		ReporterID:  c.ReporterID,
		AssigneeIDs: assignees,
		CreatedAt:   c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   c.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func toCaseDetail(c *model.Case) caseDetail {
	fvs := make([]fieldValue, 0, len(c.FieldValues))
	for _, fv := range c.FieldValues {
		fvs = append(fvs, fieldValue{
			FieldID: string(fv.FieldID),
			Type:    string(fv.Type),
			Value:   fv.Value,
		})
	}
	sourceIDs := make([]string, 0, len(c.AgentSourceIDs))
	for _, id := range c.AgentSourceIDs {
		sourceIDs = append(sourceIDs, string(id))
	}
	return caseDetail{
		caseSummary:    toCaseSummary(c),
		Description:    c.Description,
		SlackChannelID: c.SlackChannelID,
		SlackThreadTS:  c.SlackThreadTS,
		FieldValues:    fvs,
		AgentSourceIDs: sourceIDs,
	}
}

func toActionDetail(a *model.Action) actionDetail {
	d := actionDetail{
		ID:             a.ID,
		CaseID:         a.CaseID,
		Title:          a.Title,
		Description:    a.Description,
		AssigneeID:     a.AssigneeID,
		Status:         a.Status.String(),
		SlackMessageTS: a.SlackMessageTS,
		CreatedAt:      a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      a.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if a.DueDate != nil {
		d.DueDate = a.DueDate.UTC().Format(time.RFC3339)
	}
	if a.ArchivedAt != nil {
		d.ArchivedAt = a.ArchivedAt.UTC().Format(time.RFC3339)
	}
	return d
}

// --- list_cases ---

type listCasesInput struct {
	WorkspaceID string `json:"workspace_id" jsonschema:"the workspace ID to list cases for"`
	Status      string `json:"status,omitempty" jsonschema:"optional status filter: DRAFT, OPEN, or CLOSED"`
}

type listCasesOutput struct {
	Cases []caseSummary `json:"cases"`
}

func (h *mcpHandler) runListCases(ctx context.Context, in listCasesInput) (listCasesOutput, error) {
	if in.WorkspaceID == "" {
		return listCasesOutput{}, goerr.New("workspace_id is required")
	}
	var statusPtr *types.CaseStatus
	if in.Status != "" {
		status, err := types.ParseCaseStatus(in.Status)
		if err != nil {
			return listCasesOutput{}, goerr.Wrap(err, "invalid status filter", goerr.V("status", in.Status))
		}
		statusPtr = &status
	}

	// Archived cases are never exposed via MCP: the tool has no archive
	// argument, so the read surface stays the active slice.
	cases, err := h.caseUC.ListCases(ctx, in.WorkspaceID, statusPtr, interfaces.CaseArchiveScopeActiveOnly)
	if err != nil {
		return listCasesOutput{}, goerr.Wrap(err, "failed to list cases")
	}

	out := listCasesOutput{Cases: make([]caseSummary, 0, len(cases))}
	for _, c := range cases {
		// Private cases are never exposed via MCP, even to members. A
		// RestrictCase'd entry also keeps IsPrivate true, so this single
		// check covers both the member and non-member shapes.
		if c.IsPrivate {
			continue
		}
		out.Cases = append(out.Cases, toCaseSummary(c))
	}
	return out, nil
}

// --- get_cases ---

type getCasesInput struct {
	WorkspaceID string  `json:"workspace_id" jsonschema:"the workspace ID the cases belong to"`
	IDs         []int64 `json:"ids" jsonschema:"the case IDs to fetch (at least one)"`
}

type getCasesOutput struct {
	Cases []caseDetail `json:"cases"`
}

func (h *mcpHandler) runGetCases(ctx context.Context, in getCasesInput) (getCasesOutput, error) {
	if in.WorkspaceID == "" {
		return getCasesOutput{}, goerr.New("workspace_id is required")
	}
	if len(in.IDs) == 0 {
		return getCasesOutput{}, goerr.New("at least one case id is required")
	}

	cases, err := h.caseUC.GetCases(ctx, in.WorkspaceID, in.IDs)
	if err != nil {
		return getCasesOutput{}, goerr.Wrap(err, "failed to get cases")
	}

	out := getCasesOutput{Cases: make([]caseDetail, 0, len(cases))}
	for _, c := range cases {
		// Private cases (full for members, RestrictCase'd for non-members) are
		// never returned via MCP — omit without revealing their existence.
		if c.IsPrivate {
			continue
		}
		out.Cases = append(out.Cases, toCaseDetail(c))
	}
	return out, nil
}

// --- list_actions ---

type listActionsInput struct {
	WorkspaceID     string `json:"workspace_id" jsonschema:"the workspace ID to list actions for"`
	CaseID          int64  `json:"case_id,omitempty" jsonschema:"optional case ID to scope the actions to"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"include archived actions (default false)"`
}

type listActionsOutput struct {
	Actions []actionDetail `json:"actions"`
}

func (h *mcpHandler) runListActions(ctx context.Context, in listActionsInput) (listActionsOutput, error) {
	if in.WorkspaceID == "" {
		return listActionsOutput{}, goerr.New("workspace_id is required")
	}
	opts := interfaces.ActionListOptions{
		ExcludePrivateCaseActions: true,
	}
	if in.IncludeArchived {
		opts.ArchiveScope = interfaces.ActionArchiveScopeAll
	}

	var actions []*model.Action
	var err error
	if in.CaseID != 0 {
		actions, err = h.actionUC.GetActionsByCase(ctx, in.WorkspaceID, in.CaseID, opts)
	} else {
		actions, err = h.actionUC.ListActions(ctx, in.WorkspaceID, opts)
	}
	if err != nil {
		return listActionsOutput{}, goerr.Wrap(err, "failed to list actions")
	}

	out := listActionsOutput{Actions: make([]actionDetail, 0, len(actions))}
	for _, a := range actions {
		out.Actions = append(out.Actions, toActionDetail(a))
	}
	return out, nil
}

// --- get_actions ---

type getActionsInput struct {
	WorkspaceID string  `json:"workspace_id" jsonschema:"the workspace ID the actions belong to"`
	IDs         []int64 `json:"ids" jsonschema:"the action IDs to fetch (at least one)"`
}

type getActionsOutput struct {
	Actions []actionDetail `json:"actions"`
}

func (h *mcpHandler) runGetActions(ctx context.Context, in getActionsInput) (getActionsOutput, error) {
	if in.WorkspaceID == "" {
		return getActionsOutput{}, goerr.New("workspace_id is required")
	}
	if len(in.IDs) == 0 {
		return getActionsOutput{}, goerr.New("at least one action id is required")
	}
	opts := interfaces.ActionListOptions{ExcludePrivateCaseActions: true}
	actions, err := h.actionUC.GetActions(ctx, in.WorkspaceID, in.IDs, opts)
	if err != nil {
		return getActionsOutput{}, goerr.Wrap(err, "failed to get actions")
	}

	// Not-found ids and actions whose parent case is private are already
	// omitted by GetActions.
	out := getActionsOutput{Actions: make([]actionDetail, 0, len(actions))}
	for _, a := range actions {
		out.Actions = append(out.Actions, toActionDetail(a))
	}
	return out, nil
}
