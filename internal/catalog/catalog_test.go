package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func docs(rev string) map[string]string {
	return map[string]string{"providers.yaml": "schema_version: 1\nrevision: " + rev + "\ngroups: []\nproviders: []\n", "models.yaml": "schema_version: 1\nrevision: " + rev + "\nproviders: {}\n", "mcp.yaml": "schema_version: 1\nrevision: " + rev + "\nservers: []\n", "profiles.yaml": "schema_version: 1\nrevision: " + rev + "\nchatProfiles: []\n"}
}
func TestCatalogSaveCASAndUnsafeInput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	b, err := s.Save(0, docs("1"))
	if err != nil || b.Manifest.Revision != 1 {
		t.Fatalf("save: %+v %v", b, err)
	}
	if _, err = s.Save(0, docs("1")); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	d := docs("2")
	d["providers.yaml"] += "apiKey: credential-sentinel\n"
	if _, err = s.Save(1, d); !errors.Is(err, ErrInvalid) {
		t.Fatalf("secret: %v", err)
	}
	d = docs("2")
	d["mcp.yaml"] += "servers: []\n"
	if _, err = s.Save(1, d); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate: %v", err)
	}
	if b, err = s.Get(); err != nil || b.Manifest.Revision != 1 {
		t.Fatalf("get: %+v %v", b, err)
	}
	path := filepath.Join(dir, "providers.yaml")
	raw, _ := os.ReadFile(path)
	if err = os.WriteFile(path, append(raw, []byte("# external\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(1, docs("2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual edit: %v", err)
	}
}
func TestRecoverPublishedTransaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	if _, err := s.Save(0, docs("1")); err != nil {
		t.Fatal(err)
	}
	tx := filepath.Join(dir, ".catalog-transaction")
	if err := os.Mkdir(tx, 0700); err != nil {
		t.Fatal(err)
	}
	next := docs("2")
	m := Manifest{SchemaVersion: 1, Revision: 2, Checksums: map[string]string{}}
	for _, name := range files {
		m.Checksums[name] = digest([]byte(next[name]))
		if err := writeFile(filepath.Join(tx, name), []byte(next[name])); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(m)
	if err := writeFile(filepath.Join(tx, "catalog-manifest.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(tx, "READY"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get()
	if err != nil || got.Manifest.Revision != 2 || !strings.Contains(got.Files["models.yaml"], "revision: 2") {
		t.Fatalf("recover: %+v %v", got, err)
	}
}

func TestManagementPreservesSelectionAndRevisions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	if _, err := s.Save(0, docs("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(1, true, func(d map[string]*yaml.Node) error { return AddProvider(d, "p", "Provider", "openai", "") }); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(); err != nil || got.Manifest.Revision != 1 {
		t.Fatalf("dry run changed catalog: %+v %v", got, err)
	}
	if _, err := s.Update(1, false, func(d map[string]*yaml.Node) error { return AddProvider(d, "p", "Provider", "openai", "") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(2, false, func(d map[string]*yaml.Node) error { return AddModel(d, "p", "org/model", "Model", "TEXT_GENERATION") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(3, false, func(d map[string]*yaml.Node) error { return SelectModels(d, "p", "explicit", []string{}) }); err != nil {
		t.Fatal(err)
	}
	b, err := s.Get()
	if err != nil || !strings.Contains(b.Files["models.yaml"], "modelIds: []") || !strings.Contains(b.Files["models.yaml"], "org/model") {
		t.Fatalf("explicit empty: %s %v", b.Files["models.yaml"], err)
	}
	if _, err = s.Update(4, false, func(d map[string]*yaml.Node) error { return SelectModels(d, "p", "discovery", nil) }); err != nil {
		t.Fatal(err)
	}
	b, err = s.Get()
	if err != nil || !strings.Contains(b.Files["models.yaml"], "modelIds: null") {
		t.Fatalf("discovery: %s %v", b.Files["models.yaml"], err)
	}
	if _, err = s.Update(4, false, func(d map[string]*yaml.Node) error { return HideProvider(d, "p", true) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
}

func TestMediaProfileEditingPreservesOpaqueFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	if _, err := s.Save(0, docs("1")); err != nil {
		t.Fatal(err)
	}
	var profile yaml.Node
	raw := `{"id":"music-1","name":"Music","mode":"custom","requiresModel":false,"chain":{"steps":[{"endpoint":"https://example.invalid/generate","method":"POST"}],"finalResponse":{"type":"audio"}},"future":{"nullable":null}}`
	if err := yaml.Unmarshal([]byte(raw), &profile); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(1, false, func(d map[string]*yaml.Node) error { return AddMediaProfile(d, "music", profile.Content[0]) }); err != nil {
		t.Fatal(err)
	}
	b, err := s.Get()
	if err != nil || !strings.Contains(b.Files["profiles.yaml"], "music-1") || !strings.Contains(b.Files["profiles.yaml"], `"nullable": null`) {
		t.Fatalf("profile: %s %v", b.Files["profiles.yaml"], err)
	}
}
