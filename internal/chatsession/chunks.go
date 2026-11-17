package chatsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
)

// Uploads are bounded, owner-bound, process-local and never written to disk.
const chunkSize = 256 << 10
const maxUploads = 4
const uploadTTL = 10 * time.Minute

type ChunkInfo struct {
	SessionID string `json:"session_id"`
	Revision  uint64 `json:"revision"`
	Size      int    `json:"size"`
	Version   int    `json:"version"`
	EventSeq  uint64 `json:"event_seq"`
}
type Chunk struct {
	Revision uint64 `json:"revision"`
	Offset   int    `json:"offset"`
	Data     []byte `json:"data"`
	Done     bool   `json:"done"`
}
type chunkUpload struct {
	owner, session, operation, digest string
	expected                          uint64
	size                              int
	data                              []byte
	expires                           time.Time
	committed                         *ChunkInfo
	timer                             *time.Timer
}

func (m *Manager) OpenChunked(owner, id string) (ChunkInfo, error) {
	v, err := m.Open(owner, id, nil, nil)
	if err != nil {
		return ChunkInfo{}, err
	}
	return chunkInfo(v), nil
}
func chunkInfo(v View) ChunkInfo {
	return ChunkInfo{SessionID: v.SessionID, Revision: v.Revision, Size: len(v.Conversation), Version: 1, EventSeq: v.EventSeq}
}
func (m *Manager) ReadChunk(owner, id string, revision uint64, offset, limit int) (Chunk, error) {
	if revision == 0 || offset < 0 || limit < 1 || limit > chunkSize {
		return Chunk{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.owner != owner {
		return Chunk{}, ErrNotFound
	}
	if err := m.checkSnapshot(id, s); err != nil {
		return Chunk{}, err
	}
	if s.revision != revision {
		return Chunk{}, chatstore.ErrConflict
	}
	if offset > len(s.conversation) {
		return Chunk{}, ErrInvalid
	}
	end := offset + limit
	if end > len(s.conversation) {
		end = len(s.conversation)
	}
	m.touch(id, s)
	return Chunk{Revision: revision, Offset: offset, Data: append([]byte{}, s.conversation[offset:end]...), Done: end == len(s.conversation)}, nil
}
func (m *Manager) BeginUpload(owner, id, operation, digest string, expected uint64, size int) (string, error) {
	if owner == "" || id == "" || operation == "" || expected == 0 || size < 1 || size > maxChunkedConversationBytes {
		return "", ErrInvalid
	}
	hash, err := hex.DecodeString(digest)
	if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != digest {
		return "", ErrInvalid
	}
	v, err := m.Get(owner, id)
	if err != nil {
		return "", err
	}
	if v.Revision != expected {
		return "", chatstore.ErrConflict
	}
	token, err := randomID()
	if err != nil {
		return "", err
	}
	m.transferMu.Lock()
	defer m.transferMu.Unlock()
	now := time.Now()
	if m.uploads == nil {
		m.uploads = map[string]*chunkUpload{}
	}
	for key, u := range m.uploads {
		if !now.Before(u.expires) {
			if u.timer != nil {
				u.timer.Stop()
			}
			delete(m.uploads, key)
		}
	}
	if len(m.uploads) >= maxUploads {
		return "", ErrLimit
	}
	m.uploads[token] = &chunkUpload{owner: owner, session: id, operation: operation, digest: digest, expected: expected, size: size, data: make([]byte, 0, size), expires: now.Add(uploadTTL)}
	m.uploads[token].timer = time.AfterFunc(uploadTTL, func() {
		m.transferMu.Lock()
		defer m.transferMu.Unlock()
		if u := m.uploads[token]; u != nil && !time.Now().Before(u.expires) {
			delete(m.uploads, token)
		}
	})
	return token, nil
}
func (m *Manager) UploadChunk(owner, id, token string, offset int, data []byte) (int, error) {
	if offset < 0 || len(data) == 0 || len(data) > chunkSize {
		return 0, ErrInvalid
	}
	m.transferMu.Lock()
	defer m.transferMu.Unlock()
	u := m.uploads[token]
	if u == nil || u.owner != owner || u.session != id || !time.Now().Before(u.expires) || u.committed != nil {
		return 0, ErrInvalid
	}
	if offset < len(u.data) {
		if offset+len(data) > len(u.data) || !bytes.Equal(u.data[offset:offset+len(data)], data) {
			return 0, ErrInvalid
		}
		return len(u.data), nil
	}
	if offset != len(u.data) || len(data) > u.size-len(u.data) {
		return 0, ErrInvalid
	}
	u.data = append(u.data, data...)
	return len(u.data), nil
}
func (m *Manager) CommitUpload(owner, id, token string) (ChunkInfo, error) {
	m.transferMu.Lock()
	defer m.transferMu.Unlock()
	u := m.uploads[token]
	if u == nil || u.owner != owner || u.session != id || !time.Now().Before(u.expires) {
		return ChunkInfo{}, ErrInvalid
	}
	if u.committed != nil {
		return *u.committed, nil
	}
	sum := sha256.Sum256(u.data)
	if len(u.data) != u.size || hex.EncodeToString(sum[:]) != u.digest {
		return ChunkInfo{}, ErrInvalid
	}
	v, err := m.saveBounded(owner, u.session, u.operation, u.expected, json.RawMessage(u.data), maxChunkedConversationBytes)
	if err != nil {
		return ChunkInfo{}, err
	}
	info := chunkInfo(v)
	u.committed = &info
	u.data = nil
	return info, nil
}
func (m *Manager) AbortUpload(owner, id, token string) error {
	m.transferMu.Lock()
	defer m.transferMu.Unlock()
	u := m.uploads[token]
	if u == nil || u.owner != owner || u.session != id {
		return ErrInvalid
	}
	if u.timer != nil {
		u.timer.Stop()
	}
	delete(m.uploads, token)
	return nil
}
