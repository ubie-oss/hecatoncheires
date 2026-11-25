package harness

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrSessionDone means the session has finished.
var ErrSessionDone = errors.New("harness: the session has finished")

// SessionClient reads and connects to a local MCP rendezvous for shell clients.
type SessionClient struct {
	session   *mcp.ClientSession
	transport *http.Transport
}

// SessionFinished reports whether the owner-only endpoint records a clean shutdown.
func SessionFinished(ctx context.Context, dir string) bool {
	endpoint, err := readEndpoint(ctx, dir)
	return err == nil && endpoint.Done
}

// DialSession uses an owner-only endpoint file and connects directly to IPv4 loopback.
func DialSession(ctx context.Context, dir string) (*SessionClient, error) {
	endpoint, err := readEndpoint(ctx, dir)
	if err != nil {
		return nil, err
	}
	if endpoint.Done {
		return nil, ErrSessionDone
	}
	if err := checkLoopback(endpoint.URL); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: 3 * time.Second}
	transport := &http.Transport{DialContext: dialer.DialContext, ResponseHeaderTimeout: 60 * time.Second}
	httpClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "hecatoncheires-harness", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, sessionTransportError(ctx, "harness: could not connect to the local session")
	}
	return &SessionClient{session: conn, transport: transport}, nil
}

func checkLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && len(u.Path) == 1+2*tokenBytes && u.Path[0] == '/' {
		port, portErr := strconv.ParseUint(u.Port(), 10, 16)
		_, tokenErr := hex.DecodeString(u.Path[1:])
		if portErr == nil && port > 0 && tokenErr == nil {
			return nil
		}
	}
	return goerr.New("harness: invalid local session URL")
}

func sessionTransportError(ctx context.Context, message string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return goerr.Wrap(ctxErr, message)
	}
	// HTTP and MCP transport errors may carry the capability URL.
	return goerr.New(message)
}

// Close ends the connection.
func (c *SessionClient) Close() error {
	defer c.transport.CloseIdleConnections()
	if err := c.session.Close(); err != nil {
		return goerr.New("harness: close the local session connection")
	}
	return nil
}

// Call calls a session tool and returns its textual result and MCP error flag.
func (c *SessionClient) Call(ctx context.Context, tool string, args map[string]any) (text string, failed bool, err error) {
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", false, sessionTransportError(ctx, "harness: could not call the local session")
	}
	var parts []string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}
