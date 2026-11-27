package storedruntime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starfall-org/generative-server/internal/catalog"
	"github.com/starfall-org/generative-server/internal/secrets"
)

func TestResolveStoredMCPUsesOnlySelectedCatalogAndExplicitGrants(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	docs, err := catalog.Empty()
	if err != nil {
		t.Fatal(err)
	}
	docs["mcp.yaml"] = "schema_version: 1\nrevision: 1\nservers:\n  - id: s\n    name: S\n    protocol: streamable_http\n    url: https://example.com/mcp\n    auth: {method: CUSTOM_HEADER, header_name: X-Token, credential_ref: mcp/s/key}\n"
	docs["profiles.yaml"] = "schema_version: 1\nrevision: 1\nchatProfiles: []\ntoolSettings:\n  quickMcp: {s: true}\n  mcpTools: {s: {echo: true}}\n"
	if _, err = catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put("mcp", "s", "key", "", "auth", []byte("stored-mcp-secret")); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"source":"stored","toolGrants":{"mcp":{"s":{"enabled":true,"tools":{"echo":true}}}},"mcp":[{"id":"s","url":"https://evil.invalid","auth":{"value":"fake"}}],"toolSettings":{"quickMcp":{"s":false}}}`)
	result, err := ResolveMCP(dir, raw, json.RawMessage(`{"id":"c"}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		MCP []struct {
			URL  string `json:"url"`
			Auth struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"auth"`
		} `json:"mcp"`
		ToolSettings struct {
			Quick map[string]bool `json:"quickMcp"`
		} `json:"toolSettings"`
	}
	if json.Unmarshal(result, &decoded) != nil || len(decoded.MCP) != 1 || decoded.MCP[0].URL != "https://example.com/mcp" || decoded.MCP[0].Auth.Key != "X-Token" || decoded.MCP[0].Auth.Value != "stored-mcp-secret" || !decoded.ToolSettings.Quick["s"] {
		t.Fatal("stored runtime did not use catalog")
	}
	if strings.Contains(string(result), "evil.invalid") || strings.Contains(string(result), "fake") {
		t.Fatal("mobile connection settings were used")
	}
	if _, err = ResolveMCP(dir, raw, json.RawMessage(`{"id":"c"}`), "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown server: %v", err)
	}
	if _, err = ResolveMCP(dir, json.RawMessage(`{"source":"stored"}`), json.RawMessage(`{"id":"c"}`), "s"); !errors.Is(err, ErrAuth) {
		t.Fatalf("missing grants: %v", err)
	}
	if _, err = ResolveMCP(dir, raw, json.RawMessage(`{"id":"c","profile_id":"missing"}`), "s"); !errors.Is(err, ErrAuth) {
		t.Fatalf("missing profile: %v", err)
	}
}
