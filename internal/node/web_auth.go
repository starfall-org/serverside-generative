package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/starfall-org/generative-server/internal/rpc"
	"golang.org/x/crypto/argon2"
	"golang.org/x/time/rate"
)

// Mobile IDs are exactly 22 base64url characters. This reserved owner is stable
// for the lifetime of the data directory and cannot be issued by mobile pairing.
const webOwner = "generative-web-workspace"
const webIdle = 24 * time.Hour
const webLifetime = 7 * 24 * time.Hour
const webSessionLimit = 64
const webPasswordlessWarning = "WARNING: Web UI has no password. Anyone who can reach it may access your web workspace. Set one with: generative web password set"

type webPassword struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"argon2id_v19_m65536_t3_p1"`
}

type webSession struct {
	Owner    string    `json:"owner"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	LastUsed time.Time `json:"last_used"`
}

func (s webSession) valid(now time.Time) bool {
	return s.Owner == webOwner && now.Before(s.Expires) && now.Before(s.LastUsed.Add(webIdle))
}

type webAuthState struct {
	Password webPassword  `json:"password"`
	Override *webPassword `json:"override,omitempty"`
	// Removal is revocation. No revoked digest remains usable.
	Sessions map[string]webSession `json:"sessions"`
}

func (s webAuthState) effectivePassword() webPassword {
	if s.Override != nil {
		return *s.Override
	}
	return s.Password
}

func (p webPassword) valid() bool {
	return len(p.Hash) == 32 && len(p.Salt) == 16
}

type webAuth struct {
	mu                      sync.Mutex
	passwordWork            sync.Mutex
	path                    string
	origin                  *url.URL
	proxy                   netip.Addr
	allowPublicPasswordless bool
	state                   webAuthState
	attempts                *rate.Limiter
	bootstrapAttempts       *rate.Limiter
	changed                 chan struct{}
	releaseWorkspace        func()
}

func newWebAuth(c Config) (*webAuth, error) {
	a := &webAuth{
		allowPublicPasswordless: c.AllowPublicWebWithoutPassword,
		path:                    filepath.Join(c.IdentityDir, "web-auth.json"),
		attempts:                rate.NewLimiter(rate.Every(10*time.Second), 5),
		bootstrapAttempts:       rate.NewLimiter(rate.Every(time.Second), 10),
		changed:                 make(chan struct{}),
	}
	if c.WebOrigin == "" && c.WebTrustedProxy == "" {
		host, port, err := net.SplitHostPort(c.HTTPListen)
		if err == nil && net.ParseIP(host).IsLoopback() && port != "0" {
			if port == "80" {
				c.WebOrigin = "http://" + strings.TrimSuffix(c.HTTPListen, ":80")
			} else {
				c.WebOrigin = "http://" + c.HTTPListen
			}
		}
	}
	if c.WebOrigin != "" {
		u, err := url.Parse(c.WebOrigin)
		if err != nil || u.User != nil || u.Hostname() == "" || u.String() != u.Scheme+"://"+u.Host ||
			(u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("web origin must be an exact HTTPS origin or an explicit loopback HTTP origin")
		}
		if u.Scheme == "http" {
			ip := net.ParseIP(u.Hostname())
			if !ip.IsLoopback() {
				return nil, errors.New("web HTTP origin requires a numeric loopback address")
			}
			// Compare numeric addresses and the configured listener port.
			host, port, err := net.SplitHostPort(c.HTTPListen)
			originPort := u.Port()
			if originPort == "" {
				originPort = "80"
			}
			if err != nil || !net.ParseIP(host).Equal(ip) || port != originPort {
				return nil, errors.New("web HTTP origin must match the loopback HTTP listener")
			}
		}
		a.origin = u
	}
	if c.WebTrustedProxy != "" {
		ip, err := netip.ParseAddr(c.WebTrustedProxy)
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || a.origin == nil || a.origin.Scheme != "https" {
			return nil, errors.New("web trusted proxy requires one numeric IP and an HTTPS web origin")
		}
		a.proxy = ip.Unmap()
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	if err := a.applyStartupOverride(c.WebPasswordOverride); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *webAuth) load() error {
	info, err := os.Lstat(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 ||
		(DataSocketSupported() && validateDataSocketOwner(info) != nil) || a.checkDirectory() != nil {
		return errors.New("unsafe web authentication store")
	}
	raw, err := os.ReadFile(a.path)
	if err != nil {
		return err
	}
	var disk struct {
		Password *webPassword          `json:"password"`
		Override *webPassword          `json:"override"`
		Sessions map[string]webSession `json:"sessions"`
	}
	var fields struct {
		Password map[string]json.RawMessage `json:"password"`
	}
	if _, err := rpc.Decode(raw); err != nil || json.Unmarshal(raw, &disk) != nil ||
		json.Unmarshal(raw, &fields) != nil || fields.Password["salt"] == nil ||
		fields.Password["argon2id_v19_m65536_t3_p1"] == nil || disk.Password == nil ||
		((disk.Password.Salt != nil || disk.Password.Hash != nil) && !disk.Password.valid()) ||
		(disk.Override != nil && !disk.Override.valid()) || len(disk.Sessions) > webSessionLimit {
		return errors.New("invalid web authentication store")
	}
	a.state = webAuthState{Password: *disk.Password, Override: disk.Override, Sessions: disk.Sessions}
	return nil
}

func (a *webAuth) applyStartupOverride(input *string) error {
	state := a.state
	previous := state.effectivePassword()
	if input == nil {
		if state.Override == nil {
			return nil
		}
		state.Override = nil
	} else {
		if len(*input) < 12 || len(*input) > 512 {
			return errors.New("startup web password must be 12 to 512 bytes")
		}
		if previous.matches(*input) {
			if state.Override != nil {
				return nil
			}
			state.Override = &previous
			return a.commit(state)
		}
		verifier := state.Password
		if !verifier.matches(*input) {
			var err error
			verifier, err = newWebPassword(*input)
			if err != nil {
				return err
			}
		}
		state.Override = &verifier
	}
	current := state.effectivePassword()
	if !bytes.Equal(previous.Hash, current.Hash) || !bytes.Equal(previous.Salt, current.Salt) {
		state.Sessions = nil
	}
	return a.commit(state)
}

func (p webPassword) matches(password string) bool {
	return p.valid() && subtle.ConstantTimeCompare(p.Hash, webPasswordHash(password, p.Salt)) == 1
}

func newWebPassword(password string) (webPassword, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return webPassword{}, errors.New("could not generate password salt")
	}
	return webPassword{Salt: salt, Hash: webPasswordHash(password, salt)}, nil
}

func (a *webAuth) checkDirectory() error {
	info, err := os.Lstat(filepath.Dir(a.path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 ||
		(DataSocketSupported() && validateDataSocketOwner(info) != nil) {
		return errors.New("web credentials require a server-owned identity directory")
	}
	return nil
}

func (a *webAuth) commit(state webAuthState) error {
	if err := a.checkDirectory(); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = atomicPrivateWrite(a.path, raw); err != nil {
		return err
	}
	a.state = state
	return nil
}

// The OWASP Argon2id baseline uses one lane. A larger memory/time cost
// (64 MiB and 3 passes) and one in-flight derivation bound daemon overhead.
func webPasswordHash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
}

func (a *webAuth) setPassword(password string, reset bool) error {
	if len(password) < 12 || len(password) > 512 {
		return errors.New("web password must be 12 to 512 bytes")
	}
	a.passwordWork.Lock()
	defer a.passwordWork.Unlock()
	a.mu.Lock()
	configured := a.state.Password.valid()
	override := a.state.Override
	a.mu.Unlock()
	if configured != reset {
		if configured {
			return errors.New("web password already configured; use reset")
		}
		return errors.New("web password not configured; use set")
	}
	var verifier webPassword
	if override != nil && override.matches(password) {
		verifier = *override
	} else {
		var err error
		verifier, err = newWebPassword(password)
		if err != nil {
			return err
		}
	}
	return a.savePassword(verifier)
}

func (a *webAuth) unsetPassword() error {
	a.passwordWork.Lock()
	defer a.passwordWork.Unlock()
	return a.savePassword(webPassword{})
}

func (a *webAuth) savePassword(verifier webPassword) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.state
	state.Password = verifier
	if state.Override == nil {
		state.Sessions = nil
	}
	if err := a.commit(state); err != nil {
		return errors.New("could not save web credentials")
	}
	if state.Override == nil {
		a.notifyRevocation()
		a.releaseRuntime()
		log.Print("web password configuration updated; all browser sessions revoked")
		if !verifier.valid() && a.origin != nil {
			log.Print(webPasswordlessWarning)
		}
	}
	return nil
}

func (a *webAuth) passwordMode() (required, override bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.effectivePassword().valid(), a.state.Override != nil
}

func (a *webAuth) passwordlessAllowed() bool {
	return a.origin != nil && (a.origin.Scheme == "http" || a.allowPublicPasswordless)
}

func (n *Node) WebStartupWarning(listener string) string {
	required, _ := n.webAuth.passwordMode()
	if required {
		return ""
	}
	message := webPasswordlessWarning
	if n.webAuth.allowPublicPasswordless {
		return message + "\nWARNING: Public HTTPS passwordless web access has been explicitly enabled. Listener: " + listener
	}
	return message + "\nWeb listener: " + listener + ". Keep passwordless access on loopback and do not proxy it. HTTPS web access requires a password unless public passwordless access is explicitly enabled."
}

func (a *webAuth) revokeAll() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Sessions) == 0 {
		return nil
	}
	state := a.state
	state.Sessions = nil
	if err := a.commit(state); err != nil {
		return errors.New("could not revoke web sessions")
	}
	a.notifyRevocation()
	a.releaseRuntime()
	log.Print("administrator revoked all web sessions")
	return nil
}

func (a *webAuth) releaseRuntime() {
	if a.releaseWorkspace != nil {
		a.releaseWorkspace()
	}
}

func (a *webAuth) notifyRevocation() {
	close(a.changed)
	a.changed = make(chan struct{})
}

func (a *webAuth) secureRequest(r *http.Request) bool {
	if a.origin == nil || r.Host != a.origin.Host {
		return false
	}
	if a.origin.Scheme == "http" {
		addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		port := a.origin.Port()
		if port == "" {
			port = "80"
		}
		local, tcp := addr.(*net.TCPAddr)
		if !ok || !tcp || local.String() != net.JoinHostPort(a.origin.Hostname(), port) || !localPairRequest(r) {
			return false
		}
		for name := range r.Header {
			name = strings.ToLower(name)
			if name == "forwarded" || strings.HasPrefix(name, "x-forwarded-") {
				return false
			}
		}
		return true
	}
	if r.TLS != nil {
		return true
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	proto := r.Header.Values("X-Forwarded-Proto")
	return err == nil && a.proxy.IsValid() && peer.Addr().Unmap() == a.proxy &&
		len(proto) == 1 && proto[0] == "https"
}

func (a *webAuth) cookieName() string {
	if a.origin != nil && a.origin.Scheme == "https" {
		return "__Host-generative-web"
	}
	return "generative-web-local"
}

func (a *webAuth) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(), Value: token, MaxAge: maxAge, Path: "/",
		HttpOnly: true, Secure: a.origin.Scheme == "https", SameSite: http.SameSiteStrictMode,
	})
}

func (a *webAuth) sessionKey(r *http.Request) string {
	c, err := r.Cookie(a.cookieName())
	if err != nil || len(c.Value) != 43 {
		return ""
	}
	return webTokenHash(c.Value)
}

func webTokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (a *webAuth) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	configured := a.state.effectivePassword().valid()
	key := a.sessionKey(r)
	authenticated := a.state.Sessions[key].valid(time.Now())
	a.mu.Unlock()
	result := map[string]any{"configured": configured, "authenticated": authenticated}
	if authenticated {
		result["session_version"] = webSessionVersion(key)
	}
	jsonReply(w, 200, result)
}

func (a *webAuth) login(w http.ResponseWriter, r *http.Request) {
	raw, ok := readRPCBody(w, r, 4096)
	if !ok {
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if _, err := rpc.Decode(raw); err != nil || json.Unmarshal(raw, &input) != nil || len(input.Password) > 512 {
		rpcHTTPError(w, 400, "invalid_request", "Invalid request")
		return
	}
	if !a.passwordWork.TryLock() {
		webRateLimited(w)
		return
	}
	defer a.passwordWork.Unlock()
	a.mu.Lock()
	password := a.state.effectivePassword()
	a.mu.Unlock()
	if len(password.Hash) == 0 {
		rpcHTTPError(w, 503, "web_unconfigured", "Web password is not configured")
		return
	}
	if !a.attempts.Allow() {
		webRateLimited(w)
		return
	}
	if subtle.ConstantTimeCompare(password.Hash, webPasswordHash(input.Password, password.Salt)) != 1 {
		log.Print("web login rejected")
		rpcHTTPError(w, 401, "unauthorized", "Invalid password")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.issueSession(w, r)
}

func (a *webAuth) bootstrap(w http.ResponseWriter, r *http.Request) {
	raw, ok := readRPCBody(w, r, 4096)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil || len(body) != 0 {
		rpcHTTPError(w, 400, "invalid_request", "Expected an empty JSON object")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.effectivePassword().valid() || !a.passwordlessAllowed() {
		rpcHTTPError(w, 403, "web_password_required", "Web password required")
		return
	}
	if key := a.sessionKey(r); a.state.Sessions[key].valid(time.Now()) {
		jsonReply(w, 200, map[string]string{"session_version": webSessionVersion(key)})
		return
	}
	if !a.bootstrapAttempts.Allow() {
		webRateLimited(w)
		return
	}
	a.issueSession(w, r)
}

func (a *webAuth) issueSession(w http.ResponseWriter, r *http.Request) {
	token, err := RandomToken(32)
	if err != nil {
		rpcHTTPError(w, 503, "internal_error", "Service unavailable")
		return
	}
	now := time.Now()
	state := a.state
	state.Sessions = make(map[string]webSession)
	oldKey := a.sessionKey(r)
	var oldest string
	for key, session := range a.state.Sessions {
		if key != oldKey && session.valid(now) {
			state.Sessions[key] = session
			if oldest == "" || session.LastUsed.Before(state.Sessions[oldest].LastUsed) {
				oldest = key
			}
		}
	}
	if len(state.Sessions) >= webSessionLimit {
		delete(state.Sessions, oldest)
	}
	state.Sessions[webTokenHash(token)] = webSession{Owner: webOwner, Created: now, Expires: now.Add(webLifetime), LastUsed: now}
	if err := a.commit(state); err != nil {
		rpcHTTPError(w, 503, "internal_error", "Service unavailable")
		return
	}
	a.notifyRevocation()
	a.setCookie(w, token, int(webLifetime/time.Second))
	if a.state.effectivePassword().valid() {
		log.Print("web login accepted")
	} else {
		log.Print("passwordless web session established")
	}
	jsonReply(w, 200, map[string]string{"session_version": webSessionVersion(webTokenHash(token))})
}

func webRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	rpcHTTPError(w, 429, "rate_limited", "Try again shortly")
}

// A public session version binds in-flight UI work to the cookie it was
// created for, including when another tab subsequently rotates that cookie.
func webSessionVersion(digest string) string {
	return webTokenHash("web-session-version:" + digest)
}

func (a *webAuth) boundKey(r *http.Request) string {
	key := a.sessionKey(r)
	if key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Generative-Session")), []byte(webSessionVersion(key))) != 1 {
		return ""
	}
	return key
}

func (a *webAuth) logout(w http.ResponseWriter, r *http.Request) {
	raw, ok := readRPCBody(w, r, 4096)
	if !ok {
		return
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || input == nil || len(input) != 0 {
		rpcHTTPError(w, 400, "invalid_request", "Expected an empty JSON object")
		return
	}
	key := a.boundKey(r)
	if key == "" && a.sessionKey(r) != "" {
		rpcHTTPError(w, 401, "unauthorized", "Session changed")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.state.Sessions[key]; exists {
		state := a.state
		state.Sessions = make(map[string]webSession)
		for digest, session := range a.state.Sessions {
			if digest != key && session.valid(time.Now()) {
				state.Sessions[digest] = session
			}
		}
		if err := a.commit(state); err != nil {
			rpcHTTPError(w, 503, "internal_error", "Could not sign out")
			return
		}
		a.notifyRevocation()
		if len(state.Sessions) == 0 {
			a.releaseRuntime()
		}
		log.Print("web session signed out")
	}
	a.setCookie(w, "", -1)
	jsonReply(w, 200, map[string]bool{"authenticated": false})
}

func (a *webAuth) principal(r *http.Request) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state.Sessions[a.boundKey(r)]
	if (!a.state.effectivePassword().valid() && !a.passwordlessAllowed()) || !s.valid(time.Now()) {
		return ""
	}
	return s.Owner
}

func (a *webAuth) rpcPrincipal(r *http.Request) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := a.boundKey(r)
	s := a.state.Sessions[key]
	now := time.Now()
	if (!a.state.effectivePassword().valid() && !a.passwordlessAllowed()) || !s.valid(now) {
		return ""
	}
	if now.Sub(s.LastUsed) >= time.Minute {
		previous := s
		s.LastUsed = now
		a.state.Sessions[key] = s
		if err := a.commit(a.state); err != nil {
			a.state.Sessions[key] = previous
			log.Print("web session activity could not be saved")
		}
	}
	return s.Owner
}

func (a *webAuth) guard(next http.HandlerFunc, mutation bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fetchSite := r.Header.Get("Sec-Fetch-Site")
		origins := r.Header.Values("Origin")
		if !a.secureRequest(r) {
			rpcHTTPError(w, 403, "web_origin_not_allowed", "Web origin is not allowed")
			return
		}
		if (fetchSite != "" && fetchSite != "same-origin") ||
			((mutation || len(origins) != 0) && (len(origins) != 1 || origins[0] != a.origin.String())) {
			rpcHTTPError(w, 403, "forbidden", "Web origin is not allowed")
			return
		}
		if mutation && (!strings.EqualFold(r.Header.Get("Content-Type"), "application/json") ||
			r.Header.Get("X-Generative-Web") != "1") {
			rpcHTTPError(w, 403, "forbidden", "Web request headers required")
			return
		}
		required, _ := a.passwordMode()
		if !required && !a.passwordlessAllowed() {
			rpcHTTPError(w, 403, "web_password_required", "Set a web password to access this HTTPS origin")
			return
		}
		next(w, r)
	}
}

func (n *Node) registerWebAuth(mux *http.ServeMux) {
	a := n.webAuth
	mux.HandleFunc("GET /web/auth/status", a.guard(a.status, false))
	mux.HandleFunc("POST /web/auth/bootstrap", a.guard(a.bootstrap, true))
	mux.HandleFunc("POST /web/auth/login", a.guard(a.login, true))
	mux.HandleFunc("POST /web/auth/logout", a.guard(a.logout, true))
	mux.HandleFunc("POST /web/rpc", a.guard(n.rpcHTTP(a.principal, a.rpcPrincipal), true))
	events := n.eventsHTTP(a.principal)
	mux.HandleFunc("GET /web/events", a.guard(func(w http.ResponseWriter, r *http.Request) {
		if a.principal(r) == "" {
			rpcHTTPError(w, 401, "unauthorized", "Authentication required")
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go a.watchSession(ctx, cancel, a.sessionKey(r))
		events(w, r.WithContext(ctx))
	}, false))
}

func (a *webAuth) watchSession(ctx context.Context, cancel context.CancelFunc, key string) {
	defer cancel()
	for {
		a.mu.Lock()
		s := a.state.Sessions[key]
		changed := a.changed
		a.mu.Unlock()
		now := time.Now()
		if !s.valid(now) {
			return
		}
		expiry := minTime(s.Expires, s.LastUsed.Add(webIdle))
		timer := time.NewTimer(expiry.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
