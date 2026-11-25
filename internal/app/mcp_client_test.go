package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestMCPSSEStartupCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer endpoint.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := NewMCPClientManager()
	result := make(chan error, 1)
	go func() {
		result <- m.connectOne(ctx, "test", MCPServerConfig{Type: "sse", URL: endpoint.URL}, NewToolRegistry())
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE startup did not reach the endpoint")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE startup did not stop after cancellation")
	}
}

func TestMCPSSEConnectionOutlivesStartup(t *testing.T) {
	s := server.NewMCPServer("example", "1")
	s.AddTool(mcp.NewTool("ping"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})
	endpoint := httptest.NewServer(server.NewSSEServer(s))
	defer endpoint.Close()
	m := NewMCPClientManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	registry := NewToolRegistry()
	if err := m.connectOne(ctx, "test", MCPServerConfig{Type: "sse", URL: endpoint.URL + "/sse"}, registry); err != nil {
		t.Fatal(err)
	}
	cancel()
	tool, ok := registry.Get("ping").(*MCPRemoteTool)
	if !ok {
		t.Fatal("ping tool was not registered")
	}
	callCtx, stopCall := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopCall()
	if got, err := tool.ExecuteContext(callCtx, "{}"); err != nil || got != "pong" {
		t.Fatalf("tool = %q, %v, want pong", got, err)
	}
}
