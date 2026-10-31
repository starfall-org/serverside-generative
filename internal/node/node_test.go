package node

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testNode(t *testing.T) *Node {
	t.Helper()
	n, err := New(Config{IdentityDir: t.TempDir(), Advertise: "127.0.0.1:7090", Password: "correct-password-long"})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestTicketOneTimeAndPersistence(t *testing.T) {
	n := testNode(t)
	invitation, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	id, secret, err := n.pairWithTicket(invitation.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = n.pairWithTicket(invitation.Ticket, ""); err == nil {
		t.Fatal("reused ticket accepted")
	}
	if !n.verifyPair(id, secret) {
		t.Fatal("valid pair rejected")
	}
	if n.verifyPair(id, "invalid") {
		t.Fatal("wrong pair credential accepted")
	}
	rebuilt, err := New(n.Config)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Fingerprint != n.Fingerprint {
		t.Fatal("TLS identity did not persist")
	}
	if !rebuilt.verifyPair(id, secret) {
		t.Fatal("paired credentials not restored")
	}
	contents, err := os.ReadFile(filepath.Join(n.Config.IdentityDir, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), secret) {
		t.Fatal("client secret persisted in plaintext")
	}
	info, err := os.Stat(filepath.Join(n.Config.IdentityDir, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("insecure client store perms: %o", info.Mode().Perm())
	}
}
func TestHumanManualAndExpiry(t *testing.T) {
	n := testNode(t)
	h, err := n.IssueHuman()
	if err != nil {
		t.Fatal(err)
	}
	if len(h.PIN) != 12 || !strings.HasPrefix(h.URI, "generative://pair?") {
		t.Fatalf("bad human invitation: %+v", h)
	}
	if _, _, err = n.pairWithTicket("", h.PIN); err != nil {
		t.Fatal(err)
	}
	if _, _, err = n.pairWithTicket("", h.PIN); err == nil {
		t.Fatal("reused PIN")
	}
	ticket, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(ticket.Ticket))
	n.mu.Lock()
	n.tickets[key] = time.Now().Add(-time.Second)
	n.mu.Unlock()
	if _, _, err = n.pairWithTicket(ticket.Ticket, ""); err == nil {
		t.Fatal("expired ticket")
	}
}
func TestAuthenticationRateLimit(t *testing.T) {
	n := testNode(t)
	for i := 0; i < 10; i++ {
		if !n.limit("tcp:127.0.0.1") {
			t.Fatal("blocked too early")
		}
	}
	if !n.blocked("tcp:127.0.0.1") {
		t.Fatal("not blocked after 10 failures")
	}
	if n.limit("tcp:127.0.0.1") {
		t.Fatal("accepted extra failed attempt")
	}
	n.mu.Lock()
	n.attempts["tcp:127.0.0.1"].Until = time.Now().Add(-time.Second)
	n.mu.Unlock()
	if n.blocked("tcp:127.0.0.1") {
		t.Fatal("expired limit did not reset")
	}
}
func TestSessionRAMAndGap(t *testing.T) {
	n := testNode(t)
	s, err := n.getSession("client1", "chat", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Handoff(1, json.RawMessage(`{"api_key":"DO_NOT_PERSIST","mcp":[1]}`)); err != nil {
		t.Fatal(err)
	}
	if err = s.Handoff(1, json.RawMessage(`{"x":2}`)); err == nil {
		t.Fatal("stale revision accepted")
	}
	s.Publish("job", "delta", "abc")
	s.Publish("job", "completed", "")
	events, err := s.EventsSince(0)
	if err != nil || len(events) != 2 {
		t.Fatalf("replay: %+v %v", events, err)
	}
	s.Ack(1)
	if _, err = s.EventsSince(0); err == nil {
		t.Fatal("missing sync_gap after ACK")
	}
	events, err = s.EventsSince(1)
	if err != nil || len(events) != 1 {
		t.Fatalf("wrong replay since 1: %+v %v", events, err)
	}
	s.Ack(2)
	if len(s.Events) != 0 {
		t.Fatal("acked events still in memory")
	}
	if _, err = n.getSession("otherClient", "chat", false); err == nil {
		t.Fatal("cross-client isolation broken")
	}
	files, err := os.ReadDir(n.Config.IdentityDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(n.Config.IdentityDir, f.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "DO_NOT_PERSIST") {
			t.Fatal("RAM session was written to disk")
		}
	}
}
func TestSessionEventBufferOverflow(t *testing.T) {
	s := &Session{ID: "one", Subscribers: map[chan struct{}]struct{}{}}
	for i := 0; i < maxEvents+10; i++ {
		s.Publish("job", "delta", "x")
	}
	if s.Floor != 10 {
		t.Fatalf("floor=%d want 10", s.Floor)
	}
	if _, err := s.EventsSince(0); err == nil {
		t.Fatal("overflow did not signal a sync gap")
	}
	events, err := s.EventsSince(10)
	if err != nil || len(events) != maxEvents {
		t.Fatalf("overflow replay len=%d err=%v", len(events), err)
	}
}
func TestSubscriberLifecycle(t *testing.T) {
	s := &Session{ID: "one", Subscribers: map[chan struct{}]struct{}{}}
	watcher, cancel := s.Subscribe()
	s.Publish("job", "delta", "hi")
	select {
	case <-watcher:
	case <-time.After(time.Second):
		t.Fatal("missing notification")
	}
	cancel()
	if _, ok := <-watcher; ok {
		t.Fatal("watcher not closed")
	}
	s.Publish("job", "completed", "")
}
func TestConcurrentSessionExecution(t *testing.T) {
	s := &Session{ID: "one", Subscribers: map[chan struct{}]struct{}{}, Started: map[string]bool{}, Jobs: map[string]context.CancelFunc{}}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Start("same", "echo", "hello", 10); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := s.EventsSince(0)
		completed := 0
		for _, event := range events {
			if event.Event == "completed" {
				completed++
			}
		}
		if completed == 1 {
			return
		}
		if completed > 1 {
			t.Fatal("duplicate completion")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("execution never completed")
}
func TestHTTPBindSafety(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7091", "localhost:7091", "[::1]:7091", "off"} {
		if err := SafeHTTPBind(addr, false); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:7091", ":7091", "192.168.1.5:7091"} {
		if err := SafeHTTPBind(addr, false); err == nil {
			t.Fatalf("accepted unsafe binding %s", addr)
		}
	}
}
