package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/m-mizutani/goerr/v2"

	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/authz"
)

// WorkspaceAccessCacheConfig bounds the in-memory decision cache. Values come
// from the caller (pkg/cli); this package holds no defaults.
type WorkspaceAccessCacheConfig struct {
	TTL  time.Duration // lifetime of a cached decision
	Size int           // max Slack users whose decisions are cached
}

// WorkspaceAccessUseCase is the single implementation of the per-workspace
// authorization decision. A workspace without a policy is accessible to
// everyone; for the others the policy is evaluated against the user's record
// from the existing Slack user sync.
type WorkspaceAccessUseCase struct {
	registry *model.WorkspaceRegistry
	policies map[string]interfaces.PolicyClient // workspace ID -> policy; read-only after construction
	users    interfaces.SlackUserRepository

	// decisions maps a Slack user ID to that user's allow decision for every
	// workspace that has a policy. It is held in process memory on purpose,
	// as an exception to the multi-instance rule against unshared business-data
	// caches: an entry is derived data, recomputable at any time from the
	// synced user record and the policies, and its staleness is bounded by the
	// TTL on every instance alike. A shared backend would give the same bound
	// at the cost of a read per request (the same trade-off as authCache).
	decisions *expirable.LRU[string, map[string]bool]
}

var _ interfaces.WorkspaceAuthorizer = (*WorkspaceAccessUseCase)(nil)

// NewWorkspaceAccessUseCase builds the authorizer. With no policies it allows
// everything and needs neither the registry nor the user repository.
func NewWorkspaceAccessUseCase(
	registry *model.WorkspaceRegistry,
	policies map[string]interfaces.PolicyClient,
	users interfaces.SlackUserRepository,
	cfg WorkspaceAccessCacheConfig,
) (*WorkspaceAccessUseCase, error) {
	uc := &WorkspaceAccessUseCase{registry: registry, users: users}
	if len(policies) == 0 {
		return uc, nil
	}

	if registry == nil {
		return nil, goerr.New("workspace registry is required when a workspace has an authz policy")
	}
	if users == nil {
		return nil, goerr.New("Slack user repository is required when a workspace has an authz policy")
	}
	if cfg.TTL <= 0 || cfg.Size <= 0 {
		return nil, goerr.New("workspace access cache TTL and size must be positive",
			goerr.V("ttl", cfg.TTL), goerr.V("size", cfg.Size))
	}
	copied := make(map[string]interfaces.PolicyClient, len(policies))
	for wsID, p := range policies {
		if _, err := registry.Get(wsID); err != nil {
			return nil, goerr.Wrap(err, "authz policy names an unregistered workspace",
				goerr.V("workspace_id", wsID))
		}
		if p == nil {
			return nil, goerr.New("authz policy is nil", goerr.V("workspace_id", wsID))
		}
		copied[wsID] = p
	}
	uc.policies = copied
	uc.decisions = expirable.NewLRU[string, map[string]bool](cfg.Size, nil, cfg.TTL)
	return uc, nil
}

// Authorize returns nil when slackUserID may access workspaceID, an error
// wrapping model.ErrWorkspaceAccessDenied when the policy denies it, and any
// other error when the decision could not be made.
func (uc *WorkspaceAccessUseCase) Authorize(ctx context.Context, workspaceID, slackUserID string) error {
	if _, ok := uc.policies[workspaceID]; !ok {
		return nil
	}
	if slackUserID == "" {
		return goerr.Wrap(model.ErrWorkspaceAccessDenied, "workspace access requires a Slack user",
			goerr.V("workspace_id", workspaceID))
	}
	// A case-channel actor runs an agent turn an app (a Slack workflow, an
	// integration bot) started in a case channel. Bot-authored events are not
	// checked against the policy (eventActorAllowed), and this actor is how such
	// a turn carries on; evaluating it would ask the policy about a "user" that
	// does not exist.
	if model.IsCaseChannelActor(slackUserID) {
		return nil
	}
	decisions, err := uc.decisionsFor(ctx, slackUserID)
	if err != nil {
		return goerr.Wrap(err, "decide workspace access",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", slackUserID))
	}
	if !decisions[workspaceID] {
		return goerr.Wrap(model.ErrWorkspaceAccessDenied, "workspace policy denied the user",
			goerr.V("workspace_id", workspaceID), goerr.V("user_id", slackUserID))
	}
	return nil
}

// AuthorizeCurrentUser applies Authorize to the auth token's Sub, and does not
// check at all when ctx carries no token (a system context), matching the
// private-case access control convention.
func (uc *WorkspaceAccessUseCase) AuthorizeCurrentUser(ctx context.Context, workspaceID string) error {
	token, err := auth.TokenFromContext(ctx)
	if err != nil {
		return nil
	}
	return uc.Authorize(ctx, workspaceID, token.Sub)
}

// FilterAccessible returns the entries slackUserID may access, in the input order.
func (uc *WorkspaceAccessUseCase) FilterAccessible(ctx context.Context, entries []*model.WorkspaceEntry, slackUserID string) ([]*model.WorkspaceEntry, error) {
	out := make([]*model.WorkspaceEntry, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		err := uc.Authorize(ctx, e.Workspace.ID, slackUserID)
		switch {
		case err == nil:
			out = append(out, e)
		case isWorkspaceAccessDenied(err):
			// Filtered out.
		default:
			return nil, err
		}
	}
	return out, nil
}

// FilterAccessibleForCurrentUser applies FilterAccessible to the auth token's
// Sub, and returns entries unchanged when ctx carries no token.
func (uc *WorkspaceAccessUseCase) FilterAccessibleForCurrentUser(ctx context.Context, entries []*model.WorkspaceEntry) ([]*model.WorkspaceEntry, error) {
	token, err := auth.TokenFromContext(ctx)
	if err != nil {
		return entries, nil
	}
	return uc.FilterAccessible(ctx, entries, token.Sub)
}

// decisionsFor evaluates every policy for one user at once, so listing the
// workspaces costs one record read however many of them have a policy.
func (uc *WorkspaceAccessUseCase) decisionsFor(ctx context.Context, slackUserID string) (map[string]bool, error) {
	if d, ok := uc.decisions.Get(slackUserID); ok {
		return d, nil
	}

	user, found, err := uc.buildUser(ctx, slackUserID)
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(uc.policies))
	for wsID, p := range uc.policies {
		entry, err := uc.registry.Get(wsID)
		if err != nil {
			return nil, goerr.Wrap(err, "look up workspace of an authz policy", goerr.V("workspace_id", wsID))
		}
		var res authz.WorkspaceResult
		in := authz.WorkspaceInput{
			Workspace: authz.WorkspaceRef{ID: entry.Workspace.ID, Name: entry.Workspace.Name},
			User:      user,
		}
		if err := p.Query(ctx, authz.WorkspaceQuery, in, &res); err != nil {
			return nil, goerr.Wrap(err, "evaluate workspace authz policy", goerr.V("workspace_id", wsID))
		}
		out[wsID] = res.Allow
	}

	// A missing record is expected while the Slack user sync is between its
	// DeleteAll and SaveMany, so a decision made on the empty shape is not
	// kept: caching it would lock the user out for a whole TTL.
	if found {
		uc.decisions.Add(slackUserID, out)
	}
	return out, nil
}

// buildUser returns found=false when no synced record exists for slackUserID;
// every field but ID is then "".
func (uc *WorkspaceAccessUseCase) buildUser(ctx context.Context, slackUserID string) (authz.WorkspaceUser, bool, error) {
	rec, err := uc.users.GetByID(ctx, model.SlackUserID(slackUserID))
	if err != nil {
		if isRepoNotFound(err) {
			return authz.WorkspaceUser{ID: slackUserID}, false, nil
		}
		return authz.WorkspaceUser{}, false, goerr.Wrap(err, "load synced Slack user for workspace authz",
			goerr.V("user_id", slackUserID))
	}
	return authz.WorkspaceUser{
		ID:          slackUserID,
		Email:       rec.Email,
		Name:        rec.Name,
		DisplayName: rec.RealName,
	}, true, nil
}

// allowAllWorkspaces is the authorizer a usecase holds until New wires the
// configured one: with no policy registered it allows every workspace. It keeps
// a usecase built directly (as tests do) from needing a nil check.
func allowAllWorkspaces() interfaces.WorkspaceAuthorizer {
	return &WorkspaceAccessUseCase{}
}

func isWorkspaceAccessDenied(err error) bool {
	return errors.Is(err, model.ErrWorkspaceAccessDenied)
}
