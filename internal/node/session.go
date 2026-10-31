package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/starfall-org/serverside-generative/internal/execution"
)

const maxEvents = 2048
const maxSessionBytes = 2 << 20

type Event struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
	RequestID string `json:"request_id,omitempty"`
	Event     string `json:"event"`
	Content   string `json:"content,omitempty"`
}
type Session struct {
	mu          sync.Mutex
	ID          string
	Owner       string
	Revision    uint64
	State       json.RawMessage
	NextSeq     uint64
	Floor       uint64 // highest contiguous event no longer retained
	Events      []Event
	Started     map[string]bool
	Jobs        map[string]context.CancelFunc
	Subscribers map[chan struct{}]struct{}
}

func validateID(id string) error {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) {
		return errors.New("invalid identifier")
	}
	for _, r := range id {
		if r < 32 {
			return errors.New("control characters in identifier")
		}
	}
	return nil
}
func (n *Node) getSession(owner, id string, create bool) (*Session, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	key := owner + "\x00" + id
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.sessions[key]
	if s == nil && create {
		if len(n.sessions) >= 128 {
			return nil, errors.New("session limit reached")
		}
		s = &Session{ID: id, Owner: owner, Started: make(map[string]bool), Jobs: make(map[string]context.CancelFunc), Subscribers: make(map[chan struct{}]struct{})}
		n.sessions[key] = s
	}
	if s == nil {
		return nil, errors.New("session not found (server state is memory-only)")
	}
	return s, nil
}
func (n *Node) deleteSession(owner, id string) error {
	s, err := n.getSession(owner, id, false)
	if err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.sessions, owner+"\x00"+id)
	n.mu.Unlock()
	s.mu.Lock()
	for _, cancel := range s.Jobs {
		cancel()
	}
	s.mu.Unlock()
	return nil
}
func (s *Session) Handoff(revision uint64, data json.RawMessage) error {
	if len(data) == 0 || string(data) == "null" {
		return errors.New("missing state")
	}
	if len(data) > maxSessionBytes {
		return errors.New("state too large")
	}
	if !json.Valid(data) {
		return errors.New("invalid state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision <= s.Revision {
		return fmt.Errorf("stale revision: current=%d", s.Revision)
	}
	s.State = append(json.RawMessage(nil), data...)
	s.Revision = revision
	return nil
}
func (s *Session) Publish(requestID, event, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NextSeq++
	e := Event{Type: "event", SessionID: s.ID, Seq: s.NextSeq, RequestID: requestID, Event: event, Content: content}
	s.Events = append(s.Events, e)
	if len(s.Events) > maxEvents {
		removed := len(s.Events) - maxEvents
		s.Floor = s.Events[removed-1].Seq
		s.Events = append([]Event(nil), s.Events[removed:]...)
	}
	for watcher := range s.Subscribers {
		select {
		case watcher <- struct{}{}:
		default:
		}
	}
}
func (s *Session) EventsSince(seq uint64) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < s.Floor {
		return nil, fmt.Errorf("sync_gap: events through seq %d are no longer retained", s.Floor)
	}
	if len(s.Events) > 0 && s.Events[0].Seq > 0 && seq < s.Events[0].Seq-1 {
		return nil, fmt.Errorf("sync_gap: oldest available seq %d", s.Events[0].Seq)
	}
	var result []Event
	for _, event := range s.Events {
		if event.Seq > seq {
			result = append(result, event)
		}
	}
	return result, nil
}
func (s *Session) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.Subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() { s.mu.Lock(); delete(s.Subscribers, ch); close(ch); s.mu.Unlock() }
}
func (s *Session) Ack(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.NextSeq {
		seq = s.NextSeq
	}
	i := 0
	for i < len(s.Events) && s.Events[i].Seq <= seq {
		i++
	}
	if i > 0 {
		s.Events = append([]Event(nil), s.Events[i:]...)
	}
	if seq > s.Floor {
		s.Floor = seq
	}
}
func (s *Session) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"type": "status", "session_id": s.ID, "revision": s.Revision, "last_seq": s.NextSeq, "pending_events": len(s.Events), "active_jobs": len(s.Jobs)}
}
func (s *Session) BeginJob(requestID string, configCheck func(json.RawMessage) error) (context.Context, json.RawMessage, bool, error) {
	if err := validateID(requestID); err != nil {
		return nil, nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Started[requestID] {
		return nil, nil, true, nil
	}
	state := append(json.RawMessage(nil), s.State...)
	if configCheck != nil {
		if err := configCheck(state); err != nil {
			return nil, nil, false, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Started[requestID] = true
	s.Jobs[requestID] = cancel
	return ctx, state, false, nil
}
func (s *Session) FinishJob(requestID string, err error) {
	if err != nil {
		s.Publish(requestID, "error", "remote_execution_failed")
	} else {
		s.Publish(requestID, "completed", "")
	}
	s.mu.Lock()
	if cancel, ok := s.Jobs[requestID]; ok {
		cancel()
		delete(s.Jobs, requestID)
	}
	s.mu.Unlock()
}
func (s *Session) Start(requestID, kind, content string, delayMS int, clientCtx ...execution.ClientContext) error {
	if kind != "echo" && kind != "model" {
		return errors.New("unsupported execution kind")
	}
	if len(content) > 64*1024 {
		return errors.New("text too large")
	}
	if delayMS < 0 || delayMS > 30000 {
		return errors.New("invalid delay_ms")
	}
	var model execution.State
	check := func(raw json.RawMessage) error {
		if kind == "echo" {
			return nil
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return errors.New("missing or invalid model state")
		}
		if len(clientCtx) > 0 {
			var err error
			model, err = model.WithClientContext(clientCtx[0])
			if err != nil {
				return err
			}
		} else if len(model.SelectedSkillIDs) > 0 {
			return errors.New("skills unavailable: client context was not synchronized")
		}
		return model.ValidateModel()
	}
	ctx, _, duplicate, err := s.BeginJob(requestID, check)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	go func() {
		var jobErr error
		defer func() { s.FinishJob(requestID, jobErr) }()
		if kind == "model" {
			jobErr = execution.Stream(ctx, model, func(delta string) {
				if ctx.Err() != nil {
					return
				}
				// Preserve stream events for reconnect, bounded by the event buffer.
				for len(delta) > 0 {
					n := len(delta)
					if n > 12*1024 {
						n = 12 * 1024
					}
					// Avoid breaking UTF-8 if a provider emits a large chunk.
					for n < len(delta) && !utf8.ValidString(delta[:n]) {
						n++
					}
					s.Publish(requestID, "delta", delta[:n])
					delta = delta[n:]
				}
			})
			return
		}
		if delayMS > 0 {
			select {
			case <-ctx.Done():
				jobErr = ctx.Err()
				return
			case <-time.After(time.Duration(delayMS) * time.Millisecond):
			}
		}
		runes := []rune(content)
		for len(runes) > 0 {
			if ctx.Err() != nil {
				jobErr = ctx.Err()
				return
			}
			n := len(runes)
			if n > 12 {
				n = 12
			}
			s.Publish(requestID, "delta", string(runes[:n]))
			runes = runes[n:]
			select {
			case <-ctx.Done():
				jobErr = ctx.Err()
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
	}()
	return nil
}
func (s *Session) StartMCP(requestID, serverID, toolName string, args json.RawMessage, list bool) error {
	var state execution.State
	check := func(raw json.RawMessage) error {
		if err := json.Unmarshal(raw, &state); err != nil {
			return errors.New("missing or invalid MCP state")
		}
		return state.ValidateMCP(serverID)
	}
	ctx, _, duplicate, err := s.BeginJob(requestID, check)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	go func() {
		var runErr error
		defer func() { s.FinishJob(requestID, runErr) }()
		var result json.RawMessage
		if list {
			result, runErr = execution.ListTools(ctx, state, serverID)
		}
		if !list {
			result, runErr = execution.CallTool(ctx, state, serverID, toolName, args)
		}
		if runErr == nil {
			if list {
				s.Publish(requestID, "mcp_tools", string(result))
			} else {
				s.Publish(requestID, "mcp_result", string(result))
			}
		}
	}()
	return nil
}
func (s *Session) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join([]string{fmt.Sprint(s.Revision), fmt.Sprint(s.NextSeq)}, "/")
}
