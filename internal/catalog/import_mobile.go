package catalog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/starfall-org/serverside-generative/internal/compat"
	"github.com/starfall-org/serverside-generative/internal/execution"
	"github.com/starfall-org/serverside-generative/internal/secrets"
	"go.yaml.in/yaml/v4"
)

// ErrImportUnsupported signals that a mobile configuration cannot be imported
// without losing information or storing credentials in public catalog files.
var ErrImportUnsupported = errors.New("mobile configuration cannot be imported losslessly")

type importCredential struct {
	id, label, value string
}

// ImportMobile stages only the explicitly imported record's credentials in
// encrypted storage and publishes metadata with the existing catalog CAS.
// Existing records are never overwritten. A failed CAS can leave encrypted
// unreferenced credentials, as with the existing CLI credential commands.
func (s *Store) ImportMobile(kind string, raw json.RawMessage) error {
	var original, record map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&original) != nil || original == nil {
		return ErrInvalid
	}
	id, _ := original["id"].(string)
	if id == "" || len(id) > 128 || strings.Contains(id, "/") {
		return ErrInvalid
	}
	var staged []importCredential
	addSecret := func(label, value string) (string, error) {
		if value == "" || len(value) > 1<<20 {
			return "", ErrInvalid
		}
		var random [18]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		sid := hex.EncodeToString(random[:])
		staged = append(staged, importCredential{sid, label, value})
		return kind + "/" + id + "/" + sid, nil
	}
	var modelRecord map[string]any
	switch kind {
	case "provider":
		parsed, err := compat.ParseMobileProvider(raw)
		if err != nil {
			return ErrImportUnsupported
		}
		fields, _ := parsed.Fields()
		config, _ := original["config"].(map[string]any)
		if len(config) == 0 {
			config = map[string]any{}
		}
		// Account markers on Android are not portable OAuth credentials.
		if fields.Auth.Method == "oauth" || fields.Auth.OAuthAccountID != nil ||
			nonempty(config["oauthAccounts"]) || nonempty(config["headers"]) ||
			nonempty(config["httpProxy"]) || nonempty(config["socksProxy"]) {
			return ErrImportUnsupported
		}
		safe, err := parsed.StoredMetadataJSON()
		if err != nil || yaml.Unmarshal(safe, &record) != nil {
			return ErrImportUnsupported
		}
		// Folder membership is local organization, not part of the connection.
		delete(record, "groupId")
		metadata, _ := record["config"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			record["config"] = metadata
		}
		if _, exists := metadata["maxTokens"]; !exists {
			metadata["maxTokens"] = 4000
		}
		method := fields.Auth.Method
		key, value := stringValue(fields.Auth.Key), stringValue(fields.Auth.Value)
		if method == "platform_default" || method == "other" {
			if value == "" {
				value = key
			}
			if value == "" {
				method = "none"
			} else {
				method = "platform_default"
			}
		}
		if method == "bearer_token" || method == "custom_header" {
			if key == "" {
				key = "Authorization"
			}
			if strings.EqualFold(key, "Authorization") {
				method = "bearer_token"
				value = stripBearer(value)
			} else {
				method = "custom_header"
			}
		}
		auth := map[string]any{"method": method}
		switch method {
		case "none", "platform_default", "bearer_token":
		case "custom_header", "query_param":
			if !execution.ValidAuthKey(method, key) {
				return ErrImportUnsupported
			}
			auth["parameter_name"] = key
		default:
			return ErrImportUnsupported
		}
		if method != "none" && value != "" {
			ref, err := addSecret("", value)
			if err != nil {
				return err
			}
			auth["credential_ref"] = ref
		}
		record["auth"] = auth
		if entries, ok := config["apiKeys"].([]any); ok {
			keys := make([]any, 0, len(entries))
			for _, entry := range entries {
				item, ok := entry.(map[string]any)
				if !ok {
					return ErrInvalid
				}
				keyID, _ := item["id"].(string)
				label, _ := item["label"].(string)
				token, _ := item["value"].(string)
				if method == "bearer_token" {
					token = stripBearer(token)
				}
				ref, err := addSecret(label, token)
				if err != nil {
					return err
				}
				keys = append(keys, map[string]any{"id": keyID, "label": label, "credential_ref": ref})
			}
			metadata["apiKeys"] = keys
		}
		if method != "none" && auth["credential_ref"] == nil && metadata["multipleApiKeys"] != true {
			return ErrImportUnsupported
		}
		models, _ := metadata["modelConfigs"].(map[string]any)
		if models == nil {
			models = map[string]any{}
		}
		ids := metadata["modelIds"]
		// The catalog's model records own selection and per-model configuration.
		delete(metadata, "modelIds")
		delete(metadata, "modelConfigs")
		catalog := []any{}
		if ids == nil {
			modelIDs := make([]string, 0, len(models))
			for modelID := range models {
				modelIDs = append(modelIDs, modelID)
			}
			sort.Strings(modelIDs)
			for _, modelID := range modelIDs {
				catalog = append(catalog, map[string]any{"id": modelID, "display_name": modelID, "provider_id": id})
			}
		}
		modelRecord = map[string]any{"modelIds": ids, "modelConfigs": models, "catalog": catalog}
	case "mcp":
		parsed, err := compat.ParseMobileMCP(raw)
		if err != nil {
			return ErrImportUnsupported
		}
		fields, _ := parsed.Fields()
		if fields.Auth.Method == "OAUTH2" ||
			fields.Auth.OAuthClientID != nil || fields.Auth.OAuthClientSecret != nil ||
			fields.Auth.OAuthRedirectURI != nil || fields.Auth.OAuthAuthorized {
			return ErrImportUnsupported
		}
		private := map[string]any{}
		if len(fields.Headers) != 0 {
			private["headers"] = fields.Headers
			delete(original, "headers")
		}
		if fields.URL != nil {
			u, err := url.Parse(*fields.URL)
			if err != nil {
				return ErrImportUnsupported
			}
			if u.RawQuery != "" || u.ForceQuery {
				private["url"] = *fields.URL
				u.RawQuery = ""
				u.ForceQuery = false
				original["url"] = u.String()
			}
		}
		sanitized, err := json.Marshal(original)
		if err != nil {
			return ErrImportUnsupported
		}
		parsed, err = compat.ParseMobileMCP(sanitized)
		if err != nil {
			return ErrImportUnsupported
		}
		safe, err := parsed.StoredMetadataJSON()
		if err != nil || yaml.Unmarshal(safe, &record) != nil {
			return ErrImportUnsupported
		}
		record["protocol"] = fields.Protocol
		if len(private) != 0 {
			encoded, err := json.Marshal(private)
			if err != nil {
				return err
			}
			ref, err := addSecret("connection", string(encoded))
			if err != nil {
				return err
			}
			record["connection_ref"] = ref
		}
		method := fields.Auth.Method
		key, value := stringValue(fields.Auth.Key), stringValue(fields.Auth.Value)
		auth := map[string]any{"method": method}
		switch method {
		case "NONE":
		case "BEARER_TOKEN", "CUSTOM_HEADER":
			if key == "" {
				key = "Authorization"
			}
			if strings.EqualFold(key, "Authorization") {
				method = "BEARER_TOKEN"
				value = stripBearer(value)
			} else {
				method = "CUSTOM_HEADER"
				if !execution.ValidAuthKey("custom_header", key) {
					return ErrImportUnsupported
				}
				auth["header_name"] = key
			}
			auth["method"] = method
			ref, err := addSecret("", value)
			if err != nil {
				return err
			}
			auth["credential_ref"] = ref
		case "QUERY_PARAM":
			if key == "" {
				key = "key"
			}
			if !execution.ValidAuthKey("query_param", key) {
				return ErrImportUnsupported
			}
			auth["parameter_name"] = key
			ref, err := addSecret("", value)
			if err != nil {
				return err
			}
			auth["credential_ref"] = ref
		default:
			return ErrImportUnsupported
		}
		record["auth"] = auth
	default:
		return ErrInvalid
	}
	current, err := s.Ensure()
	if err != nil {
		return err
	}
	edit := func(docs map[string]*yaml.Node) error {
		if kind == "provider" {
			if err := appendRecord(lookup(docs["providers.yaml"].Content[0], "providers"), record, id); err != nil {
				return err
			}
			return addMapping(lookup(docs["models.yaml"].Content[0], "providers"), id, modelRecord)
		}
		return appendRecord(lookup(docs["mcp.yaml"].Content[0], "servers"), record, id)
	}
	// Validate all resulting documents before accepting a credential.
	if _, err = s.Update(current.Manifest.Revision, true, edit); err != nil {
		return err
	}
	if len(staged) > 0 {
		store, err := secrets.Open(s.dir)
		if err != nil {
			return err
		}
		for _, item := range staged {
			if err = store.Put(kind, id, item.id, item.label, "auth", []byte(item.value)); err != nil {
				_ = store.Close()
				return err
			}
		}
		if err = store.Close(); err != nil {
			return err
		}
	}
	_, err = s.Update(current.Manifest.Revision, false, edit)
	return err
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stripBearer(value string) string {
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func nonempty(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case []any:
		return len(v) != 0
	case map[string]any:
		return len(v) != 0
	default:
		return true
	}
}

type MCPInventoryItem struct {
	ID       string `json:"id" yaml:"id"`
	Name     string `json:"name" yaml:"name"`
	Protocol string `json:"protocol" yaml:"protocol"`
}

// MCPInventory exposes only allowlisted display metadata.
func (s *Store) MCPInventory() ([]MCPInventoryItem, error) {
	b, err := s.PreviewInventory()
	if err != nil {
		return nil, err
	}
	if b.Files == nil {
		return []MCPInventoryItem{}, nil
	}
	var doc struct {
		Servers []MCPInventoryItem `yaml:"servers"`
	}
	if yaml.Unmarshal([]byte(b.Files["mcp.yaml"]), &doc) != nil {
		return nil, ErrInvalid
	}
	if doc.Servers == nil {
		doc.Servers = []MCPInventoryItem{}
	}
	return doc.Servers, nil
}
