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
	"time"

	"github.com/coder/websocket"
)

type handshakeRequest struct {
	Password string `json:"password"`
}

func (n *Node) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/handshake", func(w http.ResponseWriter, r *http.Request) {
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
			if ctx.Err() != nil {
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
		Method string `json:"method"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&input); err != nil {
		return
	}
	encoder := json.NewEncoder(conn)
	switch input.Method {
	case "ticket":
		result, err := n.IssueTicket()
		if err != nil {
			_ = encoder.Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = encoder.Encode(result)
	case "human":
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
	if _, err := os.Stat(path); err == nil {
		c, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("daemon already running: %s", path)
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
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to running daemon: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err = json.NewEncoder(conn).Encode(map[string]string{"method": method}); err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err = json.NewDecoder(conn).Decode(&result); err != nil {
		return nil, err
	}
	var check map[string]any
	if err = json.Unmarshal(result, &check); err != nil {
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
