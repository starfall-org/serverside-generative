package compat

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestConversationRoundTrip(t *testing.T) {
	raw := []byte(`{"id":"c","title":"x","created_at":1700000000000,"updated_at":1700000000001,"messages":[{"id":"m","role":"model","versions":[{"toolActivity":[{"id":"t","name":"tool","arguments":"{\"x\":1}","response":"null"}],"reasoning_content":null},{"content":"hi","future":[null,{}]}],"active_version_index":1}],"selected_prompt_ids":null,"remote_workspaces":{},"extension":{"value":9007199254740993}}`)
	c, err := ParseConversation(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	da, db := json.NewDecoder(bytesReader(raw)), json.NewDecoder(bytesReader(out))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&a) != nil || db.Decode(&b) != nil {
		t.Fatal("invalid JSON")
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatalf("lost data: %s", out)
	}
}
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
func TestConversationRejectsInvalidStructure(t *testing.T) {
	cases := []string{
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"id":"d"}`,
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[{"id":"m","role":"assistant"}]}`,
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[{"id":"m","role":"model","versions":[{}],"active_version_index":2}]}`,
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[{"id":"m","role":"model","versions":[{"toolActivity":[{"id":"t","name":"tool","arguments":{}}]}]}]}`,
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[],"summary":{"id":"s","content":"x","through_message_id":"missing"}}`,
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[{"id":"m","role":"user"},{"id":"m","role":"model"}]}`,
		`{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[],"runtime":{"api_key":"secret"}}`,
	}
	for _, v := range cases {
		if _, err := ParseConversation([]byte(v)); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
}

func TestKotlinConversationFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/conversation-mobile.json")
	if err != nil {
		t.Fatal(err)
	}
	dto, err := ParseConversation(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var first, second any
	if json.Unmarshal(raw, &first) != nil || json.Unmarshal(out, &second) != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("Kotlin conversation changed during round trip")
	}
}

func TestServerConversationFixtureRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/conversation-server.json")
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := ParseConversation(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	first, second := json.NewDecoder(bytes.NewReader(raw)), json.NewDecoder(bytes.NewReader(encoded))
	first.UseNumber()
	second.UseNumber()
	if err = first.Decode(&before); err != nil {
		t.Fatal(err)
	}
	if err = second.Decode(&after); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("server fixture changed: %v", err)
	}
}
