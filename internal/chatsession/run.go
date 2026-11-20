package chatsession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
	"github.com/starfall-org/serverside-generative/internal/execution"
)

var ErrRuntime = errors.New("missing runtime configuration")
var ErrAuthRequired = errors.New("authentication required")
var ErrUnsupported = errors.New("unsupported feature")
var ErrProvider = errors.New("unsupported provider adapter")
var ErrBusy = errors.New("session busy")
var ErrGap = errors.New("event gap")
var ErrReplay = errors.New("request already recorded")

type Event struct {
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	Data      any    `json:"data"`
}

func randomID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (m *Manager) emit(s *session, id, request, kind string, data any) {
	s.seq++
	event := Event{id, s.seq, request, kind, data}
	encoded, err := json.Marshal(event)
	if err != nil || len(encoded) > maxConversationBytes+(64<<10) {
		event.Kind = "event_gap"
		event.Data = map[string]any{"reason": "event_too_large"}
		encoded, _ = json.Marshal(event)
	}
	s.events = append(s.events, event)
	s.eventSizes = append(s.eventSizes, len(encoded))
	s.eventBytes += len(encoded)
	for len(s.events) > 2048 || s.eventBytes > 8<<20 {
		s.floor = s.events[0].Seq
		s.eventBytes -= s.eventSizes[0]
		s.events[0] = Event{}
		s.events = s.events[1:]
		s.eventSizes = s.eventSizes[1:]
	}
	for ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func opID(id, part string) string {
	hash := sha256.Sum256([]byte(id + "\x00" + part))
	return hex.EncodeToString(hash[:])
}

// emitConversation sends the current transcript; callers hold m.mu.
// Clients can recover it with session.get after an interrupted stream.
func (m *Manager) emitConversation(s *session, id, requestID string) {
	m.emit(s, id, requestID, "conversation", map[string]any{
		"conversation": json.RawMessage(s.conversation),
		"revision":     s.revision,
	})
}

func (m *Manager) Events(owner, id string, after uint64) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return nil, ErrNotFound
	}
	m.touch(id, s)
	if after < s.floor || after > s.seq {
		return nil, ErrGap
	}
	events := []Event{}
	size := 0
	for i, e := range s.events {
		if e.Seq > after {
			if len(events) > 0 && size+s.eventSizes[i] > 2<<20 {
				break
			}
			events = append(events, e)
			size += s.eventSizes[i]
		}
	}
	return events, nil
}
func (m *Manager) Subscribe(owner, id string) (<-chan struct{}, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return nil, nil, ErrNotFound
	}
	m.touch(id, s)
	if len(s.watchers) >= 64 {
		return nil, nil, ErrLimit
	}
	if s.watchers == nil {
		s.watchers = map[chan struct{}]struct{}{}
	}
	ch := make(chan struct{}, 1)
	s.watchers[ch] = struct{}{}
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := s.watchers[ch]; ok {
			delete(s.watchers, ch)
			close(ch)
		}
	}, nil
}

// Run supports text-only conversations with the existing provider adapters.
// Media and tools are explicitly refused until their execution is implemented.
func (m *Manager) Run(owner, id, requestID, content string) error {
	if requestID == "" || len(requestID) > 128 || content == "" || len(content) > 64<<10 {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return ErrNotFound
	}
	m.touch(id, s)
	contentDigest := sha256.Sum256([]byte(content))
	fingerprint := "run:" + hex.EncodeToString(contentDigest[:])
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
	var prior struct {
		Messages []struct {
			Versions []struct {
				RequestID string `json:"server_request_id"`
			} `json:"versions"`
		} `json:"messages"`
	}
	if json.Unmarshal(s.conversation, &prior) != nil {
		return ErrInvalid
	}
	for _, msg := range prior.Messages {
		for _, ver := range msg.Versions {
			if ver.RequestID == requestID {
				return ErrReplay
			}
		}
	}
	runtime := s.runtime
	var source struct {
		Source string `json:"source"`
	}
	if json.Unmarshal(runtime, &source) != nil {
		return ErrInvalid
	}
	if source.Source == "stored" {
		if m.storedResolver == nil {
			return ErrUnsupported
		}
		var err error
		runtime, err = m.storedResolver(runtime)
		if err != nil {
			return storedRuntimeError(err)
		}
	}
	secrets := runtimeSecrets(runtime)
	if len(secrets) > 256 {
		return ErrInvalid
	}
	for _, secret := range secrets {
		if len(secret) > 16<<10 {
			return ErrInvalid
		}
	}
	state, err := runtimeState(runtime, s.conversation, content)
	if err != nil {
		return err
	}
	if hasSecret(owner, secrets) || hasSecret(id, secrets) || hasSecret(requestID, secrets) || hasSecret(content, secrets) || hasSecret(state.Provider.ID, secrets) || hasSecret(state.Provider.Model, secrets) {
		return ErrSecretLeak
	}
	for _, message := range state.Conversation {
		if hasSecret(message.Content, secrets) {
			return ErrSecretLeak
		}
	}
	state.HTTPClient = execution.SafeHTTPClient(m.allowLocalAPI)
	state.HTTPClient = execution.WithWireAuth(state.HTTPClient, state.WireAuthMethod, state.WireAuthKey, state.WireAuthValue)
	if err = state.ValidateModel(); err != nil {
		return ErrInvalid
	}
	userID, err := randomID()
	if err != nil {
		return err
	}
	modelID, err := randomID()
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(s.conversation, &doc) != nil {
		return ErrInvalid
	}
	var msgs []json.RawMessage
	if v := doc["messages"]; len(v) > 0 && json.Unmarshal(v, &msgs) != nil {
		return ErrInvalid
	}
	now := time.Now().UTC()
	makeMsg := func(mid, role, text string, interrupted bool, output *runOutput) json.RawMessage {
		version := map[string]any{"content": text, "timestamp": strconv.FormatInt(now.UnixMilli(), 10), "files": []string{}, "toolActivity": []any{}, "generation_interrupted": interrupted, "provider_id": state.Provider.ID, "model_id": state.Provider.Model, "model_display_name": state.Provider.Model}
		if output != nil {
			output.applyToVersion(version)
		}
		v := map[string]any{"id": mid, "role": role, "versions": []any{version}, "active_version_index": 0}

		b, _ := json.Marshal(v)
		return b
	}
	msgs = append(msgs, makeMsg(userID, "user", content, false, nil), makeMsg(modelID, "model", "", true, nil))
	doc["messages"], _ = json.Marshal(msgs)
	// Persist the actual model used so another client reopening this chat will not
	// accidentally continue it with a different catalogue default.
	doc["provider_id"], _ = json.Marshal(state.Provider.ID)
	doc["model_id"], _ = json.Marshal(state.Provider.Model)
	doc["updated_at"], _ = json.Marshal(now.UnixMilli())
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
	snap, err := m.store.SaveRunCompared(id, owner, opID(requestID, "user"), s.revision, s.conversation, initial, requestID, "started")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.cancel = cancel
	m.emitConversation(s, id, requestID)
	go func() {
		defer lease()
		output := newRunOutput(modelID, secrets)
		rawBytes := 0
		buildResult := func(interrupted bool) (json.RawMessage, error) {
			var d map[string]json.RawMessage
			if json.Unmarshal(s.conversation, &d) != nil {
				return nil, ErrInvalid
			}
			var messages []json.RawMessage
			if json.Unmarshal(d["messages"], &messages) != nil || len(messages) == 0 {
				return nil, ErrInvalid
			}
			messages[len(messages)-1] = makeMsg(modelID, "model", "", interrupted, output)
			d["messages"], _ = json.Marshal(messages)
			d["updated_at"], _ = json.Marshal(time.Now().UnixMilli())
			return json.Marshal(d)
		}
		var flushErr error
		persisted := 0
		stopFlush := make(chan struct{})
		defer close(stopFlush)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopFlush:
					return
				case <-ticker.C:
					m.mu.Lock()
					if m.sessions[id] != s || !s.running || flushErr != nil {
						m.mu.Unlock()
						return
					}
					if output.changes != persisted {
						result, er := buildResult(true)
						if er == nil && len(result) > maxConversationBytes {
							er = ErrLimit
						}
						if er == nil {
							var snap chatstore.Snapshot
							snap, er = m.store.SaveCompared(id, owner,
								opID(requestID, "progress:"+strconv.FormatUint(s.revision, 10)),
								s.revision, s.conversation, result)
							if er == nil {
								s.conversation = clone(snap.Conversation)
								s.revision = snap.Revision
								persisted = output.changes
								m.emitConversation(s, id, requestID)
							}
						}
						if er != nil {
							flushErr = er
							cancel()
						}
					}
					m.mu.Unlock()
				}
			}
		}()
		emit := func(kind string, data any) { m.emit(s, id, requestID, kind, data) }
		streamErr := execution.StreamOutput(ctx, state, func(event execution.OutputEvent) {
			rawBytes += len(event.Text) + len(event.Tool.Arguments) + len(event.Tool.Result) + len(event.Tool.Name)
			if rawBytes > 1<<20 {
				cancel()
				return
			}
			m.mu.Lock()
			if m.sessions[id] == s && !output.leaked {
				output.apply(event, emit)
			}
			m.mu.Unlock()
			if output.leaked {
				cancel()
			}
		})
		m.mu.Lock()
		if m.sessions[id] == s {
			output.finish(emit)
		}
		m.mu.Unlock()
		if output.leaked {
			streamErr = errors.New("runtime secret in provider output")
		}

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
		if flushErr != nil {
			m.emit(s, id, requestID, "error", map[string]any{"code": "persistence_failed"})
			return
		}
		result, err := buildResult(streamErr != nil)
		if err != nil || len(result) > maxConversationBytes {
			m.emit(s, id, requestID, "error", map[string]any{"code": "resource_exhausted"})
			return
		}
		status := "completed"
		if streamErr != nil {
			status = "interrupted"
		}
		snap, err := m.store.SaveRunCompared(id, owner, opID(requestID, "result"),
			s.revision, s.conversation, result, requestID, status)
		if err != nil {
			m.emit(s, id, requestID, "error", map[string]any{"code": "persistence_failed"})
			return
		}
		s.conversation = clone(snap.Conversation)
		s.revision = snap.Revision
		m.emitConversation(s, id, requestID)
		if streamErr != nil {
			code := "remote_execution_failed"
			if output.leaked {
				code = "secret_leak_blocked"
			} else if state.WireAuthMethod == "oauth" && execution.Unauthorized(streamErr) {
				code = "auth_required"
			}
			m.emit(s, id, requestID, "error", map[string]any{"code": code})
		} else {
			m.emit(s, id, requestID, "completed", map[string]any{})
		}
	}()
	return nil
}

// ValidateStoredRuntime uses the same adapter/config validation as a text run,
// without performing network I/O or exposing resolved credentials.
func ValidateStoredRuntime(raw json.RawMessage) error {
	state, err := runtimeState(raw, json.RawMessage(`{"messages":[]}`), "probe")
	if err != nil {
		return err
	}
	return state.ValidateModel()
}

func runtimeState(raw, conversation json.RawMessage, content string) (execution.State, error) {
	var v struct {
		Source   string `json:"source"`
		Provider struct {
			ID          string          `json:"id"`
			Type        string          `json:"type"`
			BaseURL     string          `json:"baseUrl"`
			ModelID     string          `json:"modelId"`
			ModelConfig json.RawMessage `json:"modelConfig"`
			Config      json.RawMessage `json:"config"`
			Auth        struct {
				Method string `json:"method"`
				Key    string `json:"key"`
				Value  string `json:"value"`
			} `json:"auth"`
		} `json:"provider"`
		MCP          []json.RawMessage `json:"mcp"`
		CustomTools  []json.RawMessage `json:"customTools"`
		ToolSettings json.RawMessage   `json:"toolSettings"`
		Instructions map[string]string `json:"instructions"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Source != "mobile_ephemeral" {
		return execution.State{}, ErrUnsupported
	}
	if len(v.MCP) > 0 || len(v.CustomTools) > 0 {
		return execution.State{}, ErrUnsupported
	}
	if len(v.ToolSettings) > 0 && string(v.ToolSettings) != "null" {
		var settings any
		if json.Unmarshal(v.ToolSettings, &settings) != nil || enabled(settings) {
			return execution.State{}, ErrUnsupported
		}
	}
	temperature, topP, topK, err := modelConfig(v.Provider.ModelConfig)
	if err != nil {
		return execution.State{}, err
	}
	responsesAPI, maxTokens, err := providerConfig(v.Provider.Config, v.Provider.ModelID)
	if err != nil {
		return execution.State{}, err
	}
	platform := map[string]string{"openai": "openai-chat", "openai_responses": "openai-responses", "anthropic": "anthropic", "google": "gemini"}[v.Provider.Type]
	if responsesAPI {
		if platform != "openai-chat" && platform != "openai-responses" {
			return execution.State{}, ErrUnsupported
		}
		platform = "openai-responses"
	}

	if platform == "" {
		return execution.State{}, ErrProvider
	}
	if (platform == "openai-chat" || platform == "openai-responses") && topK != nil {
		return execution.State{}, ErrUnsupported
	}
	if platform == "anthropic" && (temperature != nil || topP != nil || topK != nil) {
		return execution.State{}, ErrUnsupported
	}
	method, key, secret := v.Provider.Auth.Method, v.Provider.Auth.Key, v.Provider.Auth.Value
	if method == "" {
		method = "platform_default"
	}
	if method == "platform_default" && secret == "" {
		secret = key
	}
	if method != "platform_default" && v.Provider.BaseURL != "" {
		u, err := url.Parse(v.Provider.BaseURL)
		if err != nil || u.Query().Has("key") {
			return execution.State{}, ErrUnsupported
		}
	}
	switch method {
	case "platform_default", "bearer_token", "oauth":
		if secret == "" {
			if method == "oauth" {
				return execution.State{}, ErrAuthRequired
			}
			return execution.State{}, ErrRuntime
		}
	case "none":
	case "custom_header", "query_param":
		if key == "" || secret == "" || !execution.ValidAuthKey(method, key) {
			return execution.State{}, ErrInvalid
		}
	default:
		return execution.State{}, ErrUnsupported
	}

	var c struct {
		Messages []struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			Versions []struct {
				Content      string            `json:"content"`
				Files        []string          `json:"files"`
				ToolActivity []json.RawMessage `json:"toolActivity"`
				Reasoning    string            `json:"reasoning_content"`
				Signature    string            `json:"reasoning_signature"`
			} `json:"versions"`
			Active int `json:"active_version_index"`
		} `json:"messages"`
		Summary *struct {
			Content string `json:"content"`
			Through string `json:"through_message_id"`
			Role    string `json:"role"`
		} `json:"summary"`
		Prompts []string `json:"selected_prompt_ids"`
	}
	if json.Unmarshal(conversation, &c) != nil {
		return execution.State{}, ErrInvalid
	}
	apiKey := secret
	if method == "none" || method == "custom_header" || method == "query_param" {
		apiKey = "unused"
	}
	state := execution.State{Provider: execution.Provider{ID: v.Provider.ID, Platform: platform, APIKey: apiKey, Model: v.Provider.ModelID, BaseURL: v.Provider.BaseURL, MaxTokens: maxTokens, Temperature: temperature, TopP: topP, TopK: topK}, WireAuthMethod: method, WireAuthKey: key, WireAuthValue: secret}
	if len(c.Prompts) > 32 || len(v.Instructions) > 32 {
		return execution.State{}, ErrInvalid
	}
	seenInstructions := map[string]bool{}
	for _, id := range c.Prompts {
		text, ok := v.Instructions[id]
		if !ok || id == "" || seenInstructions[id] || strings.TrimSpace(text) == "" || len(text) > 16<<10 {
			return execution.State{}, ErrUnsupported
		}
		seenInstructions[id] = true
		state.Conversation = append(state.Conversation, execution.Message{Role: "system", Content: text})
	}
	start := 0
	if c.Summary != nil {
		found := false
		for i, msg := range c.Messages {
			if msg.ID == c.Summary.Through {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return execution.State{}, ErrInvalid
		}
		role := strings.ToLower(c.Summary.Role)
		if role == "" {
			role = "system"
		}
		text := c.Summary.Content
		switch role {
		case "system":
			text = "Conversation summary:\n" + text
		case "assistant", "user":
		default:
			return execution.State{}, ErrInvalid
		}
		state.Conversation = append(state.Conversation, execution.Message{Role: role, Content: text})
	}
	for _, msg := range c.Messages[start:] {
		if len(msg.Versions) == 0 {
			continue
		}
		if msg.Active < 0 || msg.Active >= len(msg.Versions) {
			return execution.State{}, ErrInvalid
		}
		ver := msg.Versions[msg.Active]
		if len(ver.Files) > 0 || ver.Signature != "" {
			return execution.State{}, ErrUnsupported
		}
		// Preserve thinking and tool records in history. The text runner uses
		// the answer in subsequent turns and never replays recorded actions.
		if msg.Role == "model" && ver.Content == "" && (len(ver.ToolActivity) > 0 || ver.Reasoning != "") {
			continue
		}
		role := msg.Role
		if role == "model" {
			role = "assistant"
		}
		state.Conversation = append(state.Conversation, execution.Message{Role: role, Content: ver.Content})
	}
	state.Conversation = append(state.Conversation, execution.Message{Role: "user", Content: content})
	return state, nil
}

func enabled(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case []any:
		for _, x := range v {
			if enabled(x) {
				return true
			}
		}
	case map[string]any:
		for _, x := range v {
			if enabled(x) {
				return true
			}
		}
	}
	return false
}

func modelConfig(raw json.RawMessage) (*float64, *float64, *int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil, nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, nil, nil, ErrInvalid
	}
	var temperature, topP *float64
	var topK *int
	for k, v := range values {
		switch k {
		case "temperature":
			if string(v) != "null" && json.Unmarshal(v, &temperature) != nil {
				return nil, nil, nil, ErrInvalid
			}
		case "top_p":
			if string(v) != "null" && json.Unmarshal(v, &topP) != nil {
				return nil, nil, nil, ErrInvalid
			}
		case "top_k":
			if string(v) != "null" && json.Unmarshal(v, &topK) != nil {
				return nil, nil, nil, ErrInvalid
			}
		case "displayName", "contextWindowTokens", "supportsVision", "supportsVideoInput", "supportsAudioInput", "supportsThinking", "supportsToolCalls", "model_tools", "reasoning_efforts", "icon", "mediaApiType", "mediaRequestProfileId":
		case "modelType":
			var typ string
			if json.Unmarshal(v, &typ) != nil || typ != "TEXT_GENERATION" {
				return nil, nil, nil, ErrUnsupported
			}
		case "supportStream":
			var value *bool
			if json.Unmarshal(v, &value) != nil || (value != nil && !*value) {
				return nil, nil, nil, ErrUnsupported
			}
		case "reasoning_effort", "thinking_level", "send_thinking_content", "thinking_passback":
			if string(v) != "null" {
				return nil, nil, nil, ErrUnsupported
			}
		case "enabled_model_tools":
			var a []json.RawMessage
			if json.Unmarshal(v, &a) != nil || len(a) > 0 {
				return nil, nil, nil, ErrUnsupported
			}
		case "model_json":
			var m map[string]json.RawMessage
			if json.Unmarshal(v, &m) != nil || len(m) > 0 {
				return nil, nil, nil, ErrUnsupported
			}
		default:
			return nil, nil, nil, ErrUnsupported
		}
	}
	return temperature, topP, topK, nil
}

func providerConfig(raw json.RawMessage, modelID string) (bool, int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, 0, nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false, 0, ErrInvalid
	}
	responses := false
	var max int64
	for k, v := range values {
		switch k {
		case "supportStream":
			var enabled bool
			if json.Unmarshal(v, &enabled) != nil || !enabled {
				return false, 0, ErrUnsupported
			}
		case "responsesApi":
			if json.Unmarshal(v, &responses) != nil {
				return false, 0, ErrInvalid
			}
		case "maxTokens":
			if json.Unmarshal(v, &max) != nil || max < 0 || max > 131072 {
				return false, 0, ErrInvalid
			}
		case "modelIds":
			if string(v) != "null" {
				var ids []string
				if json.Unmarshal(v, &ids) != nil || ids == nil {
					return false, 0, ErrInvalid
				}
				if !slices.Contains(ids, modelID) {
					return false, 0, ErrUnsupported
				}
			}
		case "httpProxy", "socksProxy", "headers", "customEndpoints", "modelConfigs", "apiKeys", "oauthAccounts", "antigravityProjectIds", "geminiCliProjectIds":
			var x any
			if json.Unmarshal(v, &x) != nil {
				return false, 0, ErrInvalid
			}
			switch x := x.(type) {
			case nil:
			case map[string]any:
				if len(x) > 0 {
					return false, 0, ErrUnsupported
				}
			case []any:
				if len(x) > 0 {
					return false, 0, ErrUnsupported
				}
			default:
				return false, 0, ErrInvalid
			}
		case "multipleApiKeys":
			var enabled bool
			if json.Unmarshal(v, &enabled) != nil || enabled {
				return false, 0, ErrUnsupported
			}
		case "customListModelsUrl", "codexClientVersion", "antigravityVersion", "antigravityChangelist":
			var x *string
			if json.Unmarshal(v, &x) != nil || (x != nil && *x != "") {
				return false, 0, ErrUnsupported
			}
		case "anthropicPromptCache":
			var x map[string]any
			if json.Unmarshal(v, &x) != nil || len(x) > 0 {
				return false, 0, ErrUnsupported
			}
		default:
			return false, 0, ErrUnsupported
		}
	}
	return responses, max, nil
}
