package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

var ErrMobileMCP = errors.New("invalid mobile MCP definition")
var ErrMobileChatProfile = errors.New("invalid mobile chat profile")

// MobileMCP retains exact Kotlin McpInfo JSON, including null cachedTools,
// [] cachedTools and unknown fields; it never executes the remote MCP server.
type MobileMCP struct{ raw json.RawMessage }

func (m MobileMCP) Bytes() json.RawMessage { return append(json.RawMessage(nil), m.raw...) }
func (m MobileMCP) MarshalJSON() ([]byte, error) {
	if len(m.raw) == 0 {
		return nil, ErrMobileMCP
	}
	return m.Bytes(), nil
}

type MCPFields struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Protocol    string            `json:"protocol"`
	URL         *string           `json:"url"`
	Headers     map[string]string `json:"headers"`
	Auth        MCPAuthFields     `json:"auth"`
	CachedTools json.RawMessage   `json:"cachedTools"`
	SortOrder   int               `json:"sortOrder"`
	Icon        *string           `json:"icon"`
}
type MCPAuthFields struct {
	Method            string  `json:"method"`
	Key               *string `json:"key"`
	Value             *string `json:"value"`
	OAuthClientID     *string `json:"oauthClientId"`
	OAuthClientSecret *string `json:"oauthClientSecret"`
	OAuthRedirectURI  *string `json:"oauthRedirectUri"`
	OAuthAuthorized   bool    `json:"oauthAuthorized"`
}

func ParseMobileMCP(raw []byte) (MobileMCP, error) {
	if err := mobileUnique(raw); err != nil {
		return MobileMCP{}, ErrMobileMCP
	}
	if _, err := mobileMCPFields(raw); err != nil {
		return MobileMCP{}, err
	}
	return MobileMCP{append(json.RawMessage(nil), raw...)}, nil
}
func (m MobileMCP) Fields() (MCPFields, error) { return mobileMCPFields(m.raw) }

func mobileMCPFields(raw []byte) (MCPFields, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil || !mediaPresent(obj, "id", "name") ||
		!mediaNonNull(obj, "protocol", "sortOrder", "auth") {
		return MCPFields{}, ErrMobileMCP
	}
	f := MCPFields{Protocol: "streamable_http", SortOrder: 2147483647, Auth: MCPAuthFields{Method: "NONE"}}
	if json.Unmarshal(raw, &f) != nil || strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Name) == "" ||
		len(f.ID) > 128 || !mediaIs(f.Protocol, "streamable_http", "sse") ||
		!mediaIs(f.Auth.Method, "NONE", "BEARER_TOKEN", "QUERY_PARAM", "CUSTOM_HEADER", "OAUTH2") {
		return MCPFields{}, ErrMobileMCP
	}
	if f.URL != nil && *f.URL != "" {
		u, err := url.Parse(*f.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil ||
			u.Fragment != "" {
			return MCPFields{}, ErrMobileMCP
		}
	}
	if a, ok := obj["auth"]; ok {
		var fields map[string]json.RawMessage
		if json.Unmarshal(a, &fields) != nil || fields == nil ||
			!mediaNonNull(fields, "method", "oauthAuthorized") {
			return MCPFields{}, ErrMobileMCP
		}
	}
	if rawTools, ok := obj["cachedTools"]; ok && string(rawTools) != "null" {
		var tools []json.RawMessage
		if json.Unmarshal(rawTools, &tools) != nil || tools == nil {
			return MCPFields{}, ErrMobileMCP
		}
		for _, rawTool := range tools {
			var t map[string]json.RawMessage
			if json.Unmarshal(rawTool, &t) != nil || t == nil || !mediaPresent(t, "name", "description", "schema") {
				return MCPFields{}, ErrMobileMCP
			}
			var schema map[string]json.RawMessage
			if json.Unmarshal(t["schema"], &schema) != nil || schema == nil {
				return MCPFields{}, ErrMobileMCP
			}
			var name, description string
			if json.Unmarshal(t["name"], &name) != nil || json.Unmarshal(t["description"], &description) != nil || name == "" {
				return MCPFields{}, ErrMobileMCP
			}
			if !mediaNonNull(t, "originalName") {
				return MCPFields{}, ErrMobileMCP
			}
		}
	}
	return f, nil
}

// StoredMetadataJSON is a secret-free projection, not a lossless mobile export.
// Credential values, OAuth secrets, and arbitrary sensitive HTTP headers must
// be put into encrypted storage or supplied at runtime instead of in YAML.
func (m MobileMCP) StoredMetadataJSON() ([]byte, error) {
	f, err := m.Fields()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(m.raw))
	dec.UseNumber()
	var root map[string]any
	if dec.Decode(&root) != nil || root == nil {
		return nil, ErrMobileMCP
	}
	// The auth.method is metadata; auth.value and OAuth client material are not.
	auth := map[string]any{"method": f.Auth.Method}
	if f.Auth.Method == "QUERY_PARAM" || f.Auth.Method == "CUSTOM_HEADER" {
		if f.Auth.Key != nil && *f.Auth.Key != "" {
			key := *f.Auth.Key
			if !validMobileAuthKey(key) {
				return nil, ErrMobileMCP
			}
			auth["key"] = key // validated parameter/header *name*, not value
		}
	}
	root["auth"] = auth
	// Known benign headers may be retained; unknown headers may be secrets.
	if f.Headers != nil {
		safe := map[string]string{}
		for key, val := range f.Headers {
			switch strings.ToLower(key) {
			case "accept", "content-type", "user-agent":
				safe[key] = val
			default:
				if val != "" {
					return nil, ErrMobileMCP
				}
			}
		}
		root["headers"] = safe
	}
	// The sanitized auth descriptor is explicitly allowlisted above. Scan
	// everything else, then restore auth, to avoid mistaking a safe "auth"
	// metadata key for the actual secret value that was removed.
	delete(root, "auth")
	if mediaPrivate(root, "") {
		return nil, ErrMobileMCP
	}
	root["auth"] = auth
	return json.Marshal(root)
}

// MobileChatProfile similarly preserves ChatProfile's Kotlin names, including
// config.system_prompt, active_mcp, active_model_tools and unknown extensions.
type MobileChatProfile struct{ raw json.RawMessage }

func (p MobileChatProfile) Bytes() json.RawMessage { return append(json.RawMessage(nil), p.raw...) }
func (p MobileChatProfile) MarshalJSON() ([]byte, error) {
	if len(p.raw) == 0 {
		return nil, ErrMobileChatProfile
	}
	return p.Bytes(), nil
}

type ChatProfileFields struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Icon             *string           `json:"icon"`
	Config           ChatProfileConfig `json:"config"`
	SortOrder        int               `json:"sortOrder"`
	ActiveMCP        []string          `json:"active_mcp"`
	ActiveModelTools []string          `json:"active_model_tools"`
}
type ChatProfileConfig struct {
	SystemPrompt string                     `json:"system_prompt"`
	MCPAccess    map[string]MCPAccessFields `json:"mcpAccess"`
}
type MCPAccessFields struct {
	Enabled bool            `json:"enabled"`
	Tools   map[string]bool `json:"tools"`
}

func ParseMobileChatProfile(raw []byte) (MobileChatProfile, error) {
	if err := mobileUnique(raw); err != nil {
		return MobileChatProfile{}, ErrMobileChatProfile
	}
	if _, err := mobileChatFields(raw); err != nil {
		return MobileChatProfile{}, err
	}
	return MobileChatProfile{append(json.RawMessage(nil), raw...)}, nil
}
func (p MobileChatProfile) Fields() (ChatProfileFields, error) { return mobileChatFields(p.raw) }
func mobileChatFields(raw []byte) (ChatProfileFields, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil || !mediaPresent(root, "id", "name") ||
		!mediaNonNull(root, "config", "sortOrder", "active_mcp", "active_model_tools") {
		return ChatProfileFields{}, ErrMobileChatProfile
	}
	f := ChatProfileFields{
		SortOrder: 2147483647, ActiveMCP: []string{}, ActiveModelTools: []string{},
		Config: ChatProfileConfig{MCPAccess: map[string]MCPAccessFields{}},
	}
	if cr, ok := root["config"]; ok {
		var cm map[string]json.RawMessage
		if json.Unmarshal(cr, &cm) != nil || cm == nil || !mediaNonNull(cm, "system_prompt", "mcpAccess") {
			return ChatProfileFields{}, ErrMobileChatProfile
		}
		if ar, ok := cm["mcpAccess"]; ok {
			var entries map[string]json.RawMessage
			if json.Unmarshal(ar, &entries) != nil || entries == nil {
				return ChatProfileFields{}, ErrMobileChatProfile
			}
			for key, rawEntry := range entries {
				var access map[string]json.RawMessage
				if key == "" || json.Unmarshal(rawEntry, &access) != nil || access == nil ||
					!mediaNonNull(access, "enabled", "tools") {
					return ChatProfileFields{}, ErrMobileChatProfile
				}
				if tr, ok := access["tools"]; ok {
					var tools map[string]bool
					if json.Unmarshal(tr, &tools) != nil || tools == nil {
						return ChatProfileFields{}, ErrMobileChatProfile
					}
				}
			}
		}
	}
	for _, key := range []string{"active_mcp", "active_model_tools"} {
		if v, ok := root[key]; ok {
			var rawEntries []json.RawMessage
			if json.Unmarshal(v, &rawEntries) != nil || rawEntries == nil {
				return ChatProfileFields{}, ErrMobileChatProfile
			}
			for _, entry := range rawEntries {
				var text string
				if string(entry) == "null" || json.Unmarshal(entry, &text) != nil {
					return ChatProfileFields{}, ErrMobileChatProfile
				}
			}
		}
	}
	if json.Unmarshal(raw, &f) != nil || strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Name) == "" ||
		f.ActiveMCP == nil || f.ActiveModelTools == nil || f.Config.MCPAccess == nil {
		return ChatProfileFields{}, ErrMobileChatProfile
	}
	for id, access := range f.Config.MCPAccess {
		if access.Tools == nil {
			access.Tools = map[string]bool{}
			f.Config.MCPAccess[id] = access
		}
	}
	return f, nil
}

func mobileUnique(raw []byte) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return ErrMobileMCP
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := mediaWalk(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrMobileMCP
	}
	return nil
}
