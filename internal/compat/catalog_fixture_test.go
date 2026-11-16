package compat

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// The exact same resource is constructed and encoded by real kotlinx
// serializers in ServerSideCatalogFixtureTest.kt. Changes to the Android
// contract must update both copies and then pass the two test suites.
func TestKotlinCatalogMobileFixtureRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/catalog-mobile.json")
	if err != nil {
		t.Fatal(err)
	}
	var objects map[string]json.RawMessage
	if json.Unmarshal(raw, &objects) != nil {
		t.Fatal("invalid Kotlin fixture JSON")
	}
	expected := []string{"provider", "provider_null", "group", "model", "model_config", "mcp", "chat_profile", "media_profile", "tool_settings"}
	for _, name := range expected {
		value, ok := objects[name]
		if !ok {
			t.Fatalf("Kotlin fixture %s missing", name)
		}
		var decoded []byte
		switch name {
		case "provider", "provider_null":
			obj, e := ParseMobileProvider(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "group":
			obj, e := ParseMobileProviderGroup(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "model":
			obj, e := ParseMobileLanguageModel(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "model_config":
			obj, e := ParseMobileModelConfiguration(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "mcp":
			obj, e := ParseMobileMCP(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "chat_profile":
			obj, e := ParseMobileChatProfile(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "media_profile":
			obj, e := ParseMobileMediaProfile(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		case "tool_settings":
			obj, e := ParseMobileToolSettings(value)
			if e != nil {
				t.Fatalf("%s: %v", name, e)
			}
			decoded, e = json.Marshal(obj)
			if e != nil {
				t.Fatal(e)
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, value); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(decoded), compact.Bytes()) {
			t.Errorf("%s: lost exact Kotlin JSON fields: %s", name, decoded)
		}
	}
}
