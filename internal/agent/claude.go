package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
)

type claudeBackend struct {
	s         *Session
	p         *stdioProcess
	mu        sync.Mutex
	approvals map[string]json.RawMessage
}

func newClaude(s *Session) *claudeBackend {
	b := &claudeBackend{s: s, approvals: make(map[string]json.RawMessage)}
	b.p = newProcess(s, []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio"}, b.onMessage)
	return b
}

func (b *claudeBackend) start(ctx context.Context) error {
	if err := b.p.start(); err != nil {
		return err
	}
	select {
	case <-b.p.done:
		return errors.New("claude process exited during initialization")
	default:
		return nil
	}
}

func (b *claudeBackend) send(prompt string) error {
	b.s.setBusy(true)
	if err := b.p.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}); err != nil {
		b.s.setBusy(false)
		return err
	}
	return nil
}
func (b *claudeBackend) steer(prompt string) error {
	// Streaming input is bidirectional. This queues a message while the
	// process is already running, without restarting or PTY interaction.
	return b.p.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}
func (b *claudeBackend) interrupt() error {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	return b.p.write(map[string]any{"type": "control_request", "request_id": hex.EncodeToString(token), "request": map[string]any{"subtype": "interrupt"}})
}
func (b *claudeBackend) respond(id, decision string) error {
	b.mu.Lock()
	input, ok := b.approvals[id]
	if ok {
		delete(b.approvals, id)
	}
	b.mu.Unlock()
	if !ok {
		return errors.New("approval request not found or already answered")
	}
	response := map[string]any{"behavior": "deny", "message": "User denied"}
	if decision == "accept" || decision == "acceptForSession" || decision == "always" || decision == "once" {
		response = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	return b.p.write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": response,
	}})
}
func (b *claudeBackend) close() error { return b.p.stop() }

func (b *claudeBackend) onMessage(msg map[string]json.RawMessage, raw json.RawMessage) {
	msgType := getString(msg, "type")
	switch msgType {
	case "control_request":
		request := msg["request"]
		if getFieldString(request, "subtype") != "can_use_tool" {
			b.s.publish("status", "", "", "", "control_request", "pending", raw)
			return
		}
		id := getString(msg, "request_id")
		if id == "" {
			return
		}
		b.mu.Lock()
		b.approvals[id] = getField(request, "input")
		b.mu.Unlock()
		toolID := getFieldString(request, "tool_use_id")
		b.s.publish("approval", toolID, toolID, id, getFieldString(request, "tool_name"), "pending", raw)
	case "stream_event":
		var ev struct {
			Event struct {
				Type         string `json:"type"`
				Index        int    `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			} `json:"event"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			return
		}
		blockID := strconv.Itoa(ev.Event.Index)
		switch ev.Event.Type {
		case "content_block_start":
			switch ev.Event.ContentBlock.Type {
			case "tool_use":
				b.s.publish("tool_call", blockID, ev.Event.ContentBlock.ID, "", ev.Event.ContentBlock.Name, "started", raw)
			case "thinking":
				b.s.publish("thinking", blockID, "", "", "", "started", raw)
			default:
				b.s.publish("content", blockID, "", "", "", "started", raw)
			}
		case "content_block_delta":
			switch ev.Event.Delta.Type {
			case "thinking_delta":
				b.s.publish("thinking", blockID, "", "", ev.Event.Delta.Thinking, "streaming", raw)
			case "text_delta":
				b.s.publish("content", blockID, "", "", ev.Event.Delta.Text, "streaming", raw)
			case "input_json_delta":
				b.s.publish("tool_call", blockID, "", "", ev.Event.Delta.PartialJSON, "streaming", raw)
			}
		case "content_block_stop":
			b.s.publish("status", blockID, "", "", "block_finished", "completed", raw)
		}
	case "assistant":
		// The complete assistant message includes text, thinking and
		// tool_use blocks. Keep it as an immutable snapshot in Raw.
		var value struct {
			Message struct {
				Content []struct {
					Type     string `json:"type"`
					ID       string `json:"id"`
					Name     string `json:"name"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &value) == nil {
			for _, part := range value.Message.Content {
				switch part.Type {
				case "tool_use":
					b.s.publish("tool_call", part.ID, part.ID, "", part.Name, "completed", raw)
				case "thinking":
					b.s.publish("thinking", "", "", "", part.Thinking, "completed", raw)
				case "text":
					b.s.publish("content", "", "", "", part.Text, "completed", raw)
				}
			}
		}
	case "user":
		// Tool results may appear in user-role tool_result content blocks.
		var value struct {
			Message struct {
				Content []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &value) == nil {
			for _, part := range value.Message.Content {
				if part.Type == "tool_result" {
					b.s.publish("tool_result", part.ToolUseID, part.ToolUseID, "", "", "completed", raw)
				}
			}
		}
	case "result":
		b.s.setBusy(false)
		b.s.publish("completed", "", "", "", "", "completed", raw)
	default:
		b.s.publish("status", "", "", "", msgType, "", raw)
	}
}
