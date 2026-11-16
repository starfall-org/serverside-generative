package storedruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/secrets"
)

func TestStoredResolutionReadOnlyAndNoFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	docs := map[string]string{"providers.yaml": "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: P\n    type: openai\n    baseUrl: https://example.invalid\n    auth:\n      method: bearer_token\n      credential_ref: provider/p/main\n    config: {}\n", "models.yaml": "schema_version: 1\nrevision: 1\nproviders:\n  p:\n    modelIds: [org/model]\n    modelConfigs: {}\n    catalog: []\n", "mcp.yaml": "schema_version: 1\nrevision: 1\nservers: []\n", "profiles.yaml": "schema_version: 1\nrevision: 1\nchatProfiles: []\n"}
	if _, err := catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	s, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put("provider", "p", "main", "", "api_key", []byte("stored-secret-sentinel")); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := map[string][]byte{}
	for _, entry := range before {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		snapshots[entry.Name()] = data
	}
	out, err := Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"org/model"}`))
	if err != nil || !bytes.Contains(out, []byte("stored-secret-sentinel")) {
		t.Fatalf("resolution failed: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(before) != len(after) {
		t.Fatalf("read created files: %v", err)
	}
	for name, old := range snapshots {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(data, old) {
			t.Fatalf("read modified %s: %v", name, err)
		}
	}
	if _, err = Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"other","model_id":"org/model"}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("provider fallback: %v", err)
	}
	if _, err = Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"other"}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("model fallback: %v", err)
	}
	for _, name := range []string{"providers.yaml", "models.yaml", "mcp.yaml", "profiles.yaml", "data.sqlite"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || bytes.Contains(data, []byte("stored-secret-sentinel")) {
			t.Fatalf("plaintext in %s: %v", name, err)
		}
	}
}

func TestMultiKeyRequiresExplicitSelection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	docs := map[string]string{"providers.yaml": "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: P\n    type: openai\n    auth: {method: bearer_token}\n    config:\n      multipleApiKeys: true\n      apiKeys:\n        - {id: main, label: Main, credential_ref: provider/p/main}\n", "models.yaml": "schema_version: 1\nrevision: 1\nproviders:\n  p: {modelIds: null, modelConfigs: {}, catalog: []}\n", "mcp.yaml": "schema_version: 1\nrevision: 1\nservers: []\n", "profiles.yaml": "schema_version: 1\nrevision: 1\nchatProfiles: []\n"}
	if _, err := catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	s, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put("provider", "p", "main", "Main", "api_key", []byte("value")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"m"}`)); !errors.Is(err, ErrAuth) {
		t.Fatalf("implicit selection: %v", err)
	}
	got, err := Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"m","key_id":"main"}`))
	if err != nil || !bytes.Contains(got, []byte(`"value":"value"`)) {
		t.Fatalf("explicit selection: %v", err)
	}
}

func TestStoredProviderQueryAndHeaderAuthentication(t *testing.T) {
	for _, method := range []string{"query_param", "custom_header"} {
		t.Run(method, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			docs, err := catalog.Empty()
			if err != nil {
				t.Fatal(err)
			}
			docs["providers.yaml"] = "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: P\n    type: openai\n    baseUrl: https://example.invalid\n    auth: {method: " + method + ", parameter_name: X_Key, credential_ref: provider/p/main}\n    config: {}\n"
			docs["models.yaml"] = "schema_version: 1\nrevision: 1\nproviders:\n  p: {modelIds: null, modelConfigs: {}, catalog: []}\n"
			if _, err = catalog.New(dir).Save(0, docs); err != nil {
				t.Fatal(err)
			}
			store, err := secrets.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Put("provider", "p", "main", "", "api_key", []byte("provider-secret")); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"m"}`))
			if err != nil {
				t.Fatal(err)
			}
			var runtime struct {
				Provider struct {
					Auth struct {
						Method string `json:"method"`
						Key    string `json:"key"`
						Value  string `json:"value"`
					} `json:"auth"`
				} `json:"provider"`
			}
			if json.Unmarshal(result, &runtime) != nil || runtime.Provider.Auth.Method != method || runtime.Provider.Auth.Key != "X_Key" || runtime.Provider.Auth.Value != "provider-secret" {
				t.Fatal("stored auth was not resolved")
			}
		})
	}
}
