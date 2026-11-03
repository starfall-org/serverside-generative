// Package agent exposes structured, directly controllable coding CLI sessions.
//
// Sessions survive client disconnects but not a server restart. The client calls
// these APIs itself; they are intentionally not model-invokable built-in tools.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	MaxSessions = 8
	maxEvents   = 2048
	maxBytes    = 2 << 20
	maxPrompt   = 64 << 10
)

type Type string

const (
	Claude   Type = "claude"
	Codex    Type = "codex"
	OpenCode Type = "opencode"
)

type Spec struct {
	Type   Type   `json:"type"`             // required; "claude", "codex", "opencode"
	Binary string `json:"binary,omitempty"` // optional name in PATH / absolute path
	CWD    string `json:"cwd,omitempty"`
}

type Descriptor struct {
	ID      string `json:"agent_session_id"`
	Type    Type   `json:"type"`
	Binary  string `json:"binary"`
	CWD     string `json:"cwd"`
	Running bool   `json:"running"`
	Busy    bool   `json:"busy"`
	LastSeq uint64 `json:"last_seq"`
}

type Event struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"agent_session_id"`
	Seq        uint64          `json:"seq"`
	Kind       string          `json:"kind"`
	BlockID    string          `json:"block_id,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ApprovalID string          `json:"approval_id,omitempty"`
	Content    string          `json:"content,omitempty"`
	Status     string          `json:"status,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

type Events struct {
	Type      string  `json:"type"`
	SessionID string  `json:"agent_session_id"`
	Events    []Event `json:"events"`
	LastSeq   uint64  `json:"last_seq"`
}

type backend interface {
	start(context.Context) error
	send(string) error
	steer(string) error
	respond(string, string) error
	interrupt() error
	close() error
}

type Session struct {
	mu      sync.Mutex
	closed  bool
	ready   bool
	alive   bool
	id      string
	kind    Type
	binary  string
	cwd     string
	busy    bool
	next    uint64
	floor   uint64
	events  []Event
	bytes   int
	updates chan struct{}
	impl    backend
}

type Manager struct {
	mu       sync.Mutex
	root     string
	sessions map[string]*Session
	closed   bool
}

func New(root string) (*Manager, error) {
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(canonical)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errors.New("agent root is not a directory")
	}
	return &Manager{root: canonical, sessions: make(map[string]*Session)}, nil
}

func resolveBinary(kind Type, requested string) (string, error) {
	switch kind {
	case Claude, Codex, OpenCode:
	default:
		return "", errors.New("unsupported agent type")
	}
	if requested == "" {
		requested = string(kind)
	}
	if strings.ContainsAny(requested, "\x00\r\n") || len(requested) > 4096 {
		return "", errors.New("invalid agent binary")
	}
	// Unqualified names are resolved via PATH; paths must be absolute. There
	// is never a shell and no caller-provided flags or arguments are accepted.
	if strings.ContainsAny(requested, `/\`) && !filepath.IsAbs(requested) {
		return "", errors.New("binary path must be absolute")
	}
	path, err := exec.LookPath(requested)
	if err != nil {
		return "", fmt.Errorf("agent binary unavailable: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return "", errors.New("agent binary is not executable")
	}
	return path, nil
}

func (m *Manager) resolveCWD(cwd string) (string, error) {
	if cwd == "" {
		return m.root, nil
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(m.root, cwd)
	}
	path, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(m.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("cwd is outside agent root")
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("agent cwd is not a directory")
	}
	return path, nil
}

func (m *Manager) Open(ctx context.Context, spec Spec) (Descriptor, error) {
	bin, err := resolveBinary(spec.Type, spec.Binary)
	if err != nil {
		return Descriptor{}, err
	}
	cwd, err := m.resolveCWD(spec.CWD)
	if err != nil {
		return Descriptor{}, err
	}
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return Descriptor{}, err
	}
	s := &Session{id: hex.EncodeToString(token), kind: spec.Type, binary: bin, cwd: cwd, updates: make(chan struct{}, 1), alive: true}
	// Construct the backend before publishing the session to other clients
	// or the daemon shutdown path.
	switch spec.Type {
	case Claude:
		s.impl = newClaude(s)
	case Codex:
		s.impl = newCodex(s)
	case OpenCode:
		s.impl = newOpenCode(s)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Descriptor{}, errors.New("agent manager closed")
	}
	if len(m.sessions) >= MaxSessions {
		m.mu.Unlock()
		return Descriptor{}, errors.New("agent session limit reached")
	}
	m.sessions[s.id] = s
	m.mu.Unlock()
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.impl.start(startCtx); err != nil {
		_ = m.Close(s.id)
		return Descriptor{}, err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	s.publish("status", "", "", "", "ready", "", nil)
	return s.descriptor(), nil
}

func (s *Session) descriptor() Descriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Descriptor{ID: s.id, Type: s.kind, Binary: s.binary, CWD: s.cwd, Running: s.alive && !s.closed, Busy: s.busy, LastSeq: s.next}
}

func (s *Session) publish(kind, block, toolID, approvalID, content, status string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.next++
	if len(raw) > maxBytes {
		raw = nil
	}
	ev := Event{Type: "agent_event", SessionID: s.id, Seq: s.next, Kind: kind, BlockID: block, ToolCallID: toolID, ApprovalID: approvalID, Content: content, Status: status, Raw: append(json.RawMessage(nil), raw...)}
	s.events = append(s.events, ev)
	s.bytes += len(content) + len(raw) + 160
	for len(s.events) > maxEvents || s.bytes > maxBytes {
		removed := s.events[0]
		s.bytes -= len(removed.Content) + len(removed.Raw) + 160
		s.floor = removed.Seq
		s.events = s.events[1:]
	}
	select {
	case s.updates <- struct{}{}:
	default:
	}
}

func (s *Session) mustAlive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.alive {
		return errors.New("agent process has exited")
	}
	return nil
}
func (s *Session) markExited() {
	s.mu.Lock()
	s.alive = false
	s.busy = false
	s.mu.Unlock()
}

func (s *Session) beginTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *Session) setBusy(busy bool) {
	s.mu.Lock()
	s.busy = busy
	s.mu.Unlock()
}

func (m *Manager) get(id string) (*Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, errors.New("agent session not found")
	}
	s.mu.Lock()
	closed := s.closed
	ready := s.ready
	s.mu.Unlock()
	if closed {
		return nil, errors.New("agent session closed")
	}
	if !ready {
		return nil, errors.New("agent session still initializing")
	}
	return s, nil
}

func validPrompt(prompt string) error {
	if len(prompt) == 0 || len(prompt) > maxPrompt {
		return errors.New("invalid prompt length")
	}
	return nil
}

func (m *Manager) Send(id, prompt string) error {
	if err := validPrompt(prompt); err != nil {
		return err
	}
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if err := s.mustAlive(); err != nil {
		return err
	}
	return s.impl.send(prompt)
}

func (m *Manager) Steer(id, prompt string) error {
	if err := validPrompt(prompt); err != nil {
		return err
	}
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if err := s.mustAlive(); err != nil {
		return err
	}
	return s.impl.steer(prompt)
}

func (m *Manager) Respond(id, approvalID, decision string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if approvalID == "" || len(approvalID) > 256 {
		return errors.New("invalid approval id")
	}
	switch decision {
	case "accept", "decline", "cancel", "acceptForSession", "once", "always", "reject":
	default:
		return errors.New("unsupported approval decision")
	}
	if err := s.mustAlive(); err != nil {
		return err
	}
	return s.impl.respond(approvalID, decision)
}

func (m *Manager) Interrupt(id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if err := s.mustAlive(); err != nil {
		return err
	}
	return s.impl.interrupt()
}

func (m *Manager) Status(id string) (Descriptor, error) {
	s, err := m.get(id)
	if err != nil {
		return Descriptor{}, err
	}
	return s.descriptor(), nil
}

func (m *Manager) List() []Descriptor {
	m.mu.Lock()
	copy := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		copy = append(copy, s)
	}
	m.mu.Unlock()
	out := make([]Descriptor, 0, len(copy))
	for _, s := range copy {
		out = append(out, s.descriptor())
	}
	return out
}

func (m *Manager) Events(id string, since uint64, waitMS int) (Events, error) {
	s, err := m.get(id)
	if err != nil {
		return Events{}, err
	}
	if waitMS < 0 || waitMS > 3000 {
		return Events{}, errors.New("invalid wait_ms")
	}
	snapshot := func() (Events, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if since < s.floor {
			return Events{}, fmt.Errorf("sync_gap: events through %d have been discarded", s.floor)
		}
		out := Events{Type: "agent_events", SessionID: id, LastSeq: s.next, Events: []Event{}}
		for _, e := range s.events {
			if e.Seq > since {
				out.Events = append(out.Events, e)
			}
		}
		return out, nil
	}
	result, err := snapshot()
	if err != nil || len(result.Events) > 0 || waitMS == 0 {
		return result, err
	}
	timer := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.updates:
	case <-timer.C:
	}
	return snapshot()
}

func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s != nil {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if s == nil {
		return errors.New("agent session not found")
	}
	// Cancel native process before dropping the in-memory state.
	var err error
	if s.impl != nil {
		err = s.impl.close()
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return err
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	m.closed = true
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Close(id)
	}
}
