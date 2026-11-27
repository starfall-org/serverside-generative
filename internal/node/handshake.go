package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

type handshakeRequest struct {
	Password string `json:"password"`
}

func (n *Node) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	n.registerV5(mux)
	registerWeb(mux)
	n.registerWebAuth(mux)
	mux.HandleFunc("POST /v1/handshake", func(w http.ResponseWriter, r *http.Request) {
		if !n.Config.LegacyPairing {
			http.Error(w, "legacy pairing disabled: use SSH for TCP or URL for HTTP/WS", http.StatusGone)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.ContentLength > 4096 {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		var input handshakeRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if n.blocked("http:" + host) {
			http.Error(w, "rate limited", 429)
			return
		}
		if !n.checkPassword(input.Password) {
			if !n.limit("http:" + host) {
				http.Error(w, "rate limited", 429)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		invite, err := n.IssueTicket()
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(invite)
	})
	if !n.Config.HTTPOnly {
		mux.HandleFunc("GET /v1/handshake/ws", func(w http.ResponseWriter, r *http.Request) {
			if !n.Config.LegacyPairing {
				http.Error(w, "legacy pairing disabled", http.StatusGone)
				return
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if n.blocked("ws:" + host) {
				http.Error(w, "rate limited", 429)
				return
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if len(raw) > 4096 {
				return
			}
			var input handshakeRequest
			if err = json.Unmarshal(raw, &input); err != nil {
				return
			}
			if !n.checkPassword(input.Password) {
				if !n.limit("ws:" + host) {
					return
				}
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"error":"unauthorized"}`))
				return
			}
			invite, err := n.IssueTicket()
			if err != nil {
				return
			}
			reply, _ := json.Marshal(invite)
			_ = conn.Write(ctx, websocket.MessageText, reply)
		})
	}
	return mux
}
func SafeHTTPBind(addr string, allowPublic bool) error {
	if addr == "" || strings.EqualFold(addr, "off") {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if allowPublic {
		return nil
	}
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		return errors.New("public HTTP binding disallowed: use a TLS reverse proxy to loopback, or explicitly --allow-public-http")
	}
	return nil
}
func (n *Node) ServeIPC(ctx context.Context, listener net.Listener) {
	go func() { <-ctx.Done(); _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go n.handleIPC(conn)
	}
}
func (n *Node) handleIPC(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var input struct {
		Method   string `json:"method"`
		Endpoint string `json:"endpoint"`
		Pin      string `json:"pin"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&input); err != nil {
		return
	}
	encoder := json.NewEncoder(conn)
	switch input.Method {
	case "web_password_set", "web_password_reset", "web_password_unset", "web_sessions_revoke":
		if DataSocketSupported() && !authorizedLocalPeer(conn) {
			_ = encoder.Encode(map[string]string{"error": "unauthorized control peer"})
			return
		}
		if input.Method == "web_sessions_revoke" {
			if err := n.webAuth.revokeAll(); err != nil {
				_ = encoder.Encode(map[string]string{"error": err.Error()})
			} else {
				_ = encoder.Encode(map[string]bool{"revoked": true})
			}
			return
		}
		var err error
		switch input.Method {
		case "web_password_unset":
			err = n.webAuth.unsetPassword()
		default:
			err = n.webAuth.setPassword(input.Password, input.Method == "web_password_reset")
		}
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": err.Error()})
			return
		}
		required, override := n.webAuth.passwordMode()
		_ = encoder.Encode(map[string]bool{"configured": required, "startup_override": override})
	case "pair_http_cancel":
		n.mu.Lock()
		count := len(n.httpPairCodes)
		clear(n.httpPairCodes)
		n.mu.Unlock()
		_ = encoder.Encode(map[string]int{"cancelled": count})
	case "pair_http":
		result, err := n.issueHTTPPairURL(input.Endpoint, input.Pin)
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = encoder.Encode(result)
	case "pair_http_local":
		result, err := n.issueLocalPairCode()
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": "pairing failed"})
			return
		}
		_ = encoder.Encode(result)
	case "pair_tcp":
		result, err := n.pairTCP()
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": "pairing failed"})
			return
		}
		_ = encoder.Encode(result)
	case "ticket":
		if !n.Config.LegacyPairing {
			_ = encoder.Encode(map[string]string{"error": "legacy ticket pairing disabled; use pair --stdio --transport tcp"})
			return
		}
		result, err := n.IssueTicket()
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = encoder.Encode(result)
	case "human":
		if !n.Config.LegacyPairing {
			_ = encoder.Encode(map[string]string{"error": "legacy QR/PIN pairing disabled; use HTTP/WS URL or SSH TCP pairing"})
			return
		}
		result, err := n.IssueHuman()
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = encoder.Encode(result)
	default:
		_ = encoder.Encode(map[string]string{"error": "unknown method"})
	}
}
func StartIPCSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Do not unlink an active daemon's socket.
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("control socket path is not a socket: %s", path)
		}
		c, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("daemon already running: %s", path)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, fmt.Errorf("cannot verify existing control socket: %w", dialErr)
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
func ControlRequest(path, method string) (json.RawMessage, error) {
	return ControlRequestFields(path, map[string]string{"method": method})
}
func ControlRequestFields(path string, input map[string]string) (json.RawMessage, error) {
	web := strings.HasPrefix(input["method"], "web_")
	if web {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
			return nil, errors.New("web administration requires the running daemon's private control socket")
		}
	}
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to running daemon: %w", err)
	}
	defer conn.Close()
	if web && DataSocketSupported() && !authorizedLocalPeer(conn) {
		return nil, errors.New("control socket peer mismatch")
	}
	return ExchangeControlRequest(conn, input)
}

// ExchangeControlRequest sends a single request on an established connection.
func ExchangeControlRequest(conn net.Conn, input map[string]string) (json.RawMessage, error) {
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err := json.NewEncoder(conn).Encode(input); err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		return nil, err
	}
	var check map[string]any
	if err := json.Unmarshal(result, &check); err != nil {
		return nil, err
	}
	if e, ok := check["error"].(string); ok {
		return nil, errors.New(e)
	}
	return result, nil
}
func SamePassword(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
