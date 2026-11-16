package chatsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/chatstore"
	"github.com/starfall-org/serverside-generative/internal/secrets"
	"github.com/starfall-org/serverside-generative/internal/storedruntime"
)

func TestRuntimeSecretCollectionAndSplitStream(t *testing.T) {
	raw := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"auth":{"method":"bearer_token","value":"long-secret"}},"customTools":[{"headers":{"X-Key":"tool-secret"},"url":"https://example.invalid/?key=abc%2Fdef"}],"toolSettings":{"mcpToolParameters":{"s":{"t":{"password":{"defaultValue":"secret-default"}}}}}}`)
	secrets := runtimeSecrets(raw)
	for _, expected := range []string{"long-secret", "tool-secret", "abc/def", "abc%2Fdef", "secret-default"} {
		found := false
		for _, value := range secrets {
			if value == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("secret not detected: %q", expected)
		}
	}
	guard := newStreamGuard([]string{"split-secret"})
	text := guard.push("ordinary split-", false) + guard.push("secret end", false) + guard.push("", true)
	if !guard.leaked || strings.Contains(text, "split-secret") {
		t.Fatalf("split leak: %q", text)
	}
}
func TestEchoedRuntimeSecretsNeverReachSyncedChatOrEvents(t *testing.T) {
	const secret = "runtime-sentinel-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"before runtime-sentinel-\"}}]}\n\ndata: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"secret after\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": secret}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.Run("a", "c", "run", "hello"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	cursor := uint64(0)
	done := false
	for !done {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), secret) {
				t.Fatal("secret leaked to event")
			}
			if event.Kind == "error" {
				data := event.Data.(map[string]any)
				if data["code"] != "secret_leak_blocked" {
					t.Fatalf("wrong error: %+v", event)
				}
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	if view, err := m.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM conversation: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}
func TestEchoedToolHeaderBlocked(t *testing.T) {
	const secret = "runtime-tool-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"echo": r.Header.Get("X-Key")})
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	m.AllowLocalAPI(true)
	tool := map[string]any{"id": "t", "name": "tool", "enabled": true, "url": upstream.URL, "headers": map[string]string{"X-Key": secret}, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "customTools": []any{tool}, "toolGrants": map[string]any{"custom_http": map[string]bool{"t": true}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := m.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = m.StartCustomTool("a", "c", "call", "t", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	cursor := uint64(0)
	done := false
	for !done {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), secret) {
				t.Fatal("secret leaked to event")
			}
			if event.Kind == "error" {
				if event.Data.(map[string]any)["code"] != "secret_leak_blocked" {
					t.Fatalf("wrong error: %+v", event)
				}
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	if view, err := m.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestMCPHiddenSecretDefaultsAreNotPersisted(t *testing.T) {
	const secret = "hidden-default-secret"
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type input struct {
		Password string `json:"password"`
	}
	type output struct {
		Echo string `json:"echo"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in input) (*mcp.CallToolResult, output, error) {
		return nil, output{Echo: in.Password}, nil
	})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	manager := New(chatstore.New(dir))
	manager.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "mcp": []any{map[string]any{"id": "s", "protocol": "streamable_http", "url": upstream.URL}}, "toolGrants": map[string]any{"mcp": map[string]any{"s": map[string]any{"enabled": true, "tools": map[string]bool{"echo": true}}}}, "toolSettings": map[string]any{"quickMcp": map[string]bool{"s": true}, "mcpTools": map[string]any{"s": map[string]bool{"echo": true}}, "mcpToolParameters": map[string]any{"s": map[string]any{"echo": map[string]any{"password": map[string]any{"enabled": false, "defaultValue": secret}}}}}})
	if _, err := manager.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := manager.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = manager.StartMCPTool("a", "c", "call", "s", "echo", json.RawMessage(`{"password":"ignored-user-value"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	cursor := uint64(0)
	done := false
	for !done {
		events, err := manager.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), secret) {
				t.Fatal("secret leaked to event")
			}
			if event.Kind == "error" {
				if event.Data.(map[string]any)["code"] != "secret_leak_blocked" {
					t.Fatalf("wrong error: %+v", event)
				}
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	if view, err := manager.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestRuntimeSecretCannotBecomeReceiptOrEventID(t *testing.T) {
	const secret = "secret-receipt-sentinel"
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"id":"p","type":"openai","modelId":"m","auth":{"method":"bearer_token","value":"secret-receipt-sentinel"}}}`)
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save("a", "c", secret, 1, sample); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("save ID accepted: %v", err)
	}
	if err := m.Run("a", "c", secret, "hello"); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("run ID accepted: %v", err)
	}
	if view, err := m.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestToolArgumentsCannotPersistRuntimeSecret(t *testing.T) {
	const secret = "tool-argument-runtime-secret"
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	tool := map[string]any{"id": "t", "name": "tool", "enabled": true, "url": "https://example.com", "headers": map[string]string{"X-Key": secret}, "parameters": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}}}
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "customTools": []any{tool}, "toolGrants": map[string]any{"custom_http": map[string]bool{"t": true}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"input": secret})
	if err := m.StartCustomTool("a", "c", "call", "t", args); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("expected leak rejection: %v", err)
	}
	if view, err := m.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestUserSuppliedSecretNotPersisted(t *testing.T) {
	const secret = "runtime-user-content-secret"
	dir := filepath.Join(t.TempDir(), "data")
	m := New(chatstore.New(dir))
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": "https://example.com", "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": secret}}})
	var conversation map[string]any
	if err := json.Unmarshal(sample, &conversation); err != nil {
		t.Fatal(err)
	}
	conversation["title"] = secret
	contaminated, _ := json.Marshal(conversation)
	if _, err := m.Open("a", "c", contaminated, runtime); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chats")); !os.IsNotExist(err) {
		t.Fatal("rejected open wrote files")
	}
	view, err := m.Open("a", "c", sample, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save("a", "c", "save", view.Revision, contaminated); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("save: %v", err)
	}
	if err := m.Run("a", "c", "run", secret); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("run: %v", err)
	}
	if view, err := m.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestStoredMCPExecutesWithExplicitGrantsAndBlocksEcho(t *testing.T) {
	const secret = "stored-mcp-runtime-secret"
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type input struct {
		Text string `json:"text"`
	}
	type output struct {
		Echo string `json:"echo"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, output, error) {
		return nil, output{Echo: secret}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != secret {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	docs, err := catalog.Empty()
	if err != nil {
		t.Fatal(err)
	}
	docs["mcp.yaml"] = "schema_version: 1\nrevision: 1\nservers:\n  - id: s\n    name: S\n    protocol: streamable_http\n    url: " + upstream.URL + "\n    auth: {method: CUSTOM_HEADER, header_name: X-Token, credential_ref: mcp/s/key}\n"
	docs["profiles.yaml"] = "schema_version: 1\nrevision: 1\nchatProfiles: []\ntoolSettings:\n  quickMcp: {s: true}\n  mcpTools: {s: {echo: true}}\n"
	if _, err = catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put("mcp", "s", "key", "", "auth", []byte(secret)); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	manager := New(chatstore.New(dir))
	manager.AllowLocalAPI(true)
	manager.SetStoredMCPResolver(func(r, c json.RawMessage, server string) (json.RawMessage, error) {
		return storedruntime.ResolveMCP(dir, r, c, server)
	})
	runtime := json.RawMessage(`{"source":"stored","toolGrants":{"mcp":{"s":{"enabled":true,"tools":{"echo":true}}}}}`)
	if _, err = manager.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	denied := json.RawMessage(`{"source":"stored","toolGrants":{"mcp":{"s":{"enabled":false,"tools":{"echo":true}}}}}`)
	if err = manager.Configure("a", "c", denied); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.MCPTools("a", "c", "s"); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled grant: %v", err)
	}
	if err = manager.Configure("a", "c", runtime); err != nil {
		t.Fatal(err)
	}
	listing, err := manager.MCPTools("a", "c", "s")
	if err != nil || !strings.Contains(string(listing), "echo") {
		t.Fatalf("list: %v", err)
	}
	ch, stop, err := manager.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = manager.StartMCPTool("a", "c", "call", "s", "echo", json.RawMessage(`{"text":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	cursor := uint64(0)
	done := false
	for !done {
		events, err := manager.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), secret) {
				t.Fatal("secret in event")
			}
			if event.Kind == "error" {
				if event.Data.(map[string]any)["code"] != "secret_leak_blocked" {
					t.Fatalf("event: %+v", event)
				}
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	if view, err := manager.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestStoredCustomToolRequiresGrantAndBlocksCredentialEcho(t *testing.T) {
	const secret = "stored-custom-runtime-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"echo": r.Header.Get("X-Key")})
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "data")
	tool, _ := json.Marshal(map[string]any{"id": "t", "name": "tool", "enabled": true, "url": upstream.URL, "headers": map[string]string{"X-Key": secret}, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	store, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutTool("t", tool); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	manager := New(chatstore.New(dir))
	manager.AllowLocalAPI(true)
	manager.SetStoredToolResolver(func(r json.RawMessage) (json.RawMessage, error) { return storedruntime.ResolveCustomTools(dir, r) })
	denied := json.RawMessage(`{"source":"stored","toolGrants":{"custom_http":{"t":false}}}`)
	if _, err = manager.Open("a", "c", sample, denied); err != nil {
		t.Fatal(err)
	}
	if err = manager.StartCustomTool("a", "c", "denied", "t", json.RawMessage(`{}`)); !errors.Is(err, ErrDenied) {
		t.Fatalf("grant: %v", err)
	}
	runtime := json.RawMessage(`{"source":"stored","toolGrants":{"custom_http":{"t":true}},"customTools":[{"id":"t","url":"https://evil.invalid"}]}`)
	if err = manager.Configure("a", "c", runtime); err != nil {
		t.Fatal(err)
	}
	listing, err := manager.CustomTools("a", "c")
	if err != nil || len(listing) != 1 {
		t.Fatalf("listing: %v", err)
	}
	encoded, _ := json.Marshal(listing)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "evil.invalid") {
		t.Fatal("sensitive data in listing")
	}
	ch, stop, err := manager.Subscribe("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = manager.StartCustomTool("a", "c", "call", "t", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	cursor := uint64(0)
	done := false
	for !done {
		events, err := manager.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), secret) {
				t.Fatal("secret in event")
			}
			if event.Kind == "error" {
				if event.Data.(map[string]any)["code"] != "secret_leak_blocked" {
					t.Fatalf("event: %+v", event)
				}
				done = true
			}
		}
		if !done {
			select {
			case <-ch:
			case <-deadline:
				t.Fatal("timeout")
			}
		}
	}
	if view, err := manager.Get("a", "c"); err != nil || strings.Contains(string(view.Conversation), secret) {
		t.Fatalf("secret leaked to RAM transcript: %s %v", view.Conversation, err)
	}
	assertStoredSecretAbsent(t, dir, secret)
}

func TestStreamCheckpointDoesNotPersistSplitRuntimeSecret(t *testing.T) {
	const secret = "checkpoint-runtime-secret-sentinel-518913"
	firstSent, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(content string) {
			b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": content}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		write("ordinary safe output with sufficient padding before " + secret[:20])
		close(firstSent)
		<-release
		write(secret[20:])
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	m := New(store)
	m.AllowLocalAPI(true)
	runtime, _ := json.Marshal(map[string]any{"source": "mobile_ephemeral", "provider": map[string]any{"type": "openai", "baseUrl": upstream.URL, "modelId": "test", "auth": map[string]string{"method": "bearer_token", "value": secret}}})
	if _, err := m.Open("a", "c", sample, runtime); err != nil {
		t.Fatal(err)
	}
	if err := m.Run("a", "c", "run", "hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstSent:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not called")
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		snapshot, err := store.Get("a", "c")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Revision >= 3 {
			if strings.Contains(string(snapshot.Conversation), secret[:20]) {
				t.Fatal("checkpoint included buffered secret prefix")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	var cursor uint64
	for time.Now().Before(deadline) {
		events, err := m.Events("a", "c", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			b, _ := json.Marshal(event)
			if strings.Contains(string(b), secret) {
				t.Fatal("secret in event")
			}
			if event.Kind == "error" {
				if !strings.Contains(string(b), "secret_leak_blocked") {
					t.Fatalf("unexpected error: %s", b)
				}
				snapshot, err := store.Get("a", "c")
				if err != nil || strings.Contains(string(snapshot.Conversation), secret) {
					t.Fatalf("secret in stored conversation: %v", err)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stream did not finish")
}

// assertStoredSecretAbsent verifies both sides of the privacy contract: the
// conversation is durable, but transient provider/MCP credentials are not.
func assertStoredSecretAbsent(t *testing.T, dir, secret string) {
	t.Helper()
	snap, err := chatstore.New(dir).Get("a", "c")
	if err != nil {
		t.Fatalf("missing durable chat: %v", err)
	}
	if strings.Contains(string(snap.Conversation), secret) {
		t.Fatalf("runtime secret stored in conversation: %s", snap.Conversation)
	}
}
