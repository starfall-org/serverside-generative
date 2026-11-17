package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
	"github.com/starfall-org/serverside-generative/internal/rpc"
)

func TestV5HTTPRevocationOnlyOwnIdentityAndInvalidatesBearer(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	inviteA, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	idA, secretA, err := n.pairWithTicket(inviteA.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	inviteB, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	idB, secretB, err := n.pairWithTicket(inviteB.Ticket, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	tokenFor := func(id, secret string) string {
		t.Helper()
		body, err := json.Marshal(Message{ClientID: id, Secret: secret})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(server.URL+"/v1/auth", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var auth struct {
			Token string `json:"token"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&auth) != nil || auth.Token == "" {
			t.Fatalf("authentication failed for %s: %d", id, resp.StatusCode)
		}
		return auth.Token
	}
	tokenA := tokenFor(idA, secretA)
	tokenB := tokenFor(idB, secretB)
	// Revocation does not erase this identity's durable server-side chat data.
	if reply := n.rpc.Dispatch(idA, rpc.Request{ID: "create", Op: "session.open", SessionID: "held-history", Data: json.RawMessage("{}")}); !reply.OK {
		t.Fatalf("could not create durable chat: %+v", reply)
	}
	revoke := func(token string, body []byte) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/revoke", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	// No client ID is accepted, including a valid other client's ID.
	if status := revoke(tokenA, []byte(`{"client_id":"`+idB+`"}`)); status != 400 {
		t.Fatalf("cross-client target was accepted: %d", status)
	}
	if !n.verifyPair(idA, secretA) || !n.verifyPair(idB, secretB) {
		t.Fatal("invalid revocation erased pairing state")
	}
	if status := revoke(tokenA, nil); status != 200 {
		t.Fatalf("self-revocation failed: %d", status)
	}
	if n.verifyPair(idA, secretA) {
		t.Fatal("revoked identity still authenticated")
	}
	n.mu.Lock()
	_, oldBearerPresent := n.tokens[sha256.Sum256([]byte(tokenA))]
	_, otherBearerPresent := n.tokens[sha256.Sum256([]byte(tokenB))]
	n.mu.Unlock()
	if oldBearerPresent || !otherBearerPresent {
		t.Fatal("revoke did not wipe only the revoked identity's bearer tokens")
	}
	if saved, err := chatstore.New(n.Config.DataDir).Get(idA, "held-history"); err != nil || saved.OwnerID != idA {
		t.Fatalf("revoke erased durable conversation: %+v %v", saved, err)
	}
	if status := revoke(tokenA, nil); status != 401 {
		t.Fatalf("revoked bearer should not authorize another request: %d", status)
	}
	if status := revoke("", nil); status != 401 {
		t.Fatalf("anonymous revocation status: %d", status)
	}
	if !n.verifyPair(idB, secretB) {
		t.Fatal("revoke affected another identity")
	}
	reply := n.rpc.Dispatch(idB, rpc.Request{ID: "cap", Op: "capabilities.get"})
	if !reply.OK {
		t.Fatalf("another identity lost RPC access: %+v", reply)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/rpc", bytes.NewBufferString(`{"id":"cap","op":"capabilities.get"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+tokenB)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("other bearer invalidated: %d", response.StatusCode)
	}
}
