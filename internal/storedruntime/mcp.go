package storedruntime

import (
	"encoding/json"
	"strings"

	"github.com/starfall-org/generative-server/internal/catalog"
	"github.com/starfall-org/generative-server/internal/secrets"
	"go.yaml.in/yaml/v4"
)

// ResolveMCP loads only the explicitly named stored server and its referenced
// credential. Tool switches come from the stored catalog; grants remain explicit
// per session. No mobile-supplied connection configuration is used as fallback.
func ResolveMCP(dir string, runtime, conversation json.RawMessage, serverID string) (json.RawMessage, error) {
	var request struct {
		Source     string          `json:"source"`
		ToolGrants json.RawMessage `json:"toolGrants"`
	}
	if json.Unmarshal(runtime, &request) != nil || request.Source != "stored" || serverID == "" {
		return nil, ErrUnsupported
	}
	var grants any
	if len(request.ToolGrants) == 0 || json.Unmarshal(request.ToolGrants, &grants) != nil {
		return nil, ErrAuth
	}
	bundle, err := catalog.New(dir).Preview()
	if err != nil {
		return nil, err
	}
	var servers struct {
		Servers []struct {
			ID            string `yaml:"id"`
			Protocol      string `yaml:"protocol"`
			URL           string `yaml:"url"`
			ConnectionRef string `yaml:"connection_ref"`
			Auth          struct {
				Method    string `yaml:"method"`
				Header    string `yaml:"header_name"`
				Parameter string `yaml:"parameter_name"`
				Ref       string `yaml:"credential_ref"`
			} `yaml:"auth"`
		} `yaml:"servers"`
	}
	if yaml.Unmarshal([]byte(bundle.Files["mcp.yaml"]), &servers) != nil {
		return nil, ErrUnsupported
	}
	var entry map[string]any
	for _, s := range servers.Servers {
		if s.ID != serverID {
			continue
		}
		if entry != nil {
			return nil, ErrUnsupported
		}
		auth := map[string]any{"method": s.Auth.Method}
		switch s.Auth.Method {
		case "", "NONE":
		case "BEARER_TOKEN", "CUSTOM_HEADER", "QUERY_PARAM":
			value, e := storedMCPCredential(dir, serverID, s.Auth.Ref)
			if e != nil {
				return nil, e
			}
			auth["value"] = string(value)
			if s.Auth.Method == "CUSTOM_HEADER" {
				auth["key"] = s.Auth.Header
			} else if s.Auth.Method == "QUERY_PARAM" {
				auth["key"] = s.Auth.Parameter
			}
		default:
			return nil, ErrUnsupported
		}
		entry = map[string]any{"id": s.ID, "protocol": s.Protocol, "url": s.URL, "auth": auth}
		if s.ConnectionRef != "" {
			value, err := storedMCPCredential(dir, serverID, s.ConnectionRef)
			if err != nil {
				return nil, err
			}
			var private struct {
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			}
			if json.Unmarshal(value, &private) != nil {
				return nil, ErrAuth
			}
			if private.URL != "" {
				entry["url"] = private.URL
			}
			if len(private.Headers) != 0 {
				header := s.Auth.Header
				if s.Auth.Method == "BEARER_TOKEN" {
					header = "Authorization"
				}
				if s.Auth.Method == "BEARER_TOKEN" || s.Auth.Method == "CUSTOM_HEADER" {
					for key := range private.Headers {
						if strings.EqualFold(key, header) {
							delete(private.Headers, key)
						}
					}
				}
				entry["headers"] = private.Headers
			}
		}

	}
	if entry == nil {
		return nil, ErrNotFound
	}
	var profiles struct {
		ToolSettings map[string]any   `yaml:"toolSettings"`
		ChatProfiles []map[string]any `yaml:"chatProfiles"`
	}
	if yaml.Unmarshal([]byte(bundle.Files["profiles.yaml"]), &profiles) != nil {
		return nil, ErrUnsupported
	}
	var chat struct {
		ProfileID *string `json:"profile_id"`
	}
	if json.Unmarshal(conversation, &chat) != nil {
		return nil, ErrUnsupported
	}
	result := map[string]any{"source": "mobile_ephemeral", "mcp": []any{entry}, "toolGrants": grants, "toolSettings": profiles.ToolSettings}
	if chat.ProfileID != nil && *chat.ProfileID != "" {
		for _, profile := range profiles.ChatProfiles {
			if profile["id"] == *chat.ProfileID {
				if result["profile"] != nil {
					return nil, ErrUnsupported
				}
				result["profile"] = profile
			}
		}
		if result["profile"] == nil {
			return nil, ErrAuth
		}
	}
	return json.Marshal(result)
}

func storedMCPCredential(dir, serverID, ref string) ([]byte, error) {
	prefix := "mcp/" + serverID + "/"
	if !strings.HasPrefix(ref, prefix) || len(ref) == len(prefix) {
		return nil, ErrAuth
	}
	store, err := secrets.OpenReadOnly(dir)
	if err != nil {
		return nil, ErrAuth
	}
	value, err := store.Get("mcp", serverID, strings.TrimPrefix(ref, prefix))
	closeErr := store.Close()
	if err != nil || closeErr != nil || len(value) == 0 {
		return nil, ErrAuth
	}
	return value, nil
}
