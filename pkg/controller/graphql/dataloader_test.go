package graphql_test

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/m-mizutani/gt"
	graphqlctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/graphql"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model/config"
	gqlmodel "github.com/m-mizutani/hecatoncheires/pkg/domain/model/graphql"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/semantic/definition"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/types"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
)

// TestSlackUser_NormalizesCompositeSubClaim pins the rescue path for
// legacy reporter IDs persisted as the composite "Uxxx-Txxx" OIDC sub
// form before the auth-side fix. The bare user ID is what the
// SlackUser repository keys on, so the loader must strip the team
// suffix before lookup; otherwise the reporter resolver returns nil
// and the UI shows an empty cell even though identity is known.
func TestSlackUser_NormalizesCompositeSubClaim(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
		{ID: "U01ABC", Name: "alice", RealName: "Alice"},
		{ID: "W04ENT", Name: "bob", RealName: "Bob"},
	})).Required()

	dl := graphqlctrl.NewDataLoaders(repo, nil, nil)

	users, errs := dl.SlackUser.LoadMany(ctx, []string{
		"U01ABC-T02XYZ", // composite, user-first
		"T02XYZ-W04ENT", // composite, team-first, W prefix
	})()
	for _, e := range errs {
		gt.NoError(t, e).Required()
	}
	gt.Array(t, users).Length(2).Required()
	gt.Value(t, users[0]).NotNil().Required()
	gt.Value(t, users[0].ID).Equal("U01ABC")
	gt.Value(t, users[0].RealName).Equal("Alice")
	gt.Value(t, users[1]).NotNil().Required()
	gt.Value(t, users[1].ID).Equal("W04ENT")
	gt.Value(t, users[1].RealName).Equal("Bob")
}

// TestSlackUser_PreservesBareIDs guards that the normalisation path
// does not regress the common case where the caller passes the
// already-bare user ID (assignee IDs picked through the Slack picker,
// modern reporter IDs persisted after the auth fix).
func TestSlackUser_PreservesBareIDs(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
		{ID: "U01ABC", Name: "alice", RealName: "Alice"},
	})).Required()

	dl := graphqlctrl.NewDataLoaders(repo, nil, nil)

	user, err := dl.SlackUser.Load(ctx, "U01ABC")()
	gt.NoError(t, err).Required()
	gt.Value(t, user).NotNil().Required()
	gt.Value(t, user.ID).Equal("U01ABC")
}

// TestSlackUser_MissingIDReturnsNilData guards the contract used by
// caseResolver.Reporter / Assignees: missing repository rows surface
// as Data=nil with no error so the resolver decides whether to
// escalate to a field-level error (Reporter) or filter the entry
// (Assignees). The batch function also emits a non-fatal
// errutil.Handle entry; this test only asserts the Data shape because
// the errutil wiring is a side effect on the global error reporter.
func TestSlackUser_MissingIDReturnsNilData(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	gt.NoError(t, repo.SlackUser().SaveMany(ctx, []*model.SlackUser{
		{ID: "U01ABC", Name: "alice", RealName: "Alice"},
	})).Required()

	dl := graphqlctrl.NewDataLoaders(repo, nil, nil)

	users, errs := dl.SlackUser.LoadMany(ctx, []string{"U01ABC", "UNOPE"})()
	for _, e := range errs {
		gt.NoError(t, e).Required()
	}
	gt.Array(t, users).Length(2).Required()
	gt.Value(t, users[0]).NotNil().Required()
	gt.Value(t, users[0].ID).Equal("U01ABC")
	gt.Value(t, users[1]).Nil()
}

// TestSlackUser_BatchCollapse pins the N+1 regression: when 20
// independent Load calls land in the same request, the underlying
// SlackUser.GetByIDs must run once. This is the whole point of the
// rewrite - the previous implementation made 20 separate batched
// fetches because each resolver call started its own "batch".
func TestSlackUser_BatchCollapse(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()

	users := make([]*model.SlackUser, 20)
	ids := make([]string, 20)
	for i := range users {
		id := model.SlackUserID(rune('A' + i%26))
		// Make every ID distinct so we exercise the dedup path on
		// real keys, not on duplicates.
		idStr := "U0000" + string([]byte{byte('A' + i%26), byte('0' + i/10), byte('0' + i%10)})
		users[i] = &model.SlackUser{ID: model.SlackUserID(idStr), Name: string(id), RealName: string(id)}
		ids[i] = idStr
	}
	gt.NoError(t, repo.SlackUser().SaveMany(ctx, users)).Required()

	counter := &slackUserCallCounter{inner: repo.SlackUser()}
	counted := &countingRepo{Repository: repo, slackUser: counter}

	dl := graphqlctrl.NewDataLoaders(counted, nil, nil)

	// Enqueue 20 single-ID Load calls concurrently the same way gqlgen
	// would when 20 case rows each resolve their reporter in parallel.
	results := make([]string, len(ids))
	errsCh := make(chan error, len(ids))
	doneCh := make(chan struct{}, len(ids))
	for i, id := range ids {
		i, id := i, id
		go func() {
			user, err := dl.SlackUser.Load(ctx, id)()
			if err != nil {
				errsCh <- err
				doneCh <- struct{}{}
				return
			}
			if user != nil {
				results[i] = user.ID
			}
			doneCh <- struct{}{}
		}()
	}
	for range ids {
		<-doneCh
	}
	select {
	case e := <-errsCh:
		gt.NoError(t, e).Required()
	default:
	}

	gt.Number(t, counter.calls.Load()).Equal(int32(1))
}

// slackUserCallCounter wraps an interfaces.SlackUserRepository and
// counts how many times GetByIDs is invoked. Anchors the N+1
// regression assertion in TestSlackUser_BatchCollapse.
type slackUserCallCounter struct {
	inner interfaces.SlackUserRepository
	calls atomic.Int32
}

func (c *slackUserCallCounter) GetAll(ctx context.Context) ([]*model.SlackUser, error) {
	return c.inner.GetAll(ctx)
}

func (c *slackUserCallCounter) FindByEmail(ctx context.Context, email string) (*model.SlackUser, error) {
	return c.inner.FindByEmail(ctx, email)
}

func (c *slackUserCallCounter) GetByID(ctx context.Context, id model.SlackUserID) (*model.SlackUser, error) {
	return c.inner.GetByID(ctx, id)
}
func (c *slackUserCallCounter) GetByIDs(ctx context.Context, ids []model.SlackUserID) (map[model.SlackUserID]*model.SlackUser, error) {
	c.calls.Add(1)
	return c.inner.GetByIDs(ctx, ids)
}
func (c *slackUserCallCounter) SaveMany(ctx context.Context, users []*model.SlackUser) error {
	return c.inner.SaveMany(ctx, users)
}
func (c *slackUserCallCounter) DeleteAll(ctx context.Context) error {
	return c.inner.DeleteAll(ctx)
}
func (c *slackUserCallCounter) GetMetadata(ctx context.Context) (*model.SlackUserMetadata, error) {
	return c.inner.GetMetadata(ctx)
}
func (c *slackUserCallCounter) SaveMetadata(ctx context.Context, m *model.SlackUserMetadata) error {
	return c.inner.SaveMetadata(ctx, m)
}

// countingRepo wraps a memory.Repository so we can substitute the
// SlackUser repository with the call-counting fake while keeping every
// other repository pointing at the real memory implementation. This
// lets NewDataLoaders construct its batch functions against the same
// repository fabric the production code uses.
type countingRepo struct {
	interfaces.Repository
	slackUser interfaces.SlackUserRepository
}

func (c *countingRepo) SlackUser() interfaces.SlackUserRepository { return c.slackUser }

type fakeTextDisplayResolver struct {
	calls   map[types.Semantic][][]string
	results map[types.Semantic]map[string]definition.Display
	errs    map[types.Semantic]error
}

func (f *fakeTextDisplayResolver) ResolveTextDisplays(_ context.Context, id types.Semantic, values []string) (map[string]definition.Display, error) {
	if f.calls == nil {
		f.calls = map[types.Semantic][][]string{}
	}
	f.calls[id] = append(f.calls[id], values)
	if err := f.errs[id]; err != nil {
		return nil, err
	}
	return f.results[id], nil
}

func TestTextDisplayLoader(t *testing.T) {
	ctx := context.Background()
	const other types.Semantic = "other_semantic"

	t.Run("one call per semantic; a failure only fails its own keys", func(t *testing.T) {
		cause := errors.New("lookup failed")
		fake := &fakeTextDisplayResolver{
			results: map[types.Semantic]map[string]definition.Display{
				types.SemanticSlackChannelID: {"C1": {Label: "#one", URL: "https://slack.com/archives/C1"}},
			},
			errs: map[types.Semantic]error{other: cause},
		}
		dl := graphqlctrl.NewDataLoaders(memory.New(), nil, fake)

		got, errs := graphqlctrl.LoadTextDisplaysForTest(ctx, dl, []graphqlctrl.TextDisplayKeyForTest{
			{Semantic: types.SemanticSlackChannelID, Value: "C1"},
			{Semantic: other, Value: "x"},
			{Semantic: types.SemanticSlackChannelID, Value: "C2"},
		})
		gt.Array(t, got).Length(3).Required()
		gt.Value(t, got[0]).NotNil().Required()
		gt.Value(t, *got[0]).Equal(definition.Display{Label: "#one", URL: "https://slack.com/archives/C1"})
		gt.Value(t, got[2]).Nil()
		gt.Array(t, errs).Length(3).Required()
		gt.NoError(t, errs[0])
		gt.Error(t, errs[1]).Is(cause)
		gt.NoError(t, errs[2])

		// The loader does not keep key order within a batch.
		slackCalls := fake.calls[types.SemanticSlackChannelID]
		gt.Array(t, slackCalls).Length(1).Required()
		sort.Strings(slackCalls[0])
		gt.Equal(t, slackCalls[0], []string{"C1", "C2"})
		gt.Equal(t, fake.calls[other], [][]string{{"x"}})
	})

	t.Run("without a resolver every key is nil", func(t *testing.T) {
		dl := graphqlctrl.NewDataLoaders(memory.New(), nil, nil)
		got, errs := graphqlctrl.LoadTextDisplaysForTest(ctx, dl, []graphqlctrl.TextDisplayKeyForTest{
			{Semantic: types.SemanticSlackChannelID, Value: "C1"},
		})
		gt.Array(t, got).Length(1).Required()
		gt.Value(t, got[0]).Nil()
		for _, err := range errs {
			gt.NoError(t, err)
		}
	})
}

func TestWithSemantics(t *testing.T) {
	fields := []*gqlmodel.FieldValue{
		{FieldID: "channel", Value: "C1"},
		{FieldID: "note", Value: "x"},
		{FieldID: "removed", Value: "y"},
	}
	defs := []config.FieldDefinition{
		{ID: "channel", Type: types.FieldTypeText, Semantic: types.SemanticSlackChannelID},
		{ID: "note", Type: types.FieldTypeText},
	}
	got := graphqlctrl.WithSemanticsForTest(fields, defs)
	gt.Array(t, got).Length(3).Required()
	gt.Value(t, got[0].Semantic).Equal(types.SemanticSlackChannelID)
	gt.Value(t, got[1].Semantic).Equal(types.Semantic(""))
	gt.Value(t, got[2].Semantic).Equal(types.Semantic(""))
	// The input is not mutated: it may be shared with other resolvers.
	gt.Value(t, fields[0].Semantic).Equal(types.Semantic(""))
}

func TestToGraphQLFieldValueDisplay(t *testing.T) {
	gt.Value(t, graphqlctrl.ToGraphQLFieldValueDisplayForTest(nil)).Nil()

	// An unresolved value keeps a display object so the client can show an
	// error, distinct from a field with no semantic (nil).
	unresolved := graphqlctrl.ToGraphQLFieldValueDisplayForTest(&definition.Display{})
	gt.Value(t, unresolved).NotNil().Required()
	gt.Value(t, unresolved.Label).Nil()
	gt.Value(t, unresolved.URL).Nil()

	both := graphqlctrl.ToGraphQLFieldValueDisplayForTest(&definition.Display{Label: "#one", URL: "u"})
	gt.Value(t, both).NotNil().Required()
	gt.Value(t, *both.Label).Equal("#one")
	gt.Value(t, *both.URL).Equal("u")
}
