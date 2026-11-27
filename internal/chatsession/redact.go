package chatsession

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/starfall-org/generative-server/internal/httptool"
)

// runtimeSecrets gathers credential-bearing values. Each adapter must also
// avoid placing runtime configuration itself in snapshots.
func runtimeSecrets(raw json.RawMessage) []string {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	found := map[string]bool{}
	var walk func(any, string)
	add := func(v string) {
		if v != "" {
			found[v] = true
			if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
				found[v[7:]] = true
			}
			if encoded := url.QueryEscape(v); encoded != v {
				found[encoded] = true
			}
			if encoded := url.PathEscape(v); encoded != v {
				found[encoded] = true
			}
		}
	}
	walk = func(value any, parent string) {
		switch v := value.(type) {
		case map[string]any:
			if parent == "customtools" {
				if encoded, err := json.Marshal(v); err == nil {
					for _, secret := range httptool.StaticSecrets(encoded) {
						add(secret)
					}
				}
			}
			method, _ := v["method"].(string)
			for k, x := range v {
				if parent == "headers" {
					switch x.(type) {
					case float64, bool, json.Number:
						b, _ := json.Marshal(x)
						add(string(b))
					}
				}
				lower := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
				if str, ok := x.(string); ok {
					sensitive := lower == "apikey" || lower == "accesstoken" || lower == "refreshtoken" || lower == "clientsecret" || lower == "oauthclientsecret" || lower == "password" || lower == "token" || lower == "encryptedcredential" || lower == "marker" || (parent == "headers") || (parent == "auth" && lower == "value") || (parent == "auth" && lower == "key" && (method == "platform_default" || method == "")) || (parent == "apikeys" && lower == "value") || (lower == "defaultvalue")
					if sensitive {
						add(str)
					}
					if strings.HasPrefix(strings.ToLower(str), "https://") || strings.HasPrefix(strings.ToLower(str), "http://") {
						if u, err := url.Parse(str); err == nil {
							if u.User != nil {
								if password, ok := u.User.Password(); ok {
									add(password)
								}
							}
							for _, vals := range u.Query() {
								for _, v := range vals {
									add(v)
								}
							}
						}
					}
				}
				walk(x, lower)
			}
		case []any:
			for _, x := range v {
				walk(x, parent)
			}
		}
	}
	walk(root, "")
	out := make([]string, 0, len(found))
	for s := range found {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
func jsonContainsSecret(raw json.RawMessage, secrets []string) bool {
	var value any
	return json.Unmarshal(raw, &value) == nil && hasSecret(value, secrets)
}
func hasSecret(value any, secrets []string) bool {
	switch v := value.(type) {
	case string:
		for _, s := range secrets {
			if strings.Contains(v, s) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if hasSecret(x, secrets) {
				return true
			}
		}
	case map[string]any:
		for k, x := range v {
			if hasSecret(k, secrets) || hasSecret(x, secrets) {
				return true
			}
		}
	case float64, bool, json.Number:
		encoded, _ := json.Marshal(v)
		for _, secret := range secrets {
			if strings.Contains(string(encoded), secret) {
				return true
			}
		}
	}
	return false
}

// streamGuard holds enough trailing bytes to detect secrets split across deltas.
type streamGuard struct {
	pending string
	secrets []string
	keep    int
	leaked  bool
}

func newStreamGuard(secrets []string) *streamGuard {
	keep := 0
	for _, s := range secrets {
		if len(s)-1 > keep {
			keep = len(s) - 1
		}
	}
	return &streamGuard{secrets: secrets, keep: keep}
}
func (g *streamGuard) push(chunk string, final bool) string {
	if g.leaked {
		return ""
	}
	g.pending += chunk
	for _, secret := range g.secrets {
		if strings.Contains(g.pending, secret) {
			g.leaked = true
			g.pending = ""
			return ""
		}
	}
	if final {
		out := g.pending
		g.pending = ""
		return out
	}
	n := len(g.pending) - g.keep
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.ValidString(g.pending[:n]) {
		n--
	}
	out := g.pending[:n]
	g.pending = g.pending[n:]
	return out
}
