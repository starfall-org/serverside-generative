// Package chatsession persists all v5 conversations to server storage while
// keeping client-supplied provider and MCP runtime settings exclusively in RAM.
package chatsession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
	"github.com/starfall-org/serverside-generative/internal/storedruntime"
)

var (
	ErrNotFound = errors.New("session not found")
	ErrMode     = errors.New("conversation conflict")
	ErrInvalid  = errors.New("invalid session request")
	ErrLimit    = errors.New("session limit reached")
)

// storedRuntimeError maps storage failures to the public session contract.
func storedRuntimeError(err error) error {
	switch {
	case errors.Is(err, storedruntime.ErrAuth):
		return ErrAuthRequired
	case errors.Is(err, storedruntime.ErrNotFound):
		return ErrRuntime
	default:
		return ErrUnsupported
	}
}

const maxSessions = 128
const maxRuntimeBytes = 2 << 20

// Leave room for the JSON RPC/event envelope inside a 4 MiB transport frame.
const maxConversationBytes = 3 << 20

type View struct {
	SessionID    string          `json:"session_id"`
	Conversation json.RawMessage `json:"conversation"`
	Revision     uint64          `json:"revision"`
}

type session struct {
	owner             string
	conversation      json.RawMessage
	revision          uint64
	runtime           json.RawMessage
	running           bool
	activeRequest     string
	cancel            context.CancelFunc
	started           map[string]string
	recorded          map[string]bool
	seq, floor        uint64
	events            []Event
	eventSizes        []int
	eventBytes        int
	watchers          map[chan struct{}]struct{}
	timer             *time.Timer
	timerGeneration   uint64
	discoveries       map[uint64]context.CancelFunc
	discoverySequence uint64
}

type Manager struct {
	mu                 sync.Mutex
	sessions           map[string]*session
	store              *chatstore.Store
	allowLocalAPI      bool
	storedResolver     func(json.RawMessage) (json.RawMessage, error)
	storedMCPResolver  func(json.RawMessage, json.RawMessage, string) (json.RawMessage, error)
	storedToolResolver func(json.RawMessage) (json.RawMessage, error)
	ttl                time.Duration
}

// New uses the provided durable chat store; never silently fall back to RAM.
func New(store *chatstore.Store) *Manager {
	return &Manager{sessions: make(map[string]*session), store: store, ttl: 30 * time.Minute}
}

func clone(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }
func view(id string, s *session) View {
	return View{SessionID: id, Conversation: clone(s.conversation), Revision: s.revision}
}

func initialConversation(id string) json.RawMessage {
	now := time.Now().UnixMilli()
	b, _ := json.Marshal(map[string]any{
		"id": id, "title": "", "created_at": now, "updated_at": now,
		"messages": []any{},
	})
	return b
}

// Open always resumes or creates a durable conversation. There is no chat
// memory/sync selection: the client controls only its own local retention.
// Runtime provider/MCP secrets are never saved in the conversation store.
func (m *Manager) Open(owner, id string, conversation, runtime json.RawMessage) (View, error) {
	if owner == "" || id == "" || m.store == nil {
		return View{}, ErrInvalid
	}
	if len(runtime) > 0 && !validRuntime(runtime) {
		return View{}, ErrInvalid
	}
	if len(conversation) > maxConversationBytes {
		return View{}, ErrLimit
	}
	if len(conversation) > 0 && chatstore.Validate(id, conversation) != nil {
		return View{}, ErrInvalid
	}
	if len(runtime) > 0 {
		secrets := runtimeSecrets(runtime)
		if hasSecret(owner, secrets) || hasSecret(id, secrets) || jsonContainsSecret(conversation, secrets) {
			return View{}, ErrSecretLeak
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		if s.owner != owner {
			return View{}, ErrNotFound
		}
		if err := m.checkSnapshot(id, s); err != nil {
			return View{}, err
		}
		if len(runtime) > 0 {
			if s.running || len(s.discoveries) > 0 {
				return View{}, ErrBusy
			}
			s.runtime = clone(runtime)
		}
		m.touch(id, s)
		return view(id, s), nil
	}
	if len(m.sessions) >= maxSessions {
		return View{}, ErrLimit
	}
	snap, err := m.store.Get(owner, id)
	if errors.Is(err, os.ErrNotExist) {
		if len(conversation) == 0 {
			conversation = initialConversation(id)
		}
		opID, e := randomID()
		if e != nil {
			return View{}, e
		}
		snap, err = m.store.Save(id, owner, opID, 0, conversation)
	}
	if errors.Is(err, chatstore.ErrDeleted) {
		return View{}, ErrMode
	}
	if err != nil {
		return View{}, err
	}
	snap, err = m.recoverInterrupted(owner, id, snap)
	if err != nil {
		return View{}, err
	}
	s := &session{
		owner: owner, conversation: clone(snap.Conversation),
		revision: snap.Revision, runtime: clone(runtime),
		recorded: map[string]bool{},
	}
	for _, r := range snap.Runs {
		s.recorded[r.ID] = true
	}
	m.sessions[id] = s
	m.touch(id, s)
	return view(id, s), nil
}

// Recovery preserves the original session history and marks unfinished model or
// tool calls as interrupted, but only after the previous job lease is released.
func (m *Manager) recoverInterrupted(owner, id string, snap chatstore.Snapshot) (chatstore.Snapshot, error) {
	active := false
	for _, r := range snap.Runs {
		if r.Status == "started" {
			active = true
			break
		}
	}
	if !active {
		return snap, nil
	}
	unlock, err := m.store.TryJobLease(id)
	if errors.Is(err, chatstore.ErrJobBusy) {
		return snap, nil
	}
	if err != nil {
		return chatstore.Snapshot{}, err
	}
	defer unlock()
	conversation, err := interruptedToolConversation(snap.Conversation)
	if err != nil {
		return chatstore.Snapshot{}, err
	}
	for _, r := range snap.Runs {
		if r.Status != "started" {
			continue
		}
		op, e := randomID()
		if e != nil {
			return chatstore.Snapshot{}, e
		}
		snap, err = m.store.SaveRunCompared(id, owner, op, snap.Revision, snap.Conversation, conversation, r.ID, "interrupted")
		if err != nil {
			return chatstore.Snapshot{}, err
		}
	}
	return snap, nil
}

func interruptedToolConversation(raw json.RawMessage) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	var messages []json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil, ErrInvalid
	}
	if len(doc["messages"]) == 0 {
		return raw, nil
	}
	if json.Unmarshal(doc["messages"], &messages) != nil {
		return nil, ErrInvalid
	}
	if len(messages) == 0 {
		return raw, nil
	}
	var message map[string]json.RawMessage
	var versions []json.RawMessage
	if json.Unmarshal(messages[len(messages)-1], &message) != nil {
		return nil, ErrInvalid
	}
	if len(message["versions"]) == 0 {
		return raw, nil
	}
	if json.Unmarshal(message["versions"], &versions) != nil {
		return nil, ErrInvalid
	}
	changed := false
	for i, rv := range versions {
		var ver map[string]json.RawMessage
		var interrupted bool
		if json.Unmarshal(rv, &ver) != nil || json.Unmarshal(ver["generation_interrupted"], &interrupted) != nil || !interrupted {
			continue
		}
		var activities []map[string]json.RawMessage
		if json.Unmarshal(ver["toolActivity"], &activities) != nil {
			continue
		}
		update := false
		for _, a := range activities {
			var status string
			if json.Unmarshal(a["status"], &status) == nil && status == "running" {
				a["status"], _ = json.Marshal("error")
				a["response"], _ = json.Marshal("tool_outcome_unknown_after_restart")
				update = true
			}
		}
		if update {
			ver["toolActivity"], _ = json.Marshal(activities)
			versions[i], _ = json.Marshal(ver)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	message["versions"], _ = json.Marshal(versions)
	messages[len(messages)-1], _ = json.Marshal(message)
	doc["messages"], _ = json.Marshal(messages)
	return json.Marshal(doc)
}

func (m *Manager) Get(owner, id string) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return View{}, ErrNotFound
	}
	if err := m.checkSnapshot(id, s); err != nil {
		return View{}, err
	}
	m.touch(id, s)
	return view(id, s), nil
}

func (m *Manager) Configure(owner, id string, runtime json.RawMessage) error {
	if !validRuntime(runtime) {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return ErrNotFound
	}
	if s.running || len(s.discoveries) > 0 {
		return ErrBusy
	}
	secrets := runtimeSecrets(runtime)
	if hasSecret(owner, secrets) || hasSecret(id, secrets) {
		return ErrSecretLeak
	}
	s.runtime = clone(runtime)
	m.touch(id, s)
	return nil
}

// Save is an explicit client-side edit of the durable server conversation.
// The revision/CAS boundary prevents lost updates and duplicated save requests.
func (m *Manager) Save(owner, id, requestID string, expected uint64, conversation json.RawMessage) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return View{}, ErrNotFound
	}
	if s.running {
		return View{}, ErrBusy
	}
	if len(conversation) > maxConversationBytes {
		return View{}, ErrLimit
	}
	if chatstore.Validate(id, conversation) != nil {
		return View{}, ErrInvalid
	}
	secrets := runtimeSecrets(s.runtime)
	if hasSecret(requestID, secrets) || jsonContainsSecret(conversation, secrets) {
		return View{}, ErrSecretLeak
	}
	unlock, err := m.store.TryJobLease(id)
	if errors.Is(err, chatstore.ErrJobBusy) {
		return View{}, ErrBusy
	}
	if err != nil {
		return View{}, err
	}
	defer unlock()
	snap, err := m.store.SaveCompared(id, owner, requestID, expected, s.conversation, conversation)
	if err != nil {
		return View{}, err
	}
	s.conversation = clone(snap.Conversation)
	s.revision = snap.Revision
	m.emitConversation(s, id, requestID)
	m.touch(id, s)
	return view(id, s), nil
}

func (m *Manager) Close(owner, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return ErrNotFound
	}
	if s.running {
		return ErrBusy
	}
	m.release(id, s)
	return nil
}

func validRuntime(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > maxRuntimeBytes || !json.Valid(raw) {
		return false
	}
	secrets := runtimeSecrets(raw)
	if len(secrets) > 256 {
		return false
	}
	for _, secret := range secrets {
		if len(secret) > 16<<10 {
			return false
		}
	}
	var v struct {
		Source string `json:"source"`
	}
	return json.Unmarshal(raw, &v) == nil && (v.Source == "stored" || v.Source == "mobile_ephemeral")
}

type ListItem struct {
	DeletedAt int64  `json:"deleted_at,omitempty"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
	Revision  uint64 `json:"revision"`
}

// List includes the owner's persisted conversations, even after process restart.
func (m *Manager) List(owner string, offset, limit int) ([]ListItem, int, error) {
	if offset < 0 || limit < 1 || limit > 100 {
		return nil, 0, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snaps, tombs, err := m.store.List(owner)
	if err != nil {
		return nil, 0, err
	}
	items := make([]ListItem, 0, len(snaps)+len(tombs))
	for _, snap := range snaps {
		var c struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			UpdatedAt int64  `json:"updated_at"`
		}
		if json.Unmarshal(snap.Conversation, &c) != nil {
			return nil, 0, ErrInvalid
		}
		items = append(items, ListItem{ID: c.ID, Title: c.Title, UpdatedAt: c.UpdatedAt, Revision: snap.Revision})
	}
	for _, t := range tombs {
		items = append(items, ListItem{ID: t.ID, Revision: t.Revision, DeletedAt: t.DeletedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	next := 0
	if end < len(items) {
		next = end
	}
	return items[offset:end], next, nil
}

// Explicit deletion uses durable tombstones; closing a session never deletes it.
func (m *Manager) Delete(owner, id, opID string, expected *uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s != nil {
		if s.owner != owner {
			return ErrNotFound
		}
		if s.running {
			return ErrBusy
		}
		if hasSecret(opID, runtimeSecrets(s.runtime)) {
			return ErrSecretLeak
		}
	}
	if expected == nil {
		return ErrInvalid
	}
	if s == nil {
		_, err := m.store.Get(owner, id)
		if errors.Is(err, chatstore.ErrDeleted) {
			_, err = m.store.Delete(owner, id, opID, *expected)
			return err
		}
		if err != nil {
			return err
		}
	}
	unlock, err := m.store.TryJobLease(id)
	if errors.Is(err, chatstore.ErrJobBusy) {
		return ErrBusy
	}
	if err != nil {
		return err
	}
	defer unlock()
	if s != nil {
		_, err = m.store.DeleteCompared(owner, id, opID, *expected, s.conversation)
	} else {
		_, err = m.store.Delete(owner, id, opID, *expected)
	}
	if err != nil {
		return err
	}
	if s != nil {
		m.release(id, s)
	}
	return nil
}

func (m *Manager) release(id string, s *session) {
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.cancel != nil {
		s.cancel()
	}
	for _, cancel := range s.discoveries {
		cancel()
	}
	s.discoveries = nil
	for ch := range s.watchers {
		delete(s.watchers, ch)
		close(ch)
	}
	delete(m.sessions, id)
}
func (m *Manager) Revoke(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if owner == "" || s.owner == owner {
			m.release(id, s)
		}
	}
}
func (m *Manager) AllowLocalAPI(allow bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowLocalAPI = allow
}
func (m *Manager) touch(id string, s *session) {
	if s.timer != nil {
		s.timer.Stop()
	}
	ttl := m.ttl
	s.timerGeneration++
	gen := s.timerGeneration
	s.timer = time.AfterFunc(ttl, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.sessions[id] != s || s.timerGeneration != gen {
			return
		}
		if s.running {
			m.touch(id, s)
			return
		}
		m.release(id, s)
	})
}

func (m *Manager) checkSnapshot(id string, s *session) error {
	snap, err := m.store.Get(s.owner, id)
	if err != nil {
		if errors.Is(err, chatstore.ErrDeleted) {
			m.release(id, s)
		}
		return err
	}
	if snap.Revision != s.revision || !chatstore.Equal(snap.Conversation, s.conversation) {
		return chatstore.ErrConflict
	}
	return nil
}

func (m *Manager) SetStoredResolver(resolve func(json.RawMessage) (json.RawMessage, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storedResolver = resolve
}

func (m *Manager) SetStoredMCPResolver(resolve func(json.RawMessage, json.RawMessage, string) (json.RawMessage, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storedMCPResolver = resolve
}

func (m *Manager) SetStoredToolResolver(resolve func(json.RawMessage) (json.RawMessage, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storedToolResolver = resolve
}

func (m *Manager) Cancel(owner, id, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return ErrNotFound
	}
	if !s.running || s.activeRequest != requestID || s.cancel == nil {
		return ErrNotFound
	}
	s.cancel()
	m.touch(id, s)
	return nil
}
