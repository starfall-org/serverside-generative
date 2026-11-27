package chatsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starfall-org/generative-server/internal/chatstore"
)

func TestCustomToolGrantDedupeAndDurableResult(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":"ok"}`)
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	tool := map[string]any{"id": "t", "name": "tool", "enabled": true, "url": server.URL, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "customTools": []any{tool}})
	if _, err := m.Open("owner", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	if err := m.StartCustomTool("owner", "c", "call", "t", json.RawMessage(`{}`)); !errors.Is(err, ErrDenied) || calls.Load() != 0 {
		t.Fatalf("missing grant: %v", err)
	}
	runtime, _ = json.Marshal(map[string]any{"source": "mobile_ephemeral", "customTools": []any{tool}, "toolGrants": map[string]any{"custom_http": map[string]bool{"t": true}}})
	if err := m.Configure("owner", "c", runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("owner", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.StartCustomTool("owner", "c", "call", "t", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err = m.StartCustomTool("owner", "c", "call", "t", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	var cursor uint64
	done := false
	deadline := time.After(5 * time.Second)
	for !done {
		events, err := m.Events("owner", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			cursor = e.Seq
			if e.Kind == "error" {
				t.Fatalf("tool failed: %+v", e)
			}
			if e.Kind == "completed" {
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	view, err := m.Get("owner", "c")
	if err != nil || calls.Load() != 1 || !strings.Contains(string(view.Conversation), `"toolActivity"`) || !strings.Contains(string(view.Conversation), `"success"`) {
		t.Fatalf("result: %s calls=%d err=%v", view.Conversation, calls.Load(), err)
	}
	if snap, err := chatstore.New(dir).Get("owner", "c"); err != nil || !strings.Contains(string(snap.Conversation), `"success"`) {
		t.Fatalf("tool result missing from server storage: %+v %v", snap, err)
	}
}

func TestToolResultPersistedAndDeduplicatedAfterRestart(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":"ok"}`)
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	tool := map[string]any{"id": "t", "name": "tool", "enabled": true, "url": server.URL,
		"headers":    map[string]string{"X-Key": "runtime-tool-secret"},
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral",
		"customTools": []any{tool}, "toolGrants": map[string]any{"custom_http": map[string]bool{"t": true}}})
	if _, err := m.Open("owner", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("owner", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.StartCustomTool("owner", "c", "call", "t", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	cursor := uint64(0)
	deadline := time.After(5 * time.Second)
	complete := false
	for !complete {
		events, err := m.Events("owner", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			cursor = e.Seq
			if e.Kind == "error" {
				t.Fatalf("tool error: %+v", e)
			}
			if e.Kind == "completed" {
				complete = true
			}
		}
		if !complete {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	v, err := m.Get("owner", "c")
	if err != nil || v.Revision != 3 || !strings.Contains(string(v.Conversation), `"success"`) {
		t.Fatalf("view=%s rev=%d err=%v", v.Conversation, v.Revision, err)
	}
	if err = m.StartCustomTool("owner", "c", "call", "t", json.RawMessage(`{}`)); err != nil || calls.Load() != 1 {
		t.Fatalf("in-process duplicate: %v %d", err, calls.Load())
	}
	snap, err := chatstore.New(dir).Get("owner", "c")
	if err != nil || snap.Revision != 3 || !strings.Contains(string(snap.Conversation), `"success"`) || strings.Contains(string(snap.Conversation), "runtime-tool-secret") {
		t.Fatalf("persistent tool result or secret error: %+v %v", snap, err)
	}
	if err = m.Close("owner", "c"); err != nil {
		t.Fatal(err)
	}
	m = New(chatstore.New(dir))
	if _, err = m.Open("owner", "c", nil, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Get("owner", "c"); err != nil {
		t.Fatal(err)
	}
	if view, err := m.Get("owner", "c"); err != nil || !strings.Contains(string(view.Conversation), `"success"`) {
		t.Fatalf("durable tool result lost after restart: %s %v", view.Conversation, err)
	}
	if err := m.StartCustomTool("owner", "c", "call", "t", json.RawMessage(`{}`)); !errors.Is(err, ErrReplay) || calls.Load() != 1 {
		t.Fatalf("tool replayed after restart: %v calls=%d", err, calls.Load())
	}
}
