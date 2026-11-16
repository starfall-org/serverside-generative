// Package httptool validates mobile custom HTTP tool definitions.
package httptool

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var ErrInvalid = errors.New("invalid custom HTTP tool")
var nameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,58}$`)
var placeholders = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// Validate checks fields needed by the mobile contract without interpreting
// or printing secret-bearing values. The original JSON is retained by storage.
func Validate(raw []byte) (string, error) {
	if len(raw) == 0 || len(raw) > 1<<20 || !json.Valid(raw) {
		return "", ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", ErrInvalid
	}
	for _, key := range []string{"id", "name", "description", "enabled", "method", "url", "parameters", "query", "headers", "body", "bodyFormat", "responseMode", "responseMapping"} {
		if v, ok := fields[key]; ok && string(v) == "null" {
			return "", ErrInvalid
		}
	}
	for _, key := range []string{"query", "headers", "responseMapping"} {
		if v, ok := fields[key]; ok {
			var object map[string]json.RawMessage
			if json.Unmarshal(v, &object) != nil || object == nil {
				return "", ErrInvalid
			}
		}
	}
	t := struct {
		ID              string                     `json:"id"`
		Name            string                     `json:"name"`
		Method          string                     `json:"method"`
		URL             string                     `json:"url"`
		Parameters      map[string]json.RawMessage `json:"parameters"`
		Query           any                        `json:"query"`
		Headers         map[string]json.RawMessage `json:"headers"`
		Body            string                     `json:"body"`
		BodyFormat      string                     `json:"bodyFormat"`
		ResponseMode    string                     `json:"responseMode"`
		ResponseMapping any                        `json:"responseMapping"`
	}{Method: "GET", BodyFormat: "JSON", ResponseMode: "AUTO", Parameters: map[string]json.RawMessage{"type": json.RawMessage(`"object"`), "properties": json.RawMessage(`{}`)}}
	if json.Unmarshal(raw, &t) != nil || t.ID == "" || len(t.ID) > 128 || !nameRE.MatchString(t.Name) {
		return "", ErrInvalid
	}
	switch t.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return "", ErrInvalid
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", ErrInvalid
	}
	if t.BodyFormat != "JSON" && t.BodyFormat != "TEXT" {
		return "", ErrInvalid
	}
	switch t.ResponseMode {
	case "AUTO", "RAW", "JSON", "MEDIA":
	default:
		return "", ErrInvalid
	}
	var typ string
	var props map[string]json.RawMessage
	if json.Unmarshal(t.Parameters["type"], &typ) != nil || typ != "object" || json.Unmarshal(t.Parameters["properties"], &props) != nil || props == nil {
		return "", ErrInvalid
	}
	for _, v := range props {
		var m map[string]json.RawMessage
		if json.Unmarshal(v, &m) != nil || m == nil {
			return "", ErrInvalid
		}
	}
	if rawReq, ok := t.Parameters["required"]; ok {
		var required []string
		if json.Unmarshal(rawReq, &required) != nil || required == nil {
			return "", ErrInvalid
		}
		for _, key := range required {
			if _, ok := props[key]; !ok {
				return "", ErrInvalid
			}
		}
	}
	headerNames := map[string]bool{}
	for key, v := range t.Headers {
		name := strings.ToLower(key)
		if name == "" || headerNames[name] {
			return "", ErrInvalid
		}
		headerNames[name] = true
		for _, r := range key {
			if r < 33 || r > 126 || !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
				return "", ErrInvalid
			}
		}
		var value any
		if json.Unmarshal(v, &value) != nil || value == nil {
			return "", ErrInvalid
		}
		switch value.(type) {
		case string, float64, bool:
		default:
			return "", ErrInvalid
		}
		if strings.ContainsAny(string(v), "\r\n") {
			return "", ErrInvalid
		}
		if str, ok := value.(string); ok && strings.ContainsAny(str, "\r\n") {
			return "", ErrInvalid
		}
	}
	if (t.Method == "GET" || t.Method == "HEAD") && strings.TrimSpace(t.Body) != "" {
		return "", ErrInvalid
	}
	if !templates(t.Query, props) || !templates(t.Headers, props) {
		return "", ErrInvalid
	}
	if strings.TrimSpace(t.Body) != "" {
		if t.BodyFormat == "JSON" {
			var v any
			if json.Unmarshal([]byte(t.Body), &v) != nil || !templates(v, props) {
				return "", ErrInvalid
			}
		} else if !templates(t.Body, props) {
			return "", ErrInvalid
		}
	}
	if !pointers(t.ResponseMapping) {
		return "", ErrInvalid
	}
	return t.ID, nil
}
func templates(v any, props map[string]json.RawMessage) bool {
	switch v := v.(type) {
	case string:
		for _, m := range placeholders.FindAllStringSubmatch(v, -1) {
			if _, ok := props[strings.TrimSpace(m[1])]; !ok {
				return false
			}
		}
	case []any:
		for _, x := range v {
			if !templates(x, props) {
				return false
			}
		}
	case map[string]any:
		for _, x := range v {
			if !templates(x, props) {
				return false
			}
		}
	case map[string]json.RawMessage:
		for _, raw := range v {
			var x any
			if json.Unmarshal(raw, &x) != nil || !templates(x, props) {
				return false
			}
		}
	}
	return true
}
func pointers(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		if v != "" && !strings.HasPrefix(v, "/") {
			return false
		}
		for i := 0; i < len(v); i++ {
			if v[i] == '~' {
				if i+1 >= len(v) || (v[i+1] != '0' && v[i+1] != '1') {
					return false
				}
				i++
			}
		}
		return true
	case []any:
		for _, x := range v {
			if !pointers(x) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, x := range v {
			if !pointers(x) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
