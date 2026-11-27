package chatsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/starfall-org/generative-server/internal/chatstore"
	"github.com/starfall-org/generative-server/internal/httptool"
)

var ErrDenied = errors.New("tool permission denied")
var ErrSecretLeak = errors.New("runtime secret in tool output")

type customRuntime struct {
	Source      string            `json:"source"`
	CustomTools []json.RawMessage `json:"customTools"`
	ToolGrants  struct {
		CustomHTTP map[string]bool `json:"custom_http"`
	} `json:"toolGrants"`
}

// Caller holds m.mu so the grants cannot change while loading stored tools.
func (m *Manager) resolveCustomRuntime(s *session) (json.RawMessage, error) {
	var source struct {
		Source string `json:"source"`
	}
	if json.Unmarshal(s.runtime, &source) != nil {
		return nil, ErrRuntime
	}
	if source.Source == "mobile_ephemeral" {
		return s.runtime, nil
	}
	if source.Source != "stored" || m.storedToolResolver == nil {
		return nil, ErrUnsupported
	}
	raw, err := m.storedToolResolver(s.runtime)
	if err != nil {
		return nil, storedRuntimeError(err)
	}
	if !validRuntime(raw) {
		return nil, ErrInvalid
	}
	return raw, nil
}

func customTool(runtime json.RawMessage, id string) (json.RawMessage, string, error) {
	var r customRuntime
	if json.Unmarshal(runtime, &r) != nil || r.Source != "mobile_ephemeral" || !r.ToolGrants.CustomHTTP[id] {
		return nil, "", ErrDenied
	}
	var found json.RawMessage
	var name string
	for _, raw := range r.CustomTools {
		var entry struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return nil, "", ErrInvalid
		}
		if entry.ID == id {
			if found != nil || !entry.Enabled {
				return nil, "", ErrDenied
			}
			if _, err := httptool.Validate(raw); err != nil {
				return nil, "", ErrInvalid
			}
			found = clone(raw)
			name = entry.Name
		}
	}
	if found == nil {
		return nil, "", ErrDenied
	}
	return found, name, nil
}
func (m *Manager) CustomTools(owner, id string) ([]map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return nil, ErrNotFound
	}
	m.touch(id, s)
	runtime, err := m.resolveCustomRuntime(s)
	if err != nil {
		return nil, err
	}
	var r customRuntime
	if json.Unmarshal(runtime, &r) != nil || r.Source != "mobile_ephemeral" {
		return nil, ErrInvalid
	}
	tools := []map[string]any{}
	secrets := runtimeSecrets(runtime)
	if hasSecret(owner, secrets) || hasSecret(id, secrets) {
		return nil, ErrSecretLeak
	}
	seen := map[string]bool{}
	for _, raw := range r.CustomTools {
		var entry struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if json.Unmarshal(raw, &entry) != nil || seen[entry.ID] {
			return nil, ErrInvalid
		}
		seen[entry.ID] = true
		if entry.Enabled && r.ToolGrants.CustomHTTP[entry.ID] {
			if hasSecret(entry.ID, secrets) || hasSecret(entry.Name, secrets) {
				return nil, ErrSecretLeak
			}
			if _, err := httptool.Validate(raw); err != nil {
				return nil, ErrInvalid
			}
			tools = append(tools, map[string]any{"id": entry.ID, "name": entry.Name, "execution_supported": true})
		}
	}
	return tools, nil
}

// StartCustomTool performs one explicitly granted call. A durable receipt is
// recorded before any network request when the chat is synced.
func (m *Manager) StartCustomTool(owner, id, requestID, toolID string, args json.RawMessage) error {
	if requestID == "" || len(requestID) > 128 || toolID == "" || len(args) > 64<<10 {
		return ErrInvalid
	}
	var arguments map[string]any
	if json.Unmarshal(args, &arguments) != nil || arguments == nil {
		return ErrInvalid
	}
	signature := sha256.Sum256(append(append([]byte(toolID), 0), args...))
	fingerprint := "tool:" + hex.EncodeToString(signature[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return ErrNotFound
	}
	m.touch(id, s)
	if prior, ok := s.started[requestID]; ok {
		if prior == fingerprint {
			return nil
		}
		return ErrReplay
	}
	if s.recorded[requestID] {
		return ErrReplay
	}
	if s.running {
		return ErrBusy
	}
	if len(s.started) >= 1024 {
		return ErrLimit
	}
	if len(s.runtime) == 0 {
		return ErrRuntime
	}
	args = clone(args)
	runtime, err := m.resolveCustomRuntime(s)
	if err != nil {
		return err
	}
	raw, name, err := customTool(runtime, toolID)
	if err != nil {
		return err
	}
	allowLocal := m.allowLocalAPI
	secrets := runtimeSecrets(runtime)
	if hasSecret(owner, secrets) || hasSecret(id, secrets) || hasSecret(requestID, secrets) || hasSecret(name, secrets) || hasSecret(toolID, secrets) {
		return ErrSecretLeak
	}
	return m.startToolJobLocked(s, owner, id, requestID, toolID, name, "http_"+name, args, fingerprint, secrets, func(ctx context.Context) (any, error) {
		return httptool.Execute(ctx, raw, args, httptool.Policy{Allowed: true, AllowLoopback: allowLocal})
	})
}

// startToolJobLocked records a receipt before invoking any granted tool.
func (m *Manager) startToolJobLocked(s *session, owner, id, requestID, toolID, name, wireName string, args json.RawMessage, fingerprint string, secrets []string, invoke func(context.Context) (any, error)) error {
	if s.recorded[requestID] {
		return ErrReplay
	}
	var visibleArgs any
	if json.Unmarshal(args, &visibleArgs) != nil {
		return ErrInvalid
	}
	if hasSecret(visibleArgs, secrets) {
		return ErrSecretLeak
	}
	messageID, err := randomID()
	if err != nil {
		return err
	}
	activityID, err := randomID()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	makeMessage := func(status, response string) json.RawMessage {
		activity := map[string]any{"id": activityID, "name": name, "wireName": wireName, "status": status, "summary": "", "arguments": string(args), "response": response, "files": []string{}, "contentOffset": 0}
		version := map[string]any{"content": "", "timestamp": fmt.Sprint(now), "files": []string{}, "toolActivity": []any{activity}, "generation_interrupted": status == "running"}
		b, _ := json.Marshal(map[string]any{"id": messageID, "role": "model", "versions": []any{version}, "active_version_index": 0})
		return b
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(s.conversation, &doc) != nil {
		return ErrInvalid
	}
	var messages []json.RawMessage
	if v := doc["messages"]; len(v) > 0 && json.Unmarshal(v, &messages) != nil {
		return ErrInvalid
	}
	messages = append(messages, makeMessage("running", ""))
	doc["messages"], _ = json.Marshal(messages)
	doc["updated_at"], _ = json.Marshal(now)
	initial, err := json.Marshal(doc)
	if err != nil {
		return ErrInvalid
	}
	if len(initial) > maxConversationBytes {
		return ErrLimit
	}
	lease, err := m.store.TryJobLease(id)
	if errors.Is(err, chatstore.ErrJobBusy) {
		return ErrBusy
	}
	if err != nil {
		return err
	}
	snap, err := m.store.SaveRunCompared(id, owner, opID(requestID, "tool-start"),
		s.revision, s.conversation, initial, requestID, "started")
	if err != nil {
		lease()
		return err
	}
	s.revision = snap.Revision
	if s.recorded == nil {
		s.recorded = map[string]bool{}
	}
	s.recorded[requestID] = true
	s.conversation = initial
	if s.started == nil {
		s.started = map[string]string{}
	}
	s.started[requestID] = fingerprint
	s.running = true
	s.activeRequest = requestID
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	s.cancel = cancel
	m.emitConversation(s, id, requestID)
	go func() {
		defer lease()
		result, toolErr := invoke(ctx)
		cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.sessions[id] != s {
			return
		}
		s.running = false
		s.activeRequest = ""
		s.cancel = nil
		m.touch(id, s)
		if toolErr == nil {
			b, e := json.Marshal(result)
			var decoded any
			if e != nil || json.Unmarshal(b, &decoded) != nil {
				toolErr = ErrInvalid
			} else if hasSecret(decoded, secrets) {
				toolErr = ErrSecretLeak
			}
		}
		status, response, receipt := "success", "", "completed"
		if toolErr != nil {
			status = "error"
			response = "tool_execution_failed"
			receipt = "interrupted"
		} else {
			b, err := json.Marshal(result)
			if err != nil {
				status = "error"
				response = "tool_execution_failed"
				receipt = "interrupted"
			} else {
				response = string(b)
			}
		}
		var d map[string]json.RawMessage
		var msgs []json.RawMessage
		if json.Unmarshal(s.conversation, &d) != nil || json.Unmarshal(d["messages"], &msgs) != nil || len(msgs) == 0 {
			m.emit(s, id, requestID, "error", map[string]any{"code": "internal_error"})
			return
		}
		msgs[len(msgs)-1] = makeMessage(status, response)
		d["messages"], _ = json.Marshal(msgs)
		d["updated_at"], _ = json.Marshal(time.Now().UnixMilli())
		updated, err := json.Marshal(d)
		if err != nil {
			m.emit(s, id, requestID, "error", map[string]any{"code": "persistence_failed"})
			return
		}
		if len(updated) > maxConversationBytes {
			m.emit(s, id, requestID, "error", map[string]any{"code": "resource_exhausted"})
			return
		}
		snap, err := m.store.SaveRunCompared(id, owner, opID(requestID, "tool-result"),
			s.revision, s.conversation, updated, requestID, receipt)
		if err != nil {
			m.emit(s, id, requestID, "error", map[string]any{"code": "persistence_failed"})
			return
		}
		s.conversation = clone(snap.Conversation)
		s.revision = snap.Revision
		m.emitConversation(s, id, requestID)
		if status == "error" {
			code := "tool_execution_failed"
			if errors.Is(toolErr, httptool.ErrUnsupported) {
				code = "unsupported_feature"
			}
			if errors.Is(toolErr, httptool.ErrDenied) {
				code = "permission_denied"
			}
			if errors.Is(toolErr, ErrSecretLeak) || errors.Is(toolErr, httptool.ErrSecretLeak) {
				code = "secret_leak_blocked"
			}
			m.emit(s, id, requestID, "error", map[string]any{"code": code})
		} else {
			m.emit(s, id, requestID, "tool_result", map[string]any{"tool_id": toolID, "result": result})
			m.emit(s, id, requestID, "completed", map[string]any{})
		}
	}()
	return nil
}
