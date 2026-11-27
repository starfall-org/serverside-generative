package chatsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starfall-org/generative-server/internal/chatstore"
)

var sample = json.RawMessage(`{"id":"c","title":"title","created_at":1,"updated_at":2,"messages":[]}`)

func TestListProjectsMetadataAndOrdersByUpdate(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	for _, raw := range []string{
		`{"id":"a","title":"old","created_at":1,"updated_at":2,"messages":[]}`,
		`{"id":"b","title":"new","created_at":3,"updated_at":5,"provider_id":"p","model_id":"m","profile_id":"profile","reasoning_effort":"high","token_count":42,"messages":[{"id":"msg","role":"user","versions":[]}],"summary":{"id":"s","content":"summary","through_message_id":"msg"}}`,
		`{"id":"c","title":"tie","created_at":4,"updated_at":5,"messages":[]}`,
	} {
		var doc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Open("owner", doc.ID, json.RawMessage(raw), nil); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := m.List("owner", 0, 2)
	if err != nil || len(page) != 2 || next != 2 || page[0].ID != "b" || page[1].ID != "c" {
		t.Fatalf("first page: %+v next=%d err=%v", page, next, err)
	}
	item := page[0]
	if item.CreatedAt != 3 || item.ProviderID != "p" || item.ModelID != "m" || item.ProfileID == nil || *item.ProfileID != "profile" || item.ReasoningEffort == nil || *item.ReasoningEffort != "high" || item.TokenCount == nil || *item.TokenCount != 42 || !item.HasMessages || !item.HasSummary {
		t.Fatalf("projection: %+v", item)
	}
	page, next, err = m.List("owner", next, 2)
	if err != nil || len(page) != 1 || page[0].ID != "a" || next != 0 {
		t.Fatalf("second page: %+v next=%d err=%v", page, next, err)
	}
	other, _, err := m.List("other", 0, 2)
	if err != nil || len(other) != 0 {
		t.Fatalf("owner isolation: %+v %v", other, err)
	}
}

func TestPairedOwnersCannotAccessEachOthersSessions(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	if _, err := m.Open("device-a", "c", sample, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open("device-b", "c", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open: %v", err)
	}
	if _, err := m.Get("device-b", "c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if _, err := m.Save("device-b", "c", "edit", 1, sample); !errors.Is(err, ErrNotFound) {
		t.Fatalf("save: %v", err)
	}
	rev := uint64(1)
	if err := m.Delete("device-b", "c", "delete", &rev); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Get("device-a", "c"); err != nil {
		t.Fatalf("owner lost access: %v", err)
	}
}

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

func TestListSnapshotStableAcrossChangesAndOwnerBound(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	for _, id := range []string{"a", "b", "c"} {
		raw, _ := json.Marshal(map[string]any{"id": id, "title": "", "created_at": 1, "updated_at": 1, "messages": []any{}})
		if _, err := m.Open("owner", id, raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := m.ListSnapshot("owner", "", 1)
	if err != nil || first[0].ID != "a" || cursor == "" {
		t.Fatalf("first: %v %q %v", first, cursor, err)
	}
	rev := uint64(1)
	if err = m.Delete("owner", "b", "delete", &rev); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.ListSnapshot("other", cursor, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross owner: %v", err)
	}
	second, next, err := m.ListSnapshot("owner", cursor, 1)
	if err != nil || len(second) != 1 || second[0].ID != "b" || second[0].DeletedAt != 0 {
		t.Fatalf("snapshot: %v %v", second, err)
	}
	retry, _, err := m.ListSnapshot("owner", cursor, 1)
	if err != nil || retry[0] != second[0] {
		t.Fatalf("retry: %v %v", retry, err)
	}
	third, next, err := m.ListSnapshot("owner", next, 1)
	if err != nil || len(third) != 1 || third[0].ID != "c" || next != "" {
		t.Fatalf("last: %v %q %v", third, next, err)
	}
	fresh, _, err := m.ListSnapshot("owner", "", 100)
	if err != nil || len(fresh) != 3 || fresh[0].ID != "b" || fresh[0].DeletedAt == 0 {
		t.Fatalf("tombstone: %v %v", fresh, err)
	}
	m.listMu.Lock()
	for key, snap := range m.listSnapshots {
		snap.expires = time.Now().Add(-time.Second)
		m.listSnapshots[key] = snap
	}
	m.listMu.Unlock()
	if _, _, err = m.ListSnapshot("owner", cursor, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired: %v", err)
	}
}

func TestListSnapshotLargeTraversal(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("chat-%03d", i)
		raw, _ := json.Marshal(map[string]any{"id": id, "title": "", "created_at": 1, "updated_at": i + 1, "messages": []any{}})
		if _, err := m.Open("owner", id, raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	cursor := ""
	for {
		page, next, err := m.ListSnapshot("owner", cursor, 17)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page {
			if ids[item.ID] {
				t.Fatal("duplicate", item.ID)
			}
			ids[item.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(ids) != 105 {
		t.Fatalf("got %d", len(ids))
	}
}

func TestRuntimeCannotMakeExistingHistoryContainCredential(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	conversation := json.RawMessage(`{"id":"c","title":"private-sentinel","created_at":1,"updated_at":2,"messages":[]}`)
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"m","auth":{"method":"bearer_token","value":"private-sentinel"}}}`)
	if _, err := m.Open("owner", "c", conversation, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("owner", "c", runtime); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("configure: %v", err)
	}
	if _, err := m.Open("owner", "c", nil, runtime); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("active open: %v", err)
	}
	if err := m.Close("owner", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open("owner", "c", nil, runtime); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("durable open: %v", err)
	}
}

func TestListSnapshotBoundsMetadataPageBytes(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "storage")))
	for _, id := range []string{"a", "b"} {
		raw, _ := json.Marshal(map[string]any{"id": id, "title": strings.Repeat("q", 1100<<10), "created_at": 1, "updated_at": 1, "messages": []any{}})
		if _, err := m.Open("owner", id, raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := m.ListSnapshot("owner", "", 100)
	if err != nil || len(first) != 1 || cursor == "" {
		t.Fatalf("first=%d cursor=%q err=%v", len(first), cursor, err)
	}
	second, next, err := m.ListSnapshot("owner", cursor, 100)
	if err != nil || len(second) != 1 || next != "" {
		t.Fatalf("second=%d next=%q err=%v", len(second), next, err)
	}
}

// Reopening a live session exposes a current event cursor even after old
// events have been evicted; a daemon restart resets the cursor to zero.
func TestViewEventCursorIsScopedToLiveSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "storage")
	m := New(chatstore.New(dir))
	first, err := m.Open("owner", "c", sample, nil)
	if err != nil || first.EventSeq != 0 {
		t.Fatalf("initial cursor: %+v %v", first, err)
	}
	edited := json.RawMessage(`{"id":"c","title":"changed","created_at":1,"updated_at":3,"messages":[]}`)
	saved, err := m.Save("owner", "c", "edit", first.Revision, edited)
	if err != nil || saved.EventSeq != 1 {
		t.Fatalf("saved cursor: %+v %v", saved, err)
	}
	reopened, err := m.Open("owner", "c", nil, nil)
	if err != nil || reopened.EventSeq != 1 {
		t.Fatalf("reopen cursor: %+v %v", reopened, err)
	}
	if events, err := m.Events("owner", "c", reopened.EventSeq); err != nil || len(events) != 0 {
		t.Fatalf("cursor not accepted: %v %v", events, err)
	}
	// Expire all initial events without changing the durable conversation.
	m.mu.Lock()
	for i := 0; i < 2050; i++ {
		m.emit(m.sessions["c"], "c", "older", "delta", map[string]string{"text": "x"})
	}
	m.mu.Unlock()
	if _, err := m.Events("owner", "c", 0); !errors.Is(err, ErrGap) {
		t.Fatalf("expected old cursor to expire, got %v", err)
	}
	reopened, err = m.Open("owner", "c", nil, nil)
	if err != nil || reopened.EventSeq != 2051 {
		t.Fatalf("post-eviction cursor: %+v %v", reopened, err)
	}
	if events, err := m.Events("owner", "c", reopened.EventSeq); err != nil || len(events) != 0 {
		t.Fatalf("fresh cursor not accepted: %v %v", events, err)
	}
	chunked, err := m.OpenChunked("owner", "c")
	if err != nil || chunked.EventSeq != reopened.EventSeq {
		t.Fatalf("chunked cursor: %+v %v", chunked, err)
	}
	if got, err := m.Get("other", "c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner access leaked: %+v %v", got, err)
	}
	restarted := New(chatstore.New(dir))
	recovered, err := restarted.Open("owner", "c", nil, nil)
	if err != nil || recovered.EventSeq != 0 || recovered.Revision != saved.Revision {
		t.Fatalf("restart cursor: %+v %v", recovered, err)
	}
}
