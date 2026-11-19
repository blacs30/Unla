package mcpproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/amoylab/unla/internal/common/config"
	"github.com/amoylab/unla/internal/template"
	"github.com/amoylab/unla/pkg/mcp"
)

// fakeStreamableServer is a minimal MCP Streamable HTTP server. It tracks the
// currently valid session so a test can simulate a backend restart by
// invalidating the old one.
type fakeStreamableServer struct {
	mu          sync.Mutex
	current     string
	initializes int
}

func (f *fakeStreamableServer) invalidateSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = "expired"
}

func (f *fakeStreamableServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			f.mu.Lock()
			f.initializes++
			session := fmt.Sprintf("session-%d", f.initializes)
			f.current = session
			f.mu.Unlock()

			w.Header().Set("Mcp-Session-Id", session)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if !f.validSession(r) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"download_book","description":"d","inputSchema":{"type":"object","properties":{}}}]}}`, req.ID)
		case "tools/call":
			if !f.validSession(r) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, req.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
}

func (f *fakeStreamableServer) validSession(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return r.Header.Get("Mcp-Session-Id") == f.current
}

func newFakeStreamableTransport(t *testing.T) (*StreamableTransport, *fakeStreamableServer) {
	t.Helper()
	fake := &fakeStreamableServer{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &StreamableTransport{cfg: config.MCPServerConfig{URL: srv.URL}}, fake
}

// A backend restart invalidates the session held by the gateway. FetchTools
// must re-initialize and retry instead of returning "Failed to fetch tools".
func TestStreamableTransport_FetchToolsReconnectsAfterBackendRestart(t *testing.T) {
	tr, fake := newFakeStreamableTransport(t)
	ctx := context.Background()

	if err := tr.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer tr.Stop(ctx)

	// Simulate the backend being redeployed: the old session is gone.
	fake.invalidateSessions()

	tools, err := tr.FetchTools(ctx)
	if err != nil {
		t.Fatalf("fetch tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "download_book" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	fake.mu.Lock()
	initializes := fake.initializes
	fake.mu.Unlock()
	if initializes != 2 {
		t.Fatalf("expected one re-initialize, got %d", initializes)
	}
}

// A 404 from a stale session happens before the tool runs, so CallTool may
// safely retry on a fresh session.
func TestStreamableTransport_CallToolReconnectsAfterBackendRestart(t *testing.T) {
	tr, fake := newFakeStreamableTransport(t)
	ctx := context.Background()

	if err := tr.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer tr.Stop(ctx)

	fake.invalidateSessions()

	args, _ := json.Marshal(map[string]any{"book_id": 1})
	res, err := tr.CallTool(ctx, mcp.CallToolParams{Name: "download_book", Arguments: args}, &template.RequestWrapper{})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("expected tool content")
	}
}

func TestIsSessionTerminated(t *testing.T) {
	if !isSessionTerminated(fmt.Errorf("session terminated (404). need to re-initialize")) {
		t.Fatalf("expected session terminated error to be detected")
	}
	if isSessionTerminated(nil) || isSessionTerminated(fmt.Errorf("connection refused")) {
		t.Fatalf("unrelated errors must not trigger a reconnect")
	}
}
