package mcpproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/amoylab/unla/internal/common/cnst"

	"github.com/amoylab/unla/internal/common/config"
	"github.com/amoylab/unla/internal/template"
	"github.com/amoylab/unla/pkg/mcp"
	apptrace "github.com/amoylab/unla/pkg/trace"
	"github.com/amoylab/unla/pkg/version"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// StreamableTransport implements Transport using Streamable HTTP
type StreamableTransport struct {
	mu     sync.Mutex
	client *client.Client
	cfg    config.MCPServerConfig
}

var _ Transport = (*StreamableTransport)(nil)

func (t *StreamableTransport) Start(ctx context.Context, tmplCtx *template.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.startLocked(ctx)
}

// startLocked starts the transport and initializes the MCP session. The caller
// must hold t.mu.
func (t *StreamableTransport) startLocked(ctx context.Context) error {
	if t.client != nil {
		return nil
	}

	// Create streamable transport, forwarding any configured static headers
	// (e.g. per-server API keys) on every request.
	var opts []transport.StreamableHTTPCOption
	if len(t.cfg.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(t.cfg.Headers))
	}

	httpClient, err := newTLSHTTPClient(t.cfg.TLS)
	if err != nil {
		return fmt.Errorf("failed to configure TLS client: %w", err)
	}
	if httpClient != nil {
		opts = append(opts, transport.WithHTTPBasicClient(httpClient))
	}

	streamableTransport, err := transport.NewStreamableHTTP(t.cfg.URL, opts...)
	if err != nil {
		return fmt.Errorf("failed to create Streamable HTTP transport: %w", err)
	}

	// Start the transport
	if err := streamableTransport.Start(ctx); err != nil {
		return fmt.Errorf("failed to start Streamable HTTP transport: %w", err)
	}

	// Create client with the transport
	c := client.NewClient(streamableTransport)

	// Initialize the client
	initRequest := mcpgo.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcpgo.Implementation{
		Name:    cnst.AppName,
		Version: version.Get(),
	}

	_, err = c.Initialize(ctx, initRequest)
	if err != nil {
		_ = streamableTransport.Close()
		return fmt.Errorf("failed to initialize streamable client: %w", err)
	}

	t.client = c
	return nil
}

func (t *StreamableTransport) Stop(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopLocked()
}

// stopLocked closes the client and clears it. The caller must hold t.mu.
func (t *StreamableTransport) stopLocked() error {
	if t.client == nil {
		return nil
	}
	err := t.client.Close()
	t.client = nil
	return err
}

func (t *StreamableTransport) IsRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client != nil
}

// currentClient returns the active client, if any.
func (t *StreamableTransport) currentClient() *client.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client
}

// restart drops the current session and initializes a new one. It is used when
// the backend server was restarted (or redeployed) and no longer recognizes the
// Mcp-Session-Id cached by the client.
func (t *StreamableTransport) restart(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Closing the stale session may itself fail (the backend already forgot
	// it). That must not prevent us from establishing a new one.
	_ = t.stopLocked()
	return t.startLocked(ctx)
}

// isSessionTerminated reports whether err means the backend no longer knows the
// MCP session. mcp-go returns this error when the server answers 404, which is
// what happens after the backend process is replaced.
func isSessionTerminated(err error) bool {
	return err != nil && strings.Contains(err.Error(), "session terminated (404)")
}

func (t *StreamableTransport) FetchTools(ctx context.Context) ([]mcp.ToolSchema, error) {
	scope := apptrace.Tracer(cnst.TraceMCPProxy).
		Start(ctx, cnst.SpanTransportStreamableFetchTools, oteltrace.WithSpanKind(oteltrace.SpanKindClient)).
		WithAttrs(attribute.String(cnst.AttrTransportType, "streamable-http"))
	ctx = scope.Ctx
	defer scope.End()
	if !t.IsRunning() {
		if err := t.Start(ctx, nil); err != nil {
			return nil, err
		}
	}

	// List available tools
	toolsResult, err := t.listTools(ctx)
	if err != nil {
		if !isSessionTerminated(err) {
			return nil, fmt.Errorf("failed to list tools: %w", err)
		}
		// The backend was restarted and dropped our session; re-initialize and
		// retry once. Listing tools has no side effects, so this is safe.
		if restartErr := t.restart(ctx); restartErr != nil {
			return nil, fmt.Errorf("failed to list tools: %w (reinitialize failed: %v)", err, restartErr)
		}
		toolsResult, err = t.listTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list tools: %w", err)
		}
	}

	// Convert from mcpgo.Tool to mcp.ToolSchema
	tools := make([]mcp.ToolSchema, len(toolsResult.Tools))
	for i, schema := range toolsResult.Tools {
		// Create local mcp package ToolInputSchema
		inputSchema := mcp.ToolInputSchema{
			Type:       "object",
			Properties: make(map[string]any),
		}

		// Convert mcpgo InputSchema to local mcp format
		rawSchema, err := json.Marshal(schema.InputSchema)
		if err == nil {
			// Parse schema properties
			var schemaMap map[string]interface{}
			if err := json.Unmarshal(rawSchema, &schemaMap); err == nil {
				if properties, ok := schemaMap["properties"].(map[string]interface{}); ok {
					inputSchema.Properties = properties
				}
				if typ, ok := schemaMap["type"].(string); ok {
					inputSchema.Type = typ
				}
				if required, ok := schemaMap["required"].([]interface{}); ok {
					reqStrings := make([]string, len(required))
					for j, r := range required {
						if rStr, ok := r.(string); ok {
							reqStrings[j] = rStr
						}
					}
					inputSchema.Required = reqStrings
				}
			}
		}

		tools[i] = mcp.ToolSchema{
			Name:        schema.Name,
			Description: schema.Description,
			InputSchema: inputSchema,
		}
	}

	return tools, nil
}

func (t *StreamableTransport) CallTool(ctx context.Context, params mcp.CallToolParams, req *template.RequestWrapper) (*mcp.CallToolResult, error) {
	scope := apptrace.Tracer(cnst.TraceMCPProxy).
		Start(ctx, cnst.SpanTransportStreamableCallTool, oteltrace.WithSpanKind(oteltrace.SpanKindClient)).
		WithAttrs(
			attribute.String(cnst.AttrTransportType, "streamable-http"),
			attribute.String(cnst.AttrMCPTool, params.Name),
		)
	ctx = scope.Ctx
	defer scope.End()
	if !t.IsRunning() {
		var args map[string]any
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
		tmplCtx, err := template.AssembleTemplateContext(req, args, nil)
		if err != nil {
			return nil, err
		}

		if err := t.Start(ctx, tmplCtx); err != nil {
			return nil, err
		}
	}
	defer func() {
		if t.cfg.Policy == cnst.PolicyOnDemand {
			_ = t.Stop(ctx)
		}
	}()

	// Prepare tool call request parameters
	toolCallRequestParams := make(map[string]interface{})
	if err := json.Unmarshal(params.Arguments, &toolCallRequestParams); err != nil {
		return nil, fmt.Errorf("failed to unmarshal arguments: %w", err)
	}

	// Call tool
	callRequest := mcpgo.CallToolRequest{}
	callRequest.Params.Name = params.Name
	callRequest.Params.Arguments = toolCallRequestParams

	res, err := t.callTool(ctx, &callRequest)
	if err != nil {
		if !isSessionTerminated(err) {
			return nil, fmt.Errorf("failed to call tool: %w", err)
		}
		// The backend was restarted and dropped our session. The call was
		// rejected with 404 before it ran, so retrying cannot duplicate it.
		if restartErr := t.restart(ctx); restartErr != nil {
			return nil, fmt.Errorf("failed to call tool: %w (reinitialize failed: %v)", err, restartErr)
		}
		res, err = t.callTool(ctx, &callRequest)
		if err != nil {
			return nil, fmt.Errorf("failed to call tool: %w", err)
		}
	}

	return convertMCPGoResult(res), nil
}

func (t *StreamableTransport) callTool(ctx context.Context, req *mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	c := t.currentClient()
	if c == nil {
		return nil, fmt.Errorf("transport is not running")
	}
	return c.CallTool(ctx, *req)
}

func (t *StreamableTransport) listTools(ctx context.Context) (*mcpgo.ListToolsResult, error) {
	c := t.currentClient()
	if c == nil {
		return nil, fmt.Errorf("transport is not running")
	}
	return c.ListTools(ctx, mcpgo.ListToolsRequest{})
}

// FetchPrompts returns all prompts
func (t *StreamableTransport) FetchPrompts(ctx context.Context) ([]mcp.PromptSchema, error) {
	return []mcp.PromptSchema{}, nil
}

// FetchPrompt returns a specific prompt by name
func (t *StreamableTransport) FetchPrompt(ctx context.Context, name string) (*mcp.PromptSchema, error) {
	return nil, &HTTPError{
		StatusCode: http.StatusNotFound,
		Message:    "Prompt not found",
	}
}
