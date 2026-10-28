package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
)

// mockActionMutator captures CreateAction and UpdateAction calls so the
// create / update / set-assignee / status tool tests can verify the tools
// route through the unified ActionUseCase entry point. The real
// ActionUseCase (and its Slack post / refresh / event recording) is
// exercised by usecase-level tests; here we only check the tool's
// argument-extraction behaviour.
type mockActionMutator struct {
	createFn    func(ctx context.Context, workspaceID string, caseID int64, title, description string, assigneeID string, slackMessageTS string, status types.ActionStatus, dueDate *time.Time) (*model.Action, error)
	updateFn    func(ctx context.Context, workspaceID string, actionID int64, params core.UpdateActionParams) (*model.Action, error)
	archiveFn   func(ctx context.Context, workspaceID string, actionID int64) (*model.Action, error)
	unarchiveFn func(ctx context.Context, workspaceID string, actionID int64) (*model.Action, error)
}

func (m *mockActionMutator) CreateAction(ctx context.Context, workspaceID string, caseID int64, title, description string, assigneeID string, slackMessageTS string, status types.ActionStatus, dueDate *time.Time) (*model.Action, error) {
	if m.createFn != nil {
		return m.createFn(ctx, workspaceID, caseID, title, description, assigneeID, slackMessageTS, status, dueDate)
	}
	return &model.Action{
		ID: 1, CaseID: caseID, Title: title, Description: description,
		AssigneeID: assigneeID, SlackMessageTS: slackMessageTS,
		Status: status, DueDate: dueDate,
	}, nil
}

func (m *mockActionMutator) UpdateAction(ctx context.Context, workspaceID string, actionID int64, params core.UpdateActionParams) (*model.Action, error) {
	if m.updateFn != nil {
		return m.updateFn(ctx, workspaceID, actionID, params)
	}
	// Default: synthesize a minimal Action reflecting the requested edits.
	// Callers that need to assert on persisted state should provide their
	// own updateFn.
	a := &model.Action{ID: actionID}
	if params.Title != nil {
		a.Title = *params.Title
	}
	if params.Description != nil {
		a.Description = *params.Description
	}
	if params.AssigneeID != nil {
		a.AssigneeID = *params.AssigneeID
	}
	if params.Status != nil {
		a.Status = *params.Status
	}
	if params.DueDate != nil {
		a.DueDate = params.DueDate
	}
	return a, nil
}

func (m *mockActionMutator) ArchiveAction(ctx context.Context, workspaceID string, actionID int64) (*model.Action, error) {
	if m.archiveFn != nil {
		return m.archiveFn(ctx, workspaceID, actionID)
	}
	now := time.Now().UTC()
	return &model.Action{ID: actionID, ArchivedAt: &now}, nil
}

func (m *mockActionMutator) UnarchiveAction(ctx context.Context, workspaceID string, actionID int64) (*model.Action, error) {
	if m.unarchiveFn != nil {
		return m.unarchiveFn(ctx, workspaceID, actionID)
	}
	return &model.Action{ID: actionID}, nil
}

// newCtxWithUpdateCapture returns a context that captures all update messages
// and a pointer to the slice where they are appended.
func newCtxWithUpdateCapture() (context.Context, *[]string) {
	var messages []string
	ctx := tool.WithUpdate(context.Background(), func(_ context.Context, msg string) {
		messages = append(messages, msg)
	})
	return ctx, &messages
}

const (
	testWorkspaceID = "ws-tool-test"
	testCaseID      = int64(100)
)

// ----- mock ActionRepository -----

type mockActionRepo struct {
	createFn     func(ctx context.Context, workspaceID string, action *model.Action) (*model.Action, error)
	getFn        func(ctx context.Context, workspaceID string, id int64) (*model.Action, error)
	listFn       func(ctx context.Context, workspaceID string, opts interfaces.ActionListOptions) ([]*model.Action, error)
	updateFn     func(ctx context.Context, workspaceID string, action *model.Action) (*model.Action, error)
	deleteFn     func(ctx context.Context, workspaceID string, id int64) error
	getByCaseFn  func(ctx context.Context, workspaceID string, caseID int64, opts interfaces.ActionListOptions) ([]*model.Action, error)
	getByCasesFn func(ctx context.Context, workspaceID string, caseIDs []int64, opts interfaces.ActionListOptions) (map[int64][]*model.Action, error)
}

func (m *mockActionRepo) Create(ctx context.Context, workspaceID string, action *model.Action) (*model.Action, error) {
	if m.createFn != nil {
		return m.createFn(ctx, workspaceID, action)
	}
	created := *action
	created.ID = 1
	return &created, nil
}

func (m *mockActionRepo) Get(ctx context.Context, workspaceID string, id int64) (*model.Action, error) {
	if m.getFn != nil {
		return m.getFn(ctx, workspaceID, id)
	}
	return nil, errors.New("not found")
}

func (m *mockActionRepo) List(ctx context.Context, workspaceID string, opts interfaces.ActionListOptions) ([]*model.Action, error) {
	if m.listFn != nil {
		return m.listFn(ctx, workspaceID, opts)
	}
	return nil, nil
}

func (m *mockActionRepo) Update(ctx context.Context, workspaceID string, action *model.Action) (*model.Action, error) {
	if m.updateFn != nil {
		return m.updateFn(ctx, workspaceID, action)
	}
	return action, nil
}

func (m *mockActionRepo) Delete(ctx context.Context, workspaceID string, id int64) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, workspaceID, id)
	}
	return nil
}

func (m *mockActionRepo) GetByCase(ctx context.Context, workspaceID string, caseID int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
	if m.getByCaseFn != nil {
		return m.getByCaseFn(ctx, workspaceID, caseID, opts)
	}
	return nil, nil
}

func (m *mockActionRepo) GetByCases(ctx context.Context, workspaceID string, caseIDs []int64, opts interfaces.ActionListOptions) (map[int64][]*model.Action, error) {
	if m.getByCasesFn != nil {
		return m.getByCasesFn(ctx, workspaceID, caseIDs, opts)
	}
	return nil, nil
}

func (m *mockActionRepo) GetByIDs(ctx context.Context, workspaceID string, ids []int64) (map[int64]*model.Action, error) {
	// Tests in this file do not exercise batch get; the agent tools
	// resolve actions one at a time through Get. Returning an empty
	// map keeps the interface contract satisfied without forcing every
	// existing test to wire up a getByIDsFn.
	return map[int64]*model.Action{}, nil
}

func (m *mockActionRepo) GetBySlackMessageTS(ctx context.Context, workspaceID string, ts string) (*model.Action, error) {
	return nil, nil
}

// ----- mock Repository -----

type mockRepo struct {
	actionRepo interfaces.ActionRepository
}

func (m *mockRepo) Action() interfaces.ActionRepository { return m.actionRepo }

// Unused methods — panic to catch accidental calls in tests
func (m *mockRepo) Case() interfaces.CaseRepository           { panic("unexpected call: Case()") }
func (m *mockRepo) Slack() interfaces.SlackRepository         { panic("unexpected call: Slack()") }
func (m *mockRepo) SlackUser() interfaces.SlackUserRepository { panic("unexpected call: SlackUser()") }
func (m *mockRepo) Source() interfaces.SourceRepository       { panic("unexpected call: Source()") }
func (m *mockRepo) CaseMessage() interfaces.CaseMessageRepository {
	panic("unexpected call: CaseMessage()")
}
func (m *mockRepo) ActionMessage() interfaces.ActionMessageRepository {
	panic("unexpected call: ActionMessage()")
}
func (m *mockRepo) ActionEvent() interfaces.ActionEventRepository {
	panic("unexpected call: ActionEvent()")
}
func (m *mockRepo) ActionStep() interfaces.ActionStepRepository {
	panic("unexpected call: ActionStep()")
}
func (m *mockRepo) ActionComment() interfaces.ActionCommentRepository {
	panic("unexpected call: ActionComment()")
}
func (m *mockRepo) PutToken(ctx context.Context, token *auth.Token) error {
	panic("unexpected call: PutToken()")
}
func (m *mockRepo) GetToken(ctx context.Context, tokenID auth.TokenID) (*auth.Token, error) {
	panic("unexpected call: GetToken()")
}
func (m *mockRepo) DeleteToken(ctx context.Context, tokenID auth.TokenID) error {
	panic("unexpected call: DeleteToken()")
}
func (m *mockRepo) AssistLog() interfaces.AssistLogRepository {
	panic("unexpected call: AssistLog()")
}
func (m *mockRepo) CaseProposal() interfaces.CaseProposalRepository {
	panic("unexpected call: CaseProposal()")
}
func (m *mockRepo) Session() interfaces.SessionRepository {
	panic("unexpected call: Session()")
}
func (m *mockRepo) NotificationSlot() interfaces.NotificationSlotRepository {
	panic("unexpected call: NotificationSlot()")
}
func (m *mockRepo) JobRun() interfaces.JobRunRepository {
	panic("unexpected call: JobRun()")
}
func (m *mockRepo) JobRunLog() interfaces.JobRunLogRepository {
	panic("unexpected call: JobRunLog()")
}
func (m *mockRepo) JobRunEvent() interfaces.JobRunEventRepository {
	panic("unexpected call: JobRunEvent()")
}
func (m *mockRepo) JobSlot() interfaces.JobSlotRepository {
	panic("unexpected call: JobSlot()")
}
func (m *mockRepo) Import() interfaces.ImportRepository {
	panic("unexpected call: Import()")
}
func (m *mockRepo) ReactionClaim() interfaces.ReactionClaimRepository {
	panic("unexpected call: ReactionClaim()")
}
func (m *mockRepo) UserPreference() interfaces.UserPreferenceRepository {
	panic("unexpected call: UserPreference()")
}
func (m *mockRepo) HomeMessage() interfaces.HomeMessageRepository {
	panic("unexpected call: HomeMessage()")
}
func (m *mockRepo) AssigneeRanking() interfaces.AssigneeRankingRepository {
	panic("unexpected call: AssigneeRanking()")
}
func (m *mockRepo) SlackMemberChannels() interfaces.SlackMemberChannelsRepository {
	panic("unexpected call: SlackMemberChannels()")
}
func (m *mockRepo) Memo() interfaces.MemoRepository {
	panic("unexpected call: Memo()")
}
func (m *mockRepo) Knowledge() interfaces.KnowledgeRepository {
	panic("unexpected call: Knowledge()")
}
func (m *mockRepo) Tag() interfaces.TagRepository {
	panic("unexpected call: Tag()")
}
func (m *mockRepo) Close() error { return nil }

// newMockRepo builds a mockRepo with default no-op sub-repos
func newMockRepo(actionRepo interfaces.ActionRepository) *mockRepo {
	if actionRepo == nil {
		actionRepo = &mockActionRepo{}
	}
	return &mockRepo{actionRepo: actionRepo}
}

// findTool returns the tool with the given name from the list
func findTool(tools []gollem.Tool, name string) gollem.Tool {
	for _, t := range tools {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

// ----- tests -----

func TestNew_ReturnsAllTools(t *testing.T) {
	repo := newMockRepo(nil)
	tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})
	gt.Array(t, tools).Length(13)

	toolNames := make(map[string]bool)
	for _, tl := range tools {
		toolNames[tl.Spec().Name] = true
	}
	gt.Value(t, toolNames["core__list_actions"]).Equal(true)
	gt.Value(t, toolNames["core__archive_action"]).Equal(true)
	gt.Value(t, toolNames["core__unarchive_action"]).Equal(true)
	gt.Value(t, toolNames["core__get_action"]).Equal(true)
	gt.Value(t, toolNames["core__create_action"]).Equal(true)
	gt.Value(t, toolNames["core__update_action"]).Equal(true)
	gt.Value(t, toolNames["core__update_action_status"]).Equal(true)
	gt.Value(t, toolNames["core__set_action_assignee"]).Equal(true)
	gt.Value(t, toolNames["core__list_action_steps"]).Equal(true)
	gt.Value(t, toolNames["core__add_action_step"]).Equal(true)
	gt.Value(t, toolNames["core__set_action_step_done"]).Equal(true)
	gt.Value(t, toolNames["core__rename_action_step"]).Equal(true)
	gt.Value(t, toolNames["core__delete_action_step"]).Equal(true)
}

func TestNewForAssist_ReturnsSameTools(t *testing.T) {
	repo := newMockRepo(nil)
	tools := core.NewForAssist(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})
	gt.Array(t, tools).Length(13)
}

func TestListActionsTool(t *testing.T) {
	ctx := context.Background()

	t.Run("returns empty list when repository returns no actions", func(t *testing.T) {
		actionRepo := &mockActionRepo{
			getByCaseFn: func(ctx context.Context, workspaceID string, caseID int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
				gt.Value(t, workspaceID).Equal(testWorkspaceID)
				gt.Value(t, caseID).Equal(testCaseID)
				gt.Value(t, opts.ArchiveScope).Equal(interfaces.ActionArchiveScopeActiveOnly)
				return []*model.Action{}, nil
			},
		}
		repo := newMockRepo(actionRepo)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		result, err := findTool(tools, "core__list_actions").Run(ctx, map[string]any{})
		gt.NoError(t, err)
		items := result["actions"].([]map[string]any)
		gt.Array(t, items).Length(0)
	})

	t.Run("returns actions from repository", func(t *testing.T) {
		actionRepo := &mockActionRepo{
			getByCaseFn: func(ctx context.Context, workspaceID string, caseID int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
				return []*model.Action{
					{ID: 1, CaseID: caseID, Title: "Fix bug", Status: types.ActionStatusTodo, AssigneeID: "U001"},
					{ID: 2, CaseID: caseID, Title: "Write docs", Status: types.ActionStatusCompleted},
				}, nil
			},
		}
		repo := newMockRepo(actionRepo)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		result, err := findTool(tools, "core__list_actions").Run(ctx, map[string]any{})
		gt.NoError(t, err)
		items := result["actions"].([]map[string]any)
		gt.Array(t, items).Length(2)
		gt.Value(t, items[0]["title"]).Equal("Fix bug")
		gt.Value(t, items[1]["title"]).Equal("Write docs")
	})

	t.Run("propagates repository error", func(t *testing.T) {
		actionRepo := &mockActionRepo{
			getByCaseFn: func(_ context.Context, _ string, _ int64, _ interfaces.ActionListOptions) ([]*model.Action, error) {
				return nil, errors.New("database unavailable")
			},
		}
		repo := newMockRepo(actionRepo)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		_, err := findTool(tools, "core__list_actions").Run(ctx, map[string]any{})
		gt.Error(t, err)
	})
}

func TestGetActionTool(t *testing.T) {
	ctx := context.Background()

	t.Run("passes correct workspace and action ID to repository", func(t *testing.T) {
		var gotWorkspaceID string
		var gotActionID int64
		actionRepo := &mockActionRepo{
			getFn: func(ctx context.Context, workspaceID string, id int64) (*model.Action, error) {
				gotWorkspaceID = workspaceID
				gotActionID = id
				return &model.Action{ID: id, CaseID: testCaseID, Title: "My action", Status: types.ActionStatusInProgress}, nil
			},
		}
		repo := newMockRepo(actionRepo)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		result, err := findTool(tools, "core__get_action").Run(ctx, map[string]any{"action_id": float64(42)})
		gt.NoError(t, err)
		gt.Value(t, gotWorkspaceID).Equal(testWorkspaceID)
		gt.Value(t, gotActionID).Equal(int64(42))
		gt.Value(t, result["title"]).Equal("My action")
		gt.Value(t, result["status"]).Equal("IN_PROGRESS")
	})

	t.Run("returns error when repository returns error", func(t *testing.T) {
		actionRepo := &mockActionRepo{
			getFn: func(_ context.Context, _ string, id int64) (*model.Action, error) {
				return nil, errors.New("not found")
			},
		}
		repo := newMockRepo(actionRepo)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		_, err := findTool(tools, "core__get_action").Run(ctx, map[string]any{"action_id": float64(999)})
		gt.Error(t, err)
	})

	t.Run("returns error when action_id is missing", func(t *testing.T) {
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		_, err := findTool(tools, "core__get_action").Run(ctx, map[string]any{})
		gt.Error(t, err)
	})

	// An action id is a small integer the model can arrive at by guessing, or by
	// being told to in text it read. The read goes straight to the repository and
	// passes no access gate, so without this check a run on one case could read
	// another case's work — including a private case its actor is not in.
	t.Run("refuses an action belonging to another case", func(t *testing.T) {
		const otherCaseID = testCaseID + 1
		var reached bool
		actionRepo := &mockActionRepo{
			getFn: func(_ context.Context, _ string, id int64) (*model.Action, error) {
				reached = true
				return &model.Action{ID: id, CaseID: otherCaseID, Title: "Someone else's work",
					Status: types.ActionStatusInProgress}, nil
			},
		}
		tools := core.New(core.Deps{
			Repo: newMockRepo(actionRepo), WorkspaceID: testWorkspaceID, CaseID: testCaseID,
		})

		result, err := findTool(tools, "core__get_action").Run(ctx, map[string]any{"action_id": float64(42)})
		gt.Error(t, err).Required()
		gt.Value(t, result).Nil()
		gt.Bool(t, reached).True() // the repository was consulted; the answer was withheld
		// The same wording as a genuinely missing action: confirming that the id
		// exists but belongs elsewhere would leak the other case's existence.
		gt.String(t, err.Error()).Contains("action not found")
		gt.Bool(t, strings.Contains(err.Error(), "Someone else's work")).False()
	})

	// A run with no case pinned has no core toolset at all, so the tool does not
	// exist there; the zero guard is what keeps the check from rejecting
	// everything if one ever did.
	t.Run("without a pinned case the check does not apply", func(t *testing.T) {
		actionRepo := &mockActionRepo{
			getFn: func(_ context.Context, _ string, id int64) (*model.Action, error) {
				return &model.Action{ID: id, CaseID: 7, Title: "Any", Status: types.ActionStatusTodo}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(actionRepo), WorkspaceID: testWorkspaceID})

		result, err := findTool(tools, "core__get_action").Run(ctx, map[string]any{"action_id": float64(1)})
		gt.NoError(t, err).Required()
		gt.Value(t, result["title"]).Equal("Any")
	})
}

func TestCreateActionTool(t *testing.T) {
	ctx := context.Background()

	t.Run("creates action with correct fields", func(t *testing.T) {
		var (
			capturedWorkspaceID string
			capturedCaseID      int64
			capturedTitle       string
			capturedDescription string
			capturedAssigneeID  string
			capturedStatus      types.ActionStatus
		)
		creator := &mockActionMutator{
			createFn: func(_ context.Context, workspaceID string, caseID int64, title, description string, assigneeID string, _ string, status types.ActionStatus, _ *time.Time) (*model.Action, error) {
				capturedWorkspaceID = workspaceID
				capturedCaseID = caseID
				capturedTitle = title
				capturedDescription = description
				capturedAssigneeID = assigneeID
				capturedStatus = status
				return &model.Action{ID: 10, CaseID: caseID, Title: title, Description: description, AssigneeID: assigneeID, Status: status}, nil
			},
		}
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: creator})

		result, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{
			"title":       "New investigation",
			"description": "Look into the alerts",
			"status":      "IN_PROGRESS",
			"assignee_id": "U001",
		})
		gt.NoError(t, err)
		gt.Value(t, capturedWorkspaceID).Equal(testWorkspaceID)
		gt.Value(t, capturedCaseID).Equal(testCaseID)
		gt.Value(t, capturedTitle).Equal("New investigation")
		gt.Value(t, capturedDescription).Equal("Look into the alerts")
		gt.Value(t, capturedStatus).Equal(types.ActionStatusInProgress)
		gt.Value(t, capturedAssigneeID).Equal("U001")
		gt.Value(t, result["id"]).Equal(int64(10))
	})

	t.Run("defaults status to TODO when omitted", func(t *testing.T) {
		var capturedStatus types.ActionStatus
		creator := &mockActionMutator{
			createFn: func(_ context.Context, _ string, caseID int64, title, description string, assigneeID string, _ string, status types.ActionStatus, _ *time.Time) (*model.Action, error) {
				capturedStatus = status
				return &model.Action{ID: 1, CaseID: caseID, Title: title, Description: description, AssigneeID: assigneeID, Status: status}, nil
			},
		}
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: creator})

		_, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{"title": "Quick task"})
		gt.NoError(t, err)
		// With no statusSet override, the tool falls back to
		// model.DefaultActionStatusSet() whose initial id is BACKLOG.
		gt.Value(t, capturedStatus).Equal(types.ActionStatusBacklog)
	})

	t.Run("returns error when title is missing", func(t *testing.T) {
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})

		_, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{})
		gt.Error(t, err)
	})

	t.Run("returns error for invalid status", func(t *testing.T) {
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})

		_, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{
			"title":  "Test",
			"status": "FLYING",
		})
		gt.Error(t, err)
	})

	t.Run("returns error when assignee_ids contains non-string element", func(t *testing.T) {
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})

		_, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{
			"title":       "Test",
			"assignee_id": 42,
		})
		gt.Error(t, err)
	})

	t.Run("returns error when ActionUC dependency is nil", func(t *testing.T) {
		// Guards against silent regression to the legacy repository-direct
		// path: if Deps.ActionUC is forgotten by a future caller, the tool
		// should fail loudly rather than skip Slack posting.
		repo := newMockRepo(nil)
		tools := core.New(core.Deps{Repo: repo, WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		_, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{
			"title": "Should not reach repo",
		})
		gt.Error(t, err)
	})
}

func TestUpdateActionTool(t *testing.T) {
	ctx := context.Background()

	// captureUpdateMutator returns a mutator whose UpdateAction stores the
	// invocation arguments into the supplied pointers. Tests then assert on
	// what was captured rather than reaching for repository.Update.
	captureUpdateMutator := func(capID *int64, capParams *core.UpdateActionParams) *mockActionMutator {
		return &mockActionMutator{
			updateFn: func(_ context.Context, _ string, actionID int64, params core.UpdateActionParams) (*model.Action, error) {
				*capID = actionID
				*capParams = params
				return &model.Action{ID: actionID}, nil
			},
		}
	}

	t.Run("forwards title and description as pointer fields", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := captureUpdateMutator(&capID, &capParams)
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(5),
			"title":       "New title",
			"description": "New description",
		})
		gt.NoError(t, err)
		gt.Value(t, capID).Equal(int64(5))
		gt.Value(t, capParams.Title).NotNil().Required()
		gt.Value(t, *capParams.Title).Equal("New title")
		gt.Value(t, capParams.Description).NotNil().Required()
		gt.Value(t, *capParams.Description).Equal("New description")
		// Assignee fields untouched: tool must signal "no change", not
		// accidentally clear or reassign.
		gt.Value(t, capParams.AssigneeID).Nil()
		gt.Bool(t, capParams.ClearAssignee).False()
	})

	t.Run("forwards non-empty assignee_id as AssigneeID", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := captureUpdateMutator(&capID, &capParams)
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(6),
			"assignee_id": "U003",
		})
		gt.NoError(t, err)
		gt.Value(t, capParams.AssigneeID).NotNil().Required()
		gt.Value(t, *capParams.AssigneeID).Equal("U003")
		gt.Bool(t, capParams.ClearAssignee).False()
	})

	t.Run("translates empty assignee_id into ClearAssignee=true", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := captureUpdateMutator(&capID, &capParams)
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(60),
			"assignee_id": "",
		})
		gt.NoError(t, err)
		// AssigneeID stays nil — the explicit clear flag is what the
		// usecase reads.
		gt.Value(t, capParams.AssigneeID).Nil()
		gt.Bool(t, capParams.ClearAssignee).True()
	})

	t.Run("translates empty due_date into ClearDueDate=true", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := captureUpdateMutator(&capID, &capParams)
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id": float64(61),
			"due_date":  "",
		})
		gt.NoError(t, err)
		gt.Value(t, capParams.DueDate).Nil()
		gt.Bool(t, capParams.ClearDueDate).True()
	})

	t.Run("treats empty description as no-change", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := captureUpdateMutator(&capID, &capParams)
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(7),
			"description": "",
		})
		gt.NoError(t, err)
		// No-change semantics: pointer stays nil so usecase keeps the
		// stored description.
		gt.Value(t, capParams.Description).Nil()
	})

	t.Run("omits unspecified fields entirely", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := captureUpdateMutator(&capID, &capParams)
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		desc := "Added desc"
		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(8),
			"description": desc,
		})
		gt.NoError(t, err)
		gt.Value(t, capParams.Title).Nil()
		gt.Value(t, capParams.AssigneeID).Nil()
		gt.Value(t, capParams.Status).Nil()
		gt.Value(t, capParams.DueDate).Nil()
		gt.Bool(t, capParams.ClearAssignee).False()
		gt.Bool(t, capParams.ClearDueDate).False()
	})

	t.Run("returns error when action_id missing", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})
		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{})
		gt.Error(t, err)
	})

	t.Run("propagates ActionUseCase error", func(t *testing.T) {
		mutator := &mockActionMutator{
			updateFn: func(_ context.Context, _ string, _ int64, _ core.UpdateActionParams) (*model.Action, error) {
				return nil, errors.New("not found")
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{"action_id": float64(999), "title": "X"})
		gt.Error(t, err)
	})

	t.Run("returns error when assignee_id is non-string", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})
		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(10),
			"assignee_id": 99,
		})
		gt.Error(t, err)
	})

	t.Run("returns error when ActionUC dependency is nil", func(t *testing.T) {
		// Same regression guard as create_action: nil ActionUC must error
		// loudly, not silently fall back to repo.Action().Update.
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id": float64(1),
			"title":     "Anything",
		})
		gt.Error(t, err)
	})
}

func TestUpdateActionStatusTool(t *testing.T) {
	ctx := context.Background()

	t.Run("forwards status as a pointer to ActionStatus", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := &mockActionMutator{
			updateFn: func(_ context.Context, _ string, actionID int64, params core.UpdateActionParams) (*model.Action, error) {
				capID = actionID
				capParams = params
				return &model.Action{ID: actionID, Status: *params.Status}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		result, err := findTool(tools, "core__update_action_status").Run(ctx, map[string]any{
			"action_id": float64(5),
			"status":    "COMPLETED",
		})
		gt.NoError(t, err)
		gt.Value(t, capID).Equal(int64(5))
		gt.Value(t, capParams.Status).NotNil().Required()
		gt.Value(t, *capParams.Status).Equal(types.ActionStatusCompleted)
		// All other fields should be empty: status update must not
		// accidentally clear the assignee or zero the title.
		gt.Value(t, capParams.Title).Nil()
		gt.Value(t, capParams.AssigneeID).Nil()
		gt.Bool(t, capParams.ClearAssignee).False()
		gt.Value(t, result["status"]).Equal("COMPLETED")
	})

	t.Run("propagates ActionUseCase error", func(t *testing.T) {
		mutator := &mockActionMutator{
			updateFn: func(_ context.Context, _ string, _ int64, _ core.UpdateActionParams) (*model.Action, error) {
				return nil, errors.New("usecase error")
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__update_action_status").Run(ctx, map[string]any{
			"action_id": float64(1),
			"status":    "COMPLETED",
		})
		gt.Error(t, err)
	})

	t.Run("returns error for invalid status string", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})

		_, err := findTool(tools, "core__update_action_status").Run(ctx, map[string]any{
			"action_id": float64(1),
			"status":    "UNKNOWN_STATUS",
		})
		gt.Error(t, err)
	})

	t.Run("returns error when ActionUC dependency is nil", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__update_action_status").Run(ctx, map[string]any{
			"action_id": float64(1),
			"status":    "COMPLETED",
		})
		gt.Error(t, err)
	})
}

func TestSetActionAssigneeTool(t *testing.T) {
	ctx := context.Background()

	t.Run("forwards non-empty assignee_id as AssigneeID", func(t *testing.T) {
		var capID int64
		var capParams core.UpdateActionParams
		mutator := &mockActionMutator{
			updateFn: func(_ context.Context, _ string, actionID int64, params core.UpdateActionParams) (*model.Action, error) {
				capID = actionID
				capParams = params
				return &model.Action{ID: actionID, AssigneeID: *params.AssigneeID}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__set_action_assignee").Run(ctx, map[string]any{
			"action_id":   float64(3),
			"assignee_id": "U002",
		})
		gt.NoError(t, err)
		gt.Value(t, capID).Equal(int64(3))
		gt.Value(t, capParams.AssigneeID).NotNil().Required()
		gt.Value(t, *capParams.AssigneeID).Equal("U002")
		gt.Bool(t, capParams.ClearAssignee).False()
	})

	t.Run("translates empty assignee_id into ClearAssignee=true", func(t *testing.T) {
		var capParams core.UpdateActionParams
		mutator := &mockActionMutator{
			updateFn: func(_ context.Context, _ string, actionID int64, params core.UpdateActionParams) (*model.Action, error) {
				capParams = params
				return &model.Action{ID: actionID}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__set_action_assignee").Run(ctx, map[string]any{
			"action_id":   float64(4),
			"assignee_id": "",
		})
		gt.NoError(t, err)
		gt.Value(t, capParams.AssigneeID).Nil()
		gt.Bool(t, capParams.ClearAssignee).True()
	})

	t.Run("returns error when assignee_id is missing", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: &mockActionMutator{}})

		_, err := findTool(tools, "core__set_action_assignee").Run(ctx, map[string]any{
			"action_id": float64(1),
		})
		gt.Error(t, err)
	})

	t.Run("returns error when ActionUC dependency is nil", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__set_action_assignee").Run(ctx, map[string]any{
			"action_id":   float64(1),
			"assignee_id": "U001",
		})
		gt.Error(t, err)
	})
}

// ----- tool.Update call verification tests -----

func TestToolUpdateCalls(t *testing.T) {
	t.Run("list_actions posts update message", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		actionRepo := &mockActionRepo{
			getByCaseFn: func(_ context.Context, _ string, _ int64, _ interfaces.ActionListOptions) ([]*model.Action, error) {
				return []*model.Action{}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(actionRepo), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__list_actions").Run(ctx, map[string]any{})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Listing actions...")
	})

	t.Run("get_action posts update message with action ID", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		actionRepo := &mockActionRepo{
			getFn: func(_ context.Context, _ string, id int64) (*model.Action, error) {
				// CaseID matters: get_action only returns an action of the case the
				// run is pinned to.
				return &model.Action{ID: id, CaseID: testCaseID, Title: "T", Status: types.ActionStatusTodo}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(actionRepo), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__get_action").Run(ctx, map[string]any{"action_id": float64(7)})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Getting action #7...")
	})

	t.Run("create_action posts update message with title", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		creator := &mockActionMutator{}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: creator})
		_, err := findTool(tools, "core__create_action").Run(ctx, map[string]any{"title": "Deploy fix"})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Creating action: Deploy fix")
	})

	t.Run("update_action posts update message with action ID", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		mutator := &mockActionMutator{}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})
		_, err := findTool(tools, "core__update_action").Run(ctx, map[string]any{
			"action_id":   float64(11),
			"description": "Updated desc",
		})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Updating action #11...")
	})

	t.Run("update_action_status posts update message with ID and status", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		mutator := &mockActionMutator{}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})
		_, err := findTool(tools, "core__update_action_status").Run(ctx, map[string]any{
			"action_id": float64(3),
			"status":    "COMPLETED",
		})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Updating action #3 status -> COMPLETED")
	})

	t.Run("set_action_assignee posts update message when setting", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		mutator := &mockActionMutator{}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})
		_, err := findTool(tools, "core__set_action_assignee").Run(ctx, map[string]any{
			"action_id":   float64(2),
			"assignee_id": "U005",
		})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Setting assignee U005 on action #2")
	})

	t.Run("set_action_assignee posts update message when clearing", func(t *testing.T) {
		ctx, msgs := newCtxWithUpdateCapture()
		mutator := &mockActionMutator{}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})
		_, err := findTool(tools, "core__set_action_assignee").Run(ctx, map[string]any{
			"action_id":   float64(9),
			"assignee_id": "",
		})
		gt.NoError(t, err)
		gt.Array(t, *msgs).Length(1)
		gt.Value(t, (*msgs)[0]).Equal("Clearing assignee on action #9")
	})
}

func TestArchiveActionTool(t *testing.T) {
	ctx := context.Background()

	t.Run("archive_action invokes ActionMutator.ArchiveAction", func(t *testing.T) {
		var gotID int64
		now := time.Now().UTC()
		mutator := &mockActionMutator{
			archiveFn: func(_ context.Context, _ string, actionID int64) (*model.Action, error) {
				gotID = actionID
				return &model.Action{ID: actionID, Title: "archived", ArchivedAt: &now}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		result, err := findTool(tools, "core__archive_action").Run(ctx, map[string]any{
			"action_id": float64(7),
		})
		gt.NoError(t, err).Required()
		gt.Value(t, gotID).Equal(int64(7))
		gt.Value(t, result["archived"]).Equal(true)
	})

	t.Run("archive_action propagates ActionMutator error", func(t *testing.T) {
		mutator := &mockActionMutator{
			archiveFn: func(_ context.Context, _ string, _ int64) (*model.Action, error) {
				return nil, errors.New("already archived")
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__archive_action").Run(ctx, map[string]any{
			"action_id": float64(8),
		})
		gt.Error(t, err)
	})

	t.Run("archive_action without ActionUC returns error", func(t *testing.T) {
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID})

		_, err := findTool(tools, "core__archive_action").Run(ctx, map[string]any{
			"action_id": float64(1),
		})
		gt.Error(t, err)
	})
}

func TestUnarchiveActionTool(t *testing.T) {
	ctx := context.Background()

	t.Run("unarchive_action invokes ActionMutator.UnarchiveAction", func(t *testing.T) {
		var gotID int64
		mutator := &mockActionMutator{
			unarchiveFn: func(_ context.Context, _ string, actionID int64) (*model.Action, error) {
				gotID = actionID
				return &model.Action{ID: actionID, Title: "restored"}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		result, err := findTool(tools, "core__unarchive_action").Run(ctx, map[string]any{
			"action_id": float64(3),
		})
		gt.NoError(t, err).Required()
		gt.Value(t, gotID).Equal(int64(3))
		gt.Value(t, result["archived"]).Equal(false)
	})

	t.Run("unarchive_action propagates ActionMutator error", func(t *testing.T) {
		mutator := &mockActionMutator{
			unarchiveFn: func(_ context.Context, _ string, _ int64) (*model.Action, error) {
				return nil, errors.New("not archived")
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(nil), WorkspaceID: testWorkspaceID, CaseID: testCaseID, ActionUC: mutator})

		_, err := findTool(tools, "core__unarchive_action").Run(ctx, map[string]any{
			"action_id": float64(4),
		})
		gt.Error(t, err)
	})
}

func TestListActionsTool_IncludeArchived(t *testing.T) {
	ctx := context.Background()

	t.Run("default uses ActiveOnly scope", func(t *testing.T) {
		var gotOpts interfaces.ActionListOptions
		actionRepo := &mockActionRepo{
			getByCaseFn: func(_ context.Context, _ string, _ int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
				gotOpts = opts
				return []*model.Action{}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(actionRepo), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__list_actions").Run(ctx, map[string]any{})
		gt.NoError(t, err).Required()
		gt.Value(t, gotOpts.ArchiveScope).Equal(interfaces.ActionArchiveScopeActiveOnly)
	})

	t.Run("include_archived=true maps to ArchiveScopeAll", func(t *testing.T) {
		var gotOpts interfaces.ActionListOptions
		actionRepo := &mockActionRepo{
			getByCaseFn: func(_ context.Context, _ string, _ int64, opts interfaces.ActionListOptions) ([]*model.Action, error) {
				gotOpts = opts
				return []*model.Action{}, nil
			},
		}
		tools := core.New(core.Deps{Repo: newMockRepo(actionRepo), WorkspaceID: testWorkspaceID, CaseID: testCaseID})
		_, err := findTool(tools, "core__list_actions").Run(ctx, map[string]any{
			"include_archived": true,
		})
		gt.NoError(t, err).Required()
		gt.Value(t, gotOpts.ArchiveScope).Equal(interfaces.ActionArchiveScopeAll)
	})
}
