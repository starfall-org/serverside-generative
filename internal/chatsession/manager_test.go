package chatsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
)

var sample = json.RawMessage(`{"id":"c","title":"title","created_at":1,"updated_at":2,"messages":[]}`)

func TestConversationAlwaysPersistsAndReturnsToClient(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	store := chatstore.New(dir)
	m := New(store)
	v, err := m.Open("owner", "c", sample, nil)
	if err != nil || v.Revision != 1 || !chatstore.Equal(v.Conversation, sample) {
		t.Fatalf("open: %+v %v", v, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "chats")); err != nil {
		t.Fatalf("missing durable chat: %v", err)
	}
	if again, err := m.Open("owner", "c", nil, nil); err != nil || !chatstore.Equal(again.Conversation, sample) {
		t.Fatalf("reopen: %+v %v", again, err)
	}
	if _, err := m.Open("other", "c", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner allowed: %v", err)
	}
	if err := m.Close("owner", "c"); err != nil {
		t.Fatal(err)
	}
	m = New(chatstore.New(dir))
	resumed, err := m.Open("owner", "c", nil, nil)
	if err != nil || resumed.Revision != 1 || !chatstore.Equal(resumed.Conversation, sample) {
		t.Fatalf("restart recovery: %+v %v", resumed, err)
	}
	listed, _, err := m.List("owner", 0, 20)
	if err != nil || len(listed) != 1 || listed[0].ID != "c" {
		t.Fatalf("durable list: %+v %v", listed, err)
	}
}

func TestTransientProviderAndMCPSettingsNeverPersist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"x","auth":{"method":"bearer_token","value":"temporary-provider-secret"}},"mcp":[{"id":"s","url":"https://example.com","auth":{"token":"temporary-mcp-secret"}}]}`)
	v, err := m.Open("owner", "c", sample, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Configure("owner", "c", runtime); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(v.Conversation, []byte("temporary")) {
		t.Fatal("runtime config leaked to RPC response")
	}
	snap, err := chatstore.New(dir).Get("owner", "c")
	if err != nil {
		t.Fatal(err)
	}
	if !chatstore.Equal(snap.Conversation, sample) || bytes.Contains(snap.Conversation, []byte("temporary")) {
		t.Fatalf("runtime leaked to JSON: %s", snap.Conversation)
	}
	if err = m.Close("owner", "c"); err != nil {
		t.Fatal(err)
	}
	m = New(chatstore.New(dir))
	if v, err = m.Open("owner", "c", nil, nil); err != nil || !chatstore.Equal(v.Conversation, sample) {
		t.Fatalf("restart: %+v %v", v, err)
	}
	if err = m.Run("owner", "c", "new", "hello"); !errors.Is(err, ErrRuntime) {
		t.Fatalf("runtime survived restart: %v", err)
	}
}

func TestSavedEditsPersistAndEmitAuthoritativeConversation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	_, err := m.Open("owner", "c", sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	edited := json.RawMessage(`{"id":"c","title":"edited","created_at":1,"updated_at":3,"messages":[]}`)
	if _, err = m.Save("owner", "c", "update", 0, edited); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	v, err := m.Save("owner", "c", "update", 1, edited)
	if err != nil || v.Revision != 2 || !chatstore.Equal(v.Conversation, edited) {
		t.Fatalf("save: %+v %v", v, err)
	}
	events, err := m.Events("owner", "c", 0)
	if err != nil || len(events) != 1 || events[0].Kind != "conversation" {
		t.Fatalf("push event: %+v %v", events, err)
	}
	payload := events[0].Data.(map[string]any)["conversation"].(json.RawMessage)
	if !chatstore.Equal(payload, edited) {
		t.Fatalf("pushed wrong snapshot: %s", payload)
	}
	persisted, err := chatstore.New(dir).Get("owner", "c")
	if err != nil || persisted.Revision != 2 || !chatstore.Equal(persisted.Conversation, edited) {
		t.Fatalf("persisted: %+v %v", persisted, err)
	}
	if err = m.Close("owner", "c"); err != nil {
		t.Fatal(err)
	}
	resumed, err := New(chatstore.New(dir)).Open("owner", "c", nil, nil)
	if err != nil || !chatstore.Equal(resumed.Conversation, edited) {
		t.Fatalf("resumed: %+v %v", resumed, err)
	}
}

func TestCursorIsScopedToActiveSession(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	if _, err := m.Open("owner", "c", sample, nil); err != nil {
		t.Fatal(err)
	}
	first := json.RawMessage(`{"id":"c","title":"first","created_at":1,"updated_at":3,"messages":[]}`)
	if _, err := m.Save("owner", "c", "first", 1, first); err != nil {
		t.Fatal(err)
	}
	events, err := m.Events("owner", "c", 0)
	if err != nil || len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("first session events: %+v %v", events, err)
	}
	if err := m.Close("owner", "c"); err != nil {
		t.Fatal(err)
	}
	view, err := m.Open("owner", "c", nil, nil)
	if err != nil || view.Revision != 2 || !chatstore.Equal(view.Conversation, first) {
		t.Fatalf("reopened snapshot: %+v %v", view, err)
	}
	if _, err := m.Events("owner", "c", 1); !errors.Is(err, ErrGap) {
		t.Fatalf("future cursor: %v", err)
	}
	second := json.RawMessage(`{"id":"c","title":"second","created_at":1,"updated_at":4,"messages":[]}`)
	if _, err := m.Save("owner", "c", "second", 2, second); err != nil {
		t.Fatal(err)
	}
	// The old cursor now falls within the new sequence, so it cannot identify
	// the previous session or report the missed event.
	events, err = m.Events("owner", "c", 1)
	if err != nil || len(events) != 0 {
		t.Fatalf("reused cursor: %+v %v", events, err)
	}
	view, err = m.Get("owner", "c")
	if err != nil || view.Revision != 3 || !chatstore.Equal(view.Conversation, second) {
		t.Fatalf("snapshot recovery: %+v %v", view, err)
	}
}

func TestSessionTTLReleasesOnlyRAMNotStorage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	m.ttl = 20 * time.Millisecond
	if _, err := m.Open("owner", "c", sample, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := m.Get("owner", "c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("handle not released: %v", err)
	}
	v, err := New(chatstore.New(dir)).Open("owner", "c", nil, nil)
	if err != nil || !chatstore.Equal(v.Conversation, sample) {
		t.Fatalf("chat lost after TTL: %+v %v", v, err)
	}
}

func TestExplicitDeletionUsesDurableTombstone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	if _, err := m.Open("owner", "c", sample, nil); err != nil {
		t.Fatal(err)
	}
	old := uint64(0)
	if err := m.Delete("owner", "c", "delete", &old); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	current := uint64(1)
	if err := m.Delete("owner", "c", "delete", &current); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("owner", "c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("still open: %v", err)
	}
	records, _, err := m.List("owner", 0, 10)
	if err != nil || len(records) != 1 || records[0].DeletedAt == 0 {
		t.Fatalf("no tombstone: %+v %v", records, err)
	}
	if _, err := New(chatstore.New(dir)).Open("owner", "c", sample, nil); err == nil {
		t.Fatal("resurrected deleted conversation")
	}
}

func TestUnmodifiedExistingChatFileCannotBeOverwritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	if _, err := m.Open("owner", "c", sample, nil); err != nil {
		t.Fatal(err)
	}
	edited := json.RawMessage(`{"id":"c","title":"external edit","created_at":1,"updated_at":2,"messages":[]}`)
	// A separate CLI edit changes disk, so an active in-memory session must not overwrite it.
	if _, err := chatstore.New(dir).Save("c", "owner", "outside", 1, edited); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("owner", "c"); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("concurrent edit not detected: %v", err)
	}
	if _, err := m.Save("owner", "c", "update", 1, sample); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("overwrote edit: %v", err)
	}
}

func TestMobileFixturePersistsThroughRestart(t *testing.T) {
	raw, err := os.ReadFile("../compat/testdata/conversation-mobile.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	v, err := m.Open("owner", "conversation-1", raw, nil)
	if err != nil || !chatstore.Equal(raw, v.Conversation) {
		t.Fatalf("open fixture: %v", err)
	}
	if err = m.Close("owner", "conversation-1"); err != nil {
		t.Fatal(err)
	}
	v, err = New(chatstore.New(dir)).Open("owner", "conversation-1", nil, nil)
	if err != nil || !chatstore.Equal(raw, v.Conversation) {
		t.Fatalf("restart fixture: %+v %v", v, err)
	}
	if strings.Contains(string(v.Conversation), "source\x22:") {
		t.Fatal("runtime config mixed into conversation")
	}
}
