package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/catalog"
)

func TestProviderGroupCommandsRoundTripAndCAS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := run([]string{"config", "init", "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"provider", "add", "--id", "p", "--name", "Provider", "--type", "openai", "--data-dir", dir, "--if-revision", "1"}); err != nil {
		t.Fatal(err)
	}
	group := []string{"provider", "group", "add", "--id", "research", "--name", "Research", "--sort-order", "8", "--data-dir", dir, "--if-revision", "2"}
	if err := run(append(append([]string{}, group...), "--dry-run")); err != nil {
		t.Fatal(err)
	}
	got, err := catalog.New(dir).Get()
	if err != nil || got.Manifest.Revision != 2 {
		t.Fatalf("dry-run committed: %+v %v", got.Manifest, err)
	}
	if err := run(group); err != nil {
		t.Fatal(err)
	}
	if err := run(group); !errors.Is(err, catalog.ErrConflict) {
		t.Fatalf("stale add succeeded: %v", err)
	}
	if err := run([]string{"provider", "group", "assign", "--provider-id", "p", "--group-id", "missing", "--data-dir", dir, "--if-revision", "3"}); err == nil {
		t.Fatal("accepted nonexistent group")
	}
	if err := run([]string{"provider", "group", "assign", "--provider-id", "p", "--group-id", "research", "--data-dir", dir, "--if-revision", "3"}); err != nil {
		t.Fatal(err)
	}
	got, err = catalog.New(dir).Get()
	if err != nil || got.Manifest.Revision != 4 || !strings.Contains(got.Files["providers.yaml"], "groupId: research") ||
		!strings.Contains(got.Files["providers.yaml"], "sortOrder: 8") {
		t.Fatalf("wrong group fields: %v %s", err, got.Files["providers.yaml"])
	}
	if err := run([]string{"provider", "group", "delete", "--id", "research", "--data-dir", dir, "--if-revision", "4"}); !errors.Is(err, catalog.ErrConflict) {
		t.Fatalf("deleted assigned group: %v", err)
	}
	if err := run([]string{"provider", "group", "clear", "--provider-id", "p", "--data-dir", dir, "--if-revision", "4"}); err != nil {
		t.Fatal(err)
	}
	got, err = catalog.New(dir).Get()
	if err != nil || got.Manifest.Revision != 5 || !strings.Contains(got.Files["providers.yaml"], "groupId: null") {
		t.Fatalf("did not preserve null ungrouping: %v %s", err, got.Files["providers.yaml"])
	}
	if err := run([]string{"provider", "group", "delete", "--id", "research", "--data-dir", dir, "--if-revision", "5"}); err != nil {
		t.Fatal(err)
	}
	got, err = catalog.New(dir).Get()
	if err != nil || got.Manifest.Revision != 6 || strings.Contains(got.Files["providers.yaml"], "research") {
		t.Fatalf("group deletion failed: %v %s", err, got.Files["providers.yaml"])
	}
}

func TestProviderGroupEditRevisionAndValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := run([]string{"config", "init", "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"provider", "group", "add", "--id", "g", "--name", "Original", "--sort-order", "8", "--data-dir", dir, "--if-revision", "1"}); err != nil {
		t.Fatal(err)
	}
	edit := []string{"provider", "group", "edit", "--id", "g", "--name", "Renamed", "--sort-order", "0", "--data-dir", dir, "--if-revision", "2"}
	if err := run(append(append([]string{}, edit...), "--dry-run")); err != nil {
		t.Fatal(err)
	}
	before, err := catalog.New(dir).Get()
	if err != nil || before.Manifest.Revision != 2 || !strings.Contains(before.Files["providers.yaml"], "Original") {
		t.Fatalf("dry run changed catalog: %+v %v", before.Manifest, err)
	}
	if err := run(edit); err != nil {
		t.Fatal(err)
	}
	if err := run(edit); !errors.Is(err, catalog.ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	got, err := catalog.New(dir).Get()
	if err != nil || got.Manifest.Revision != 3 || !strings.Contains(got.Files["providers.yaml"], "Renamed") || !strings.Contains(got.Files["providers.yaml"], "sortOrder: 0") {
		t.Fatalf("edit: %+v %v", got, err)
	}
	if err := run([]string{"provider", "group", "edit", "--id", "g", "--name", " ", "--data-dir", dir, "--if-revision", "3"}); err == nil {
		t.Fatal("accepted empty group name")
	}
	if err := run([]string{"provider", "group", "edit", "--id", "missing", "--name", "x", "--data-dir", dir, "--if-revision", "3"}); err == nil {
		t.Fatal("accepted missing group")
	}
	if err := run([]string{"provider", "group", "edit", "--id", "g", "--data-dir", dir, "--if-revision", "3"}); err == nil {
		t.Fatal("accepted edit without changes")
	}
}
