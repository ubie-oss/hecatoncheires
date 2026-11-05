package config

import "github.com/m-mizutani/goerr/v2"

// Sentinel errors for configuration validation
var (
	ErrConfigNotFound        = goerr.New("configuration file not found")
	ErrInvalidConfig         = goerr.New("invalid configuration")
	ErrDuplicateFieldID      = goerr.New("duplicate field ID")
	ErrDuplicateOptionID     = goerr.New("duplicate option ID")
	ErrInvalidFieldID        = goerr.New("invalid field ID format")
	ErrInvalidFieldType      = goerr.New("invalid field type")
	ErrMissingOptions        = goerr.New("select/multi-select field requires at least one option")
	ErrInvalidMetadata       = goerr.New("invalid metadata format")
	ErrMissingName           = goerr.New("name is required")
	ErrInvalidWorkspaceID    = goerr.New("invalid workspace ID format")
	ErrMissingWorkspaceID    = goerr.New("workspace ID is required")
	ErrDuplicateWorkspaceID  = goerr.New("duplicate workspace ID")
	ErrNoConfigFiles         = goerr.New("no configuration files found")
	ErrInvalidWelcomeMessage = goerr.New("invalid Slack welcome message template")
	// ErrWorkspaceEmojiColorConflict is returned when both emoji and color are
	// set in the [workspace] section. They are mutually exclusive because the
	// UI renders either an emoji badge (neutral background) or a colored
	// initials badge, never both.
	ErrWorkspaceEmojiColorConflict = goerr.New("workspace emoji and color are mutually exclusive")
	// ErrInvalidWorkspaceColor is returned when the [workspace] color is not a
	// 6-digit #RRGGBB hex code.
	ErrInvalidWorkspaceColor = goerr.New("invalid workspace color format")
	// ErrInvalidWorkspaceEmoji is returned when the [workspace] emoji exceeds
	// the allowed rune length.
	ErrInvalidWorkspaceEmoji = goerr.New("invalid workspace emoji")
	// ErrInvalidWorkspaceLanguage is returned when the [workspace] language is
	// not one of the supported language codes ("en", "ja").
	ErrInvalidWorkspaceLanguage = goerr.New("invalid workspace language")
	ErrInvalidCaseMode          = goerr.New("invalid case mode")
	ErrInvalidCaseTrigger       = goerr.New("invalid case trigger")
	ErrMissingMonitorChannel    = goerr.New("thread mode requires [slack] channel")
	ErrInvalidMonitorChannel    = goerr.New("invalid Slack channel ID")
	ErrMissingCaseStatus        = goerr.New("thread mode requires [case.status]")
	// ErrReactionRequiresThreadMode is returned when [slack] reaction is set on a
	// workspace that is not in thread mode. Reaction-triggered case creation needs
	// a destination thread, which only thread mode provides.
	ErrReactionRequiresThreadMode = goerr.New("[slack] reaction requires mode = \"thread\"")
	// ErrInvalidReactionEmoji is returned when [slack] reaction, after stripping
	// surrounding colons, is empty or contains characters outside a Slack emoji
	// name.
	ErrInvalidReactionEmoji = goerr.New("invalid Slack reaction emoji name")
	// ErrDuplicateReactionEmoji is returned when the same reaction emoji is
	// configured on more than one workspace, which would make emoji-to-workspace
	// resolution ambiguous.
	ErrDuplicateReactionEmoji = goerr.New("duplicate Slack reaction emoji across workspaces")

	// --- Workspace channel / agent ([slack] workspace_channel + [slack.workspace_agent]) ---

	// ErrWorkspaceChannelRequiresChannelMode is returned when workspace_channel is
	// set on a workspace that is not in channel mode. It names a separate channel
	// to host the cross-case agent; a thread-mode workspace already has one — the
	// monitored channel, where a channel-root mention runs that agent — so a
	// second one would only make channel-to-workspace routing ambiguous.
	// [slack.workspace_agent] itself is accepted in both modes.
	ErrWorkspaceChannelRequiresChannelMode = goerr.New("[slack] workspace_channel requires channel mode")
	// ErrMissingWorkspaceChannel is returned when [slack.workspace_agent] is set
	// on a CHANNEL-mode workspace but [slack] workspace_channel is empty: there
	// the agent runs in the workspace channel and is meaningless without it. In
	// thread mode the section stands alone (the monitored channel hosts it).
	ErrMissingWorkspaceChannel = goerr.New("[slack.workspace_agent] requires [slack] workspace_channel")
	// ErrInvalidWorkspaceChannel is returned when [slack] workspace_channel is not
	// a Slack channel ID (e.g. a channel name).
	ErrInvalidWorkspaceChannel = goerr.New("invalid Slack workspace_channel ID")
	// ErrDuplicateWorkspaceChannel is returned when the same channel ID is used as
	// a workspace_channel on more than one workspace, or collides with another
	// workspace's monitored channel, which would make channel-to-workspace routing
	// ambiguous.
	ErrDuplicateWorkspaceChannel = goerr.New("duplicate Slack workspace_channel across workspaces")
	// ErrWorkspaceAgentPromptConflict is returned when [slack.workspace_agent]
	// sets both prompt and prompt_file (mutually exclusive).
	ErrWorkspaceAgentPromptConflict = goerr.New("[slack.workspace_agent] prompt and prompt_file are mutually exclusive")
	// ErrWorkspaceAgentPromptEmpty is returned when [slack.workspace_agent]
	// prompt_file resolves to an empty file.
	ErrWorkspaceAgentPromptEmpty = goerr.New("[slack.workspace_agent] prompt_file is empty")
	// ErrCasePromptConflict is returned when a [case.prompts] phase sets both
	// its inline key and its _file form (e.g. mention and mention_file).
	ErrCasePromptConflict = goerr.New("[case.prompts] inline prompt and its _file form are mutually exclusive")
	// ErrCasePromptEmpty is returned when a [case.prompts] _file resolves to an
	// empty file.
	ErrCasePromptEmpty = goerr.New("[case.prompts] prompt file is empty")
	// ErrMissingReferenceWorkspace is returned when a case_ref /
	// multi_case_ref field omits reference_workspace.
	ErrMissingReferenceWorkspace = goerr.New("case_ref field requires reference_workspace")
	// ErrUnexpectedReferenceWorkspace is returned when reference_workspace is set
	// on a field whose type is not a case_ref type.
	ErrUnexpectedReferenceWorkspace = goerr.New("reference_workspace is only valid for case_ref fields")
	// ErrUnknownReferenceWorkspace is returned when reference_workspace points at
	// a workspace ID that is not defined across the loaded configs.
	ErrUnknownReferenceWorkspace = goerr.New("reference_workspace points to an unknown workspace")
	// ErrRequiredCaseRefUnsupported is returned when a case_ref / multi_case_ref
	// field is marked required: the Slack case-creation modal cannot collect a
	// case reference, so a required one would make the case un-creatable.
	ErrRequiredCaseRefUnsupported = goerr.New("case_ref fields cannot be required")

	// --- Workspace authorization ([authz]) ---

	// ErrAuthzPolicyEmpty is returned when [authz] is present but lists no
	// policy path, or lists an empty one.
	ErrAuthzPolicyEmpty = goerr.New("[authz] policy must list at least one non-empty path")
	// ErrAuthzPolicyTrialFailed is returned when a compiled [authz] policy
	// errors while being evaluated against the sample inputs at load time
	// (no package authz, a non-boolean allow, a rule producing conflicting
	// values, ...). The Rego error is joined to it.
	ErrAuthzPolicyTrialFailed = goerr.New("[authz] policy failed a trial evaluation")

	// --- Global config ([[workspace_group]]) ---

	// ErrMissingWorkspaceGroupID is returned when a [[workspace_group]] omits id.
	ErrMissingWorkspaceGroupID = goerr.New("workspace group ID is required")
	// ErrInvalidWorkspaceGroupID is returned when a workspace group id does not
	// match the allowed pattern or exceeds the length limit.
	ErrInvalidWorkspaceGroupID = goerr.New("invalid workspace group ID format")
	// ErrDuplicateWorkspaceGroupID is returned when the same workspace group id
	// is defined more than once across the global config files.
	ErrDuplicateWorkspaceGroupID = goerr.New("duplicate workspace group ID")
	// ErrDuplicateGroupMember is returned when the same workspace id appears more
	// than once in a single group's members list.
	ErrDuplicateGroupMember = goerr.New("duplicate workspace group member")
	// ErrUnknownGroupMember is returned when a group member references a
	// workspace id that is not defined across the loaded workspace configs.
	ErrUnknownGroupMember = goerr.New("workspace group member references an unknown workspace")
	// ErrGlobalConfigContainsWorkspace is returned when a --global-config file
	// contains a [workspace] section. Workspace definitions belong under
	// --config (1 file = 1 workspace); the global config is for deployment-wide
	// settings only, so mixing the two is rejected loudly rather than ignored.
	ErrGlobalConfigContainsWorkspace = goerr.New("global config file must not contain a [workspace] section")

	// --- Global config ([export]) ---

	// ErrInvalidExportConfig is returned when the [export] section is malformed,
	// e.g. a missing [export.bigquery] project.
	ErrInvalidExportConfig = goerr.New("invalid export configuration")
	// ErrInvalidExportDataset is returned when an [[export.bigquery.workspace]]
	// dataset name is not a valid BigQuery dataset ID (BigQuery forbids hyphens).
	ErrInvalidExportDataset = goerr.New("invalid export dataset name")
	// ErrDuplicateExportWorkspace is returned when the same workspace id — or the
	// same dataset name — appears more than once in the export config.
	ErrDuplicateExportWorkspace = goerr.New("duplicate export workspace mapping")
	// ErrUnknownExportWorkspace is returned when an export workspace mapping
	// references a workspace id not defined across the loaded workspace configs.
	ErrUnknownExportWorkspace = goerr.New("export workspace mapping references an unknown workspace")
	// ErrDuplicateExportConfig is returned when more than one global config file
	// defines an [export] section.
	ErrDuplicateExportConfig = goerr.New("duplicate [export] section across global config files")

	// --- Global config ([agent], [[llm_model]]) ---

	// ErrDuplicateAgentConfig is returned when more than one global config file
	// defines an [agent] section. Like [export], the deployment-wide agent
	// settings must have a single home.
	ErrDuplicateAgentConfig = goerr.New("duplicate [agent] section across global config files")
	// ErrInvalidBudget is returned when a configured budget is negative, or so
	// small it rounds away to zero (which would read as "not set").
	ErrInvalidBudget = goerr.New("invalid budget")
	// ErrInvalidLLMModel is returned when an [[llm_model]] entry is malformed,
	// e.g. it names no model.
	ErrInvalidLLMModel = goerr.New("invalid [[llm_model]] entry")
	// ErrInvalidLLMModelRef is returned when a model reference name — an
	// [[llm_model]] alias, or a Job's llm_model — does not match the allowed
	// pattern or exceeds the length limit.
	ErrInvalidLLMModelRef = goerr.New("invalid model reference name")
	// ErrInvalidLLMModelPrice is returned when an [[llm_model]] price is
	// negative, missing where required, or too small to represent.
	ErrInvalidLLMModelPrice = goerr.New("invalid model price")
	// ErrDuplicateLLMModelRef is returned when the same model reference name is
	// defined more than once across the global config files.
	ErrDuplicateLLMModelRef = goerr.New("duplicate model reference name")
	// ErrUnknownLLMModelRef is returned when a Job — or the --llm-model flag —
	// names a model that no [[llm_model]] entry defines.
	ErrUnknownLLMModelRef = goerr.New("model reference name is not defined")
	// ErrUnexpectedSemantic is returned when a field sets semantic on a type
	// other than text.
	ErrUnexpectedSemantic = goerr.New("semantic is only valid for text fields")
	// ErrUnknownSemantic is returned when a field names a semantic that the
	// catalog (pkg/domain/semantic) does not define.
	ErrUnknownSemantic = goerr.New("unknown semantic")
	// ErrUnexpectedPattern is returned when a field sets validation.pattern on
	// a type other than text.
	ErrUnexpectedPattern = goerr.New("validation.pattern is only valid for text fields")
	// ErrInvalidPattern is returned when a field's validation.pattern is not a
	// valid Go RE2 regular expression.
	ErrInvalidPattern = goerr.New("invalid validation.pattern")
)

// Context keys for error values
const (
	ConfigPathKey       = "config_path"
	FieldIDKey          = "field_id"
	FieldTypeKey        = "field_type"
	SemanticKey         = "semantic"
	PatternKey          = "pattern"
	OptionIDKey         = "option_id"
	FieldIndexKey       = "field_index"
	OptionIndexKey      = "option_index"
	WorkspaceIDKey      = "workspace_id"
	WorkspaceColorKey   = "workspace_color"
	WorkspaceEmojiKey   = "workspace_emoji"
	WorkspaceEmojiLen   = "workspace_emoji_len"
	WorkspaceLangKey    = "language"
	WorkspaceGroupIDKey = "workspace_group_id"
	GroupMemberKey      = "group_member"
	ExportDatasetKey    = "dataset"
	LLMModelRefKey      = "llm_model"
)
