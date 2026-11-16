package httptool

import "testing"

func TestToolValidation(t *testing.T) {
	good := `{"id":"t","name":"weather","method":"POST","url":"https://example.invalid","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"headers":{"X-Search":"{{q}}"},"body":"{\"q\":\"{{q}}\"}","responseMapping":{"text":"/result/0"}}`
	if id, err := Validate([]byte(good)); err != nil || id != "t" {
		t.Fatalf("valid: %s %v", id, err)
	}
	cases := []string{`{"id":"t","name":"bad-name","url":"https://example.invalid","parameters":{"type":"object","properties":{}}}`, `{"id":"t","name":"tool","url":"https://user:secret@example.invalid","parameters":{"type":"object","properties":{}}}`, `{"id":"t","name":"tool","url":"https://example.invalid","parameters":{"type":"object","properties":{}},"headers":{"A":"{{unknown}}"}}`, `{"id":"t","name":"tool","url":"https://example.invalid","parameters":{"type":"object","properties":{}},"responseMapping":{"x":"/bad~3escape"}}`, `{"id":"t","name":"tool","url":"https://example.invalid","parameters":{"type":"object","properties":{}},"headers":{"Authorization":"bad\r\ninjection"}}`}
	for _, s := range cases {
		if _, err := Validate([]byte(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
