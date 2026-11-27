package node

import (
	"context"
	"errors"

	"github.com/starfall-org/generative-server/internal/agent"
)

// Agent controls belong to paired clients and are intentionally excluded from
// builtinTools(): they are not available to a model as tool calls.
func (n *Node) agentAction(msg Message) (any, error) {
	mgr := n.Agents
	if mgr == nil {
		return nil, errors.New("agent runtime disabled; start generative with --enable-agent")
	}
	switch msg.Type {
	case "agent_open":
		if msg.Agent == nil {
			return nil, errors.New("agent spec required: {type: claude|codex|opencode, binary?: name|absolute path}")
		}
		result, err := mgr.Open(context.Background(), *msg.Agent)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_opened", "session": result}, nil
	case "agent_send":
		if err := mgr.Send(msg.AgentSessionID, msg.Prompt); err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_send_ack", "agent_session_id": msg.AgentSessionID}, nil
	case "agent_steer":
		if err := mgr.Steer(msg.AgentSessionID, msg.Prompt); err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_steer_ack", "agent_session_id": msg.AgentSessionID}, nil
	case "agent_respond":
		if err := mgr.Respond(msg.AgentSessionID, msg.ApprovalID, msg.Decision); err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_respond_ack", "agent_session_id": msg.AgentSessionID, "approval_id": msg.ApprovalID}, nil
	case "agent_interrupt":
		if err := mgr.Interrupt(msg.AgentSessionID); err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_interrupt_ack", "agent_session_id": msg.AgentSessionID}, nil
	case "agent_events":
		return mgr.Events(msg.AgentSessionID, msg.Since, msg.WaitMS)
	case "agent_status":
		result, err := mgr.Status(msg.AgentSessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_status", "session": result}, nil
	case "agent_list":
		return map[string]any{"type": "agent_list", "sessions": mgr.List(), "max_sessions": agent.MaxSessions}, nil
	case "agent_close":
		if err := mgr.Close(msg.AgentSessionID); err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_closed", "agent_session_id": msg.AgentSessionID}, nil
	default:
		return nil, errors.New("unknown agent action")
	}
}
func (n *Node) handleAgent(p *peer, msg Message) {
	result, err := n.agentAction(msg)
	if err != nil {
		p.error(err)
		return
	}
	_ = p.send(result)
}
