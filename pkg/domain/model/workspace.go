package model

import (
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	"github.com/m-mizutani/hecatoncheires/pkg/i18n"
)

// Workspace represents a workspace's identity
type Workspace struct {
	ID          string
	Name        string
	Description string // Human-readable description (e.g. for AI workspace estimation, UI tooltips)
	// Emoji is an optional display glyph for the workspace badge. Mutually
	// exclusive with Color (enforced at config load). Empty when unset.
	Emoji string
	// Color is an optional #RRGGBB hex used as the workspace badge background.
	// Mutually exclusive with Emoji (enforced at config load). Empty when unset.
	Color string
}

// ErrWorkspaceNotFound is returned when a workspace is not found in the registry
var ErrWorkspaceNotFound = goerr.New("workspace not found")

// ErrWorkspaceAccessDenied is returned when a workspace's authorization policy
// does not allow the acting Slack user.
var ErrWorkspaceAccessDenied = goerr.New("workspace access denied")

// WorkspaceEntry holds workspace identity and its field schema
type WorkspaceEntry struct {
	Workspace            Workspace
	FieldSchema          *config.FieldSchema
	MemoConfig           *config.MemoConfig // Memo "strong definition" + custom field schema (nil/empty when memos disabled)
	ActionStatusSet      *ActionStatusSet
	SlackChannelPrefix   string
	SlackTeamID          string // Slack Team ID for org-level app support (empty for WS-level apps)
	SlackInviteUsers     []string
	SlackInviteGroups    []string
	SlackWelcomeMessages []string // Go text/template strings posted to the case channel after creation
	CompilePrompt        string
	AssistPrompt         string
	AssistLanguage       string
	// Language is the workspace's output language from [workspace] language.
	// It takes priority over each Slack user's locale; empty when unset.
	Language i18n.Lang
	// CaseCreatePrompt is the workspace-specific additional prompt for the
	// thread-mode case initialization (create) agent, configured via TOML
	// [case.prompts].create. Empty when unset; appended to the ModeCreate
	// planner system prompt.
	CaseCreatePrompt string
	Jobs             []*Job // Event-driven agent jobs loaded from workspace TOML

	// CaseMode selects channel-per-case (default) or thread-per-case binding.
	CaseMode CaseMode
	// CaseTrigger selects what starts a Case in thread mode: instant (default,
	// every channel-root post) or mention (only an @mention of the bot). Only
	// meaningful when CaseMode is thread.
	CaseTrigger CaseTrigger
	// SlackMonitorChannelID is the channel watched for thread-mode case
	// creation. Required (and only meaningful) when CaseMode is thread.
	SlackMonitorChannelID string
	// AcceptBot opts the thread-mode monitored channel into
	// treating bot-authored channel-root posts (e.g. an intake-form app's
	// relayed request) as case-creation triggers. Default false: only human
	// channel-root posts start a case, so the channel is not flooded with a case
	// per bot notification. When true, every bot root post (bot_message /
	// bot_id) starts a case.
	AcceptBot bool
	// CaseStatusSet is the configurable workflow status set that attaches to
	// Cases in thread mode (the Kanban columns). Non-nil only for thread-mode
	// workspaces; reuses the generic ActionStatusSet value type.
	CaseStatusSet *ActionStatusSet
	// ReactionEmoji is the Slack reaction (emoji name, without surrounding
	// colons) that triggers case creation for this workspace. Empty when the
	// reaction trigger is disabled. Only meaningful in thread mode, and unique
	// across workspaces (enforced at config load).
	ReactionEmoji string
	// SlackWorkspaceChannelID is the workspace-level shared channel where the
	// cross-case workspace agent runs (and future notifications flow). Channel
	// mode only; empty when unset. Unique across workspaces / monitor channels
	// (enforced at config load).
	SlackWorkspaceChannelID string
	// WorkspaceAgentPrompt is the operator-supplied custom instruction for the
	// workspace agent (from [slack.workspace_agent] prompt/prompt_file). It is
	// appended to the host-owned base system prompt and cannot relax it. Empty
	// when unset.
	WorkspaceAgentPrompt string
	// AllowExistingSlackChannel lets case creation link an existing Slack
	// channel instead of creating one ([slack] allow_existing_channel).
	// Channel mode only; always false for a thread-mode workspace.
	AllowExistingSlackChannel bool
}

// IsThreadMode reports whether this workspace uses thread-per-case binding.
func (e *WorkspaceEntry) IsThreadMode() bool {
	return e != nil && e.CaseMode.IsThread()
}

// WorkspaceRegistry holds workspace configurations.
// It does not hold Repository or UseCase instances (settings only).
type WorkspaceRegistry struct {
	entries map[string]*WorkspaceEntry
	order   []string // preserves registration order
}

// NewWorkspaceRegistry creates a new empty WorkspaceRegistry
func NewWorkspaceRegistry() *WorkspaceRegistry {
	return &WorkspaceRegistry{
		entries: make(map[string]*WorkspaceEntry),
	}
}

// Register adds a workspace entry to the registry
func (r *WorkspaceRegistry) Register(entry *WorkspaceEntry) {
	if _, exists := r.entries[entry.Workspace.ID]; !exists {
		r.order = append(r.order, entry.Workspace.ID)
	}
	r.entries[entry.Workspace.ID] = entry
}

// Get retrieves a workspace entry by ID
func (r *WorkspaceRegistry) Get(workspaceID string) (*WorkspaceEntry, error) {
	entry, ok := r.entries[workspaceID]
	if !ok {
		return nil, goerr.Wrap(ErrWorkspaceNotFound, "workspace not found",
			goerr.V("workspace_id", workspaceID))
	}
	return entry, nil
}

// Language returns the configured output language of the workspace, or an
// empty string when the workspace sets none or is not registered. It reports
// no error for an unregistered workspace: looking up the language only picks
// a language, and each caller's own lookup already reports a missing
// workspace. A nil registry returns an empty string.
func (r *WorkspaceRegistry) Language(workspaceID string) i18n.Lang {
	if r == nil {
		return ""
	}
	entry, ok := r.entries[workspaceID]
	if !ok {
		return ""
	}
	return entry.Language
}

// FindByMonitorChannel returns the thread-mode workspace entry whose monitored
// channel matches channelID. It only considers thread-mode workspaces; the
// boolean is false when no thread-mode workspace watches the channel.
func (r *WorkspaceRegistry) FindByMonitorChannel(channelID string) (*WorkspaceEntry, bool) {
	if r == nil || channelID == "" {
		return nil, false
	}
	for _, id := range r.order {
		entry := r.entries[id]
		if entry.IsThreadMode() && entry.SlackMonitorChannelID == channelID {
			return entry, true
		}
	}
	return nil, false
}

// FindByWorkspaceChannel returns the channel-mode workspace entry whose
// workspace channel matches channelID. Thread-mode workspaces are never matched
// (the workspace channel is a channel-mode feature). The boolean is false when
// no channel-mode workspace uses the channel.
func (r *WorkspaceRegistry) FindByWorkspaceChannel(channelID string) (*WorkspaceEntry, bool) {
	if r == nil || channelID == "" {
		return nil, false
	}
	for _, id := range r.order {
		entry := r.entries[id]
		if !entry.IsThreadMode() && entry.SlackWorkspaceChannelID == channelID {
			return entry, true
		}
	}
	return nil, false
}

// FindByReactionEmoji returns the thread-mode workspace entry whose reaction
// emoji matches. It only considers thread-mode workspaces; the boolean is false
// when no thread-mode workspace uses the emoji. emoji must already be normalized
// (no surrounding colons); an empty emoji never matches.
func (r *WorkspaceRegistry) FindByReactionEmoji(emoji string) (*WorkspaceEntry, bool) {
	if r == nil || emoji == "" {
		return nil, false
	}
	for _, id := range r.order {
		entry := r.entries[id]
		if entry.IsThreadMode() && entry.ReactionEmoji == emoji {
			return entry, true
		}
	}
	return nil, false
}

// IsConfiguredChannel reports whether channelID is any workspace's thread-mode
// monitor channel or channel-mode workspace channel. Such a channel already
// routes Slack events for its configured purpose, so it can never become a
// case channel. A nil registry or an empty channelID returns false.
func (r *WorkspaceRegistry) IsConfiguredChannel(channelID string) bool {
	if _, ok := r.FindByMonitorChannel(channelID); ok {
		return true
	}
	_, ok := r.FindByWorkspaceChannel(channelID)
	return ok
}

// List returns all registered workspace entries in registration order
func (r *WorkspaceRegistry) List() []*WorkspaceEntry {
	result := make([]*WorkspaceEntry, 0, len(r.order))
	for _, id := range r.order {
		result = append(result, r.entries[id])
	}
	return result
}

// Workspaces returns all registered workspaces in registration order
func (r *WorkspaceRegistry) Workspaces() []Workspace {
	result := make([]Workspace, 0, len(r.order))
	for _, id := range r.order {
		result = append(result, r.entries[id].Workspace)
	}
	return result
}
