//go:build darwin || linux || dragonfly || freebsd || netbsd || openbsd

package harness_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/m-mizutani/hecatoncheires/pkg/agent/harness"
)

func TestEndpointRequiresExactHostAndCapability(t *testing.T) {
	const host = "127.0.0.1:12345"
	tokenPath := "/" + strings.Repeat("a", 48)
	endpoint := harness.AuthenticatedSessionHandlerForTest(host, tokenPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		method, host, path string
		want               int
	}{
		{http.MethodPost, host, tokenPath, http.StatusNoContent},
		{http.MethodPost, host, "/" + strings.Repeat("b", 48), http.StatusNotFound},
		{http.MethodPost, "example.invalid:12345", tokenPath, http.StatusNotFound},
		{http.MethodPost, host, tokenPath + "?", http.StatusNotFound},
		{http.MethodGet, host, tokenPath, http.StatusMethodNotAllowed},
	} {
		res := httptest.NewRecorder()
		endpoint.ServeHTTP(res, httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil))
		if res.Code != tc.want {
			t.Errorf("endpoint returned %d, want %d", res.Code, tc.want)
		}
	}
}

func TestEndpointRejectsSharedDirectoriesAndSymlinks(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.NewSession(shared); err == nil {
		t.Fatal("session accepted a shared directory")
	}
	linkedDir := filepath.Join(dir, "link")
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, linkedDir); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.NewSession(linkedDir); err == nil {
		t.Fatal("session accepted a symlink directory")
	}
	public := filepath.Join(private, "public")
	if err := os.WriteFile(public, []byte(`{"done":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(private, "endpoint.json")
	if err := os.Symlink(public, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.DialSession(ctx, private); err == nil || errors.Is(err, harness.ErrSessionDone) {
		t.Fatalf("symlink endpoint trusted: %v", err)
	}
	if err := os.Rename(public, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.DialSession(ctx, private); err == nil || errors.Is(err, harness.ErrSessionDone) {
		t.Fatalf("public endpoint trusted: %v", err)
	}
}

func TestSessionFinishedReadsOnlyExplicitCompletion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if harness.SessionFinished(t.Context(), dir) {
		t.Fatal("a missing endpoint was reported as complete")
	}
	path := filepath.Join(dir, "endpoint.json")
	if err := os.WriteFile(path, []byte(`{"url":"http://127.0.0.1:1/unreachable"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if harness.SessionFinished(t.Context(), dir) {
		t.Fatal("an unreachable endpoint was reported as complete")
	}
	if err := os.WriteFile(path, []byte(`{"done":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !harness.SessionFinished(t.Context(), dir) {
		t.Fatal("an explicit completion record was ignored")
	}
}

func TestLoopbackMCPAndSessionDirectoryLease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	s, err := harness.NewSession(dir)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skip("loopback listeners unavailable in this sandbox")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if other, err := harness.NewSession(dir); err == nil {
		_ = other.Close()
		t.Fatal("two sessions acquired the same directory")
	}
	info, err := os.Stat(filepath.Join(dir, "endpoint.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("endpoint is not an owner-only file")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := harness.DialSession(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	work := run(s, ctx, harness.Request{Prompt: "synthetic task"})
	raw, failed, err := c.Call(ctx, harness.SessionToolNext, map[string]any{"wait_seconds": 2})
	if failed || err != nil {
		t.Fatalf("loopback MCP next_request failed: %v", err)
	}
	request := decode[dispatch](t, raw)
	if request.ID == "" {
		t.Fatal("loopback client could not take the request")
	}
	if _, failed, err := c.Call(ctx, harness.SessionToolSubmit, map[string]any{"id": request.ID, "answer": "synthetic answer"}); failed || err != nil {
		t.Fatalf("loopback worker could not submit: %v", err)
	}
	if outcome := receive(t, ctx, work); outcome.err != nil || outcome.result.Text != "synthetic answer" {
		t.Fatalf("loopback worker's answer was not returned: %v", outcome.err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.DialSession(ctx, dir); !errors.Is(err, harness.ErrSessionDone) {
		t.Fatalf("closed session does not appear done: %v", err)
	}
	reopened, err := harness.NewSession(dir)
	if err != nil {
		t.Fatalf("session did not release the directory: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}
