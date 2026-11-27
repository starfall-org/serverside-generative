package chatsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/starfall-org/generative-server/internal/execution"
	"github.com/starfall-org/generative-server/internal/httptool"
)

// MCP requires an explicit session grant in addition to mobile tool and profile switches.
type mcpAccess struct {
	Enabled bool            `json:"enabled"`
	Tools   map[string]bool `json:"tools"`
}
type mcpRuntime struct {
	Source string `json:"source"`
	MCP    []struct {
		ID       string            `json:"id"`
		Protocol string            `json:"protocol"`
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		Auth     struct {
			Method string `json:"method"`
			Key    string `json:"key"`
			Value  string `json:"value"`
		} `json:"auth"`
	} `json:"mcp"`
	ToolGrants struct {
		MCP map[string]mcpAccess `json:"mcp"`
	} `json:"toolGrants"`
	ToolSettings struct {
		WebSearchProviderID *string                    `json:"webSearchProviderId"`
		WebSearchEnabled    bool                       `json:"webSearchEnabled"`
		QuickMCP            map[string]bool            `json:"quickMcp"`
		MCPTools            map[string]map[string]bool `json:"mcpTools"`
		Parameters          map[string]map[string]map[string]struct {
			Enabled *bool  `json:"enabled"`
			Default string `json:"defaultValue"`
		} `json:"mcpToolParameters"`
	} `json:"toolSettings"`
	Profile *struct {
		ID     string `json:"id"`
		Config struct {
			Access map[string]mcpAccess `json:"mcpAccess"`
		} `json:"config"`
	} `json:"profile"`
}

func mcpConfig(runtime, conversation json.RawMessage, server, tool string, allowLocal bool) (execution.State, mcpRuntime, error) {
	var r mcpRuntime
	if json.Unmarshal(runtime, &r) != nil || r.Source != "mobile_ephemeral" {
		return execution.State{}, r, ErrDenied
	}
	grant, ok := r.ToolGrants.MCP[server]
	if !ok || !grant.Enabled || !r.ToolSettings.QuickMCP[server] {
		return execution.State{}, r, ErrDenied
	}
	if r.ToolSettings.WebSearchProviderID != nil && *r.ToolSettings.WebSearchProviderID == server && !r.ToolSettings.WebSearchEnabled {
		return execution.State{}, r, ErrDenied
	}
	if tool != "" && (!grant.Tools[tool] || !r.ToolSettings.MCPTools[server][tool]) {
		return execution.State{}, r, ErrDenied
	}
	var chat struct {
		ProfileID *string `json:"profile_id"`
	}
	if json.Unmarshal(conversation, &chat) != nil {
		return execution.State{}, r, ErrInvalid
	}
	if chat.ProfileID != nil && *chat.ProfileID != "" {
		if r.Profile == nil || r.Profile.ID != *chat.ProfileID {
			return execution.State{}, r, ErrDenied
		}
	}
	if r.Profile != nil {
		access, ok := r.Profile.Config.Access[server]
		if !ok || !access.Enabled || (tool != "" && !access.Tools[tool]) {
			return execution.State{}, r, ErrDenied
		}
	}
	var cfg *execution.MCPServer
	for _, entry := range r.MCP {
		if entry.ID != server {
			continue
		}
		if cfg != nil {
			return execution.State{}, r, ErrInvalid
		}
		headers := map[string]string{}
		headerNames := map[string]bool{}
		for key, value := range entry.Headers {
			lower := strings.ToLower(key)
			if headerNames[lower] {
				return execution.State{}, r, ErrInvalid
			}
			headerNames[lower] = true
			headers[key] = value
		}
		endpoint, err := url.Parse(entry.URL)
		if err != nil {
			return execution.State{}, r, ErrInvalid
		}
		switch entry.Auth.Method {
		case "", "NONE":
		case "BEARER_TOKEN", "OAUTH2":
			if entry.Auth.Value == "" {
				return execution.State{}, r, ErrAuthRequired
			}
			if headerNames["authorization"] {
				return execution.State{}, r, ErrInvalid
			}
			headers["Authorization"] = "Bearer " + entry.Auth.Value
		case "CUSTOM_HEADER":
			if !execution.ValidAuthKey("custom_header", entry.Auth.Key) || entry.Auth.Value == "" {
				return execution.State{}, r, ErrInvalid
			}
			if headerNames[strings.ToLower(entry.Auth.Key)] {
				return execution.State{}, r, ErrInvalid
			}
			headers[entry.Auth.Key] = entry.Auth.Value
		case "QUERY_PARAM":
			if !execution.ValidAuthKey("query_param", entry.Auth.Key) || entry.Auth.Value == "" {
				return execution.State{}, r, ErrInvalid
			}
			q := endpoint.Query()
			q.Set(entry.Auth.Key, entry.Auth.Value)
			endpoint.RawQuery = q.Encode()
		default:
			return execution.State{}, r, ErrUnsupported
		}
		cfg = &execution.MCPServer{ID: entry.ID, URL: endpoint.String(), Protocol: entry.Protocol, Headers: headers}
	}
	if cfg == nil {
		return execution.State{}, r, ErrDenied
	}
	state := execution.State{MCP: []execution.MCPServer{*cfg}, HTTPClient: execution.SafeHTTPClient(allowLocal)}
	if err := state.ValidateMCP(server); err != nil {
		return execution.State{}, r, ErrInvalid
	}
	return state, r, nil
}
func allowedMCPTool(r mcpRuntime, server, name string) bool {
	g := r.ToolGrants.MCP[server]
	if !g.Enabled || !g.Tools[name] || !r.ToolSettings.MCPTools[server][name] {
		return false
	}
	if r.Profile != nil {
		a := r.Profile.Config.Access[server]
		if !a.Enabled || !a.Tools[name] {
			return false
		}
	}
	return true
}

// Caller holds m.mu so runtime and policy cannot change while resolving.
func (m *Manager) resolveMCPRuntime(s *session, server string) (json.RawMessage, error) {
	var source struct {
		Source string `json:"source"`
	}
	if json.Unmarshal(s.runtime, &source) != nil {
		return nil, ErrRuntime
	}
	if source.Source == "mobile_ephemeral" {
		return s.runtime, nil
	}
	if source.Source != "stored" || m.storedMCPResolver == nil {
		return nil, ErrUnsupported
	}
	raw, err := m.storedMCPResolver(s.runtime, s.conversation, server)
	if err != nil {
		return nil, storedRuntimeError(err)
	}
	if !validRuntime(raw) {
		return nil, ErrInvalid
	}
	return raw, nil
}
func (m *Manager) MCPTools(owner, id, server string) (json.RawMessage, error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	m.touch(id, s)
	runtime, err := m.resolveMCPRuntime(s, server)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	state, policy, err := mcpConfig(runtime, s.conversation, server, "", m.allowLocalAPI)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if len(s.discoveries) >= 64 {
		m.mu.Unlock()
		return nil, ErrLimit
	}
	secrets := runtimeSecrets(runtime)
	if hasSecret(owner, secrets) || hasSecret(id, secrets) || hasSecret(server, secrets) {
		m.mu.Unlock()
		return nil, ErrSecretLeak
	}
	runtimeHash := sha256.Sum256(s.runtime)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	if s.discoveries == nil {
		s.discoveries = map[uint64]context.CancelFunc{}
	}
	s.discoverySequence++
	jobID := s.discoverySequence
	s.discoveries[jobID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		if m.sessions[id] == s {
			delete(s.discoveries, jobID)
			m.touch(id, s)
		}
		m.mu.Unlock()
	}()
	raw, err := execution.ListTools(ctx, state, server)
	if err != nil {
		return nil, errors.New("MCP discovery failed")
	}
	m.mu.Lock()
	stillAuthorized := m.sessions[id] == s && sha256.Sum256(s.runtime) == runtimeHash
	m.mu.Unlock()
	if !stillAuthorized {
		return nil, ErrDenied
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil || hasSecret(decoded, secrets) {
		return nil, ErrSecretLeak
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, ErrInvalid
	}
	filtered := []map[string]json.RawMessage{}
	for _, entry := range tools {
		var name string
		if json.Unmarshal(entry["name"], &name) != nil {
			return nil, ErrInvalid
		}
		if allowedMCPTool(policy, server, name) {
			filtered = append(filtered, entry)
		}
	}
	return json.Marshal(filtered)
}

func (m *Manager) StartMCPTool(owner, id, requestID, server, name string, args json.RawMessage) error {
	if requestID == "" || len(requestID) > 128 || server == "" || name == "" || len(args) > 64<<10 {
		return ErrInvalid
	}
	var arguments map[string]any
	if json.Unmarshal(args, &arguments) != nil || arguments == nil {
		return ErrInvalid
	}
	signature := sha256.Sum256(append(append(append(append([]byte(server), 0), []byte(name)...), 0), args...))
	fingerprint := "mcp:" + hex.EncodeToString(signature[:])
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
	runtime, err := m.resolveMCPRuntime(s, server)
	if err != nil {
		return err
	}
	state, policy, err := mcpConfig(runtime, s.conversation, server, name, m.allowLocalAPI)
	if err != nil {
		return err
	}
	defaults := map[string]any{}
	for key, config := range policy.ToolSettings.Parameters[server][name] {
		if config.Enabled != nil && !*config.Enabled {
			delete(arguments, key)
			if config.Default != "" {
				var value any
				if json.Unmarshal([]byte(config.Default), &value) != nil {
					value = config.Default
				}
				defaults[key] = value
			}
		}
	}
	visible, err := json.Marshal(arguments)
	if err != nil {
		return ErrInvalid
	}
	for key, value := range defaults {
		arguments[key] = value
	}
	effective, err := json.Marshal(arguments)
	if err != nil {
		return ErrInvalid
	}
	secrets := runtimeSecrets(runtime)
	if hasSecret(owner, secrets) || hasSecret(id, secrets) || hasSecret(requestID, secrets) || hasSecret(name, secrets) || hasSecret(server, secrets) {
		return ErrSecretLeak
	}
	return m.startToolJobLocked(s, owner, id, requestID, name, name, name, visible, fingerprint, secrets, func(ctx context.Context) (any, error) {
		listing, err := execution.ListTools(ctx, state, server)
		if err != nil {
			return nil, errors.New("MCP discovery failed")
		}
		var tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if json.Unmarshal(listing, &tools) != nil {
			return nil, ErrInvalid
		}
		var schemaRaw json.RawMessage
		for _, tool := range tools {
			if tool.Name == name {
				if schemaRaw != nil {
					return nil, ErrInvalid
				}
				schemaRaw = tool.InputSchema
			}
		}
		if len(schemaRaw) == 0 {
			return nil, httptool.ErrUnsupported
		}
		var schema jsonschema.Schema
		if json.Unmarshal(schemaRaw, &schema) != nil {
			return nil, httptool.ErrUnsupported
		}
		resolved, err := schema.Resolve(nil)
		if err != nil || resolved.Validate(arguments) != nil {
			return nil, ErrInvalid
		}
		result, err := execution.CallTool(ctx, state, server, name, effective)
		if err != nil {
			return nil, errors.New("MCP call failed")
		}
		var decoded any
		if json.Unmarshal(result, &decoded) != nil {
			return nil, ErrInvalid
		}
		return decoded, nil
	})
}
