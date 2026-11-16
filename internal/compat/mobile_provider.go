package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

var ErrMobileProvider = errors.New("invalid mobile provider")
var ErrMobileModel = errors.New("invalid mobile model")
var ErrMobileGroup = errors.New("invalid mobile provider group")

type MobileProvider struct{ raw json.RawMessage }

func (p MobileProvider) MarshalJSON() ([]byte, error) {
	if len(p.raw) == 0 {
		return nil, ErrMobileProvider
	}
	return append([]byte(nil), p.raw...), nil
}
func (p MobileProvider) Bytes() json.RawMessage { return append(json.RawMessage(nil), p.raw...) }

type ProviderFields struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	BaseURL   string          `json:"baseUrl"`
	Icon      *string         `json:"icon"`
	Auth      ProviderAuth    `json:"auth"`
	Config    json.RawMessage `json:"config"`
	GroupID   *string         `json:"groupId"`
	SortOrder int             `json:"sortOrder"`
	IsHidden  bool            `json:"isHidden"`
}
type ProviderAuth struct {
	Method         string  `json:"method"`
	Key            *string `json:"key"`
	Value          *string `json:"value"`
	OAuthAccountID *string `json:"oauthAccountId"`
}

func ParseMobileProvider(raw []byte) (MobileProvider, error) {
	if mobileUnique(raw) != nil {
		return MobileProvider{}, ErrMobileProvider
	}
	if _, err := providerFields(raw); err != nil {
		return MobileProvider{}, err
	}
	return MobileProvider{append(json.RawMessage(nil), raw...)}, nil
}
func (p MobileProvider) Fields() (ProviderFields, error) { return providerFields(p.raw) }
func providerFields(raw []byte) (ProviderFields, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil ||
		!mediaPresent(root, "id", "name", "type", "baseUrl") ||
		!mediaNonNull(root, "auth", "config", "sortOrder", "isHidden") {
		return ProviderFields{}, ErrMobileProvider
	}
	f := ProviderFields{Auth: ProviderAuth{Method: "platform_default"}, Config: json.RawMessage("{}"), SortOrder: 2147483647}
	if json.Unmarshal(raw, &f) != nil || f.ID == "" || len(f.ID) > 128 || strings.TrimSpace(f.Name) == "" ||
		!mediaIs(f.Type, "openai", "openai_responses", "anthropic", "google", "ollama", "antigravity", "github_copilot",
			"openai_codex", "claude_code", "grok_build", "meta_muse", "gemini_cli", "devin") ||
		!mediaIs(f.Auth.Method, "platform_default", "none", "query_param", "bearer_token", "custom_header", "oauth", "other") {
		return ProviderFields{}, ErrMobileProvider
	}
	if f.BaseURL != "" {
		u, err := url.Parse(f.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ProviderFields{}, ErrMobileProvider
		}
	}
	var config map[string]json.RawMessage
	if json.Unmarshal(f.Config, &config) != nil || config == nil {
		return ProviderFields{}, ErrMobileProvider
	}
	if err := validateProviderConfig(config); err != nil {
		return ProviderFields{}, err
	}
	if rawAuth, ok := root["auth"]; ok {
		var auth map[string]json.RawMessage
		if json.Unmarshal(rawAuth, &auth) != nil || auth == nil || !mediaNonNull(auth, "method") {
			return ProviderFields{}, ErrMobileProvider
		}
	}
	return f, nil
}

func validateProviderConfig(config map[string]json.RawMessage) error {
	if !mediaNonNull(config, "httpProxy", "socksProxy", "supportStream", "headers", "responsesApi",
		"customEndpoints", "maxTokens", "modelConfigs", "multipleApiKeys", "apiKeys",
		"oauthAccounts", "antigravityProjectIds", "geminiCliProjectIds", "anthropicPromptCache") {
		return ErrMobileProvider
	}
	if raw, ok := config["modelIds"]; ok && string(raw) != "null" {
		if !mobileStringList(raw) {
			return ErrMobileProvider
		}
	}
	if raw, ok := config["apiKeys"]; ok {
		var keys []json.RawMessage
		if json.Unmarshal(raw, &keys) != nil || keys == nil {
			return ErrMobileProvider
		}
		seen := map[string]bool{}
		for _, item := range keys {
			var fields map[string]json.RawMessage
			if json.Unmarshal(item, &fields) != nil || fields == nil || !mediaPresent(fields, "id", "label") {
				return ErrMobileProvider
			}
			var id string
			if json.Unmarshal(fields["id"], &id) != nil || id == "" || seen[id] {
				return ErrMobileProvider
			}
			seen[id] = true
		}
	}
	if raw, ok := config["modelConfigs"]; ok {
		var models map[string]json.RawMessage
		if json.Unmarshal(raw, &models) != nil || models == nil {
			return ErrMobileProvider
		}
		for id, model := range models {
			if id == "" || modelConfigValid(model) != nil {
				return ErrMobileProvider
			}
		}
	}
	if raw, ok := config["headers"]; ok {
		var headers map[string]string
		if json.Unmarshal(raw, &headers) != nil || headers == nil {
			return ErrMobileProvider
		}
	}
	return nil
}
func mobileStringList(raw []byte) bool {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil || list == nil {
		return false
	}
	for _, item := range list {
		var value string
		if string(item) == "null" || json.Unmarshal(item, &value) != nil {
			return false
		}
	}
	return true
}

// StoredMetadataJSON removes explicitly mobile credential-bearing fields,
// without losing ordered multiple-key labels/IDs or opaque nonsecret metadata.
// Any extra secret-looking value is refused rather than copied to YAML.
func (p MobileProvider) StoredMetadataJSON() ([]byte, error) {
	f, err := p.Fields()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(p.raw))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil || obj == nil {
		return nil, ErrMobileProvider
	}
	auth := map[string]any{"method": f.Auth.Method}
	if f.Auth.OAuthAccountID != nil {
		auth["oauthAccountId"] = *f.Auth.OAuthAccountID
	}
	if f.Auth.Key != nil && *f.Auth.Key != "" &&
		(f.Auth.Method == "custom_header" || f.Auth.Method == "query_param") {
		key := *f.Auth.Key
		if !validMobileAuthKey(key) {
			return nil, ErrMobileProvider
		}
		auth["key"] = key
	}
	delete(obj, "auth")
	var safeKeys any
	var safeAccounts any
	var multipleKeys any
	if config, ok := obj["config"].(map[string]any); ok {
		multipleKeys = config["multipleApiKeys"]
		delete(config, "multipleApiKeys") // Boolean switch, not an API key.
		if keys, ok := config["apiKeys"].([]any); ok {
			cleaned := make([]any, 0, len(keys))
			for _, entry := range keys {
				key, ok := entry.(map[string]any)
				if !ok {
					return nil, ErrMobileProvider
				}
				result := map[string]any{}
				for name, value := range key {
					if name == "id" || name == "label" || name == "credential_ref" {
						result[name] = value
					} else if name != "value" {
						return nil, ErrMobileProvider
					}
				}
				cleaned = append(cleaned, result)
			}
			safeKeys = cleaned
			delete(config, "apiKeys")
		}
		if accounts, ok := config["oauthAccounts"].([]any); ok {
			cleaned := make([]any, 0, len(accounts))
			for _, entry := range accounts {
				account, ok := entry.(map[string]any)
				if !ok {
					return nil, ErrMobileProvider
				}
				result := map[string]any{}
				for name, value := range account {
					if name == "id" || name == "label" || name == "identity" {
						result[name] = value
					} else if name != "marker" && name != "accessToken" && name != "refreshToken" {
						return nil, ErrMobileProvider
					}
				}
				cleaned = append(cleaned, result)
			}
			safeAccounts = cleaned
			delete(config, "oauthAccounts")
		}
	}
	if mediaPrivate(obj, "") {
		return nil, ErrMobileProvider
	}
	obj["auth"] = auth
	if config, ok := obj["config"].(map[string]any); ok {
		if multipleKeys != nil {
			config["multipleApiKeys"] = multipleKeys
		}
		if safeKeys != nil {
			config["apiKeys"] = safeKeys
		}
		if safeAccounts != nil {
			config["oauthAccounts"] = safeAccounts
		}
	}
	return json.Marshal(obj)
}

// validMobileAuthKey validates a nonsecret parameter or header name.
func validMobileAuthKey(key string) bool {
	if len(key) > 64 {
		return false
	}
	for i, c := range key {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			(i > 0 && (c >= '0' && c <= '9' || c == '-' || c == '_'))) {
			return false
		}
	}
	return true
}

type MobileProviderGroup struct{ raw json.RawMessage }
type ProviderGroupFields struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	SortOrder int     `json:"sortOrder"`
	Icon      *string `json:"icon"`
}

func ParseMobileProviderGroup(raw []byte) (MobileProviderGroup, error) {
	if mobileUnique(raw) != nil {
		return MobileProviderGroup{}, ErrMobileGroup
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil || !mediaPresent(m, "id", "name") ||
		!mediaNonNull(m, "sortOrder") {
		return MobileProviderGroup{}, ErrMobileGroup
	}
	var f ProviderGroupFields
	if json.Unmarshal(raw, &f) != nil || f.ID == "" || strings.TrimSpace(f.Name) == "" {
		return MobileProviderGroup{}, ErrMobileGroup
	}
	return MobileProviderGroup{append(json.RawMessage(nil), raw...)}, nil
}
func (p MobileProviderGroup) MarshalJSON() ([]byte, error) {
	if len(p.raw) == 0 {
		return nil, ErrMobileGroup
	}
	return append([]byte(nil), p.raw...), nil
}
func (p MobileProviderGroup) Fields() (ProviderGroupFields, error) {
	var f ProviderGroupFields
	f.SortOrder = 2147483647
	if _, err := ParseMobileProviderGroup(p.raw); err != nil {
		return f, err
	}
	err := json.Unmarshal(p.raw, &f)
	return f, err
}

type MobileLanguageModel struct{ raw json.RawMessage }
type LanguageModelFields struct {
	ID                 string          `json:"id"`
	DisplayName        string          `json:"display_name"`
	ProviderID         string          `json:"provider_id"`
	Icon               *string         `json:"icon"`
	InputCapabilities  json.RawMessage `json:"input_capabilities"`
	OutputCapabilities json.RawMessage `json:"output_capabilities"`
	ModelTools         []string        `json:"model_tools"`
}

func ParseMobileLanguageModel(raw []byte) (MobileLanguageModel, error) {
	if mobileUnique(raw) != nil {
		return MobileLanguageModel{}, ErrMobileModel
	}
	_, err := modelFields(raw)
	if err != nil {
		return MobileLanguageModel{}, err
	}
	return MobileLanguageModel{append(json.RawMessage(nil), raw...)}, nil
}
func (m MobileLanguageModel) MarshalJSON() ([]byte, error) {
	if len(m.raw) == 0 {
		return nil, ErrMobileModel
	}
	return append([]byte(nil), m.raw...), nil
}
func (m MobileLanguageModel) Fields() (LanguageModelFields, error) { return modelFields(m.raw) }
func modelFields(raw []byte) (LanguageModelFields, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil || !mediaPresent(root, "id", "display_name", "provider_id") ||
		!mediaNonNull(root, "input_capabilities", "output_capabilities", "model_tools") {
		return LanguageModelFields{}, ErrMobileModel
	}
	f := LanguageModelFields{InputCapabilities: json.RawMessage("{}"), OutputCapabilities: json.RawMessage("{}"), ModelTools: []string{}}
	if json.Unmarshal(raw, &f) != nil || f.ID == "" || f.ProviderID == "" {
		return LanguageModelFields{}, ErrMobileModel
	}
	for _, caps := range []json.RawMessage{f.InputCapabilities, f.OutputCapabilities} {
		var object map[string]json.RawMessage
		if json.Unmarshal(caps, &object) != nil || object == nil {
			return LanguageModelFields{}, ErrMobileModel
		}
		for key, val := range object {
			if key == "others" {
				continue
			}
			var boolean bool
			if string(val) == "null" || json.Unmarshal(val, &boolean) != nil {
				return LanguageModelFields{}, ErrMobileModel
			}
		}
	}
	if rawTools, ok := root["model_tools"]; ok && !mobileStringList(rawTools) {
		return LanguageModelFields{}, ErrMobileModel
	}
	return f, nil
}

type MobileModelConfiguration struct{ raw json.RawMessage }
type ModelConfigurationFields struct {
	ModelType             string          `json:"modelType"`
	DisplayName           string          `json:"displayName"`
	ReasoningEffort       *string         `json:"reasoning_effort"`
	MediaApiType          string          `json:"mediaApiType"`
	MediaRequestProfileID string          `json:"mediaRequestProfileId"`
	ModelJSON             json.RawMessage `json:"model_json"`
}

func ParseMobileModelConfiguration(raw []byte) (MobileModelConfiguration, error) {
	if mobileUnique(raw) != nil || modelConfigValid(raw) != nil {
		return MobileModelConfiguration{}, ErrMobileModel
	}
	return MobileModelConfiguration{append(json.RawMessage(nil), raw...)}, nil
}
func (m MobileModelConfiguration) MarshalJSON() ([]byte, error) {
	if len(m.raw) == 0 {
		return nil, ErrMobileModel
	}
	return append([]byte(nil), m.raw...), nil
}
func (m MobileModelConfiguration) Fields() (ModelConfigurationFields, error) {
	if modelConfigValid(m.raw) != nil {
		return ModelConfigurationFields{}, ErrMobileModel
	}
	f := ModelConfigurationFields{ModelType: "TEXT_GENERATION", MediaApiType: "default", ModelJSON: json.RawMessage("{}")}
	err := json.Unmarshal(m.raw, &f)
	return f, err
}
func modelConfigValid(raw []byte) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil || !mediaNonNull(m, "modelType", "displayName",
		"contextWindowTokens", "supportsVision", "supportsVideoInput", "supportsAudioInput",
		"supportsThinking", "supportsToolCalls", "model_tools", "enabled_model_tools",
		"mediaApiType", "mediaRequestProfileId", "model_json") {
		return ErrMobileModel
	}
	f := ModelConfigurationFields{ModelType: "TEXT_GENERATION", MediaApiType: "default", ModelJSON: json.RawMessage("{}")}
	if json.Unmarshal(raw, &f) != nil ||
		!mediaIs(f.ModelType, "TEXT_GENERATION", "IMAGE_GENERATION", "VIDEO_GENERATION",
			"SPEECH_TO_TEXT", "TEXT_TO_SPEECH", "EMBEDDING", "RERANKING", "REALTIME") ||
		!mediaIs(f.MediaApiType, "default", "custom") {
		return ErrMobileModel
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(f.ModelJSON, &object) != nil || object == nil {
		return ErrMobileModel
	}
	for _, key := range []string{"model_tools", "enabled_model_tools", "reasoning_efforts"} {
		if v, ok := m[key]; ok && !mobileStringList(v) {
			return ErrMobileModel
		}
	}
	return nil
}
