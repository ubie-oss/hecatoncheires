package http_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/m-mizutani/gt"
	httpctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/http"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	slacksvc "github.com/m-mizutani/hecatoncheires/pkg/service/slack"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
	goslack "github.com/slack-go/slack"
)

// mockSlackServiceForCommand is a minimal mock for slash command handler tests
type mockSlackServiceForCommand struct{}

func (m *mockSlackServiceForCommand) ListJoinedChannels(_ context.Context, _ string) ([]slacksvc.Channel, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) ListMemberChannels(_ context.Context, _, _ string) ([]slacksvc.Channel, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) JoinChannel(_ context.Context, _ string) error {
	return nil
}
func (m *mockSlackServiceForCommand) GetChannelNames(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) GetUserInfo(_ context.Context, _ string) (*slacksvc.User, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) ListUsers(_ context.Context, _ string) ([]*slacksvc.User, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) CreateChannel(_ context.Context, _ int64, _ string, _ string, _ bool, _ string) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) GetConversationMembers(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) GetChannelInfo(_ context.Context, channelID string) (*slacksvc.ChannelInfo, error) {
	return &slacksvc.ChannelInfo{ID: channelID}, nil
}
func (m *mockSlackServiceForCommand) RenameChannel(_ context.Context, _ string, _ int64, _ string, _ string) error {
	return nil
}
func (m *mockSlackServiceForCommand) InviteUsersToChannel(_ context.Context, _ string, _ []string) error {
	return nil
}
func (m *mockSlackServiceForCommand) AddBookmark(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *mockSlackServiceForCommand) GetTeamURL(_ context.Context) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) PostMessage(_ context.Context, _ string, _ []goslack.Block, _ string) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) UpdateMessage(_ context.Context, _, _ string, _ []goslack.Block, _ string) error {
	return nil
}
func (m *mockSlackServiceForCommand) PostMessageWithAttachment(_ context.Context, _ string, _ string, _ goslack.Attachment) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) UpdateMessageWithAttachment(_ context.Context, _, _ string, _ string, _ goslack.Attachment) error {
	return nil
}
func (m *mockSlackServiceForCommand) PostMessageWithAttachments(_ context.Context, _ string, _ string, _ []goslack.Attachment) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) UpdateMessageWithAttachments(_ context.Context, _ string, _ string, _ string, _ []goslack.Attachment) error {
	return nil
}
func (m *mockSlackServiceForCommand) GetConversationReplies(_ context.Context, _, _ string, _ int) ([]slacksvc.ConversationMessage, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) GetConversationHistory(_ context.Context, _ string, _ time.Time, _ int) ([]slacksvc.ConversationMessage, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) PostThreadReply(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) PostThreadMessage(_ context.Context, _, _ string, _ []goslack.Block, _ string, _ ...slacksvc.PostThreadOption) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) GetBotUserID(_ context.Context) (string, error) {
	return "", nil
}
func (m *mockSlackServiceForCommand) OpenView(_ context.Context, _ string, _ goslack.ModalViewRequest) error {
	return nil
}
func (m *mockSlackServiceForCommand) UpdateView(_ context.Context, _ goslack.ModalViewRequest, _, _, _ string) error {
	return nil
}
func (m *mockSlackServiceForCommand) ListUserGroups(_ context.Context, _ string) ([]slacksvc.UserGroup, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) ListTeams(_ context.Context) ([]slacksvc.Team, error) {
	return nil, nil
}
func (m *mockSlackServiceForCommand) PostEphemeral(_ context.Context, _ string, _ string, _ string, _ ...slacksvc.PostEphemeralOption) error {
	return nil
}
func (m *mockSlackServiceForCommand) PostEphemeralBlocks(_ context.Context, _ string, _ string, _ []goslack.Block, _ string) (string, error) {
	return "ts-eph", nil
}
func (m *mockSlackServiceForCommand) GetPermalink(_ context.Context, channelID string, ts string) (string, error) {
	return "https://slack.test/" + channelID + "/" + ts, nil
}
func (m *mockSlackServiceForCommand) GetUserGroupMembers(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func TestSlackCommandHandler(t *testing.T) {
	setup := func(t *testing.T, workspaces ...model.Workspace) *httpctrl.SlackCommandHandler {
		t.Helper()
		repo := memory.New()
		registry := model.NewWorkspaceRegistry()
		for _, ws := range workspaces {
			registry.Register(&model.WorkspaceEntry{Workspace: ws})
		}
		slackUC := usecase.NewSlackUseCases(repo, registry, nil, nil, &mockSlackServiceForCommand{})
		return httpctrl.NewSlackCommandHandler(slackUC)
	}

	t.Run("returns 200 for valid command without ws_id", func(t *testing.T) {
		handler := setup(t, model.Workspace{ID: "risk", Name: "Risk"})

		form := url.Values{
			"trigger_id": {"trigger-1"},
			"user_id":    {"U001"},
			"channel_id": {"C001"},
			"command":    {"/create-case"},
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/slack/command", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
	})

	t.Run("returns 200 for valid command with ws_id via chi router", func(t *testing.T) {
		handler := setup(t, model.Workspace{ID: "risk", Name: "Risk"})

		form := url.Values{
			"trigger_id": {"trigger-1"},
			"user_id":    {"U001"},
			"channel_id": {"C001"},
			"command":    {"/create-risk-case"},
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/slack/command/risk", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		// Use chi router to inject URL params
		r := chi.NewRouter()
		r.Post("/hooks/slack/command/{ws_id}", handler.ServeHTTP)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
	})

	t.Run("returns 400 for missing trigger_id", func(t *testing.T) {
		handler := setup(t, model.Workspace{ID: "risk", Name: "Risk"})

		form := url.Values{
			"user_id":    {"U001"},
			"channel_id": {"C001"},
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/slack/command", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		gt.Value(t, rec.Code).Equal(http.StatusBadRequest)
	})

	t.Run("returns 200 with error message for invalid ws_id", func(t *testing.T) {
		handler := setup(t, model.Workspace{ID: "risk", Name: "Risk"})

		form := url.Values{
			"trigger_id": {"trigger-1"},
			"user_id":    {"U001"},
			"channel_id": {"C001"},
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/slack/command/nonexistent", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		r := chi.NewRouter()
		r.Post("/hooks/slack/command/{ws_id}", handler.ServeHTTP)

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		// Slack expects 200 even on errors (error text shown as ephemeral)
		gt.Value(t, rec.Code).Equal(http.StatusOK)
		gt.String(t, rec.Body.String()).Contains("Failed to open case creation dialog")
	})
}
