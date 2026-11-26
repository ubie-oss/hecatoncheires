package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/m-mizutani/goerr/v2"
	"github.com/urfave/cli/v3"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
	notiontool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/notion"
	slacktool "github.com/m-mizutani/hecatoncheires/pkg/agent/tool/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
	eval "github.com/m-mizutani/hecatoncheires/pkg/usecase/eval"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/safe"
)

// evalDefaultBudgetUSD is what one scenario run may spend when the global config
// declares no [agent] budget. It is deliberately generous: a scenario is meant to
// fail on the agent's judgement, not on a ceiling the harness imposed. The eval
// command carries no --agent-* flags, so this is the only fallback.
const evalDefaultBudgetUSD = 20.0

func cmdEval() *cli.Command {
	var (
		appCfg       config.AppConfig
		llmCfg       config.LLM
		slackCfg     config.Slack
		slackToolCfg config.SlackTool
		githubCfg    config.GitHub
		jiraCfg      config.Jira
		webfetchCfg  config.WebFetch
		notionTok    string
		reportPath   string
		dumpDir      string
		dumpAll      bool
		dryRun       bool
		listTools    bool
		quiet        bool
		verbose      bool
		langStr      string
		concurrency  int
		harnessDir   string
	)

	flags := []cli.Flag{
		&cli.BoolFlag{Name: "list-tools", Usage: "Print the catalog of tools usable in scenarios and exit", Destination: &listTools},
		&cli.StringFlag{Name: "harness-dir", Usage: "Publish eval requests for an external worker in this private directory", Destination: &harnessDir},
		&cli.BoolFlag{Name: "dryrun", Usage: "Validate scenario files only (no LLM / tools / network)", Destination: &dryRun},
		&cli.StringFlag{Name: "report", Usage: "Write a JSON report to this path", Destination: &reportPath},
		&cli.IntFlag{Name: "concurrency", Usage: "Number of scenarios to run in parallel", Value: 2, Destination: &concurrency},
		&cli.BoolFlag{Name: "quiet", Usage: "Compact one-line-per-scenario summary", Destination: &quiet},
		&cli.BoolFlag{Name: "verbose", Usage: "Expand transcript and tool-call details", Destination: &verbose},
		&cli.StringFlag{Name: "dump-dir", Usage: "Diagnostic dump directory", Value: "tmp/eval", Destination: &dumpDir},
		&cli.BoolFlag{Name: "dump-all", Usage: "Dump every scenario, not only those with failing checks", Destination: &dumpAll},
		&cli.StringFlag{
			Name:        "lang",
			Usage:       "Output language for judge reasons / analysis (en, ja)",
			Value:       "en",
			Sources:     cli.EnvVars("HECATONCHEIRES_DEFAULT_LANG"),
			Destination: &langStr,
		},
		&cli.StringFlag{
			Name:        "notion-api-token",
			Usage:       "Notion API token (only for tools marked live=true)",
			Sources:     cli.EnvVars("HECATONCHEIRES_NOTION_API_TOKEN"),
			Destination: &notionTok,
		},
	}
	flags = append(flags, appCfg.GlobalConfigFlags()...)
	flags = append(flags, llmCfg.Flags()...)
	flags = append(flags, slackCfg.Flags()...)
	flags = append(flags, slackToolCfg.Flags()...)
	flags = append(flags, githubCfg.Flags()...)
	flags = append(flags, jiraCfg.Flags()...)
	flags = append(flags, webfetchCfg.Flags()...)

	return &cli.Command{
		Name:      "eval",
		Usage:     "Run LLM workflow evaluation scenarios",
		ArgsUsage: "<scenario.toml | dir> ...",
		Flags:     flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			logger := logging.From(ctx)

			if listTools {
				printToolCatalog()
				return nil
			}

			lang, err := i18n.ParseLang(langStr)
			if err != nil {
				return goerr.Wrap(err, "invalid --lang value")
			}
			i18n.Init(lang)

			paths := c.Args().Slice()
			if len(paths) == 0 {
				return goerr.New("at least one scenario file or directory is required")
			}

			if err := slackToolCfg.Validate(); err != nil {
				return goerr.Wrap(err, "invalid Slack agent tool configuration")
			}

			cfg := eval.Config{
				Concurrency: concurrency,
				DryRun:      dryRun,
				DumpDir:     dumpDir,
				DumpAll:     dumpAll,
				Language:    string(lang),
				ReportPath:  reportPath,
				Quiet:       quiet,
				Verbose:     verbose,
				// Applies to simulated and live Slack tools alike: a scenario
				// must see the same bounds a deployed run would.
				SlackToolLimits: slackToolCfg.Limits(),
			}

			if !dryRun {
				// The harness resolves models exactly as serve does, so a
				// scenario Job naming a model reaches the same client a deployed
				// one would. It passes no workspace registry: scenarios bring
				// their own workspace per run, so every DEFINED model gets a
				// client rather than only the ones a registry names.
				setup, err := buildEvalLLMSetup(ctx, c, &appCfg, &llmCfg, harnessDir != "")
				if err != nil {
					return err
				}
				if !setup.enabled() {
					return goerr.New("a model is required to run scenarios: declare [[llm_model]] in --global-config and name it with --llm-model")
				}
				cfg.LLM = setup.Default
				cfg.Models = setup.Policy

				if harnessDir != "" {
					session, err := harness.NewSession(harnessDir)
					if err != nil {
						return goerr.Wrap(err, "start the external eval harness")
					}
					defer safe.Close(ctx, session)
					cfg.Harness = session
				} else if err := wireLiveTools(ctx, &cfg, &slackCfg, &githubCfg, &jiraCfg, &webfetchCfg, notionTok); err != nil {
					return err
				}
			}

			code, err := eval.Run(ctx, paths, cfg, os.Stdout)
			if err != nil {
				return err
			}
			if code != eval.ExitOK {
				logger.Warn("eval finished with execution errors", "exit_code", code)
				return cli.Exit("", code)
			}
			return nil
		},
	}
}

// wireLiveTools builds real tool clients from flags so scenarios that mark a
// tool live=true can use them. Each is optional; env errors clearly if a live
// tool is requested without its client.
func wireLiveTools(ctx context.Context, cfg *eval.Config, slackCfg *config.Slack, githubCfg *config.GitHub, jiraCfg *config.Jira, webfetchCfg *config.WebFetch, notionTok string) error {
	if slackCfg.UserOAuthToken() != "" {
		searchSvc, err := slacktool.NewSearchClient(slackCfg.UserOAuthToken())
		if err != nil {
			return goerr.Wrap(err, "failed to initialize Slack search client")
		}
		cfg.LiveSlackSearch = searchSvc
	}
	if notionTok != "" {
		notionClient, err := notiontool.NewClient(notionTok)
		if err != nil {
			return goerr.Wrap(err, "failed to initialize Notion client")
		}
		cfg.LiveNotion = notionClient
	}
	githubSvc, err := githubCfg.Configure()
	if err != nil {
		return goerr.Wrap(err, "failed to initialize GitHub client")
	}
	cfg.GitHub = githubSvc

	jiraTools, err := jiraCfg.Configure(ctx)
	if err != nil {
		return goerr.Wrap(err, "failed to initialize Jira tools")
	}
	cfg.JiraTools = jiraTools

	// WebFetch is live-only: its HTTP-side settings flow through, and the eval
	// env injects the eval LLM client into the tool before it is built.
	if webfetchCfg.IsEnabled() {
		settings := webfetchCfg.Settings()
		cfg.WebFetch = &settings
	}
	return nil
}

func printToolCatalog() {
	fmt.Println("Tools usable in scenario [tools.*] tables and tool-usage checks:")
	for _, e := range eval.ToolCatalog() {
		ro := ""
		if e.ReadOnly {
			ro = " (read-only)"
		}
		sim := "sim+live"
		if !e.Simulatable {
			sim = "live-only"
		}
		fmt.Printf("  %-16s %s%s [%s]\n", e.Name, e.Description, ro, sim)
	}
}
