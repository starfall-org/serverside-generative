// Package rpc contains the transport-independent v5 request dispatcher.
package rpc

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/starfall-org/serverside-generative/internal/catalog"

	"github.com/starfall-org/serverside-generative/internal/chatsession"
	"github.com/starfall-org/serverside-generative/internal/chatstore"
)

type Request struct {
	ID        string          `json:"id"`
	Op        string          `json:"op"`
	SessionID string          `json:"session_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type Reply struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}
type Dispatcher struct {
	sessions     *chatsession.Manager
	catalog      *catalog.Store
	native       func(string, Request) Reply
	capabilities func() any
}

func New(s *chatsession.Manager) *Dispatcher                        { return &Dispatcher{sessions: s} }
func (d *Dispatcher) SetCatalog(c *catalog.Store)                   { d.catalog = c }
func (d *Dispatcher) SetNative(handler func(string, Request) Reply) { d.native = handler }
func (d *Dispatcher) SetCapabilities(handler func() any)            { d.capabilities = handler }
func failure(id, code, message string) Reply {
	return Reply{ID: id, Error: &Error{Code: code, Message: message}}
}
func decode(raw json.RawMessage, out any) bool {
	return len(raw) > 0 && json.Unmarshal(raw, out) == nil
}

// Dispatch must only receive an owner that has already been authenticated by
// its transport; the request body cannot choose or override that owner.
func (d *Dispatcher) Dispatch(owner string, r Request) Reply {
	if owner == "" {
		return failure(r.ID, "unauthorized", "Authentication required")
	}
	if r.ID == "" || len(r.ID) > 128 || r.Op == "" {
		return failure(r.ID, "invalid_request", "Invalid request")
	}
	var result any
	var err error
	switch r.Op {
	case "capabilities.get":
		if d.capabilities == nil {
			return failure(r.ID, "unsupported_feature", "Capabilities unavailable")
		}
		result = d.capabilities()
	case "tool.list":
		var in struct {
			ServerID string `json:"server_id"`
		}
		if len(r.Data) > 0 && !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid tool request")
		}
		if in.ServerID != "" {
			var raw json.RawMessage
			raw, err = d.sessions.MCPTools(owner, r.SessionID, in.ServerID)
			if err == nil {
				result = map[string]any{"tools": raw}
			}
			break
		}
		names := []string{"generate_image", "generate_video", "generate_music", "send_file", "search_tools", "agent_skills_search", "create_skill", "activate_skill", "read_skill_resource"}
		tools := make([]map[string]any, 0, len(names))
		for _, name := range names {
			media := name == "generate_image" || name == "generate_video" || name == "generate_music" || name == "send_file"
			tools = append(tools, map[string]any{"name": name, "configuration_supported": false, "execution_supported": false, "requires_media_io": media})
		}
		if r.SessionID != "" {
			custom, e := d.sessions.CustomTools(owner, r.SessionID)
			if e != nil {
				err = e
			} else {
				tools = append(tools, custom...)
			}
		}
		result = map[string]any{"tools": tools}
	case "tool.call":
		var in struct {
			Kind      string          `json:"kind"`
			ToolID    string          `json:"tool_id"`
			ServerID  string          `json:"server_id"`
			ToolName  string          `json:"tool_name"`
			RequestID string          `json:"request_id"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid tool request")
		}
		if in.RequestID == "" {
			in.RequestID = r.ID
		}
		switch in.Kind {
		case "custom_http":
			err = d.sessions.StartCustomTool(owner, r.SessionID, in.RequestID, in.ToolID, in.Arguments)
		case "mcp":
			err = d.sessions.StartMCPTool(owner, r.SessionID, in.RequestID, in.ServerID, in.ToolName, in.Arguments)
		default:
			return failure(r.ID, "unsupported_feature", "Tool kind not implemented")
		}
		if err == nil {
			result = map[string]string{"request_id": in.RequestID}
		}

	case "config.get":
		if d.catalog == nil {
			return failure(r.ID, "unsupported_feature", "Catalog unavailable")
		}
		result, err = d.catalog.Get()
		if errors.Is(err, os.ErrNotExist) {
			return failure(r.ID, "not_found", "Catalog not initialized")
		}
	case "config.save":
		if d.catalog == nil {
			return failure(r.ID, "unsupported_feature", "Catalog unavailable")
		}
		var in struct {
			ExpectedRevision *uint64           `json:"expected_revision"`
			Files            map[string]string `json:"files"`
		}
		if !decode(r.Data, &in) || in.ExpectedRevision == nil {
			return failure(r.ID, "invalid_request", "Invalid catalog data")
		}
		result, err = d.catalog.Save(*in.ExpectedRevision, in.Files)

	case "session.open":
		// A client may have local sync preferences, but those are not
		// transmitted or interpreted by this RAM-only runtime.
		var in struct {
			After        uint64          `json:"after"`
			Conversation json.RawMessage `json:"conversation"`
			Runtime      json.RawMessage `json:"runtime"`
		}
		if !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid session data")
		}
		if string(in.Runtime) == "null" {
			in.Runtime = nil
		}
		result, err = d.sessions.Open(owner, r.SessionID, in.Conversation, in.Runtime)
	case "session.get":
		var in struct {
			IfRevision *uint64 `json:"if_revision"`
		}
		if len(r.Data) > 0 && !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid session data")
		}
		var v chatsession.View
		v, err = d.sessions.Get(owner, r.SessionID)
		if err == nil {
			if in.IfRevision != nil && *in.IfRevision == v.Revision {
				result = map[string]any{"session_id": r.SessionID, "revision": v.Revision, "unchanged": true}
			} else {
				result = v
			}
		}
	case "session.configure":
		var in struct {
			Runtime json.RawMessage `json:"runtime"`
		}
		if !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid runtime settings")
		}
		err = d.sessions.Configure(owner, r.SessionID, in.Runtime)
		if err == nil {
			result = map[string]string{"session_id": r.SessionID}
		}
	case "session.save":
		var in struct {
			Conversation     json.RawMessage `json:"conversation"`
			ExpectedRevision *uint64         `json:"expected_revision"`
			IdempotencyKey   string          `json:"idempotency_key"`
		}
		if !decode(r.Data, &in) || in.ExpectedRevision == nil || in.IdempotencyKey == "" {
			return failure(r.ID, "invalid_request", "Invalid save data")
		}
		result, err = d.sessions.Save(owner, r.SessionID, in.IdempotencyKey, *in.ExpectedRevision, in.Conversation)
	case "session.run":
		var in struct {
			RequestID string `json:"request_id"`
			Content   string `json:"content"`
		}
		if !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid run data")
		}
		if in.RequestID == "" {
			in.RequestID = r.ID
		}
		err = d.sessions.Run(owner, r.SessionID, in.RequestID, in.Content)
		if err == nil {
			result = map[string]string{"request_id": in.RequestID}
		}
	case "session.cancel":
		var in struct {
			RequestID string `json:"request_id"`
		}
		if !decode(r.Data, &in) || in.RequestID == "" {
			return failure(r.ID, "invalid_request", "Request ID required")
		}
		err = d.sessions.Cancel(owner, r.SessionID, in.RequestID)
		if err == nil {
			result = map[string]string{"request_id": in.RequestID}
		}

	case "session.events":
		var in struct {
			After uint64 `json:"after"`
		}
		if len(r.Data) > 0 && !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid event data")
		}
		result, err = d.sessions.Events(owner, r.SessionID, in.After)

	case "session.list":
		var in struct {
			Offset int `json:"offset"`
			Limit  int `json:"limit"`
		}
		in.Limit = 50
		if len(r.Data) > 0 && !decode(r.Data, &in) {
			return failure(r.ID, "invalid_request", "Invalid list data")
		}
		var items []chatsession.ListItem
		var next int
		items, next, err = d.sessions.List(owner, in.Offset, in.Limit)
		if err == nil {
			result = map[string]any{"items": items, "next_offset": next}
		}
	case "session.delete":
		var in struct {
			ExpectedRevision *uint64 `json:"expected_revision"`
			IdempotencyKey   string  `json:"idempotency_key"`
		}
		if !decode(r.Data, &in) || in.IdempotencyKey == "" {
			return failure(r.ID, "invalid_request", "Invalid delete data")
		}
		err = d.sessions.Delete(owner, r.SessionID, in.IdempotencyKey, in.ExpectedRevision)
		if err == nil {
			result = map[string]string{"session_id": r.SessionID}
		}

	case "session.close":
		err = d.sessions.Close(owner, r.SessionID)
		if err == nil {
			result = map[string]string{"session_id": r.SessionID}
		}
	default:
		if d.native != nil && (strings.HasPrefix(r.Op, "terminal.") || strings.HasPrefix(r.Op, "agent.")) {
			return d.native(owner, r)
		}
		return failure(r.ID, "unsupported_feature", "Operation not implemented")
	}
	if err != nil {
		switch {
		case errors.Is(err, chatsession.ErrSecretLeak):
			return failure(r.ID, "secret_leak_blocked", "Tool output contained runtime credentials")
		case errors.Is(err, chatsession.ErrDenied):
			return failure(r.ID, "permission_denied", "Tool access denied")
		case errors.Is(err, chatsession.ErrAuthRequired):
			return failure(r.ID, "auth_required", "A valid mobile token is required")
		case errors.Is(err, chatsession.ErrRuntime):
			return failure(r.ID, "missing_runtime_config", "Runtime configuration required")
		case errors.Is(err, chatsession.ErrProvider):
			return failure(r.ID, "unsupported_provider_adapter", "Provider adapter is not implemented")
		case errors.Is(err, chatsession.ErrUnsupported):
			return failure(r.ID, "unsupported_feature", "Runtime feature is not supported")
		case errors.Is(err, chatsession.ErrReplay):
			return failure(r.ID, "request_already_recorded", "Request already recorded; inspect session")
		case errors.Is(err, chatsession.ErrBusy):
			return failure(r.ID, "session_busy", "Session is busy")
		case errors.Is(err, chatsession.ErrGap):
			return failure(r.ID, "event_gap", "Event history is no longer available")
		case errors.Is(err, chatsession.ErrNotFound):
			return failure(r.ID, "not_found", "Session not found")
		case errors.Is(err, chatstore.ErrDeleted):
			return failure(r.ID, "session_deleted", "Session was deleted")
		case errors.Is(err, chatstore.ErrConflict), errors.Is(err, catalog.ErrConflict):
			return failure(r.ID, "revision_conflict", "Session changed")
		case errors.Is(err, chatsession.ErrInvalid), errors.Is(err, chatstore.ErrInvalid), errors.Is(err, catalog.ErrInvalid):
			return failure(r.ID, "invalid_request", "Invalid session data")
		case errors.Is(err, chatsession.ErrLimit):
			return failure(r.ID, "resource_exhausted", "Session limit reached")
		default:
			return failure(r.ID, "internal_error", "Session operation failed")
		}
	}
	return Reply{ID: r.ID, OK: true, Data: result}
}

func (d *Dispatcher) Events(owner, id string, after uint64) ([]chatsession.Event, error) {
	return d.sessions.Events(owner, id, after)
}
func (d *Dispatcher) Subscribe(owner, id string) (<-chan struct{}, func(), error) {
	return d.sessions.Subscribe(owner, id)
}

func (d *Dispatcher) Revoke(owner string) { d.sessions.Revoke(owner) }
