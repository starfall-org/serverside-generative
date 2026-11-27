package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/starfall-org/generative-server/internal/rpc"
)

// StartDataSocket creates a private Unix socket for framed v5 traffic.
func StartDataSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := ensureDataSocketDir(dir); err != nil {
		return nil, err
	}
	if err := validateDataSocketDir(dir); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("data socket path is not a socket")
		}
		if err = validateDataSocketOwner(info); err != nil {
			return nil, err
		}
		c, e := net.DialTimeout("unix", path, 250*time.Millisecond)
		if e == nil {
			c.Close()
			return nil, errors.New("data socket already in use")
		}
		if !isStaleSocketError(e) {
			return nil, errors.New("cannot verify existing data socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
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
func (n *Node) ServeDataSocket(ctx context.Context, listener net.Listener) {
	var mu sync.Mutex
	connections := make(map[net.Conn]struct{})
	done := make(chan struct{})
	defer func() {
		close(done)
		mu.Lock()
		for conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !authorizedLocalPeer(conn) {
			conn.Close()
			continue
		}
		mu.Lock()
		connections[conn] = struct{}{}
		mu.Unlock()
		go func() {
			defer func() {
				mu.Lock()
				delete(connections, conn)
				mu.Unlock()
			}()
			n.handleLocalV5(conn)
		}()
	}
}
func (n *Node) handleLocalV5(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	p := &peer{conn: conn}
	var raw json.RawMessage
	if frameRead(conn, &raw) != nil || len(raw) > 4096 {
		return
	}
	if _, err := rpc.Decode(raw); err != nil {
		return
	}
	var auth Message
	if json.Unmarshal(raw, &auth) != nil {
		return
	}
	if auth.Type != "auth" || auth.Protocol != 5 {
		p.error(errors.New("v5 auth required"))
		return
	}
	if n.blocked("data:local") {
		return
	}
	var id, secret string
	newPair := false
	if auth.ClientID != "" && auth.Secret != "" && auth.Ticket == "" && auth.PIN == "" && n.verifyPairFor(auth.ClientID, auth.Secret, "tcp") {
		id = auth.ClientID
	} else if auth.ClientID == "" && auth.Secret == "" && auth.Ticket != "" && auth.PIN == "" {
		id, secret, _ = n.pairWithTicketScope(strings.TrimSpace(auth.Ticket), "", "tcp")
		newPair = id != ""
	}
	if id == "" {
		if n.limit("data:local") {
			p.error(errors.New("authentication failed"))
		}
		return
	}
	p.clientID = id
	_ = conn.SetDeadline(time.Time{})
	p.writeTimeout = 10 * time.Second
	response := map[string]any{"type": "paired", "client_id": id, "protocol": 5}
	if newPair {
		response["secret"] = secret
	}
	if p.send(response) == nil {
		n.serveRPCPeer(p, true)
	}
}

// DialDataSocket refuses symlinks, nonprivate socket paths and other local users.
func DialDataSocket(path string) (net.Conn, error) {
	return DialDataSocketContext(context.Background(), path)
}

func DialDataSocketContext(ctx context.Context, path string) (net.Conn, error) {
	if err := validateDataSocketDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe data socket")
	}
	if err = validateDataSocketOwner(info); err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	c, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if !authorizedLocalPeer(c) {
		c.Close()
		return nil, errors.New("data socket peer mismatch")
	}
	return c, nil
}
