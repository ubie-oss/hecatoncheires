package goast

# Every exported function/method under pkg/usecase that performs use-case work
# must take context.Context as its first parameter, so cancellation, deadlines
# and the request-scoped logger propagate through the whole use-case layer.
#
# Scope: files whose directory path contains "pkg/usecase". Excluded:
#   - pkg/usecase/eval/** — the offline eval harness is temporarily out of
#     scope for this policy.
#   - _test.go files (exported TestXxx(t) take *testing.T, not context).
#   - Idiomatic context-free functions that Go convention keeps ctx-free:
#     constructors (New*), functional options (With*), boolean predicates
#     (Is*/Has*/Can*), and pure value/accessor methods listed in exempt_name.
# Adjust exempt_prefix / exempt_name below to tune the exemption set.
fail contains res if {
	input.Kind == "FuncDecl"
	contains(input.DirName, "pkg/usecase")
	not contains(input.DirName, "pkg/usecase/eval")
	not endswith(input.FileName, "_test.go")

	# exported == identifier starts with an upper-case letter
	regex.match("^[A-Z]", input.Node.Name.Name)

	not is_exempt
	not has_context_first_param

	res := {
		"msg": sprintf("exported use-case function %q must take context.Context as its first parameter", [input.Node.Name.Name]),
		"pos": input.Node.Name.NamePos,
		"sev": "ERROR",
	}
}

# --- exemptions ---------------------------------------------------------------

# Name prefixes for idioms that are context-free by Go convention.
exempt_prefix := {
	"New", # constructors
	"With", # functional options
	"Is", # boolean predicates
	"Has",
	"Can",
	"Parse", # pure decoders (string/value -> value)
}

# Exact names of pure value / accessor methods that carry no I/O and take no
# ctx. Each entry was verified against the current tree to be a plain
# getter/setter, an in-memory registry/map lookup, a pure data transform, or a
# constructor-like wrapper — none touch external systems. Add a name here only
# after confirming the same: a function that later grows real I/O must be
# removed from this list so the rule flags it again.
exempt_name := {
	# pure value methods / formatters
	"Validate",
	"Kind",
	"Kinds",
	"KnownIDs",
	"Render",
	"RenderUserPrompt",
	"BuildSystemPrompt",
	"Default",
	"Dump",
	"Summary",
	"ComputeScore",
	"String",
	# sums the Count fields of an already-computed ValidationResult
	"TotalCount",
	"FormatPrefix",
	"CaseURL",
	"SlackActionAssigneeBlockID",
	# field getters / setters
	"WorkspaceRegistry",
	"WorkspaceGroups",
	"SlackService",
	"WebFetchClient",
	"SlackSearchService",
	"SlackMessageRetriever",
	"SlackToolLimits",
	"NotionToolClient",
	"GitHubToolClient",
	"PlannerLoopMax",
	"SubAgentLoopMax",
	"LLMCalls",
	"SetEventPublisher",
	"SetTrigger",
	# in-memory registry / config lookups (no I/O)
	"GetAuthURL",
	"GetFieldConfiguration",
	"GetActionStatusSet",
	"GetCaseStatusSet",
	"ReferenceWorkspaceForField",
	"MemoConfiguration",
	# reads the workspace entry and whether a Slack service is wired; no I/O
	"ExistingSlackChannelAllowed",
	"Resolve",
	"ResolveJobName",
	# static catalogs / pure transforms
	"NextFireTime",
	"LLMRequestFromTrace",
	"LLMResponseFromTrace",
	"ToolCallFromTrace",
	# maps toolset ids through a fixed in-package table; no I/O. It stays beside the
	# ToolSet* constants so that adding one puts its classification in view.
	"ReadOnlyToolSetIDs",
	# in-memory counters (mutex-guarded, no external I/O)
	"Next",
	# agent-runtime startup wiring. These run once at process start, before any
	# request exists: Register/RegisterAgents fill an in-memory agentkit.Registry
	# and Bind/BindAgentKernel are field setters. None performs I/O, so there is
	# no cancellation or deadline to propagate. A host that later does real work
	# in one of these must be renamed rather than left on this list.
	"Register",
	"RegisterAgents",
	"Bind",
	"BindAgentKernel",
	"BindDurableDraft",
	"AttachRunner",
	# assembles the agent tool dependencies from clients the UseCases already
	# holds. A struct literal over its own fields: no I/O, and it runs once per
	# process while the Kernel is being built, before any request exists.
	"AgentToolDeps",
	# TrackSpawns is the same kind of startup switch: it initialises an in-memory
	# slice so a batch command can later wait for its own runs. No I/O.
	"TrackSpawns",
	# returns the usecase's own completion-handler adapter; a struct literal.
	"DurableDraftHost",
	# agentkit Strategy methods. Their signatures are fixed by the library, and
	# every one of them is pure: Version reports a constant, Init builds the
	# initial state (agentkit gives it no ctx and no Syscalls precisely so a
	# strategy author cannot perform effects there), and the four codecs only
	# marshal. The transition method that DOES take a ctx is Step, which is not
	# exempted here because it has one.
	#
	# The proper fix is relocating the plan-execute Strategy to pkg/agent/, next to
	# the ReAct one, which is where runtime machinery with library-fixed method
	# sets already lives. It shares unexported schema and prompt helpers with the
	# runtime it is replacing, so the move happens when that runtime is deleted.
	"Version",
	"Init",
	"EncodeState",
	"DecodeState",
	"EncodeOutput",
	"DecodeOutput",
	# renders a question and its answers into the user turn a suspended run
	# continues from. A pure string transform, moved with the Strategy above.
	"RenderAnswers",
}

is_exempt if {
	some p in exempt_prefix
	startswith(input.Node.Name.Name, p)
}

is_exempt if {
	exempt_name[input.Node.Name.Name]
}

# --- helpers ------------------------------------------------------------------

# True when the first parameter's type is the selector expression context.Context.
# Undefined (so the guard fails) when there are no parameters or the first one
# is some other type.
has_context_first_param if {
	first := input.Node.Type.Params.List[0]
	first.Type.X.Name == "context"
	first.Type.Sel.Name == "Context"
}
