package node

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedWeb(t *testing.T) {
	mux := http.NewServeMux()
	registerWeb(mux)
	var asset string
	for _, path := range []string{"/", "/connect"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ServerSide Studio") {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Content-Security-Policy") == "" || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s missing security headers", path)
		}
		match := regexp.MustCompile(`src="(/assets/[^"]+\.js)"`).FindStringSubmatch(rr.Body.String())
		if len(match) != 2 {
			t.Fatalf("%s missing asset", path)
		}
		asset = match[1]
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", asset, nil))
	if rr.Code != http.StatusOK || rr.Body.Len() == 0 {
		t.Fatalf("asset: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/unknown", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rr.Code)
	}
}
