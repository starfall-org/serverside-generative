package compat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderMobileRoundTripNullVsEmptyAndSafeMetadata(t *testing.T) {
	for _, ids := range []string{"null", "[]", `["org/model"]`} {
		raw := []byte(`{"id":"provider-1","name":"Provider","type":"openai","baseUrl":"https://example.invalid/v1",
  "auth":{"method":"custom_header","key":"X-API-Key","value":"PRIVATE-PROVIDER-KEY"},
  "config":{"modelIds":` + ids + `,"multipleApiKeys":true,
   "apiKeys":[{"id":"main","label":"Main","value":"SECRET-MAIN"},{"id":"backup","label":"Backup","value":"SECRET-BACKUP"}],
   "modelConfigs":{"org/model":{"modelType":"TEXT_GENERATION","model_json":{"huge":9007199254740993}}}},
  "groupId":"g","isHidden":true,"future":{"value":null,"list":[]}}`)
		p, err := ParseMobileProvider(raw)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err = json.Compact(&buf, raw); err != nil {
			t.Fatal(err)
		}
		output, err := json.Marshal(p)
		if err != nil || !bytes.Equal(output, buf.Bytes()) || !bytes.Equal(raw, p.Bytes()) {
			t.Fatalf("provider round trip changed %s / %v", output, err)
		}
		f, err := p.Fields()
		if err != nil || f.ID != "provider-1" || !f.IsHidden || f.GroupID == nil || *f.GroupID != "g" {
			t.Fatalf("typed provider: %+v %v", f, err)
		}
		var config map[string]json.RawMessage
		if err = json.Unmarshal(f.Config, &config); err != nil || string(config["modelIds"]) != ids {
			t.Fatalf("modelIds null-vs-empty corrupted: %s, %v", config["modelIds"], err)
		}
		meta, err := p.StoredMetadataJSON()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(meta, []byte("PRIVATE-PROVIDER-KEY")) ||
			bytes.Contains(meta, []byte("SECRET-MAIN")) || bytes.Contains(meta, []byte("SECRET-BACKUP")) ||
			bytes.Contains(meta, []byte("oauthClientSecret")) {
			t.Fatalf("credential in metadata: %s", meta)
		}
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(meta))
		dec.UseNumber()
		if err = dec.Decode(&obj); err != nil {
			t.Fatal(err)
		}
		keys := obj["config"].(map[string]any)["apiKeys"].([]any)
		if len(keys) != 2 || keys[1].(map[string]any)["label"] != "Backup" {
			t.Fatalf("provider API-key labels/order lost: %+v", keys)
		}
		big := obj["config"].(map[string]any)["modelConfigs"].(map[string]any)["org/model"].(map[string]any)["model_json"].(map[string]any)["huge"].(json.Number)
		if big.String() != "9007199254740993" {
			t.Fatal("model extension lost precision")
		}
	}
}

func TestProviderMobileRejectsMalformedFields(t *testing.T) {
	for _, raw := range []string{
		`{"id":"p","name":"x","type":"openai"}`,
		`{"id":"p","name":"x","type":"OPENAI","baseUrl":""}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"https://user:secret@example.invalid"}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","auth":{"method":"BEARER_TOKEN"}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","config":{"modelIds":[null]}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","config":{"modelIds":[12]}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","config":{"apiKeys":[{"id":"a","label":"A"},{"id":"a","label":"B"}]}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","config":{"modelConfigs":{"id":{"modelType":"image_generation"}}}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","config":{"modelConfigs":{"id":{"modelType":"TEXT_GENERATION","mediaApiType":"CUSTOM"}}}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","config":{"headers":null}}`,
		`{"id":"p","name":"x","type":"openai","baseUrl":"","future":{"unknown":1,"unknown":2}}`,
	} {
		if _, err := ParseMobileProvider([]byte(raw)); err == nil {
			t.Errorf("accepted malformed provider: %s", raw)
		}
	}
}

func TestProviderGroupsLanguageModelsAndConfigs(t *testing.T) {
	group := []byte(`{"id":"g","name":"Group","sortOrder":3,"icon":null,"unknown":{}}`)
	parsed, err := ParseMobileProviderGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(parsed)
	if err != nil || !bytes.Equal(out, group) {
		t.Fatal("group round trip lost fields")
	}
	gf, err := parsed.Fields()
	if err != nil || gf.SortOrder != 3 {
		t.Fatalf("group fields: %+v %v", gf, err)
	}

	catalog := []byte(`{"id":"org/model","display_name":"Model","provider_id":"p","icon":null,
  "input_capabilities":{"text":true,"image":false},
  "output_capabilities":{"text":true},"model_tools":[],"future":null}`)
	model, err := ParseMobileLanguageModel(catalog)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := model.Fields()
	if err != nil || mf.ID != "org/model" || mf.ProviderID != "p" {
		t.Fatalf("model fields: %+v %v", mf, err)
	}
	out, err = json.Marshal(model)
	var compact bytes.Buffer
	if e := json.Compact(&compact, catalog); e != nil {
		t.Fatal(e)
	}
	if err != nil || !bytes.Equal(out, compact.Bytes()) {
		t.Fatal("language model metadata changed")
	}

	config := []byte(`{"modelType":"IMAGE_GENERATION","mediaApiType":"custom","model_json":{"future":true},"top_p":null,"model_tools":["generate_image"]}`)
	m, err := ParseMobileModelConfiguration(config)
	if err != nil {
		t.Fatal(err)
	}
	f, err := m.Fields()
	if err != nil || f.ModelType != "IMAGE_GENERATION" || f.MediaApiType != "custom" {
		t.Fatalf("config fields: %+v %v", f, err)
	}
	out, err = json.Marshal(m)
	if err != nil || !bytes.Equal(out, config) {
		t.Fatal("model config lost fields")
	}
}

func TestProviderMetadataRejectsSecretInExtensions(t *testing.T) {
	for _, extra := range []string{
		`"future":{"apiToken":"PRIVATE"}`,
		`"config":{"headers":{"Authorization":"Bearer PRIVATE"}}`,
		`"config":{"customEndpoints":{"private":"https://example.invalid/?key=PRIVATE"}}`,
		`"auth":{"method":"custom_header","key":"Authorization:PRIVATE","value":"PRIVATE"}`,
	} {
		raw := []byte(`{"id":"p","name":"P","type":"openai","baseUrl":"",` + extra + `}`)
		p, err := ParseMobileProvider(raw)
		if err != nil {
			t.Fatalf("private mobile DTO rejected: %v, %s", err, raw)
		}
		meta, err := p.StoredMetadataJSON()
		if err == nil {
			t.Errorf("metadata should reject credentials: %s", meta)
		}
	}
}

func TestProviderEnumsExactlyKotlin(t *testing.T) {
	for _, typ := range []string{"openai", "openai_responses", "anthropic", "google", "ollama", "antigravity", "github_copilot", "openai_codex", "claude_code", "grok_build", "meta_muse", "gemini_cli", "devin"} {
		raw := []byte(`{"id":"p","name":"P","type":"` + typ + `","baseUrl":""}`)
		if _, err := ParseMobileProvider(raw); err != nil {
			t.Errorf("missing Kotlin provider enum: %s (%v)", typ, err)
		}
	}
	if _, err := ParseMobileProvider([]byte(`{"id":"p","name":"P","type":"gemini","baseUrl":""}`)); err == nil {
		t.Fatal("accepted legacy runtime alias as mobile enum")
	}
	_ = strings.Compare
}
