// Package compat validates mobile JSON while retaining its original fields.
package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrConversation = errors.New("invalid conversation")

// Conversation retains the exact mobile representation, including unknown
// fields and null/empty distinctions. It must never contain runtime settings.
type Conversation struct{ raw json.RawMessage }

func (c Conversation) MarshalJSON() ([]byte, error) { return c.raw, nil }
func (c Conversation) Bytes() json.RawMessage       { return append(json.RawMessage(nil), c.raw...) }

// ParseConversation checks Kotlin's required fields and basic relationships.
// Unknown values are retained, not interpreted as runtime configuration.
func ParseConversation(raw []byte) (Conversation, error) {
	if len(raw) == 0 || len(raw) > 16<<20 {
		return Conversation{}, ErrConversation
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := value(dec, 0); err != nil {
		return Conversation{}, ErrConversation
	}
	if _, err := dec.Token(); err != io.EOF {
		return Conversation{}, ErrConversation
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return Conversation{}, ErrConversation
	}
	var id, title string
	var created, updated int64
	if !required(obj, "id", &id) || id == "" || len(id) > 128 || !required(obj, "title", &title) || !required(obj, "created_at", &created) || !required(obj, "updated_at", &updated) {
		return Conversation{}, ErrConversation
	}
	ids := make(map[string]bool)
	if v, ok := obj["messages"]; ok {
		var msgs []json.RawMessage
		if string(v) == "null" || json.Unmarshal(v, &msgs) != nil {
			return Conversation{}, ErrConversation
		}

		for _, rawMsg := range msgs {
			var m map[string]json.RawMessage
			var mid, role string
			if json.Unmarshal(rawMsg, &m) != nil || m == nil || !required(m, "id", &mid) || mid == "" || ids[mid] || !required(m, "role", &role) || (role != "user" && role != "model" && role != "system") {
				return Conversation{}, ErrConversation
			}
			ids[mid] = true
			var versions []json.RawMessage
			if v, ok := m["versions"]; ok {
				if string(v) == "null" || json.Unmarshal(v, &versions) != nil {
					return Conversation{}, ErrConversation
				}
				for _, ver := range versions {
					var fields map[string]json.RawMessage
					if json.Unmarshal(ver, &fields) != nil || fields == nil {
						return Conversation{}, ErrConversation
					}
					if err := validateActivities(fields); err != nil {
						return Conversation{}, err
					}
				}
			}
			index := 0
			if v, ok := m["active_version_index"]; ok && (string(v) == "null" || json.Unmarshal(v, &index) != nil) {
				return Conversation{}, ErrConversation
			}
			if index < 0 || (len(versions) == 0 && index != 0) || (len(versions) > 0 && index >= len(versions)) {
				return Conversation{}, ErrConversation
			}
		}
	}
	if rawSummary, ok := obj["summary"]; ok && string(rawSummary) != "null" {
		var summary map[string]json.RawMessage
		var sid, content, through string
		if json.Unmarshal(rawSummary, &summary) != nil || summary == nil || !required(summary, "id", &sid) || sid == "" || !required(summary, "content", &content) || !required(summary, "through_message_id", &through) || !ids[through] {
			return Conversation{}, ErrConversation
		}
		if v, ok := summary["role"]; ok {
			var role string
			if string(v) == "null" || json.Unmarshal(v, &role) != nil || (role != "SYSTEM" && role != "ASSISTANT" && role != "USER") {
				return Conversation{}, ErrConversation
			}
		}
	}

	return Conversation{append(json.RawMessage(nil), raw...)}, nil
}

func required(m map[string]json.RawMessage, name string, dst any) bool {
	v, ok := m[name]
	return ok && string(v) != "null" && json.Unmarshal(v, dst) == nil
}
func validateActivities(v map[string]json.RawMessage) error {
	raw, ok := v["toolActivity"]
	if !ok {
		return nil
	}
	var activities []map[string]json.RawMessage
	if string(raw) == "null" || json.Unmarshal(raw, &activities) != nil {
		return ErrConversation
	}
	for _, a := range activities {
		var id, name string
		if a == nil || !required(a, "id", &id) || id == "" || !required(a, "name", &name) {
			return ErrConversation
		}
		for _, key := range []string{"arguments", "response"} {
			if v, ok := a[key]; ok {
				var s string
				if string(v) == "null" || json.Unmarshal(v, &s) != nil {
					return ErrConversation
				}
			}
		}
	}
	return nil
}

// value rejects duplicate object keys, excessive nesting and malformed JSON.
func value(d *json.Decoder, depth int) error {
	if depth > 128 {
		return ErrConversation
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			switch normalized {
			case "runtime", "apikey", "accesstoken", "refreshtoken", "clientsecret", "password", "authorization", "credentials":
				return ErrConversation
			}
			if !ok || seen[key] {
				return fmt.Errorf("duplicate or invalid key")
			}
			seen[key] = true
			if err = value(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrConversation
		}
	case '[':
		for d.More() {
			if err = value(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrConversation
		}
	default:
		return ErrConversation
	}
	return nil
}
