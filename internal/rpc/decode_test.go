package rpc

import "testing"

func TestDecodeRejectsAmbiguousRequests(t *testing.T) {
	for _, s := range []string{`{"id":"a","id":"b","op":"session.get"}`, `{"id":"a","op":"x","data":{"runtime":{"source":"stored","source":"mobile_ephemeral"}}}`, `{"id":"a","op":"x"} {}`} {
		if _, err := Decode([]byte(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
	if _, err := Decode([]byte(`{"id":"a","op":"x","data":{"runtime":{"secret":"allowed in RAM"}}}`)); err != nil {
		t.Fatal(err)
	}
}
