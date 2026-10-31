package node

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/execution"
)

func pairID(t *testing.T, n *Node) string {
	t.Helper()
	ticket, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := n.pairWithTicket(ticket.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func makeConfig(t *testing.T, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(execution.ClientContext{
		Instructions: text,
		Skills:       []execution.AgentSkill{{ID: "review", Name: "Reviewer", Content: "# Review"}},
		Providers:    []execution.ProviderMetadata{{ID: "main", Name: "Main", Platform: "openai-chat", BaseURL: "https://api.example.com/v1", Models: []string{"a", "b"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestGlobalSharedConfig(t *testing.T) {
	n := testNode(t)
	a, b := pairID(t, n), pairID(t, n)
	ea := n.beginClientConnection(a)
	rev, digest, _, err := n.applyContext(a, ea, 0, makeConfig(t, "First"))
	if err != nil || rev != 1 {
		t.Fatalf("first update: %d %v", rev, err)
	}
	if got := n.clientContext(b); got.Instructions != "First" || len(got.Skills) != 1 || got.Providers[0].Models[1] != "b" {
		t.Fatalf("data not shared: %+v", got)
	}
	eb := n.beginClientConnection(b)
	count, err := n.confirmContext(b, eb, rev, digest)
	if err != nil || count != 1 {
		t.Fatal("B cannot confirm", err)
	}
	_, _, _, err = n.applyContext(b, eb, 0, makeConfig(t, "Outdated"))
	if err == nil {
		t.Fatal("stale update accepted")
	}
	rev, _, _, err = n.applyContext(b, eb, rev, makeConfig(t, "Second"))
	if err != nil || rev != 2 {
		t.Fatal("B cannot update", err)
	}
	if n.clientContext(a).Instructions != "Second" {
		t.Fatal("A cannot see B update")
	}
	if !n.connectionCurrent(a, ea) {
		t.Fatal("B update disconnected A")
	}
	if _, err = n.confirmContext(a, ea, 1, digest); err == nil {
		t.Fatal("stale confirm accepted")
	}
	path := n.sharedPath()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", err)
	}
	restarted, err := New(n.Config)
	if err != nil || restarted.contextSnapshot().Context.Instructions != "Second" {
		t.Fatal("config lost on restart", err)
	}
	if err = n.revokePair(a); err != nil {
		t.Fatal(err)
	}
	if n.clientContext(b).Instructions != "Second" {
		t.Fatal("revoked client erased global data")
	}
	restarted, err = New(n.Config)
	if err != nil || restarted.contextSnapshot().Context.Instructions != "Second" {
		t.Fatal("revoke erased shared config", err)
	}
}
func TestSharedConfigCASConcurrentAndSchema(t *testing.T) {
	n := testNode(t)
	a, b := pairID(t, n), pairID(t, n)
	ea, eb := n.beginClientConnection(a), n.beginClientConnection(b)
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, tc := range []struct {
		id    string
		epoch uint64
		value string
	}{{a, ea, "A"}, {b, eb, "B"}} {
		wg.Add(1)
		go func(id string, ep uint64, v string) {
			defer wg.Done()
			_, _, _, err := n.applyContext(id, ep, 0, makeConfig(t, v))
			out <- err
		}(tc.id, tc.epoch, tc.value)
	}
	wg.Wait()
	close(out)
	count := 0
	for err := range out {
		if err == nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent CAS allowed %d writes", count)
	}
	bad := []string{
		`{"instructions":"x","skills":[],"api_key":"forbidden"}`,
		`{"instructions":"x","skills":[],"providers":[{"id":"p","name":"P","platform":"openai-chat","api_key":"forbidden"}]}`,
		`{"instructions":"x","skills":[],"providers":[{"id":"p","name":"P","platform":"openai-chat","base_url":"https://api.example.com/?key=forbidden"}]}`,
	}
	for _, v := range bad {
		_, _, _, err := n.applyContext(a, ea, 1, []byte(v))
		if err == nil {
			t.Fatal("invalid metadata allowed")
		}
	}
	data, err := os.ReadFile(n.sharedPath())
	if err != nil || strings.Contains(string(data), "forbidden") {
		t.Fatal("credential leaked to disk", err)
	}
}
func TestBrokenSharedConfigNotSilentlyReplaced(t *testing.T) {
	n := testNode(t)
	id := pairID(t, n)
	_, _, _, err := n.applyContext(id, n.beginClientConnection(id), 0, makeConfig(t, "valid"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(n.sharedPath(), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(n.Config); err == nil {
		t.Fatal("corrupt shared config silently reset")
	}
}
