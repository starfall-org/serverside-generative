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
func (s *Session) Start(requestID, kind, content string, delayMS int) error {
	if err := validateID(requestID); err != nil {
		return err
	}
	if kind != "echo" {
		return errors.New("unsupported test runtime kind (only echo is implemented)")
	}
	if len(content) > 64*1024 {
		return errors.New("text too large")
	}
	if delayMS < 0 || delayMS > 30000 {
		return errors.New("invalid delay_ms")
	}
	s.mu.Lock()
	if s.Started[requestID] {
		s.mu.Unlock()
		return nil
	} // Idempotent execution.
	ctx, cancel := context.WithCancel(context.Background())
	s.Started[requestID] = true
	s.Jobs[requestID] = cancel
	s.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { s.mu.Lock(); delete(s.Jobs, requestID); s.mu.Unlock() }()
		if delayMS > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(delayMS) * time.Millisecond):
			}
		}
		runes := []rune(content)
		for len(runes) > 0 {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n := len(runes)
			if n > 12 {
				n = 12
			}
			s.Publish(requestID, "delta", string(runes[:n]))
			runes = runes[n:]
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
		s.Publish(requestID, "completed", "")
	}()
	return nil
}
func (s *Session) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join([]string{fmt.Sprint(s.Revision), fmt.Sprint(s.NextSeq)}, "/")
}
