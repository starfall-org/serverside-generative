package httptool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExecuteRequiresGrantAndMapsTemplates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("q") != "hello world" || r.Header.Get("X-Auth") != "token" || !strings.Contains(string(body), `"count":3`) {
			t.Errorf("incorrect request: %s %s", r.URL, string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"value":"ok"}]}`)
	}))
	defer server.Close()
	tool, _ := json.Marshal(map[string]any{"id": "t", "name": "search", "enabled": true, "method": "POST", "url": server.URL, "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer", "default": 3}, "token": map[string]any{"type": "string"}}, "required": []string{"q", "token"}}, "query": map[string]any{"q": "{{q}}"}, "headers": map[string]any{"X-Auth": "{{token}}"}, "body": `{"count":"{{count}}"}`, "bodyFormat": "JSON", "responseMode": "JSON", "responseMapping": map[string]any{"answer": "/items/0/value"}})
	args := json.RawMessage(`{"q":"hello world","token":"token"}`)
	if _, err := Execute(context.Background(), tool, args, Policy{}); err != ErrDenied || calls.Load() != 0 {
		t.Fatalf("no grant: %v", err)
	}
	if _, err := Execute(context.Background(), tool, args, Policy{Allowed: true}); err != ErrDenied || calls.Load() != 0 {
		t.Fatalf("no loopback policy: %v", err)
	}
	result, err := Execute(context.Background(), tool, args, Policy{Allowed: true, AllowLoopback: true})
	if err != nil || result.Status != 200 || result.Result.(map[string]any)["answer"] != "ok" || calls.Load() != 1 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls.Load())
	}
	if _, err = Execute(context.Background(), tool, json.RawMessage(`{"q":5}`), Policy{Allowed: true, AllowLoopback: true}); err == nil || calls.Load() != 1 {
		t.Fatalf("invalid args reached endpoint: %v", err)
	}
}
func TestExecuteRejectsMediaResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "binary")
	}))
	defer server.Close()
	raw, _ := json.Marshal(map[string]any{"id": "t", "name": "tool", "enabled": true, "url": server.URL, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	if _, err := Execute(context.Background(), raw, json.RawMessage(`{}`), Policy{Allowed: true, AllowLoopback: true}); err != ErrUnsupported {
		t.Fatalf("media: %v", err)
	}
}

func TestToolRejectsEchoedCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"echo":"sentinel-secret"}`)
	}))
	defer server.Close()
	raw, _ := json.Marshal(map[string]any{"id": "t", "name": "tool", "enabled": true, "url": server.URL, "headers": map[string]string{"Authorization": "Bearer sentinel-secret"}, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	if _, err := Execute(context.Background(), raw, json.RawMessage(`{}`), Policy{Allowed: true, AllowLoopback: true}); err != ErrSecretLeak {
		t.Fatalf("echoed credential accepted: %v", err)
	}
}

func TestToolRejectsEchoedBodyAndQueryLiterals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"echo":"body-secret-sentinel"}`)
	}))
	defer server.Close()
	raw, _ := json.Marshal(map[string]any{"id": "t", "name": "tool", "enabled": true, "method": "POST", "url": server.URL, "query": map[string]any{"q": "query-secret-sentinel"}, "body": `{"api_key":"body-secret-sentinel"}`, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	if _, err := Execute(context.Background(), raw, json.RawMessage(`{}`), Policy{Allowed: true, AllowLoopback: true}); err != ErrSecretLeak {
		t.Fatalf("echoed body secret accepted: %v", err)
	}
}
