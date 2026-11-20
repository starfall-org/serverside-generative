package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/catalog"
)

func TestCatalogExportImportDiffAndCAS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	docs, err := catalog.Empty()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}

	export := filepath.Join(t.TempDir(), "providers.yaml")
	if err = run([]string{"config", "export", "--kind", "providers", "--data-dir", dir, "--out", export}); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(export)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("export should be private: %v %v", stat, err)
	}
	raw, err := os.ReadFile(export)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "groups: []", "groups:\n  - id: group-a\n    name: Research", 1)
	if edited == string(raw) {
		t.Fatal("no change made")
	}
	if err = os.WriteFile(export, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"config", "diff", "--kind", "providers", "--file", export, "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"config", "import", "--kind", "providers", "--file", export, "--data-dir", dir, "--if-revision", "1", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := catalog.New(dir).Get()
	if err != nil || bundle.Manifest.Revision != 1 {
		t.Fatalf("dry-run published: %v %+v", err, bundle.Manifest)
	}
	if err = run([]string{"config", "import", "--kind", "providers", "--file", export, "--data-dir", dir, "--if-revision", "1"}); err != nil {
		t.Fatal(err)
	}
	bundle, err = catalog.New(dir).Get()
	if err != nil || bundle.Manifest.Revision != 2 || !strings.Contains(bundle.Files["providers.yaml"], "group-a") {
		t.Fatalf("catalog not updated: %v %+v", err, bundle.Manifest)
	}
	if err = run([]string{"config", "import", "--kind", "providers", "--file", export, "--data-dir", dir, "--if-revision", "1"}); err == nil {
		t.Fatal("stale revision accepted")
	}
	leak := filepath.Join(t.TempDir(), "leak.yaml")
	unsafe := strings.Replace(edited, "name: Research", "name: Research\n    api_key: plaintext", 1)
	if err = os.WriteFile(leak, []byte(unsafe), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"config", "import", "--kind", "providers", "--file", leak, "--data-dir", dir, "--if-revision", "2", "--dry-run"}); err == nil {
		t.Fatal("raw credential accepted in YAML")
	}
	bundle, err = catalog.New(dir).Get()
	if err != nil || bundle.Manifest.Revision != 2 {
		t.Fatal("rejected import modified catalog")
	}
}

func TestManageMenuValidatesAndQuits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	docs, err := catalog.Empty()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = runManage([]string{"--data-dir", dir}, strings.NewReader("5\n6\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Catalog valid at revision 1") || !strings.Contains(output.String(), "Generative server management") {
		t.Fatalf("missing management flow: %s", output.String())
	}
}

func TestCatalogFileDiffDoesNotDisplayValues(t *testing.T) {
	pair := []byte("schema_version: 1\nrevision: 1\nproviders: [{id: p, name: Old}]\n")
	next := []byte("schema_version: 1\nrevision: 1\nproviders: [{id: p, name: New}]\n")
	paths, err := changedCatalogPaths(pair, next)
	if err != nil || len(paths) == 0 {
		t.Fatalf("cannot diff: %v", err)
	}
	for _, p := range paths {
		if strings.Contains(p, "Old") || strings.Contains(p, "New") {
			t.Fatal("diff contained literal values")
		}
	}
}

func TestCatalogDiffNeverPrintsDynamicMapKeys(t *testing.T) {
	before := []byte("schema_version: 1\nrevision: 1\nmodels:\n  provider-a: {modelIds: []}\n")
	after := []byte("schema_version: 1\nrevision: 1\nmodels:\n  super-secret-key-ABC123: {modelIds: []}\n")
	paths, err := changedCatalogPaths(before, after)
	if err != nil || len(paths) == 0 {
		t.Fatalf("missing changes: %v", err)
	}
	for _, path := range paths {
		if strings.Contains(path, "super-secret-key") || strings.Contains(path, "provider-a") {
			t.Fatalf("dynamic map key leaked into diff: %q", path)
		}
	}
}

func TestServeRefusesLegacyNewPairingByDefault(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "--human", "--http-listen", "off"},
		{"serve", "--password", "temporary-test-password-1234"},
	} {
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "--allow-legacy-pairing") {
			t.Fatalf("serve must reject deprecated pairing without explicit migration: %v (%v)", args, err)
		}
	}
}
