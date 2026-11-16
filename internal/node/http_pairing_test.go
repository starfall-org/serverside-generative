package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHTTPPairingIsScopedBoundAndSingleUse(t *testing.T) {
	identity := t.TempDir()
	n, err := New(Config{IdentityDir: identity, DataDir: t.TempDir() + "/private-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	server := httptest.NewTLSServer(n.HTTPHandler())
	defer server.Close()
	pinBytes := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	pin := "sha256/" + base64.StdEncoding.EncodeToString(pinBytes[:])
	if _, err = n.issueHTTPPairURL("http://example.invalid", pin); err == nil {
		t.Fatal("plaintext endpoint accepted")
	}
	if _, err = n.issueHTTPPairURL(server.URL, "sha256/not-a-pin"); err == nil {
		t.Fatal("invalid pin accepted")
	}
	issue := func() string {
		value, err := n.issueHTTPPairURL(server.URL, pin)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(value["url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		query, err := url.ParseQuery(u.EscapedFragment())
		if err != nil || query.Get("pin") != pin || query.Get("v") != "1" || query.Get("code") == "" {
			t.Fatal("invalid pairing URL")
		}
		return query.Get("code")
	}
	post := func(code, host, origin string) (*http.Response, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"code": code})
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/pair", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response, result
	}
	code := issue()
	if response, _ := post(code, "other.invalid", ""); response.StatusCode != 401 {
		t.Fatalf("wrong host: %d", response.StatusCode)
	}
	if response, _ := post(code, "", "https://other.invalid"); response.StatusCode != 403 {
		t.Fatalf("wrong origin: %d", response.StatusCode)
	}
	response, result := post(code, "", "")
	if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("pair: %d", response.StatusCode)
	}
	id, ok := result["client_id"].(string)
	if !ok || id == "" {
		t.Fatal("missing client id")
	}
	secret, ok := result["secret"].(string)
	if !ok || secret == "" {
		t.Fatal("missing secret")
	}
	if !n.verifyPairFor(id, secret, "http") || n.verifyPairFor(id, secret, "tcp") {
		t.Fatal("wrong identity scope")
	}
	if response, _ := post(code, "", ""); response.StatusCode != 401 {
		t.Fatalf("code replay: %d", response.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(identity, "clients.json"))
	if err != nil || bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(code)) {
		t.Fatalf("plaintext identity data: %v", err)
	}

	concurrent := issue()
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); response, _ := post(concurrent, "", ""); statuses <- response.StatusCode }()
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == 200 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent successes: %d", successes)
	}
	expired := issue()
	n.mu.Lock()
	hash := sha256.Sum256([]byte(expired))
	entry := n.httpPairCodes[hash]
	entry.expires = time.Now().Add(-time.Second)
	n.httpPairCodes[hash] = entry
	n.mu.Unlock()
	if response, _ := post(expired, "", ""); response.StatusCode != 401 {
		t.Fatalf("expired code: %d", response.StatusCode)
	}
}
