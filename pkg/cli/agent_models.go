package cli

import (
	"context"
	"maps"
	"slices"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/urfave/cli/v3"

	agentkernel "github.com/m-mizutani/hecatoncheires/pkg/agent/kernel"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/logging"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/pricing"
)

// llmSetup is what an agent host needs to reach a model: the default client the
// Kernel is built on, and the policy that answers, per run, which model it
// generates through and what it may spend.
//
// A zero value means the deployment configured no LLM. Every caller treats that
// the same way it treated a nil client before: the agent runtime is not built and
// the AI features stay dormant.
type llmSetup struct {
	Default gollem.LLMClient
	Policy  agentkernel.ModelPolicy
}

// enabled reports whether an LLM is configured at all.
func (s llmSetup) enabled() bool { return s.Default != nil }

// budgetResolver decides the default budget for a run, and says where the figure
// came from. config.Agent.BudgetOr is the production implementation; the eval
// harness supplies its own because it carries no --agent-* flags.
type budgetResolver func(*config.AgentSection) (pricing.NanoUSD, string, error)

// buildLLMSetup resolves the deployment's models and budget from the global
// config and the LLM flags.
//
// It is the single place a model reference name becomes a client, so the set of
// usable models is exactly the set the operator declared — a Job cannot reach a
// model this function did not resolve, and a model nobody names costs nothing to
// have declared.
//
// registry supplies the Jobs whose model references need clients; it may be nil
// for a command that runs no Jobs.
func buildLLMSetup(ctx context.Context, c *cli.Command, appCfg *config.AppConfig,
	llmCfg *config.LLM, registry *model.WorkspaceRegistry, resolveBudget budgetResolver,
) (llmSetup, error) {
	defs, err := appCfg.ConfigureLLMModels(c)
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "load the model definitions")
	}
	agentSection, err := appCfg.ConfigureAgentSection(c)
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "load the [agent] section")
	}
	jobRefs := jobModelRefs(registry)

	if !llmCfg.IsEnabled() {
		// Definitions with no default model named is a half-finished
		// configuration, not a deployment that opted out of AI: something was
		// set up and nothing will use it. Say so rather than starting quietly
		// degraded.
		if len(defs) > 0 || len(jobRefs) > 0 {
			return llmSetup{}, goerr.New(
				"--llm-model is required when models are configured: it names which [[llm_model]] is the default",
				goerr.V("defined_models", len(defs)), goerr.V("job_model_refs", jobRefs))
		}
		return llmSetup{}, nil
	}

	if err := config.ValidateJobModels(defs, registry); err != nil {
		return llmSetup{}, err
	}

	// Every Job's model plus the default one. A model nobody names gets no
	// client: declaring one must not cost a connection at startup.
	refs := append([]string{llmCfg.ModelRef()}, jobRefs...)
	slices.Sort(refs)
	refs = slices.Compact(refs)

	setup, err := newLLMSetup(ctx, llmCfg, defs, refs, agentSection, resolveBudget)
	if err != nil {
		return llmSetup{}, err
	}
	return setup, nil
}

// buildEvalLLMSetup is buildLLMSetup for the eval harness, which differs in two
// ways: it has no workspace registry at this point (each scenario brings its own
// workspace, so EVERY defined model gets a client rather than only the ones a
// registry names), and it carries no --agent-* flags, so the budget comes from
// the [agent] section or the harness's own default.
func buildEvalLLMSetup(ctx context.Context, c *cli.Command, appCfg *config.AppConfig,
	llmCfg *config.LLM,
) (llmSetup, error) {
	defs, err := appCfg.ConfigureLLMModels(c)
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "load the model definitions")
	}
	agentSection, err := appCfg.ConfigureAgentSection(c)
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "load the [agent] section")
	}
	if !llmCfg.IsEnabled() || len(defs) == 0 {
		return llmSetup{}, nil
	}

	refs := make([]string, 0, len(defs))
	for _, d := range defs {
		refs = append(refs, d.Ref)
	}

	return newLLMSetup(ctx, llmCfg, defs, refs, agentSection,
		func(sec *config.AgentSection) (pricing.NanoUSD, string, error) {
			if fromDoc := sec.DefaultBudget(); fromDoc > 0 {
				return fromDoc, config.BudgetSourceGlobalConfig, nil
			}
			return pricing.FromUSD(evalDefaultBudgetUSD), config.BudgetSourceBuiltin, nil
		})
}

// newLLMSetup builds one client per reference name in refs and assembles the
// policy. refs must contain the default reference name.
func newLLMSetup(ctx context.Context, llmCfg *config.LLM, defs []agentkernel.ModelDef,
	refs []string, agentSection *config.AgentSection, resolveBudget budgetResolver,
) (llmSetup, error) {
	byRef := make(map[string]agentkernel.ModelDef, len(defs))
	for _, d := range defs {
		byRef[d.Ref] = d
	}

	defaultRef := llmCfg.ModelRef()
	defaultDef, ok := byRef[defaultRef]
	if !ok {
		known := make([]string, 0, len(byRef))
		for ref := range byRef {
			known = append(known, ref)
		}
		slices.Sort(known)
		return llmSetup{}, goerr.Wrap(config.ErrUnknownLLMModelRef,
			"--llm-model names a model that no [[llm_model]] section defines",
			goerr.V("llm_model", defaultRef), goerr.V("known", known))
	}

	budget, source, err := resolveBudget(agentSection)
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "resolve the default agent budget")
	}

	defaultClient, err := llmCfg.NewClientFor(ctx, defaultDef)
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "build the default model client")
	}

	// The default model needs no entry: an unbound role already resolves to the
	// Kernel's positional client, which is this one.
	clients := make(map[string]gollem.LLMClient)
	for _, ref := range refs {
		if ref == defaultRef {
			continue
		}
		def, ok := byRef[ref]
		if !ok {
			return llmSetup{}, goerr.Wrap(config.ErrUnknownLLMModelRef,
				"a model reference name is not defined",
				goerr.V("llm_model", ref))
		}
		client, err := llmCfg.NewClientFor(ctx, def)
		if err != nil {
			return llmSetup{}, goerr.Wrap(err, "build a model client",
				goerr.V("llm_model", ref))
		}
		clients[ref] = client
	}

	policy, err := agentkernel.NewModelPolicy(agentkernel.ModelPolicyInput{
		Defs:          defs,
		DefaultRef:    defaultRef,
		Clients:       clients,
		DefaultBudget: budget,
	})
	if err != nil {
		return llmSetup{}, goerr.Wrap(err, "build the model policy")
	}

	logging.Default().Info("LLM models resolved",
		"default_model_ref", defaultRef,
		"default_provider", defaultDef.Provider,
		"default_model", defaultDef.Model,
		"job_model_refs", slices.Sorted(maps.Keys(clients)),
		"default_budget", budget.USD(),
		"default_budget_source", source,
	)
	return llmSetup{Default: policy.WrapClient(defaultRef, defaultClient), Policy: policy}, nil
}

// jobModelRefs returns the model reference names the registry's enabled Jobs
// name, deduplicated. A disabled Job is excluded for the same reason it is
// excluded from event matching: it does not run, so nothing needs a client for
// it.
func jobModelRefs(registry *model.WorkspaceRegistry) []string {
	if registry == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var refs []string
	for _, ws := range registry.List() {
		if ws == nil {
			continue
		}
		for _, j := range ws.Jobs {
			if j == nil || j.Disabled {
				continue
			}
			for _, ref := range j.ModelRefs() {
				if _, dup := seen[ref]; dup {
					continue
				}
				seen[ref] = struct{}{}
				refs = append(refs, ref)
			}
		}
	}
	slices.Sort(refs)
	return refs
}
