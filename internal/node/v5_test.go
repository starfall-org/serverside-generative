package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/starfall-org/generative-server/internal/chatsession"
	"github.com/starfall-org/generative-server/internal/chatstore"
	"github.com/starfall-org/generative-server/internal/rpc"
)

func TestV5TransportSessionParity(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	inv, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	id, secret, err := n.pairWithTicket(inv.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(n.HTTPHandler())
	defer srv.Close()
	body, _ := json.Marshal(Message{ClientID: id, Secret: secret})
	res, err := http.Post(srv.URL+"/v1/auth", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var token struct {
		Token string `json:"token"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&token) != nil || token.Token == "" {
		t.Fatal("http auth")
	}
	request := rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"c","title":"t","created_at":1,"updated_at":2,"messages":[]}}`)}
	body, _ = json.Marshal(request)
	hreq, _ := http.NewRequest("POST", srv.URL+"/v1/rpc", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer "+token.Token)
	res, err = http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	var open rpc.Reply
	if err = json.NewDecoder(res.Body).Decode(&open); err != nil || !open.OK {
		t.Fatalf("HTTP: %+v error=%+v, parse=%v", open, open.Error, err)
	}
	res.Body.Close()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	auth, _ := json.Marshal(Message{Type: "auth", ClientID: id, Secret: secret})
	if err = ws.Write(ctx, websocket.MessageText, auth); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	get := rpc.Request{ID: "get", Op: "session.get", SessionID: "c"}
	raw, _ := json.Marshal(get)
	if err = ws.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = ws.Read(ctx)
	var wr rpc.Reply
	if err != nil || json.Unmarshal(raw, &wr) != nil || !wr.OK {
		t.Fatalf("WS: %s %v", raw, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tc, cancel := context.WithCancel(ctx)
	defer cancel()
	go n.ServeTCP(tc, listener)
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := peer{conn: conn}
	if err = p.send(Message{Type: "auth", Protocol: 5, ClientID: id, Secret: secret}); err != nil {
		t.Fatal(err)
	}
	var paired map[string]any
	if err = frameRead(conn, &paired); err != nil || paired["protocol"] != float64(5) {
		t.Fatalf("tcp auth: %v %v", paired, err)
	}
	if err = p.send(get); err != nil {
		t.Fatal(err)
	}
	var tr rpc.Reply
	if err = frameRead(conn, &tr); err != nil || !tr.OK {
		t.Fatalf("tcp: %+v %v", tr, err)
	}
	wb, _ := json.Marshal(wr.Data)
	tb, _ := json.Marshal(tr.Data)
	if !bytes.Equal(wb, tb) {
		t.Fatalf("transport mismatch: %s %s", wb, tb)
	}
	fixture, err := os.ReadFile("../compat/testdata/conversation-server.json")
	if err != nil {
		t.Fatal(err)
	}
	openData, _ := json.Marshal(map[string]any{"storage": "memory", "conversation": json.RawMessage(fixture)})
	openFixture := rpc.Request{ID: "fixture-open", Op: "session.open", SessionID: "server-fixture", Data: openData}
	body, _ = json.Marshal(openFixture)
	hreq, _ = http.NewRequest("POST", srv.URL+"/v1/rpc", bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer "+token.Token)
	res, err = http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	var httpRaw json.RawMessage
	if err = json.NewDecoder(res.Body).Decode(&httpRaw); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	checkFixture := func(transport string, raw []byte) {
		var envelope struct {
			OK   bool `json:"ok"`
			Data struct {
				Conversation json.RawMessage `json:"conversation"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK || !chatstore.Equal(envelope.Data.Conversation, fixture) {
			t.Fatalf("%s fixture changed: %v", transport, err)
		}
	}
	checkFixture("HTTP", httpRaw)
	fixtureGet := rpc.Request{ID: "fixture-get", Op: "session.get", SessionID: "server-fixture"}
	raw, _ = json.Marshal(fixtureGet)
	if err = ws.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkFixture("WS", raw)
	if err = p.send(fixtureGet); err != nil {
		t.Fatal(err)
	}
	var tcpRaw json.RawMessage
	if err = frameRead(conn, &tcpRaw); err != nil {
		t.Fatal(err)
	}
	checkFixture("TCP", tcpRaw)
}

func TestV5HTTPOnlyRunAndEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: dir, AllowLocalAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	inv, _ := n.IssueTicket()
	id, secret, err := n.pairWithTicket(inv.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	b, _ := json.Marshal(Message{ClientID: id, Secret: secret})
	resp, err := http.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	call := func(req rpc.Request) rpc.Reply {
		b, _ := json.Marshal(req)
		r, _ := http.NewRequest("POST", server.URL+"/v1/rpc", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+auth.Token)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply rpc.Reply
		if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	data, _ := json.Marshal(map[string]any{"storage": "memory", "conversation": map[string]any{"id": "c", "title": "x", "created_at": 1, "updated_at": 2, "messages": []any{}}, "runtime": map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": "token"}}}})
	if reply := call(rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: data}); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	if reply := call(rpc.Request{ID: "run", Op: "session.run", SessionID: "c", Data: json.RawMessage(`{"content":"hi"}`)}); !reply.OK {
		t.Fatalf("run: %+v", reply)
	}
	cursor := uint64(0)
	done := false
	client := http.Client{Timeout: 5 * time.Second}
	for !done {
		r, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/events?session_id=c&after=%d", server.URL, cursor), nil)
		r.Header.Set("Authorization", "Bearer "+auth.Token)
		r.Header.Set("Accept", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Events []struct {
				Seq  uint64 `json:"seq"`
				Kind string `json:"kind"`
			} `json:"events"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, e := range page.Events {
			cursor = e.Seq
			if e.Kind == "error" {
				t.Fatal("provider error")
			}
			if e.Kind == "completed" {
				done = true
			}
		}
	}
	streamReq, _ := http.NewRequest("GET", server.URL+"/v1/events?session_id=c&after=0", nil)
	streamReq.Header.Set("Authorization", "Bearer "+auth.Token)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := client.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(streamResp.Body)
	found := false
	for i := 0; i < 8; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &event) != nil || event["session_id"] != "c" {
				t.Fatalf("invalid SSE event: %q", line)
			}
			found = true
			break
		}
	}
	streamResp.Body.Close()
	if !found {
		t.Fatal("missing SSE event")
	}
	snap, err := chatstore.New(dir).Get(id, "c")
	if err != nil || !strings.Contains(string(snap.Conversation), "hello") {
		t.Fatalf("HTTP conversation missing from storage: %+v %v", snap, err)
	}
}

func TestV5OriginAndRevocation(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	inv, _ := n.IssueTicket()
	id, secret, err := n.pairWithTicket(inv.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	body, _ := json.Marshal(Message{ClientID: id, Secret: secret})
	req, _ := http.NewRequest("POST", server.URL+"/v1/auth", bytes.NewReader(body))
	req.Header.Set("Origin", "https://evil.invalid")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("origin: %d", resp.StatusCode)
	}
	resp, err = http.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/ws"
	_, rejected, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://evil.invalid"}}})
	if err == nil || rejected == nil || rejected.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin WebSocket: %v %v", rejected, err)
	}
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	authMessage, _ := json.Marshal(Message{Type: "auth", Protocol: 5, ClientID: id, Secret: secret})
	if err = c.Write(ctx, websocket.MessageText, authMessage); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = n.revokePair(id); err != nil {
		t.Fatal(err)
	}
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"id":"after-revoke","op":"session.list","data":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Read(ctx); err == nil {
		t.Fatalf("revoked WebSocket received reply: %s", data)
	}
	req, _ = http.NewRequest("POST", server.URL+"/v1/rpc", strings.NewReader(`{"id":"x","op":"session.list","data":{}}`))
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", resp.StatusCode)
	}
}

func TestV5WebSocketOnlyRun(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data"), AllowLocalAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, secret, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send := func(value any) {
		b, _ := json.Marshal(value)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]json.RawMessage {
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err = json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(Message{Type: "auth", ClientID: id, Secret: secret})
	read()
	data, _ := json.Marshal(map[string]any{"storage": "sync", "conversation": map[string]any{"id": "c", "title": "x", "created_at": 1, "updated_at": 2, "messages": []any{}}, "runtime": map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": "secret"}}}})
	send(rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: data})
	if string(read()["ok"]) != "true" {
		t.Fatal("open failed")
	}
	send(rpc.Request{ID: "run", Op: "session.run", SessionID: "c", Data: json.RawMessage(`{"content":"hi"}`)})
	ack, complete := false, false
	for !ack || !complete {
		m := read()
		if string(m["id"]) == `"run"` {
			ack = string(m["ok"]) == "true"
		}
		if string(m["kind"]) == `"completed"` {
			complete = true
		}
		if string(m["kind"]) == `"error"` {
			t.Fatal("run failed")
		}
	}
	send(rpc.Request{ID: "get", Op: "session.get", SessionID: "c"})
	m := read()
	if string(m["ok"]) != "true" || !bytes.Contains(m["data"], []byte("hello")) {
		t.Fatalf("get: %s", m["data"])
	}
	send(rpc.Request{ID: "close", Op: "session.close", SessionID: "c"})
	if string(read()["ok"]) != "true" {
		t.Fatal("close failed")
	}
}

func TestV5NativeTerminalOptInAndDeduplication(t *testing.T) {
	disabled, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	inv, _ := disabled.IssueTicket()
	id, _, err := disabled.pairWithTicket(inv.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	req := rpc.Request{ID: "create", Op: "terminal.create", Data: json.RawMessage(`{"rows":24,"cols":80}`)}
	if reply := disabled.rpc.Dispatch(id, req); reply.Error == nil || reply.Error.Code != "forbidden" {
		t.Fatalf("disabled: %+v", reply)
	}
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data"), EnableTerminal: true, TerminalCWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	inv, _ = n.IssueTicket()
	id, _, err = n.pairWithTicket(inv.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	first := n.rpc.Dispatch(id, req)
	if !first.OK {
		t.Fatalf("create: %+v", first)
	}
	second := n.rpc.Dispatch(id, req)
	a, _ := json.Marshal(first.Data)
	b, _ := json.Marshal(second.Data)
	if !second.OK || !bytes.Equal(a, b) || len(n.Terminals.List()) != 1 {
		t.Fatalf("duplicate: %+v", second)
	}
	req.Data = json.RawMessage(`{"rows":25,"cols":80}`)
	if conflict := n.rpc.Dispatch(id, req); conflict.Error == nil || conflict.Error.Code != "idempotency_conflict" {
		t.Fatalf("id reuse: %+v", conflict)
	}
}

func TestV5CredentialEchoBlockedAcrossTransports(t *testing.T) {
	const secret = "cross-transport-secret-sentinel"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"cross-transport-\"}}]}\n\ndata: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"secret-sentinel\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: dir, AllowLocalAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	owner, pairSecret, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	authBody, _ := json.Marshal(Message{ClientID: owner, Secret: pairSecret})
	resp, err := http.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(authBody))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	data, _ := json.Marshal(map[string]any{"storage": "sync", "conversation": map[string]any{"id": "c", "title": "x", "created_at": 1, "updated_at": 2, "messages": []any{}}, "runtime": map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": secret}}}})
	request, _ := json.Marshal(rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: data})
	httpReq, _ := http.NewRequest("POST", server.URL+"/v1/rpc", bytes.NewReader(request))
	httpReq.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err = http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	var opened rpc.Reply
	if err = json.NewDecoder(resp.Body).Decode(&opened); err != nil || !opened.OK {
		t.Fatalf("open: %+v %v", opened, err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	wsAuth, _ := json.Marshal(Message{Type: "auth", ClientID: owner, Secret: pairSecret})
	if err = ws.Write(ctx, websocket.MessageText, wsAuth); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	openWS, _ := json.Marshal(rpc.Request{ID: "attach", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"sync"}`)})
	if err = ws.Write(ctx, websocket.MessageText, openWS); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpCtx, stop := context.WithCancel(ctx)
	defer stop()
	go n.ServeTCP(tcpCtx, listener)
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	peer := peer{conn: conn}
	if err = peer.send(Message{Type: "auth", Protocol: 5, ClientID: owner, Secret: pairSecret}); err != nil {
		t.Fatal(err)
	}
	var paired map[string]any
	if err = frameRead(conn, &paired); err != nil {
		t.Fatal(err)
	}
	if err = peer.send(rpc.Request{ID: "run", Op: "session.run", SessionID: "c", Data: json.RawMessage(`{"content":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	var reply rpc.Reply
	if err = frameRead(conn, &reply); err != nil || !reply.OK {
		t.Fatalf("run: %+v %v", reply, err)
	}
	done := false
	for !done {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("secret in websocket event")
		}
		var event struct {
			Kind string `json:"kind"`
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &event) == nil && event.Kind == "error" {
			if event.Data.Code != "secret_leak_blocked" {
				t.Fatalf("error: %s", raw)
			}
			done = true
		}
	}
	poll, _ := http.NewRequest("GET", server.URL+"/v1/events?session_id=c&after=0", nil)
	poll.Header.Set("Authorization", "Bearer "+auth.Token)
	poll.Header.Set("Accept", "application/json")
	resp, err = http.DefaultClient.Do(poll)
	if err != nil {
		t.Fatal(err)
	}
	eventData, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || bytes.Contains(eventData, []byte(secret)) || !bytes.Contains(eventData, []byte("secret_leak_blocked")) {
		t.Fatalf("HTTP event leak or gap: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(dir, "chats"))
	if err != nil || len(files) != 1 {
		t.Fatalf("chat files: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "chats", files[0].Name()))
	if err != nil || bytes.Contains(stored, []byte(secret)) {
		t.Fatalf("chat leaked credential: %v", err)
	}
}

func TestV5PeerWriteTimeoutBoundsSlowClient(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	p := &peer{conn: server, writeTimeout: 30 * time.Millisecond}
	start := time.Now()
	err := p.send(map[string]any{"payload": "blocked"})
	if e, ok := err.(net.Error); !ok || !e.Timeout() || time.Since(start) > 2*time.Second {
		t.Fatalf("slow write was not bounded: %v", err)
	}
}

func TestV5EventForwarderDrainsAllPagesWithoutNewEvents(t *testing.T) {
	payload := strings.Repeat("x", 240<<10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": payload})
	}))
	defer upstream.Close()
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data", AllowLocalAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	owner, secret, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	runtime := map[string]any{"source": "mobile_ephemeral", "customTools": []any{map[string]any{"id": "t", "name": "tool", "enabled": true, "url": upstream.URL, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}, "toolGrants": map[string]any{"custom_http": map[string]bool{"t": true}}}
	openData, _ := json.Marshal(map[string]any{"storage": "memory", "conversation": map[string]any{"id": "c", "title": "test", "created_at": 1, "updated_at": 2, "messages": []any{}}, "runtime": runtime})
	if reply := n.rpc.Dispatch(owner, rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: openData}); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	var cursor uint64
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("call-%d", i)
		data, _ := json.Marshal(map[string]any{"kind": "custom_http", "tool_id": "t", "arguments": map[string]any{}})
		if reply := n.rpc.Dispatch(owner, rpc.Request{ID: id, Op: "tool.call", SessionID: "c", Data: data}); !reply.OK {
			t.Fatalf("tool: %+v", reply)
		}
		deadline := time.Now().Add(8 * time.Second)
		complete := false
		for !complete && time.Now().Before(deadline) {
			events, err := n.rpc.Events(owner, "c", cursor)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				cursor = event.Seq
				if event.Kind == "error" {
					t.Fatalf("tool event: %+v", event.Data)
				}
				if event.Kind == "completed" {
					complete = true
				}
			}
			if !complete {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !complete {
			t.Fatal("tool timed out")
		}
	}
	updates, cancel, err := n.rpc.Subscribe(owner, "c")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	forwarded := make(chan chatsession.Event, 16)
	go n.forwardEvents(owner, "c", 0, updates, func(e chatsession.Event) error { forwarded <- e; return nil })
	deadline := time.After(8 * time.Second)
	completed := 0
	for completed < 3 {
		select {
		case event := <-forwarded:
			if event.Kind == "completed" {
				completed++
			}
		case <-deadline:
			t.Fatal("forwarder stopped before draining pages")
		}
	}
	token, err := n.issueAPIToken(owner)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(n.HTTPHandler())
	defer api.Close()
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api.URL+"/v1/events?session_id=c&after=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	completed = 0
	for completed < 3 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE stopped before draining pages: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event chatsession.Event
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "completed" {
			completed++
		}
	}
	// Fill the bounded event buffer, then verify both stale and retained cursors.
	response.Body.Close()
	cancel()
	var retained uint64
	for i := 3; i < 12; i++ {
		if i == 11 {
			retained = cursor
		}
		data, _ := json.Marshal(map[string]any{"kind": "custom_http", "tool_id": "t", "arguments": map[string]any{}})
		if reply := n.rpc.Dispatch(owner, rpc.Request{ID: fmt.Sprintf("call-%d", i), Op: "tool.call", SessionID: "c", Data: data}); !reply.OK {
			t.Fatalf("tool %d: %+v", i, reply)
		}
		deadline := time.Now().Add(8 * time.Second)
		finished := false
		for !finished && time.Now().Before(deadline) {
			events, err := n.rpc.Events(owner, "c", cursor)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				cursor = event.Seq
				if event.Kind == "error" {
					t.Fatalf("tool failed: %+v", event)
				}
				if event.Kind == "completed" {
					finished = true
				}
			}
			if !finished {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !finished {
			t.Fatal("tool timed out")
		}
	}
	requestEvents := func(after uint64) (*http.Response, map[string]json.RawMessage) {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/events?session_id=c&after=%d", api.URL, after), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp, body
	}
	resp, body := requestEvents(0)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body["error"], []byte("event_gap")) {
		t.Fatalf("stale cursor: %d %v", resp.StatusCode, body)
	}
	resp, body = requestEvents(retained)
	var resumed []chatsession.Event
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body["events"], &resumed) != nil || len(resumed) == 0 || resumed[0].Seq <= retained {
		t.Fatalf("retained cursor: %d %v", resp.StatusCode, body)
	}

	wsCtx, wsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wsCancel()
	checkWS := func(after uint64, wantGap bool) {
		c, _, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(api.URL, "http")+"/v1/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		c.SetReadLimit(maxFrame)
		write := func(v any) {
			b, _ := json.Marshal(v)
			if err := c.Write(wsCtx, websocket.MessageText, b); err != nil {
				t.Fatal(err)
			}
		}
		read := func() map[string]json.RawMessage {
			_, b, err := c.Read(wsCtx)
			if err != nil {
				t.Fatal(err)
			}
			var v map[string]json.RawMessage
			if err = json.Unmarshal(b, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		write(Message{Type: "auth", ClientID: owner, Secret: secret})
		read()
		data, _ := json.Marshal(map[string]any{"storage": "memory", "after": after})
		write(rpc.Request{ID: "reopen", Op: "session.open", SessionID: "c", Data: data})
		for {
			v := read()
			if string(v["kind"]) == `"event_gap"` {
				if !wantGap {
					t.Fatal("unexpected gap")
				}
				return
			}
			if len(v["seq"]) > 0 {
				var seq uint64
				if json.Unmarshal(v["seq"], &seq) != nil || seq <= after || wantGap {
					t.Fatalf("unexpected event: %v", v)
				}
				return
			}
			if string(v["id"]) == `"reopen"` && string(v["ok"]) != "true" {
				t.Fatalf("reopen failed: %v", v)
			}
		}
	}
	checkWS(0, true)
	checkWS(retained, false)

}

func TestV5HTTPAndWebSocketCannotRedeemLegacyTickets(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	body, _ := json.Marshal(map[string]any{"ticket": invite.Ticket})
	response, err := http.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP redeemed ticket: %d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	body, _ = json.Marshal(map[string]any{"type": "auth", "ticket": invite.Ticket})
	if err = ws.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ws.Read(ctx); err == nil {
		t.Fatal("WebSocket redeemed ticket")
	}
	if _, _, err = n.pairWithTicket(invite.Ticket, ""); err != nil {
		t.Fatalf("rejected request consumed ticket: %v", err)
	}
}

func TestScopedTCPPairingIsNotValidForHTTPOrWebSocket(t *testing.T) {
	identity, data := t.TempDir(), t.TempDir()
	n, err := New(Config{IdentityDir: identity, DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	pair, err := n.pairTCP()
	if err != nil {
		t.Fatal(err)
	}
	id, secret := pair["client_id"].(string), pair["secret"].(string)
	if !n.verifyPairFor(id, secret, "tcp") || n.verifyPairFor(id, secret, "http") {
		t.Fatal("pairing scope not enforced")
	}
	n2, err := New(Config{IdentityDir: identity, DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	if !n2.verifyPairFor(id, secret, "tcp") || n2.verifyPairFor(id, secret, "http") {
		t.Fatal("pairing scope lost on restart")
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	body, _ := json.Marshal(map[string]string{"client_id": id, "secret": secret})
	response, err := http.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("TCP identity accepted by HTTP: %d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	body, _ = json.Marshal(map[string]string{"type": "auth", "client_id": id, "secret": secret})
	if err = ws.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ws.Read(ctx); err == nil {
		t.Fatal("TCP identity accepted by WebSocket")
	}
}

func TestV5TCPRejectsTicketPairingWithoutConsumingTicket(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.ServeTCP(ctx, listener)
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err = (&peer{conn: conn}).send(Message{Type: "auth", Protocol: 5, Ticket: invite.Ticket}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err = frameRead(conn, &response); err != nil || response["type"] != "error" {
		t.Fatalf("TCP ticket accepted: %v %v", response, err)
	}
	if _, _, err = n.pairWithTicket(invite.Ticket, ""); err != nil {
		t.Fatalf("rejected ticket was consumed: %v", err)
	}
}

func TestV5InterruptedStreamResumeAcrossHTTPAndWebSocket(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first part of the streaming response\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" second part\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data"), AllowLocalAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, secret, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(n.HTTPHandler())
	client := server.Client()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https")+"/v1/ws", &websocket.DialOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(Message{Type: "auth", ClientID: id, Secret: secret})
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Read(ctx); err != nil {
			t.Fatal(err)
		}
		return c
	}
	send := func(c *websocket.Conn, v any) {
		b, _ := json.Marshal(v)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func(c *websocket.Conn) map[string]json.RawMessage {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]json.RawMessage
		if err = json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	c := connect()
	data, _ := json.Marshal(map[string]any{"storage": "memory", "conversation": map[string]any{"id": "resume", "title": "x", "created_at": 1, "updated_at": 2, "messages": []any{}}, "runtime": map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": "secret"}}}})
	send(c, rpc.Request{ID: "open", Op: "session.open", SessionID: "resume", Data: data})
	if string(read(c)["ok"]) != "true" {
		t.Fatal("open failed")
	}
	run := rpc.Request{ID: "run", Op: "session.run", SessionID: "resume", Data: json.RawMessage(`{"content":"hello"}`)}
	send(c, run)
	var cursor uint64
	for cursor == 0 {
		v := read(c)
		if string(v["kind"]) == `"delta"` {
			if err := json.Unmarshal(v["seq"], &cursor); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.CloseNow()
	// Retry the uncertain request over HTTP while the original upstream is still running.
	authBody, _ := json.Marshal(Message{ClientID: id, Secret: secret})
	resp, err := client.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(authBody))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	b, _ := json.Marshal(run)
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/rpc", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var reply rpc.Reply
	if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil || !reply.OK {
		t.Fatalf("retry: %+v %v", reply, err)
	}
	resp.Body.Close()
	close(release)
	// Resume via HTTP from the last observed sequence, then switch back to WS.
	poll, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/events?session_id=resume&after=%d", server.URL, cursor), nil)
	poll.Header.Set("Authorization", "Bearer "+auth.Token)
	poll.Header.Set("Accept", "application/json")
	resp, err = client.Do(poll)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Events []chatsession.Event `json:"events"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&page); err != nil || resp.StatusCode != http.StatusOK || len(page.Events) == 0 {
		t.Fatalf("poll: %+v %v", page, err)
	}
	resp.Body.Close()
	events := page.Events
	cursor = events[0].Seq
	c = connect()
	defer c.CloseNow()
	data, _ = json.Marshal(map[string]any{"storage": "memory", "after": cursor})
	send(c, rpc.Request{ID: "reopen", Op: "session.open", SessionID: "resume", Data: data})
	completed := false
	for !completed {
		v := read(c)
		if len(v["seq"]) > 0 {
			var seq uint64
			if err = json.Unmarshal(v["seq"], &seq); err != nil {
				t.Fatal(err)
			}
			if seq <= cursor {
				t.Fatalf("replayed cursor %d after %d", seq, cursor)
			}
			cursor = seq
		}
		if string(v["kind"]) == `"completed"` {
			completed = true
		}
		if string(v["kind"]) == `"error"` || string(v["kind"]) == `"event_gap"` {
			t.Fatalf("unexpected event: %v", v)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream executed %d times", calls.Load())
	}
	// A side-effecting tool also retains its receipt when the originating WS disconnects.
	var toolCalls atomic.Int32
	toolStarted, toolRelease := make(chan struct{}), make(chan struct{})
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolCalls.Add(1)
		close(toolStarted)
		<-toolRelease
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":"done"}`)
	}))
	defer toolServer.Close()
	defer func() {
		select {
		case <-toolRelease:
		default:
			close(toolRelease)
		}
	}()
	toolWS := connect()
	toolRuntime := map[string]any{"source": "mobile_ephemeral", "customTools": []any{map[string]any{"id": "t", "name": "tool", "enabled": true, "url": toolServer.URL, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}, "toolGrants": map[string]any{"custom_http": map[string]bool{"t": true}}}
	toolData, _ := json.Marshal(map[string]any{"storage": "sync", "conversation": map[string]any{"id": "c2", "title": "x", "created_at": 1, "updated_at": 2, "messages": []any{}}, "runtime": toolRuntime})
	send(toolWS, rpc.Request{ID: "tool-open", Op: "session.open", SessionID: "c2", Data: toolData})
	if opened := read(toolWS); string(opened["ok"]) != "true" {
		t.Fatalf("tool open failed: %v", opened)
	}
	toolReq := rpc.Request{ID: "tool-call", Op: "tool.call", SessionID: "c2", Data: json.RawMessage(`{"kind":"custom_http","tool_id":"t","arguments":{}}`)}
	send(toolWS, toolReq)
	select {
	case <-toolStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	toolWS.CloseNow()
	b, _ = json.Marshal(toolReq)
	req, _ = http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/rpc", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil || !reply.OK {
		t.Fatalf("tool retry: %+v %v", reply, err)
	}
	resp.Body.Close()
	close(toolRelease)
	var toolCursor uint64
	for {
		poll, _ = http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/events?session_id=c2&after=%d", server.URL, toolCursor), nil)
		poll.Header.Set("Authorization", "Bearer "+auth.Token)
		poll.Header.Set("Accept", "application/json")
		resp, err = client.Do(poll)
		if err != nil {
			t.Fatal(err)
		}
		page.Events = nil
		if err = json.NewDecoder(resp.Body).Decode(&page); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("tool poll: %+v %v", page, err)
		}
		resp.Body.Close()
		done := false
		for _, event := range page.Events {
			toolCursor = event.Seq
			if event.Kind == "error" {
				t.Fatalf("tool failed: %+v", event)
			}
			if event.Kind == "completed" {
				done = true
			}
		}
		if done {
			break
		}
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("tool executed %d times", toolCalls.Load())
	}
	conflict := run
	conflict.Data = json.RawMessage(`{"content":"different"}`)
	b, _ = json.Marshal(conflict)
	req, _ = http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/rpc", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.Error == nil || reply.Error.Code != "request_already_recorded" {
		t.Fatalf("conflicting retry: %+v %v", reply, err)
	}
	resp.Body.Close()
	gap := connect()
	defer gap.CloseNow()
	data, _ = json.Marshal(map[string]any{"storage": "memory", "after": cursor + 1})
	send(gap, rpc.Request{ID: "future", Op: "session.open", SessionID: "resume", Data: data})
	for {
		v := read(gap)
		if string(v["kind"]) == `"event_gap"` {
			break
		}
		if string(v["id"]) == `"future"` && string(v["ok"]) != "true" {
			t.Fatalf("open: %v", v)
		}
	}
}

func TestV5RejectsOversizedTransportRequests(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	id, secret, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	token, err := n.issueAPIToken(id)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), maxFrame+1)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/rpc", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var reply rpc.Reply
	err = json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge || reply.Error == nil || reply.Error.Code != "payload_too_large" {
		t.Fatalf("HTTP: %d %+v %v", resp.StatusCode, reply, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	auth, _ := json.Marshal(Message{Type: "auth", Protocol: 5, ClientID: id, Secret: secret})
	if err = c.Write(ctx, websocket.MessageText, auth); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("WebSocket oversized frame: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go n.ServeTCP(ctx, listener)
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer := &peer{conn: conn}
	if err = peer.send(Message{Type: "auth", Protocol: 5, ClientID: id, Secret: secret}); err != nil {
		t.Fatal(err)
	}
	var paired map[string]any
	if err = frameRead(conn, &paired); err != nil || paired["type"] != "paired" {
		t.Fatalf("TCP auth: %v %v", paired, err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxFrame+1)
	if _, err = conn.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err = conn.Read(data[:]); err == nil {
		t.Fatal("oversized TCP frame was accepted")
	} else if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
		t.Fatalf("TCP frame was not rejected promptly: %v", err)
	}

}

func TestV5SubscriptionLimitIsReportedAndCanRecover(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, secret, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	open := rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}}`)}
	if reply := n.rpc.Dispatch(id, open); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	var cancels []func()
	for i := 0; i < 64; i++ {
		_, cancel, err := n.rpc.Subscribe(id, "c")
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	token, err := n.issueAPIToken(id)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/events?session_id=c", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var reply rpc.Reply
	err = json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || reply.Error == nil || reply.Error.Code != "resource_exhausted" {
		t.Fatalf("HTTP: %d %+v %v", resp.StatusCode, reply, err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	write := func(v any) {
		b, _ := json.Marshal(v)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func(v any) {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	write(Message{Type: "auth", Protocol: 5, ClientID: id, Secret: secret})
	var auth map[string]any
	read(&auth)
	open.Data = json.RawMessage(`{"storage":"memory"}`)
	write(open)
	read(&reply)
	if reply.OK || reply.Error == nil || reply.Error.Code != "resource_exhausted" {
		t.Fatalf("WS limit: %+v", reply)
	}
	cancels[0]()
	write(open)
	reply = rpc.Reply{}
	read(&reply)
	if !reply.OK {
		t.Fatalf("WS retry: %+v", reply)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go n.ServeTCP(ctx, listener)
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	p := &peer{conn: conn}
	if err = p.send(Message{Type: "auth", Protocol: 5, ClientID: id, Secret: secret}); err != nil {
		t.Fatal(err)
	}
	var paired map[string]any
	if err = frameRead(conn, &paired); err != nil || paired["type"] != "paired" {
		t.Fatalf("TCP auth: %v %v", paired, err)
	}
	if err = p.send(open); err != nil {
		t.Fatal(err)
	}
	reply = rpc.Reply{}
	if err = frameRead(conn, &reply); err != nil || reply.Error == nil || reply.Error.Code != "resource_exhausted" {
		t.Fatalf("TCP limit: %+v %v", reply, err)
	}
}

func TestV5SSEReportsClosedSessionInsteadOfEventGap(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, _, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	open := rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}}`)}
	if reply := n.rpc.Dispatch(id, open); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	token, err := n.issueAPIToken(id)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events?session_id=c", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: %d", resp.StatusCode)
	}
	if reply := n.rpc.Dispatch(id, rpc.Request{ID: "close", Op: "session.close", SessionID: "c"}); !reply.OK {
		t.Fatalf("close: %+v", reply)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil || event["code"] != "not_found" {
				t.Fatalf("SSE error: %q %v", line, err)
			}
			return
		}
	}
}

func TestV5RevokedPendingPollReturnsUnauthorized(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, _, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	open := rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}}`)}
	if reply := n.rpc.Dispatch(id, open); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	var cancels []func()
	for i := 0; i < 63; i++ {
		_, cancel, err := n.rpc.Subscribe(id, "c")
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	token, err := n.issueAPIToken(id)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events?session_id=c", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	type outcome struct {
		status int
		reply  rpc.Reply
		err    error
	}
	result := make(chan outcome, 1)
	startPoll := func() {
		go func() {
			resp, err := http.DefaultClient.Do(req.Clone(ctx))
			if err != nil {
				result <- outcome{err: err}
				return
			}
			defer resp.Body.Close()
			var reply rpc.Reply
			err = json.NewDecoder(resp.Body).Decode(&reply)
			result <- outcome{resp.StatusCode, reply, err}
		}()
	}
	startPoll()
	for {
		_, extra, err := n.rpc.Subscribe(id, "c")
		if errors.Is(err, chatsession.ErrLimit) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		extra()
		select {
		case got := <-result:
			// The short-lived probe may have occupied the last slot first.
			if got.err != nil || got.status != http.StatusTooManyRequests {
				t.Fatalf("poll ended before revocation: %+v", got)
			}
			startPoll()
		default:
		}
		select {
		case <-ctx.Done():
			t.Fatal("poll did not subscribe")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = n.revokePair(id); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.status != http.StatusUnauthorized || got.reply.Error == nil || got.reply.Error.Code != "unauthorized" {
			t.Fatalf("revoked poll: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("poll did not finish")
	}
}

func TestV5SSEClosesOnRevocationWithoutSessionError(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, _, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	open := rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}}`)}
	if reply := n.rpc.Dispatch(id, open); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	token, err := n.issueAPIToken(id)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events?session_id=c", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err = n.revokePair(id); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) != 0 {
		t.Fatalf("revoked SSE sent session data: %q %v", body, err)
	}
}

func TestV5HTTPRuntimeErrorStatuses(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	invite, _ := n.IssueTicket()
	id, _, err := n.pairWithTicket(invite.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := n.issueAPIToken(id)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	call := func(req rpc.Request) (int, rpc.Reply) {
		b, _ := json.Marshal(req)
		h, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/rpc", bytes.NewReader(b))
		h.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(h)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply rpc.Reply
		if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, reply
	}
	open := rpc.Request{ID: "open", Op: "session.open", SessionID: "c", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}}`)}
	if _, reply := call(open); !reply.OK {
		t.Fatalf("open: %+v", reply)
	}
	run := rpc.Request{ID: "run", Op: "session.run", SessionID: "c", Data: json.RawMessage(`{"content":"hello"}`)}
	status, reply := call(run)
	if status != http.StatusBadRequest || reply.Error == nil || reply.Error.Code != "missing_runtime_config" {
		t.Fatalf("missing runtime: %d %+v", status, reply)
	}
	configure := rpc.Request{ID: "configure", Op: "session.configure", SessionID: "c", Data: json.RawMessage(`{"runtime":{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"model","auth":{"method":"oauth"}}}}`)}
	if _, reply = call(configure); !reply.OK {
		t.Fatalf("configure: %+v", reply)
	}
	status, reply = call(run)
	if status != http.StatusUnauthorized || reply.Error == nil || reply.Error.Code != "auth_required" {
		t.Fatalf("missing OAuth token: %d %+v", status, reply)
	}
}
