// Package storedruntime resolves locally managed provider settings in RAM only.
package storedruntime

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/execution"
	"github.com/starfall-org/serverside-generative/internal/secrets"
	"go.yaml.in/yaml/v4"
)

var ErrAuth = errors.New("stored credential unavailable")
var ErrUnsupported = errors.New("stored configuration unsupported")
var ErrNotFound = errors.New("stored provider or model not found")

func Resolve(dir string, raw json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Source       string            `json:"source"`
		ProviderID   string            `json:"provider_id"`
		ModelID      string            `json:"model_id"`
		KeyID        string            `json:"key_id"`
		Instructions map[string]string `json:"instructions"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Source != "stored" || request.ProviderID == "" || request.ModelID == "" {
		return nil, ErrUnsupported
	}
	b, err := catalog.New(dir).Preview()
	if err != nil {
		return nil, err
	}
	type providerRecord struct {
		ID      string `yaml:"id"`
		Type    string `yaml:"type"`
		BaseURL string `yaml:"baseUrl"`
		Auth    struct {
			Method        string `yaml:"method"`
			CredentialRef string `yaml:"credential_ref"`
			ParameterName string `yaml:"parameter_name"`
		} `yaml:"auth"`
		Config map[string]any `yaml:"config"`
	}
	var providers struct {
		Providers []providerRecord `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(b.Files["providers.yaml"]), &providers) != nil {
		return nil, ErrUnsupported
	}
	var p *providerRecord
	for i := range providers.Providers {
		if providers.Providers[i].ID == request.ProviderID {
			p = &providers.Providers[i]
			break
		}
	}
	if p == nil {
		return nil, ErrNotFound
	}
	var models struct {
		Providers map[string]struct {
			ModelIDs     *[]string      `yaml:"modelIds"`
			ModelConfigs map[string]any `yaml:"modelConfigs"`
			Catalog      []struct {
				ID string `yaml:"id"`
			} `yaml:"catalog"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(b.Files["models.yaml"]), &models) != nil {
		return nil, ErrUnsupported
	}
	model, ok := models.Providers[request.ProviderID]
	if !ok {
		return nil, ErrNotFound
	}
	if model.ModelIDs != nil {
		selected := false
		for _, id := range *model.ModelIDs {
			if id == request.ModelID {
				selected = true
			}
		}
		if !selected {
			return nil, ErrNotFound
		}
	}
	auth := map[string]any{"method": p.Auth.Method}
	switch p.Auth.Method {
	case "", "oauth":
		return nil, ErrAuth
	case "none", "platform_default", "bearer_token", "custom_header", "query_param":
	default:
		return nil, ErrUnsupported
	}
	if p.Auth.Method == "custom_header" || p.Auth.Method == "query_param" {
		if !execution.ValidAuthKey(p.Auth.Method, p.Auth.ParameterName) {
			return nil, ErrUnsupported
		}
		auth["key"] = p.Auth.ParameterName
	}
	config := p.Config
	if config == nil {
		config = map[string]any{}
	}
	ref := p.Auth.CredentialRef
	if multiple, ok := config["multipleApiKeys"].(bool); ok && multiple && request.KeyID == "" {
		return nil, ErrAuth
	}
	if request.KeyID != "" {
		found := false
		entries, ok := config["apiKeys"].([]any)
		if !ok {
			return nil, ErrAuth
		}
		for _, v := range entries {
			item, ok := v.(map[string]any)
			if !ok {
				return nil, ErrUnsupported
			}
			if item["id"] == request.KeyID {
				ref, _ = item["credential_ref"].(string)
				found = true
				break
			}
		}
		if !found {
			return nil, ErrAuth
		}
	}
	if p.Auth.Method != "none" {
		prefix := "provider/" + request.ProviderID + "/"
		if !strings.HasPrefix(ref, prefix) || len(ref) == len(prefix) {
			return nil, ErrAuth
		}
		keyID := strings.TrimPrefix(ref, prefix)
		store, err := secrets.OpenReadOnly(dir)
		if err != nil {
			return nil, ErrAuth
		}
		value, err := store.Get("provider", request.ProviderID, keyID)
		closeErr := store.Close()
		if err != nil || closeErr != nil || len(value) == 0 {
			return nil, ErrAuth
		}
		auth["value"] = string(value)
	}
	// apiKeys contain references, not values. Explicit key selection above is the
	// only way to choose one; never silently pick the first or another provider.
	delete(config, "apiKeys")
	delete(config, "multipleApiKeys")
	if model.ModelIDs != nil {
		config["modelIds"] = *model.ModelIDs
	} else {
		config["modelIds"] = nil
	}
	provider := map[string]any{"id": p.ID, "type": p.Type, "baseUrl": p.BaseURL, "modelId": request.ModelID, "auth": auth, "config": config}
	if m, ok := model.ModelConfigs[request.ModelID]; ok {
		provider["modelConfig"] = m
	}
	encoded, err := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": provider, "instructions": request.Instructions})
	if err != nil {
		return nil, ErrUnsupported
	}
	return encoded, nil
}
