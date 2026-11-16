package compat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMobileMediaProfileRoundTripAndDefaults(t *testing.T) {
	// The keys, enum spellings and null/empty distinctions follow Kotlin's
	// shared/model/media/MediaRequestChain.kt (not a Go-specific wire format).
	input := []byte(`{
		"id":"music-1","name":"Music \\u266b","mode":"custom",
		"chain":{"steps":[{"endpoint":"https://example.invalid/create",
			"headers":{"Content-Type":"application/json","Authorization":"Bearer {{api_token}}"},
			"mode":"json","fileTransport":"base64","body":"{\"prompt\":\"{{prompt}}\"}",
			"observation":{"enabled":null,"statusPath":"$.status","mode":"value",
				"readyValues":[],"pendingValues":["pending"],"errorValues":["failed"],
				"retry":{"maxAttempts":3,"delayMillis":1500,"maxWaitMillis":5000}},
			"futureStep":{"order":9007199254740993}}],
			"finalResponse":{"type":"audio","mediaPath":"/result","valueKind":"link","sourceStep":null}},
		"options":{"future":{"null":null,"empty":[],"number":9007199254740993}},
		"disabledParams":[],"paramsConfigured":true,
		"encryptedCredential":"keystore:v1:opaque-android-blob","requiresModel":false,
		"futureProfile":{"null":null,"array":[]}
	}`)
	p, err := ParseMobileMediaProfile(input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(p)
	var compact bytes.Buffer
	if e := json.Compact(&compact, input); e != nil {
		t.Fatal(e)
	}
	if err != nil || !bytes.Equal(compact.Bytes(), encoded) ||
		!bytes.Equal(input, p.Bytes()) {
		t.Fatalf("media DTO changed on round-trip: %v / %s", err, encoded)
	}
	fields, err := p.Fields()
	if err != nil {
		t.Fatal(err)
	}
	if fields.Mode != "custom" || fields.RequiresModel ||
		fields.Chain.Steps[0].Observation.Enabled != nil ||
		fields.Chain.Steps[0].Observation.Retry.MaxAttempts != 3 ||
		fields.Chain.Steps[0].FileTransport != "base64" ||
		fields.Chain.FinalResponse.SourceStep != nil ||
		fields.Chain.FinalResponse.Type != "audio" {
		t.Fatalf("incorrect media profile fields: %+v", fields)
	}
	// The original stays untouched if the caller modifies the typed view.
	fields.ID = "overwritten"
	encoded, _ = json.Marshal(p)
	if !bytes.Equal(compact.Bytes(), encoded) {
		t.Fatal("mutating read-only view changed original media DTO")
	}
	metadata, err := p.StoredMetadataJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte("keystore:v1")) ||
		bytes.Contains(metadata, []byte("encryptedCredential")) {
		t.Fatal("Android Keystore material leaked into YAML metadata projection")
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(metadata))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	number := obj["options"].(map[string]any)["future"].(map[string]any)["number"].(json.Number)
	if number.String() != "9007199254740993" {
		t.Fatalf("unknown extension number lost precision: %s", number)
	}
	if _, ok := obj["futureProfile"]; !ok {
		t.Fatal("unknown extension removed from metadata")
	}

	minimal, err := ParseMobileMediaProfile([]byte(`{"id":"default","name":"Default"}`))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := minimal.Fields()
	if f.Mode != "custom" || !f.RequiresModel ||
		len(f.Chain.Steps) != 1 || f.Chain.Steps[0].Method != "POST" ||
		f.Chain.Steps[0].Observation.Retry.DelayMillis != 1000 {
		t.Fatalf("Kotlin defaults not respected: %+v", f)
	}
	partial, err := ParseMobileMediaProfile([]byte(`{
		"id":"image","name":"Image","chain":{"steps":[{"mode":"form","observation":{"enabled":false}}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	f, _ = partial.Fields()
	if f.Chain.Steps[0].Observation.Mode != "value" ||
		f.Chain.Steps[0].Observation.ReadyValues == nil ||
		f.Chain.Steps[0].Observation.Retry.MaxAttempts != 1 ||
		f.Chain.Steps[0].Observation.Enabled == nil ||
		*f.Chain.Steps[0].Observation.Enabled {
		t.Fatalf("partial nested defaults lost: %+v", f.Chain.Steps[0].Observation)
	}
}

func TestMobileMediaProfileInvalidWire(t *testing.T) {
	cases := []string{
		`{}`,
		`{"id":null,"name":"x"}`,
		`{"id":"p","name":null}`,
		`{"id":"p","name":"x","mode":"CUSTOM"}`,
		`{"id":"p","name":"x","mode":null}`,
		`{"id":"p","name":"x","chain":null}`,
		`{"id":"p","name":"x","chain":{"steps":null}}`,
		`{"id":"p","name":"x","chain":{"steps":[null]}}`,
		`{"id":"p","name":"x","chain":{"steps":[{"mode":"xml"}]}}`,
		`{"id":"p","name":"x","chain":{"steps":[{"observation":{"mode":"other"}}]}}`,
		`{"id":"p","name":"x","chain":{"steps":[{"observation":{"retry":{"maxAttempts":0}}}]}}`,
		`{"id":"p","name":"x","chain":{"steps":[{"fileTransport":"data-url"}]}}`,
		`{"id":"p","name":"x","chain":{"finalResponse":{"type":"unknown"}}}`,
		`{"id":"p","name":"x","chain":{"finalResponse":{"sourceStep":2}}}`,
		`{"id":"p","name":"x","encryptedCredential":"plaintext-secret"}`,
		`{"id":"p","name":"x","disabledParams":null}`,
		`{"id":"p","name":"x","options":[]}`,
		`{"id":"p","name":"x","id":"duplicate"}`,
		`{"id":"p","name":"x","options":{"future":1,"future":2}}`,
		`{"id":"p","name":"x"} trailing garbage`,
	}
	for i, raw := range cases {
		if _, err := ParseMobileMediaProfile([]byte(raw)); err == nil {
			t.Errorf("case %d accepted invalid Kotlin profile: %s", i, raw)
		}
	}
}

func TestMobileMediaProfileSecretSafety(t *testing.T) {
	unsafe := []string{
		`"headers":{"Authorization":"Bearer actual-token"}`,
		`"headers":{"X-API-Key":"hardcoded-secret"}`,
		`"endpoint":"https://example.invalid/api?api_key=abc"`,
		`"endpoint":"https://secret@example.invalid/api"`,
		`"body":"{\"api_key\":\"plaintext\"}"`,
	}
	for _, fragment := range unsafe {
		key := strings.SplitN(fragment, ":", 2)[0]
		var raw string
		if key == `"headers"` || key == `"endpoint"` || key == `"body"` {
			raw = `{"id":"p","name":"x","chain":{"steps":[{` + fragment + `}]}}`
		} else {
			t.Fatalf("bad fixture %q", fragment)
		}
		p, err := ParseMobileMediaProfile([]byte(raw))
		if err != nil {
			t.Fatalf("valid private DTO rejected: %v / %s", err, raw)
		}
		if metadata, err := p.StoredMetadataJSON(); err == nil {
			t.Errorf("unsafe catalog metadata accepted: %s", metadata)
		}
	}
	for _, fragment := range []string{
		`"options":{"accessToken":"secret"}`,
		`"future":{"oauthClientSecret":"secret"}`,
		`"future":{"nested":[{"password":"secret"}]}`,
	} {
		raw := []byte(`{"id":"p","name":"x",` + fragment + `}`)
		p, err := ParseMobileMediaProfile(raw)
		if err != nil {
			t.Fatalf("private opaque DTO unexpectedly invalid: %v", err)
		}
		if b, err := p.StoredMetadataJSON(); err == nil {
			t.Fatalf("secret extension written to metadata: %s", b)
		}
	}
}
