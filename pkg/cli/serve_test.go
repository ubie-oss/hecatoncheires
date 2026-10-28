package cli_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/hecatoncheires/pkg/cli"
	"github.com/m-mizutani/hecatoncheires/pkg/cli/config"
	gqlctrl "github.com/m-mizutani/hecatoncheires/pkg/controller/graphql"
	"github.com/m-mizutani/hecatoncheires/pkg/domain/model"
	"github.com/m-mizutani/hecatoncheires/pkg/repository/memory"
	"github.com/m-mizutani/hecatoncheires/pkg/usecase"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"missing required → BAD_USER_INPUT", goerr.Wrap(model.ErrMissingRequired, "x"), gqlctrl.ErrCodeBadUserInput},
		{"invalid option ID → BAD_USER_INPUT", goerr.Wrap(model.ErrInvalidOptionID, "x"), gqlctrl.ErrCodeBadUserInput},
		{"invalid field type → BAD_USER_INPUT", goerr.Wrap(model.ErrInvalidFieldType, "x"), gqlctrl.ErrCodeBadUserInput},
		{"case not found → NOT_FOUND", goerr.Wrap(usecase.ErrCaseNotFound, "x"), gqlctrl.ErrCodeNotFound},
		{"action not found → NOT_FOUND", goerr.Wrap(usecase.ErrActionNotFound, "x"), gqlctrl.ErrCodeNotFound},
		{"workspace not found → NOT_FOUND", goerr.Wrap(model.ErrWorkspaceNotFound, "x"), gqlctrl.ErrCodeNotFound},
		{"access denied → FORBIDDEN", goerr.Wrap(usecase.ErrAccessDenied, "x"), gqlctrl.ErrCodeForbidden},
		{"already closed → CONFLICT", goerr.Wrap(usecase.ErrCaseAlreadyClosed, "x"), gqlctrl.ErrCodeConflict},
		{"missing required on submit → MISSING_REQUIRED_FIELDS", goerr.Wrap(usecase.ErrMissingRequiredOnSubmit, "x"), gqlctrl.ErrCodeMissingRequiredFields},
		{"draft title required → TITLE_REQUIRED", goerr.Wrap(usecase.ErrDraftTitleRequired, "x"), gqlctrl.ErrCodeTitleRequired},
		{"case not draft (domain) → INVALID_STATUS_TRANSITION", goerr.Wrap(model.ErrCaseNotDraft, "x"), gqlctrl.ErrCodeInvalidStatusTransition},
		{"case not draft (usecase) → INVALID_STATUS_TRANSITION", goerr.Wrap(usecase.ErrCaseNotDraft, "x"), gqlctrl.ErrCodeInvalidStatusTransition},
		{"field validation failed → FIELD_VALIDATION_FAILED", goerr.Wrap(usecase.ErrFieldValidationFailed, "x"), gqlctrl.ErrCodeFieldValidationFailed},
		{"activation failed → ACTIVATION_FAILED", goerr.Wrap(usecase.ErrActivationFailed, "x"), gqlctrl.ErrCodeActivationFailed},
		{"random error → untagged", goerr.New("boom"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gt.String(t, cli.ClassifyErrorForTest(c.err)).Equal(c.want)
		})
	}
}

func TestStatusForExtensionCode(t *testing.T) {
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeBadUserInput)).Equal(http.StatusBadRequest)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeNotFound)).Equal(http.StatusNotFound)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeForbidden)).Equal(http.StatusForbidden)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeConflict)).Equal(http.StatusConflict)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeUnauthenticated)).Equal(http.StatusUnauthorized)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeMissingRequiredFields)).Equal(http.StatusBadRequest)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeTitleRequired)).Equal(http.StatusBadRequest)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeFieldValidationFailed)).Equal(http.StatusBadRequest)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeInvalidStatusTransition)).Equal(http.StatusConflict)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeActivationFailed)).Equal(0) // server fault → 500 fallback
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeSlackChannelNotLinkable)).Equal(http.StatusBadRequest)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeSlackChannelLinked)).Equal(http.StatusConflict)
	gt.Number(t, cli.StatusForExtensionCodeForTest(gqlctrl.ErrCodeSlackChannelJoinFailed)).Equal(0) // may be a missing scope → 500
	gt.Number(t, cli.StatusForExtensionCodeForTest("")).Equal(0)
	gt.Number(t, cli.StatusForExtensionCodeForTest("WHATEVER")).Equal(0)
}

func TestHTTPStatusForGraphQLErrors(t *testing.T) {
	gt.Number(t, cli.HTTPStatusForGraphQLErrorCodesForTest("BAD_USER_INPUT")).Equal(http.StatusBadRequest)
	gt.Number(t, cli.HTTPStatusForGraphQLErrorCodesForTest("NOT_FOUND")).Equal(http.StatusNotFound)

	// One untagged error in the batch → escalate to 500
	gt.Number(t, cli.HTTPStatusForGraphQLErrorCodesForTest("BAD_USER_INPUT", "")).Equal(http.StatusInternalServerError)

	// Highest 4xx wins when all are tagged
	gt.Number(t, cli.HTTPStatusForGraphQLErrorCodesForTest("BAD_USER_INPUT", "FORBIDDEN")).Equal(http.StatusForbidden)

	// Empty list → 500 fallback
	gt.Number(t, cli.HTTPStatusForGraphQLErrorCodesForTest()).Equal(http.StatusInternalServerError)
}

func TestGraphqlErrorStatusMiddleware_MapsClientErrorsTo4xx(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "BAD_USER_INPUT → 400",
			body:       `{"data":null,"errors":[{"message":"required field is missing","extensions":{"code":"BAD_USER_INPUT"}}]}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "NOT_FOUND → 404",
			body:       `{"data":null,"errors":[{"message":"case not found","extensions":{"code":"NOT_FOUND"}}]}`,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "FORBIDDEN → 403",
			body:       `{"data":null,"errors":[{"message":"access denied","extensions":{"code":"FORBIDDEN"}}]}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "untagged error → 500",
			body:       `{"data":null,"errors":[{"message":"boom"}]}`,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "no errors → 200 passthrough",
			body:       `{"data":{"hello":"world"}}`,
			wantStatus: http.StatusOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := cli.GraphqlErrorStatusMiddlewareForTest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
			h.ServeHTTP(rec, req)
			gt.Number(t, rec.Code).Equal(c.wantStatus)
		})
	}
}

// buildWorkspaceAccess must take the policy of every workspace that declares
// one, and leave a workspace without [authz] open.
func TestBuildWorkspaceAccess(t *testing.T) {
	dir := t.TempDir()
	gt.NoError(t, os.WriteFile(filepath.Join(dir, "deny.rego"), []byte("package authz\n\nallow := false\n"), 0o600)).Required()
	configs, err := config.ParseWorkspaceConfigs([]config.WorkspaceConfigSource{
		{Name: "open.toml", Data: []byte("[workspace]\nid = \"open\"\n"), BaseDir: dir},
		{Name: "secured.toml", Data: []byte("[workspace]\nid = \"secured\"\n\n[authz]\npolicy = [\"deny.rego\"]\n"), BaseDir: dir},
	})
	gt.NoError(t, err).Required()
	registry := config.BuildWorkspaceRegistry(configs)

	access, err := cli.BuildWorkspaceAccessForTest(registry, configs, memory.New().SlackUser())
	gt.NoError(t, err).Required()

	ctx := context.Background()
	gt.NoError(t, access.Authorize(ctx, "open", "U0ALICE"))
	gt.Error(t, access.Authorize(ctx, "secured", "U0ALICE")).Is(model.ErrWorkspaceAccessDenied)
}
