package agent

import (
	"encoding/json"
	"strings"
)

// OpenCode v2 emits typed SSE envelopes with a "data" payload, unlike the
// legacy "properties" envelopes. We preserve each original envelope in Raw.
func (b *openCodeBackend) onV2Event(raw json.RawMessage) {
	var event struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	data := event.Data
	sessionID := getFieldString(data, "sessionID")
	if sessionID == "" {
		sessionID = getFieldString(getField(data, "session"), "id")
	}
	b.mu.Lock()
	own := b.sessionID
	b.mu.Unlock()
	if sessionID != "" && sessionID != own {
		return
	}

	// Native identifiers are not invented. Missing IDs remain empty and
	// clients can inspect the original JSON if their fork adds new fields.
	id := getFieldString(data, "id")
	if id == "" {
		id = getFieldString(data, "partID")
	}
	if id == "" {
		id = getFieldString(data, "messageID")
	}
	callID := getFieldString(data, "callID")
	if callID == "" {
		callID = getFieldString(data, "toolCallID")
	}
	delta := getFieldString(data, "delta")
	if delta == "" {
		delta = getFieldString(data, "text")
	}

	switch event.Type {
	case "session.text.started":
		b.s.publish("content", id, "", "", "", "started", raw)
	case "session.text.delta":
		b.s.publish("content", id, "", "", delta, "streaming", raw)
	case "session.text.ended":
		b.s.publish("content", id, "", "", "", "completed", raw)
	case "session.reasoning.started":
		b.s.publish("thinking", id, "", "", "", "started", raw)
	case "session.reasoning.delta":
		b.s.publish("thinking", id, "", "", delta, "streaming", raw)
	case "session.reasoning.ended":
		b.s.publish("thinking", id, "", "", "", "completed", raw)
	case "session.tool.input.started", "session.tool.called":
		b.s.publish("tool_call", id, callID, "", getFieldString(data, "tool"), "started", raw)
	case "session.tool.input.delta", "session.tool.progress":
		b.s.publish("tool_call", id, callID, "", delta, "streaming", raw)
	case "session.tool.input.ended":
		b.s.publish("tool_call", id, callID, "", "", "completed", raw)
	case "session.tool.success":
		b.s.publish("tool_result", id, callID, "", "", "completed", raw)
	case "session.tool.failed":
		b.s.publish("tool_result", id, callID, "", "", "failed", raw)
	case "permission.asked":
		permission := getFieldString(data, "id")
		b.mu.Lock()
		if permission != "" {
			b.approvals[permission] = true
		}
		b.mu.Unlock()
		b.s.publish("approval", id, callID, permission, "", "pending", raw)
	case "permission.replied":
		permission := getFieldString(data, "id")
		b.mu.Lock()
		delete(b.approvals, permission)
		b.mu.Unlock()
		b.s.publish("approval", id, callID, permission, "", "completed", raw)
	case "session.execution.started", "session.step.started":
		b.s.setBusy(true)
		b.s.publish("status", id, "", "", event.Type, "running", raw)
	case "session.execution.succeeded":
		b.s.setBusy(false)
		b.s.publish("completed", "", "", "", "", "completed", raw)
	case "session.execution.failed", "session.execution.interrupted":
		b.s.setBusy(false)
		b.s.publish("error", "", "", "", event.Type, "failed", raw)
	case "session.status", "session.idle":
		if event.Type == "session.idle" || getFieldString(data, "type") == "idle" {
			b.s.setBusy(false)
		}
		b.s.publish("status", id, "", "", event.Type, "", raw)
	default:
		if event.Type != "" && !strings.HasPrefix(event.Type, "server.") {
			b.s.publish("status", id, "", "", event.Type, "", raw)
		}
	}
}
