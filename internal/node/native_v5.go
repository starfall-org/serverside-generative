package node

import (
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/starfall-org/generative-server/internal/rpc"
)

type nativeReceipt struct {
	hash  [32]byte
	reply rpc.Reply
	ready chan struct{}
}

func (n *Node) nativeRPC(owner string, req rpc.Request) rpc.Reply {
	fail := func(code, message string) rpc.Reply {
		return rpc.Reply{ID: req.ID, Error: &rpc.Error{Code: code, Message: message}}
	}
	if (strings.HasPrefix(req.Op, "terminal.") && n.Terminals == nil) || (strings.HasPrefix(req.Op, "agent.") && n.Agents == nil) {
		return fail("forbidden", "Native capability is disabled")
	}
	var msg Message
	if len(req.Data) > 0 && json.Unmarshal(req.Data, &msg) != nil {
		return fail("invalid_request", "Invalid native request")
	}
	msg.Type = strings.Replace(req.Op, ".", "_", 1)
	allowed := map[string]bool{"terminal_create": true, "terminal_execute": true, "terminal_read": true, "terminal_close": true, "terminal_list": true, "agent_open": true, "agent_send": true, "agent_steer": true, "agent_respond": true, "agent_interrupt": true, "agent_events": true, "agent_status": true, "agent_list": true, "agent_close": true}
	if !allowed[msg.Type] {
		return fail("unsupported_feature", "Operation not implemented")
	}
	execute := func() rpc.Reply {
		var data any
		var err error
		if strings.HasPrefix(msg.Type, "terminal_") {
			data, err = n.terminalAction(msg)
		} else {
			data, err = n.agentAction(msg)
		}
		if err != nil {
			return fail("operation_failed", "Native operation failed")
		}
		return rpc.Reply{ID: req.ID, OK: true, Data: data}
	}
	// Read-only operations do not need side-effect receipts and can return
	// large event pages; avoid retaining their output in memory.
	readonly := msg.Type == "terminal_read" || msg.Type == "terminal_list" || msg.Type == "agent_events" || msg.Type == "agent_status" || msg.Type == "agent_list"
	if readonly {
		if !n.paired(owner) {
			return fail("unauthorized", "Authentication required")
		}
		return execute()
	}
	raw, _ := json.Marshal(req)
	hash := sha256.Sum256(raw)
	key := owner + "\x00" + req.ID
	n.mu.Lock()
	if _, ok := n.pairs[owner]; !ok {
		n.mu.Unlock()
		return fail("unauthorized", "Authentication required")
	}

	if old := n.nativeReceipts[key]; old != nil {
		if old.hash != hash {
			n.mu.Unlock()
			return fail("idempotency_conflict", "Request ID was already used")
		}
		ready := old.ready
		n.mu.Unlock()
		<-ready
		return old.reply
	}
	if len(n.nativeReceipts) >= 1024 {
		n.mu.Unlock()
		return fail("resource_exhausted", "Too many requests")
	}
	receipt := &nativeReceipt{hash: hash, ready: make(chan struct{})}
	n.nativeReceipts[key] = receipt
	n.mu.Unlock()
	reply := execute()
	n.mu.Lock()
	receipt.reply = reply
	close(receipt.ready)
	n.mu.Unlock()
	return reply
}
