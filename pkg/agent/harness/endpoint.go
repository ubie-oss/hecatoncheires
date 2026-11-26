package harness

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/hecatoncheires/pkg/utils/safe"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	endpointFile     = "endpoint.json"
	maxEndpointBytes = 4 << 10
	maxRequestBytes  = 8 << 20
	tokenBytes       = 24
)

type endpointRecord struct {
	URL  string `json:"url,omitempty"`
	Done bool   `json:"done,omitempty"`
}

type sessionEndpoint struct {
	root   *os.Root
	lock   *os.File
	server *http.Server
	served chan error
}

func openSessionEndpoint(dir string, s *Session) (*sessionEndpoint, error) {
	if dir == "" {
		return nil, goerr.New("harness: a session directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, goerr.Wrap(err, "harness: create session directory", goerr.V("dir", dir))
	}
	root, err := openPrivateRoot(dir)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	lock, err := root.OpenFile("session.lock", os.O_CREATE|os.O_RDWR|privateOpenFlags, 0o600)
	if err != nil {
		safe.Close(ctx, root)
		return nil, goerr.Wrap(err, "harness: open the session directory lock")
	}

	endpoint := &sessionEndpoint{root: root, lock: lock, served: make(chan error, 1)}
	if err := endpoint.open(s); err != nil {
		safe.Close(ctx, lock)
		safe.Close(ctx, root)
		return nil, err
	}
	return endpoint, nil
}

func (e *sessionEndpoint) open(s *Session) error {
	if err := checkPrivateFile(e.lock); err != nil {
		return err
	}
	if err := acquireSessionLock(e.lock); err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	path := "/" + token
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return goerr.Wrap(err, "harness: listen on loopback")
	}
	host := listener.Addr().String()

	server := s.mcpServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, CrossOriginProtection: &http.CrossOriginProtection{},
	})
	e.server = &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      65 * time.Second,
		IdleTimeout:       30 * time.Second,
		Handler:           authenticatedSessionHandler(host, path, handler),
	}
	if err := e.write(endpointRecord{URL: "http://" + host + path}); err != nil {
		safe.Close(context.Background(), listener)
		return err
	}
	go func() {
		err := e.server.Serve(listener)
		s.stop()
		e.served <- err
	}()
	return nil
}

func authenticatedSessionHandler(host, path string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.URL.ForceQuery || r.URL.RawQuery != "" || r.URL.RawPath != "" ||
			subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(path)) != 1 {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		handler.ServeHTTP(w, r)
	})
}

func (e *sessionEndpoint) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer safe.Close(ctx, e.root)
	defer safe.Close(ctx, e.lock)

	writeErr := e.write(endpointRecord{Done: true})
	shutdownErr := e.server.Shutdown(ctx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, e.server.Close())
	}
	serveErr := <-e.served
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(writeErr, serveErr, shutdownErr)
}

func (e *sessionEndpoint) write(record endpointRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return goerr.Wrap(err, "harness: encode the endpoint")
	}
	suffix, err := randomToken()
	if err != nil {
		return err
	}
	temp := endpointFile + "." + suffix + ".tmp"
	file, err := e.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return goerr.Wrap(err, "harness: create the endpoint file")
	}
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return errors.Join(goerr.Wrap(err, "harness: write the endpoint file"), e.root.Remove(temp))
	}
	if err := e.root.Rename(temp, endpointFile); err != nil {
		return errors.Join(goerr.Wrap(err, "harness: publish the endpoint file"), e.root.Remove(temp))
	}
	return nil
}

func openPrivateRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, goerr.Wrap(err, "harness: inspect the session directory", goerr.V("dir", dir))
	}
	if !info.IsDir() || !ownedByThisUser(info) || info.Mode().Perm()&0o077 != 0 {
		return nil, goerr.New("harness: the session directory must be a real owner-only directory (0700)", goerr.V("dir", dir))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, goerr.Wrap(err, "harness: open the session directory", goerr.V("dir", dir))
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		safe.Close(context.Background(), root)
		return nil, goerr.New("harness: the session directory changed while opening it", goerr.V("dir", dir))
	}
	return root, nil
}

func checkPrivateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return goerr.Wrap(err, "harness: inspect the session file")
	}
	if !info.Mode().IsRegular() || !ownedByThisUser(info) || info.Mode().Perm()&0o077 != 0 {
		return goerr.New("harness: the session file must be a regular owner-only file (0600)")
	}
	return nil
}

func readEndpoint(ctx context.Context, dir string) (endpointRecord, error) {
	root, err := openPrivateRoot(dir)
	if err != nil {
		return endpointRecord{}, err
	}
	defer safe.Close(ctx, root)
	file, err := root.OpenFile(endpointFile, os.O_RDONLY|privateOpenFlags, 0)
	if err != nil {
		return endpointRecord{}, goerr.Wrap(err, "harness: no evaluation endpoint in the session directory", goerr.V("dir", dir))
	}
	defer safe.Close(ctx, file)
	if err := checkPrivateFile(file); err != nil {
		return endpointRecord{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxEndpointBytes+1))
	if err != nil {
		return endpointRecord{}, goerr.Wrap(err, "harness: read the endpoint file")
	}
	var record endpointRecord
	if len(raw) > maxEndpointBytes || json.Unmarshal(raw, &record) != nil {
		return endpointRecord{}, goerr.New("harness: invalid endpoint file")
	}
	return record, nil
}

// randomToken generates an unguessable capability or request ID.
func randomToken() (string, error) {
	var token [tokenBytes]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", goerr.Wrap(err, "harness: generate a random identifier")
	}
	return hex.EncodeToString(token[:]), nil
}
