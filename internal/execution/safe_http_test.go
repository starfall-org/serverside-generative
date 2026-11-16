package execution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSafeHTTPClientLoopbackPolicyAndRedirects(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()
	if resp, err := SafeHTTPClient(false).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("loopback accepted without policy")
	}
	if calls.Load() != 0 {
		t.Fatal("rejected endpoint was contacted")
	}
	resp, err := SafeHTTPClient(true).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || calls.Load() != 1 {
		t.Fatalf("redirect followed: %d", resp.StatusCode)
	}
	if resp, err = SafeHTTPClient(false).Get("http://169.254.169.254/latest/meta-data"); err == nil {
		resp.Body.Close()
		t.Fatal("metadata endpoint accepted")
	}
}

func TestWireAuthModes(t *testing.T) {
	seen := make(chan *http.Request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cases := []struct{ method, key, value, header, query string }{{"none", "", "", "", ""}, {"bearer_token", "", "bearer-secret", "Bearer bearer-secret", ""}, {"custom_header", "X-Custom", "custom-secret", "", ""}, {"query_param", "auth", "query-secret", "", "query-secret"}}
	for _, tt := range cases {
		client := WithWireAuth(SafeHTTPClient(true), tt.method, tt.key, tt.value)
		req, _ := http.NewRequest("GET", srv.URL+"/?key=unused", nil)
		req.Header.Set("Authorization", "Bearer unused")
		req.Header.Set("X-Api-Key", "unused")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := <-seen
		if got.Header.Get("Authorization") != tt.header || got.Header.Get("X-Api-Key") != "" || got.URL.Query().Get("key") != "" || got.URL.Query().Get("auth") != tt.query {
			t.Fatalf("method %s not applied", tt.method)
		}
		if tt.method == "custom_header" && got.Header.Get("X-Custom") != tt.value {
			t.Fatal("custom header missing")
		}
	}
	if ValidAuthKey("custom_header", "Host") || ValidAuthKey("custom_header", "Bad\r\nName") {
		t.Fatal("invalid header accepted")
	}
}

func TestStreamingDoesNotRetryAfterFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	s := State{HTTPClient: SafeHTTPClient(true), Provider: Provider{Platform: "openai-chat", APIKey: "secret", Model: "model", BaseURL: srv.URL}, Conversation: []Message{{Role: "user", Content: "hello"}}}
	if err := Stream(context.Background(), s, func(string) {}); err == nil {
		t.Fatal("expected upstream failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("retried %d times", calls.Load())
	}
}
