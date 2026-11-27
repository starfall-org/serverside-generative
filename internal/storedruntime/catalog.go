package storedruntime

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/starfall-org/generative-server/internal/catalog"
	"go.yaml.in/yaml/v4"
)

// Catalog is a deliberately allowlisted, nonsecret view of stored selection.
type Catalog struct {
	Version   int        `json:"version"`
	Revision  uint64     `json:"revision"`
	Providers []Provider `json:"providers"`
}
type Provider struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	Type                 string  `json:"type"`
	CustomModelID        bool    `json:"custom_model_id"`
	KeySelectionRequired bool    `json:"key_selection_required"`
	Keys                 []Key   `json:"keys"`
	Models               []Model `json:"models"`
}
type Key struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Availability string `json:"availability"`
}
type Model struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Availability   string `json:"availability"`
	TextGeneration bool   `json:"text_generation"`
	ContextLimit   *int   `json:"context_limit"`
	OutputLimit    *int   `json:"output_limit"`
}

func ReadCatalog(dir string) (Catalog, error) { return ReadCatalogValidated(dir, nil) }

// ReadCatalogValidated can check the resolved runtime using the same chat adapter rules.
func ReadCatalogValidated(dir string, validate func(json.RawMessage) error) (Catalog, error) {
	b, err := catalog.New(dir).PreviewInventory()
	if err != nil {
		return Catalog{}, err
	}
	out := Catalog{Version: 1, Revision: b.Manifest.Revision, Providers: []Provider{}}
	if b.Files == nil {
		return out, nil
	}
	var providers struct {
		Providers []struct {
			ID, Name, Type string
			Config         struct {
				Multiple bool                         `yaml:"multipleApiKeys"`
				Keys     []struct{ ID, Label string } `yaml:"apiKeys"`
			} `yaml:"config"`
		} `yaml:"providers"`
	}
	var models struct {
		Providers map[string]struct {
			IDs     *[]string `yaml:"modelIds"`
			Catalog []struct {
				ID   string `yaml:"id"`
				Name string `yaml:"display_name"`
			} `yaml:"catalog"`
			Configs map[string]struct {
				Name string `yaml:"displayName"`
				Type string `yaml:"modelType"`
			} `yaml:"modelConfigs"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(b.Files["providers.yaml"]), &providers) != nil || yaml.Unmarshal([]byte(b.Files["models.yaml"]), &models) != nil {
		return Catalog{}, ErrUnsupported
	}
	for _, p := range providers.Providers {
		group, exists := models.Providers[p.ID]
		entry := Provider{ID: p.ID, Name: p.Name, Type: p.Type, CustomModelID: exists && group.IDs == nil, KeySelectionRequired: p.Config.Multiple, Keys: []Key{}, Models: []Model{}}
		if p.Config.Multiple {
			for _, k := range p.Config.Keys {
				entry.Keys = append(entry.Keys, Key{ID: k.ID, Label: k.Label, Availability: "unknown"})
			}
		}
		ids := []string{}
		if group.IDs != nil {
			ids = append(ids, (*group.IDs)...)
		} else {
			for _, m := range group.Catalog {
				ids = append(ids, m.ID)
			}
		}
		if p.Config.Multiple && len(ids) > 0 {
			for i := range entry.Keys {
				raw, _ := json.Marshal(map[string]string{"source": "stored", "provider_id": p.ID, "model_id": ids[0], "key_id": entry.Keys[i].ID})
				_, e := Resolve(dir, raw)
				switch {
				case e == nil:
					entry.Keys[i].Availability = "available"
				case errors.Is(e, ErrAuth):
					entry.Keys[i].Availability = "credential_unavailable"
				default:
					entry.Keys[i].Availability = "unavailable"
				}
			}
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			name := id
			for _, m := range group.Catalog {
				if m.ID == id && m.Name != "" {
					name = m.Name
					break
				}
			}
			cfg := group.Configs[id]
			if cfg.Name != "" {
				name = cfg.Name
			}
			text := cfg.Type == "" || cfg.Type == "TEXT_GENERATION"
			availability := "available"
			if p.Config.Multiple {
				// Provider/model configuration is independent of the credential value;
				// check one available explicit key without returning its secret.
				keyID := ""
				for _, key := range entry.Keys {
					if key.Availability == "available" {
						keyID = key.ID
						break
					}
				}
				if keyID == "" {
					availability = "credential_unavailable"
				} else {
					raw, _ := json.Marshal(map[string]string{"source": "stored", "provider_id": p.ID, "model_id": id, "key_id": keyID})
					resolved, e := Resolve(dir, raw)
					switch {
					case e != nil:
						availability = "unavailable"
					case validate != nil && validate(resolved) != nil:
						availability = "unsupported"
					default:
						availability = "key_selection_required"
					}
				}
			} else {
				raw, _ := json.Marshal(map[string]string{"source": "stored", "provider_id": p.ID, "model_id": id})
				resolved, e := Resolve(dir, raw)
				if e == nil && validate != nil && validate(resolved) != nil {
					availability = "unsupported"
				}
				if errors.Is(e, ErrAuth) {
					availability = "credential_unavailable"
				} else if e != nil {
					availability = "unavailable"
				}
			}
			switch p.Type {
			case "openai", "openai_responses", "anthropic", "google":
			default:
				text = false
			}
			if !text {
				availability = "unsupported"
			}
			entry.Models = append(entry.Models, Model{ID: id, Name: name, Availability: availability, TextGeneration: text})
		}
		sort.Slice(entry.Models, func(i, j int) bool { return entry.Models[i].ID < entry.Models[j].ID })
		out.Providers = append(out.Providers, entry)
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].ID < out.Providers[j].ID })
	return out, nil
}
