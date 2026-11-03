package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type codexBackend struct {
	s         *Session
	p         *stdioProcess
	mu        sync.Mutex
	threadID  string
	turnID    string
	approvals map[string]json.RawMessage
}

func newCodex(s *Session) *codexBackend {
	b := &codexBackend{s: s, approvals: make(map[string]json.RawMessage)}
	b.p = newProcess(s, []string{"app-server"}, b.onMessage)
	return b
}

func (b *codexBackend) start(ctx context.Context) error {
	if err := b.p.start(); err != nil {
		return err
	}
	_, err := b.p.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "serverside_generative", "title": "Serverside Generative", "version": "1.0"},
	})
	if err != nil {
		return fmt.Errorf("codex initialize: %w", err)
	}
	if err = b.p.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	raw, err := b.p.request(ctx, "thread/start", map[string]any{"cwd": b.s.cwd})
	if err != nil {
		return fmt.Errorf("codex thread/start: %w", err)
	}
	var value struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Thread.ID == "" {
		return errors.New("codex thread/start returned no thread id")
	}
	b.mu.Lock()
	b.threadID = value.Thread.ID
	b.mu.Unlock()
	return nil
}

func (b *codexBackend) send(prompt string) error {
	if !b.s.beginTurn() {
		return errors.New("codex turn is already running; use agent_steer")
	}
	b.mu.Lock()
	thread := b.threadID
	b.mu.Unlock()
	raw, err := b.p.call("turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": prompt}}})
	if err != nil {
		b.s.setBusy(false)
		return err
	}
	var value struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Turn.ID == "" {
		b.s.setBusy(false)
		return errors.New("codex turn/start returned no turn id")
	}
	b.mu.Lock()
	b.turnID = value.Turn.ID
	b.mu.Unlock()
	b.s.publish("status", "", "", "", "turn_started", "running", raw)
	return nil
}

func (b *codexBackend) steer(prompt string) error {
	b.mu.Lock()
	thread, turn := b.threadID, b.turnID
	b.mu.Unlock()
	if thread == "" || turn == "" {
		return errors.New("no running codex turn")
	}
	_, err := b.p.call("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []any{map[string]any{"type": "text", "text": prompt}}})
	return err
}

func (b *codexBackend) interrupt() error {
	b.mu.Lock()
	thread, turn := b.threadID, b.turnID
	b.mu.Unlock()
	if turn == "" {
		return errors.New("no active codex turn")
	}
	_, err := b.p.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}

func (b *codexBackend) respond(id, decision string) error {
	b.mu.Lock()
	requestID, ok := b.approvals[id]
	if ok {
		delete(b.approvals, id)
	}
	b.mu.Unlock()
	if !ok {
		return errors.New("approval request not found or already answered")
	}
	if decision == "once" {
		decision = "accept"
	}
	if decision == "always" {
		decision = "acceptForSession"
	}
	if decision == "reject" {
		decision = "decline"
	}
	// JSON-RPC replies MUST reuse the exact id in the request.
	return b.p.write(map[string]any{"id": requestID, "result": map[string]any{"decision": decision}})
}

func (b *codexBackend) close() error { return b.p.stop() }

func getString(raw map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(raw[key], &v)
	return v
}
func getField(raw json.RawMessage, key string) json.RawMessage {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	return obj[key]
}
func getFieldString(raw json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(getField(raw, key), &v)
	return v
}
func (b *codexBackend) onMessage(msg map[string]json.RawMessage, raw json.RawMessage) {
	method := getString(msg, "method")
	if method == "" {
		return
	}
	params := msg["params"]
	if id, ok := msg["id"]; ok {
		approvalID := approvalKey(id)
		b.mu.Lock()
		b.approvals[approvalID] = append(json.RawMessage(nil), id...)
		b.mu.Unlock()
		b.s.publish("approval", "", getFieldString(params, "itemId"), approvalID, method, "pending", raw)
		return
	}
	item := getField(params, "item")
	itemID := getFieldString(item, "id")
	itemType := getFieldString(item, "type")
	delta := getFieldString(params, "delta")
	switch {
	case method == "item/agentMessage/delta" || method == "item/agentMessage/contentDelta":
		b.s.publish("content", itemID, "", "", delta, "streaming", raw)
	case strings.HasPrefix(method, "item/reasoning"):
		b.s.publish("thinking", itemID, "", "", delta, "streaming", raw)
	case method == "item/started" && itemType == "agentMessage":
		b.s.publish("content", itemID, "", "", "", "started", raw)
	case method == "item/started" && itemType == "reasoning":
		b.s.publish("thinking", itemID, "", "", "", "started", raw)
	case method == "item/started":
		if isCodexToolItem(itemType) {
			b.s.publish("tool_call", itemID, itemID, "", "", "started", raw)
		} else {
			b.s.publish("status", itemID, "", "", method, "started", raw)
		}
	case method == "item/completed" && itemType == "agentMessage":
		b.s.publish("content", itemID, "", "", "", "completed", raw)
	case method == "item/completed" && itemType == "reasoning":
		b.s.publish("thinking", itemID, "", "", "", "completed", raw)
	case method == "item/completed":
		if isCodexToolItem(itemType) {
			b.s.publish("tool_result", itemID, itemID, "", "", "completed", raw)
		} else {
			b.s.publish("status", itemID, "", "", method, "completed", raw)
		}
	case method == "turn/completed":
		b.mu.Lock()
		b.turnID = ""
		b.mu.Unlock()
		b.s.setBusy(false)
		b.s.publish("completed", "", "", "", "", "completed", raw)
	case strings.HasPrefix(method, "turn/"):
		b.s.publish("status", "", "", "", method, "", raw)
	default:
		b.s.publish("status", itemID, "", "", method, "", raw)
	}
}

func isCodexToolItem(kind string) bool {
	switch kind {
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch", "imageGeneration", "collabAgentToolCall":
		return true
	default:
		return false
	}
}
