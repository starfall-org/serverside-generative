package compat

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/starfall-org/serverside-generative/internal/httptool"
)

var ErrMobileToolSettings = errors.New("invalid mobile tool settings")
var ErrMobileCustomTool = errors.New("invalid mobile custom HTTP tool")

type MobileToolSettings struct{ raw json.RawMessage }

func (m MobileToolSettings) Bytes() json.RawMessage { return append(json.RawMessage(nil), m.raw...) }
func (m MobileToolSettings) MarshalJSON() ([]byte, error) {
	if len(m.raw) == 0 {
		return nil, ErrMobileToolSettings
	}
	return m.Bytes(), nil
}

type ToolSettingsFields struct {
	System                    map[string]SystemToolConfigFields                   `json:"system"`
	QuickMCP                  map[string]bool                                     `json:"quickMcp"`
	WebSearchProviderID       *string                                             `json:"webSearchProviderId"`
	WebSearchEnabled          bool                                                `json:"webSearchEnabled"`
	MCPTools                  map[string]map[string]bool                          `json:"mcpTools"`
	MCPToolParameters         map[string]map[string]map[string]MCPParameterFields `json:"mcpToolParameters"`
	MachinesEnabled           bool                                                `json:"machinesEnabled"`
	EnabledMachines           map[string]bool                                     `json:"enabledMachines"`
	SelectedMachineID         *string                                             `json:"selectedMachineId"`
	RememberedEnabledMachines map[string]bool                                     `json:"rememberedEnabledMachines"`
	MachineTools              map[string]map[string]bool                          `json:"machineTools"`
	SendFileEnabled           bool                                                `json:"sendFileEnabled"`
	SearchToolsEnabled        bool                                                `json:"searchToolsEnabled"`
	AgentSkillsSearchEnabled  bool                                                `json:"agentSkillsSearchEnabled"`
}
type MCPParameterFields struct {
	Enabled      bool   `json:"enabled"`
	DefaultValue string `json:"defaultValue"`
}
type SystemToolConfigFields struct {
	Enabled                     bool                       `json:"enabled"`
	ProviderID                  string                     `json:"providerId"`
	ModelID                     string                     `json:"modelId"`
	Prompt                      string                     `json:"prompt"`
	TitleGenerationMinMessages  int                        `json:"titleGenerationMinMessages"`
	TitleGenerationMinWords     int                        `json:"titleGenerationMinWords"`
	ImageGenerationMode         string                     `json:"imageGenerationMode"`
	VideoGenerationMode         string                     `json:"videoGenerationMode"`
	ImageRequestProfiles        map[string]json.RawMessage `json:"imageRequestProfiles"`
	VideoRequestProfiles        map[string]json.RawMessage `json:"videoRequestProfiles"`
	MusicRequestProfiles        map[string]json.RawMessage `json:"musicRequestProfiles"`
	ActiveImageRequestProfileID string                     `json:"activeImageRequestProfileId"`
	ActiveVideoRequestProfileID string                     `json:"activeVideoRequestProfileId"`
	ActiveMusicRequestProfileID string                     `json:"activeMusicRequestProfileId"`
	DisabledParams              []string                   `json:"disabledParams"`
}

func toolSettingsDefaults() ToolSettingsFields {
	return ToolSettingsFields{
		System: map[string]SystemToolConfigFields{}, QuickMCP: map[string]bool{},
		MCPTools:          map[string]map[string]bool{},
		MCPToolParameters: map[string]map[string]map[string]MCPParameterFields{},
		EnabledMachines:   map[string]bool{}, RememberedEnabledMachines: map[string]bool{},
		MachineTools: map[string]map[string]bool{}, SendFileEnabled: true,
	}
}
func systemToolDefaults() SystemToolConfigFields {
	return SystemToolConfigFields{
		TitleGenerationMinMessages: 3, TitleGenerationMinWords: 10,
		ImageGenerationMode: "default", VideoGenerationMode: "default",
		ImageRequestProfiles: map[string]json.RawMessage{},
		VideoRequestProfiles: map[string]json.RawMessage{},
		MusicRequestProfiles: map[string]json.RawMessage{},
		DisabledParams:       []string{},
	}
}
func ParseMobileToolSettings(raw []byte) (MobileToolSettings, error) {
	if mobileUnique(raw) != nil {
		return MobileToolSettings{}, ErrMobileToolSettings
	}
	if _, err := toolSettingsFields(raw); err != nil {
		return MobileToolSettings{}, err
	}
	return MobileToolSettings{append(json.RawMessage(nil), raw...)}, nil
}
func (m MobileToolSettings) Fields() (ToolSettingsFields, error) { return toolSettingsFields(m.raw) }
func toolSettingsFields(raw []byte) (ToolSettingsFields, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil ||
		!mediaNonNull(root, "system", "quickMcp", "webSearchEnabled", "mcpTools",
			"mcpToolParameters", "machinesEnabled", "enabledMachines",
			"rememberedEnabledMachines", "machineTools", "sendFileEnabled",
			"searchToolsEnabled", "agentSkillsSearchEnabled") {
		return ToolSettingsFields{}, ErrMobileToolSettings
	}
	fields := toolSettingsDefaults()
	if system, ok := root["system"]; ok {
		var entries map[string]json.RawMessage
		if json.Unmarshal(system, &entries) != nil || entries == nil {
			return ToolSettingsFields{}, ErrMobileToolSettings
		}
		for name, rawEntry := range entries {
			if name == "" {
				return ToolSettingsFields{}, ErrMobileToolSettings
			}
			item, err := systemToolFields(rawEntry)
			if err != nil {
				return ToolSettingsFields{}, err
			}
			fields.System[name] = item
		}
	}
	var providers map[string]map[string]map[string]json.RawMessage
	if rawParameters, ok := root["mcpToolParameters"]; ok {
		if json.Unmarshal(rawParameters, &providers) != nil || providers == nil {
			return ToolSettingsFields{}, ErrMobileToolSettings
		}
		for _, servers := range providers {
			if servers == nil {
				return ToolSettingsFields{}, ErrMobileToolSettings
			}
			for _, tools := range servers {
				if tools == nil {
					return ToolSettingsFields{}, ErrMobileToolSettings
				}
				for _, rawParameter := range tools {
					var m map[string]json.RawMessage
					if json.Unmarshal(rawParameter, &m) != nil || m == nil ||
						!mediaNonNull(m, "enabled", "defaultValue") {
						return ToolSettingsFields{}, ErrMobileToolSettings
					}
				}
			}
		}
	}
	previous := make(map[string]SystemToolConfigFields, len(fields.System))
	for name, config := range fields.System {
		previous[name] = config
	}
	if json.Unmarshal(raw, &fields) != nil {
		return ToolSettingsFields{}, ErrMobileToolSettings
	}
	fields.System = previous
	// Kotlin McpToolParameterConfig.enabled defaults to true when omitted.
	if providers != nil {
		for provider, tools := range providers {
			if fields.MCPToolParameters[provider] == nil {
				fields.MCPToolParameters[provider] = map[string]map[string]MCPParameterFields{}
			}
			for tool, parameters := range tools {
				fields.MCPToolParameters[provider][tool] = map[string]MCPParameterFields{}
				for name, rawParameter := range parameters {
					value := MCPParameterFields{Enabled: true}
					if json.Unmarshal(rawParameter, &value) != nil {
						return ToolSettingsFields{}, ErrMobileToolSettings
					}
					fields.MCPToolParameters[provider][tool][name] = value
				}
			}
		}
	}
	if fields.QuickMCP == nil || fields.MCPTools == nil || fields.MCPToolParameters == nil ||
		fields.EnabledMachines == nil || fields.RememberedEnabledMachines == nil ||
		fields.MachineTools == nil {
		return ToolSettingsFields{}, ErrMobileToolSettings
	}
	return fields, nil
}
func systemToolFields(raw []byte) (SystemToolConfigFields, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil ||
		!mediaNonNull(root, "enabled", "providerId", "modelId", "prompt",
			"titleGenerationMinMessages", "titleGenerationMinWords",
			"imageOptionsByModel", "videoOptionsByModel", "imageGenerationMode",
			"videoGenerationMode", "imageRequestProfiles", "videoRequestProfiles",
			"musicRequestProfiles", "activeImageRequestProfileId",
			"activeVideoRequestProfileId", "activeMusicRequestProfileId", "disabledParams") {
		return SystemToolConfigFields{}, ErrMobileToolSettings
	}
	f := systemToolDefaults()
	if json.Unmarshal(raw, &f) != nil ||
		!mediaIs(f.ImageGenerationMode, "default", "custom") ||
		!mediaIs(f.VideoGenerationMode, "default", "custom") ||
		f.TitleGenerationMinMessages < 0 || f.TitleGenerationMinWords < 0 {
		return SystemToolConfigFields{}, ErrMobileToolSettings
	}
	for kind, profiles := range map[string]map[string]json.RawMessage{
		"image": f.ImageRequestProfiles, "video": f.VideoRequestProfiles, "music": f.MusicRequestProfiles,
	} {
		if profiles == nil {
			return SystemToolConfigFields{}, ErrMobileToolSettings
		}
		for id, rawProfile := range profiles {
			profile, err := ParseMobileMediaProfile(rawProfile)
			if err != nil {
				return SystemToolConfigFields{}, ErrMobileToolSettings
			}
			view, _ := profile.Fields()
			if id != view.ID || (kind == "music" && view.Mode != "custom") {
				return SystemToolConfigFields{}, ErrMobileToolSettings
			}
		}
	}
	if f.DisabledParams == nil {
		return SystemToolConfigFields{}, ErrMobileToolSettings
	}
	return f, nil
}

func (m MobileToolSettings) StoredMetadataJSON() ([]byte, error) {
	if _, err := m.Fields(); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(m.raw))
	dec.UseNumber()
	var root map[string]any
	if dec.Decode(&root) != nil || root == nil {
		return nil, ErrMobileToolSettings
	}
	if !discardEmptyMediaCredentials(root) || mediaPrivate(root, "") {
		return nil, ErrMobileToolSettings
	}
	return json.Marshal(root)
}

func discardEmptyMediaCredentials(value any) bool {
	switch data := value.(type) {
	case map[string]any:
		if item, ok := data["encryptedCredential"]; ok {
			text, ok := item.(string)
			if !ok || text != "" {
				return false
			}
			delete(data, "encryptedCredential")
		}
		for _, item := range data {
			if !discardEmptyMediaCredentials(item) {
				return false
			}
		}
	case []any:
		for _, item := range data {
			if !discardEmptyMediaCredentials(item) {
				return false
			}
		}
	}
	return true
}

// Unlike catalog metadata, CustomHttpTool JSON may contain credential-bearing
// URL/body/header templates and is only stored encrypted in SQLite.
type MobileCustomHTTPTool struct{ raw json.RawMessage }

func ParseMobileCustomHTTPTool(raw []byte) (MobileCustomHTTPTool, error) {
	if mobileUnique(raw) != nil {
		return MobileCustomHTTPTool{}, ErrMobileCustomTool
	}
	if _, err := httptool.Validate(raw); err != nil {
		return MobileCustomHTTPTool{}, ErrMobileCustomTool
	}
	return MobileCustomHTTPTool{append(json.RawMessage(nil), raw...)}, nil
}
func (m MobileCustomHTTPTool) MarshalJSON() ([]byte, error) {
	if len(m.raw) == 0 {
		return nil, ErrMobileCustomTool
	}
	return append([]byte(nil), m.raw...), nil
}
func (m MobileCustomHTTPTool) Bytes() json.RawMessage { return append(json.RawMessage(nil), m.raw...) }
