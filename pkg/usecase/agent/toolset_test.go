package agent_test

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casemulti"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/casewriter"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/core"
	knowledgetool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/knowledge"
	memotool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/memo"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/agent/tool/webfetch"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent"
)

// stubCaseMutator is a no-op casewriter.CaseMutator: the toolset tests only
// assert which tools the resolver builds, never invoke them.
type stubCaseMutator struct{}

func (stubCaseMutator) UpdateCase(context.Context, string, int64, casewriter.CaseUpdate) (*model.Case, error) {
	return &model.Case{}, nil
}
func (stubCaseMutator) UpdateCaseStatus(context.Context, string, int64, string) (*model.Case, string, error) {
	return &model.Case{}, "", nil
}
func (stubCaseMutator) CloseCase(context.Context, string, int64) (*model.Case, error) {
	return &model.Case{}, nil
}
func (stubCaseMutator) AssignCase(context.Context, string, int64, []string) (*model.Case, error) {
	return &model.Case{}, nil
}
func (stubCaseMutator) UnassignCase(context.Context, string, int64, []string) (*model.Case, error) {
	return &model.Case{}, nil
}

func testStatusSet(t *testing.T) *model.ActionStatusSet {
	t.Helper()
	set, err := model.NewActionStatusSet("open", []string{"closed"}, []model.ActionStatusDefinition{
		{ID: "open", Name: "Open"},
		{ID: "closed", Name: "Closed"},
	})
	gt.NoError(t, err).Required()
	return set
}

func TestIsKnownToolSetID(t *testing.T) {
	t.Run("known IDs return true", func(t *testing.T) {
		for _, id := range agent.KnownToolSetIDs {
			gt.Bool(t, agent.IsKnownToolSetID(id)).True()
		}
	})
	t.Run("unknown ID returns false", func(t *testing.T) {
		gt.Bool(t, agent.IsKnownToolSetID("not_a_toolset")).False()
		gt.Bool(t, agent.IsKnownToolSetID("")).False()
	})
}

func TestToolSetResolver_ResolveCore(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		Core:   core.Deps{Repo: nil, WorkspaceID: "ws", CaseID: 1},
		Slack:  slacktool.Deps{},
		Notion: notiontool.Deps{},
		GitHub: nil,
	})
	tools := r.Resolve([]string{agent.ToolSetCoreRO})
	// core_ro is read-only: list_actions + get_action (no step UC wired here).
	gt.Array(t, tools).Length(2)
}

func TestToolSetResolver_OmitCore(t *testing.T) {
	// Thread-mode workspaces manage no Actions: OmitCore must withhold the
	// core read tools even though they only need Repo (which is wired here).
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		OmitCore: true,
		Core:     core.Deps{WorkspaceID: "ws", CaseID: 1},
	})
	gt.Array(t, r.Resolve([]string{agent.ToolSetCoreRO})).Length(0)
}

func TestKnownToolSetIDsNoCore(t *testing.T) {
	t.Run("excludes the core toolset", func(t *testing.T) {
		for _, id := range agent.KnownToolSetIDsNoCore {
			gt.Bool(t, id == agent.ToolSetCoreRO).False()
		}
	})
	t.Run("retains the non-core toolsets", func(t *testing.T) {
		want := []string{
			agent.ToolSetSlackRO,
			agent.ToolSetNotion,
			agent.ToolSetGitHub,
			agent.ToolSetWebFetch,
			agent.ToolSetJira,
		}
		gt.Array(t, agent.KnownToolSetIDsNoCore).Equal(want)
	})
}

func TestToolSetResolver_ResolveMultipleAndUnknown(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		Core:   core.Deps{Repo: nil, WorkspaceID: "ws", CaseID: 1},
		Slack:  slacktool.Deps{},
		Notion: notiontool.Deps{},
		GitHub: nil,
	})
	tools := r.Resolve([]string{agent.ToolSetCoreRO, "ghost_set", agent.ToolSetSlackRO})
	// Only core (2 tools) survives — ghost_set is silently skipped, slack_ro is empty
	// because Slack Deps has no Bot/Search wired.
	gt.Array(t, tools).Length(2)
}

func TestToolSetResolver_ResolveEmpty(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{})
	gt.Array(t, r.Resolve(nil)).Length(0)
	gt.Array(t, r.Resolve([]string{})).Length(0)
}

func TestToolSetResolver_NilReceiver(t *testing.T) {
	var r *agent.ToolSetResolver
	gt.Array(t, r.Resolve([]string{agent.ToolSetCoreRO})).Length(0)
}

func TestToolSetResolver_ResolveWebFetch(t *testing.T) {
	llm := &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{}, nil
		},
	}
	wfClient := webfetch.NewClient(webfetch.ClientConfig{
		Timeout:     10 * time.Second,
		MaxBytes:    1024,
		MaxPDFBytes: 1024,
		UserAgent:   "test",
		LLM:         llm,
	})

	t.Run("webfetch ID resolves to the single tool", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{WebFetch: wfClient})
		tools := r.Resolve([]string{agent.ToolSetWebFetch})
		gt.Array(t, tools).Length(1).Required()
		gt.String(t, tools[0].Spec().Name).Equal("webfetch")
	})

	t.Run("combined with core returns both sets", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			Core:     core.Deps{WorkspaceID: "ws", CaseID: 1},
			WebFetch: wfClient,
		})
		tools := r.Resolve([]string{agent.ToolSetCoreRO, agent.ToolSetWebFetch})
		// core_ro (2) + webfetch (1).
		gt.Array(t, tools).Length(3)
	})

	t.Run("nil webfetch client yields no tools", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{WebFetch: nil})
		gt.Array(t, r.Resolve([]string{agent.ToolSetWebFetch})).Length(0)
	})
}

func TestToolSetResolver_CaseWrite(t *testing.T) {
	t.Run("thread-mode deps resolve to the full writer set", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			CaseWrite: casewriter.Deps{
				CaseUC:      stubCaseMutator{},
				WorkspaceID: "ws",
				CaseID:      7,
				StatusSet:   testStatusSet(t),
			},
		})
		tools := r.Resolve([]string{agent.ToolSetCaseWrite})
		names := make([]string, 0, len(tools))
		for _, tl := range tools {
			names = append(names, tl.Spec().Name)
		}
		gt.Array(t, names).Equal([]string{
			"case__update_case",
			"case__assign",
			"case__unassign",
			"case__update_case_status",
		})
	})

	t.Run("nil StatusSet still resolves to writes, closing via case__close_case", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			CaseWrite: casewriter.Deps{CaseUC: stubCaseMutator{}, WorkspaceID: "ws", CaseID: 7},
		})
		tools := r.Resolve([]string{agent.ToolSetCaseWrite})
		names := make([]string, 0, len(tools))
		for _, tl := range tools {
			names = append(names, tl.Spec().Name)
		}
		gt.Array(t, names).Equal([]string{
			"case__update_case",
			"case__assign",
			"case__unassign",
			"case__close_case",
		})
	})

	t.Run("no case pinned builds no writer tools (create turn)", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			CaseWrite: casewriter.Deps{CaseUC: stubCaseMutator{}, WorkspaceID: "ws", StatusSet: testStatusSet(t)},
		})
		gt.Array(t, r.Resolve([]string{agent.ToolSetCaseWrite})).Length(0)
	})

	t.Run("nil CaseUC builds no writer tools", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			CaseWrite: casewriter.Deps{WorkspaceID: "ws", CaseID: 7, StatusSet: testStatusSet(t)},
		})
		gt.Array(t, r.Resolve([]string{agent.ToolSetCaseWrite})).Length(0)
	})

	t.Run("not requested means not included", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			CaseWrite: casewriter.Deps{
				CaseUC:    stubCaseMutator{},
				CaseID:    7,
				StatusSet: testStatusSet(t),
			},
		})
		// Only slack_ro requested (empty) — the writer tools must not leak in.
		gt.Array(t, r.Resolve([]string{agent.ToolSetSlackRO})).Length(0)
	})
}

// fakeTool is a minimal gollem.Tool stand-in used to populate
// ToolSetDeps.Jira in tests without depending on the external jira module.
type fakeTool struct {
	name string
}

func (f *fakeTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{Name: f.name, Description: "fake"}
}

func (f *fakeTool) Run(context.Context, map[string]any) (map[string]any, error) {
	return nil, nil
}

func TestToolSetResolver_ResolveJira(t *testing.T) {
	jiraTools := []gollem.Tool{
		&fakeTool{name: "jira_list_projects"},
		&fakeTool{name: "jira_search_issues"},
		&fakeTool{name: "jira_get_issues"},
	}

	t.Run("jira ID resolves to the configured tools", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{Jira: jiraTools})
		tools := r.Resolve([]string{agent.ToolSetJira})
		gt.Array(t, tools).Length(3).Required()
		gt.String(t, tools[0].Spec().Name).Equal("jira_list_projects")
		gt.String(t, tools[1].Spec().Name).Equal("jira_search_issues")
		gt.String(t, tools[2].Spec().Name).Equal("jira_get_issues")
	})

	t.Run("not requested means not included", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{Jira: jiraTools})
		gt.Array(t, r.Resolve([]string{agent.ToolSetSlackRO})).Length(0)
	})

	t.Run("nil Jira deps yields no tools even when requested", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{})
		gt.Array(t, r.Resolve([]string{agent.ToolSetJira})).Length(0)
	})

	t.Run("combined with core returns both sets", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			Core: core.Deps{WorkspaceID: "ws", CaseID: 1},
			Jira: jiraTools,
		})
		tools := r.Resolve([]string{agent.ToolSetCoreRO, agent.ToolSetJira})
		// core_ro (2) + jira (3).
		gt.Array(t, tools).Length(5)
	})
}

func TestKnownToolSetIDsThreadWrite(t *testing.T) {
	// It is the no-core list plus exactly the case-write id, the two action ids
	// and the memo id, in order.
	want := append(append([]string{}, agent.KnownToolSetIDsNoCore...), agent.ToolSetCaseWrite, agent.ToolSetCoreRO, agent.ToolSetCore, agent.ToolSetMemo)
	gt.Array(t, agent.KnownToolSetIDsThreadWrite).Equal(want)
}

// toolNames extracts the tool names so a test can assert on the exact set the
// agent is handed, rather than on a count that hides which tools moved.
func toolNames(tools []gollem.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Spec().Name)
	}
	sort.Strings(names)
	return names
}

// stubMemoMutator is a no-op memotool.MemoMutator; the resolver tests only
// assert which tools get built, never invoke them.
type stubMemoMutator struct{}

func (stubMemoMutator) CreateMemo(context.Context, string, int64, string, map[string]model.FieldValue) (*model.Memo, error) {
	return &model.Memo{}, nil
}

func (stubMemoMutator) UpdateMemo(context.Context, string, int64, model.MemoID, *string, map[string]model.FieldValue) (*model.Memo, error) {
	return &model.Memo{}, nil
}

func (stubMemoMutator) ArchiveMemo(context.Context, string, int64, model.MemoID) (*model.Memo, error) {
	return &model.Memo{}, nil
}

// stubKnowledgeAccessor / stubKnowledgeMutator are no-op knowledge backends.
type stubKnowledgeAccessor struct{}

func (stubKnowledgeAccessor) SearchKnowledge(context.Context, string, string, []model.TagID, int) ([]*model.Knowledge, error) {
	return nil, nil
}

func (stubKnowledgeAccessor) GetKnowledge(context.Context, string, model.KnowledgeID) (*model.Knowledge, error) {
	return &model.Knowledge{}, nil
}

func (stubKnowledgeAccessor) ListTags(context.Context, string) ([]*model.Tag, error) {
	return nil, nil
}

type stubKnowledgeMutator struct{}

func (stubKnowledgeMutator) CreateTag(context.Context, string, string) (*model.Tag, error) {
	return &model.Tag{}, nil
}

func (stubKnowledgeMutator) UpdateTag(context.Context, string, model.TagID, string) (*model.Tag, error) {
	return &model.Tag{}, nil
}

func (stubKnowledgeMutator) DeleteTag(context.Context, string, model.TagID) error { return nil }

func (stubKnowledgeMutator) CreateKnowledge(context.Context, string, string, string, []model.TagID) (*model.Knowledge, error) {
	return &model.Knowledge{}, nil
}

func (stubKnowledgeMutator) UpdateKnowledge(context.Context, string, model.KnowledgeID, *string, *string, *[]model.TagID) (*model.Knowledge, error) {
	return &model.Knowledge{}, nil
}

// TestToolSetResolver_ResolveCoreFull pins that the full (mutating) action set
// is what ToolSetCore resolves to, and that it is a strict superset of what
// core_ro resolves to.
func TestToolSetResolver_ResolveCoreFull(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		Core: core.Deps{WorkspaceID: "ws", CaseID: 1},
	})

	full := toolNames(r.Resolve([]string{agent.ToolSetCore}))
	readOnly := toolNames(r.Resolve([]string{agent.ToolSetCoreRO}))

	gt.Array(t, readOnly).Equal([]string{"core__get_action", "core__list_actions"})
	gt.Array(t, full).Equal([]string{
		"core__add_action_step",
		"core__archive_action",
		"core__create_action",
		"core__delete_action_step",
		"core__get_action",
		"core__list_action_steps",
		"core__list_actions",
		"core__rename_action_step",
		"core__set_action_assignee",
		"core__set_action_step_done",
		"core__unarchive_action",
		"core__update_action",
		"core__update_action_status",
	})
	for _, name := range readOnly {
		gt.Bool(t, slices.Contains(full, name)).True()
	}
}

// TestToolSetResolver_OmitCoreWithholdsBothCoreSets pins that a thread-mode
// workspace, which manages no Actions, gets neither the read-only nor the
// mutating action tools.
func TestToolSetResolver_OmitCoreWithholdsBothCoreSets(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		OmitCore: true,
		Core:     core.Deps{WorkspaceID: "ws", CaseID: 1},
	})
	gt.Array(t, r.Resolve([]string{agent.ToolSetCore})).Length(0)
	gt.Array(t, r.Resolve([]string{agent.ToolSetCoreRO})).Length(0)
}

// stubCaseRefReader satisfies core.CaseRefReader so the case_ref tools are
// built; the resolver tests never call them.
type stubCaseRefReader struct{ core.CaseRefReader }

// TestToolSetResolver_CoreAccess pins the narrower grants a thread-mode Job
// gets: with [case] actions, core_ro resolves to the action reads plus the
// case_ref lookups; without, to the case_ref lookups alone. Neither writer set
// resolves in either, so the probe filters core / core_job out of the palette.
func TestToolSetResolver_CoreAccess(t *testing.T) {
	deps := core.Deps{WorkspaceID: "ws", CaseID: 1, CaseRefUC: stubCaseRefReader{}}

	cases := []struct {
		name   string
		access agent.CoreAccess
		want   []string
	}{
		{
			name:   "read-only",
			access: agent.CoreAccessReadOnly,
			want: []string{
				"core__get_action", "core__get_referenceable_cases",
				"core__list_actions", "core__search_referenceable_cases",
			},
		},
		{
			name:   "case_ref only",
			access: agent.CoreAccessCaseRefOnly,
			want:   []string{"core__get_referenceable_cases", "core__search_referenceable_cases"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := agent.NewToolSetResolver(agent.ToolSetDeps{CoreAccess: tc.access, Core: deps})
			gt.Array(t, toolNames(r.Resolve([]string{agent.ToolSetCoreRO}))).Equal(tc.want)
			gt.Array(t, r.Resolve([]string{agent.ToolSetCore})).Length(0)
			gt.Array(t, r.Resolve([]string{agent.ToolSetCoreJob})).Length(0)
			gt.Bool(t, r.Has(agent.ToolSetCoreRO)).True()
			gt.Bool(t, r.Has(agent.ToolSetCore)).False()
			gt.Bool(t, r.Has(agent.ToolSetCoreJob)).False()
		})
	}

	t.Run("case_ref only without a reader resolves to nothing", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			CoreAccess: agent.CoreAccessCaseRefOnly,
			Core:       core.Deps{WorkspaceID: "ws", CaseID: 1},
		})
		gt.Bool(t, r.Has(agent.ToolSetCoreRO)).False()
	})

	t.Run("OmitCore takes precedence", func(t *testing.T) {
		both := agent.NewToolSetResolver(agent.ToolSetDeps{
			OmitCore:   true,
			CoreAccess: agent.CoreAccessReadOnly,
			Core:       deps,
		})
		gt.Array(t, both.Resolve([]string{agent.ToolSetCoreRO})).Length(0)
	})
}

// TestToolSetResolver_CoreReadsAreNotOfferedTwice pins that a request naming
// core_ro beside a set that already carries the reads (core, core_job) offers
// each read tool once. The Job palette names core_job and core_ro together.
func TestToolSetResolver_CoreReadsAreNotOfferedTwice(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		Core: core.Deps{WorkspaceID: "ws", CaseID: 1},
	})
	for _, writer := range []string{agent.ToolSetCore, agent.ToolSetCoreJob} {
		alone := toolNames(r.Resolve([]string{writer}))
		withReads := toolNames(r.Resolve([]string{writer, agent.ToolSetCoreRO}))
		gt.Array(t, withReads).Equal(alone)
	}
}

// TestToolSetResolver_Memo pins that memo tools appear only when both the
// mutator and the workspace's memo schema are wired, since the schema drives
// field coercion in create/update.
func TestToolSetResolver_Memo(t *testing.T) {
	t.Run("absent without a mutator", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			Memo: memotool.Deps{WorkspaceID: "ws", CaseID: 1, Schema: &config.FieldSchema{}},
		})
		gt.Array(t, r.Resolve([]string{agent.ToolSetMemo})).Length(0)
	})

	t.Run("absent without a schema", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			Memo: memotool.Deps{WorkspaceID: "ws", CaseID: 1, MemoUC: stubMemoMutator{}},
		})
		gt.Array(t, r.Resolve([]string{agent.ToolSetMemo})).Length(0)
	})

	t.Run("present with both", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{
			Memo: memotool.Deps{
				WorkspaceID: "ws", CaseID: 1,
				MemoUC: stubMemoMutator{}, Schema: &config.FieldSchema{},
			},
		})
		names := toolNames(r.Resolve([]string{agent.ToolSetMemo}))
		gt.Bool(t, len(names) > 0).True()
		for _, name := range names {
			gt.Bool(t, strings.HasPrefix(name, "memo__")).True()
		}
	})
}

// TestToolSetResolver_Knowledge pins the read/write split: the read tools are
// always offered, and requesting the knowledge id REPLACES them with the
// read+write set rather than offering the read tools twice.
func TestToolSetResolver_Knowledge(t *testing.T) {
	deps := knowledgetool.Deps{WorkspaceID: "ws", Accessor: stubKnowledgeAccessor{}}

	t.Run("read tools are offered without being requested", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{Knowledge: deps})
		names := toolNames(r.Resolve([]string{agent.ToolSetSlackRO}))
		gt.Bool(t, len(names) > 0).True()
		for _, name := range names {
			gt.Bool(t, strings.HasPrefix(name, "knowledge__")).True()
		}
	})

	t.Run("without a mutator the knowledge id adds nothing", func(t *testing.T) {
		r := agent.NewToolSetResolver(agent.ToolSetDeps{Knowledge: deps})
		gt.Array(t, toolNames(r.Resolve([]string{agent.ToolSetKnowledge}))).
			Equal(toolNames(r.Resolve(nil)))
	})

	t.Run("with a mutator the knowledge id widens the set without duplicating", func(t *testing.T) {
		writable := deps
		writable.Mutator = stubKnowledgeMutator{}
		r := agent.NewToolSetResolver(agent.ToolSetDeps{Knowledge: writable})

		readOnly := toolNames(r.Resolve(nil))
		full := toolNames(r.Resolve([]string{agent.ToolSetKnowledge}))

		gt.Bool(t, len(full) > len(readOnly)).True()
		gt.Array(t, full).Equal(slices.Compact(slices.Clone(full)))
		for _, name := range readOnly {
			gt.Bool(t, slices.Contains(full, name)).True()
		}
	})
}

func TestKnownToolSetIDsCaseChannel(t *testing.T) {
	gt.Array(t, agent.KnownToolSetIDsCaseChannel).Equal([]string{
		agent.ToolSetCore,
		agent.ToolSetSlackRO,
		agent.ToolSetNotion,
		agent.ToolSetGitHub,
		agent.ToolSetWebFetch,
		agent.ToolSetJira,
		agent.ToolSetCaseWrite,
		agent.ToolSetMemo,
		agent.ToolSetKnowledge,
	})
}

// allToolSetIDs lists every ToolSet* constant. A constant added to toolset.go
// belongs here too, which is what makes
// TestReadOnlyToolSetIDs_ClassifiesEveryToolSet catch an unclassified one.
var allToolSetIDs = []string{
	agent.ToolSetCoreRO,
	agent.ToolSetSlackRO,
	agent.ToolSetNotion,
	agent.ToolSetGitHub,
	agent.ToolSetWebFetch,
	agent.ToolSetJira,
	agent.ToolSetCore,
	agent.ToolSetMemo,
	agent.ToolSetKnowledge,
	agent.ToolSetCaseWrite,
	agent.ToolSetCaseMulti,
	agent.ToolSetSlackWrite,
	agent.ToolSetCoreJob,
	agent.ToolSetSlackPost,
	agent.ToolSetWSMeta,
	agent.ToolSetMemoRO,
	agent.ToolSetCaseMultiRO,
}

func TestReadOnlyToolSetIDs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"core sets keep their reads", []string{agent.ToolSetCore}, []string{agent.ToolSetCoreRO}},
		{"job core set keeps its reads", []string{agent.ToolSetCoreJob}, []string{agent.ToolSetCoreRO}},
		{"core reads stay", []string{agent.ToolSetCoreRO}, []string{agent.ToolSetCoreRO}},
		{"slack write keeps its reads", []string{agent.ToolSetSlackWrite}, []string{agent.ToolSetSlackRO}},
		{"slack reads stay", []string{agent.ToolSetSlackRO}, []string{agent.ToolSetSlackRO}},
		{"memo keeps its reads", []string{agent.ToolSetMemo}, []string{agent.ToolSetMemoRO}},
		{"memo reads stay", []string{agent.ToolSetMemoRO}, []string{agent.ToolSetMemoRO}},
		{"case_multi keeps its reads", []string{agent.ToolSetCaseMulti}, []string{agent.ToolSetCaseMultiRO}},
		{"case_multi reads stay", []string{agent.ToolSetCaseMultiRO}, []string{agent.ToolSetCaseMultiRO}},
		{
			"read-only integrations stay",
			[]string{agent.ToolSetNotion, agent.ToolSetGitHub, agent.ToolSetWebFetch, agent.ToolSetJira, agent.ToolSetWSMeta},
			[]string{agent.ToolSetNotion, agent.ToolSetGitHub, agent.ToolSetWebFetch, agent.ToolSetJira, agent.ToolSetWSMeta},
		},
		{"write-only sets are dropped", []string{agent.ToolSetSlackPost, agent.ToolSetCaseWrite}, []string{}},
		{"knowledge is dropped", []string{agent.ToolSetKnowledge}, []string{}},
		{
			"the job palette keeps order and drops duplicates",
			agent.KnownToolSetIDsJob,
			[]string{
				agent.ToolSetCoreRO, agent.ToolSetSlackRO, agent.ToolSetNotion,
				agent.ToolSetWebFetch, agent.ToolSetJira, agent.ToolSetMemoRO,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agent.ReadOnlyToolSetIDs(tc.in)
			gt.NoError(t, err).Required()
			gt.Array(t, got).Equal(tc.want)
		})
	}
}

func TestReadOnlyToolSetIDs_ClassifiesEveryToolSet(t *testing.T) {
	for _, id := range allToolSetIDs {
		t.Run(id, func(t *testing.T) {
			_, err := agent.ReadOnlyToolSetIDs([]string{id})
			gt.NoError(t, err)
		})
	}
}

func TestReadOnlyToolSetIDs_RejectsAnUnclassifiedID(t *testing.T) {
	_, err := agent.ReadOnlyToolSetIDs([]string{agent.ToolSetSlackRO, "not_a_toolset"})
	gt.Error(t, err)
}

// stubCaseMultiUC satisfies casemulti.CaseUsecase; the resolver tests never
// call it.
type stubCaseMultiUC struct{ casemulti.CaseUsecase }

type stubCaseMultiActionUC struct{ casemulti.ActionUsecase }

// TestReadOnlyToolSets_ResolveNoWriter pins that the read halves of memo and
// case_multi resolve to the read tools of the full sets and to nothing else.
func TestReadOnlyToolSets_ResolveNoWriter(t *testing.T) {
	r := agent.NewToolSetResolver(agent.ToolSetDeps{
		Memo: memotool.Deps{
			WorkspaceID: "ws", CaseID: 1,
			MemoUC: stubMemoMutator{}, Schema: &config.FieldSchema{},
		},
		CaseMulti: casemulti.Deps{
			WorkspaceID: "ws", ActorID: "U1",
			CaseUC: stubCaseMultiUC{}, ActionUC: stubCaseMultiActionUC{},
		},
	})

	gt.Array(t, toolNames(r.Resolve([]string{agent.ToolSetMemoRO}))).
		Equal([]string{"memo__get_memo", "memo__list_memos"})
	gt.Array(t, toolNames(r.Resolve([]string{agent.ToolSetCaseMultiRO}))).
		Equal([]string{"case__get_action", "case__get_case", "case__list_actions", "case__list_cases"})

	// The full sets carry the writers the read halves leave out.
	gt.Array(t, toolNames(r.Resolve([]string{agent.ToolSetMemo}))).Has("memo__apply_memo_changes")
	gt.Array(t, toolNames(r.Resolve([]string{agent.ToolSetCaseMulti}))).Has("case__update_case")

	gt.Bool(t, r.Has(agent.ToolSetMemoRO)).True()
	gt.Bool(t, r.Has(agent.ToolSetCaseMultiRO)).True()
}

func TestReadOnlyToolSets_AreNotOfferedToPlanners(t *testing.T) {
	palettes := [][]string{
		agent.KnownToolSetIDs,
		agent.KnownToolSetIDsNoCore,
		agent.KnownToolSetIDsThreadWrite,
		agent.KnownToolSetIDsWorkspaceChannel,
		agent.KnownToolSetIDsCaseChannel,
		agent.KnownToolSetIDsAssist,
		agent.KnownToolSetIDsJob,
		agent.KnownToolSetIDsProposal,
	}
	for _, palette := range palettes {
		gt.Bool(t, slices.Contains(palette, agent.ToolSetMemoRO)).False()
		gt.Bool(t, slices.Contains(palette, agent.ToolSetCaseMultiRO)).False()
	}
}
