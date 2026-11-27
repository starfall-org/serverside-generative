package node

import (
	"errors"

	"github.com/starfall-org/generative-server/internal/terminal"
)

type BuiltinTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	Enabled     bool           `json:"enabled"`
}

func (n *Node) builtinTools() []BuiltinTool {
	enabled := n.Terminals != nil
	return []BuiltinTool{
		{Name: "terminal_create", Description: "Open a new interactive terminal session on the self-hosted server.", Enabled: enabled,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"rows": map[string]any{"type": "integer", "minimum": 10, "maximum": 200}, "cols": map[string]any{"type": "integer", "minimum": 20, "maximum": 400}}}},
		{Name: "terminal_execute", Description: "Send a shell command or multiline snippet to a terminal. It continues after the client disconnects.", Enabled: enabled,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"terminal_id": map[string]any{"type": "string"}, "command": map[string]any{"type": "string"}, "wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 3000}}, "required": []string{"terminal_id", "command"}}},
		{Name: "terminal_read", Description: "Read pending terminal stdout/stderr/PTY bytes using a bounded byte offset.", Enabled: enabled,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"terminal_id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0}, "wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 3000}}, "required": []string{"terminal_id"}}},
		{Name: "terminal_close", Description: "Terminate the terminal shell and its child process group.", Enabled: enabled,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"terminal_id": map[string]any{"type": "string"}}, "required": []string{"terminal_id"}}},
		{Name: "terminal_list", Description: "List active terminals available to any paired client on this personal server.", Enabled: enabled,
			InputSchema: map[string]any{"type": "object"}},
	}
}
func (n *Node) terminalAction(msg Message) (any, error) {
	mgr := n.Terminals
	if mgr == nil {
		return nil, errors.New("terminal tools disabled; start server with --enable-terminal")
	}
	switch msg.Type {
	case "terminal_create":
		id, err := mgr.Create(msg.Rows, msg.Cols)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "terminal_created", "terminal_id": id}, nil
	case "terminal_execute":
		return mgr.Execute(msg.TerminalID, msg.Command, msg.WaitMS)
	case "terminal_read":
		return mgr.Read(msg.TerminalID, msg.Offset, msg.MaxBytes, msg.WaitMS)
	case "terminal_close":
		if err := mgr.Close(msg.TerminalID); err != nil {
			return nil, err
		}
		return map[string]any{"type": "terminal_closed", "terminal_id": msg.TerminalID}, nil
	case "terminal_list":
		return map[string]any{"type": "terminal_list", "terminals": mgr.List(), "max_terminals": terminal.MaxTerminals}, nil
	default:
		return nil, errors.New("unknown terminal action")
	}
}
func (n *Node) handleTerminal(p *peer, msg Message) {
	result, err := n.terminalAction(msg)
	if err != nil {
		p.error(err)
		return
	}
	_ = p.send(result)
}
