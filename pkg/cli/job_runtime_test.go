package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/gt"

	urfavecli "github.com/urfave/cli/v3"

	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/cli"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
)

// The fakes below stub their interfaces by embedding them (nil), so any method
// call would panic. buildJobTools only constructs the tool structs, so the
// methods are never invoked — a non-nil fake is enough to prove the tool is
// wired in.
type fakeJobBot struct{ slacksvc.Service }
type fakeJobSearch struct{ slacktool.SearchService }
type fakeJobRetriever struct{ slacktool.MessageRetriever }
type fakeJobNotion struct{ notiontool.Client }

// fakeJiraTool is a minimal gollem.Tool stand-in used to populate
// JobReadToolDepsForTest.Jira without depending on the external jira module.
type fakeJiraTool struct{ name string }

func (f *fakeJiraTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{Name: f.name, Description: "fake"}
}

func (f *fakeJiraTool) Run(context.Context, map[string]any) (map[string]any, error) {
	return nil, nil
}

func fakeJiraTools() []gollem.Tool {
	return []gollem.Tool{
		&fakeJiraTool{name: "jira_list_projects"},
		&fakeJiraTool{name: "jira_search_issues"},
		&fakeJiraTool{name: "jira_get_issues"},
	}
}

// A deployment with an agent runtime registers no in-process executor: every
// strategy is Spawned onto the runtime instead.
func TestInProcessExecutors_NoneWithAnAgentRuntime(t *testing.T) {
	got := cli.InProcessExecutorsForTest(&job.DurableRuntime{})
	gt.Number(t, len(got)).Equal(0)
}

// A deployment WITHOUT an agent runtime — no LLM configured, so no Kernel is built
// — must still get the single-loop executor.
//
// It is not there to run an agent: with no model the run fails immediately. It is
// there so the run is RECORDED. `Run` only reaches the stage that writes the run log
// and the FAILED outcome by going through an executor; with none registered the
// attempt aborts in the prepare stage and the run history shows nothing at all,
// which is what the manual-run e2e caught.
func TestInProcessExecutors_SingleLoopWithoutAnAgentRuntime(t *testing.T) {
	got := cli.InProcessExecutorsForTest(nil)
	gt.Number(t, len(got)).Equal(1)
	gt.Value(t, got[model.JobStrategySimple]).NotNil()
}

func toolNames(tools []gollem.Tool) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		names[t.Spec().Name] = true
	}
	return names
}

func hasActionTool(tools []gollem.Tool) bool {
	for _, t := range tools {
		// Both the read-only (core__list_actions / core__get_action) and writer
		// (core__create_action, ...) action tools share the core__ prefix.
		if strings.HasPrefix(t.Spec().Name, "core__") {
			return true
		}
	}
	return false
}

func TestBuildJobTools_ChannelModeHasActionTools(t *testing.T) {
	ws := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}}
	c := &model.Case{ID: 1, SlackChannelID: "C1"}

	tools := cli.BuildJobToolsForTest(c, ws)

	gt.Bool(t, hasActionTool(tools)).True()
	names := toolNames(tools)
	gt.Bool(t, names["core__list_actions"]).True()
	gt.Bool(t, names["core__create_action"]).True()
	// Case-editing tools are present in both modes.
	gt.Bool(t, len(names) > 0).True()
	gt.Bool(t, hasCaseTool(tools)).True()
}

func TestBuildJobTools_ThreadModeOmitsActionTools(t *testing.T) {
	ws := &model.WorkspaceEntry{
		Workspace: model.Workspace{ID: "ws"},
		CaseMode:  model.CaseModeThread,
	}
	// Thread-mode case binds a thread, not a dedicated channel.
	c := &model.Case{ID: 1, SlackChannelID: "C-MONITOR", SlackThreadTS: "1700000000.000100"}

	tools := cli.BuildJobToolsForTest(c, ws)

	// No action tools at all — thread-mode workspaces manage no Actions.
	gt.Bool(t, hasActionTool(tools)).False()
	// Case-editing tools (incl. board status) remain available.
	gt.Bool(t, hasCaseTool(tools)).True()
}

// A thread-mode workspace that enables [case] actions gives its Jobs the action
// READS only: the Job may consult the case's Actions but never writes them.
func TestBuildJobTools_ThreadModeWithActionsHasReadOnlyActionTools(t *testing.T) {
	ws := &model.WorkspaceEntry{
		Workspace:   model.Workspace{ID: "ws"},
		CaseMode:    model.CaseModeThread,
		CaseActions: true,
	}
	c := &model.Case{ID: 1, SlackChannelID: "C-MONITOR", SlackThreadTS: "1700000000.000100"}

	names := toolNames(cli.BuildJobToolsForTest(c, ws))

	gt.Bool(t, names["core__list_actions"]).True()
	gt.Bool(t, names["core__get_action"]).True()
	for _, writer := range []string{
		"core__create_action", "core__update_action", "core__update_action_status",
		"core__set_action_assignee", "core__archive_action", "core__add_action_step",
	} {
		gt.Bool(t, names[writer]).False()
	}
	gt.Bool(t, hasCaseTool(cli.BuildJobToolsForTest(c, ws))).True()
}

func hasCaseTool(tools []gollem.Tool) bool {
	for _, t := range tools {
		if strings.HasPrefix(t.Spec().Name, "case__") {
			return true
		}
	}
	return false
}

// readToolNames are the read-only Slack / Notion / Jira tools the Job agent
// uses to read its case thread and corroborate findings.
var readToolNames = []string{
	"slack__get_messages",
	"slack__search_messages",
	"notion__search",
	"notion__get_page",
	"notion__get_database",
	"jira_list_projects",
	"jira_search_issues",
	"jira_get_issues",
}

func TestBuildJobTools_ReadToolsWiredInBothModes(t *testing.T) {
	deps := cli.JobReadToolDepsForTest{
		Bot:       &fakeJobBot{},
		Search:    &fakeJobSearch{},
		Retriever: &fakeJobRetriever{},
		Notion:    &fakeJobNotion{},
		Jira:      fakeJiraTools(),
	}

	cases := []struct {
		name string
		ws   *model.WorkspaceEntry
		c    *model.Case
	}{
		{
			name: "channel-mode",
			ws:   &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}},
			c:    &model.Case{ID: 1, SlackChannelID: "C1"},
		},
		{
			name: "thread-mode",
			ws:   &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}, CaseMode: model.CaseModeThread},
			c:    &model.Case{ID: 1, SlackChannelID: "C-MONITOR", SlackThreadTS: "1700000000.000100"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := cli.BuildJobToolsWithReadDepsForTest(deps, tc.c, tc.ws)
			names := toolNames(tools)
			for _, n := range readToolNames {
				gt.Bool(t, names[n]).True()
			}
			// The post tool (separate slackpost package) stays wired alongside
			// the read tools; NewReadOnly must not have displaced it.
			gt.Bool(t, names["slack__post_to_case_channel"]).True()
			// Read tools are not Action tools, so thread-mode still forgoes
			// the core__ action tools.
			if tc.ws.IsThreadMode() {
				gt.Bool(t, hasActionTool(tools)).False()
			}
		})
	}
}

func TestBuildJobTools_ReadToolsOmittedWhenDepsNil(t *testing.T) {
	// Zero deps: every read dependency is nil, so none of the read tools bind.
	ws := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}}
	c := &model.Case{ID: 1, SlackChannelID: "C1"}

	names := toolNames(cli.BuildJobToolsWithReadDepsForTest(cli.JobReadToolDepsForTest{}, c, ws))
	for _, n := range readToolNames {
		gt.Bool(t, names[n]).False()
	}
	// With a nil Bot, the post tool is also absent (it shares the Bot dep).
	gt.Bool(t, names["slack__post_to_case_channel"]).False()
}

func TestBuildJobTools_GetMessagesNeedsBotButSearchIsIndependent(t *testing.T) {
	ws := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}}
	c := &model.Case{ID: 1, SlackChannelID: "C1"}

	// Search wired but Bot nil: search_messages binds, get_messages does not.
	names := toolNames(cli.BuildJobToolsWithReadDepsForTest(
		cli.JobReadToolDepsForTest{Search: &fakeJobSearch{}}, c, ws))
	gt.Bool(t, names["slack__search_messages"]).True()
	gt.Bool(t, names["slack__get_messages"]).False()

	// Bot wired but Search nil: get_messages binds, search_messages does not.
	names = toolNames(cli.BuildJobToolsWithReadDepsForTest(
		cli.JobReadToolDepsForTest{Bot: &fakeJobBot{}}, c, ws))
	gt.Bool(t, names["slack__get_messages"]).True()
	gt.Bool(t, names["slack__search_messages"]).False()
}

func TestBuildJobTools_JiraOnlyWiredWhenToolsProvided(t *testing.T) {
	ws := &model.WorkspaceEntry{Workspace: model.Workspace{ID: "ws"}}
	c := &model.Case{ID: 1, SlackChannelID: "C1"}

	names := toolNames(cli.BuildJobToolsWithReadDepsForTest(
		cli.JobReadToolDepsForTest{Jira: fakeJiraTools()}, c, ws))
	gt.Bool(t, names["jira_list_projects"]).True()
	gt.Bool(t, names["jira_search_issues"]).True()
	gt.Bool(t, names["jira_get_issues"]).True()

	names = toolNames(cli.BuildJobToolsWithReadDepsForTest(cli.JobReadToolDepsForTest{}, c, ws))
	gt.Bool(t, names["jira_list_projects"]).False()
	gt.Bool(t, names["jira_search_issues"]).False()
	gt.Bool(t, names["jira_get_issues"]).False()
}

func TestRegistryHasInteractiveJob(t *testing.T) {
	mkJob := func(id string, interactive, disabled bool) *model.Job {
		return &model.Job{
			ID:          id,
			Prompt:      "x",
			Strategy:    model.JobStrategyPlanexec,
			Interactive: interactive,
			Disabled:    disabled,
			Events: model.JobEvents{
				Case: &model.CaseEventConfig{On: []model.CaseLifecycle{model.CaseLifecycleCreated}},
			},
		}
	}

	t.Run("nil registry", func(t *testing.T) {
		gt.Bool(t, cli.RegistryHasInteractiveJobForTest(nil)).False()
	})

	t.Run("no interactive job", func(t *testing.T) {
		reg := model.NewWorkspaceRegistry()
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws"},
			Jobs:      []*model.Job{mkJob("a", false, false)},
		})
		gt.Bool(t, cli.RegistryHasInteractiveJobForTest(reg)).False()
	})

	t.Run("enabled interactive job", func(t *testing.T) {
		reg := model.NewWorkspaceRegistry()
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws"},
			Jobs:      []*model.Job{mkJob("a", false, false), mkJob("b", true, false)},
		})
		gt.Bool(t, cli.RegistryHasInteractiveJobForTest(reg)).True()
	})

	t.Run("disabled interactive job does not count", func(t *testing.T) {
		reg := model.NewWorkspaceRegistry()
		reg.Register(&model.WorkspaceEntry{
			Workspace: model.Workspace{ID: "ws"},
			Jobs:      []*model.Job{mkJob("b", true, true)},
		})
		gt.Bool(t, cli.RegistryHasInteractiveJobForTest(reg)).False()
	})
}

// --- configureTickIntegrations -------------------------------------------

// slackConfigFromEnv populates a config.Slack the way the process actually does
// — by parsing its own flags — so the test also pins that the sweep reads the
// same environment variables serve does.
func slackConfigFromEnv(t *testing.T, botToken, userToken string) *config.Slack {
	t.Helper()
	t.Setenv("HECATONCHEIRES_SLACK_BOT_TOKEN", botToken)
	t.Setenv("HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN", userToken)

	var cfg config.Slack
	cmd := &urfavecli.Command{
		Name:   "tick",
		Flags:  cfg.RuntimeFlags(),
		Action: func(context.Context, *urfavecli.Command) error { return nil },
	}
	gt.NoError(t, cmd.Run(context.Background(), []string{"tick"})).Required()
	gt.String(t, cfg.BotToken()).Equal(botToken)
	return &cfg
}

// Every config pointer is required: a sweep executes the runs it dispatches, so
// omitting one silently leaves a toolset the Job palette advertises resolving to
// nothing.
func TestTickIntegrationConfigs_Validate(t *testing.T) {
	full := func() cli.TickIntegrationConfigsForTest {
		return cli.TickIntegrationConfigsForTest{
			Slack:     &config.Slack{},
			SlackTool: &config.SlackTool{},
			Jira:      &config.Jira{},
			WebFetch:  &config.WebFetch{},
		}
	}
	gt.NoError(t, full().Validate())

	testCases := map[string]func(*cli.TickIntegrationConfigsForTest){
		"slack":      func(c *cli.TickIntegrationConfigsForTest) { c.Slack = nil },
		"slack tool": func(c *cli.TickIntegrationConfigsForTest) { c.SlackTool = nil },
		"jira":       func(c *cli.TickIntegrationConfigsForTest) { c.Jira = nil },
		"webfetch":   func(c *cli.TickIntegrationConfigsForTest) { c.WebFetch = nil },
	}
	for name, drop := range testCases {
		t.Run("missing "+name, func(t *testing.T) {
			cfg := full()
			drop(&cfg)
			gt.Error(t, cfg.Validate())
		})
	}
}

// With nothing configured the sweep must still come up — every client stays nil
// and its tool constructor binds nothing — but the base URL is applied so a
// scheduled run's Slack messages link back the way serve's do.
func TestConfigureTickIntegrations_UnconfiguredLeavesClientsNil(t *testing.T) {
	slackSvc, jiraTools, opts, err := cli.ConfigureTickIntegrationsForTest(
		context.Background(), cli.TickIntegrationConfigsForTest{
			Slack:     &config.Slack{},
			Jira:      &config.Jira{},
			WebFetch:  &config.WebFetch{},
			SlackTool: &config.SlackTool{},
			BaseURL:   "https://hecatoncheires.example.com",
		})
	gt.NoError(t, err).Required()
	gt.Value(t, slackSvc).Nil()
	gt.Array(t, jiraTools).Length(0)

	uc := usecase.New(memory.New(), model.NewWorkspaceRegistry(), opts...)
	gt.Value(t, uc.SlackService()).Nil()
	gt.Value(t, uc.SlackSearchService()).Nil()
	gt.Value(t, uc.SlackMessageRetriever()).Nil()
	gt.Value(t, uc.NotionToolClient()).Nil()
	gt.Value(t, uc.GitHubToolClient()).Nil()
	gt.Value(t, uc.WebFetchClient()).Nil()
	gt.String(t, uc.Case.CaseURL("ws-1", 7)).Equal("https://hecatoncheires.example.com/ws/ws-1/cases/7")
}

// A Slack bot token is what makes the whole Slack surface exist for a sweep: the
// service the runner reports through, the read tools, and the poster
// slack__post_to_case_channel is built from.
func TestConfigureTickIntegrations_SlackBotTokenWiresTheService(t *testing.T) {
	slackCfg := slackConfigFromEnv(t, "xoxb-test-token", "")
	slackSvc, _, opts, err := cli.ConfigureTickIntegrationsForTest(
		context.Background(), cli.TickIntegrationConfigsForTest{
			Slack:     slackCfg,
			Jira:      &config.Jira{},
			WebFetch:  &config.WebFetch{},
			SlackTool: &config.SlackTool{},
		})
	gt.NoError(t, err).Required()
	gt.Value(t, slackSvc).NotNil()

	// usecase.New refuses a Slack service with no LLM client, which is the same
	// invariant buildTickRuntime reports as an error before getting here.
	opts = append(opts, usecase.WithLLMClient(&mock.LLMClientMock{}))
	uc := usecase.New(memory.New(), model.NewWorkspaceRegistry(), opts...)
	gt.Value(t, uc.SlackService()).NotNil()
	// Non-nil is what the tool factory checks, so the poster must materialise.
	gt.Value(t, uc.AgentToolDeps().SlackPoster).NotNil()
	// No User OAuth token, so the two User-token-backed read clients stay unset.
	gt.Value(t, uc.SlackSearchService()).Nil()
	gt.Value(t, uc.SlackMessageRetriever()).Nil()
}

// The User OAuth token is the only token search.messages accepts, and what lets
// the message reader reach public channels the bot has not joined.
func TestConfigureTickIntegrations_UserTokenWiresTheReadClients(t *testing.T) {
	slackCfg := slackConfigFromEnv(t, "xoxb-test-token", "xoxp-test-token")
	_, _, opts, err := cli.ConfigureTickIntegrationsForTest(
		context.Background(), cli.TickIntegrationConfigsForTest{
			Slack:     slackCfg,
			Jira:      &config.Jira{},
			WebFetch:  &config.WebFetch{},
			SlackTool: &config.SlackTool{},
		})
	gt.NoError(t, err).Required()

	opts = append(opts, usecase.WithLLMClient(&mock.LLMClientMock{}))
	uc := usecase.New(memory.New(), model.NewWorkspaceRegistry(), opts...)
	gt.Value(t, uc.SlackSearchService()).NotNil()
	gt.Value(t, uc.SlackMessageRetriever()).NotNil()
}

// The Notion token backs the notion__* agent tools; without it the `notion`
// toolset the Job palette advertises resolves to nothing.
func TestConfigureTickIntegrations_NotionTokenWiresTheToolClient(t *testing.T) {
	_, _, opts, err := cli.ConfigureTickIntegrationsForTest(
		context.Background(), cli.TickIntegrationConfigsForTest{
			Slack:       &config.Slack{},
			SlackTool:   &config.SlackTool{},
			Jira:        &config.Jira{},
			WebFetch:    &config.WebFetch{},
			NotionToken: "secret_test_token",
		})
	gt.NoError(t, err).Required()

	uc := usecase.New(memory.New(), model.NewWorkspaceRegistry(), opts...)
	gt.Value(t, uc.NotionToolClient()).NotNil()
}
