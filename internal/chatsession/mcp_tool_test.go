package chatsession

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/starfall-org/generative-server/internal/chatstore"
)

func TestMCPGrantsAndParameterOverrides(t *testing.T) {
	for _, protocol := range []string{"streamable_http", "sse"} {
		t.Run(protocol, func(t *testing.T) {
			var calls atomic.Int32
			observed := make(chan string, 1)
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			type input struct {
				Text string `json:"text" jsonschema:"required"`
			}
			type output struct {
				Echo string `json:"echo"`
			}
			mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in input) (*mcp.CallToolResult, output, error) {
				calls.Add(1)
				observed <- in.Text
				return nil, output{Echo: "ok"}, nil
			})
			var handler http.Handler
			if protocol == "sse" {
				handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
			} else {
				handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			}
			upstream := httptest.NewServer(handler)
			defer upstream.Close()
			dir := filepath.Join(t.TempDir(), "data")
			m := New(chatstore.New(dir))
			m.AllowLocalAPI(true)
			serverInfo := map[string]any{"id": "s", "protocol": protocol, "url": upstream.URL, "auth": map[string]any{"method": "NONE"}}
			settings := map[string]any{"quickMcp": map[string]bool{"s": true}, "mcpTools": map[string]any{"s": map[string]bool{"echo": true}}, "mcpToolParameters": map[string]any{"s": map[string]any{"echo": map[string]any{"text": map[string]any{"enabled": false, "defaultValue": "safe"}}}}}
			runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "mcp": []any{serverInfo}, "toolSettings": settings})
			if _, err := m.Open("owner", "c", sample, runtime); err != nil {
				t.Fatal(err)
			}
			if err := m.StartMCPTool("owner", "c", "call", "s", "echo", json.RawMessage(`{"text":"unsafe"}`)); !errors.Is(err, ErrDenied) || calls.Load() != 0 {
				t.Fatalf("no grant: %v", err)
			}
			runtime, _ = json.Marshal(map[string]any{"source": "mobile_ephemeral", "mcp": []any{serverInfo}, "toolSettings": settings, "toolGrants": map[string]any{"mcp": map[string]any{"s": map[string]any{"enabled": true, "tools": map[string]bool{"echo": true}}}}})
			if err := m.Configure("owner", "c", runtime); err != nil {
				t.Fatal(err)
			}
			tools, err := m.MCPTools("owner", "c", "s")
			if err != nil || !strings.Contains(string(tools), "echo") {
				t.Fatalf("list: %s %v", tools, err)
			}
			ch, stop, err := m.Subscribe("owner", "c")
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			args := json.RawMessage(`{"text":"unsafe"}`)
			if err = m.StartMCPTool("owner", "c", "call", "s", "echo", args); err != nil {
				t.Fatal(err)
			}
			if err = m.StartMCPTool("owner", "c", "call", "s", "echo", args); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(8 * time.Second)
			var cursor uint64
			done := false
			for !done {
				events, err := m.Events("owner", "c", cursor)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events {
					cursor = e.Seq
					if e.Kind == "error" {
						t.Fatalf("MCP error: %+v", e)
					}
					if e.Kind == "completed" {
						done = true
					}
				}
				if !done {
					select {
					case <-ch:
					case <-deadline:
						t.Fatal("timeout")
					}
				}
			}
			view, err := m.Get("owner", "c")
			if err != nil || calls.Load() != 1 || strings.Contains(string(view.Conversation), "safe") || strings.Contains(string(view.Conversation), "unsafe") || <-observed != "safe" {
				t.Fatalf("view=%s calls=%d err=%v", view.Conversation, calls.Load(), err)
			}
		})
	}
}

func TestMCPPermissionIntersection(t *testing.T) {
	base := map[string]any{"source": "mobile_ephemeral", "mcp": []any{map[string]any{"id": "s", "url": "https://example.invalid/mcp", "protocol": "sse"}}, "toolGrants": map[string]any{"mcp": map[string]any{"s": map[string]any{"enabled": true, "tools": map[string]bool{"echo": true}}}}, "toolSettings": map[string]any{"quickMcp": map[string]bool{"s": true}, "mcpTools": map[string]any{"s": map[string]bool{"echo": true}}}}
	chat := json.RawMessage(`{"profile_id":"profile"}`)
	raw, _ := json.Marshal(base)
	if _, _, err := mcpConfig(raw, chat, "s", "echo", false); !errors.Is(err, ErrDenied) {
		t.Fatalf("missing profile: %v", err)
	}
	base["profile"] = map[string]any{"id": "profile", "config": map[string]any{"mcpAccess": map[string]any{"s": map[string]any{"enabled": true, "tools": map[string]bool{"echo": false}}}}}
	raw, _ = json.Marshal(base)
	if _, _, err := mcpConfig(raw, chat, "s", "echo", false); !errors.Is(err, ErrDenied) {
		t.Fatalf("profile denied tool: %v", err)
	}
	base["profile"] = map[string]any{"id": "profile", "config": map[string]any{"mcpAccess": map[string]any{"s": map[string]any{"enabled": true, "tools": map[string]bool{"echo": true}}}}}
	settings := base["toolSettings"].(map[string]any)
	settings["webSearchProviderId"] = "s"
	settings["webSearchEnabled"] = false
	raw, _ = json.Marshal(base)
	if _, _, err := mcpConfig(raw, chat, "s", "echo", false); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled web search: %v", err)
	}
	settings["webSearchEnabled"] = true
	raw, _ = json.Marshal(base)
	if _, _, err := mcpConfig(raw, chat, "s", "echo", false); err != nil {
		t.Fatalf("fully granted: %v", err)
	}
}

func TestMCPDiscoveryCancelledOnRevocation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	manager := New(chatstore.New(filepath.Join(t.TempDir(), "data")))
	manager.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "mcp": []any{map[string]any{"id": "s", "protocol": "streamable_http", "url": upstream.URL}}, "toolGrants": map[string]any{"mcp": map[string]any{"s": map[string]any{"enabled": true}}}, "toolSettings": map[string]any{"quickMcp": map[string]bool{"s": true}}})
	if _, err := manager.Open("owner", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := manager.MCPTools("owner", "c", "s"); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not start")
	}
	if err := manager.Configure("owner", "c", runtime); !errors.Is(err, ErrBusy) {
		t.Fatalf("configuration changed during discovery: %v", err)
	}
	manager.Revoke("owner")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked discovery returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked discovery did not cancel")
	}
}
