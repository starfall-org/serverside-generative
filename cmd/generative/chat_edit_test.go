package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
)

func chatEditor(t *testing.T, code string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+code+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncedChatEditRevisionSafetyAndFidelity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX test editor")
	}
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	original := json.RawMessage("{\"id\":\"c\",\"title\":\"before\",\"created_at\":1,\"updated_at\":2,\"messages\":[],\"future\":null}")
	snap, err := store.Save("c", "owner", "create", 0, original)
	if err != nil || snap.Revision != 1 {
		t.Fatalf("create: %+v %v", snap, err)
	}
	common := []string{"chat", "edit", "--data-dir", dir, "--client-id", "owner", "--id", "c", "--if-revision", "1"}
	noop := chatEditor(t, "exit 0")
	if err = run(append(append([]string{}, common...), "--editor", noop)); err != nil {
		t.Fatalf("no-op editing should be safe without --yes: %v", err)
	}
	edited := "{\"id\":\"c\",\"title\":\"after\",\"created_at\":1,\"updated_at\":2,\"messages\":[{\"id\":\"msg\",\"role\":\"model\",\"versions\":[{\"content\":\"ok\",\"future\":null,\"toolActivity\":[{\"id\":\"call\",\"name\":\"find\",\"arguments\":\"{\\\"x\\\":1}\",\"response\":\"{}\"}]}],\"active_version_index\":0}],\"future\":null}"
	script := chatEditor(t, "cat > \"$1\" <<'CHAT_EOF'\n"+edited+"\nCHAT_EOF")
	if err = run(append(append([]string{}, common...), "--editor", script, "--dry-run")); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if err = run(append(append([]string{}, common...), "--editor", script)); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("mutation without approval accepted: %v", err)
	}
	after, err := store.Get("owner", "c")
	if err != nil || after.Revision != 1 || !chatstore.Equal(after.Conversation, original) {
		t.Fatalf("dry-run/unapproved edit wrote data: %+v %v", after, err)
	}
	if err = run(append(append([]string{}, common...), "--editor", script, "--yes")); err != nil {
		t.Fatalf("edit: %v", err)
	}
	after, err = chatstore.New(dir).Get("owner", "c")
	if err != nil || after.Revision != 2 || !chatstore.Equal(after.Conversation, []byte(edited)) {
		t.Fatalf("edit lost fields or revision: %+v %v", after, err)
	}
	if err = run(append(append([]string{}, common...), "--editor", script, "--yes")); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("stale editor request accepted: %v", err)
	}
	if err = run(append(append([]string{}, common...), "--client-id", "other", "--editor", script, "--yes")); err == nil {
		t.Fatal("foreign owner could edit a chat")
	}
}

func TestSyncedChatEditRejectsInvalidAndUnsafeEditorFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX test editor")
	}
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	original := json.RawMessage("{\"id\":\"c\",\"title\":\"before\",\"created_at\":1,\"updated_at\":2,\"messages\":[]}")
	if _, err := store.Save("c", "owner", "create", 0, original); err != nil {
		t.Fatal(err)
	}
	base := []string{"chat", "edit", "--data-dir", dir, "--client-id", "owner", "--id", "c", "--if-revision", "1", "--yes"}
	for name, script := range map[string]string{
		"malformed":            "echo '{' > \"$1\"",
		"invalid-conversation": "echo '{\"id\":\"c\",\"title\":\"bad\",\"created_at\":1,\"updated_at\":2,\"messages\":null}' > \"$1\"",
		"too-public":           "chmod 666 \"$1\"",
		"symlink":              "rm \"$1\"; ln -s /etc/passwd \"$1\"",
		"editor-fails":         "exit 1",
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(append(append([]string{}, base...), "--editor", chatEditor(t, script))); err == nil {
				t.Fatalf("unsafe edit accepted: %s", name)
			}
			got, err := store.Get("owner", "c")
			if err != nil || got.Revision != 1 || !chatstore.Equal(got.Conversation, original) {
				t.Fatalf("invalid editor modified stored chat: %+v %v", got, err)
			}
		})
	}
}

func TestSyncedChatEditRejectsSameRevisionHandEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX test editor")
	}
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	original := json.RawMessage(`{"id":"c","title":"before","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "create", 0, original); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("c"))
	path := filepath.Join(dir, "chats", fmt.Sprintf("%x.json", hash))
	t.Setenv("CHAT_EXTERNAL_PATH", path)
	editor := chatEditor(t, `sed -i 's/before/external/g' "$CHAT_EXTERNAL_PATH"`+"\n"+`sed -i 's/before/interactive/g' "$1"`)
	err := run([]string{"chat", "edit", "--data-dir", dir, "--client-id", "owner", "--id", "c", "--if-revision", "1", "--editor", editor, "--yes"})
	if !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("concurrent hand edit was overwritten: %v", err)
	}
	snap, err := store.Get("owner", "c")
	if err != nil || !strings.Contains(string(snap.Conversation), "external") || snap.Revision != 1 {
		t.Fatalf("external edit lost: %+v %v", snap, err)
	}
}

func TestSyncedChatEditRespectsActiveJobLease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX test editor")
	}
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	raw := json.RawMessage(`{"id":"c","title":"before","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.TryJobLease("c")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	editor := chatEditor(t, `sed -i 's/before/after/g' "$1"`)
	err = run([]string{"chat", "edit", "--data-dir", dir, "--client-id", "owner", "--id", "c", "--if-revision", "1", "--editor", editor, "--yes"})
	if !errors.Is(err, chatstore.ErrJobBusy) {
		t.Fatalf("edit bypassed job lease: %v", err)
	}
}
