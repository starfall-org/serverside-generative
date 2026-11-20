package configdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelocatePreservesDataAndIdentity(t *testing.T) {
	base := t.TempDir()
	paths := map[string]string{
		"serverside/providers.yaml":          "catalog",
		"serverside/chats/chat.json":         "chat",
		"serverside/secret.db":               "database",
		"serverside-generative/identity.key": "original TLS key",
		"serverside-generative/clients.json": "paired identities",
	}
	for rel, value := range paths {
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := relocate(base); err != nil {
		t.Fatal(err)
	}
	for rel, value := range paths {
		var dst string
		if filepath.Dir(rel) == "serverside-generative" {
			dst = filepath.Join(base, "generative", "identity", filepath.Base(rel))
		} else {
			relative, _ := filepath.Rel("serverside", rel)
			dst = filepath.Join(base, "generative", relative)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != value {
			t.Fatalf("%s: %q %v", dst, got, err)
		}
		if _, err := os.Lstat(filepath.Join(base, rel)); !os.IsNotExist(err) {
			t.Fatalf("old path remains: %s %v", rel, err)
		}
	}
	if err := relocate(base); err != nil {
		t.Fatal(err)
	}
}
func TestRelocateRejectsRegisteredServiceAndConflictingDestination(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "serverside-generative")
	service := filepath.Join(legacy, "service")
	if err := os.MkdirAll(service, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(service, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := relocate(base); err == nil {
		t.Fatal("moved registered daemon")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "generative")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := relocate(base); err == nil {
		t.Fatal("overwrote destination")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal(err)
	}
}
