package usecase

import (
	"context"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
)

// tokenActor resolves the acting user for private-Case access control from the
// context auth token. checkAccess is false when the context carries no token
// (system / agent contexts), which bypasses the access check per project policy.
func tokenActor(ctx context.Context) (actorID string, checkAccess bool) {
	if token, err := auth.TokenFromContext(ctx); err == nil {
		return token.Sub, true
	}
	return "", false
}

// tokenUserID returns the actor the context auth token names, for
// ATTRIBUTION — a "<@U...>" change line, a "created by" record. It is "" when
// there is no token, and also when the token names a case channel
// (model.CaseChannelActorID): that actor is not a person, so its changes are
// attributed to the system. Access control must keep using the token's Sub as
// is (tokenActor).
func tokenUserID(ctx context.Context) string {
	token, err := auth.TokenFromContext(ctx)
	if err != nil || model.IsCaseChannelActor(token.Sub) {
		return ""
	}
	return token.Sub
}

// assertCaseWriteAccess is the single gate enforcing private-Case write access
// control against an already-loaded Case. Every Case write path funnels its
// access check through here so a new write path cannot structurally miss it.
// It denies with ErrAccessDenied when checkAccess is true and actorID cannot
// access c; checkAccess=false (no identified actor: system/agent context)
// bypasses the check.
//
// A private DRAFT case has no Slack channel yet, so its ChannelUserIDs are empty
// and model.IsCaseAccessible would lock out everyone including the owner. For a
// private draft, access therefore falls back to the reporter. A public draft is
// workspace-shared and accessible to anyone. This draft-aware behaviour was
// previously duplicated in UpdateCase / assertCaseEditable; unifying it here is
// strictly more permissive only for the reporter of their own private draft and
// never widens access to a non-owner.
func assertCaseWriteAccess(c *model.Case, actorID string, checkAccess bool) error {
	// Fail closed and loud on a nil case rather than panicking downstream in
	// model.IsCaseAccessible (which dereferences c). Every real caller passes a
	// Get-loaded, error-checked Case, so this only guards against future misuse.
	if c == nil {
		return goerr.Wrap(ErrCaseNotFound, "case is nil")
	}
	if !checkAccess {
		return nil
	}
	if c.IsDraft() {
		if c.IsPrivate && c.ReporterID != actorID {
			return goerr.Wrap(ErrAccessDenied, "cannot access private draft case",
				goerr.V(CaseIDKey, c.ID), goerr.V("user_id", actorID))
		}
		return nil
	}
	if !model.IsCaseAccessible(c, actorID) {
		return goerr.Wrap(ErrAccessDenied, "cannot access private case",
			goerr.V(CaseIDKey, c.ID), goerr.V("user_id", actorID))
	}
	return nil
}

// loadCaseForWrite loads a Case by id and enforces private-Case write access
// control against the context auth token. It is the shared "Get + access gate"
// behind every token-driven Case write path (CaseUseCase and MemoUseCase), so a
// new write path structurally cannot forget the check. A missing case is
// reported as ErrCaseNotFound. The repository is passed explicitly so the same
// helper serves every usecase that owns an interfaces.Repository.
func loadCaseForWrite(ctx context.Context, repo interfaces.Repository, workspaceID string, id int64) (*model.Case, error) {
	c, err := repo.Case().Get(ctx, workspaceID, id)
	if err != nil {
		return nil, goerr.Wrap(ErrCaseNotFound, "case not found", goerr.V(CaseIDKey, id))
	}
	actorID, checkAccess := tokenActor(ctx)
	if err := assertCaseWriteAccess(c, actorID, checkAccess); err != nil {
		return nil, err
	}
	return c, nil
}
