package node

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/starfall-org/serverside-generative/internal/agent"
)

const maxFrame = 4 << 20

type Message struct {
	Type           string          `json:"type"`
	Ticket         string          `json:"ticket,omitempty"`
	PIN            string          `json:"pin,omitempty"`
	ClientID       string          `json:"client_id,omitempty"`
	Secret         string          `json:"secret,omitempty"`
	SessionID      string          `json:"session_id,omitempty"`
	Revision       uint64          `json:"revision,omitempty"`
	State          json.RawMessage `json:"state,omitempty"`
	Context        json.RawMessage `json:"context,omitempty"`
	Digest         string          `json:"digest,omitempty"`
	Since          uint64          `json:"since,omitempty"`
	Seq            uint64          `json:"seq,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	Kind           string          `json:"kind,omitempty"`
	Content        string          `json:"content,omitempty"`
	DelayMS        int             `json:"delay_ms,omitempty"`
	MCPServer      string          `json:"mcp_server,omitempty"`
	MCPTool        string          `json:"mcp_tool,omitempty"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	TerminalID     string          `json:"terminal_id,omitempty"`
	Command        string          `json:"command,omitempty"`
	Offset         uint64          `json:"offset,omitempty"`
	MaxBytes       int             `json:"max_bytes,omitempty"`
	WaitMS         int             `json:"wait_ms,omitempty"`
	Rows           int             `json:"rows,omitempty"`
	Cols           int             `json:"cols,omitempty"`
	Agent          *agent.Spec     `json:"agent,omitempty"`
	AgentSessionID string          `json:"agent_session_id,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	ApprovalID     string          `json:"approval_id,omitempty"`
	Decision       string          `json:"decision,omitempty"`
}
type peer struct {
	conn     net.Conn
	writes   sync.Mutex
	clientID string
	subs     []func()
}

func frameRead(r io.Reader, v any) error {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(head)
	if size == 0 || size > maxFrame {
		return fmt.Errorf("invalid frame length %d", size)
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
func (p *peer) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxFrame {
		return errors.New("response too large")
	}
	p.writes.Lock()
	defer p.writes.Unlock()
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(data)))
	if _, err = p.conn.Write(head[:]); err != nil {
		return err
	}
	_, err = p.conn.Write(data)
	return err
}
func (p *peer) error(err error) {
	_ = p.send(map[string]any{"type": "error", "message": err.Error()})
}
func (n *Node) ServeTCP(ctx context.Context, listener net.Listener) {
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{n.Certificate}, MinVersion: tls.VersionTLS13})
	go func() { <-ctx.Done(); _ = tlsListener.Close() }()
	for {
		conn, err := tlsListener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tcp accept: %v", err)
			continue
		}
		go n.handleTCP(conn)
	}
}
func (n *Node) handleTCP(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if t, ok := conn.(*tls.Conn); ok {
		if err := t.Handshake(); err != nil {
			return
		}
	}
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	if n.blocked("tcp:" + host) {
		return
	}
	p := &peer{conn: conn}
	var auth Message
	if err := frameRead(conn, &auth); err != nil {
		return
	}
	if auth.Type != "auth" {
		p.error(errors.New("auth required"))
		return
	}
	id, secret, newPair := n.authenticate(auth)
	if id == "" {
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		if n.limit("tcp:" + host) {
			p.error(errors.New("authentication failed"))
		}
		return
	}
	p.clientID = id
	_ = conn.SetDeadline(time.Time{})
	cachedRevision, cachedDigest, cachedSkills := n.contextSummary()
	reply := map[string]any{"type": "paired", "client_id": id, "protocol": ProtocolVersion,
		"shared_revision": cachedRevision, "context_digest": cachedDigest, "context_skills": cachedSkills}
	if newPair {
		reply["secret"] = secret
	}
	if err := p.send(reply); err != nil {
		return
	}
	epoch := n.beginClientConnection(id)
	// The client must first acknowledge the shared context or explicitly
	// fetch/update it. No chat/session traffic can run before this point.
	for {
		var initial Message
		if err := frameRead(conn, &initial); err != nil {
			return
		}
		if !n.connectionCurrent(id, epoch) {
			p.error(errors.New("connection superseded or revoked"))
			return
		}
		if initial.Type != "context_sync" && initial.Type != "context_get" && initial.Type != "context_update" {
			p.error(errors.New("context synchronization required before session traffic"))
			return
		}
		if err := n.handleContextRequest(p, epoch, initial); err != nil {
			p.error(err)
			continue
		}
		break
	}
	_ = conn.SetDeadline(time.Time{})
	defer func() {
		for _, cancel := range p.subs {
			cancel()
		}
	}()
	for {
		var msg Message
		if err := frameRead(conn, &msg); err != nil {
			return
		}
		if !n.connectionCurrent(id, epoch) {
			p.error(errors.New("connection superseded or pairing revoked"))
			return
		}
		switch msg.Type {
		case "context_sync", "context_get", "context_update":
			if err := n.handleContextRequest(p, epoch, msg); err != nil {
				p.error(err)
				continue
			}
		case "handoff":
			s, err := n.getSession(id, msg.SessionID, true)
			if err == nil {
				err = s.Handoff(msg.Revision, msg.State)
			}
			if err != nil {
				p.error(err)
				continue
			}
			_ = p.send(map[string]any{"type": "handoff_ack", "session_id": msg.SessionID, "revision": msg.Revision})
		case "start":
			s, err := n.getSession(id, msg.SessionID, false)
			if err == nil {
				err = s.Start(msg.RequestID, msg.Kind, msg.Content, msg.DelayMS, n.clientContext(id))
			}
			if err != nil {
				p.error(err)
				continue
			}
			_ = p.send(map[string]any{"type": "start_ack", "session_id": msg.SessionID, "request_id": msg.RequestID})
		case "mcp_list", "mcp_call":
			s, err := n.getSession(id, msg.SessionID, false)
			if err == nil {
				err = s.StartMCP(msg.RequestID, msg.MCPServer, msg.MCPTool, msg.Arguments, msg.Type == "mcp_list")
			}
			if err != nil {
				p.error(err)
				continue
			}
			_ = p.send(map[string]any{"type": "start_ack", "session_id": msg.SessionID, "request_id": msg.RequestID})
		case "sync":
			s, err := n.getSession(id, msg.SessionID, false)
			if err != nil {
				p.error(err)
				continue
			}
			notify, cancel := s.Subscribe()
			replay, err := s.EventsSince(msg.Since)
			if err != nil {
				cancel()
				p.error(err)
				continue
			}
			if err = p.send(map[string]any{"type": "sync_begin", "session_id": msg.SessionID, "count": len(replay)}); err != nil {
				cancel()
				return
			}
			last := msg.Since
			for _, event := range replay {
				if err = p.send(event); err != nil {
					cancel()
					return
				}
				last = event.Seq
			}
			if err = p.send(map[string]any{"type": "sync_end", "session_id": msg.SessionID, "last_seq": last}); err != nil {
				cancel()
				return
			}
			p.subs = append(p.subs, cancel)
			go func() {
				cursor := last
				for range notify {
					events, err := s.EventsSince(cursor)
					if err != nil {
						p.error(err)
						return
					}
					for _, e := range events {
						if err = p.send(e); err != nil {
							return
						}
						cursor = e.Seq
					}
				}
			}()
		case "ack":
			s, err := n.getSession(id, msg.SessionID, false)
			if err != nil {
				p.error(err)
				continue
			}
			s.Ack(msg.Seq)
			_ = p.send(map[string]any{"type": "ack_ok", "session_id": msg.SessionID, "seq": msg.Seq})
		case "status":
			s, err := n.getSession(id, msg.SessionID, false)
			if err != nil {
				p.error(err)
				continue
			}
			_ = p.send(s.Status())
		case "delete":
			if err := n.deleteSession(id, msg.SessionID); err != nil {
				p.error(err)
				continue
			}
			_ = p.send(map[string]any{"type": "deleted", "session_id": msg.SessionID})
		case "revoke":
			if err := n.revokePair(id); err != nil {
				p.error(err)
				continue
			}
			_ = p.send(map[string]any{"type": "revoked"})
			return
		case "tools_list":
			_ = p.send(map[string]any{"type": "tools_list", "tools": n.builtinTools()})
		case "terminal_create", "terminal_execute", "terminal_read", "terminal_close", "terminal_list":
			n.handleTerminal(p, msg)
		case "agent_open", "agent_send", "agent_steer", "agent_events", "agent_respond", "agent_interrupt", "agent_status", "agent_list", "agent_close":
			n.handleAgent(p, msg)
		case "ping":
			_ = p.send(map[string]any{"type": "pong"})
		default:
			p.error(fmt.Errorf("unknown message type: %s", msg.Type))
		}
	}
}
func (n *Node) handleContextRequest(p *peer, epoch uint64, msg Message) error {
	switch msg.Type {
	case "context_get":
		state := n.contextSnapshot()
		data, _ := json.Marshal(state.Context)
		return p.send(map[string]any{"type": "context_state", "revision": state.Revision, "digest": digestContext(data), "context": state.Context})
	case "context_sync":
		count, err := n.confirmContext(p.clientID, epoch, msg.Revision, msg.Digest)
		if err != nil {
			return err
		}
		return p.send(map[string]any{"type": "context_ack", "revision": msg.Revision, "digest": msg.Digest, "skills": count, "cached": true})
	case "context_update":
		revision, digest, count, err := n.applyContext(p.clientID, epoch, msg.Revision, msg.Context)
		if err != nil {
			return err
		}
		return p.send(map[string]any{"type": "context_ack", "revision": revision, "digest": digest, "skills": count, "cached": false})
	default:
		return errors.New("unknown context action")
	}
}

func (n *Node) authenticate(auth Message) (id, secret string, newPair bool) {
	if auth.ClientID != "" && auth.Secret != "" {
		if n.verifyPair(auth.ClientID, auth.Secret) {
			return auth.ClientID, "", false
		}
		return "", "", false
	}
	if auth.Ticket == "" && auth.PIN == "" {
		return "", "", false
	}
	id, secret, err := n.pairWithTicket(strings.TrimSpace(auth.Ticket), strings.TrimSpace(auth.PIN))
	if err != nil {
		return "", "", false
	}
	return id, secret, true
}
