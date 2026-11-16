package rpc

import (
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
