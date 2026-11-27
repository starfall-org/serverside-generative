package node

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/starfall-org/generative-server/internal/rpc"
)

type httpPairCode struct {
	host, pin, endpoint string
	expires             time.Time
	local               bool
}

func httpsAuthority(host string) (string, error) {
	u, err := url.Parse("//" + host)
	if err != nil || u.User != nil || u.Host != host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return "", errors.New("invalid endpoint")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port), nil
}

// issueHTTPPairURL issues a short-lived capability bound to an HTTPS authority
// and an explicitly supplied pin for the client-visible TLS terminator.
func (n *Node) issueHTTPPairURL(endpoint, pin string) (map[string]any, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid HTTPS endpoint")
	}
	host, err := httpsAuthority(u.Host)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pin, "sha256/") {
		return nil, errors.New("invalid SPKI pin")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pin, "sha256/"))
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("invalid SPKI pin")
	}
	code, err := RandomToken(32)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(TicketLifetime)
	n.mu.Lock()
	defer n.mu.Unlock()
	for key, value := range n.httpPairCodes {
		if !time.Now().Before(value.expires) {
			delete(n.httpPairCodes, key)
		}
	}
	if len(n.httpPairCodes) >= 128 {
		return nil, errors.New("too many pending pairing codes")
	}
	base := "https://" + u.Host
	n.httpPairCodes[sha256.Sum256([]byte(code))] = httpPairCode{host: host, pin: pin, endpoint: base, expires: expires}
	fragment := url.Values{"code": {code}, "pin": {pin}, "v": {"1"}}.Encode()
	return map[string]any{"url": base + "/connect#" + fragment, "expires": expires.Unix()}, nil
}

// issueLocalPairCode creates a short-lived code redeemable only on loopback.
func (n *Node) issueLocalPairCode() (map[string]any, error) {
	code, err := RandomToken(24)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for key, entry := range n.httpPairCodes {
		if !now.Before(entry.expires) {
			delete(n.httpPairCodes, key)
		}
	}
	if len(n.httpPairCodes) >= 128 {
		return nil, errors.New("too many pending pairing codes")
	}
	expires := now.Add(TicketLifetime)
	n.httpPairCodes[sha256.Sum256([]byte(code))] = httpPairCode{local: true, expires: expires}
	return map[string]any{"code": code, "expires": expires.Unix()}, nil
}

func localPairRequest(r *http.Request) bool {
	if r.TLS != nil {
		return false
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(remote).IsLoopback() {
		return false
	}
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	local, _, err := net.SplitHostPort(addr.String())
	if err != nil || !net.ParseIP(local).IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return net.ParseIP(strings.Trim(host, "[]")).IsLoopback() || strings.EqualFold(host, "localhost")
}

func (n *Node) redeemHTTPPairCode(code, host string, local, localOnly bool) (map[string]any, error) {
	authority, err := httpsAuthority(host)
	if (err != nil && !local) || code == "" || len(code) > 128 {
		return nil, errors.New("invalid pairing code")
	}
	hash := sha256.Sum256([]byte(code))
	n.mu.Lock()
	defer n.mu.Unlock()
	entry, ok := n.httpPairCodes[hash]
	if !ok || !time.Now().Before(entry.expires) || (entry.local && !local) || (!entry.local && (localOnly || entry.host != authority)) {
		return nil, errors.New("invalid pairing code")
	}
	id, secret, err := n.createPairLocked("http")
	if err != nil {
		return nil, err
	}
	delete(n.httpPairCodes, hash)
	return map[string]any{"protocol": 5, "client_id": id, "secret": secret, "server_id": n.Fingerprint, "fingerprint": n.Fingerprint, "endpoint": entry.endpoint, "tls_pin": entry.pin, "transports": []string{"http", "websocket"}}, nil
}

func (n *Node) handleHTTPPair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !secureDataRequest(r) {
		rpcHTTPError(w, 403, "forbidden", "TLS required")
		return
	}
	local := localPairRequest(r)
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "https" && !(local && u.Scheme == "http")) {
			rpcHTTPError(w, 403, "forbidden", "Invalid origin")
			return
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if n.blocked("pair:" + host) {
		rpcHTTPError(w, 429, "rate_limited", "Rate limited")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		rpcHTTPError(w, 400, "invalid_request", "Invalid request")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		rpcHTTPError(w, 413, "payload_too_large", "Request too large")
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if _, err = rpc.Decode(raw); err != nil || json.Unmarshal(raw, &input) != nil {
		rpcHTTPError(w, 400, "invalid_request", "Invalid request")
		return
	}
	result, err := n.redeemHTTPPairCode(input.Code, r.Host, local, local && r.Header.Get("Origin") != "")
	if err != nil {
		n.limit("pair:" + host)
		rpcHTTPError(w, 401, "unauthorized", "Invalid pairing code")
		return
	}
	jsonReply(w, 200, result)
}
