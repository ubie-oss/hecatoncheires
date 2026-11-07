package usecase_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"

	"github.com/m-mizutani/hecatoncheires/pkg/adapter/policy"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/auth"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/async"
)

// countingUsers delegates to a real repository and counts GetByID calls; it
// fails GetByID while failing is set.
type countingUsers struct {
	interfaces.SlackUserRepository
	calls   atomic.Int32
	failing atomic.Bool
}

var errUserStoreDown = errors.New("user store down")

func (c *countingUsers) GetByID(ctx context.Context, id model.SlackUserID) (*model.SlackUser, error) {
	c.calls.Add(1)
	if c.failing.Load() {
		return nil, goerr.Wrap(errUserStoreDown, "get slack user")
	}
	return c.SlackUserRepository.GetByID(ctx, id)
}

func compileAuthzPolicy(t *testing.T, src string) interfaces.PolicyClient {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authz.rego")
	gt.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	pc, err := policy.New([]string{path})
	gt.NoError(t, err)
	return pc
}

// denyingAccess builds a real authorizer over reg in which every workspace
// named in denied has a policy that allows nobody; the rest have no policy.
func denyingAccess(t *testing.T, reg *model.WorkspaceRegistry, denied ...string) interfaces.WorkspaceAuthorizer {
	t.Helper()
	pc := compileAuthzPolicy(t, policyDenyAll)
	policies := make(map[string]interfaces.PolicyClient, len(denied))
	for _, id := range denied {
		policies[id] = pc
	}
	access, err := usecase.NewWorkspaceAccessUseCase(reg, policies, memory.New().SlackUser(),
		usecase.WorkspaceAccessCacheConfig{TTL: time.Minute, Size: 16})
	gt.NoError(t, err).Required()
	return access
}

func authzRegistry(ids ...string) *model.WorkspaceRegistry {
	reg := model.NewWorkspaceRegistry()
	for _, id := range ids {
		reg.Register(&model.WorkspaceEntry{Workspace: model.Workspace{ID: id, Name: "WS " + id}})
	}
	return reg
}

func saveSlackUser(t *testing.T, users interfaces.SlackUserRepository, u *model.SlackUser) {
	t.Helper()
	gt.NoError(t, users.SaveMany(context.Background(), []*model.SlackUser{u}))
}

type accessFixture struct {
	uc    *usecase.WorkspaceAccessUseCase
	users *countingUsers
	reg   *model.WorkspaceRegistry
}

func newAccessFixture(t *testing.T, ttl time.Duration, policies map[string]string, wsIDs ...string) accessFixture {
	t.Helper()
	reg := authzRegistry(wsIDs...)
	users := &countingUsers{SlackUserRepository: memory.New().SlackUser()}
	compiled := make(map[string]interfaces.PolicyClient, len(policies))
	for id, src := range policies {
		compiled[id] = compileAuthzPolicy(t, src)
	}
	uc, err := usecase.NewWorkspaceAccessUseCase(reg, compiled, users, usecase.WorkspaceAccessCacheConfig{TTL: ttl, Size: 16})
	gt.NoError(t, err)
	return accessFixture{uc: uc, users: users, reg: reg}
}

const (
	policyAllowAll = "package authz\n\nallow := true\n"
	policyDenyAll  = "package authz\n\nallow := false\n"
	policyNever    = "package authz\n\nallow if false\n"
	policyByEmail  = "package authz\n\nallow if input.user.email == \"alice@example.com\"\n"
	policyNonBool  = "package authz\n\nallow := \"yes\"\n"
)

func TestWorkspaceAccess_NoPolicyAllowsWithoutReading(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"secured": policyDenyAll}, "open", "secured")

	gt.NoError(t, f.uc.Authorize(context.Background(), "open", "U1"))
	gt.Value(t, f.users.calls.Load()).Equal(int32(0))
}

func TestWorkspaceAccess_NoPolicyAnywhereFilterKeepsAll(t *testing.T) {
	reg := authzRegistry("a", "b", "c")
	users := &countingUsers{SlackUserRepository: memory.New().SlackUser()}
	uc, err := usecase.NewWorkspaceAccessUseCase(reg, nil, users, usecase.WorkspaceAccessCacheConfig{})
	gt.NoError(t, err)

	got, err := uc.FilterAccessible(context.Background(), reg.List(), "U1")
	gt.NoError(t, err)
	gt.Value(t, len(got)).Equal(3)
	for i, e := range reg.List() {
		gt.Value(t, got[i].Workspace.ID).Equal(e.Workspace.ID)
	}
	gt.Value(t, users.calls.Load()).Equal(int32(0))
}

// A case-channel actor is what an agent turn started by an app acts as. It is
// not a person, so the workspace policy — which is about people — is not asked
// about it in its own workspace. Every other workspace with a policy refuses
// it, so a cross-workspace tool cannot use it to reach a closed workspace.
func TestWorkspaceAccess_CaseChannelActorIsLimitedToItsWorkspace(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{
		"secured": policyDenyAll,
		"other":   policyDenyAll,
	}, "secured", "other", "open")
	ctx := context.Background()
	actor := model.CaseChannelActorID("secured", "C0CASE01")

	gt.NoError(t, f.uc.Authorize(ctx, "secured", actor))
	gt.Error(t, f.uc.Authorize(ctx, "other", actor)).Is(model.ErrWorkspaceAccessDenied)
	// A workspace with no policy stays open to everyone.
	gt.NoError(t, f.uc.Authorize(ctx, "open", actor))

	tokenCtx := auth.ContextWithToken(ctx, &auth.Token{Sub: actor})
	gt.NoError(t, f.uc.AuthorizeCurrentUser(tokenCtx, "secured"))
	gt.Error(t, f.uc.AuthorizeCurrentUser(tokenCtx, "other")).Is(model.ErrWorkspaceAccessDenied)

	got, err := f.uc.FilterAccessibleForCurrentUser(tokenCtx, f.reg.List())
	gt.NoError(t, err).Required()
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.Workspace.ID)
	}
	gt.Value(t, ids).Equal([]string{"secured", "open"})
	gt.Value(t, f.users.calls.Load()).Equal(int32(0))

	// A person is still checked.
	gt.Error(t, f.uc.Authorize(ctx, "secured", "U1")).Is(model.ErrWorkspaceAccessDenied)
}

func TestWorkspaceAccess_AllowValues(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{
		"allow": policyAllowAll,
		"deny":  policyDenyAll,
		"never": policyNever,
	}, "allow", "deny", "never")
	ctx := context.Background()

	gt.NoError(t, f.uc.Authorize(ctx, "allow", "U1"))
	gt.Error(t, f.uc.Authorize(ctx, "deny", "U1")).Is(model.ErrWorkspaceAccessDenied)
	gt.Error(t, f.uc.Authorize(ctx, "never", "U1")).Is(model.ErrWorkspaceAccessDenied)
}

func TestWorkspaceAccess_InputFields(t *testing.T) {
	cases := []struct {
		name string
		rule string
	}{
		{"workspace.id", `input.workspace.id == "ws"`},
		{"workspace.name", `input.workspace.name == "WS ws"`},
		{"user.id", `input.user.id == "U1"`},
		{"user.email", `input.user.email == "alice@example.com"`},
		{"user.name", `input.user.name == "alice"`},
		{"user.display_name", `input.user.display_name == "Alice"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccessFixture(t, time.Minute, map[string]string{
				"ws": "package authz\n\nallow if " + tc.rule + "\n",
			}, "ws")
			saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "alice@example.com", Name: "alice", RealName: "Alice"})
			saveSlackUser(t, f.users, &model.SlackUser{ID: "U2", Email: "bob@example.com", Name: "bob", RealName: "Bob"})
			ctx := context.Background()

			gt.NoError(t, f.uc.Authorize(ctx, "ws", "U1"))
			if tc.name != "workspace.id" && tc.name != "workspace.name" {
				gt.Error(t, f.uc.Authorize(ctx, "ws", "U2")).Is(model.ErrWorkspaceAccessDenied)
			}
		})
	}
}

func TestWorkspaceAccess_UnsyncedUserHasEmptyFields(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{
		"ws": `package authz

allow if {
	input.user.id == "U9"
	input.user.email == ""
	input.user.name == ""
	input.user.display_name == ""
}
`,
	}, "ws")
	gt.NoError(t, f.uc.Authorize(context.Background(), "ws", "U9"))
}

func TestWorkspaceAccess_UnsyncedDecisionIsNotCached(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyByEmail}, "ws")
	ctx := context.Background()

	gt.Error(t, f.uc.Authorize(ctx, "ws", "U1")).Is(model.ErrWorkspaceAccessDenied)

	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "alice@example.com"})
	gt.NoError(t, f.uc.Authorize(ctx, "ws", "U1"))
}

func TestWorkspaceAccess_OneReadPerUserAcrossWorkspaces(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"a": policyAllowAll, "b": policyDenyAll}, "a", "b")
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "alice@example.com"})
	ctx := context.Background()

	gt.NoError(t, f.uc.Authorize(ctx, "a", "U1"))
	gt.Error(t, f.uc.Authorize(ctx, "b", "U1")).Is(model.ErrWorkspaceAccessDenied)
	gt.Value(t, f.users.calls.Load()).Equal(int32(1))
}

func TestWorkspaceAccess_CachedWithinTTL(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyByEmail}, "ws")
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "alice@example.com"})
	ctx := context.Background()

	gt.NoError(t, f.uc.Authorize(ctx, "ws", "U1"))
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "mallory@example.com"})
	gt.NoError(t, f.uc.Authorize(ctx, "ws", "U1"))
}

func TestWorkspaceAccess_ExpiresAfterTTL(t *testing.T) {
	f := newAccessFixture(t, 10*time.Millisecond, map[string]string{"ws": policyByEmail}, "ws")
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "alice@example.com"})
	ctx := context.Background()

	gt.NoError(t, f.uc.Authorize(ctx, "ws", "U1"))
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "mallory@example.com"})
	// expirable.LRU reads the wall clock and takes no injected one, so the TTL
	// has to actually elapse here.
	time.Sleep(50 * time.Millisecond)
	gt.Error(t, f.uc.Authorize(ctx, "ws", "U1")).Is(model.ErrWorkspaceAccessDenied)
}

func TestWorkspaceAccess_ReadFailureIsAnErrorNotADenial(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyAllowAll}, "ws")
	f.users.failing.Store(true)

	err := f.uc.Authorize(context.Background(), "ws", "U1")
	gt.Error(t, err).Is(errUserStoreDown)
	gt.Value(t, errors.Is(err, model.ErrWorkspaceAccessDenied)).Equal(false)
}

func TestWorkspaceAccess_ReadFailureIsNotCached(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyAllowAll}, "ws")
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1"})
	ctx := context.Background()

	f.users.failing.Store(true)
	gt.Error(t, f.uc.Authorize(ctx, "ws", "U1"))
	f.users.failing.Store(false)
	gt.NoError(t, f.uc.Authorize(ctx, "ws", "U1"))
	gt.Value(t, f.users.calls.Load()).Equal(int32(2))
}

func TestWorkspaceAccess_EvaluationErrorIsNotCached(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyNonBool}, "ws")
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1"})
	ctx := context.Background()

	err := f.uc.Authorize(ctx, "ws", "U1")
	gt.Error(t, err)
	gt.Value(t, errors.Is(err, model.ErrWorkspaceAccessDenied)).Equal(false)
	gt.Error(t, f.uc.Authorize(ctx, "ws", "U1"))
	gt.Value(t, f.users.calls.Load()).Equal(int32(2))
}

func TestWorkspaceAccess_EmptyUserIsDenied(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyAllowAll}, "ws")
	gt.Error(t, f.uc.Authorize(context.Background(), "ws", "")).Is(model.ErrWorkspaceAccessDenied)
}

func TestWorkspaceAccess_FilterKeepsAllowedInOrder(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"b": policyDenyAll, "c": policyAllowAll}, "a", "b", "c", "d")

	got, err := f.uc.FilterAccessible(context.Background(), f.reg.List(), "U1")
	gt.NoError(t, err)
	ids := make([]string, len(got))
	for i, e := range got {
		ids[i] = e.Workspace.ID
	}
	gt.Value(t, ids).Equal([]string{"a", "c", "d"})
}

func TestWorkspaceAccess_FilterPropagatesEvaluationError(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"a": policyAllowAll, "b": policyNonBool}, "a", "b")
	_, err := f.uc.FilterAccessible(context.Background(), f.reg.List(), "U1")
	gt.Error(t, err)
}

func TestWorkspaceAccess_AuthorizeCurrentUser(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyDenyAll}, "ws")

	gt.NoError(t, f.uc.AuthorizeCurrentUser(context.Background(), "ws"))

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U1"})
	gt.Error(t, f.uc.AuthorizeCurrentUser(ctx, "ws")).Is(model.ErrWorkspaceAccessDenied)
}

func TestWorkspaceAccess_FilterForCurrentUser(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"b": policyDenyAll}, "a", "b")

	all, err := f.uc.FilterAccessibleForCurrentUser(context.Background(), f.reg.List())
	gt.NoError(t, err)
	gt.Value(t, len(all)).Equal(2)

	ctx := auth.ContextWithToken(context.Background(), &auth.Token{Sub: "U1"})
	got, err := f.uc.FilterAccessibleForCurrentUser(ctx, f.reg.List())
	gt.NoError(t, err)
	gt.Value(t, len(got)).Equal(1)
	gt.Value(t, got[0].Workspace.ID).Equal("a")
}

func TestNewWorkspaceAccessUseCase_Validation(t *testing.T) {
	users := memory.New().SlackUser()
	pc := compileAuthzPolicy(t, policyAllowAll)
	okCfg := usecase.WorkspaceAccessCacheConfig{TTL: time.Minute, Size: 16}

	_, err := usecase.NewWorkspaceAccessUseCase(authzRegistry("a"), map[string]interfaces.PolicyClient{"unknown": pc}, users, okCfg)
	gt.Error(t, err)

	_, err = usecase.NewWorkspaceAccessUseCase(authzRegistry("a"), map[string]interfaces.PolicyClient{"a": pc}, users, usecase.WorkspaceAccessCacheConfig{Size: 16})
	gt.Error(t, err)

	_, err = usecase.NewWorkspaceAccessUseCase(authzRegistry("a"), map[string]interfaces.PolicyClient{"a": pc}, nil, okCfg)
	gt.Error(t, err)

	uc, err := usecase.NewWorkspaceAccessUseCase(nil, nil, nil, usecase.WorkspaceAccessCacheConfig{})
	gt.NoError(t, err)
	gt.NoError(t, uc.Authorize(context.Background(), "any", "U1"))
}

func TestWorkspaceAccess_ConcurrentDecisions(t *testing.T) {
	f := newAccessFixture(t, time.Minute, map[string]string{"ws": policyByEmail}, "ws")
	saveSlackUser(t, f.users, &model.SlackUser{ID: "U1", Email: "alice@example.com"})

	var mu sync.Mutex
	results := make([]error, 0, 20)
	for range 20 {
		async.Dispatch(context.Background(), func(ctx context.Context) error {
			err := f.uc.Authorize(ctx, "ws", "U1")
			mu.Lock()
			results = append(results, err)
			mu.Unlock()
			return nil
		})
	}
	async.Wait()

	gt.Value(t, len(results)).Equal(20)
	for _, err := range results {
		gt.NoError(t, err)
	}
}
