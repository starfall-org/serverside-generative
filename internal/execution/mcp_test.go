package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPListAndCallViaOfficialSDK(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-remote", Version: "1.0"}, nil)
	type input struct {
		Text string `json:"text"`
	}
	type output struct {
		Echo string `json:"echo"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo its input"}, func(_ context.Context, _ *mcp.CallToolRequest, in input) (*mcp.CallToolResult, output, error) {
		return nil, output{Echo: in.Text}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var hit atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		hit.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()
	state := State{MCP: []MCPServer{{ID: "tools", URL: ts.URL, Headers: map[string]string{"Authorization": "Bearer fake-token"}}}}
	listing, err := ListTools(context.Background(), state, "tools")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(listing), `"name":"echo"`) {
		t.Fatalf("missing MCP tool: %s", listing)
	}
	result, err := CallTool(context.Background(), state, "tools", "echo", json.RawMessage(`{"text":"hello from MCP"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "hello from MCP") {
		t.Fatalf("wrong MCP tool reply: %s", result)
	}
	if hit.Load() < 2 {
		t.Fatalf("expected real HTTP exchange, got %d requests", hit.Load())
	}
	if _, err := ListTools(context.Background(), state, "unknown"); err == nil {
		t.Fatal("unauthorized MCP server ID allowed")
	}
}

func TestProviderAndMCPEndpointValidation(t *testing.T) {
	st := State{Provider: Provider{Platform: "openai-chat", Model: "test", APIKey: "fake"}, Conversation: []Message{{Role: "user", Content: "Hi"}}}
	if err := st.ValidateModel(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://public.example.com", "ftp://localhost", "https://user:password@example.com"} {
		st.Provider.BaseURL = value
		if st.ValidateModel() == nil {
			t.Fatalf("insecure API endpoint accepted: %s", value)
		}
	}
	st.Provider.BaseURL = "http://127.0.0.1:1111"
	if err := st.ValidateModel(); err != nil {
		t.Fatal(err)
	}
	if err := (State{}).ValidateMCP("unknown"); err == nil {
		t.Fatal("unknown MCP accepted")
	}
}
