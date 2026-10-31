package execution

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ClientContext is the client's durable non-secret configuration. It is persisted
// per paired client on the server. API keys, access tokens, authorization
// headers and session secrets must NEVER be present in this schema.
type ClientContext struct {
	Instructions string             `json:"instructions"`
	Skills       []AgentSkill       `json:"skills"`
	Providers    []ProviderMetadata `json:"providers,omitempty"`
}
type ProviderMetadata struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Platform string   `json:"platform"`
	BaseURL  string   `json:"base_url,omitempty"`
	Models   []string `json:"models,omitempty"`
}

type AgentSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content"` // SKILL.md text; never executed as code.
}

const MaxContextSize = 2 << 20
const MaxSkills = 64
const MaxSkillContentSize = 128 << 10
const MaxInstructionsSize = 128 << 10
const MaxProviderEntries = 128
const MaxModelsPerProvider = 512

func cleanText(v string) bool {
	if !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

func validSkillID(id string) bool {
	if len(id) < 1 || len(id) > 128 || strings.Contains(id, "..") || strings.HasPrefix(id, "/") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == '/') {
			return false
		}
	}
	return true
}

func (ctx ClientContext) Validate() error {
	if len(ctx.Instructions) > MaxInstructionsSize || !cleanText(ctx.Instructions) {
		return errors.New("invalid or oversized instructions")
	}
	if len(ctx.Providers) > MaxProviderEntries {
		return errors.New("too many provider metadata records")
	}
	if len(ctx.Skills) > MaxSkills {
		return errors.New("too many skills")
	}
	ids := make(map[string]struct{}, len(ctx.Skills))
	total := len(ctx.Instructions)
	for _, skill := range ctx.Skills {
		if !validSkillID(skill.ID) {
			return errors.New("invalid skill id")
		}
		if _, exists := ids[skill.ID]; exists {
			return errors.New("duplicate skill id")
		}
		ids[skill.ID] = struct{}{}
		if len(skill.Name) == 0 || len(skill.Name) > 128 || !cleanText(skill.Name) {
			return errors.New("invalid skill name")
		}
		if len(skill.Description) > 4096 || !cleanText(skill.Description) {
			return errors.New("invalid skill description")
		}
		if len(skill.Content) > MaxSkillContentSize || !cleanText(skill.Content) {
			return errors.New("invalid or oversized skill content")
		}
		total += len(skill.ID) + len(skill.Name) + len(skill.Description) + len(skill.Content)
		if total > MaxContextSize {
			return errors.New("context exceeds memory budget")
		}
	}
	providers := make(map[string]bool, len(ctx.Providers))
	for _, p := range ctx.Providers {
		if !validSkillID(p.ID) || providers[p.ID] || len(p.Name) > 128 || !cleanText(p.Name) || p.Name == "" || len(p.Platform) > 64 || !cleanText(p.Platform) || p.Platform == "" || len(p.BaseURL) > 2048 || len(p.Models) > MaxModelsPerProvider {
			return errors.New("invalid provider metadata")
		}
		if err := validateRemoteEndpoint(p.BaseURL); err != nil {
			return err
		}
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			// No query parameters may be persisted: some APIs encode keys in URLs.
			if err != nil || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
				return errors.New("provider metadata URL must not contain credentials or query parameters")
			}
		}
		providers[p.ID] = true
		total += len(p.ID) + len(p.Name) + len(p.Platform) + len(p.BaseURL)
		models := make(map[string]bool, len(p.Models))
		for _, model := range p.Models {
			if model == "" || len(model) > 256 || !cleanText(model) || models[model] {
				return errors.New("invalid or duplicate model identifier")
			}
			models[model] = true
			total += len(model)
		}
		if total > MaxContextSize {
			return errors.New("context exceeds memory budget")
		}
	}
	return nil
}

func (ctx ClientContext) Clone() ClientContext {
	copyCtx := ClientContext{Instructions: ctx.Instructions}
	// Preserve the difference between nil (JSON null) and an allocated empty
	// slice (JSON []); the digest must remain stable across RAM/disk copies.
	if ctx.Skills != nil {
		copyCtx.Skills = make([]AgentSkill, len(ctx.Skills))
		copy(copyCtx.Skills, ctx.Skills)
	}
	if ctx.Providers != nil {
		copyCtx.Providers = make([]ProviderMetadata, len(ctx.Providers))
		copy(copyCtx.Providers, ctx.Providers)
	}
	for i := range copyCtx.Providers {
		if ctx.Providers[i].Models != nil {
			copyCtx.Providers[i].Models = make([]string, len(ctx.Providers[i].Models))
			copy(copyCtx.Providers[i].Models, ctx.Providers[i].Models)
		}
	}
	return copyCtx
}

// WithClientContext only injects the user's instructions plus explicitly selected
// skills into *new* model calls. No skill script is launched or executed.
func (state State) WithClientContext(ctx ClientContext) (State, error) {
	if err := ctx.Validate(); err != nil {
		return State{}, err
	}
	if len(state.SelectedSkillIDs) > MaxSkills {
		return State{}, errors.New("too many selected skills")
	}
	if state.Provider.ID != "" {
		lookup := false
		for _, p := range ctx.Providers {
			if p.ID != state.Provider.ID {
				continue
			}
			lookup = true
			if state.Provider.Platform != "" && state.Provider.Platform != p.Platform {
				return State{}, errors.New("provider platform mismatch")
			}
			if state.Provider.BaseURL != "" && state.Provider.BaseURL != p.BaseURL {
				return State{}, errors.New("provider base URL mismatch")
			}
			state.Provider.Platform = p.Platform
			state.Provider.BaseURL = p.BaseURL
			break
		}
		if !lookup {
			return State{}, errors.New("provider metadata unavailable")
		}
	}
	extra := make([]Message, 0, 1+len(state.SelectedSkillIDs))
	if ctx.Instructions != "" {
		extra = append(extra, Message{Role: "system", Content: ctx.Instructions})
	}
	skillsByID := make(map[string]AgentSkill, len(ctx.Skills))
	for _, skill := range ctx.Skills {
		skillsByID[skill.ID] = skill
	}
	seen := make(map[string]bool, len(state.SelectedSkillIDs))
	for _, id := range state.SelectedSkillIDs {
		if seen[id] {
			return State{}, errors.New("duplicate selected skill")
		}
		seen[id] = true
		skill, ok := skillsByID[id]
		if !ok {
			return State{}, fmt.Errorf("selected skill unavailable: %s", id)
		}
		content := fmt.Sprintf("Agent skill (%s): %s\n%s", skill.ID, skill.Name, skill.Content)
		if len(content) > 512*1024 {
			return State{}, errors.New("selected skill too large")
		}
		extra = append(extra, Message{Role: "system", Content: content})
	}
	state.Conversation = append(extra, state.Conversation...)
	return state, nil
}
