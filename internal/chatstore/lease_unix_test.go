//go:build unix

package chatstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestJobLeaseExclusionAndUnsafeFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := New(dir)
	raw := json.RawMessage(`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.TryJobLease("c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(dir).TryJobLease("c"); !errors.Is(err, ErrJobBusy) {
		unlock()
		t.Fatalf("second lease: %v", err)
	}
	unlock()
	unlock, err = New(dir).TryJobLease("c")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	path := filepath.Join(dir, ".job-leases", filename("c")+".lock")
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0600 {
		t.Fatalf("lease file: %v %v", info, err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err = os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if unlock, err = store.TryJobLease("c"); err == nil {
		unlock()
		t.Fatal("accepted symlink lease")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("target changed: %q %v", data, err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(target, path); err != nil {
		t.Fatal(err)
	}
	if unlock, err = store.TryJobLease("c"); err == nil {
		unlock()
		t.Fatal("accepted hard-linked lease")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	leaseDir := filepath.Join(dir, ".job-leases")
	if err = os.Remove(leaseDir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, leaseDir); err != nil {
		t.Fatal(err)
	}
	if unlock, err = store.TryJobLease("c"); err == nil {
		unlock()
		t.Fatal("accepted symlink lease directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrote outside lease directory: %v %v", entries, err)
	}
}
