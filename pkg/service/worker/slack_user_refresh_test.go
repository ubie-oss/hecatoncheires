package worker_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/service/worker"
	goslack "github.com/slack-go/slack"
)

// mockSlackService is a mock implementation of slack.Service for testing
type mockSlackService struct {
	mu              sync.RWMutex
	users           []*slack.User
	usersByTeam     map[string][]*slack.User // per-team user sets for org-level tests
	listUsersError  error
	listUsersCalled int
}

func newMockSlackService() *mockSlackService {
	return &mockSlackService{
		users: []*slack.User{},
	}
}

func (m *mockSlackService) setUsers(users []*slack.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users = users
}

func (m *mockSlackService) setListUsersError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listUsersError = err
}

func (m *mockSlackService) setUsersByTeam(teamID string, users []*slack.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.usersByTeam == nil {
		m.usersByTeam = make(map[string][]*slack.User)
	}
	m.usersByTeam[teamID] = users
}

func (m *mockSlackService) ListUsers(ctx context.Context, teamID string) ([]*slack.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.listUsersCalled++

	if m.listUsersError != nil {
		return nil, m.listUsersError
	}

	// If per-team users are configured and teamID is given, use those
	if teamID != "" && m.usersByTeam != nil {
		if users, ok := m.usersByTeam[teamID]; ok {
			result := make([]*slack.User, len(users))
			for i, u := range users {
				userCopy := *u
				result[i] = &userCopy
			}
			return result, nil
		}
		return nil, nil
	}

	// Return a deep copy to prevent race conditions
	result := make([]*slack.User, len(m.users))
	for i, u := range m.users {
		userCopy := *u
		result[i] = &userCopy
	}

	return result, nil
}

func (m *mockSlackService) ListJoinedChannels(ctx context.Context, teamID string) ([]slack.Channel, error) {
	return nil, nil
}

func (m *mockSlackService) ListMemberChannels(ctx context.Context, userID, teamID string) ([]slack.Channel, error) {
	return nil, nil
}

func (m *mockSlackService) JoinChannel(ctx context.Context, channelID string) error {
	return nil
}

func (m *mockSlackService) GetChannelNames(ctx context.Context, ids []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockSlackService) GetUserInfo(ctx context.Context, userID string) (*slack.User, error) {
	return nil, nil
}

func (m *mockSlackService) CreateChannel(_ context.Context, _ int64, _, _ string, _ bool, _ string) (string, error) {
	return "", nil
}

func (m *mockSlackService) GetConversationMembers(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockSlackService) GetChannelInfo(_ context.Context, channelID string) (*slack.ChannelInfo, error) {
	return &slack.ChannelInfo{ID: channelID}, nil
}

func (m *mockSlackService) RenameChannel(ctx context.Context, channelID string, caseID int64, caseName string, prefix string) error {
	return nil
}

func (m *mockSlackService) InviteUsersToChannel(ctx context.Context, channelID string, userIDs []string) error {
	return nil
}

func (m *mockSlackService) AddBookmark(ctx context.Context, channelID, title, link string) error {
	return nil
}

func (m *mockSlackService) GetTeamURL(ctx context.Context) (string, error) {
	return "https://test-team.slack.com", nil
}

func (m *mockSlackService) PostMessage(ctx context.Context, channelID string, blocks []goslack.Block, text string) (string, error) {
	return "1234567890.123456", nil
}

func (m *mockSlackService) UpdateMessage(ctx context.Context, channelID string, timestamp string, blocks []goslack.Block, text string) error {
	return nil
}

func (m *mockSlackService) PostMessageWithAttachment(_ context.Context, _ string, _ string, _ goslack.Attachment) (string, error) {
	return "1234567890.123456", nil
}

func (m *mockSlackService) PostMessageWithAttachments(_ context.Context, _ string, _ string, _ []goslack.Attachment) (string, error) {
	return "1234567890.123456", nil
}

func (m *mockSlackService) UpdateMessageWithAttachments(_ context.Context, _ string, _ string, _ string, _ []goslack.Attachment) error {
	return nil
}

func (m *mockSlackService) UpdateMessageWithAttachment(_ context.Context, _ string, _ string, _ string, _ goslack.Attachment) error {
	return nil
}

func (m *mockSlackService) GetConversationReplies(ctx context.Context, channelID string, threadTS string, limit int) ([]slack.ConversationMessage, error) {
	return nil, nil
}

func (m *mockSlackService) GetConversationHistory(ctx context.Context, channelID string, oldest time.Time, limit int) ([]slack.ConversationMessage, error) {
	return nil, nil
}

func (m *mockSlackService) PostThreadReply(ctx context.Context, channelID string, threadTS string, text string) (string, error) {
	return "", nil
}

func (m *mockSlackService) PostThreadMessage(ctx context.Context, channelID string, threadTS string, blocks []goslack.Block, text string, opts ...slack.PostThreadOption) (string, error) {
	return "", nil
}

func (m *mockSlackService) GetBotUserID(ctx context.Context) (string, error) {
	return "", nil
}

func (m *mockSlackService) OpenView(ctx context.Context, triggerID string, view goslack.ModalViewRequest) error {
	return nil
}

func (m *mockSlackService) UpdateView(_ context.Context, _ goslack.ModalViewRequest, _, _, _ string) error {
	return nil
}

func (m *mockSlackService) ListUserGroups(ctx context.Context, teamID string) ([]slack.UserGroup, error) {
	return nil, nil
}

func (m *mockSlackService) GetUserGroupMembers(ctx context.Context, groupID string) ([]string, error) {
	return nil, nil
}

func (m *mockSlackService) ListTeams(ctx context.Context) ([]slack.Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var teams []slack.Team
	for teamID := range m.usersByTeam {
		teams = append(teams, slack.Team{ID: teamID, Name: "team-" + teamID})
	}
	return teams, nil
}

func (m *mockSlackService) PostEphemeral(_ context.Context, _ string, _ string, _ string, _ ...slack.PostEphemeralOption) error {
	return nil
}

func (m *mockSlackService) PostEphemeralBlocks(_ context.Context, _ string, _ string, _ []goslack.Block, _ string) (string, error) {
	return "ts-eph", nil
}

func (m *mockSlackService) GetPermalink(_ context.Context, channelID string, ts string) (string, error) {
	return "https://slack.test/" + channelID + "/" + ts, nil
}

func TestSlackUserRefreshWorker_ImmediateInitialSync(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	// Set up mock users
	now := time.Now()
	mockUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "https://example.com/alice.jpg",
		},
		{
			ID:       fmt.Sprintf("U%d_2", now.UnixNano()),
			Name:     "bob",
			RealName: "Bob Johnson",
			Email:    "bob@example.com",
			ImageURL: "https://example.com/bob.jpg",
		},
	}
	mockSvc.setUsers(mockUsers)

	// Create worker with short interval (not used in this test)
	worker := worker.NewSlackUserRefreshWorker(repo, mockSvc, 10*time.Minute, false)

	// Start worker (initial sync runs in background goroutine)
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer worker.Stop()

	// Wait for background initial sync to complete
	time.Sleep(50 * time.Millisecond)

	// Verify users are in database
	users, err := repo.SlackUser().GetAll(ctx)
	if err != nil {
		t.Fatalf("failed to get all users: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("expected 2 users in database, got %d", len(users))
	}

	// Verify metadata
	metadata, err := repo.SlackUser().GetMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to get metadata: %v", err)
	}

	if metadata.UserCount != 2 {
		t.Errorf("expected UserCount=2, got %d", metadata.UserCount)
	}

	if metadata.LastRefreshSuccess.IsZero() {
		t.Error("expected LastRefreshSuccess to be set")
	}

	if metadata.LastRefreshAttempt.IsZero() {
		t.Error("expected LastRefreshAttempt to be set")
	}
}

func TestSlackUserRefreshWorker_PeriodicRefresh(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	// Set up initial mock users
	now := time.Now()
	initialUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "",
		},
	}
	mockSvc.setUsers(initialUsers)

	// Create worker with very short interval for testing (100ms)
	worker := worker.NewSlackUserRefreshWorker(repo, mockSvc, 100*time.Millisecond, false)

	// Start worker
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer worker.Stop()

	// Wait for initial sync
	time.Sleep(50 * time.Millisecond)

	// Verify initial users
	users, err := repo.SlackUser().GetAll(ctx)
	if err != nil {
		t.Fatalf("failed to get all users: %v", err)
	}

	if len(users) != 1 {
		t.Fatalf("expected 1 user after initial sync, got %d", len(users))
	}

	// Update mock users
	updatedUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "",
		},
		{
			ID:       fmt.Sprintf("U%d_2", now.UnixNano()),
			Name:     "bob",
			RealName: "Bob Johnson",
			Email:    "bob@example.com",
			ImageURL: "",
		},
	}
	mockSvc.setUsers(updatedUsers)

	// Wait for periodic refresh (at least one interval + buffer)
	time.Sleep(200 * time.Millisecond)

	// Verify updated users
	users, err = repo.SlackUser().GetAll(ctx)
	if err != nil {
		t.Fatalf("failed to get all users after refresh: %v", err)
	}

	if len(users) != 2 {
		t.Errorf("expected 2 users after periodic refresh, got %d", len(users))
	}

	// Verify metadata
	metadata, err := repo.SlackUser().GetMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to get metadata: %v", err)
	}

	if metadata.UserCount != 2 {
		t.Errorf("expected UserCount=2, got %d", metadata.UserCount)
	}
}

func TestSlackUserRefreshWorker_HandlesSlackAPIErrors(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	// Set up initial successful users
	now := time.Now()
	initialUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "",
		},
	}
	mockSvc.setUsers(initialUsers)

	// Create worker
	worker := worker.NewSlackUserRefreshWorker(repo, mockSvc, 100*time.Millisecond, false)

	// Start worker
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer worker.Stop()

	// Wait for initial sync
	time.Sleep(50 * time.Millisecond)

	// Verify initial users
	users, err := repo.SlackUser().GetAll(ctx)
	if err != nil {
		t.Fatalf("failed to get all users: %v", err)
	}

	if len(users) != 1 {
		t.Fatalf("expected 1 user after initial sync, got %d", len(users))
	}

	// Set Slack API to return error
	mockSvc.setListUsersError(fmt.Errorf("slack API error"))

	// Wait for periodic refresh attempt
	time.Sleep(200 * time.Millisecond)

	// Verify old data is preserved (Graceful Degradation)
	users, err = repo.SlackUser().GetAll(ctx)
	if err != nil {
		t.Fatalf("failed to get all users after API error: %v", err)
	}

	if len(users) != 1 {
		t.Errorf("expected old data (1 user) to be preserved after API error, got %d", len(users))
	}

	// Verify metadata shows attempt but not success
	metadata, err := repo.SlackUser().GetMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to get metadata: %v", err)
	}

	// LastRefreshAttempt should be more recent than LastRefreshSuccess
	if !metadata.LastRefreshAttempt.After(metadata.LastRefreshSuccess) {
		t.Errorf("expected LastRefreshAttempt > LastRefreshSuccess after error, got attempt=%v success=%v",
			metadata.LastRefreshAttempt, metadata.LastRefreshSuccess)
	}
}

func TestSlackUserRefreshWorker_StopsCleanly(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	// Set up mock users
	now := time.Now()
	mockUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "",
		},
	}
	mockSvc.setUsers(mockUsers)

	// Create worker with short interval
	worker := worker.NewSlackUserRefreshWorker(repo, mockSvc, 100*time.Millisecond, false)

	// Start worker
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}

	// Wait briefly
	time.Sleep(50 * time.Millisecond)

	// Stop should return immediately (not block)
	stopStart := time.Now()
	worker.Stop()
	stopDuration := time.Since(stopStart)

	// Stop should complete within a reasonable time (< 1 second)
	if stopDuration > time.Second {
		t.Errorf("Stop() took too long: %v", stopDuration)
	}
}

func TestSlackUserRefreshWorker_SavesMetadataOnSuccess(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	// Set up mock users
	now := time.Now()
	mockUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "",
		},
		{
			ID:       fmt.Sprintf("U%d_2", now.UnixNano()),
			Name:     "bob",
			RealName: "Bob Johnson",
			Email:    "bob@example.com",
			ImageURL: "",
		},
		{
			ID:       fmt.Sprintf("U%d_3", now.UnixNano()),
			Name:     "charlie",
			RealName: "Charlie Brown",
			Email:    "charlie@example.com",
			ImageURL: "",
		},
	}
	mockSvc.setUsers(mockUsers)

	// Create worker
	worker := worker.NewSlackUserRefreshWorker(repo, mockSvc, 10*time.Minute, false)

	// Start worker
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer worker.Stop()

	// Wait for initial sync
	time.Sleep(50 * time.Millisecond)

	// Get metadata
	metadata, err := repo.SlackUser().GetMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to get metadata: %v", err)
	}

	// Verify LastRefreshSuccess is set
	if metadata.LastRefreshSuccess.IsZero() {
		t.Error("expected LastRefreshSuccess to be set after successful refresh")
	}

	// Verify LastRefreshAttempt is set
	if metadata.LastRefreshAttempt.IsZero() {
		t.Error("expected LastRefreshAttempt to be set after successful refresh")
	}

	// Verify UserCount matches
	if metadata.UserCount != 3 {
		t.Errorf("expected UserCount=3, got %d", metadata.UserCount)
	}

	// Verify LastRefreshSuccess and LastRefreshAttempt are close (success case)
	diff := metadata.LastRefreshAttempt.Sub(metadata.LastRefreshSuccess).Abs()
	if diff > time.Second {
		t.Errorf("expected LastRefreshSuccess and LastRefreshAttempt to be close, diff=%v", diff)
	}
}

func TestSlackUserRefreshWorker_SavesAttemptMetadataOnFailure(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	// Set up initial successful users
	now := time.Now()
	initialUsers := []*slack.User{
		{
			ID:       fmt.Sprintf("U%d_1", now.UnixNano()),
			Name:     "alice",
			RealName: "Alice Smith",
			Email:    "alice@example.com",
			ImageURL: "",
		},
	}
	mockSvc.setUsers(initialUsers)

	// Create worker
	worker := worker.NewSlackUserRefreshWorker(repo, mockSvc, 100*time.Millisecond, false)

	// Start worker
	if err := worker.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer worker.Stop()

	// Wait for initial successful sync
	time.Sleep(50 * time.Millisecond)

	// Get initial metadata
	initialMetadata, err := repo.SlackUser().GetMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to get initial metadata: %v", err)
	}

	initialSuccess := initialMetadata.LastRefreshSuccess

	// Set Slack API to return error
	mockSvc.setListUsersError(fmt.Errorf("slack API error"))

	// Wait for failed refresh attempt
	time.Sleep(200 * time.Millisecond)

	// Get updated metadata
	updatedMetadata, err := repo.SlackUser().GetMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to get updated metadata: %v", err)
	}

	// LastRefreshSuccess should remain unchanged (no new success)
	if !updatedMetadata.LastRefreshSuccess.Equal(initialSuccess) {
		t.Errorf("expected LastRefreshSuccess to remain unchanged after failure, initial=%v updated=%v",
			initialSuccess, updatedMetadata.LastRefreshSuccess)
	}

	// LastRefreshAttempt should be more recent (new attempt)
	if !updatedMetadata.LastRefreshAttempt.After(initialSuccess) {
		t.Errorf("expected LastRefreshAttempt to be updated after failure, attempt=%v success=%v",
			updatedMetadata.LastRefreshAttempt, initialSuccess)
	}
}

func TestSlackUserRefreshWorker_OrgLevelMultiTeam(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	now := time.Now()

	// Team T111 has alice and charlie
	mockSvc.setUsersByTeam("T111", []*slack.User{
		{ID: fmt.Sprintf("U%d_alice", now.UnixNano()), Name: "alice", RealName: "Alice", Email: "alice@example.com"},
		{ID: fmt.Sprintf("U%d_charlie", now.UnixNano()), Name: "charlie", RealName: "Charlie", Email: "charlie@example.com"},
	})
	// Team T222 has bob and charlie (charlie is in both teams)
	charlieID := fmt.Sprintf("U%d_charlie", now.UnixNano()) // same ID as above
	mockSvc.setUsersByTeam("T222", []*slack.User{
		{ID: fmt.Sprintf("U%d_bob", now.UnixNano()), Name: "bob", RealName: "Bob", Email: "bob@example.com"},
		{ID: charlieID, Name: "charlie", RealName: "Charlie", Email: "charlie@example.com"},
	})

	// Create worker with org-level team IDs
	w := worker.NewSlackUserRefreshWorker(repo, mockSvc, 10*time.Minute, true)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer w.Stop()

	time.Sleep(50 * time.Millisecond)

	users, err := repo.SlackUser().GetAll(ctx)
	gt.NoError(t, err).Required()

	// Should have 3 unique users (charlie deduplicated)
	gt.Number(t, len(users)).Equal(3)
}

func TestSlackUserRefreshWorker_WSLevelNoTeamIDs(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	mockSvc := newMockSlackService()

	now := time.Now()
	mockSvc.setUsers([]*slack.User{
		{ID: fmt.Sprintf("U%d_1", now.UnixNano()), Name: "alice", RealName: "Alice", Email: "alice@example.com"},
		{ID: fmt.Sprintf("U%d_2", now.UnixNano()), Name: "bob", RealName: "Bob", Email: "bob@example.com"},
	})

	// Create worker with no team IDs (WS-level app)
	w := worker.NewSlackUserRefreshWorker(repo, mockSvc, 10*time.Minute, false)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}
	defer w.Stop()

	time.Sleep(50 * time.Millisecond)

	users, err := repo.SlackUser().GetAll(ctx)
	gt.NoError(t, err).Required()

	// Should have 2 users from default (non-team-scoped) call
	gt.Number(t, len(users)).Equal(2)
}
