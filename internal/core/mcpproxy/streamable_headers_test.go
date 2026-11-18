package mcpproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/amoylab/unla/internal/common/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStreamableTransport_StaticHeaders verifies that headers configured in
// MCPServerConfig.Headers are sent to the upstream MCP server on every request,
// including the initial initialize call.
func TestStreamableTransport_StaticHeaders(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = []string{
			r.Header.Get("X-Paperless-API-Key"),
			r.Header.Get("X-Paperless-URL"),
		}
		mu.Unlock()

		var req struct {
			ID *int `json:"id"`
		}
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		if req.ID == nil {
			// Notification (e.g. notifications/initialized): no body expected.
			w.WriteHeader(http.StatusAccepted)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      *req.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "test", "version": "1"},
			},
		})
	}))
	defer srv.Close()

	tr := &StreamableTransport{cfg: config.MCPServerConfig{
		Type: "streamable-http",
		Name: "paperless",
		URL:  srv.URL,
		Headers: map[string]string{
			"X-Paperless-API-Key": "secret-key",
			"X-Paperless-URL":     "http://paperless.local",
		},
	}}

	require.NoError(t, tr.Start(context.Background(), nil))
	defer func() { _ = tr.Stop(context.Background()) }()
	assert.True(t, tr.IsRunning())

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, seen, "upstream received no requests")
	for req, headers := range seen {
		assert.Equal(t, []string{"secret-key", "http://paperless.local"}, headers, "headers missing on %s", req)
	}
}
