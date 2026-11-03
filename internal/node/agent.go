package node

import (
	"context"
	"errors"

	"github.com/starfall-org/serverside-generative/internal/agent"
)

// Agent controls belong to paired clients and are intentionally excluded from
// builtinTools(): they are not available to a model as tool calls.
func (n *Node) handleAgent(p *peer, msg Message) {
	mgr := n.Agents
	if mgr == nil {
		p.error(errors.New("agent runtime disabled; start serverside with --enable-agent"))
		return
	}
	switch msg.Type {
	case "agent_open":
		if msg.Agent == nil {
			p.error(errors.New("agent spec required: {type: claude|codex|opencode, binary?: name|absolute path}"))
			return
		}
		d, err := mgr.Open(context.Background(), *msg.Agent)
		if err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_opened", "session": d})
	case "agent_send":
		if err := mgr.Send(msg.AgentSessionID, msg.Prompt); err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_send_ack", "agent_session_id": msg.AgentSessionID})
	case "agent_steer":
		if err := mgr.Steer(msg.AgentSessionID, msg.Prompt); err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_steer_ack", "agent_session_id": msg.AgentSessionID})
	case "agent_respond":
		if err := mgr.Respond(msg.AgentSessionID, msg.ApprovalID, msg.Decision); err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_respond_ack", "agent_session_id": msg.AgentSessionID, "approval_id": msg.ApprovalID})
	case "agent_interrupt":
		if err := mgr.Interrupt(msg.AgentSessionID); err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_interrupt_ack", "agent_session_id": msg.AgentSessionID})
	case "agent_events":
		result, err := mgr.Events(msg.AgentSessionID, msg.Since, msg.WaitMS)
		if err != nil {
			p.error(err)
			return
		}
		_ = p.send(result)
	case "agent_status":
		result, err := mgr.Status(msg.AgentSessionID)
		if err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_status", "session": result})
	case "agent_list":
		_ = p.send(map[string]any{"type": "agent_list", "sessions": mgr.List(), "max_sessions": agent.MaxSessions})
	case "agent_close":
		if err := mgr.Close(msg.AgentSessionID); err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "agent_closed", "agent_session_id": msg.AgentSessionID})
	}
}
