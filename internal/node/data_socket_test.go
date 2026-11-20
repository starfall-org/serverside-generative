//go:build linux

package node

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/starfall-org/serverside-generative/internal/rpc"
)

func TestDataSocketPairAndRPC(t *testing.T) {
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: t.TempDir() + "/private-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	l, err := StartDataSocket(n.Config.DataSocket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.ServeDataSocket(ctx, l)
	defer l.Close()
	info, err := os.Stat(n.Config.DataSocket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", info, err)
	}
	if _, err := StartDataSocket(n.Config.DataSocket); err == nil {
		t.Fatal("accepted active socket")
	}
	invite, err := n.IssueTicket()
	if err != nil {
		t.Fatal(err)
	}
	bad, err := DialDataSocket(n.Config.DataSocket)
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.SetDeadline(time.Now().Add(5 * time.Second))
	if err = (&peer{conn: bad}).send(json.RawMessage(`{"type":"auth","protocol":4,"protocol":5}`)); err != nil {
		t.Fatal(err)
	}
	var denied map[string]any
	if err = frameRead(bad, &denied); err == nil {
		t.Fatal("duplicate authentication fields accepted")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("duplicate authentication was not rejected promptly")
	}
	_ = bad.Close()
	conn, err := DialDataSocket(n.Config.DataSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	p := peer{conn: conn}
	if err = p.send(Message{Type: "auth", Protocol: 5, Ticket: invite.Ticket}); err != nil {
		t.Fatal(err)
	}
	var paired map[string]any
	if err = frameRead(conn, &paired); err != nil || paired["secret"] == "" || paired["protocol"] != float64(5) {
		t.Fatalf("pair: %v %v", paired, err)
	}
	if n.verifyPairFor(paired["client_id"].(string), paired["secret"].(string), "http") {
		t.Fatal("SSH-paired identity accepted for HTTP")
	}
	if err = p.send(rpc.Request{ID: "capabilities", Op: "capabilities.get"}); err != nil {
		t.Fatal(err)
	}
	var capabilities rpc.Reply
	if err = frameRead(conn, &capabilities); err != nil || !capabilities.OK {
		t.Fatalf("capabilities: %+v %v", capabilities, err)
	}
	encoded, _ := json.Marshal(capabilities.Data)
	if !strings.Contains(string(encoded), "ssh_stdio") {
		t.Fatal("active SSH bridge not advertised")
	}
	server := httptest.NewServer(n.HTTPHandler())
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var httpCapabilities any
	err = json.NewDecoder(response.Body).Decode(&httpCapabilities)
	_ = response.Body.Close()
	if err != nil || !reflect.DeepEqual(httpCapabilities, capabilities.Data) {
		t.Fatalf("capabilities differ by transport: %v", err)
	}
	open := rpc.Request{ID: "open", Op: "session.open", SessionID: "ssh", Data: json.RawMessage(`{"storage":"memory","conversation":{"id":"ssh","title":"t","created_at":1,"updated_at":2,"messages":[]}}`)}
	if err = p.send(open); err != nil {
		t.Fatal(err)
	}
	var reply rpc.Reply
	if err = frameRead(conn, &reply); err != nil || !reply.OK {
		t.Fatalf("open: %+v detail=%+v err=%v", reply, reply.Error, err)
	}
	if err = p.send(rpc.Request{ID: "get", Op: "session.get", SessionID: "ssh"}); err != nil {
		t.Fatal(err)
	}
	if err = frameRead(conn, &reply); err != nil || !reply.OK {
		t.Fatalf("get: %+v %v", reply, err)
	}
	// Android SSH uses cursor polling. Mutations must not inject unsolicited
	// event frames into its RPC reply stream.
	save := rpc.Request{ID: "save", Op: "session.save", SessionID: "ssh", Data: json.RawMessage(`{"expected_revision":1,"idempotency_key":"edit","conversation":{"id":"ssh","title":"edited","created_at":1,"updated_at":2,"messages":[]}}`)}
	if err = p.send(save); err != nil {
		t.Fatal(err)
	}
	reply = rpc.Reply{}
	if err = frameRead(conn, &reply); err != nil || !reply.OK || reply.ID != "save" {
		t.Fatalf("save reply: %+v %v", reply, err)
	}
	if err = p.send(rpc.Request{ID: "poll", Op: "session.events", SessionID: "ssh", Data: json.RawMessage(`{"after":0}`)}); err != nil {
		t.Fatal(err)
	}
	reply = rpc.Reply{}
	if err = frameRead(conn, &reply); err != nil || !reply.OK || reply.ID != "poll" {
		t.Fatalf("poll reply: %+v %v", reply, err)
	}
	events, ok := reply.Data.([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("missing polled event: %+v", reply.Data)
	}
	cancel()
	if err = frameRead(conn, &reply); err == nil {
		t.Fatal("connection survived data socket shutdown")
	}
}

func TestDataSocketRejectsSymlinkAndUnsafeModes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := StartDataSocket(filepath.Join(link, "created", "data.sock")); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "created")); !os.IsNotExist(err) {
		t.Fatal("created through symlink")
	}
	dir := filepath.Join(root, "public")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := StartDataSocket(filepath.Join(dir, "data.sock")); err == nil {
		t.Fatal("accepted public directory")
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "data.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := DialDataSocket(path); err == nil {
		t.Fatal("accepted public socket")
	}
}
