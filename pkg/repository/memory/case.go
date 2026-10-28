package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/interfaces"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
)

type caseRepository struct {
	mu     sync.RWMutex
	cases  map[string]map[int64]*model.Case
	nextID map[string]int64
}

func newCaseRepository() *caseRepository {
	return &caseRepository{
		cases:  make(map[string]map[int64]*model.Case),
		nextID: make(map[string]int64),
	}
}

func (r *caseRepository) ensureWorkspace(workspaceID string) {
	if _, exists := r.cases[workspaceID]; !exists {
		r.cases[workspaceID] = make(map[int64]*model.Case)
	}
	if _, exists := r.nextID[workspaceID]; !exists {
		r.nextID[workspaceID] = 1
	}
}

// copyFieldValue creates a deep copy of a field value
func copyFieldValue(fv model.FieldValue) model.FieldValue {
	copied := model.FieldValue{
		FieldID: fv.FieldID,
		Type:    fv.Type,
	}
	switch v := fv.Value.(type) {
	case []string:
		s := make([]string, len(v))
		copy(s, v)
		copied.Value = s
	case []interface{}:
		s := make([]interface{}, len(v))
		copy(s, v)
		copied.Value = s
	default:
		copied.Value = fv.Value
	}
	return copied
}

// copyCase creates a deep copy of a case.
//
// It starts from a whole-struct copy rather than a field-by-field literal: a
// literal silently drops any field later added to model.Case, which is exactly
// how ReporterID was lost on the Firestore Create path (see
// .claude/rules/architecture.md § Repository write contract). Only the fields
// that carry a reference — the slices, the map, and the ArchivedAt pointer —
// need work after that, so a new scalar field is copied automatically.
func copyCase(c *model.Case) *model.Case {
	copied := *c

	copied.AssigneeIDs = make([]string, len(c.AssigneeIDs))
	copy(copied.AssigneeIDs, c.AssigneeIDs)

	copied.ChannelUserIDs = make([]string, len(c.ChannelUserIDs))
	copy(copied.ChannelUserIDs, c.ChannelUserIDs)

	if c.FieldValues != nil {
		copied.FieldValues = make(map[string]model.FieldValue, len(c.FieldValues))
		for k, v := range c.FieldValues {
			copied.FieldValues[k] = copyFieldValue(v)
		}
	}

	if c.AgentSourceIDs != nil {
		copied.AgentSourceIDs = make([]model.SourceID, len(c.AgentSourceIDs))
		copy(copied.AgentSourceIDs, c.AgentSourceIDs)
	}

	if c.ArchivedAt != nil {
		archivedAt := *c.ArchivedAt
		copied.ArchivedAt = &archivedAt
	}

	// AccessDenied is runtime-only: the usecase sets it on a value it is about
	// to return, and it is never part of stored state. Clearing it keeps the
	// stored copy free of a caller's transient flag.
	copied.AccessDenied = false

	return &copied
}

func (r *caseRepository) Create(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error) {
	// Validate at the persistence boundary so a usecase / handler bug
	// that forgets to inject the reporter (e.g. Slack interactivity
	// callback without auth.ContextWithToken) fails loudly the first
	// time it runs — instead of silently writing a case the UI cannot
	// attribute to anyone.
	if err := c.ValidateNew(); err != nil {
		return nil, goerr.Wrap(err, "case validation failed before create")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.ensureWorkspace(workspaceID)

	created := copyCase(c)
	created.ID = r.nextID[workspaceID]
	r.nextID[workspaceID]++

	r.cases[workspaceID][created.ID] = created
	return copyCase(created), nil
}

func (r *caseRepository) Get(ctx context.Context, workspaceID string, id int64) (*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
	}

	c, exists := ws[id]
	if !exists {
		return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
	}

	return copyCase(c), nil
}

func (r *caseRepository) GetByIDs(ctx context.Context, workspaceID string, ids []int64) (map[int64]*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[int64]*model.Case, len(ids))
	ws, exists := r.cases[workspaceID]
	if !exists {
		return result, nil
	}

	for _, id := range ids {
		if c, ok := ws[id]; ok {
			result[id] = copyCase(c)
		}
	}

	return result, nil
}

func (r *caseRepository) List(ctx context.Context, workspaceID string, opts ...interfaces.ListCaseOption) ([]*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return []*model.Case{}, nil
	}

	cfg := interfaces.BuildListCaseConfig(opts...)

	cases := make([]*model.Case, 0, len(ws))
	for _, c := range ws {
		// Apply status filter. When no filter is set, exclude drafts so the
		// default listing never leaks unsubmitted entries; callers that want
		// drafts must go through ListDrafts (author-scoped) or pass
		// WithStatus(CaseStatusDraft) explicitly.
		if statusFilter := cfg.Status(); statusFilter != nil {
			if c.Status.Normalize() != *statusFilter {
				continue
			}
		} else if c.IsDraft() {
			continue
		}
		// Archive scope defaults to active-only, so a caller that names no
		// scope never sees archived cases.
		if !cfg.ArchiveScope().Allows(c.IsArchived()) {
			continue
		}
		cases = append(cases, copyCase(c))
	}

	return cases, nil
}

func (r *caseRepository) ListDrafts(ctx context.Context, workspaceID string) ([]*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return []*model.Case{}, nil
	}

	drafts := make([]*model.Case, 0)
	for _, c := range ws {
		if !c.IsDraft() {
			continue
		}
		drafts = append(drafts, copyCase(c))
	}

	return drafts, nil
}

func (r *caseRepository) Update(ctx context.Context, workspaceID string, c *model.Case) (*model.Case, error) {
	if err := c.Validate(); err != nil {
		return nil, goerr.Wrap(err, "case validation failed before update")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", c.ID))
	}

	if _, exists := ws[c.ID]; !exists {
		return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", c.ID))
	}

	updated := copyCase(c)
	r.cases[workspaceID][updated.ID] = updated
	return copyCase(updated), nil
}

// Transact holds the write lock for the whole read-modify-write so fn observes
// exactly the state the write is applied to, mirroring the Firestore
// transaction. fn must not call back into the repository — that would deadlock
// on this lock. Unlike Firestore this never retries fn, but the contract still
// requires fn to be retry-safe so both backends behave identically.
func (r *caseRepository) Transact(ctx context.Context, workspaceID string, id int64, fn func(*model.Case) error) (*model.Case, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
	}
	stored, exists := ws[id]
	if !exists {
		return nil, goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
	}

	// fn mutates a copy: if it fails, the stored case must be untouched.
	updated := copyCase(stored)
	if err := fn(updated); err != nil {
		return nil, err
	}
	if err := updated.Validate(); err != nil {
		return nil, goerr.Wrap(err, "case validation failed before transactional write", goerr.V("id", id))
	}
	r.cases[workspaceID][id] = updated

	return copyCase(updated), nil
}

func (r *caseRepository) Delete(ctx context.Context, workspaceID string, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
	}

	if _, exists := ws[id]; !exists {
		return goerr.Wrap(ErrNotFound, "case not found", goerr.V("id", id))
	}

	delete(r.cases[workspaceID], id)
	return nil
}

func (r *caseRepository) GetBySlackChannelID(ctx context.Context, workspaceID string, channelID string) (*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return nil, nil
	}

	for _, c := range ws {
		if c.SlackChannelID == channelID {
			return copyCase(c), nil
		}
	}

	return nil, nil
}

func (r *caseRepository) GetBySlackChannelIDs(_ context.Context, workspaceID string, channelIDs []string) (map[string]*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]*model.Case)
	wanted := make(map[string]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		wanted[id] = struct{}{}
	}
	for _, c := range r.cases[workspaceID] {
		if c.SlackChannelID == "" {
			continue
		}
		if _, ok := wanted[c.SlackChannelID]; !ok {
			continue
		}
		if _, dup := result[c.SlackChannelID]; !dup {
			result[c.SlackChannelID] = copyCase(c)
		}
	}
	return result, nil
}

func (r *caseRepository) GetBySlackThread(_ context.Context, workspaceID string, channelID string, threadTS string) (*model.Case, error) {
	if channelID == "" || threadTS == "" {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return nil, nil
	}

	for _, c := range ws {
		if c.SlackChannelID == channelID && c.SlackThreadTS == threadTS {
			return copyCase(c), nil
		}
	}

	return nil, nil
}

func (r *caseRepository) GetByRequestKey(_ context.Context, workspaceID string, key string) (*model.Case, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ws, exists := r.cases[workspaceID]
	if !exists {
		return nil, nil
	}

	for _, c := range ws {
		if c.RequestKey == key {
			return copyCase(c), nil
		}
	}

	return nil, nil
}

// ScanAll hands every stored case to fn in ascending ID order. The order is not
// part of the interface contract, but making it deterministic here keeps the
// consistency check's output identical across backends and across runs.
func (r *caseRepository) ScanAll(_ context.Context, workspaceID string, fn func(*model.Case) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ws := r.cases[workspaceID]
	ids := make([]int64, 0, len(ws))
	for id := range ws {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	for _, id := range ids {
		if err := fn(copyCase(ws[id])); err != nil {
			return err
		}
	}

	return nil
}
