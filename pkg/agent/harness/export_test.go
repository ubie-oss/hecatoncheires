package harness

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConnectInMemoryForTest exercises the same MCP tools without any network.
func ConnectInMemoryForTest(t *testing.T) (*Session, *mcp.ClientSession) {
	t.Helper()
	s := &Session{tickets: make(map[string]*ticket), wake: make(chan struct{})}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := s.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() {
		s.stop()
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return s, clientSession
}

func StopForTest(s *Session) { s.stop() }

var CheckLoopbackForTest = checkLoopback

var AuthenticatedSessionHandlerForTest = authenticatedSessionHandler
