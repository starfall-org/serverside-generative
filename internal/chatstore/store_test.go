package chatstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveRestartCASAndReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	raw := json.RawMessage(`{"id":"../chat","title":"test","created_at":1,"updated_at":2,"messages":[{"id":"m","role":"model","versions":[{"toolActivity":[{"id":"t","name":"tool","arguments":"{\"a\":1}"}],"future":null}],"active_version_index":0}],"summary":null,"unknown":[]}`)
	first, err := s.Save("../chat", "client", "op", 0, raw)
	if err != nil || first.Revision != 1 {
		t.Fatalf("save: %+v %v", first, err)
	}
	got, err := New(dir).Get("client", "../chat")
	if err != nil || !jsonEqual(got.Conversation, raw) {
		t.Fatalf("restart: %+v %v", got, err)
	}
	if _, err = s.Save("../chat", "client", "op", 0, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save("../chat", "client", "other", 0, raw); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	if _, err = s.Save("../chat", "client", "op", 0, json.RawMessage(`{"id":"../chat","title":"test","created_at":1,"updated_at":2,"messages":[]}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected replay conflict: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(dir, "chats"))
	if err != nil || len(files) != 1 || files[0].Name() != filename("../chat") {
		t.Fatalf("files: %v %v", files, err)
	}
	info, err := files[0].Info()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
}

func TestConcurrentCAS(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "data"))
	raw := json.RawMessage(`{"id":"c","title":"test","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := s.Save("c", "a", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _, err := s.Save("c", "a", id, 1, raw); errs <- err }(id)
	}
	wg.Wait()
	close(errs)
	ok, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", ok, conflicts)
	}
}

func TestPreviousSnapshotIsBoundedAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	for i := 0; i < 3; i++ {
		raw := json.RawMessage(fmt.Sprintf(`{"id":"c","title":"revision %d","created_at":1,"updated_at":2,"messages":[]}`, i+1))
		if _, err := s.Save("c", "owner", fmt.Sprintf("op%d", i), uint64(i), raw); err != nil {
			t.Fatal(err)
		}
	}
	backupDir := filepath.Join(dir, ".backups", "previous")
	entries, err := os.ReadDir(backupDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups: %v %v", entries, err)
	}
	path := filepath.Join(backupDir, filename("c"))
	data, err := readBoundedRegular(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var previous Snapshot
	if err = json.Unmarshal(data, &previous); err != nil || previous.Revision != 2 || !bytes.Contains(previous.Conversation, []byte("revision 2")) {
		t.Fatalf("previous: %+v %v", previous, err)
	}
	view, err := s.Previous("owner", "c")
	if err != nil || view.Revision != 2 {
		t.Fatalf("inspect: %+v %v", view, err)
	}
	if _, err := s.Previous("other", "c"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("owner isolation: %v", err)
	}
	if _, err := s.Delete("owner", "c", "delete", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Previous("owner", "c"); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted backup visible: %v", err)
	}
}

func TestInvalidDoesNotWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	for _, raw := range []string{`{}`, `{"id":"other","messages":[]}`, `{"id":"c","title":"test","created_at":1,"updated_at":2,"messages":null}`} {
		if _, err := s.Save("c", "a", "op", 0, json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "chats")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected disk write: %v", err)
	}
}

func TestCorruptSnapshotIsNotOverwritten(t *testing.T) {
	for _, corrupt := range []string{
		`{"schema_version":`,
		`{"schema_version":1,"owner_id":"owner","revision":1,"conversation":{"id":"c","messages":null}}`,
	} {
		t.Run(fmt.Sprintf("bytes_%d", len(corrupt)), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			s := New(dir)
			raw := json.RawMessage(`{"id":"c","title":"private","created_at":1,"updated_at":2,"messages":[]}`)
			if _, err := s.Save("c", "owner", "create", 0, raw); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "chats", filename("c"))
			if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get("owner", "c"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("corrupt read: %v", err)
			}
			if _, err := s.Save("c", "owner", "edit", 1, raw); !errors.Is(err, ErrInvalid) {
				t.Fatalf("corrupt save: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != corrupt {
				t.Fatalf("corrupt evidence changed: %q %v", after, err)
			}
		})
	}
}

func TestUnsafeChatPermissionsRejectReadAndSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := New(dir)
	raw := json.RawMessage(`{"id":"c","title":"private","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := s.Save("c", "owner", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chats", filename("c"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("owner", "c"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe read: %v", err)
	}
	if _, _, err := s.List("owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe list: %v", err)
	}
	if _, err := s.Save("c", "owner", "edit", 1, raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe save: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("file changed: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("owner", "c"); err != nil {
		t.Fatalf("private read: %v", err)
	}
}

func TestConcurrentStoresCAS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	raw := json.RawMessage(`{"id":"c","title":"test","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := New(dir).Save("c", "a", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := New(dir).Save("c", "a", fmt.Sprintf("op%d", i), 1, raw)
			errorsOut <- err
		}(i)
	}
	wg.Wait()
	close(errorsOut)
	ok, conflicts := 0, 0
	for err := range errorsOut {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflicts != 11 {
		t.Fatalf("success=%d conflicts=%d", ok, conflicts)
	}
}

func TestReloadRejectsSameRevisionExternalChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := New(dir)
	raw := json.RawMessage(`{"id":"c","title":"original","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	inspected, err := store.Get("owner", "c")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chats", filename("c"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("original"), []byte("hand edit"), 1)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Reload("c", "owner", "reload", inspected); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reload: %v", err)
	}
	current, err := store.Get("owner", "c")
	if err != nil || !bytes.Contains(current.Conversation, []byte("hand edit")) {
		t.Fatalf("edit lost: %+v %v", current, err)
	}
	reloaded, err := store.Reload("c", "owner", "reload", current)
	if err != nil || reloaded.Revision != 2 || !bytes.Contains(reloaded.Conversation, []byte("hand edit")) {
		t.Fatalf("reload: %+v %v", reloaded, err)
	}
	backups, err := os.ReadDir(filepath.Join(dir, ".backups", "chats"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup entries: %v %v", backups, err)
	}
	backupPath := filepath.Join(dir, ".backups", "chats", backups[0].Name())
	backup, err := os.ReadFile(backupPath)
	if err != nil || !bytes.Equal(backup, b) {
		t.Fatalf("backup differs from hand edit: %v", err)
	}
	info, err := os.Stat(backupPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %v %v", info, err)
	}
}

func TestReloadRejectsSymlinkedBackupDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := New(dir)
	raw := json.RawMessage(`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "open", 0, raw); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Get("owner", "c")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".backups")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reload("c", "owner", "reload", snap); err == nil {
		t.Fatal("accepted symlinked backup directory")
	}
	after, err := store.Get("owner", "c")
	if err != nil || after.Revision != 1 || !Equal(after.Conversation, raw) {
		t.Fatalf("reload modified snapshot: %+v %v", after, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrote outside backup directory: %v %v", entries, err)
	}
}

func TestBackupReadRejectsOversizedOrSymlinkedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(maxSnapshotBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = readBoundedRegular(path, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized file accepted: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = readBoundedRegular(link, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("symlink accepted: %v", err)
	}
}
