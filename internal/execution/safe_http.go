package execution

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// SafeHTTPClient pins each connection to a checked DNS result and disables
// redirects and proxy environment variables. Loopback is operator opt-in only.
func SafeHTTPClient(allowLoopback bool) *http.Client {
	dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	blocked := []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("2001:db8::/32")}
	tr := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 45 * time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("endpoint resolution failed")
		}
		localName := strings.EqualFold(host, "localhost")
		if literal, err := netip.ParseAddr(host); err == nil && literal.IsLoopback() {
			localName = true
		}
		for _, ip := range ips {
			ip = ip.Unmap()
			permitted := ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast()
			for _, p := range blocked {
				if p.Contains(ip) {
					permitted = false
				}
			}
			if allowLoopback && localName && ip.IsLoopback() {
				permitted = true
			}
			if !permitted {
				return nil, errors.New("endpoint address not permitted")
			}
		}
		var last error
		for _, ip := range ips {
			c, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{Transport: tr, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ValidAuthKey prevents overriding transport-controlled headers or injecting
// malformed header/query syntax via mobile runtime settings.
func ValidAuthKey(method, key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_", r)) {
			return false
		}
	}
	if method == "custom_header" {
		switch strings.ToLower(key) {
		case "host", "content-length", "transfer-encoding", "connection", "proxy-authorization", "proxy-authenticate", "trailer":
			return false
		}
	}
	return true
}

func ValidProviderHeaders(headers map[string]string) bool {
	if len(headers) > 64 {
		return false
	}
	seen := make(map[string]bool, len(headers))
	for name, value := range headers {
		if len(name) > 128 || len(value) > 16<<10 || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return false
		}
		key := strings.ToLower(name)
		if seen[key] || strings.HasPrefix(key, "proxy-") {
			return false
		}
		switch key {
		case "host", "content-length", "transfer-encoding", "connection", "keep-alive", "trailer", "te", "upgrade", "expect":
			return false
		}
		seen[key] = true
	}
	return true
}

type wireAuthTransport struct {
	base               http.RoundTripper
	method, key, value string
	headers            map[string]string
}

func (t *wireAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	if t.method != "" && t.method != "platform_default" {
		r.Header.Del("Authorization")
		r.Header.Del("X-Api-Key")
		r.Header.Del("X-Goog-Api-Key")
		q := r.URL.Query()
		q.Del("key")
		switch t.method {
		case "bearer_token", "oauth":
			r.Header.Set("Authorization", "Bearer "+t.value)
		case "custom_header":
			r.Header.Set(t.key, t.value)
		case "query_param":
			q.Set(t.key, t.value)
		}
		r.URL.RawQuery = q.Encode()
	}
	for name, value := range t.headers {
		r.Header.Set(name, value)
	}
	return t.base.RoundTrip(r)
}
func WithWireAuth(client *http.Client, method, key, value string, headers map[string]string) *http.Client {
	if (method == "" || method == "platform_default") && len(headers) == 0 {
		return client
	}
	clone := *client
	clone.Transport = &wireAuthTransport{base: client.Transport, method: method, key: key, value: value, headers: headers}
	return &clone
}
