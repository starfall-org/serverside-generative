package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENT_TEST_HELPER") == "1" {
		agentTestHelper()
		return
	}
	os.Exit(m.Run())
}
func agentTestHelper() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "app-server" {
		fakeCodex()
		return
	}
	if len(args) > 0 && args[0] == "serve" {
		fakeOpenCode(args)
		return
	}
	fakeClaude()
}

func fakeCodex() {
	scanner := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	send := func(value any) {
		data, _ := json.Marshal(value)
		fmt.Fprintln(w, string(data))
		_ = w.Flush()
	}
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			continue
		}
		method := getString(m, "method")
		switch method {
		case "initialize":
			send(map[string]any{"id": m["id"], "result": map[string]any{"userAgent": "fake"}})
		case "thread/start":
			send(map[string]any{"id": m["id"], "result": map[string]any{"thread": map[string]any{"id": "mock-thread"}}})
		case "turn/start":
			send(map[string]any{"id": m["id"], "result": map[string]any{"turn": map[string]any{"id": "mock-turn"}}})
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "hello codex"}})
			send(map[string]any{"method": "item/reasoning/summaryTextDelta", "params": map[string]any{"delta": "reasoning"}})
			send(map[string]any{"method": "item/started", "params": map[string]any{"item": map[string]any{"id": "tool-1", "type": "commandExecution", "command": "echo test"}}})
			send(map[string]any{"method": "item/commandExecution/requestApproval", "id": 77, "params": map[string]any{"itemId": "tool-1"}})
		case "turn/steer":
			send(map[string]any{"id": m["id"], "result": map[string]any{}})
		case "turn/interrupt":
			send(map[string]any{"id": m["id"], "result": map[string]any{}})
		default:
			if m["result"] != nil {
				send(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{"id": "tool-1", "type": "commandExecution", "status": "completed"}}})
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "mock-turn"}}})
			}
		}
	}
}

func fakeClaude() {
	scanner := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	send := func(value any) { data, _ := json.Marshal(value); fmt.Fprintln(w, string(data)); _ = w.Flush() }
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			continue
		}
		switch getString(m, "type") {
		case "user":
			send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}}})
			send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hello claude"}}})
			send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "thinking_delta", "thinking": "thought"}}})
			send(map[string]any{"type": "control_request", "request_id": "permission-1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "tool-2", "input": map[string]any{"command": "echo test"}}})
		case "control_response":
			send(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-2"}}}})
			send(map[string]any{"type": "result", "result": "done"})
		}
	}
}

func fakeOpenCode(args []string) {
	port := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--port" {
			port = args[i+1]
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return
	}
	msgs := make(chan string, 30)
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"healthy":true}`)
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fake-session"}`)
	})
	mux.HandleFunc("/session/fake-session/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		msgs <- `{"type":"message.part.delta","properties":{"sessionID":"fake-session","partID":"part-1","field":"text","delta":"hello opencode"}}`
		msgs <- `{"type":"message.part.updated","properties":{"sessionID":"fake-session","part":{"id":"tool-3","type":"tool","callID":"call-3","tool":"bash","state":{"status":"completed"}}}}`
		msgs <- `{"type":"permission.asked","properties":{"sessionID":"fake-session","id":"permission-3","toolCallID":"call-3"}}`
		msgs <- `{"type":"session.idle","properties":{"sessionID":"fake-session"}}`
	})
	mux.HandleFunc("/session/fake-session/permissions/permission-3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "true")
	})
	mux.HandleFunc("/session/fake-session/abort", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "true")
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		for {
			select {
			case msg := <-msgs:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	_ = http.Serve(l, mux)
}

func fakeExecutables(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires executable symlinks")
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "claude", "opencode", "forked-codex"} {
		if err := os.Symlink(self, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_TEST_HELPER", "1")
}

func waitKinds(t *testing.T, m *Manager, id string, want []string) []Event {
	t.Helper()
	found := map[string]bool{}
	all := []Event{}
	cursor := uint64(0)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		events, err := m.Events(id, cursor, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events.Events {
			all = append(all, e)
			found[e.Kind] = true
			cursor = e.Seq
		}
		ok := true
		for _, kind := range want {
			if !found[kind] {
				ok = false
			}
		}
		if ok {
			return all
		}
	}
	t.Fatalf("missing event kinds %v; got %#v", want, found)
	return nil
}

func TestDefaultAndForkBinaryAndCodexProtocol(t *testing.T) {
	fakeExecutables(t)
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	d, err := m.Open(context.Background(), Spec{Type: Codex})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(d.Binary) == "something" {
		t.Fatal("unexpected binary")
	}
	if err := m.Send(d.ID, "hi"); err != nil {
		t.Fatal(err)
	}
	evs := waitKinds(t, m, d.ID, []string{"content", "thinking", "tool_call", "approval"})
	var approval string
	for _, e := range evs {
		if e.Kind == "approval" {
			approval = e.ApprovalID
		}
	}
	if approval != "77" {
		t.Fatalf("approval ID %q", approval)
	}
	if err := m.Respond(d.ID, approval, "accept"); err != nil {
		t.Fatal(err)
	}
	waitKinds(t, m, d.ID, []string{"tool_result", "completed"})
	if err := m.Send(d.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if err := m.Steer(d.ID, "clarification"); err != nil {
		t.Fatal(err)
	}
	fork, err := m.Open(context.Background(), Spec{Type: Codex, Binary: "forked-codex"})
	if err != nil {
		t.Fatal(err)
	}
	if fork.Type != Codex {
		t.Fatal("fork changed type")
	}
	customPath := filepath.Join(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0], "forked-codex")
	absolute, err := m.Open(context.Background(), Spec{Type: Codex, Binary: customPath})
	if err != nil {
		t.Fatal(err)
	}
	if absolute.Type != Codex {
		t.Fatal("absolute fork changed adapter type")
	}
}

func TestClaudeBidiAndStructuredEvents(t *testing.T) {
	fakeExecutables(t)
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	d, err := m.Open(context.Background(), Spec{Type: Claude})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(d.ID, "prompt"); err != nil {
		t.Fatal(err)
	}
	evs := waitKinds(t, m, d.ID, []string{"content", "thinking", "approval"})
	for _, e := range evs {
		if e.Kind == "approval" {
			if err := m.Respond(d.ID, e.ApprovalID, "accept"); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitKinds(t, m, d.ID, []string{"tool_result", "completed"})
}

func TestOpenCodeStructuredSSE(t *testing.T) {
	fakeExecutables(t)
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	d, err := m.Open(context.Background(), Spec{Type: OpenCode})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(d.ID, "prompt"); err != nil {
		t.Fatal(err)
	}
	waitKinds(t, m, d.ID, []string{"content", "tool_result", "approval", "completed"})
	if err := m.Interrupt(d.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Respond(d.ID, "permission-3", "accept"); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryValidationAndEventGap(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	for _, test := range []Spec{
		{Type: "invalid"},
		{Type: Codex, Binary: "foo/bar"},
		{Type: Codex, Binary: "/this/path/does/not/exist"},
	} {
		if _, err := m.Open(context.Background(), test); err == nil {
			t.Fatalf("accepted %+v", test)
		}
	}
	s := &Session{id: "test", ready: true, events: []Event{}, updates: make(chan struct{}, 1)}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	for i := 0; i < maxEvents+20; i++ {
		s.publish("content", "", "", "", strconv.Itoa(i), "", nil)
	}
	if _, err := m.Events("test", 0, 0); err == nil {
		t.Fatal("missing sync gap")
	}
	if _, err := m.Events("test", s.floor, 0); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()
}

func TestSessionPathBoundary(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveCWD(".."); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := m.resolveCWD("nested/../../"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestEventRawPreserved(t *testing.T) {
	s := &Session{id: "abc", updates: make(chan struct{}, 1)}
	raw := json.RawMessage(`{"x":1,"reasoning":{"text":"secret"}}`)
	s.publish("thinking", "", "", "", "", "", raw)
	raw[5] = '2'
	if strings.Contains(string(s.events[0].Raw), "2") {
		t.Fatal("raw payload not cloned")
	}
}
