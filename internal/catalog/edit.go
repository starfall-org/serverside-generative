package catalog

import (
	"bytes"
	"errors"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Update edits all four documents under one revision CAS. dryRun performs no
// recovery or writes; an interrupted transaction must first be recovered.
func (s *Store) Update(expected uint64, dryRun bool, edit func(map[string]*yaml.Node) error) (Bundle, error) {
	var current Bundle
	var err error
	if dryRun {
		current, err = s.Preview()
	} else {
		current, err = s.Get()
	}
	if err != nil {
		return Bundle{}, err
	}
	if current.Manifest.Revision != expected || expected == ^uint64(0) {
		return Bundle{}, ErrConflict
	}
	docs := map[string]*yaml.Node{}
	for name, raw := range current.Files {
		var n yaml.Node
		if yaml.Unmarshal([]byte(raw), &n) != nil || len(n.Content) != 1 {
			return Bundle{}, ErrInvalid
		}
		docs[name] = &n
	}
	if err = edit(docs); err != nil {
		return Bundle{}, err
	}
	output := map[string]string{}
	for _, name := range files {
		root := docs[name].Content[0]
		revision := lookup(root, "revision")
		if revision == nil {
			return Bundle{}, ErrInvalid
		}
		revision.Kind = yaml.ScalarNode
		revision.Tag = "!!int"
		revision.Value = strconv.FormatUint(expected+1, 10)
		var b bytes.Buffer
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err = enc.Encode(docs[name]); err != nil {
			return Bundle{}, err
		}
		if err = enc.Close(); err != nil {
			return Bundle{}, err
		}
		output[name] = b.String()
	}
	if err = Validate(output, expected+1); err != nil {
		return Bundle{}, err
	}
	if dryRun {
		return Bundle{Manifest: Manifest{SchemaVersion: 1, Revision: expected + 1}, Files: output}, nil
	}
	return s.Save(expected, output)
}
func lookup(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func nodeFor(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}
func addMapping(n *yaml.Node, key string, value any) error {
	if n == nil || n.Kind != yaml.MappingNode || lookup(n, key) != nil {
		return ErrInvalid
	}
	v, err := nodeFor(value)
	if err != nil {
		return err
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return nil
}
func appendRecord(n *yaml.Node, record any, id string) error {
	if n == nil || n.Kind != yaml.SequenceNode || id == "" {
		return ErrInvalid
	}
	for _, item := range n.Content {
		if k := lookup(item, "id"); k != nil && k.Value == id {
			return ErrConflict
		}
	}
	v, err := nodeFor(record)
	if err != nil {
		return err
	}
	n.Content = append(n.Content, v)
	return nil
}

// AddProviderGroup creates a Kotlin-compatible folder without changing providers.
func AddProviderGroup(docs map[string]*yaml.Node, id, name string, sortOrder int) error {
	if id == "" || strings.TrimSpace(name) == "" {
		return ErrInvalid
	}
	groups := lookup(docs["providers.yaml"].Content[0], "groups")
	return appendRecord(groups, map[string]any{"id": id, "name": name, "sortOrder": sortOrder}, id)
}

// EditProviderGroup updates only explicitly selected presentation fields.
func EditProviderGroup(docs map[string]*yaml.Node, groupID string, name *string, sortOrder *int) error {
	if groupID == "" || (name == nil && sortOrder == nil) || (name != nil && strings.TrimSpace(*name) == "") {
		return ErrInvalid
	}
	groups := lookup(docs["providers.yaml"].Content[0], "groups")
	if groups == nil || groups.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, group := range groups.Content {
		if id := lookup(group, "id"); id != nil && id.Value == groupID {
			if name != nil {
				if err := setValue(group, "name", *name); err != nil {
					return err
				}
			}
			if sortOrder != nil {
				if err := setValue(group, "sortOrder", *sortOrder); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return errors.New("provider group not found")
}

// SetProviderGroup changes only the nullable groupId field. Empty groupID
// clears the assignment; it never deletes a group or modifies API credentials.
func SetProviderGroup(docs map[string]*yaml.Node, providerID, groupID string) error {
	if providerID == "" {
		return ErrInvalid
	}
	root := docs["providers.yaml"].Content[0]
	if groupID != "" {
		groups := lookup(root, "groups")
		found := false
		if groups == nil || groups.Kind != yaml.SequenceNode {
			return ErrInvalid
		}
		for _, group := range groups.Content {
			if id := lookup(group, "id"); id != nil && id.Value == groupID {
				found = true
				break
			}
		}
		if !found {
			return errors.New("provider group not found")
		}
	}
	providers := lookup(root, "providers")
	if providers == nil || providers.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, provider := range providers.Content {
		if id := lookup(provider, "id"); id != nil && id.Value == providerID {
			var target any
			if groupID != "" {
				target = groupID
			}
			return setValue(provider, "groupId", target)
		}
	}
	return errors.New("provider not found")
}

// DeleteProviderGroup refuses to drop a group while any provider belongs to it.
func DeleteProviderGroup(docs map[string]*yaml.Node, groupID string) error {
	if groupID == "" {
		return ErrInvalid
	}
	root := docs["providers.yaml"].Content[0]
	providers := lookup(root, "providers")
	if providers == nil || providers.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, provider := range providers.Content {
		if id := lookup(provider, "groupId"); id != nil && id.Value == groupID {
			return ErrConflict
		}
	}
	groups := lookup(root, "groups")
	if groups == nil || groups.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for i, group := range groups.Content {
		if id := lookup(group, "id"); id != nil && id.Value == groupID {
			groups.Content = append(groups.Content[:i], groups.Content[i+1:]...)
			return nil
		}
	}
	return errors.New("provider group not found")
}

func AddProvider(docs map[string]*yaml.Node, id, name, typ, baseURL string) error {
	if id == "" || name == "" || typ == "" {
		return ErrInvalid
	}
	p := docs["providers.yaml"].Content[0]
	if err := appendRecord(lookup(p, "providers"), map[string]any{"id": id, "name": name, "type": typ, "baseUrl": baseURL, "auth": map[string]any{"method": "platform_default"}, "config": map[string]any{}, "sortOrder": 0, "isHidden": false}, id); err != nil {
		return err
	}
	return addMapping(lookup(docs["models.yaml"].Content[0], "providers"), id, map[string]any{"modelIds": nil, "modelConfigs": map[string]any{}, "catalog": []any{}})
}
func HideProvider(docs map[string]*yaml.Node, id string, hidden bool) error {
	list := lookup(docs["providers.yaml"].Content[0], "providers")
	if list == nil || list.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, item := range list.Content {
		key := lookup(item, "id")
		if key != nil && key.Value == id {
			value := lookup(item, "isHidden")
			if value == nil {
				return addMapping(item, "isHidden", hidden)
			}
			value.Kind = yaml.ScalarNode
			value.Tag = "!!bool"
			value.Value = strconv.FormatBool(hidden)
			return nil
		}
	}
	return errors.New("provider not found")
}
func AddModel(docs map[string]*yaml.Node, provider, id, name, typ string) error {
	if provider == "" || id == "" || name == "" || typ == "" {
		return ErrInvalid
	}
	p := lookup(lookup(docs["models.yaml"].Content[0], "providers"), provider)
	if p == nil {
		return errors.New("provider not found")
	}
	configs := lookup(p, "modelConfigs")
	if configs == nil {
		if err := addMapping(p, "modelConfigs", map[string]any{}); err != nil {
			return err
		}
		configs = lookup(p, "modelConfigs")
	}
	switch typ {
	case "TEXT_GENERATION", "IMAGE_GENERATION", "VIDEO_GENERATION", "SPEECH_TO_TEXT", "TEXT_TO_SPEECH", "EMBEDDING", "RERANKING", "REALTIME":
	default:
		return ErrInvalid
	}
	if err := addMapping(configs, id, map[string]any{"displayName": name, "modelType": typ}); err != nil {
		return err
	}
	list := lookup(p, "catalog")
	if list == nil {
		if err := addMapping(p, "catalog", []any{}); err != nil {
			return err
		}
		list = lookup(p, "catalog")
	}
	return appendRecord(list, map[string]any{"id": id, "display_name": name, "provider_id": provider, "input_capabilities": map[string]any{"text": true, "image": false}, "output_capabilities": map[string]any{"text": typ == "TEXT_GENERATION", "image": false}, "model_tools": []any{}}, id)
}
func SelectModels(docs map[string]*yaml.Node, provider, mode string, ids []string) error {
	p := lookup(lookup(docs["models.yaml"].Content[0], "providers"), provider)
	if p == nil {
		return errors.New("provider not found")
	}
	var value any
	if mode == "discovery" {
		if len(ids) > 0 {
			return ErrInvalid
		}
		value = nil
	} else if mode == "explicit" {
		if ids == nil {
			ids = []string{}
		}
		value = ids
	} else {
		return ErrInvalid
	}
	return setValue(p, "modelIds", value)
}
func AddMCP(docs map[string]*yaml.Node, id, name, protocol, endpoint string) error {
	if id == "" || name == "" || (protocol != "sse" && protocol != "streamable_http") || endpoint == "" {
		return ErrInvalid
	}
	list := lookup(docs["mcp.yaml"].Content[0], "servers")
	return appendRecord(list, map[string]any{"id": id, "name": name, "protocol": protocol, "url": endpoint, "auth": map[string]any{"method": "NONE"}, "headers": map[string]any{}, "cachedTools": nil, "sortOrder": 0}, id)
}

// SetMCPCredential publishes only a reference to a separately encrypted value.
func SetMCPCredential(docs map[string]*yaml.Node, serverID, method, key, ref string) error {
	if serverID == "" || ref == "" || (method != "BEARER_TOKEN" && method != "CUSTOM_HEADER") || (method == "CUSTOM_HEADER" && key == "") {
		return ErrInvalid
	}
	if method == "CUSTOM_HEADER" && !validAuthParameter("custom_header", key) {
		return ErrInvalid
	}
	list := lookup(docs["mcp.yaml"].Content[0], "servers")
	if list == nil || list.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, server := range list.Content {
		if id := lookup(server, "id"); id != nil && id.Value == serverID {
			auth := map[string]any{"method": method, "credential_ref": ref}
			if method == "CUSTOM_HEADER" {
				auth["header_name"] = key
			}
			return setValue(server, "auth", auth)
		}
	}
	return errors.New("MCP server not found")
}

// AddMediaProfile preserves the entire YAML profile, including unknown fields.
func AddMediaProfile(docs map[string]*yaml.Node, kind string, profile *yaml.Node) error {
	if kind != "image" && kind != "video" && kind != "music" {
		return ErrInvalid
	}
	if profile == nil || profile.Kind != yaml.MappingNode {
		return ErrInvalid
	}
	id, name, mode := lookup(profile, "id"), lookup(profile, "name"), lookup(profile, "mode")
	if id == nil || id.Value == "" || name == nil || name.Value == "" || mode == nil || (mode.Value != "default" && mode.Value != "custom") {
		return ErrInvalid
	}
	if kind == "music" && mode.Value != "custom" {
		return ErrInvalid
	}
	if mode.Value == "default" && id.Value != kind+"gen-default" {
		return ErrInvalid
	}
	if requires := lookup(profile, "requiresModel"); requires != nil && requires.Value == "false" && mode.Value != "custom" {
		return ErrInvalid
	}
	root := docs["profiles.yaml"].Content[0]
	media := lookup(root, "mediaProfiles")
	if media == nil {
		if err := addMapping(root, "mediaProfiles", map[string]any{}); err != nil {
			return err
		}
		media = lookup(root, "mediaProfiles")
	}
	group := lookup(media, kind)
	if group == nil {
		if err := addMapping(media, kind, map[string]any{}); err != nil {
			return err
		}
		group = lookup(media, kind)
	}
	if group.Kind != yaml.MappingNode || lookup(group, id.Value) != nil {
		return ErrConflict
	}
	group.Content = append(group.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id.Value}, profile)
	return nil
}

// Empty creates the four initial schema documents; it never writes them.
func Empty() (map[string]string, error) {
	values := map[string]any{"providers.yaml": map[string]any{"schema_version": 1, "revision": 1, "groups": []any{}, "providers": []any{}}, "models.yaml": map[string]any{"schema_version": 1, "revision": 1, "providers": map[string]any{}}, "mcp.yaml": map[string]any{"schema_version": 1, "revision": 1, "servers": []any{}}, "profiles.yaml": map[string]any{"schema_version": 1, "revision": 1, "chatProfiles": []any{}, "mediaProfiles": map[string]any{"image": map[string]any{}, "video": map[string]any{}, "music": map[string]any{}}, "toolSettings": map[string]any{}}}
	docs := map[string]string{}
	for k, v := range values {
		b, err := yaml.Marshal(v)
		if err != nil {
			return nil, err
		}
		docs[k] = string(b)
	}
	if err := Validate(docs, 1); err != nil {
		return nil, err
	}
	return docs, nil
}

// SetProviderCredential changes only metadata references; secret material is
// staged separately in the encrypted store before this revision is published.
func SetProviderCredential(docs map[string]*yaml.Node, providerID, keyID, label, ref string) error {
	if providerID == "" || keyID == "" || ref == "" {
		return ErrInvalid
	}
	list := lookup(docs["providers.yaml"].Content[0], "providers")
	if list == nil || list.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	var provider *yaml.Node
	for _, p := range list.Content {
		if id := lookup(p, "id"); id != nil && id.Value == providerID {
			provider = p
			break
		}
	}
	if provider == nil {
		return errors.New("provider not found")
	}
	auth := lookup(provider, "auth")
	if auth == nil {
		if err := addMapping(provider, "auth", map[string]any{"method": "platform_default"}); err != nil {
			return err
		}
		auth = lookup(provider, "auth")
	}
	method := lookup(auth, "method")
	if method == nil || (method.Value != "platform_default" && method.Value != "bearer_token" && method.Value != "custom_header" && method.Value != "query_param") {
		return ErrInvalid
	}
	if method.Value == "custom_header" || method.Value == "query_param" {
		parameter := lookup(auth, "parameter_name")
		if parameter == nil || !validAuthParameter(method.Value, parameter.Value) {
			return ErrInvalid
		}
	}
	if err := setValue(auth, "credential_ref", ref); err != nil {
		return err
	}
	config := lookup(provider, "config")
	if config == nil {
		if err := addMapping(provider, "config", map[string]any{}); err != nil {
			return err
		}
		config = lookup(provider, "config")
	}
	keys := lookup(config, "apiKeys")
	if keys == nil {
		if err := addMapping(config, "apiKeys", []any{}); err != nil {
			return err
		}
		keys = lookup(config, "apiKeys")
	}
	if keys.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, entry := range keys.Content {
		if id := lookup(entry, "id"); id != nil && id.Value == keyID {
			if err := setValue(entry, "label", label); err != nil {
				return err
			}
			return setValue(entry, "credential_ref", ref)
		}
	}
	return appendRecord(keys, map[string]any{"id": keyID, "label": label, "credential_ref": ref}, keyID)
}

// SetProviderAuth edits nonsecret authentication metadata without deleting keys.
func SetProviderAuth(docs map[string]*yaml.Node, providerID, method, parameter string) error {
	switch method {
	case "none", "platform_default", "bearer_token":
		if parameter != "" {
			return ErrInvalid
		}
	case "custom_header", "query_param":
		if !validAuthParameter(method, parameter) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	list := lookup(docs["providers.yaml"].Content[0], "providers")
	if list == nil || list.Kind != yaml.SequenceNode {
		return ErrInvalid
	}
	for _, provider := range list.Content {
		if id := lookup(provider, "id"); id != nil && id.Value == providerID {
			auth := lookup(provider, "auth")
			if auth == nil {
				if err := addMapping(provider, "auth", map[string]any{}); err != nil {
					return err
				}
				auth = lookup(provider, "auth")
			}
			if err := setValue(auth, "method", method); err != nil {
				return err
			}
			return setValue(auth, "parameter_name", parameter)
		}
	}
	return errors.New("provider not found")
}

func validAuthParameter(method, key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	if method == "custom_header" {
		switch strings.ToLower(key) {
		case "host", "content-length", "transfer-encoding", "connection", "proxy-authorization", "proxy-authenticate", "trailer":
			return false
		}
	}
	return true
}
func setValue(mapping *yaml.Node, key string, value any) error {
	node := lookup(mapping, key)
	if node == nil {
		return addMapping(mapping, key, value)
	}
	v, err := nodeFor(value)
	if err != nil {
		return err
	}
	*node = *v
	return nil
}
