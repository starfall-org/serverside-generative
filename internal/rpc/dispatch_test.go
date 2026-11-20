package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"testing"

	"github.com/starfall-org/serverside-generative/internal/catalog"

	"github.com/starfall-org/serverside-generative/internal/chatsession"
	"github.com/starfall-org/serverside-generative/internal/chatstore"
)

func TestDispatcherAlwaysPersistsSessionRegardlessOfClientStorage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	d := New(chatsession.New(chatstore.New(dir)))
	send := func(owner, id, op, sid, data string) Reply {
		return d.Dispatch(owner, Request{ID: id, Op: op, SessionID: sid, Data: json.RawMessage(data)})
	}
	if r := send("", "unauthorized", "session.open", "c", `{}`); r.Error == nil || r.Error.Code != "unauthorized" {
		t.Fatalf("unauth %+v", r)
	}
	c := `{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}`
	for _, storage := range []string{``, `,"storage":"sync"`, `,"storage":"memory"`} {
		data := `{"conversation":` + c
		if storage != "" {
			data += storage
		}
		data += `}`
		r := send("a", "open", "session.open", "c", data)
		if !r.OK {
			t.Fatalf("open ignored local storage=%s: %+v", storage, r)
		}
	}
	if r := send("b", "get", "session.get", "c", `{}`); r.Error == nil || r.Error.Code != "not_found" {
		t.Fatalf("other owner %+v", r)
	}
	v := send("a", "get", "session.get", "c", `{}`)
	if !v.OK {
		t.Fatalf("get %+v", v)
	}
	if got := send("a", "unchanged", "session.get", "c", `{"if_revision":1}`); !got.OK || got.Data.(map[string]any)["unchanged"] != true {
		t.Fatalf("not modified %+v", got)
	}
	if got := send("a", "save", "session.save", "c", `{"expected_revision":0,"idempotency_key":"save","conversation":`+c+`}`); got.Error == nil || got.Error.Code != "revision_conflict" {
		t.Fatalf("CAS %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "chats")); err != nil {
		t.Fatalf("conversation was not persisted: %v", err)
	}
	persisted, err := chatstore.New(dir).Get("a", "c")
	if err != nil || persisted.Revision != 1 {
		t.Fatalf("conversation missing from storage: %+v %v", persisted, err)
	}
	if got := send("a", "close", "session.close", "c", `{}`); !got.OK {
		t.Fatalf("close %+v", got)
	}
}

func TestConditionalSessionOpenAvoidsTransferringLargeUnchangedHistory(t *testing.T) {
	d := New(chatsession.New(chatstore.New(filepath.Join(t.TempDir(), "data"))))
	send := func(owner, op, sid string, data any) Reply {
		raw, _ := json.Marshal(data)
		return d.Dispatch(owner, Request{ID: "req", Op: op, SessionID: sid, Data: raw})
	}
	initial := send("a", "session.open", "chat", map[string]any{})
	if !initial.OK {
		t.Fatalf("open: %+v", initial)
	}
	view := initial.Data.(chatsession.View)
	// The first open supplies the transcript; a conditional open transfers only metadata.
	unchanged := send("a", "session.open", "chat", map[string]any{"if_revision": view.Revision})
	if !unchanged.OK || unchanged.Data.(map[string]any)["unchanged"] != true ||
		unchanged.Data.(map[string]any)["event_seq"] != view.EventSeq {
		t.Fatalf("unchanged: %+v", unchanged)
	}
	if got := send("b", "session.open", "chat", map[string]any{"if_revision": view.Revision}); got.OK {
		t.Fatalf("other owner used cached revision: %+v", got)
	}
	updated := json.RawMessage(`{"id":"chat","title":"changed","created_at":1,"updated_at":2,"messages":[]}`)
	saved := send("a", "session.save", "chat", map[string]any{"expected_revision": view.Revision, "idempotency_key": "edit", "conversation": updated})
	if !saved.OK {
		t.Fatalf("save: %+v", saved)
	}
	changed := send("a", "session.open", "chat", map[string]any{"if_revision": view.Revision})
	if !changed.OK {
		t.Fatalf("updated open: %+v", changed)
	}
	if changed.Data.(chatsession.View).Revision <= view.Revision {
		t.Fatalf("stale transcript: %+v", changed)
	}
	notModified := send("a", "session.get", "chat", map[string]any{"if_revision": changed.Data.(chatsession.View).Revision})
	if !notModified.OK || notModified.Data.(map[string]any)["unchanged"] != true ||
		notModified.Data.(map[string]any)["event_seq"] != changed.Data.(chatsession.View).EventSeq {
		t.Fatalf("get cache validation: %+v", notModified)
	}
}

func TestConfigSaveAndSecretRejection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	d := New(chatsession.New(chatstore.New(dir)))
	d.SetCatalog(catalog.New(dir))
	files := map[string]string{"providers.yaml": "schema_version: 1\nrevision: 1\ngroups: []\nproviders: []\n", "models.yaml": "schema_version: 1\nrevision: 1\nproviders: {}\n", "mcp.yaml": "schema_version: 1\nrevision: 1\nservers: []\n", "profiles.yaml": "schema_version: 1\nrevision: 1\nchatProfiles: []\n"}
	raw, _ := json.Marshal(map[string]any{"expected_revision": 0, "files": files})
	if got := d.Dispatch("owner", Request{ID: "save", Op: "config.save", Data: raw}); !got.OK {
		t.Fatalf("save: %+v", got)
	}
	if got := d.Dispatch("owner", Request{ID: "get", Op: "config.get"}); !got.OK {
		t.Fatalf("get: %+v", got)
	}
	for name, doc := range files {
		files[name] = strings.Replace(doc, "revision: 1", "revision: 2", 1)
	}
	files["providers.yaml"] += "apiKey: secret-sentinel\n"
	raw, _ = json.Marshal(map[string]any{"expected_revision": 1, "files": files})
	if got := d.Dispatch("owner", Request{ID: "bad", Op: "config.save", Data: raw}); got.Error == nil || got.Error.Code != "invalid_request" {
		t.Fatalf("secret: %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(dir, "providers.yaml"))
	if err != nil || strings.Contains(string(b), "secret-sentinel") {
		t.Fatalf("persisted secret: %v", err)
	}
}

func TestProviderCatalogRequiresAuthenticatedOwner(t *testing.T) {
	d := New(nil)
	called := false
	d.SetProviderCatalog(func() (any, error) { called = true; return map[string]any{"version": 1, "providers": []any{}}, nil })
	req := Request{ID: "r", Op: "provider.catalog"}
	r := d.Dispatch("", req)
	if r.Error == nil || r.Error.Code != "unauthorized" || called {
		t.Fatalf("unauthorized: %+v", r)
	}
	r = d.Dispatch("owner", req)
	if !r.OK || !called || r.Data.(map[string]any)["version"] != 1 {
		t.Fatalf("catalog: %+v", r)
	}
}

func TestChunkedRPCRequiresOwnerAndMaintainsRevision(t *testing.T) {
	d := New(chatsession.New(chatstore.New(filepath.Join(t.TempDir(), "data"))))
	send := func(owner, op, id string, data any) Reply {
		raw, _ := json.Marshal(data)
		return d.Dispatch(owner, Request{ID: "r", Op: op, SessionID: id, Data: raw})
	}
	open := send("a", "session.open_chunked", "c", map[string]any{})
	if !open.OK {
		t.Fatal(open)
	}
	raw := []byte(`{"id":"c","title":"edited","created_at":1,"updated_at":2,"messages":[]}`)
	sum := sha256.Sum256(raw)
	begin := send("a", "session.upload.begin", "c", map[string]any{"expected_revision": 1, "size": len(raw), "sha256": hex.EncodeToString(sum[:]), "idempotency_key": "op"})
	if !begin.OK {
		t.Fatal(begin)
	}
	token := begin.Data.(map[string]any)["upload_id"].(string)
	if got := send("b", "session.upload.chunk", "c", map[string]any{"upload_id": token, "offset": 0, "data": raw}); got.OK {
		t.Fatal("foreign upload accepted")
	}
	if got := send("a", "session.upload.chunk", "c", map[string]any{"upload_id": token, "offset": 0, "data": raw}); !got.OK {
		t.Fatal(got)
	}
	if got := send("a", "session.upload.commit", "c", map[string]any{"upload_id": token}); !got.OK || got.Data.(chatsession.ChunkInfo).Revision != 2 {
		t.Fatal(got)
	}
	if got := send("a", "session.read_chunk", "c", map[string]any{"revision": 1, "offset": 0, "limit": 100}); got.Error == nil || got.Error.Code != "revision_conflict" {
		t.Fatal(got)
	}
	if got := send("a", "session.read_chunk", "c", map[string]any{"revision": 2, "offset": 0, "limit": 100}); !got.OK || string(got.Data.(chatsession.Chunk).Data) != string(raw) {
		t.Fatal(got)
	}
}

func TestChunkedLargeSnapshotsDoNotEscapeWholeRPC(t *testing.T) {
	d := New(chatsession.New(chatstore.New(filepath.Join(t.TempDir(), "data"))))
	send := func(op string, data any) Reply {
		raw, _ := json.Marshal(data)
		return d.Dispatch("owner", Request{ID: "r", Op: op, SessionID: "c", Data: raw})
	}
	if r := send("session.open_chunked", map[string]any{}); !r.OK {
		t.Fatal(r)
	}
	raw, _ := json.Marshal(map[string]any{"id": "c", "title": "x", "created_at": 1, "updated_at": 1, "messages": []any{}, "future": strings.Repeat("x", 4<<20)})
	digest := sha256.Sum256(raw)
	start := send("session.upload.begin", map[string]any{"expected_revision": 1, "size": len(raw), "sha256": hex.EncodeToString(digest[:]), "idempotency_key": "large"})
	if !start.OK {
		t.Fatal(start)
	}
	id := start.Data.(map[string]any)["upload_id"].(string)
	for offset := 0; offset < len(raw); {
		end := offset + (256 << 10)
		if end > len(raw) {
			end = len(raw)
		}
		part := send("session.upload.chunk", map[string]any{"upload_id": id, "offset": offset, "data": raw[offset:end]})
		if !part.OK {
			t.Fatal(part)
		}
		offset = end
	}
	if r := send("session.upload.commit", map[string]any{"upload_id": id}); !r.OK {
		t.Fatal(r)
	}
	for _, op := range []string{"session.open", "session.get"} {
		r := send(op, map[string]any{})
		if r.OK || r.Error == nil || r.Error.Code != "resource_exhausted" || r.Data != nil {
			t.Fatalf("%s: %+v", op, r)
		}
		// A verified local mirror can bypass the whole-chat transport ceiling,
		// with the server still validating the owner and revision.
		cached := send(op, map[string]any{"if_revision": 2})
		if !cached.OK || cached.Data.(map[string]any)["unchanged"] != true ||
			cached.Data.(map[string]any)["revision"] != uint64(2) {
			t.Fatalf("%s conditional: %+v", op, cached)
		}
	}
	info := send("session.open_chunked", map[string]any{})
	if !info.OK || info.Data.(chatsession.ChunkInfo).Size != len(raw) {
		t.Fatal(info)
	}
}

func TestSessionOpenOptionalDataAndDeletedError(t *testing.T) {
	d := New(chatsession.New(chatstore.New(filepath.Join(t.TempDir(), "data"))))
	for i, raw := range []string{"", "null", `{"conversation":null,"runtime":null}`} {
		id := []string{"one", "two", "three"}[i]
		r := d.Dispatch("owner", Request{ID: "open", Op: "session.open", SessionID: id, Data: json.RawMessage(raw)})
		if !r.OK {
			t.Fatalf("open %q: %+v", raw, r.Error)
		}
		got := d.Dispatch("owner", Request{ID: "delete", Op: "session.delete", SessionID: id, Data: json.RawMessage(`{"expected_revision":1,"idempotency_key":"delete"}`)})
		if !got.OK {
			t.Fatalf("delete: %+v", got.Error)
		}
		got = d.Dispatch("owner", Request{ID: "reopen", Op: "session.open", SessionID: id})
		if got.OK || got.Error == nil || got.Error.Code != "session_deleted" {
			t.Fatalf("deleted: %+v", got)
		}
	}
}
