package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// ─── MCP Server Configuration ────────────────────────────────────────────────

// MCPServerConfig describes a single MCP server entry in the JSON config.
// Fully compatible with Claude Desktop's claude_desktop_config.json format.
// Supports "streamablehttp", "sse", and "stdio" transport types.
//
// Claude Desktop format (env as object):
//
//	{
//	  "mcpServers": {
//	    "my-server": {
//	      "command": "npx",
//	      "args": ["-y", "some-package"],
//	      "env": {
//	        "API_KEY": "value"
//	      }
//	    }
//	  }
//	}
//
// Extended format (env as array, explicit type, headers for HTTP/SSE):
//
//	{
//	  "mcpServers": {
//	    "remote-mcp": {
//	      "type": "streamablehttp",
//	      "url": "https://mcp.example.com",
//	      "headers": {
//	        "Authorization": "Bearer TOKEN"
//	      }
//	    },
//	    "local-tool": {
//	      "command": "/usr/local/bin/mytool",
//	      "args": ["--flag"],
//	      "env": ["KEY=VALUE"]
//	    }
//	  }
//	}
type MCPServerConfig struct {
	Type     string            `json:"type,omitempty"`     // "streamablehttp", "sse", or "stdio" (auto-inferred if empty)
	URL      string            `json:"url,omitempty"`      // for streamablehttp / sse
	Headers  map[string]string `json:"headers,omitempty"`  // for streamablehttp / sse
	Command  string            `json:"command,omitempty"`  // for stdio
	Args     []string          `json:"args,omitempty"`     // for stdio
	Env      FlexEnv           `json:"env,omitempty"`      // for stdio: accepts both ["K=V"] and {"K":"V"}
	Disabled bool              `json:"disabled,omitempty"` // if true, skip this server (Claude Desktop compat)
}

// FlexEnv holds environment variables and supports JSON unmarshalling from
// either an array of "KEY=VALUE" strings (our extended format) or an object
// of {"KEY": "VALUE"} pairs (Claude Desktop format).
type FlexEnv struct {
	Pairs []string // always stored as ["KEY=VALUE", …]
}

// MarshalJSON serialises FlexEnv as an array of strings.
func (e FlexEnv) MarshalJSON() ([]byte, error) {
	if e.Pairs == nil {
		return []byte("null"), nil
	}
	return json.Marshal(e.Pairs)
}

// UnmarshalJSON accepts both ["KEY=VALUE"] and {"KEY": "VALUE"} forms.
func (e *FlexEnv) UnmarshalJSON(data []byte) error {
	// Try array first.
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		e.Pairs = arr
		return nil
	}
	// Try object (Claude Desktop format).
	var obj map[string]string
	if err := json.Unmarshal(data, &obj); err == nil {
		for k, v := range obj {
			e.Pairs = append(e.Pairs, k+"="+v)
		}
		return nil
	}
	return fmt.Errorf("env must be an array of \"KEY=VALUE\" strings or an object of {\"KEY\": \"VALUE\"} pairs")
}

// EnvList returns the environment variables as a string slice suitable
// for transport.NewStdio.
func (s *MCPServerConfig) EnvList() []string {
	return s.Env.Pairs
}

// MCPConfig is the top-level JSON configuration structure.
type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// InferTransportType auto-detects the transport type from the config fields
// when the "type" field is omitted. If "command" is set → stdio; if "url" is
// set → streamablehttp; otherwise returns "".
func InferTransportType(srv *MCPServerConfig) string {
	if srv.Type != "" {
		return srv.Type
	}
	if srv.Command != "" {
		return "stdio"
	}
	if srv.URL != "" {
		return "streamablehttp"
	}
	return ""
}

// LoadMCPConfig reads and parses the MCP configuration JSON file.
func LoadMCPConfig(path string) (*MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read MCP config file: %w", err)
	}
	var cfg MCPConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse MCP config: %w", err)
	}
	return &cfg, nil
}

// ─── MCP Client Manager ─────────────────────────────────────────────────────

// MCPClientManager manages connections to multiple MCP servers and registers
// their tools into the ToolRegistry.
type MCPClientManager struct {
	clients map[string]*client.Client // serverName → MCP client
}

// NewMCPClientManager creates an empty client manager.
func NewMCPClientManager() *MCPClientManager {
	return &MCPClientManager{
		clients: make(map[string]*client.Client),
	}
}

// ConnectAll connects to all MCP servers defined in the config, discovers
// their tools, and registers them into the given ToolRegistry.
// Returns a map of server name → error for servers that failed to connect.
// Successfully connected servers are not included in the returned map.
func (m *MCPClientManager) ConnectAll(cfg *MCPConfig, registry *ToolRegistry) map[string]error {
	if cfg == nil || len(cfg.MCPServers) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var failures map[string]error
	for name, srv := range cfg.MCPServers {
		if err := m.connectOne(ctx, name, srv, registry); err != nil {
			log.Printf("[mcp] failed to connect %q: %v", name, err)
			if failures == nil {
				failures = make(map[string]error)
			}
			failures[name] = err
		}
	}
	return failures
}

// connectOne establishes a connection to a single MCP server, initializes it,
// discovers its tools, and registers them in the registry.
func (m *MCPClientManager) connectOne(
	ctx context.Context,
	name string,
	srv MCPServerConfig,
	registry *ToolRegistry,
) error {
	var c *client.Client
	var err error

	transportType := InferTransportType(&srv)
	switch strings.ToLower(transportType) {
	case "streamablehttp", "streamable-http", "http":
		if srv.URL == "" {
			return fmt.Errorf("url is required for streamablehttp transport")
		}
		opts := []transport.StreamableHTTPCOption{}
		if len(srv.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(srv.Headers))
		}
		c, err = client.NewStreamableHttpClient(srv.URL, opts...)
		if err != nil {
			return fmt.Errorf("create streamable HTTP client: %w", err)
		}

	case "sse":
		if srv.URL == "" {
			return fmt.Errorf("url is required for sse transport")
		}
		opts := []transport.ClientOption{}
		if len(srv.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(srv.Headers))
		}
		c, err = client.NewSSEMCPClient(srv.URL, opts...)
		if err != nil {
			return fmt.Errorf("create SSE client: %w", err)
		}

	case "stdio":
		if srv.Command == "" {
			return fmt.Errorf("command is required for stdio transport")
		}
		stdioTransport := transport.NewStdio(srv.Command, srv.EnvList(), srv.Args...)
		c = client.NewClient(stdioTransport)
		if err := c.Start(ctx); err != nil {
			return fmt.Errorf("start stdio client: %w", err)
		}

	default:
		return fmt.Errorf("unsupported transport type %q (use streamablehttp, sse, or stdio)", transportType)
	}

	// Initialize the MCP session.
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "llm_telebot",
		Version: "1.0.0",
	}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	serverInfo, err := c.Initialize(ctx, initReq)
	if err != nil {
		c.Close()
		return fmt.Errorf("initialize: %w", err)
	}

	log.Printf("[mcp] connected to %q → server %s (v%s)",
		name, serverInfo.ServerInfo.Name, serverInfo.ServerInfo.Version)

	// Discover and register tools.
	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return fmt.Errorf("list tools: %w", err)
	}

	for _, tool := range toolsResult.Tools {
		wrapper := &MCPRemoteTool{
			client:     c,
			serverName: name,
			tool:       tool,
		}
		registry.Register(wrapper)
		log.Printf("[mcp]   registered tool: %s (from %s)", tool.Name, name)
	}

	m.clients[name] = c
	log.Printf("[mcp] %q: %d tools registered", name, len(toolsResult.Tools))
	return nil
}

// Close gracefully shuts down all MCP client connections.
func (m *MCPClientManager) Close() {
	for name, c := range m.clients {
		if err := c.Close(); err != nil {
			log.Printf("[mcp] error closing %q: %v", name, err)
		}
	}
}

// ─── MCPRemoteTool ───────────────────────────────────────────────────────────

// MCPRemoteTool wraps a remote MCP server tool as a local MCPTool, so it can
// be registered in the ToolRegistry and called by the LLM like any built-in tool.
type MCPRemoteTool struct {
	client     *client.Client
	serverName string
	tool       mcp.Tool
}

func (t *MCPRemoteTool) Name() string        { return t.tool.Name }
func (t *MCPRemoteTool) Description() string { return t.tool.Description }

// Parameters returns the tool's input schema as a raw JSON-serializable object
// compatible with OpenAI's FunctionDefinition.Parameters (any type).
// Enum values are coerced to strings for compatibility with APIs like Gemini.
func (t *MCPRemoteTool) Parameters() any {
	// If the tool has a RawInputSchema, return it as-is.
	if t.tool.RawInputSchema != nil {
		var raw any
		if err := json.Unmarshal(t.tool.RawInputSchema, &raw); err == nil {
			sanitizeSchema(raw)
			return raw
		}
	}
	// Otherwise convert the structured InputSchema to map and sanitize.
	data, err := json.Marshal(t.tool.InputSchema)
	if err != nil {
		return t.tool.InputSchema
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return t.tool.InputSchema
	}
	sanitizeSchema(raw)
	return raw
}

// sanitizeSchema walks a JSON schema tree and fixes enum fields for
// compatibility with APIs like Gemini that only allow enum on STRING types.
// When a property has "enum", its type is forced to "string" and all enum
// values are coerced to strings.
func sanitizeSchema(v any) {
	switch val := v.(type) {
	case map[string]any:
		// If this object has an "enum" array, force type to string and
		// coerce all values to strings.
		if enumArr, ok := val["enum"].([]any); ok {
			val["type"] = "string"
			for i, item := range enumArr {
				switch n := item.(type) {
				case float64:
					if n == float64(int64(n)) {
						enumArr[i] = fmt.Sprintf("%d", int64(n))
					} else {
						enumArr[i] = fmt.Sprintf("%g", n)
					}
				case bool:
					enumArr[i] = fmt.Sprintf("%t", n)
				}
				// strings stay as-is
			}
		}
		// Recurse into all values.
		for _, child := range val {
			sanitizeSchema(child)
		}
	case []any:
		for _, child := range val {
			sanitizeSchema(child)
		}
	}
}

// Execute calls the remote MCP tool and returns the text result.
func (t *MCPRemoteTool) Execute(args string) (string, error) {
	// Parse the JSON arguments from the LLM into a map.
	var arguments map[string]any
	if args != "" && args != "{}" {
		if err := json.Unmarshal([]byte(args), &arguments); err != nil {
			return "", fmt.Errorf("invalid arguments JSON: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = t.tool.Name
	req.Params.Arguments = arguments

	result, err := t.client.CallTool(ctx, req)
	if err != nil {
		return "", fmt.Errorf("MCP tool %q call failed: %w", t.tool.Name, err)
	}

	// Extract text from the result content.
	return extractTextFromContent(result), nil
}

// extractTextFromContent concatenates all text content pieces from a CallToolResult.
func extractTextFromContent(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return "(no output)"
	}

	var parts []string
	for _, c := range result.Content {
		switch content := c.(type) {
		case mcp.TextContent:
			parts = append(parts, content.Text)
		case *mcp.TextContent:
			parts = append(parts, content.Text)
		default:
			// For non-text content (images, audio, etc.), marshal to JSON.
			data, err := json.Marshal(content)
			if err == nil {
				parts = append(parts, string(data))
			}
		}
	}

	if len(parts) == 0 {
		return "(no text output)"
	}
	return strings.Join(parts, "\n")
}
