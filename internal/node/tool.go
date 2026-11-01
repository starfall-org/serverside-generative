package node

import (
	"errors"

	"github.com/starfall-org/serverside-generative/internal/terminal"
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
func (n *Node) handleTerminal(p *peer, msg Message) {
	mgr := n.Terminals
	if mgr == nil {
		p.error(errors.New("terminal tools disabled; start server with --enable-terminal"))
		return
	}
	switch msg.Type {
	case "terminal_create":
		id, err := mgr.Create(msg.Rows, msg.Cols)
		if err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "terminal_created", "terminal_id": id})
	case "terminal_execute":
		result, err := mgr.Execute(msg.TerminalID, msg.Command, msg.WaitMS)
		if err != nil {
			p.error(err)
			return
		}
		_ = p.send(result)
	case "terminal_read":
		result, err := mgr.Read(msg.TerminalID, msg.Offset, msg.MaxBytes, msg.WaitMS)
		if err != nil {
			p.error(err)
			return
		}
		_ = p.send(result)
	case "terminal_close":
		if err := mgr.Close(msg.TerminalID); err != nil {
			p.error(err)
			return
		}
		_ = p.send(map[string]any{"type": "terminal_closed", "terminal_id": msg.TerminalID})
	case "terminal_list":
		_ = p.send(map[string]any{"type": "terminal_list", "terminals": mgr.List(), "max_terminals": terminal.MaxTerminals})
	}
}
