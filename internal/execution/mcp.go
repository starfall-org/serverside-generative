package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMCPOutputBytes = 256 * 1024

// authTransport keeps ephemeral MCP authorization headers in process RAM only.
// Redirects are prohibited so headers are not disclosed to another origin.
type authTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range t.headers {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

func (s State) ValidateMCP(id string) error { _, err := s.enabledMCP(id); return err }

func (s State) enabledMCP(id string) (MCPServer, error) {
	for _, cfg := range s.MCP {
		if cfg.ID == id {
			if cfg.ID == "" || len(cfg.ID) > 128 {
				return MCPServer{}, errors.New("invalid MCP identifier")
			}
			if cfg.URL == "" {
				return MCPServer{}, errors.New("MCP endpoint missing")
			}
			if err := validateRemoteEndpoint(cfg.URL); err != nil {
				return MCPServer{}, err
			}
			if len(cfg.Headers) > 32 {
				return MCPServer{}, errors.New("too many MCP headers")
			}
			for k, v := range cfg.Headers {
				if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") || len(v) > 8*1024 {
					return MCPServer{}, errors.New("invalid MCP header")
				}
			}
			return cfg, nil
		}
	}
	return MCPServer{}, errors.New("MCP server is not enabled in the session")
}

func openMCPSession(ctx context.Context, cfg MCPServer) (*mcp.ClientSession, error) {
	cli := &http.Client{
		Timeout:       45 * time.Second,
		Transport:     &authTransport{headers: cfg.Headers, base: http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "serverside", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: cfg.URL, HTTPClient: cli, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("MCP connect failed: %w", err)
	}
	return session, nil
}

// ListTools uses the official MCP SDK. It only queries an endpoint explicitly
// included in the Android handoff; there is no implicit command execution.
func ListTools(ctx context.Context, state State, serverID string) (json.RawMessage, error) {
	cfg, err := state.enabledMCP(serverID)
	if err != nil {
		return nil, err
	}
	session, err := openMCPSession(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	tools := make([]*mcp.Tool, 0, 16)
	cursor := ""
	for page := 0; page < 20; page++ {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("MCP list tools failed: %w", err)
		}
		tools = append(tools, result.Tools...)
		if len(tools) > 256 {
			return nil, errors.New("MCP tool list exceeds limit")
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
		if page == 19 {
			return nil, errors.New("MCP tool pagination exceeds limit")
		}
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxMCPOutputBytes {
		return nil, errors.New("MCP tool list too large")
	}
	return raw, nil
}

// CallTool is explicit and authenticated. The model is never allowed to invoke
// MCP tools automatically until a separate approval/authorization design exists.
func CallTool(ctx context.Context, state State, serverID, toolName string, args json.RawMessage) (json.RawMessage, error) {
	cfg, err := state.enabledMCP(serverID)
	if err != nil {
		return nil, err
	}
	if toolName == "" || len(toolName) > 256 {
		return nil, errors.New("invalid MCP tool name")
	}
	if len(args) > 64*1024 {
		return nil, errors.New("MCP arguments too large")
	}
	var arguments any = map[string]any{}
	if len(args) != 0 && string(args) != "null" {
		if err = json.Unmarshal(args, &arguments); err != nil {
			return nil, errors.New("invalid MCP tool arguments")
		}
		if _, ok := arguments.(map[string]any); !ok {
			return nil, errors.New("MCP tool arguments must be an object")
		}
	}
	session, err := openMCPSession(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: toolName, Arguments: arguments})
	if err != nil {
		return nil, fmt.Errorf("MCP call failed: %w", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxMCPOutputBytes {
		return nil, errors.New("MCP result too large")
	}
	return raw, nil
}
