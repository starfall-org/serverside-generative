package chatsession

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/chatstore"
	"github.com/starfall-org/serverside-generative/internal/secrets"
	"github.com/starfall-org/serverside-generative/internal/storedruntime"
)

func TestRunTextPersistenceAndDedupe(t *testing.T) {
	var calls atomic.Int32
	seen := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen <- body

		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []string{
			`{"reasoning_content":"Checking details"}`,
			`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"query\":"}}]}`,
			`{"tool_calls":[{"index":0,"function":{"arguments":"\"value\"}"}}]}`,
			`{"content":"hello"}`,
		} {
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "modelConfig": map[string]any{"temperature": 0.7, "top_p": 0.8, "modelType": "TEXT_GENERATION"}, "auth": map[string]string{"method": "bearer_token", "value": "run-secret-sentinel"}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.Run("a", "c", "run1", "hi"); err != nil {
		t.Fatal(err)
	}
	if err = m.Run("a", "c", "run1", "hi"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	cursor := uint64(0)
	done := false
	kinds := map[string]bool{}
	for !done {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			cursor = e.Seq
			kinds[e.Kind] = true
			if e.Kind == "error" {
				t.Fatalf("run error: %+v", e)
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
	for _, kind := range []string{"delta", "reasoning_delta", "tool_activity"} {
		if !kinds[kind] {
			t.Fatalf("missing %s event: %v", kind, kinds)
		}
	}
	request := <-seen
	if request["temperature"] != 0.7 || request["top_p"] != 0.8 {
		t.Fatalf("sampling settings not sent: %+v", request)
	}
	v, err := m.Get("a", "c")
	if err != nil || !strings.Contains(string(v.Conversation), "hello") || calls.Load() != 1 || v.Revision != 3 {
		t.Fatalf("view=%s rev=%d calls=%d err=%v", v.Conversation, v.Revision, calls.Load(), err)
	}
	for _, field := range []string{`"reasoning_content":"Checking details"`, `"name":"lookup"`, `"arguments":"{\"query\":\"value\"}"`, `"status":"error"`} {
		if !strings.Contains(string(v.Conversation), field) {
			t.Fatalf("missing typed output %s in %s", field, v.Conversation)
		}
	}
	if _, err = runtimeState(runtime, v.Conversation, "next"); err != nil {
		t.Fatalf("typed history blocks follow-up: %v", err)
	}
	// Reopening must select the provider/model that actually generated the turn.
	var savedModel struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err = json.Unmarshal(v.Conversation, &savedModel); err != nil || savedModel.ModelID != "test" {
		t.Fatalf("model provenance missing from durable snapshot: %+v %v", savedModel, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "chats"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "chats", entries[0].Name()))
	if err != nil || strings.Contains(string(data), "run-secret-sentinel") {
		t.Fatalf("secret written: %v", err)
	}
	if strings.Contains(string(v.Conversation), "server_request_id") {
		t.Fatal("server receipt leaked into mobile DTO")
	}
	if err = m.Close("a", "c"); err != nil {
		t.Fatal(err)
	}
	restarted := New(chatstore.New(dir))
	restarted.AllowLocalAPI(true)
	if _, err = restarted.Open("a", "c", nil, runtime); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Run("a", "c", "run1", "hi"); !errors.Is(err, ErrReplay) || calls.Load() != 1 {
		t.Fatalf("run replayed after restart: %v", err)
	}
}

func TestRuntimeUsesSummaryAndRejectsUnsupportedHistory(t *testing.T) {
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"test","auth":{"method":"none"}}}`)
	conversation := json.RawMessage(`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[{"id":"m1","role":"user","versions":[{"content":"old"}]},{"id":"m2","role":"model","versions":[{"content":"recent"}]}],"summary":{"id":"s","content":"summary","through_message_id":"m1","role":"SYSTEM"}}`)
	state, err := runtimeState(runtime, conversation, "new")
	if err != nil || len(state.Conversation) != 3 || state.Conversation[0].Content != "Conversation summary:\nsummary" || state.Conversation[1].Content != "recent" || state.Conversation[2].Content != "new" {
		t.Fatalf("summary: %+v %v", state.Conversation, err)
	}
	bad := json.RawMessage(`{"messages":[{"role":"model","versions":[{"reasoning_signature":"sig"}]}]}`)
	if _, err = runtimeState(runtime, bad, "new"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("signature silently ignored: %v", err)
	}
}

func TestStoredRuntimePersistsChatButNotTransientCredentials(t *testing.T) {
	const headerSecret = "private-provider-header-sentinel"
	seenInstruction := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stored-sentinel" || r.Header.Get("X-Provider-Key") != headerSecret {
			http.Error(w, "unauthorized", 401)
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		err := json.NewDecoder(r.Body).Decode(&body)
		seenInstruction <- err == nil && len(body.Messages) > 0 && body.Messages[0].Role == "system" && body.Messages[0].Content == "Instruction test"
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	store := catalog.New(dir)
	payload := fmt.Sprintf(`{"id":"p","name":"Provider","type":"openai","baseUrl":%q,
		"auth":{"method":"bearer_token","value":"stored-sentinel"},
		"config":{"modelIds":["test"],"headers":{"X-Provider-Key":%q}}}`, upstream.URL, headerSecret)
	if err := store.ImportMobile("provider", json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
	bundle, err := store.Get()
	if err != nil || strings.Contains(bundle.Files["providers.yaml"], headerSecret) {
		t.Fatal("plaintext header in catalog", err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	m.SetStoredResolver(func(raw json.RawMessage) (json.RawMessage, error) { return storedruntime.Resolve(dir, raw) })
	if _, err = m.Open("a", "c", json.RawMessage(`{"id":"c","title":"title","created_at":1,"updated_at":2,"messages":[],"selected_prompt_ids":["instruction"]}`), json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"test","instructions":{"instruction":"Instruction test"}}`)); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.Run("a", "c", "stored-run", "hello"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	var cursor uint64
	done := false
	for !done {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			cursor = e.Seq
			encoded, _ := json.Marshal(e)
			if strings.Contains(string(encoded), headerSecret) {
				t.Fatal("plaintext header in event")
			}
			if e.Kind == "error" {
				t.Fatalf("run: %+v", e)
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
	if !<-seenInstruction {
		t.Fatal("instruction not sent as system text")
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(after) != len(before)+3 {
		t.Fatalf("expected private chats, backups and job leases directories: before=%d after=%d err=%v", len(before), len(after), err)
	}
	snap, err := chatstore.New(dir).Get("a", "c")
	var selected struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err == nil {
		err = json.Unmarshal(snap.Conversation, &selected)
	}
	if err != nil || selected.ProviderID != "p" || selected.ModelID != "test" {
		t.Fatalf("stored provider/model selection not persisted: %+v %v", selected, err)
	}
	if err != nil || !strings.Contains(string(snap.Conversation), "ok") || strings.Contains(string(snap.Conversation), "stored-sentinel") || strings.Contains(string(snap.Conversation), headerSecret) || strings.Contains(string(snap.Conversation), "Instruction test") {
		t.Fatalf("chat persistence or leaked credential: %+v %v", snap, err)
	}
}

func TestCancelStopsInFlightRunWithoutReplay(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": "token-value"}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.Run("a", "c", "run", "hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not called")
	}
	if err = m.Cancel("a", "c", "wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong job: %v", err)
	}
	if err = m.Cancel("a", "c", "run"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	cursor := uint64(0)
	done := false
	for !done {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			if event.Kind == "error" {
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("cancel timeout")
			}
		}
	}
	if err = m.Close("a", "c"); err != nil {
		t.Fatal(err)
	}
	restarted := New(chatstore.New(dir))
	restarted.AllowLocalAPI(true)
	if _, err = restarted.Open("a", "c", nil, runtime); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Run("a", "c", "run", "hello"); !errors.Is(err, ErrReplay) || calls.Load() != 1 {
		t.Fatalf("cancelled request retried: %v", err)
	}
}

func TestStoredProviderCustomAuthenticationExecutesInMemory(t *testing.T) {
	for _, method := range []string{"query_param", "custom_header"} {
		t.Run(method, func(t *testing.T) {
			const secret = "stored-auth-test-secret"
			var received atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				value := r.URL.Query().Get("X_Key")
				if method == "custom_header" {
					value = r.Header.Get("X_Key")
				}
				if value != secret {
					http.Error(w, "unauthorized", 401)
					return
				}
				received.Store(true)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			dir := filepath.Join(t.TempDir(), "data")
			docs, err := catalog.Empty()
			if err != nil {
				t.Fatal(err)
			}
			docs["providers.yaml"] = fmt.Sprintf("schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: P\n    type: openai\n    baseUrl: %s\n    auth: {method: %s, parameter_name: X_Key, credential_ref: provider/p/main}\n    config: {}\n", upstream.URL, method)
			docs["models.yaml"] = "schema_version: 1\nrevision: 1\nproviders:\n  p: {modelIds: [test], modelConfigs: {}, catalog: []}\n"
			if _, err = catalog.New(dir).Save(0, docs); err != nil {
				t.Fatal(err)
			}
			store, err := secrets.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Put("provider", "p", "main", "", "api_key", []byte(secret)); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			m := New(chatstore.New(dir))
			m.AllowLocalAPI(true)
			m.SetStoredResolver(func(raw json.RawMessage) (json.RawMessage, error) { return storedruntime.Resolve(dir, raw) })
			if _, err = m.Open("a", "c", sample, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"test"}`)); err != nil {
				t.Fatal(err)
			}
			ch, stop, err := m.Subscribe("a", "c")
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if err = m.Run("a", "c", "run", "hello"); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(8 * time.Second)
			cursor := uint64(0)
			done := false
			for !done {
				events, err := m.Events("a", "c", cursor)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events {
					cursor = e.Seq
					if e.Kind == "error" {
						t.Fatalf("event: %+v", e)
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
			if !received.Load() {
				t.Fatal("upstream did not receive configured auth")
			}
			snap, err := chatstore.New(dir).Get("a", "c")
			if err != nil || !strings.Contains(string(snap.Conversation), "ok") {
				t.Fatalf("provider auth response not saved to chat: %+v %v", snap, err)
			}
		})
	}
}

func TestEventBufferBoundsBytesAndSignalsRecovery(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "data")))
	s := &session{owner: "a"}
	m.sessions["c"] = s
	m.emit(s, "c", "r", "tool_result", map[string]any{"result": strings.Repeat("x", maxConversationBytes+(80<<10))})
	if len(s.events) != 1 || s.events[0].Kind != "event_gap" {
		t.Fatal("oversized event did not require snapshot recovery")
	}
	for i := 0; i < 12; i++ {
		m.emit(s, "c", "r", "delta", map[string]any{"text": strings.Repeat("x", 900<<10)})
	}
	if s.eventBytes > 8<<20 || s.floor == 0 || len(s.events) >= 12 {
		t.Fatalf("unbounded event buffer: bytes=%d floor=%d events=%d", s.eventBytes, s.floor, len(s.events))
	}
	if _, err := m.Events("a", "c", 0); !errors.Is(err, ErrGap) {
		t.Fatalf("missing recovery signal: %v", err)
	}
	page, err := m.Events("a", "c", s.floor)
	if err != nil || len(page) == 0 || len(page) >= len(s.events) {
		t.Fatalf("events were not bounded into pages: %d %v", len(page), err)
	}
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > 2<<20 {
		t.Fatalf("event page too large: %d %v", len(encoded), err)
	}
	remaining, err := m.Events("a", "c", page[len(page)-1].Seq)
	if err != nil || len(remaining) == 0 || remaining[0].Seq != page[len(page)-1].Seq+1 {
		t.Fatalf("next page: %+v %v", remaining, err)
	}
}

func TestConcurrentManagersDoNotDuplicateSyncedRun(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	dir := filepath.Join(t.TempDir(), "data")
	first, second := New(chatstore.New(dir)), New(chatstore.New(dir))
	first.AllowLocalAPI(true)
	second.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": "secret"}}})
	if _, err := first.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Open("a", "c", nil, runtime); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		manager *Manager
		err     error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, manager := range []*Manager{first, second} {
		go func(m *Manager) { <-start; results <- outcome{m, m.Run("a", "c", "same-run", "hello")} }(manager)
	}
	close(start)
	one, two := <-results, <-results
	var winner *Manager
	if one.err == nil && (errors.Is(two.err, chatstore.ErrConflict) || errors.Is(two.err, ErrBusy)) {
		winner = one.manager
	} else if two.err == nil && (errors.Is(one.err, chatstore.ErrConflict) || errors.Is(one.err, ErrBusy)) {
		winner = two.manager
	} else {
		t.Fatalf("unexpected outcomes: %v, %v", one.err, two.err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not called")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	var cursor uint64
	for time.Now().Before(deadline) {
		events, err := winner.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			if event.Kind == "error" {
				t.Fatalf("run failed: %+v", event)
			}
			if event.Kind == "completed" {
				if calls.Load() != 1 {
					t.Fatalf("upstream calls: %d", calls.Load())
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run timed out")
}

func TestProgressCheckpointConflictStopsRunWithoutOverwrite(t *testing.T) {
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial text with enough padding for the guard\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	m := New(store)
	m.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": "secret"}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	if err := m.Run("a", "c", "run", "hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not called")
	}
	hash := sha256.Sum256([]byte("c"))
	path := filepath.Join(dir, "chats", fmt.Sprintf("%x.json", hash))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `"title": "title"`, `"title": "external edit"`, 1)
	if edited == string(data) {
		t.Fatal("snapshot fixture changed")
	}
	if err = os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var cursor uint64
	for time.Now().Before(deadline) {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			if event.Kind == "error" {
				data, _ := json.Marshal(event.Data)
				if !strings.Contains(string(data), "persistence_failed") {
					t.Fatalf("unexpected error: %s", data)
				}
				current, err := store.Get("a", "c")
				if err != nil || !strings.Contains(string(current.Conversation), "external edit") {
					t.Fatalf("external edit overwritten: %+v %v", current, err)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("checkpoint conflict did not stop the run")
}

func TestExpiredOAuthTokenProducesAuthRequired(t *testing.T) {
	const secret = "expired-oauth-sentinel-72846"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"code":401,"message":%q,"status":"UNAUTHENTICATED","type":"authentication_error"}}`, secret)
	}))
	defer upstream.Close()
	for _, provider := range []string{"openai", "openai_responses", "anthropic", "google"} {
		t.Run(provider, func(t *testing.T) {
			m := New(chatstore.New(filepath.Join(t.TempDir(), "data")))
			m.AllowLocalAPI(true)
			runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": provider, "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "oauth", "value": secret}}})
			if _, err := m.Open("a", "c", sample, runtime); err != nil {
				t.Fatal(err)
			}
			if err := m.Run("a", "c", "run", "hello"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(8 * time.Second)
			var cursor uint64
			for time.Now().Before(deadline) {
				events, err := m.Events("a", "c", cursor)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					cursor = event.Seq
					if event.Kind == "error" {
						b, _ := json.Marshal(event)
						if strings.Contains(string(b), secret) || !strings.Contains(string(b), "auth_required") {
							t.Fatalf("incorrect error: %s", b)
						}
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("missing auth_required event")
		})
	}
}

func TestSelectedInstructionsRequireExplicitCompleteRuntime(t *testing.T) {
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"test","auth":{"method":"none"}},"instructions":{"first":"Be concise","second":"Use English"}}`)
	conversation := json.RawMessage(`{"messages":[{"role":"user","versions":[{"content":"prior"}]}],"selected_prompt_ids":["second","first"]}`)
	state, err := runtimeState(runtime, conversation, "new")
	if err != nil || len(state.Conversation) != 4 || state.Conversation[0].Role != "system" || state.Conversation[0].Content != "Use English" || state.Conversation[1].Content != "Be concise" || state.Conversation[2].Content != "prior" {
		t.Fatalf("instruction ordering: %+v %v", state.Conversation, err)
	}
	missing := json.RawMessage(`{"messages":[],"selected_prompt_ids":["missing"]}`)
	if _, err = runtimeState(runtime, missing, "new"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing: %v", err)
	}
	duplicate := json.RawMessage(`{"messages":[],"selected_prompt_ids":["first","first"]}`)
	if _, err = runtimeState(runtime, duplicate, "new"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestCatalogReportsUnsupportedAdapterSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	docs, err := catalog.Empty()
	if err != nil {
		t.Fatal(err)
	}
	docs["providers.yaml"] = "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: Provider\n    type: openai\n    baseUrl: ''\n    auth: {method: none}\n"
	docs["models.yaml"] = "schema_version: 1\nrevision: 1\nproviders:\n  p: {modelIds: [m], modelConfigs: {m: {modelType: TEXT_GENERATION, top_k: 3}}, catalog: []}\n"
	if _, err = catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	got, err := storedruntime.ReadCatalogValidated(dir, ValidateStoredRuntime)
	if err != nil || len(got.Providers) != 1 || len(got.Providers[0].Models) != 1 || got.Providers[0].Models[0].Availability != "unsupported" {
		t.Fatalf("catalog %+v %v", got, err)
	}
}

func TestInstructionCannotForwardRuntimeCredential(t *testing.T) {
	m := New(chatstore.New(filepath.Join(t.TempDir(), "data")))
	raw := json.RawMessage(`{"id":"c","title":"","created_at":1,"updated_at":2,"messages":[],"selected_prompt_ids":["i"]}`)
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"m","auth":{"method":"bearer_token","value":"private-secret"}},"instructions":{"i":"Never send private-secret"}}`)
	if _, err := m.Open("owner", "c", raw, runtime); err != nil {
		t.Fatal(err)
	}
	if err := m.Run("owner", "c", "request", "hello"); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("credential in instruction: %v", err)
	}
	v, err := m.Get("owner", "c")
	if err != nil || v.Revision != 1 {
		t.Fatalf("run changed history: %+v %v", v, err)
	}
}
