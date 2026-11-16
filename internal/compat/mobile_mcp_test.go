package compat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMobileMCPLosslessAndMetadata(t *testing.T) {
	raw := []byte(`{
		"id":"mcp-x","name":"Search","protocol":"sse",
		"url":"https://example.invalid/mcp","headers":{"Accept":"application/json"},
		"auth":{"method":"BEARER_TOKEN","value":"redacted-secret","key":null,
			"oauthClientId":"client-display","oauthClientSecret":"oauth-private","oauthAuthorized":false},
		"cachedTools":[{"name":"lookup","description":"Look up","schema":{"type":"object","properties":{}},
			"serverId":"mcp-x","originalName":"lookup","future":{"big":9007199254740993}}],
		"sortOrder":42,"icon":null,"futureExtension":{"empty":[],"nil":null}
	}`)
	m, err := ParseMobileMCP(raw)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if e := json.Compact(&compact, raw); e != nil {
		t.Fatal(e)
	}
	out, err := json.Marshal(m)
	if err != nil || !bytes.Equal(out, compact.Bytes()) || !bytes.Equal(m.Bytes(), raw) {
		t.Fatalf("MCP round trip lost fields: %v %s", err, out)
	}
	f, err := m.Fields()
	if err != nil {
		t.Fatal(err)
	}
	if f.Protocol != "sse" || f.Auth.Method != "BEARER_TOKEN" || f.Auth.Value == nil ||
		*f.Auth.Value != "redacted-secret" || string(f.CachedTools) == "null" ||
		f.SortOrder != 42 || f.Icon != nil {
		t.Fatalf("invalid typed MCP view: %+v", f)
	}
	meta, err := m.StoredMetadataJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"redacted-secret", "oauth-private", "oauthClientSecret", "oauthClientId", "keystore:"} {
		if bytes.Contains(meta, []byte(forbidden)) {
			t.Fatalf("leaked credential %q in metadata: %s", forbidden, meta)
		}
	}
	var decoded map[string]any
	dec := json.NewDecoder(bytes.NewReader(meta))
	dec.UseNumber()
	if e := dec.Decode(&decoded); e != nil {
		t.Fatal(e)
	}
	auth := decoded["auth"].(map[string]any)
	if auth["method"] != "BEARER_TOKEN" || len(auth) != 1 {
		t.Fatalf("unsafe MCP auth projection: %+v", auth)
	}
	tools := decoded["cachedTools"].([]any)
	large := tools[0].(map[string]any)["future"].(map[string]any)["big"].(json.Number)
	if large.String() != "9007199254740993" {
		t.Fatalf("lost unknown numeric extension precision: %s", large)
	}
	if _, ok := decoded["futureExtension"]; !ok {
		t.Fatal("unknown extension lost")
	}
}

func TestMobileMCPNullVsEmptyAndDefaults(t *testing.T) {
	for _, expected := range []string{"", "null", "[]"} {
		extra := ""
		if expected != "" {
			extra = `,"cachedTools":` + expected
		}
		raw := []byte(`{"id":"m","name":"M"}`[:len(`{"id":"m","name":"M"}`)-1] + extra + `}`)
		m, err := ParseMobileMCP(raw)
		if err != nil {
			t.Fatal(err)
		}
		f, _ := m.Fields()
		if string(f.CachedTools) != expected || f.Protocol != "streamable_http" ||
			f.Auth.Method != "NONE" || f.SortOrder != 2147483647 {
			t.Fatalf("defaults/null/empty mismatch: %+v wanted %q", f, expected)
		}
	}
}

func TestMobileMCPRejectsBadShape(t *testing.T) {
	invalid := []string{
		`{"id":"","name":"x"}`,
		`{"id":"m","name":"x","protocol":"STREAMABLE_HTTP"}`,
		`{"id":"m","name":"x","auth":{"method":"none"}}`,
		`{"id":"m","name":"x","auth":null}`,
		`{"id":"m","name":"x","cachedTools":{}}`,
		`{"id":"m","name":"x","cachedTools":[{}]}`,
		`{"id":"m","name":"x","cachedTools":[{"name":"t","description":"x","schema":[]}]}`,
		`{"id":"m","name":"x","url":"https://user:pass@example.invalid/mcp"}`,
		`{"id":"m","name":"x","url":"https://example.invalid/mcp#secret"}`,
		`{"id":"m","name":"x","id":"n"}`,
		`{"id":"m","name":"x","other":{"a":1,"a":2}}`,
	}
	for _, s := range invalid {
		if _, err := ParseMobileMCP([]byte(s)); err == nil {
			t.Errorf("accepted malformed mobile MCP: %s", s)
		}
	}
}

func TestMobileMCPRejectsUnsafeMetadata(t *testing.T) {
	cases := []string{
		`{"id":"x","name":"X","headers":{"Authorization":"Bearer SECRET"}}`,
		`{"id":"x","name":"X","headers":{"X-API-KEY":"SECRET"}}`,
		`{"id":"x","name":"X","url":"https://example.invalid/mcp?access_token=SECRET"}`,
		`{"id":"x","name":"X","future":{"password":"SECRET"}}`,
		`{"id":"x","name":"X","future":{"encryptedCredential":"keystore:v1:opaque"}}`,
		`{"id":"x","name":"X","url":"https://example.invalid/mcp?key=opaque"}`,
		`{"id":"x","name":"X","auth":{"method":"CUSTOM_HEADER","key":"X-Foo: Evil","value":"secret"}}`,
	}
	for _, raw := range cases {
		m, err := ParseMobileMCP([]byte(raw))
		if err != nil {
			t.Fatalf("private mobile data cannot be parsed: %v / %s", err, raw)
		}
		if b, err := m.StoredMetadataJSON(); err == nil {
			t.Errorf("unsafe MCP metadata accepted: %s", b)
		}
	}
}

func TestMobileChatProfileWireAndDefaults(t *testing.T) {
	raw := []byte(`{"id":"chat-profile","name":"Research","icon":null,
	"config":{"system_prompt":"Keep results short","mcpAccess":{"srv":{"enabled":true,"tools":{"lookup":false}}}},
	"active_mcp":["srv"],"active_model_tools":[],"extra":null}`)
	p, err := ParseMobileChatProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, err := p.Fields()
	if err != nil {
		t.Fatal(err)
	}
	if f.Config.SystemPrompt != "Keep results short" || !f.Config.MCPAccess["srv"].Enabled ||
		f.Config.MCPAccess["srv"].Tools["lookup"] || len(f.ActiveMCP) != 1 ||
		f.ActiveModelTools == nil || f.SortOrder != 2147483647 {
		t.Fatalf("chat profile fields: %+v", f)
	}
	encoded, err := json.Marshal(p)
	var compact bytes.Buffer
	if e := json.Compact(&compact, raw); e != nil {
		t.Fatal(e)
	}
	if err != nil || !bytes.Equal(encoded, compact.Bytes()) || !bytes.Equal(p.Bytes(), raw) {
		t.Fatal("chat profile roundtrip failed")
	}
	missing, err := ParseMobileChatProfile([]byte(`{"id":"c","name":"C","config":{"mcpAccess":{"s":{"enabled":true}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := missing.Fields()
	if fields.Config.MCPAccess["s"].Tools == nil || fields.ActiveMCP == nil ||
		fields.ActiveModelTools == nil {
		t.Fatal("Kotlin empty map/list defaults lost")
	}
}

func TestMobileChatProfileRejectsInvalid(t *testing.T) {
	bad := []string{
		`{}`, `{"id":"c","name":null}`,
		`{"id":"c","name":"x","active_mcp":null}`,
		`{"id":"c","name":"x","active_model_tools":[null]}`,
		`{"id":"c","name":"x","config":{"mcpAccess":null}}`,
		`{"id":"c","name":"x","config":{"mcpAccess":{"s":{"tools":[]}}}}`,
		`{"id":"c","name":"x","config":{"mcpAccess":{"s":null}}}`,
		`{"id":"c","name":"x","config":null}`,
		`{"id":"c","name":"x","extra":{"secret":1,"secret":2}}`,
	}
	for _, raw := range bad {
		if _, err := ParseMobileChatProfile([]byte(raw)); err == nil {
			t.Errorf("accepted invalid ChatProfile: %s", raw)
		}
	}
	// Chat profile prompts are user data, not an RPC bearer token or pairing URI.
	p, _ := ParseMobileChatProfile([]byte(`{"id":"c","name":"x","config":{"system_prompt":"hello"}}`))
	if !strings.Contains(string(p.Bytes()), "hello") {
		t.Fatal("prompt lost")
	}
}
