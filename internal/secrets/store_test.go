package secrets

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptedStorageAndKeyLoss(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("credential-sentinel-897413")
	tool := []byte(`{"id":"t","name":"tool","url":"https://example.invalid","parameters":{"type":"object","properties":{}},"headers":{"Authorization":"tool-sentinel-675433"}}`)
	if err = s.Put("provider", "p", "one", "Primary", "api_key", secret); err != nil {
		t.Fatal(err)
	}
	if err = s.PutTool("t", tool); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("provider", "p", "one")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("get: %s %v", got, err)
	}
	got, err = s.GetTool("t")
	if err != nil || !bytes.Equal(got, tool) {
		t.Fatalf("tool: %s %v", got, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, e := os.ReadFile(filepath.Join(dir, "data.sqlite"+suffix))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			t.Fatal(e)
		}
		if bytes.Contains(data, secret) || bytes.Contains(data, []byte("tool-sentinel-675433")) {
			t.Fatal("plaintext in sqlite")
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get("provider", "p", "one"); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete("provider", "p", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get("provider", "p", "one"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delete: %v", err)
	}
	s.Close()
	if err = os.Remove(filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err = os.WriteFile(filepath.Join(dir, "master.key"), bytes.Repeat([]byte{7}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestReadOnlyDoesNotCreateFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put("provider", "p", "main", "", "api_key", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := readOnly.Get("provider", "p", "main")
	if err != nil || string(value) != "secret" {
		t.Fatalf("read: %s %v", value, err)
	}
	if err = readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(before) != len(after) {
		t.Fatalf("files changed: %v %v %v", before, after, err)
	}
}
