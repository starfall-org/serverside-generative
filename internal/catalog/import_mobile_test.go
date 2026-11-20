package catalog_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/storedruntime"
)

func TestMobileProviderImportPreservesModelsAndEncryptedKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := catalog.New(dir)
	raw := json.RawMessage(`{
        "id":"p","name":"Provider","type":"openai","baseUrl":"https://example.invalid/v1",
        "auth":{"method":"bearer_token","key":"Authorization","value":"Bearer primary-sentinel"},
        "config":{"modelIds":["first","second"],"maxTokens":8192,"multipleApiKeys":true,
            "apiKeys":[{"id":"main","label":"Main","value":"primary-sentinel"},{"id":"other","label":"Backup","value":"secondary-sentinel"}],
            "modelConfigs":{"first":{"displayName":"First","contextWindowTokens":32000,"temperature":0.5},"second":{"displayName":"Second"}}}
    }`)
	if err := s.ImportMobile("provider", raw); err != nil {
		t.Fatal(err)
	}
	bundle, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range bundle.Files {
		if strings.Contains(doc, "primary-sentinel") || strings.Contains(doc, "secondary-sentinel") {
			t.Fatal("plaintext credential in catalog")
		}
	}
	if !strings.Contains(bundle.Files["models.yaml"], "contextWindowTokens: 32000") ||
		!strings.Contains(bundle.Files["models.yaml"], "temperature: 0.5") {
		t.Fatal("model settings missing")
	}
	index, err := storedruntime.ReadCatalog(dir)
	if err != nil || len(index.Providers) != 1 || len(index.Providers[0].Models) != 2 ||
		len(index.Providers[0].Keys) != 2 {
		t.Fatalf("imported catalog incomplete: %v", err)
	}
	for _, item := range []struct{ key, value string }{{"main", "primary-sentinel"}, {"other", "secondary-sentinel"}} {
		req, _ := json.Marshal(map[string]string{"source": "stored", "provider_id": "p", "model_id": "first", "key_id": item.key})
		runtime, err := storedruntime.Resolve(dir, req)
		if err != nil || !strings.Contains(string(runtime), item.value) ||
			!strings.Contains(string(runtime), `"maxTokens":8192`) {
			t.Fatal("imported runtime did not resolve encrypted key and settings", err)
		}
	}
	if err := s.ImportMobile("provider", raw); !errors.Is(err, catalog.ErrConflict) {
		t.Fatal("duplicate must fail", err)
	}
	after, err := s.Get()
	if err != nil || after.Manifest.Revision != bundle.Manifest.Revision {
		t.Fatal("duplicate changed catalog", err)
	}
}

func TestMobileMCPImportPreservesCredentialAndInventory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := catalog.New(dir)
	raw := json.RawMessage(`{"id":"m","name":"Tools","protocol":"streamable_http","url":"https://example.invalid/mcp",
        "auth":{"method":"CUSTOM_HEADER","key":"X-Api-Key","value":"mcp-credential-sentinel"},"cachedTools":[]}`)
	if err := s.ImportMobile("mcp", raw); err != nil {
		t.Fatal(err)
	}
	list, err := s.MCPInventory()
	if err != nil || len(list) != 1 || list[0].ID != "m" {
		t.Fatal("inventory missing", err)
	}
	bundle, err := s.Get()
	if err != nil || strings.Contains(bundle.Files["mcp.yaml"], "mcp-credential-sentinel") {
		t.Fatal("plaintext credential", err)
	}
	runtime, err := storedruntime.ResolveMCP(dir, json.RawMessage(`{"source":"stored","toolGrants":{}}`),
		json.RawMessage(`{"id":"chat"}`), "m")
	if err != nil || !strings.Contains(string(runtime), "mcp-credential-sentinel") {
		t.Fatal("encrypted credential unavailable", err)
	}
	if err := s.ImportMobile("mcp", json.RawMessage(`{"id":"oauth","name":"OAuth","auth":{"method":"OAUTH2"}}`)); !errors.Is(err, catalog.ErrImportUnsupported) {
		t.Fatal("unportable auth should fail explicitly", err)
	}
}

func TestMobileMCPImportKeepsPrivateConnectionFieldsEncrypted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := catalog.New(dir)
	raw := json.RawMessage(`{"id":"m","name":"Tools","url":"https://example.invalid/mcp?tenant=private-query-sentinel","headers":{"X-Secret":"private-header-sentinel"},"auth":{"method":"QUERY_PARAM","key":"token","value":"private-token-sentinel"}}`)
	if err := s.ImportMobile("mcp", raw); err != nil {
		t.Fatal(err)
	}
	bundle, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := storedruntime.ResolveMCP(dir, json.RawMessage(`{"source":"stored","toolGrants":{}}`), json.RawMessage(`{"id":"chat"}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-query-sentinel", "private-header-sentinel", "private-token-sentinel"} {
		if strings.Contains(bundle.Files["mcp.yaml"], secret) || !strings.Contains(string(runtime), secret) {
			t.Fatal("private connection field not protected/preserved")
		}
	}
}
