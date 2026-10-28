package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"
	"github.com/vektah/gqlparser/v2/gqlerror"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	gqlctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/graphql"
	httpctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/http"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/agentarchive"
	"github.com/m-mizutani/hecatoncheires/pkg/service/notion"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/service/worker"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/agent/proposal"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase/job"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/errutil"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/urfave/cli/v3"
)

// logAttrsToArgs converts slog.Attr slice to slog.Logger compatible args
func logAttrsToArgs(attrs []slog.Attr) []any {
	args := make([]any, 0, len(attrs)*2)
	for _, a := range attrs {
		args = append(args, a.Key, a.Value.Any())
	}
	return args
}

// graphqlErrorStatusMiddleware maps GraphQL error responses to an appropriate
// HTTP status. The ErrorPresenter tags client-faulted errors (validation,
// not-found, access-denied) with extensions.code; this middleware reads those
// codes and returns 4xx for them. Genuine server faults stay 5xx.
func graphqlErrorStatusMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{
			ResponseWriter: w,
			body:           &bytes.Buffer{},
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		var gqlResp struct {
			Errors []gqlErrorEnvelope `json:"errors"`
		}

		if err := json.Unmarshal(rec.body.Bytes(), &gqlResp); err == nil && len(gqlResp.Errors) > 0 {
			w.WriteHeader(httpStatusForGraphQLErrors(gqlResp.Errors))
		} else {
			w.WriteHeader(rec.statusCode)
		}

		_, _ = w.Write(rec.body.Bytes())
	})
}

// gqlErrorEnvelope is the shape we care about in a GraphQL error response —
// just enough to read extensions.code for HTTP status mapping.
type gqlErrorEnvelope struct {
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// httpStatusForGraphQLErrors picks the worst (most-server-faulty) HTTP status
// across all errors. If every error is a tagged client error we return its
// 4xx; otherwise we fall back to 500.
func httpStatusForGraphQLErrors(errs []gqlErrorEnvelope) int {
	worst := 0
	for _, e := range errs {
		s := statusForExtensionCode(e.Extensions.Code)
		if s == 0 {
			return http.StatusInternalServerError
		}
		if s > worst {
			worst = s
		}
	}
	if worst == 0 {
		return http.StatusInternalServerError
	}
	return worst
}

// classifyError maps a domain/usecase error to a GraphQL extensions.code.
// Returning "" leaves the error untagged, which the HTTP middleware treats
// as a server fault (500). Delegates to the GraphQL controller's shared
// classifier so both the ErrorPresenter (extensions.code) and the HTTP
// middleware (status mapping) see the same answer.
func classifyError(err error) string {
	return gqlctrl.ErrorCode(err)
}

func isClientError(err error) bool {
	return gqlctrl.IsClientError(err)
}

// Bounds of the per-instance workspace access decision cache: a decision lives
// for a minute, and at most this many users' decisions are kept.
const (
	workspaceAccessCacheTTL  = time.Minute
	workspaceAccessCacheSize = 4096
)

// buildWorkspaceAccess builds the per-workspace authorizer from every
// workspace config that carries an [authz] policy.
func buildWorkspaceAccess(registry *model.WorkspaceRegistry, configs []*config.WorkspaceConfig, users interfaces.SlackUserRepository) (*usecase.WorkspaceAccessUseCase, error) {
	policies := make(map[string]interfaces.PolicyClient)
	for _, wc := range configs {
		if wc.AuthzPolicy != nil {
			policies[wc.ID] = wc.AuthzPolicy
		}
	}
	access, err := usecase.NewWorkspaceAccessUseCase(registry, policies, users, usecase.WorkspaceAccessCacheConfig{
		TTL:  workspaceAccessCacheTTL,
		Size: workspaceAccessCacheSize,
	})
	if err != nil {
		return nil, goerr.Wrap(err, "failed to build workspace access control")
	}
	return access, nil
}

func statusForExtensionCode(code string) int {
	switch code {
	case gqlctrl.ErrCodeBadUserInput,
		gqlctrl.ErrCodeMissingRequiredFields,
		gqlctrl.ErrCodeTitleRequired,
		gqlctrl.ErrCodeFieldValidationFailed,
		gqlctrl.ErrCodeSlackChannelNotLinkable:
		return http.StatusBadRequest
	case gqlctrl.ErrCodeNotFound:
		return http.StatusNotFound
	case gqlctrl.ErrCodeForbidden:
		return http.StatusForbidden
	case gqlctrl.ErrCodeConflict,
		gqlctrl.ErrCodeInvalidStatusTransition,
		gqlctrl.ErrCodeSlackChannelLinked:
		return http.StatusConflict
	case gqlctrl.ErrCodeUnauthenticated:
		return http.StatusUnauthorized
	default:
		return 0
	}
}

// responseRecorder captures HTTP responses for inspection
type responseRecorder struct {
	http.ResponseWriter
	body       *bytes.Buffer
	statusCode int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	// Don't write header yet, we'll do it later after inspecting the body
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

func cmdServe() *cli.Command {
	var addr string
	var baseURL string
	var enableGraphiQL bool
	var notionToken string
	var noAuthUID string
	var defaultLangStr string
	var appCfg config.AppConfig
	var repoCfg config.Repository
	var slackCfg config.Slack
	var slackToolCfg config.SlackTool
	var llmCfg config.LLM
	var embCfg config.Embedding
	var homeMsgCfg config.HomeMessageLLM
	var dashboardStaleThreshold time.Duration
	var githubCfg config.GitHub
	var jiraCfg config.Jira
	var webfetchCfg config.WebFetch
	var storageCfg config.Storage
	var sentryCfg config.Sentry
	var mcpCfg config.MCP
	var jobCfg config.JobConcurrency
	var agentCfg config.Agent

	flags := []cli.Flag{
		&cli.StringFlag{
			Name:        "addr",
			Usage:       "HTTP server address",
			Value:       ":8080",
			Sources:     cli.EnvVars("HECATONCHEIRES_ADDR"),
			Destination: &addr,
		},
		&cli.StringFlag{
			Name:        "base-url",
			Usage:       "Base URL for the application (e.g., https://your-domain.com)",
			Sources:     cli.EnvVars("HECATONCHEIRES_BASE_URL"),
			Destination: &baseURL,
		},
		&cli.BoolFlag{
			Name:        "graphiql",
			Usage:       "Enable GraphiQL playground",
			Value:       true,
			Sources:     cli.EnvVars("HECATONCHEIRES_GRAPHIQL"),
			Destination: &enableGraphiQL,
		},
		&cli.StringFlag{
			Name:        "notion-api-token",
			Usage:       "Notion API token for Source integration",
			Sources:     cli.EnvVars("HECATONCHEIRES_NOTION_API_TOKEN"),
			Destination: &notionToken,
		},
		&cli.StringFlag{
			Name:        "no-auth",
			Usage:       "Skip authentication and run as specified Slack user ID (development only). With --slack-bot-token the ID is validated against Slack; without it a placeholder test user is used. Example: --no-auth=U1234567890",
			Category:    "Authentication",
			Sources:     cli.EnvVars("HECATONCHEIRES_NO_AUTH"),
			Destination: &noAuthUID,
		},
		&cli.StringFlag{
			Name:        "default-lang",
			Usage:       "Default language for UI and Slack messages (en, ja)",
			Value:       "en",
			Sources:     cli.EnvVars("HECATONCHEIRES_DEFAULT_LANG"),
			Destination: &defaultLangStr,
		},
		&cli.DurationFlag{
			Name:        "dashboard-stale-threshold",
			Usage:       "Age after which an open Case with no update is flagged as stalled on the home dashboard (0 disables)",
			Value:       14 * 24 * time.Hour,
			Sources:     cli.EnvVars("HECATONCHEIRES_DASHBOARD_STALE_THRESHOLD"),
			Destination: &dashboardStaleThreshold,
		},
	}

	// Add shared config flags
	flags = append(flags, appCfg.Flags()...)
	flags = append(flags, repoCfg.Flags()...)
	flags = append(flags, slackCfg.Flags()...)
	flags = append(flags, slackToolCfg.Flags()...)
	flags = append(flags, llmCfg.Flags()...)
	flags = append(flags, embCfg.Flags()...)
	flags = append(flags, homeMsgCfg.Flags()...)
	flags = append(flags, githubCfg.Flags()...)
	flags = append(flags, jiraCfg.Flags()...)
	flags = append(flags, webfetchCfg.Flags()...)
	flags = append(flags, storageCfg.Flags()...)
	flags = append(flags, sentryCfg.Flags()...)
	flags = append(flags, mcpCfg.Flags()...)
	flags = append(flags, jobCfg.Flags()...)
	flags = append(flags, agentCfg.Flags()...)

	return &cli.Command{
		Name:    "serve",
		Aliases: []string{"s"},
		Usage:   "Start HTTP server",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			// Initialize Sentry as early as possible so subsequent failures
			// (config load, repo init, etc.) can be reported. Sentry init
			// errors do not abort startup — losing observability is
			// strictly better than refusing to serve.
			sentryCfg.Configure(ctx)
			defer errutil.FlushSentry(2 * time.Second)

			if err := jobCfg.Validate(); err != nil {
				return goerr.Wrap(err, "invalid job concurrency configuration")
			}

			// Load workspace configurations and build registry
			workspaceConfigs, registry, err := appCfg.Configure(c)
			if err != nil {
				return goerr.Wrap(err, "failed to load workspace configurations")
			}

			// Load deployment-wide workspace groups (--global-config). Members
			// are cross-checked against the workspace registry above; an unset
			// flag leaves an empty (dormant) registry.
			groupRegistry, err := appCfg.ConfigureGroups(c, registry)
			if err != nil {
				return goerr.Wrap(err, "failed to load workspace group configurations")
			}

			// Initialize repository based on backend type
			repo, err := repoCfg.Configure(ctx)
			if err != nil {
				return goerr.Wrap(err, "failed to initialize repository")
			}
			defer func() {
				if err := repo.Close(); err != nil {
					errutil.Handle(ctx, goerr.Wrap(err, "failed to close repository"), "failed to close repository")
				}
			}()

			// Set no-auth UID if provided
			if noAuthUID != "" {
				slackCfg.SetNoAuthUID(noAuthUID)
			}

			// Configure authentication
			authUC, err := slackCfg.Configure(ctx, repo, baseURL)
			if err != nil {
				return goerr.Wrap(err, "failed to configure authentication")
			}

			if slackCfg.IsNoAuthMode() {
				logging.Default().Warn("Running in no-auth mode (development only)", "user_id", noAuthUID)
			} else if slackCfg.IsConfigured() {
				logging.Default().Info("Slack authentication enabled", logAttrsToArgs(slackCfg.LogAttrs())...)
			}

			// Parse and initialize translator
			defaultLang, err := i18n.ParseLang(defaultLangStr)
			if err != nil {
				return goerr.Wrap(err, "invalid default-lang value")
			}
			i18n.Init(defaultLang)
			logging.Default().Info("i18n initialized", "default_lang", string(defaultLang))

			// Initialize use cases with configuration and auth
			ucOpts := []usecase.Option{
				usecase.WithAuth(authUC),
				usecase.WithBaseURL(baseURL),
			}

			// Initialize Notion services if token is provided. Two clients are
			// constructed off the same token: pkg/service/notion drives
			// Source/Compile, and pkg/agent/tool/notion drives the agent's
			// notion__search / notion__get_page / notion__get_database tools.
			if notionToken != "" {
				notionSvc, err := notion.New(notionToken)
				if err != nil {
					return goerr.Wrap(err, "failed to initialize notion service")
				}
				ucOpts = append(ucOpts, usecase.WithNotion(notionSvc))

				notionToolClient, err := notiontool.NewClient(notionToken)
				if err != nil {
					return goerr.Wrap(err, "failed to initialize notion tool client")
				}
				ucOpts = append(ucOpts, usecase.WithNotionToolClient(notionToolClient))

				logging.Default().Info("Notion service enabled")
			} else {
				logging.Default().Info("Notion API token not configured, Source features and Notion agent tools disabled")
			}

			// The Slack read tools' size bounds are installed unconditionally.
			// They cost nothing when no Slack tool is wired, and putting them
			// inside the bot-token branch below would make the flag silently
			// inert in a deployment that reaches Slack only through the User
			// OAuth Token.
			if err := slackToolCfg.Validate(); err != nil {
				return goerr.Wrap(err, "invalid Slack agent tool configuration")
			}
			ucOpts = append(ucOpts, usecase.WithSlackToolLimits(slackToolCfg.Limits()))
			logging.Default().Info("Slack agent tool limits configured", logAttrsToArgs(slackToolCfg.LogAttrs())...)

			// Initialize Slack service for Source integration if bot token is provided
			var slackSvc slack.Service
			if slackCfg.BotToken() != "" {
				svc, err := slack.New(slackCfg.BotToken())
				if err != nil {
					return goerr.Wrap(err, "failed to initialize slack service")
				}
				slackSvc = svc
				ucOpts = append(ucOpts, usecase.WithSlackService(slackSvc))
				ucOpts = append(ucOpts, usecase.WithNotificationSlotDuration(slackCfg.NotificationSlotDuration()))
				logging.Default().Info("Slack service enabled for Source integration",
					"notification_slot_duration", slackCfg.NotificationSlotDuration(),
				)

				// Initialize Slack Admin / Search / MessageRetriever services if a
				// User OAuth Token is provided. The same User OAuth Token backs:
				//   - admin.conversations.*    (cross-workspace channel connect)
				//   - search.messages          (agent message search; search:read)
				//   - conversations.replies /  (agent message fetch; channels:history,
				//     conversations.history     lets the agent read public channels
				//                               the bot has not joined)
				if slackCfg.UserOAuthToken() != "" {
					adminSvc, err := slack.NewAdminClient(slackCfg.UserOAuthToken())
					if err != nil {
						return goerr.Wrap(err, "failed to initialize Slack admin service")
					}
					ucOpts = append(ucOpts, usecase.WithSlackAdminService(adminSvc))
					logging.Default().Info("Slack admin service enabled for cross-workspace channel connect")

					searchSvc, err := slacktool.NewSearchClient(slackCfg.UserOAuthToken())
					if err != nil {
						return goerr.Wrap(err, "failed to initialize Slack search service")
					}
					ucOpts = append(ucOpts, usecase.WithSlackSearchService(searchSvc))
					logging.Default().Info("Slack search service enabled for agent (requires search:read scope)")

					retrieverSvc, err := slacktool.NewMessageRetriever(slackCfg.UserOAuthToken())
					if err != nil {
						return goerr.Wrap(err, "failed to initialize Slack message retriever")
					}
					ucOpts = append(ucOpts, usecase.WithSlackMessageRetriever(retrieverSvc))
					logging.Default().Info("Slack message retriever enabled for agent (requires channels:history scope)")
				}

				// Detect org-level app and validate workspace team IDs
				if err := slackCfg.DetectOrgLevel(ctx); err != nil {
					return goerr.Wrap(err, "failed to detect Slack app level")
				}
				if slackCfg.IsOrgLevel() {
					logging.Default().Info("Detected org-level Slack app",
						"enterprise_id", slackCfg.EnterpriseID(),
						"team_id", slackCfg.AuthTeamID(),
					)
				} else {
					logging.Default().Info("Detected workspace-level Slack app",
						"team_id", slackCfg.AuthTeamID(),
					)
				}
				if err := slackCfg.ValidateWorkspaceTeamIDs(workspaceConfigs); err != nil {
					return goerr.Wrap(err, "workspace slack.team_id validation failed")
				}
			} else {
				logging.Default().Info("Slack Bot Token not configured, Slack Source features will be limited")
			}

			// Resolve the models this deployment may use and the default budget
			// one run may spend. Required for Slack-based features (agent /
			// assist / mention-draft) — usecase.New enforces that strictly when
			// slackService is configured. When no model is named (e.g. e2e tests
			// without API keys) we run in a degraded mode that still serves the
			// GraphQL API + frontend.
			//
			// It is resolved ONCE: the agent runtime further below is built from
			// the same setup, so the client a tool reaches and the client an
			// agent generates through cannot drift apart.
			modelSetup, err := buildLLMSetup(ctx, c, &appCfg, &llmCfg, registry, agentCfg.BudgetOr)
			if err != nil {
				return goerr.Wrap(err, "failed to resolve the LLM models")
			}
			llmClient := modelSetup.Default
			if !modelSetup.enabled() {
				logging.Default().Warn("no LLM model configured; Slack-driven AI features will be unavailable")
			} else {
				ucOpts = append(ucOpts, usecase.WithLLMClient(llmClient))
				logging.Default().Info("LLM client enabled", logAttrsToArgs(llmCfg.LogAttrs())...)

				// Embedding is mandatory whenever LLM is wired: agent /
				// assist / mention-draft all rely on memory / knowledge
				// similarity search, which uses the dedicated embedder.
				// It is configured independently from the agent's models so
				// chat completion and embedding can target different
				// providers (embedding is Gemini-only).
				if !embCfg.IsEnabled() {
					return goerr.New("--embedding-gemini-project-id is required when --llm-model is set")
				}
				embedClient, err := embCfg.NewClient(ctx)
				if err != nil {
					return goerr.Wrap(err, "failed to initialize embedding client")
				}
				ucOpts = append(ucOpts, usecase.WithEmbedClient(embedClient))
				logging.Default().Info("Embedding client enabled", logAttrsToArgs(embCfg.LogAttrs())...)
			}

			// Home dashboard: stale threshold is always wired (default carried by
			// the flag). The greeting uses a dedicated LLM only when explicitly
			// configured; otherwise the usecase falls back to the shared chat LLM.
			ucOpts = append(ucOpts, usecase.WithDashboardStaleThreshold(dashboardStaleThreshold))
			if homeMsgCfg.IsEnabled() {
				homeMsgClient, err := homeMsgCfg.NewClient(ctx)
				if err != nil {
					return goerr.Wrap(err, "failed to initialize home-message LLM client")
				}
				ucOpts = append(ucOpts, usecase.WithHomeMessageLLMClient(homeMsgClient))
				logging.Default().Info("Home-message LLM client enabled", logAttrsToArgs(homeMsgCfg.LogAttrs())...)
			}

			// Initialize GitHub service if configured
			githubSvc, err := githubCfg.Configure()
			if err != nil {
				return goerr.Wrap(err, "failed to initialize GitHub service")
			}
			if githubSvc != nil {
				ucOpts = append(ucOpts, usecase.WithGitHubService(githubSvc))
				logging.Default().Info("GitHub service enabled", logAttrsToArgs(githubCfg.LogAttrs())...)
			} else {
				logging.Default().Info("GitHub App not configured, GitHub Source features will be disabled")
			}

			// Initialize Jira agent tools if configured.
			jiraTools, err := jiraCfg.Configure(ctx)
			if err != nil {
				return goerr.Wrap(err, "failed to initialize Jira tools")
			}
			if jiraTools != nil {
				ucOpts = append(ucOpts, usecase.WithJiraTools(jiraTools))
				logging.Default().Info("Jira service enabled", logAttrsToArgs(jiraCfg.LogAttrs())...)
			} else {
				logging.Default().Info("Jira not configured, Jira agent tools will be disabled")
			}

			// Enable the agent webfetch tool. It is built only when an LLM
			// client is also configured (injection screening is mandatory).
			if webfetchCfg.IsEnabled() {
				ucOpts = append(ucOpts, usecase.WithWebFetch(webfetchCfg.Settings()))
				logging.Default().Info("WebFetch tool enabled", logAttrsToArgs(webfetchCfg.LogAttrs())...)
			} else {
				logging.Default().Info("WebFetch tool disabled")
			}

			// Configure agent session archive (Cloud Storage) when Slack is wired.
			// Slack-driven AI flows (mention agent) require History + Trace
			// persistence; the bucket flag is mandatory in that case.
			var storageCleanup func()
			var agentHistoryRepo gollem.HistoryRepository
			var agentTraceRepo trace.Repository
			var agentProcessHistory agentkit.HistoryStore
			if slackSvc != nil {
				archive, err := storageCfg.Configure(ctx)
				if err != nil {
					return goerr.Wrap(err, "failed to configure agent storage")
				}
				storageCleanup = archive.Close
				agentHistoryRepo = archive.History
				agentTraceRepo = archive.Trace
				agentProcessHistory = archive.ProcessHistory
				ucOpts = append(ucOpts, usecase.WithHistoryRepository(archive.History))
				ucOpts = append(ucOpts, usecase.WithTraceRepository(archive.Trace))
				logging.Default().Info("Agent session archive enabled", logAttrsToArgs(storageCfg.LogAttrs())...)
			}
			defer func() {
				if storageCleanup != nil {
					storageCleanup()
				}
			}()

			ucOpts = append(ucOpts, usecase.WithWorkspaceGroups(groupRegistry))
			workspaceAccess, err := buildWorkspaceAccess(registry, workspaceConfigs, repo.SlackUser())
			if err != nil {
				return err
			}
			ucOpts = append(ucOpts, usecase.WithWorkspaceAccess(workspaceAccess))
			uc := usecase.New(repo, registry, ucOpts...)

			// Interactive Jobs suspend a run and resume it from a later Slack
			// submit — possibly on a different instance — so their conversation
			// history MUST live in a shared backend (Cloud Storage). Fail loudly
			// at startup rather than letting a resume silently lose context.
			// agentHistoryRepo is non-nil only when Slack + Cloud Storage are
			// configured (see above), which is also what makes the question
			// form deliverable in the first place.
			if agentHistoryRepo == nil && registryHasInteractiveJob(registry) {
				return goerr.New("interactive Jobs require a persistent agent history backend: configure Slack and Cloud Storage (HECATONCHEIRES_CLOUD_STORAGE_BUCKET)")
			}

			// Wire the event-driven Job runtime. The JobUseCase listens to
			// CaseUseCase lifecycle events and dispatches Agent Jobs in the
			// background. The ScheduledScanner / HTTP hook handler are wired
			// further below. It runs on the SAME modelSetup resolved above.
			// Build the agentkit runtime. It is skipped entirely without an LLM
			// client, for the same reason the Job runtime is: there is nothing
			// for an agent to run.
			var agentKernel *agentkit.Kernel
			// durableJobs is the agent runtime the simple-strategy Jobs run on. It
			// is declared out here because the Job runtime built further down needs
			// it, while its registration has to happen inside the Kernel build.
			var durableJobs *job.DurableRuntime
			var durableDraft *proposal.Durable
			// jobSlots is the deployment-wide concurrency gate. It is built inside the
			// Kernel block because the Kernel is what enforces it, and declared out
			// here because the Job runtime below must be given the SAME instance.
			var jobSlots *job.ConcurrencyLimiter
			if llmClient != nil {
				agentProcessRepo, agentProcessCleanup, apErr := repoCfg.ConfigureAgentProcess(ctx)
				if apErr != nil {
					return goerr.Wrap(apErr, "failed to configure the agent process repository")
				}
				defer agentProcessCleanup()

				budgets, bErr := agentCfg.Budgets()
				if bErr != nil {
					return bErr
				}

				// Without Cloud Storage (no Slack wired) the agent runtime falls
				// back to in-process stores. Those hold nothing across instances,
				// which is only acceptable because that configuration cannot
				// deliver a Slack-driven agent turn in the first place.
				processHistory := agentProcessHistory
				if processHistory == nil {
					processHistory = agentarchive.NewMemoryHistoryStore()
				}
				kernelTrace := agentTraceRepo
				if kernelTrace == nil {
					kernelTrace = agentarchive.NewMemoryTraceRepository()
				}

				// Registration must complete before the Kernel is built, and the
				// Kernel must be bound before the first Spawn — so the three
				// steps run in this order and nowhere else.
				agentRegistry := agentkit.NewRegistry()
				// The per-task sub-agent is registered once and shared: agentkit keys
				// a Process on the agent name, so a second registration would fail.
				taskAgent, tErr := agentkernel.RegisterTaskAgent(agentRegistry,
					budgets.Task.Limiter(modelSetup.Policy.Resolve), processHistory)
				if tErr != nil {
					return goerr.Wrap(tErr, "failed to register the task sub-agent")
				}
				if rErr := uc.Agent.RegisterAgents(agentRegistry,
					budgets.Root.Limiter(modelSetup.Policy.Resolve), modelSetup.Policy,
					processHistory, agentProcessRepo, taskAgent); rErr != nil {
					return goerr.Wrap(rErr, "failed to register the agents")
				}
				// One locator serves every host that needs to tell "already live" from
				// "fresh": the Job runner drops a re-trigger with it, and the case-draft
				// host tells a Slack re-delivery from a busy thread with it.
				locator, lErr := agentkernel.NewLocator(agentProcessRepo)
				if lErr != nil {
					return goerr.Wrap(lErr, "failed to build the agent process locator")
				}
				durableJobs = &job.DurableRuntime{
					History: processHistory, Locator: locator, Models: modelSetup.Policy,
				}
				if rErr := durableJobs.Register(agentRegistry, budgets.Root.Limiter(modelSetup.Policy.Resolve), taskAgent); rErr != nil {
					return goerr.Wrap(rErr, "failed to register the job agents")
				}
				// The case-draft agent is registered only when its usecase exists: it
				// needs the persistent History/Trace archive, which a deployment without
				// Cloud Storage does not have.
				if uc.MentionProposal != nil {
					d, dErr := proposal.NewDurable(repo, registry, uc.WorkspaceAccess,
						uc.MentionProposal.DurableDraftHost(), locator, modelSetup.Policy)
					if dErr != nil {
						return goerr.Wrap(dErr, "failed to build the case-draft agent")
					}
					if rErr := d.Register(agentRegistry, taskAgent, nil,
						budgets.Root.Limiter(modelSetup.Policy.Resolve), processHistory); rErr != nil {
						return goerr.Wrap(rErr, "failed to register the case-draft agent")
					}
					durableDraft = d
				}

				// The slot gate has to exist before the Kernel, because the Kernel is
				// what enforces it: a durable Job run occupies a slot for as long as
				// its claims run, not for the length of the call that spawned it. The
				// same instance is handed to the Job runner below, so both sides count
				// the same holds.
				if jobCfg.Limit() > 0 {
					l, sErr := buildJobSlotLimiter(repo, jobCfg.Limit())
					if sErr != nil {
						return sErr
					}
					jobSlots = l
				}

				// One ToolDeps value feeds both the Kernel and the probe, so what a
				// host advertises to its planner and what the tool factory builds can
				// never disagree.
				toolDeps := uc.AgentToolDeps()
				k, kErr := agentkernel.Build(agentkernel.Deps{
					Repo:    agentProcessRepo,
					History: processHistory,
					LLM:     llmClient,
					Trace:   kernelTrace,
					Budgets: budgets,
					Models:  modelSetup.Policy,
					Agents:  agentRegistry,
					Slots:   jobSlots,
					Tools:   toolDeps,
				})
				if kErr != nil {
					return goerr.Wrap(kErr, "failed to build the agent runtime")
				}
				probe, pErr := agentkernel.NewToolSetProbe(toolDeps)
				if pErr != nil {
					return goerr.Wrap(pErr, "failed to build the agent toolset probe")
				}
				agentKernel = k
				uc.Agent.BindAgentKernel(agentKernel, probe)
				durableJobs.Bind(agentKernel, probe)
				if durableDraft != nil {
					durableDraft.Bind(agentKernel, probe)
					uc.MentionProposal.BindDurableDraft(durableDraft)
				}
				logging.Default().Info("Agent runtime configured", logAttrsToArgs(agentCfg.LogAttrs())...)
			}

			jobUC, jobRunner, jobErr := buildJobRuntime(jobRuntimeDeps{
				Repo:           repo,
				Registry:       registry,
				LLMClient:      llmClient,
				UC:             uc,
				SlackService:   slackSvc,
				WebFetch:       uc.WebFetchClient(),
				SlackSearch:    uc.SlackSearchService(),
				SlackRetriever: uc.SlackMessageRetriever(),
				SlackLimits:    uc.SlackToolLimits(),
				NotionTool:     uc.NotionToolClient(),
				JiraTools:      jiraTools,
				HistoryRepo:    agentHistoryRepo,
				TraceRepo:      agentTraceRepo,
				SlotLimit:      jobCfg.Limit(),
				// The Kernel's slot gate, so the runner's pre-spawn refusal and the
				// claim-time hold that bounds execution count the same slots.
				SlotLimiter: jobSlots,
				Durable:     durableJobs,
			})
			if jobErr != nil {
				return goerr.Wrap(jobErr, "failed to build job runtime")
			}
			// The last of the three wiring steps: a finished durable run closes
			// itself out through the runner, which only exists now.
			durableJobs.AttachRunner(jobRunner)
			logging.Default().Info("Agent Job runtime configured", logAttrsToArgs(jobCfg.LogAttrs())...)
			uc.Case.SetEventPublisher(jobUC)
			// The web UI's manual Run button drives the same runner through
			// JobRunUseCase.TriggerJob.
			uc.JobRun.SetTrigger(jobRunner)
			tickScanner := job.NewScheduledScanner(job.ScannerDeps{
				Repo:      repo,
				Registry:  registry,
				Publisher: jobUC,
			})
			tickHook := httpctrl.NewTickHookHandler(tickScanner)

			// Start Slack user refresh worker if Slack service is available
			// N+1 Prevention Policy: Worker uses DeleteAll → SaveMany (Replace strategy)
			// to avoid individual DB operations in loops
			var slackUserWorker *worker.SlackUserRefreshWorker
			if slackSvc != nil {
				slackUserWorker = worker.NewSlackUserRefreshWorker(repo, slackSvc, 10*time.Minute, slackCfg.IsOrgLevel())
				if err := slackUserWorker.Start(ctx); err != nil {
					return goerr.Wrap(err, "failed to start Slack user refresh worker")
				}
			}

			// Create GraphQL handler with dataloaders
			resolver := gqlctrl.NewResolver(repo, uc)
			srv := handler.NewDefaultServer(
				gqlctrl.NewExecutableSchema(gqlctrl.Config{Resolvers: resolver}),
			)
			// Every workspace-scoped Query / Mutation root field is checked here,
			// so a field added later cannot skip the per-workspace policy.
			srv.AroundFields(gqlctrl.WorkspaceAccessMiddleware(uc.WorkspaceAccess))

			// Configure error presenter with stack traces and client/server
			// classification (extensions.code is read by graphqlErrorStatusMiddleware
			// to map errors to the right HTTP status).
			srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
				gqlErr := graphql.DefaultErrorPresenter(ctx, err)
				if gqlErr.Extensions == nil {
					gqlErr.Extensions = map[string]any{}
				}
				// Merge code + code-specific detail (e.g. missingFieldNames,
				// currentStatus) from the GraphQL error mapper. Frontend
				// switches on extensions.code and renders the detail keys.
				maps.Copy(gqlErr.Extensions, gqlctrl.ErrorExtensions(err))

				// Log full stack trace for diagnostics. Client-faulted errors
				// are expected normal flow (validation, not-found, etc.) and
				// tagged benign so they don't page anyone; server faults are
				// reported normally.
				if isClientError(err) {
					errutil.Handle(ctx, goerr.Wrap(err, "GraphQL request rejected", goerr.T(errutil.TagBenign)), "GraphQL request rejected")
				} else {
					errutil.Handle(ctx, goerr.Wrap(err, "GraphQL error occurred"), "GraphQL error occurred")
				}

				return gqlErr
			})

			// Configure panic handler
			srv.SetRecoverFunc(func(ctx context.Context, panicValue interface{}) error {
				// Create error from panic value
				var panicErr error
				switch e := panicValue.(type) {
				case error:
					panicErr = e
				case string:
					panicErr = goerr.New(e)
				default:
					panicErr = goerr.New("panic occurred", goerr.V("panic", panicValue))
				}

				// Wrap and report with stack trace
				wrappedErr := goerr.Wrap(panicErr, "GraphQL panic")
				errutil.Handle(ctx, wrappedErr, "GraphQL panic occurred")

				return wrappedErr
			})

			// Wrap with dataloader middleware (request-scoped). A fresh
			// set of loaders per request is non-negotiable - the
			// internal cache must not survive across users, and
			// dataloader/v7's batching window only collapses calls
			// inside one Load(...) tick anyway.
			gqlHandlerBase := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				loaders := gqlctrl.NewDataLoaders(repo, slackSvc, uc)
				ctx := gqlctrl.WithDataLoaders(r.Context(), loaders)
				srv.ServeHTTP(w, r.WithContext(ctx))
			})

			// Wrap with error status middleware to return HTTP 500 on GraphQL errors
			gqlHandler := graphqlErrorStatusMiddleware(gqlHandlerBase)

			// Create HTTP server options
			httpOpts := []httpctrl.Options{
				httpctrl.WithGraphiQL(enableGraphiQL),
				httpctrl.WithAuth(authUC),
				httpctrl.WithWorkspaceRegistry(registry),
				httpctrl.WithWorkspaceAccess(uc.WorkspaceAccess),
			}

			// Add Slack service if configured
			if slackSvc != nil {
				httpOpts = append(httpOpts, httpctrl.WithSlackService(slackSvc))
			}

			// Add Slack webhook handler if configured
			if slackCfg.IsWebhookConfigured() {
				slackWebhookHandler := httpctrl.NewSlackWebhookHandler(uc.Slack)
				httpOpts = append(httpOpts, httpctrl.WithSlackWebhook(slackWebhookHandler, slackCfg.SigningSecret()))
				logging.Default().Info("Slack webhook handler enabled")

				// Add Slack interaction handler (shares signing secret with webhook)
				slackInteractionHandler := httpctrl.NewSlackInteractionHandler(
					uc.Action, uc.Agent, uc.Slack, uc.Case, uc.MentionProposal, jobRunner,
				)

				// Add slash command handler
				slackCommandHandler := httpctrl.NewSlackCommandHandler(uc.Slack)
				httpOpts = append(httpOpts, httpctrl.WithSlackInteraction(slackInteractionHandler))
				httpOpts = append(httpOpts, httpctrl.WithSlackCommand(slackCommandHandler))
				logging.Default().Info("Slack interaction handler enabled")
				logging.Default().Info("Slack slash command handler enabled")
			}

			// Register the scheduled-Job sweep webhook.
			httpOpts = append(httpOpts, httpctrl.WithTickHook(tickHook))

			// Register the DB consistency check endpoint. Its configuration comes
			// from the request, not from this process, so an operator can ask
			// whether a candidate config change would leave data inconsistent
			// without restarting anything.
			httpOpts = append(httpOpts, httpctrl.WithDBCheck(httpctrl.NewDBCheckHandler(newDBConsistencyChecker(uc))))

			// Wire the MCP endpoint when enabled. Configure fails loudly if
			// --mcp is set without a --policy: we never expose the MCP data
			// surface without a Rego authorization policy.
			if mcpCfg.IsEnabled() {
				policyClient, mcpEnv, err := mcpCfg.Configure(c)
				if err != nil {
					return goerr.Wrap(err, "failed to configure MCP endpoint")
				}
				mcpHandler := httpctrl.NewMCPHandler(uc.Case, uc.Action, registry, policyClient, mcpEnv)
				httpOpts = append(httpOpts, httpctrl.WithMCP(mcpHandler))
				logging.Default().Info("MCP endpoint enabled", logAttrsToArgs(mcpCfg.LogAttrs())...)
			} else {
				logging.Default().Info("MCP endpoint disabled")
			}

			// Start the agent runtime worker. It claims runnable agent processes
			// from the shared store and drives one transition at a time, and it
			// is also what makes eager dispatch work: agentkit only dispatches a
			// just-spawned process in-process while a Serve is running here.
			//
			// Its context is cancelled on shutdown, which stops new claims; a
			// transition already in flight finishes and commits, and anything it
			// did not reach is picked up by another instance once the lease
			// expires.
			//
			// DispatchCancelable, not Dispatch: Dispatch severs the context with
			// context.WithoutCancel, so the worker would keep polling after
			// shutdown — against the Firestore and Cloud Storage clients the
			// deferred cleanup below has already closed.
			agentServeCtx, stopAgentServe := context.WithCancel(ctx)
			agentServeDone := make(chan struct{})
			if agentKernel != nil {
				if err := agentCfg.ValidateWorker(); err != nil {
					stopAgentServe()
					return goerr.Wrap(err, "invalid agent worker configuration")
				}
				// Registered before the clients it uses are closed, so it runs
				// AFTER them in LIFO order: stop the worker, wait for it to let
				// go, and only then let the client cleanups run.
				defer func() {
					stopAgentServe()
					<-agentServeDone
				}()
				async.DispatchCancelable(agentServeCtx, func(c context.Context) error {
					defer close(agentServeDone)
					logging.Default().Info("Starting agent runtime worker",
						logAttrsToArgs(agentCfg.LogAttrs())...)
					if err := agentkernel.Serve(c, agentKernel,
						agentkit.WithLease(agentCfg.WorkerLease()),
						agentkit.WithPollInterval(agentCfg.WorkerPollInterval()),
						agentkit.WithPollConcurrency(agentCfg.WorkerPollConcurrency()),
						agentkit.WithMaxConcurrent(agentCfg.WorkerConcurrency()),
					); err != nil && !errors.Is(err, context.Canceled) {
						return goerr.Wrap(err, "agent runtime worker stopped")
					}
					logging.Default().Info("Agent runtime worker stopped")
					return nil
				})
			} else {
				stopAgentServe()
				close(agentServeDone)
			}

			// Create HTTP server
			httpHandler, err := httpctrl.New(gqlHandler, httpOpts...)
			if err != nil {
				return goerr.Wrap(err, "failed to create http server")
			}
			server := &http.Server{
				Addr:              addr,
				Handler:           httpHandler,
				ReadHeaderTimeout: 30 * time.Second,
			}

			// Setup signal handling for graceful shutdown
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

			// Start server in goroutine
			errCh := make(chan error, 1)
			go func() {
				logging.Default().Info("Starting HTTP server", "addr", addr, "graphiql", enableGraphiQL)
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					errCh <- goerr.Wrap(err, "failed to start server")
				}
			}()

			// Wait for shutdown signal or server error
			select {
			case err := <-errCh:
				return err
			case sig := <-sigCh:
				logging.Default().Info("Received shutdown signal", "signal", sig)

				// Stop Slack user refresh worker first
				if slackUserWorker != nil {
					slackUserWorker.Stop()
				}

				// Create shutdown context with timeout
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				// Attempt graceful shutdown
				if err := server.Shutdown(shutdownCtx); err != nil {
					return goerr.Wrap(err, "failed to shutdown server gracefully")
				}

				logging.Default().Info("Server shutdown completed")
				return nil
			}
		},
	}
}
