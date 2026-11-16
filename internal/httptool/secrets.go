package httptool

import (
	"encoding/json"
	"net/url"
	"strings"
)

// StaticSecrets conservatively treats literal tool headers, URL query values,
// query templates and body-template literals as sensitive. Parameter values
// inserted at execution time remain caller-provided arguments.
func StaticSecrets(raw json.RawMessage) []string {
	var t struct {
		URL        string         `json:"url"`
		Headers    map[string]any `json:"headers"`
		Query      map[string]any `json:"query"`
		Body       string         `json:"body"`
		BodyFormat string         `json:"bodyFormat"`
		Parameters any            `json:"parameters"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return nil
	}
	out := map[string]bool{}
	var add func(string)
	add = func(s string) {
		if s == "" {
			return
		}
		matches := placeholders.FindAllStringIndex(s, -1)
		if len(matches) > 0 {
			last := 0
			for _, match := range matches {
				add(strings.TrimSpace(s[last:match[0]]))
				last = match[1]
			}
			add(strings.TrimSpace(s[last:]))
			return
		}
		out[s] = true
		if len(s) > 7 && strings.EqualFold(s[:7], "Bearer ") {
			add(s[7:])
		}
		if encoded := url.QueryEscape(s); encoded != s {
			out[encoded] = true
		}
		if encoded := url.PathEscape(s); encoded != s {
			out[encoded] = true
		}
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			add(v)
		case float64, bool, json.Number:
			b, _ := json.Marshal(v)
			add(string(b))
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	var defaults func(any)
	defaults = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, x := range v {
				if key == "default" {
					walk(x)
				} else {
					defaults(x)
				}
			}
		case []any:
			for _, x := range v {
				defaults(x)
			}
		}
	}
	defaults(t.Parameters)
	for _, v := range t.Headers {
		walk(v)
	}
	for _, v := range t.Query {
		walk(v)
	}
	if u, err := url.Parse(t.URL); err == nil {
		if u.User != nil {
			if pass, ok := u.User.Password(); ok {
				add(pass)
			}
		}
		for _, values := range u.Query() {
			for _, v := range values {
				add(v)
			}
		}
	}
	if t.Body != "" {
		var body any
		if json.Unmarshal([]byte(t.Body), &body) == nil {
			walk(body)
		} else {
			add(t.Body)
			if q, err := url.ParseQuery(t.Body); err == nil {
				for _, values := range q {
					for _, v := range values {
						add(v)
					}
				}
			}
		}
	}
	secrets := make([]string, 0, len(out))
	for s := range out {
		secrets = append(secrets, s)
	}
	return secrets
}
