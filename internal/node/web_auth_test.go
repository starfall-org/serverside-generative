package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starfall-org/generative-server/internal/rpc"
)

const testWebPassword = "browser-test-passphrase"

type webAuthTestClient struct {
	t       *testing.T
	handler http.Handler
	origin  string
	cookie  *http.Cookie
	version string
}

func (c *webAuthTestClient) request(method, path, body string, alter func(*http.Request)) *httptest.ResponseRecorder {
	c.t.Helper()
	r := httptest.NewRequest(method, c.origin+path, strings.NewReader(body))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", c.origin)
		r.Header.Set("X-Generative-Web", "1")
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
		r.Header.Set("X-Generative-Session", c.version)
	}
	if alter != nil {
		alter(r)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	return w
}

func (c *webAuthTestClient) login(password string) {
	c.t.Helper()
	raw, _ := json.Marshal(map[string]string{"password": password})
	reply := c.request("POST", "/web/auth/login", string(raw), nil)
	if reply.Code != 200 {
		c.t.Fatalf("login: %d %s", reply.Code, reply.Body)
	}
	cookies := reply.Result().Cookies()
	var result struct {
		Version string `json:"session_version"`
	}
	if len(cookies) != 1 || json.Unmarshal(reply.Body.Bytes(), &result) != nil || result.Version == "" {
		c.t.Fatal("missing session cookie or version")
	}
	c.cookie = cookies[0]
	c.version = result.Version
	if c.cookie.Name != "__Host-generative-web" || !c.cookie.HttpOnly || !c.cookie.Secure ||
		c.cookie.Path != "/" || c.cookie.Domain != "" || c.cookie.SameSite != http.SameSiteStrictMode ||
		c.cookie.MaxAge != int(webLifetime/time.Second) || strings.Contains(reply.Body.String(), c.cookie.Value) {
		c.t.Fatal("unsafe session response")
	}
}

func TestWebAuthLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{IdentityDir: filepath.Join(dir, "identity"), DataDir: filepath.Join(dir, "data"), WebOrigin: "https://workspace.test", AllowPublicWebWithoutPassword: true}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	web := webAuthTestClient{t: t, handler: n.HTTPHandler(), origin: cfg.WebOrigin}
	rpcBody := `{"id":"list","op":"session.list","data":{"owner_id":"mobile"}}`
	if w := web.request("GET", "/web/auth/status", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body)
	}
	if w := web.request("POST", "/web/auth/login", `{"password":"remote-first-visitor"}`, nil); w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal(w.Code, w.Body)
	}
	if w := web.request("POST", "/web/rpc", rpcBody, nil); w.Code != 401 {
		t.Fatal(w.Code, w.Body)
	}
	if _, err := os.Stat(n.webAuth.path); !os.IsNotExist(err) {
		t.Fatalf("unexpected web auth store: %v", err)
	}
	control, err := StartIPCSocket(n.Config.IPCSocket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.ServeIPC(ctx, control)
	set := func(method, password string) error {
		_, err := ControlRequestFields(n.Config.IPCSocket, map[string]string{"method": method, "password": password})
		return err
	}
	if err := set("web_password_set", testWebPassword); err != nil {
		t.Fatal(err)
	}
	if err := set("web_password_set", "attempted-overwrite"); err == nil {
		t.Fatal("set overwrote configured password")
	}
	if w := web.request("POST", "/web/auth/login", `{"password":"wrong"}`, nil); w.Code != 401 {
		t.Fatal(w.Code, w.Body)
	}
	web.login(testWebPassword)
	old := web
	web.login(testWebPassword)
	if web.cookie.Value == old.cookie.Value || old.request("POST", "/web/rpc", rpcBody, nil).Code != 401 {
		t.Fatal("login did not rotate the old cookie")
	}
	if w := web.request("POST", "/web/rpc", rpcBody, func(r *http.Request) { r.Header.Set("X-Generative-Session", old.version) }); w.Code != 401 {
		t.Fatal("stale UI request accepted a replacement cookie", w.Code)
	}
	info, err := os.Stat(n.webAuth.path)
	raw, readErr := os.ReadFile(n.webAuth.path)
	if err != nil || readErr != nil || info.Mode().Perm() != 0600 ||
		strings.Contains(string(raw), testWebPassword) || strings.Contains(string(raw), web.cookie.Value) ||
		!strings.Contains(string(raw), webTokenHash(web.cookie.Value)) || n.webAuth.state.Password.Salt == nil {
		t.Fatal("unsafe persisted web credentials")
	}

	n.mu.Lock()
	mobile, secret, err := n.createPairLocked("http")
	n.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	mobileToken, err := n.issueAPIToken(mobile)
	if err != nil {
		t.Fatal(err)
	}
	open := func(owner, id string) rpc.Reply {
		return n.rpc.Dispatch(owner, rpc.Request{ID: "open", Op: "session.open", SessionID: id,
			Data: json.RawMessage(`{"conversation":{"id":"` + id + `","title":"Owner history","created_at":1,"updated_at":1,"messages":[]}}`)})
	}
	if result := open(mobile, "legacy-browser"); !result.OK {
		t.Fatal(result.Error)
	}
	if w := web.request("POST", "/web/rpc", `{"id":"open","op":"session.open","session_id":"web-chat","data":{"conversation":{"id":"web-chat","title":"Web history","created_at":1,"updated_at":1,"messages":[]}}}`, nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := web.request("POST", "/web/rpc", `{"id":"get","op":"session.get","session_id":"legacy-browser","data":{"owner_id":"`+mobile+`"}}`, nil); w.Code != 404 {
		t.Fatal("web could access the old browser pairing", w.Code, w.Body)
	}
	if w := web.request("POST", "/web/rpc", `{"id":"native","op":"terminal.list"}`, nil); w.Code != 403 {
		t.Fatal("browser gained native terminal access", w.Code, w.Body)
	}
	for _, path := range []string{"/web/rpc", "/web/auth/logout"} {
		if w := web.request("POST", path, "{}", func(r *http.Request) { r.Header.Del("Origin") }); w.Code != 403 {
			t.Fatal("mutation without an Origin accepted", path, w.Code, w.Body)
		}
	}
	if w := web.request("POST", "/v1/rpc", rpcBody, nil); w.Code != 401 {
		t.Fatal("cookie authorized mobile RPC", w.Code)
	}
	withoutCookie := web
	withoutCookie.cookie = nil
	if w := withoutCookie.request("POST", "/web/rpc", rpcBody, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+mobileToken) }); w.Code != 401 {
		t.Fatal("mobile bearer authorized browser RPC", w.Code)
	}

	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	web.handler = restarted.HTTPHandler()
	get := `{"id":"open-existing","op":"session.open","session_id":"web-chat"}`
	if w := web.request("POST", "/web/rpc", get, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Web history") {
		t.Fatal("history lost after restart", w.Code, w.Body)
	}
	if w := web.request("POST", "/web/rpc", `{"id":"runtime","op":"session.configure","session_id":"web-chat","data":{"runtime":{"source":"stored"}}}`, nil); w.Code != 200 {
		t.Fatal("runtime setup for logout test failed", w.Code, w.Body)
	}
	if w := web.request("POST", "/web/auth/logout", "{}", nil); w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal(w.Code, w.Body)
	}
	if w := web.request("POST", "/web/rpc", get, nil); w.Code != 401 {
		t.Fatal("logged-out cookie replay", w.Code)
	}
	web.login(testWebPassword)
	if w := web.request("POST", "/web/rpc", get, nil); w.Code != 200 {
		t.Fatal("history lost after relogin", w.Code, w.Body)
	}
	if w := web.request("POST", "/web/rpc", `{"id":"run","op":"session.run","session_id":"web-chat","data":{"request_id":"run","content":"new prompt"}}`, nil); !strings.Contains(w.Body.String(), "missing_runtime_config") {
		t.Fatal("last browser logout left runtime configuration available", w.Code, w.Body)
	}
	salt := append([]byte(nil), restarted.webAuth.state.Password.Salt...)
	if err := restarted.webAuth.setPassword("replacement-password", true); err != nil {
		t.Fatal(err)
	}
	if string(salt) == string(restarted.webAuth.state.Password.Salt) || web.request("POST", "/web/rpc", get, nil).Code != 401 {
		t.Fatal("password reset failed to rotate salt or revoke sessions")
	}
	web.login("replacement-password")
	if w := web.request("POST", "/web/rpc", get, nil); w.Code != 200 {
		t.Fatal("history lost after password reset", w.Code, w.Body)
	}
	if !restarted.verifyPairFor(mobile, secret, "http") {
		t.Fatal("web reset revoked mobile identity")
	}
	if result := restarted.rpc.Dispatch(mobile, rpc.Request{ID: "get", Op: "session.open", SessionID: "legacy-browser"}); !result.OK {
		t.Fatal("legacy history lost", result.Error)
	}
	if result := restarted.rpc.Dispatch(mobile, rpc.Request{ID: "get", Op: "session.open", SessionID: "web-chat"}); result.OK {
		t.Fatal("mobile identity could access web history")
	}
	for _, absolute := range []bool{true, false} {
		restarted.webAuth.mu.Lock()
		key := webTokenHash(web.cookie.Value)
		session := restarted.webAuth.state.Sessions[key]
		original := session
		if absolute {
			session.Expires = time.Now().Add(-time.Second)
		} else {
			session.LastUsed = time.Now().Add(-webIdle - time.Second)
		}
		restarted.webAuth.state.Sessions[key] = session
		restarted.webAuth.mu.Unlock()
		if w := web.request("GET", "/web/auth/status", "", nil); !strings.Contains(w.Body.String(), `"authenticated":false`) {
			t.Fatal("status accepted expired session", w.Code, w.Body)
		}
		if w := web.request("GET", "/web/events?session_id=web-chat", "", nil); w.Code != 401 {
			t.Fatal("events accepted expired session", w.Code, w.Body)
		}
		restarted.webAuth.mu.Lock()
		restarted.webAuth.state.Sessions[key] = original
		restarted.webAuth.mu.Unlock()
	}
	if err := restarted.webAuth.revokeAll(); err != nil || web.request("POST", "/web/rpc", get, nil).Code != 401 {
		t.Fatal("administrator revocation failed", err)
	}
}

func TestWebAuthRequestPolicyAndThrottle(t *testing.T) {
	c := Config{IdentityDir: t.TempDir(), DataDir: t.TempDir(), WebOrigin: "https://workspace.test", WebTrustedProxy: "127.0.0.1"}
	n, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if err = n.webAuth.setPassword(testWebPassword, false); err != nil {
		t.Fatal(err)
	}
	web := webAuthTestClient{t: t, handler: n.HTTPHandler(), origin: c.WebOrigin}
	for name, change := range map[string]func(*http.Request){
		"unexpected host":     func(r *http.Request) { r.Host = "other.test" },
		"wrong origin":        func(r *http.Request) { r.Header.Set("Origin", "https://other.test") },
		"duplicate origin":    func(r *http.Request) { r.Header.Add("Origin", c.WebOrigin) },
		"missing origin":      func(r *http.Request) { r.Header.Del("Origin") },
		"cross-site":          func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"missing CSRF header": func(r *http.Request) { r.Header.Del("X-Generative-Web") },
		"form":                func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"proxy spoof": func(r *http.Request) {
			r.TLS = nil
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
		},
		"loopback plain proxy": func(r *http.Request) { r.TLS = nil; r.RemoteAddr = "127.0.0.1:10000" },
		"ambiguous proxy protocol": func(r *http.Request) {
			r.TLS = nil
			r.RemoteAddr = "127.0.0.1:10000"
			r.Header.Set("X-Forwarded-Proto", "https, http")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if w := web.request("POST", "/web/auth/login", `{"password":"`+testWebPassword+`"}`, change); w.Code != 403 || len(w.Result().Cookies()) != 0 {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
	if w := web.request("POST", "/web/auth/login", strings.Repeat(" ", 4097), nil); w.Code != 413 {
		t.Fatal(w.Code, w.Body)
	}
	for i := 0; i < 5; i++ {
		if w := web.request("POST", "/web/auth/login", `{"password":"wrong"}`, nil); w.Code != 401 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := web.request("POST", "/web/auth/login", `{"password":"`+testWebPassword+`"}`, nil); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code, w.Body)
	}
}

func TestWebSessionsThroughTLSReverseProxy(t *testing.T) {
	front := httptest.NewUnstartedServer(nil)
	defer front.Close()
	origin := "https://" + front.Listener.Addr().String()
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data"), WebOrigin: origin, WebTrustedProxy: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if err = n.webAuth.setPassword(testWebPassword, false); err != nil {
		t.Fatal(err)
	}
	back := httptest.NewServer(n.HTTPHandler())
	defer back.Close()
	target, _ := url.Parse(back.URL)
	front.Config.Handler = &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host
		r.SetXForwarded()
	}}
	front.StartTLS()
	client := front.Client()
	client.Timeout = 5 * time.Second
	do := func(method, path, body, version string, cookie *http.Cookie) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, front.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", front.URL)
			req.Header.Set("X-Generative-Web", "1")
		}
		if cookie != nil {
			req.AddCookie(cookie)
			req.Header.Set("X-Generative-Session", version)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	login := do("POST", "/web/auth/login", `{"password":"`+testWebPassword+`"}`, "", nil)
	var result struct {
		Version string `json:"session_version"`
	}
	if login.StatusCode != 200 || json.NewDecoder(login.Body).Decode(&result) != nil || len(login.Cookies()) != 1 {
		login.Body.Close()
		t.Fatal("proxy login failed", login.Status)
	}
	login.Body.Close()
	cookie := login.Cookies()[0]
	if !cookie.Secure {
		t.Fatal("HTTPS proxy issued insecure cookie")
	}
	open := do("POST", "/web/rpc", `{"id":"open","op":"session.open","session_id":"stream","data":{"conversation":{"id":"stream","title":"Stream","created_at":1,"updated_at":1,"messages":[]}}}`, result.Version, cookie)
	openBody, _ := io.ReadAll(open.Body)
	open.Body.Close()
	if open.StatusCode != 200 {
		t.Fatal(open.Status, string(openBody))
	}

	stream := do("GET", "/web/events?session_id=stream", "", result.Version, cookie)
	defer stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("stream failed", stream.Status)
	}
	logout := do("POST", "/web/auth/logout", "{}", result.Version, cookie)
	logout.Body.Close()
	if logout.StatusCode != 200 {
		t.Fatal(logout.Status)
	}
	if _, err = io.Copy(io.Discard, stream.Body); err != nil {
		t.Fatal("revoked stream did not close before timeout", err)
	}
}

func TestWebLoopbackPolicy(t *testing.T) {
	for _, cfg := range []Config{
		{HTTPListen: "0.0.0.0:7091", WebOrigin: "http://127.0.0.1:7091"},
		{HTTPListen: "127.0.0.1:7091", WebOrigin: "http://other.test:7091"},
		{HTTPListen: "127.0.0.1:7091", WebOrigin: "http://127.0.0.1:7091", WebTrustedProxy: "127.0.0.1"},
	} {
		if _, err := newWebAuth(cfg); err == nil {
			t.Fatalf("allowed unsafe loopback configuration: %+v", cfg)
		}
	}
	cfg := Config{IdentityDir: t.TempDir(), WebOrigin: "http://127.0.0.1:7091", HTTPListen: "127.0.0.1:7091"}
	auth, err := newWebAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", cfg.WebOrigin+"/web/auth/status", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7091}))
	if !auth.secureRequest(r) {
		t.Fatal("rejected explicit loopback")
	}
	w := httptest.NewRecorder()
	auth.setCookie(w, "local-token", 10)
	cookie := w.Result().Cookies()[0]
	if cookie.Secure || cookie.Name != "generative-web-local" || !cookie.HttpOnly {
		t.Fatal("wrong loopback cookie")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if auth.secureRequest(r) {
		t.Fatal("accepted proxied loopback")
	}
	r.Header.Del("X-Forwarded-Proto")
	r.TLS = &tls.ConnectionState{}
	if auth.secureRequest(r) {
		t.Fatal("accepted incorrect listener scheme")
	}
}

func TestWebPasswordlessDefaultAndStartupOverrides(t *testing.T) {
	cfg := Config{IdentityDir: t.TempDir(), HTTPListen: "127.0.0.1:7091"}
	restart := func(override *string) *webAuth {
		t.Helper()
		cfg.WebPasswordOverride = override
		a, err := newWebAuth(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	makeSession := func(a *webAuth, bootstrap bool, password string) *http.Cookie {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", a.origin.String(), strings.NewReader("{}"))
		if bootstrap {
			a.bootstrap(w, r)
		} else {
			raw, _ := json.Marshal(map[string]string{"password": password})
			r = httptest.NewRequest("POST", a.origin.String(), strings.NewReader(string(raw)))
			a.login(w, r)
		}
		if w.Code != 200 || len(w.Result().Cookies()) != 1 {
			t.Fatal(w.Code, w.Body)
		}
		return w.Result().Cookies()[0]
	}
	valid := func(a *webAuth, cookie *http.Cookie) bool {
		return a.state.Sessions[webTokenHash(cookie.Value)].valid(time.Now())
	}
	a := restart(nil)
	if a.origin.String() != "http://127.0.0.1:7091" || !a.passwordlessAllowed() {
		t.Fatal("default loopback setup is not passwordless")
	}
	cookie := makeSession(a, true, "")
	a = restart(nil)
	if !valid(a, cookie) {
		t.Fatal("passwordless session lost after restart")
	}

	override := "startup-test-password"
	a = restart(&override)
	if valid(a, cookie) {
		t.Fatal("startup password did not revoke passwordless session")
	}
	blocked := httptest.NewRecorder()
	a.bootstrap(blocked, httptest.NewRequest("POST", a.origin.String(), strings.NewReader("{}")))
	if blocked.Code != 403 {
		t.Fatal("bootstrap bypassed startup password")
	}
	cookie = makeSession(a, false, override)
	a = restart(&override)
	if !valid(a, cookie) {
		t.Fatal("unchanged startup override lost session")
	}
	if err := a.setPassword(testWebPassword, false); err != nil {
		t.Fatal(err)
	}
	if !valid(a, cookie) || !a.state.effectivePassword().matches(override) {
		t.Fatal("saved password changed active override")
	}
	if err := a.unsetPassword(); err != nil {
		t.Fatal(err)
	}
	if !valid(a, cookie) || !a.state.effectivePassword().matches(override) {
		t.Fatal("unset removed active override")
	}
	if err := a.setPassword(testWebPassword, false); err != nil {
		t.Fatal(err)
	}
	changed := "changed-startup-passphrase"
	a = restart(&changed)
	if valid(a, cookie) {
		t.Fatal("changed override kept old session")
	}
	cookie = makeSession(a, false, changed)
	raw, err := os.ReadFile(a.path)
	if err != nil || strings.Contains(string(raw), changed) || strings.Contains(string(raw), testWebPassword) {
		t.Fatal("plaintext override or saved password on disk")
	}
	a = restart(nil)
	if valid(a, cookie) || !a.state.effectivePassword().matches(testWebPassword) {
		t.Fatal("override removal failed to restore saved verifier")
	}
	cookie = makeSession(a, false, testWebPassword)
	if err := a.unsetPassword(); err != nil {
		t.Fatal(err)
	}
	if valid(a, cookie) || a.state.effectivePassword().valid() {
		t.Fatal("unset failed to remove password or revoke protected session")
	}
	makeSession(a, true, "")

	cfg.WebOrigin = "https://workspace.test"
	a = restart(nil)
	if a.passwordlessAllowed() {
		t.Fatal("allowed passwordless public origin by default")
	}
	cfg.AllowPublicWebWithoutPassword = true
	a = restart(nil)
	if !a.passwordlessAllowed() || !makeSession(a, true, "").Secure {
		t.Fatal("opt-in failed to use secure sessions")
	}

	if err := os.WriteFile(a.path, []byte(`{"sessions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWebAuth(cfg); err == nil {
		t.Fatal("corrupt file silently enabled passwordless mode")
	}
}
