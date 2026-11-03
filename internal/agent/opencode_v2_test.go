package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestOpenCodeV2StructuredEvents(t *testing.T) {
	s := &Session{id: "test-v2", alive: true, ready: true, updates: make(chan struct{}, 1)}
	b := newOpenCode(s)
	b.apiV2 = true
	b.sessionID = "ses-v2"
	input := []struct {
		raw     string
		kind    string
		content string
	}{
		{`{"type":"session.text.delta","data":{"sessionID":"ses-v2","id":"txt-1","delta":"hello"}}`, "content", "hello"},
		{`{"type":"session.reasoning.delta","data":{"sessionID":"ses-v2","id":"reason-1","delta":"thought"}}`, "thinking", "thought"},
		{`{"type":"session.tool.called","data":{"sessionID":"ses-v2","id":"tool-1","callID":"call-1","tool":"bash"}}`, "tool_call", "bash"},
		{`{"type":"session.tool.success","data":{"sessionID":"ses-v2","id":"tool-1","callID":"call-1"}}`, "tool_result", ""},
		{`{"type":"permission.asked","data":{"sessionID":"ses-v2","id":"permission-1"}}`, "approval", ""},
	}
	for _, item := range input {
		b.onEvent(json.RawMessage(item.raw))
	}
	b.onEvent(json.RawMessage(`{"type":"session.text.delta","data":{"sessionID":"not-this-session","delta":"should-not-appear"}}`))
	if len(s.events) != len(input) {
		t.Fatalf("event count=%d expected=%d", len(s.events), len(input))
	}
	for i, item := range input {
		if s.events[i].Kind != item.kind || s.events[i].Content != item.content {
			t.Errorf("event %d wrong: %+v", i, s.events[i])
		}
		if string(s.events[i].Raw) != item.raw {
			t.Errorf("event %d lost native payload", i)
		}
	}
	if !b.approvals["permission-1"] {
		t.Fatal("native approval request missing")
	}
}

func TestRealOpenCodeV2StartupAndClose(t *testing.T) {
	binary := os.Getenv("SERVER_TEST_OPENCODE_BIN")
	if binary == "" {
		t.Skip("set SERVER_TEST_OPENCODE_BIN to opt into an installed binary smoke test")
	}
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := manager.Open(ctx, Spec{Type: OpenCode, Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Running {
		t.Fatal("native server did not start")
	}
	sessions := manager.List()
	if len(sessions) != 1 {
		t.Fatalf("unexpected session count: %d", len(sessions))
	}
	if err := manager.Close(d.ID); err != nil {
		t.Fatal(err)
	}
}
