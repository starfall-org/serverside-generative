package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTCPEnrollmentIsSSHOnlyByDefault(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data", Password: "a-long-legacy-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/handshake", "application/json", bytes.NewReader([]byte(`{"password":"a-long-legacy-password"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("legacy HTTP enrollment not disabled: %d", resp.StatusCode)
	}
	resp, err = http.Get(server.URL + "/v1/handshake/ws")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("legacy WS enrollment not disabled: %d", resp.StatusCode)
	}

	// The old TCP v4 format must still authenticate an existing identity, but
	// cannot enroll a NEW device with a ticket when legacy mode is disabled.
	inv, err := n.IssueTicket()
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
	tlsConf := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", listener.Addr().String(), tlsConf)
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{conn: conn}
	if err = p.send(Message{Type: "auth", Ticket: inv.Ticket, Protocol: 4}); err != nil {
		t.Fatal(err)
	}
	var answer map[string]json.RawMessage
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	readErr := frameRead(conn, &answer)
	conn.Close()
	if readErr == nil && string(answer["type"]) == `"paired"` {
		t.Fatal("default TCP v4 accepted new ticket pairing")
	}

	// SSH-authorized identity works for both the modern and existing TLS/TCP
	// client paths. Scope remains stable across process restart.
	tcpPair, err := n.pairTCP()
	if err != nil {
		t.Fatal(err)
	}
	id := tcpPair["client_id"].(string)
	secret := tcpPair["secret"].(string)
	if !n.verifyPairFor(id, secret, "tcp") || n.verifyPairFor(id, secret, "http") {
		t.Fatal("SSH-issued identity crossed transport group")
	}
	n2, err := New(Config{IdentityDir: n.Config.IdentityDir, DataDir: t.TempDir() + "/private-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	if !n2.verifyPairFor(id, secret, "tcp") {
		t.Fatal("SSH-issued pair lost after restart")
	}
}

func TestLegacyPairingNeedsExplicitOptIn(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data", LegacyPairing: true, Password: "a-long-legacy-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/handshake", "application/json", bytes.NewReader([]byte(`{"password":"a-long-legacy-password"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("explicit legacy gate did not work: %d", resp.StatusCode)
	}
}
