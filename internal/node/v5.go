package node

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/starfall-org/generative-server/internal/chatsession"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/starfall-org/generative-server/internal/rpc"
)

type apiToken struct {
	owner   string
	expires time.Time
}

func (n *Node) paired(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.pairs[id]
	return ok
}
func (n *Node) issueAPIToken(owner string) (string, error) {
	token, err := RandomToken(32)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(token))
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for k, v := range n.tokens {
		if now.After(v.expires) {
			delete(n.tokens, k)
		}
	}
	if len(n.tokens) >= 4096 {
		return "", io.ErrShortBuffer
	}
	n.tokens[hash] = apiToken{owner, now.Add(15 * time.Minute)}
	return token, nil
}
func (n *Node) bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	key := sha256.Sum256([]byte(strings.TrimPrefix(h, "Bearer ")))
	n.mu.Lock()
	defer n.mu.Unlock()
	t, ok := n.tokens[key]
	if !ok || time.Now().After(t.expires) {
		delete(n.tokens, key)
		return ""
	}
	if _, ok = n.pairs[t.owner]; !ok {
		delete(n.tokens, key)
		return ""
	}
	return t.owner
}

// Plaintext data APIs are restricted to loopback listeners; legacy public
// pairing remains controlled separately by the existing flag.
func secureDataRequest(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host || u.User != nil || u.Path != "" {
			return false
		}
	}
	if r.TLS != nil {
		return true
	}
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	return err == nil && net.ParseIP(host).IsLoopback()
}
func (n *Node) serveRPCPeer(p *peer, pollDefault bool) {
	subs := map[string]func(){}
	defer func() {
		for _, cancel := range subs {
			cancel()
		}
	}()
	for {
		var raw json.RawMessage
		if frameRead(p.conn, &raw) != nil || !n.paired(p.clientID) {
			return
		}
		req, err := rpc.Decode(raw)
		if err != nil {
			_ = p.send(rpc.Reply{ID: "", Error: &rpc.Error{Code: "invalid_request", Message: "Invalid request"}})
			continue
		}
		reply := n.rpc.Dispatch(p.clientID, req)
		if reply.OK && opensSession(req) && (!pollDefault || hasEventCursor(req)) && subs[req.SessionID] == nil {
			ch, cancel, err := n.rpc.Subscribe(p.clientID, req.SessionID)
			if err == nil {
				subs[req.SessionID] = cancel
				go n.forwardEvents(p.clientID, req.SessionID, openCursor(req), ch, func(e chatsession.Event) error {
					err := p.send(e)
					if err != nil {
						_ = p.conn.Close()
					}
					return err
				})
			} else {
				reply = subscriptionFailure(req.ID, err)
			}
		}
		if reply.OK && (req.Op == "session.close" || req.Op == "session.delete") {
			if cancel := subs[req.SessionID]; cancel != nil {
				cancel()
				delete(subs, req.SessionID)
			}
		}
		if p.send(reply) != nil {
			return
		}
	}
}
func subscriptionFailure(id string, err error) rpc.Reply {
	if errors.Is(err, chatsession.ErrLimit) {
		return rpc.Reply{ID: id, Error: &rpc.Error{Code: "resource_exhausted", Message: "Session subscription limit reached"}}
	}
	return rpc.Reply{ID: id, Error: &rpc.Error{Code: "not_found", Message: "Session not found"}}
}

func eventFailure(err error) (int, string, string) {
	switch {
	case errors.Is(err, chatsession.ErrGap):
		return http.StatusConflict, "event_gap", "Event history is no longer available"
	case errors.Is(err, chatsession.ErrNotFound):
		return http.StatusNotFound, "not_found", "Session not found"
	default:
		return http.StatusInternalServerError, "internal_error", "Event stream unavailable"
	}
}

func opensSession(req rpc.Request) bool {
	return req.Op == "session.open" || req.Op == "session.open_chunked"
}
func hasEventCursor(req rpc.Request) bool {
	var data struct {
		After *uint64 `json:"after"`
	}
	return json.Unmarshal(req.Data, &data) == nil && data.After != nil
}
func openCursor(req rpc.Request) uint64 {
	var data struct {
		After uint64 `json:"after"`
	}
	_ = json.Unmarshal(req.Data, &data) // session.open has already validated the data.
	return data.After
}

func (n *Node) forwardEvents(owner, id string, cursor uint64, ch <-chan struct{}, send func(chatsession.Event) error) {
	for {
		events, err := n.rpc.Events(owner, id, cursor)
		if err != nil {
			if errors.Is(err, chatsession.ErrGap) {
				_ = send(chatsession.Event{SessionID: id, Kind: "event_gap", Data: map[string]any{}})
			}
			return
		}
		for _, e := range events {
			if !n.paired(owner) || send(e) != nil {
				return
			}
			cursor = e.Seq
		}
		if len(events) > 0 {
			continue
		}
		if _, ok := <-ch; !ok {
			return
		}
	}
}

func jsonReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func rpcHTTPError(w http.ResponseWriter, status int, code, message string) {
	jsonReply(w, status, rpc.Reply{ID: "", Error: &rpc.Error{Code: code, Message: message}})
}
func (n *Node) capabilities() map[string]any {
	operations := []string{"identity.revoke", "capabilities.get", "provider.catalog", "provider.import", "mcp.catalog", "mcp.import", "tool.list", "tool.call", "config.get", "config.save", "session.open", "session.open_chunked", "session.read_chunk", "session.upload.begin", "session.upload.chunk", "session.upload.commit", "session.upload.abort", "session.configure", "session.get", "session.save", "session.run", "session.cancel", "session.list", "session.delete", "session.events", "session.close"}
	if n.Terminals != nil {
		operations = append(operations, "terminal.create", "terminal.execute", "terminal.read", "terminal.close", "terminal.list")
	}
	if n.Agents != nil {
		operations = append(operations, "agent.open", "agent.send", "agent.steer", "agent.respond", "agent.interrupt", "agent.events", "agent.status", "agent.list", "agent.close")
	}
	transports := []string{"tcp", "websocket", "http"}
	if info, err := os.Lstat(n.Config.DataSocket); DataSocketSupported() && err == nil && info.Mode()&os.ModeSocket != 0 {
		transports = append(transports, "ssh_stdio")
	}
	return map[string]any{"protocol": 5, "transports": transports, "operations": operations, "native_terminal": n.Terminals != nil, "native_agent": n.Agents != nil,
		"chat_features": map[string]any{
			"version":                  1,
			"provider_catalog":         "provider.catalog",
			"list_snapshot_pagination": true,
			"chunked_transfer":         map[string]any{"version": 1, "max_conversation_bytes": 8 << 20, "max_chunk_bytes": 256 << 10, "upload_expiry_seconds": 600, "restart_resumable": false},
			"model_limits":             "unknown",
			"runtime_sources":          []string{"stored", "mobile_ephemeral"},
			"provider_adapters":        []string{"openai", "openai_responses", "anthropic", "google"},
			"text_generation":          true, "streaming": true, "conversation_summary": true,
			"attachments": false, "multimodal_input": false, "generation_tool_loop": false,
			"reasoning_history": false, "selected_instructions": false, "runtime_instruction_handoff": true,
			"conversation_max_bytes": 3 << 20, "workspace_scope": "paired_client", "ssh_event_delivery": "poll_default_explicit_after_push",
			"negotiation": map[string]any{
				"version": 1, "catalog_operation": "provider.catalog", "catalog_requires_authentication": true,
				"modalities": map[string]any{"input": []string{"text"}, "output": []string{"text"}, "attachment_upload": false},
				"history":    map[string]any{"summary": true, "reasoning": false, "tool_activity": false, "selected_instructions": "explicit_runtime_text"},
				"generation": map[string]any{"streaming": true, "cancellation": true, "tool_loop": false, "max_history_messages": 256, "max_message_bytes": 512 << 10, "max_run_content_bytes": 64 << 10},
				"parameters": map[string]any{
					"max_tokens":  map[string]any{"min": 0, "max": 131072, "provider_limit": "unknown"},
					"temperature": map[string]any{"min": 0, "max": 2}, "top_p": map[string]any{"min": 0, "max": 1}, "top_k": map[string]any{"min": 1, "max": 1000},
				},
				"adapters": map[string]any{
					"openai":           map[string]any{"parameters": []string{"max_tokens", "temperature", "top_p"}},
					"openai_responses": map[string]any{"parameters": []string{"max_tokens", "temperature", "top_p"}},
					"anthropic":        map[string]any{"parameters": []string{"max_tokens"}},
					"google":           map[string]any{"parameters": []string{"max_tokens", "temperature", "top_p", "top_k"}},
				},
				"stored_runtime": map[string]any{"requires_provider_and_model": true, "explicit_key_selection_when_configured": true, "credential_values_returned": false},
			},
		},
	}
}

func webRPCAllowed(op string) bool {
	return op != "identity.revoke" && !strings.HasPrefix(op, "terminal.") && !strings.HasPrefix(op, "agent.")
}

func webCapabilities(data map[string]any) {
	operations := data["operations"].([]string)
	webOperations := operations[:0]
	for _, op := range operations {
		if webRPCAllowed(op) {
			webOperations = append(webOperations, op)
		}
	}
	data["operations"] = webOperations
	data["native_terminal"] = false
	data["native_agent"] = false
	data["transports"] = []string{"http"}
	data["chat_features"].(map[string]any)["workspace_scope"] = "web_workspace"
}

// readRPCBody applies the same size limit and error mapping to HTTP RPC inputs.
func readRPCBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		return raw, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		rpcHTTPError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Request too large")
	} else {
		rpcHTTPError(w, http.StatusBadRequest, "invalid_request", "Invalid request")
	}
	return nil, false
}

func (n *Node) registerV5(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/pair", n.handleHTTPPair)
	// A paired HTTP client may revoke only its own credential, never another client.
	// A successful response invalidates all of this identity's bearer tokens.
	mux.HandleFunc("POST /v1/revoke", func(w http.ResponseWriter, r *http.Request) {
		if !secureDataRequest(r) {
			rpcHTTPError(w, 403, "forbidden", "TLS required")
			return
		}
		owner := n.bearer(r)
		if owner == "" {
			rpcHTTPError(w, 401, "unauthorized", "Authentication required")
			return
		}
		// Do not accept a caller-specified target identity or any other payload.
		body, ok := readRPCBody(w, r, 4096)
		if !ok {
			return
		}
		if len(body) != 0 {
			rpcHTTPError(w, 400, "invalid_request", "Revocation takes no body")
			return
		}
		if err := n.revokePair(owner); err != nil {
			rpcHTTPError(w, 503, "internal_error", "Could not revoke identity")
			return
		}
		jsonReply(w, 200, map[string]any{"revoked": true})
	})
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if !secureDataRequest(r) {
			rpcHTTPError(w, 403, "forbidden", "TLS required")
			return
		}
		jsonReply(w, 200, n.capabilities())
	})
	mux.HandleFunc("POST /v1/auth", func(w http.ResponseWriter, r *http.Request) {
		if !secureDataRequest(r) {
			rpcHTTPError(w, 403, "forbidden", "TLS required")
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if n.blocked("api:" + host) {
			rpcHTTPError(w, 429, "rate_limited", "Rate limited")
			return
		}
		raw, ok := readRPCBody(w, r, 4096)
		if !ok {
			return
		}
		var a Message
		if _, err := rpc.Decode(raw); err != nil || json.Unmarshal(raw, &a) != nil {
			rpcHTTPError(w, 400, "invalid_request", "Invalid request")
			return
		}
		if a.ClientID == "" || a.Secret == "" || a.Ticket != "" || a.PIN != "" || !n.verifyPairFor(a.ClientID, a.Secret, "http") {
			n.limit("api:" + host)
			rpcHTTPError(w, 401, "unauthorized", "Authentication required")
			return
		}
		id := a.ClientID
		token, err := n.issueAPIToken(id)
		if err != nil {
			rpcHTTPError(w, 503, "resource_exhausted", "Service unavailable")
			return
		}
		jsonReply(w, 200, map[string]any{"token": token, "expires_in": 900, "client_id": id})
	})
	secure := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !secureDataRequest(r) {
				rpcHTTPError(w, 403, "forbidden", "TLS required")
				return
			}
			handler(w, r)
		}
	}
	mux.HandleFunc("POST /v1/rpc", secure(n.rpcHTTP(n.bearer, nil)))
	mux.HandleFunc("GET /v1/events", secure(n.eventsHTTP(n.bearer)))

	mux.HandleFunc("GET /v1/ws", func(w http.ResponseWriter, r *http.Request) {
		if !secureDataRequest(r) {
			rpcHTTPError(w, 403, "forbidden", "TLS required")
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if n.blocked("api:" + host) {
			rpcHTTPError(w, 429, "rate_limited", "Rate limited")
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		c.SetReadLimit(maxFrame)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		_, raw, err := c.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var a Message
		if _, err = rpc.Decode(raw); err != nil {
			return
		}
		if json.Unmarshal(raw, &a) != nil || a.Type != "auth" {
			return
		}
		if a.ClientID == "" || a.Secret == "" || a.Ticket != "" || a.PIN != "" || !n.verifyPairFor(a.ClientID, a.Secret, "http") {
			n.limit("api:" + host)
			return
		}
		id := a.ClientID
		paired := map[string]any{"type": "paired", "protocol": 5, "client_id": id}
		b, _ := json.Marshal(paired)
		if c.Write(r.Context(), websocket.MessageText, b) != nil {
			return
		}
		subs := map[string]func(){}
		defer func() {
			for _, cancel := range subs {
				cancel()
			}
		}()
		var writeMu sync.Mutex
		write := func(b []byte) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			return c.Write(ctx, websocket.MessageText, b)
		}
		for {
			typ, raw, err := c.Read(r.Context())
			if err != nil || typ != websocket.MessageText || !n.paired(id) {
				return
			}
			req, err := rpc.Decode(raw)
			if err != nil {
				bad, _ := json.Marshal(rpc.Reply{ID: "", Error: &rpc.Error{Code: "invalid_request", Message: "Invalid request"}})
				if write(bad) != nil {
					return
				}
				continue
			}
			reply := n.rpc.Dispatch(id, req)
			if reply.OK && opensSession(req) && subs[req.SessionID] == nil {
				ch, cancel, err := n.rpc.Subscribe(id, req.SessionID)
				if err == nil {
					subs[req.SessionID] = cancel
					go n.forwardEvents(id, req.SessionID, openCursor(req), ch, func(event chatsession.Event) error {
						b, err := json.Marshal(event)
						if err != nil {
							return err
						}
						return write(b)
					})
				} else {
					reply = subscriptionFailure(req.ID, err)
				}
			}
			if reply.OK && (req.Op == "session.close" || req.Op == "session.delete") {
				if cancel := subs[req.SessionID]; cancel != nil {
					cancel()
					delete(subs, req.SessionID)
				}
			}
			b, err := json.Marshal(reply)
			if err != nil || write(b) != nil {
				return
			}
		}
	})
}

func (n *Node) rpcHTTP(principal, confirm func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner := principal(r)
		if owner == "" {
			rpcHTTPError(w, 401, "unauthorized", "Authentication required")
			return
		}
		raw, ok := readRPCBody(w, r, maxFrame)
		if !ok {
			return
		}
		req, err := rpc.Decode(raw)
		if err != nil {
			rpcHTTPError(w, 400, "invalid_request", "Invalid request")
			return
		}
		if confirm != nil && confirm(r) != owner {
			rpcHTTPError(w, 401, "unauthorized", "Authentication required")
			return
		}
		if owner == webOwner && !webRPCAllowed(req.Op) {
			jsonReply(w, 403, rpc.Reply{ID: req.ID, Error: &rpc.Error{Code: "permission_denied", Message: "Operation requires a paired client"}})
			return
		}
		reply := n.rpc.Dispatch(owner, req)
		if owner == webOwner && req.Op == "capabilities.get" && reply.OK {
			webCapabilities(reply.Data.(map[string]any))
		}
		status := 200
		if !reply.OK {
			switch reply.Error.Code {
			case "not_found":
				status = 404
			case "revision_conflict", "session_busy", "event_gap", "request_already_recorded", "session_deleted":
				status = 409
			case "invalid_request", "missing_runtime_config":
				status = 400
			case "unauthorized", "auth_required":
				status = 401
			case "permission_denied", "forbidden", "secret_leak_blocked":
				status = 403
			case "resource_exhausted":
				status = 429
			case "internal_error":
				status = 500
			default:
				status = 501
			}
		}
		jsonReply(w, status, reply)
	}
}

func (n *Node) eventsHTTP(principal func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner := principal(r)
		if owner == "" {
			rpcHTTPError(w, 401, "unauthorized", "Authentication required")
			return
		}
		id := r.URL.Query().Get("session_id")
		after := uint64(0)
		if v := r.URL.Query().Get("after"); v != "" {
			var err error
			after, err = strconv.ParseUint(v, 10, 64)
			if err != nil {
				rpcHTTPError(w, 400, "invalid_request", "Invalid cursor")
				return
			}
		}
		ch, cancel, err := n.rpc.Subscribe(owner, id)
		if err != nil {
			if principal(r) == "" {
				rpcHTTPError(w, 401, "unauthorized", "Authentication required")
				return
			}
			failure := subscriptionFailure("", err)
			status := http.StatusNotFound
			if failure.Error.Code == "resource_exhausted" {
				status = http.StatusTooManyRequests
			}
			jsonReply(w, status, failure)
			return
		}
		defer cancel()
		events, err := n.rpc.Events(owner, id, after)
		if err != nil {
			if principal(r) == "" {
				rpcHTTPError(w, 401, "unauthorized", "Authentication required")
				return
			}
			status, code, message := eventFailure(err)
			rpcHTTPError(w, status, code, message)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			if len(events) == 0 {
				timer := time.NewTimer(20 * time.Second)
				defer timer.Stop()
				select {
				case <-ch:
					events, err = n.rpc.Events(owner, id, after)
				case <-timer.C:
				case <-r.Context().Done():
					if owner == webOwner && principal(r) == "" {
						rpcHTTPError(w, 401, "unauthorized", "Authentication required")
					}
					return
				}
				if principal(r) == "" {
					rpcHTTPError(w, 401, "unauthorized", "Authentication required")
					return
				}
				if err != nil {
					status, code, message := eventFailure(err)
					rpcHTTPError(w, status, code, message)
					return
				}
			}
			if principal(r) == "" {
				rpcHTTPError(w, 401, "unauthorized", "Authentication required")
				return
			}
			jsonReply(w, 200, map[string]any{"events": events})
			return
		}
		controller := http.NewResponseController(w)
		flusher, ok := w.(http.Flusher)
		if !ok {
			rpcHTTPError(w, 500, "internal_error", "Streaming unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			for _, event := range events {
				if r.Context().Err() != nil || (owner == webOwner && principal(r) == "") {
					return
				}
				_ = controller.SetWriteDeadline(time.Now().Add(20 * time.Second))
				b, _ := json.Marshal(event)
				if _, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Seq, b); err != nil {
					return
				}
				after = event.Seq
			}
			flusher.Flush()
			if len(events) > 0 {
				if principal(r) == "" {
					return
				}
				events, err = n.rpc.Events(owner, id, after)
				if err != nil {
					if principal(r) == "" {
						return
					}
					_, code, _ := eventFailure(err)
					fmt.Fprintf(w, "event: error\ndata: {\"code\":%q}\n\n", code)
					flusher.Flush()
					return
				}
				continue
			}
			select {
			case <-ch:
				if principal(r) == "" {
					return
				}
				events, err = n.rpc.Events(owner, id, after)
				if err != nil {
					if principal(r) == "" {
						return
					}
					_, code, _ := eventFailure(err)
					fmt.Fprintf(w, "event: error\ndata: {\"code\":%q}\n\n", code)
					flusher.Flush()
					return
				}
			case <-ticker.C:
				if principal(r) == "" {
					return
				}
				_ = controller.SetWriteDeadline(time.Now().Add(20 * time.Second))
				if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
				events = nil
			case <-r.Context().Done():
				return
			}
		}
	}
}
