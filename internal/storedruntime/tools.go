package storedruntime

import (
	"encoding/json"
	"sort"

	"github.com/starfall-org/generative-server/internal/secrets"
)

// ResolveCustomTools decrypts only explicitly granted IDs and does not create
// any files. The returned configuration is for the current RAM session only.
func ResolveCustomTools(dir string, runtime json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Source     string `json:"source"`
		ToolGrants struct {
			CustomHTTP map[string]bool `json:"custom_http"`
		} `json:"toolGrants"`
	}
	if json.Unmarshal(runtime, &request) != nil || request.Source != "stored" {
		return nil, ErrUnsupported
	}
	ids := make([]string, 0, len(request.ToolGrants.CustomHTTP))
	for id, allowed := range request.ToolGrants.CustomHTTP {
		if allowed {
			if id == "" || len(id) > 128 {
				return nil, ErrUnsupported
			}
			ids = append(ids, id)
		}
	}
	if len(ids) > 64 {
		return nil, ErrUnsupported
	}
	sort.Strings(ids)
	values := make([]json.RawMessage, 0, len(ids))
	total := 0
	if len(ids) > 0 {
		store, err := secrets.OpenReadOnly(dir)
		if err != nil {
			return nil, ErrAuth
		}
		for _, id := range ids {
			value, err := store.GetTool(id)
			if err != nil {
				_ = store.Close()
				return nil, ErrNotFound
			}
			total += len(value)
			if total > 1<<20 {
				_ = store.Close()
				return nil, ErrUnsupported
			}
			values = append(values, value)
		}
		if err := store.Close(); err != nil {
			return nil, ErrAuth
		}
	}
	return json.Marshal(map[string]any{"source": "mobile_ephemeral", "customTools": values, "toolGrants": request.ToolGrants})
}
