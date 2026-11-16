package compat

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestToolSettingsKotlinRoundTripDefaultsAndMedia(t *testing.T) {
	raw := []byte(`{
  "system":{"generate_music":{"enabled":true,"titleGenerationMinMessages":4,
    "musicRequestProfiles":{"m1":{"id":"m1","name":"Custom music","mode":"custom",
       "chain":{"steps":[{"endpoint":"https://example.invalid/generate","mode":"json"}],
       "finalResponse":{"type":"audio"}}}}}},
  "mcpTools":{"srv":{"search":true,"write":false}},
  "mcpToolParameters":{"srv":{"search":{"query":{"defaultValue":"x"}}}},
  "quickMcp":{"srv":true},"webSearchProviderId":null,
  "selectedMachineId":null,"sendFileEnabled":false,"future":{"number":9007199254740993}
 }`)
	settings, err := ParseMobileToolSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(settings)
	if err != nil || !bytes.Equal(wire, compact.Bytes()) || !bytes.Equal(settings.Bytes(), raw) {
		t.Fatal("tool settings lost fields")
	}
	f, err := settings.Fields()
	if err != nil {
		t.Fatal(err)
	}
	if f.SendFileEnabled || !f.QuickMCP["srv"] || f.MCPTools["srv"]["write"] ||
		!f.MCPToolParameters["srv"]["search"]["query"].Enabled ||
		f.MCPToolParameters["srv"]["search"]["query"].DefaultValue != "x" ||
		f.System["generate_music"].TitleGenerationMinMessages != 4 ||
		f.System["generate_music"].TitleGenerationMinWords != 10 {
		t.Fatalf("incorrect settings defaults: %+v", f)
	}
	metadata, err := settings.StoredMetadataJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte("keystore")) {
		t.Fatal("secret in ToolSettings metadata")
	}
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.UseNumber()
	if err = decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["future"].(map[string]any)["number"].(json.Number).String() != "9007199254740993" {
		t.Fatal("future large number lost")
	}
	empty, err := ParseMobileToolSettings([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defaults, _ := empty.Fields()
	if !defaults.SendFileEnabled || defaults.System == nil || defaults.QuickMCP == nil {
		t.Fatal("Kotlin ToolSettings defaults lost")
	}
}

func TestToolSettingsRejectsBadShapes(t *testing.T) {
	for _, raw := range []string{
		`{"quickMcp":null}`,
		`{"system":{"generate_music":{"musicRequestProfiles":{"x":{"id":"m","name":"M"}}}}}`,
		`{"system":{"generate_music":{"imageGenerationMode":"CUSTOM"}}}`,
		`{"system":{"generate_music":{"titleGenerationMinMessages":-1}}}`,
		`{"mcpToolParameters":{"s":{"t":{"p":null}}}}`,
		`{"mcpToolParameters":{"s":{"t":{"p":{"enabled":null}}}}}`,
		`{"mcpToolParameters":{"s":{"t":null}}}`,
		`{"mcpTools":[]}`,
		`{"invalid":{"duplicated":1,"duplicated":2}}`,
	} {
		if _, err := ParseMobileToolSettings([]byte(raw)); err == nil {
			t.Errorf("accepted malformed ToolSettings: %s", raw)
		}
	}
}

func TestCustomHTTPToolRetainsEncryptedOnlyFields(t *testing.T) {
	raw := []byte(`{"id":"tool-1","name":"Lookup","description":"A tool",
  "method":"POST","url":"https://example.invalid/lookup",
  "parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]},
  "headers":{"X-Custom-Key":"secret-literal"},
  "body":"{\"query\":\"{{query}}\"}","bodyFormat":"JSON","responseMode":"JSON",
  "responseMapping":{"result":"/data"},"future":{"opaque":[null,{}]}}`)
	tool, err := ParseMobileCustomHTTPTool(raw)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(tool)
	if err != nil || !bytes.Equal(out, compact.Bytes()) || !bytes.Equal(tool.Bytes(), raw) {
		t.Fatal("custom tool serialized data changed")
	}
	if !bytes.Contains(out, []byte("secret-literal")) {
		t.Fatal("custom tool must retain secret value for encrypted SQLite storage")
	}
	for _, bad := range []string{
		`{"id":"t","name":"bad name","method":"GET","url":"https://example.invalid"}`,
		`{"id":"t","name":"valid","method":"GET","url":"https://example.invalid","body":"bad"}`,
	} {
		if _, err := ParseMobileCustomHTTPTool([]byte(bad)); err == nil {
			t.Errorf("accepted invalid custom tool: %s", bad)
		}
	}
}
