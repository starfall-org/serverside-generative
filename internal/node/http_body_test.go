package node

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingBody) Close() error             { return nil }

func TestReadRPCBody(t *testing.T) {
	tests := []struct {
		name       string
		body       io.ReadCloser
		wantStatus int
		wantBody   string
		wantOK     bool
	}{
		{"at limit", io.NopCloser(strings.NewReader("1234")), http.StatusOK, "1234", true},
		{"over limit", io.NopCloser(strings.NewReader("12345")), http.StatusRequestEntityTooLarge, "payload_too_large", false},
		{"read failure", failingBody{}, http.StatusBadRequest, "invalid_request", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Body = tt.body
			w := httptest.NewRecorder()
			got, ok := readRPCBody(w, r, 4)
			if ok != tt.wantOK || w.Code != tt.wantStatus {
				t.Fatalf("ok=%v status=%d", ok, w.Code)
			}
			if ok && string(got) != tt.wantBody {
				t.Fatalf("body=%q", got)
			}
			if !ok && !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Fatalf("response=%s", w.Body.String())
			}
		})
	}
}
