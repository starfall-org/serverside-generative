package catalog

import (
	"errors"
	"strings"
	"testing"
)

// All metadata writes, including manual YAML imports, pass the same
// mobile-compatible validation and secret-preservation boundary.
func TestKotlinDTOValidationAtCatalogWriteBoundary(t *testing.T) {
	cases := []struct {
		name        string
		file        string
		replacement string
		wantError   bool
	}{
		{
			name: "provider enum allowed", file: "providers.yaml",
			replacement: "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: Provider\n    type: google\n    baseUrl: https://example.invalid/v1\n",
		},
		{
			name: "invalid provider enum blocked", file: "providers.yaml",
			replacement: "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: Provider\n    type: GEMINI\n    baseUrl: https://example.invalid/v1\n", wantError: true,
		},
		{
			name: "model config enum blocked", file: "models.yaml",
			replacement: "schema_version: 1\nrevision: 1\nproviders:\n  p:\n    modelIds: []\n    modelConfigs:\n      id: {modelType: image_generation}\n    catalog: []\n", wantError: true,
		},
		{
			name: "tool settings switch valid", file: "profiles.yaml",
			replacement: "schema_version: 1\nrevision: 1\nchatProfiles: []\ntoolSettings:\n  quickMcp: {srv: true}\n  sendFileEnabled: false\n",
		},
		{
			name: "tool settings invalid media mode", file: "profiles.yaml",
			replacement: "schema_version: 1\nrevision: 1\nchatProfiles: []\ntoolSettings:\n  system:\n    generate_image:\n      imageGenerationMode: CUSTOM\n", wantError: true,
		},
		{
			name: "tool settings rejects secret-bearing metadata", file: "profiles.yaml",
			replacement: "schema_version: 1\nrevision: 1\nchatProfiles: []\ntoolSettings:\n  system:\n    generate_image:\n      imageRequestProfiles:\n        custom: {id: custom, name: Custom, encryptedCredential: 'keystore:v1:private'}\n", wantError: true,
		},
		{
			name: "media profile id mismatch blocked", file: "profiles.yaml",
			replacement: "schema_version: 1\nrevision: 1\nchatProfiles: []\nmediaProfiles:\n  image:\n    id-one: {id: id-two, name: Different, mode: custom}\n", wantError: true,
		},
		{
			name: "MCP default protocol valid", file: "mcp.yaml",
			replacement: "schema_version: 1\nrevision: 1\nservers:\n  - id: srv\n    name: Server\n    protocol: streamable_http\n",
		},
		{
			name: "MCP auth enum rejected", file: "mcp.yaml",
			replacement: "schema_version: 1\nrevision: 1\nservers:\n  - id: srv\n    name: Server\n    protocol: sse\n    auth: {method: bearer_token}\n", wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := docs("1")
			if tc.file == "models.yaml" {
				doc["providers.yaml"] = "schema_version: 1\nrevision: 1\ngroups: []\nproviders:\n  - id: p\n    name: P\n    type: openai\n    baseUrl: ''\n"
			}
			doc[tc.file] = tc.replacement
			err := Validate(doc, 1)
			if tc.wantError && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want invalid catalog, got %v", err)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("valid mobile catalog rejected: %v", err)
			}
		})
	}
}

func TestValidOpaqueMediaMetadataRoundTrip(t *testing.T) {
	doc := docs("1")
	doc["profiles.yaml"] = strings.Join([]string{
		"schema_version: 1",
		"revision: 1",
		"chatProfiles: []",
		"mediaProfiles:",
		"  image:",
		"    image-custom:",
		"      id: image-custom",
		"      name: MyImage",
		"      mode: custom",
		"      requiresModel: false",
		"      future: {enabled: true, nullable: null}",
		"  video: {}",
		"  music: {}",
	}, "\n") + "\n"
	if err := Validate(doc, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc["profiles.yaml"], "nullable: null") {
		t.Fatal("unknown metadata omitted")
	}
}
