package chatsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starfall-org/serverside-generative/internal/chatstore"
)

func chunkFixture(t *testing.T) (*Manager, json.RawMessage) {
	t.Helper()
	m := New(chatstore.New(filepath.Join(t.TempDir(), "data")))
	raw := json.RawMessage(`{"id":"large","title":"test","created_at":1,"updated_at":1,"messages":[]}`)
	if _, err := m.Open("owner", "large", raw, nil); err != nil {
		t.Fatal(err)
	}
	return m, raw
}
func digest(raw []byte) string { d := sha256.Sum256(raw); return hex.EncodeToString(d[:]) }
func TestChunkedLargeCASOwnerAndRestart(t *testing.T) {
	m, _ := chunkFixture(t)
	raw, _ := json.Marshal(map[string]any{"id": "large", "title": "test", "created_at": 1, "updated_at": 2, "messages": []any{}, "extension": strings.Repeat("z", 4<<20)})
	token, err := m.BeginUpload("owner", "large", "op", digest(raw), 1, len(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UploadChunk("other", "large", token, 0, []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("owner: %v", err)
	}
	if _, err = m.UploadChunk("owner", "wrong", token, 0, []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("session: %v", err)
	}
	for offset := 0; offset < len(raw); {
		end := offset + chunkSize
		if end > len(raw) {
			end = len(raw)
		}
		part := raw[offset:end]
		next, e := m.UploadChunk("owner", "large", token, offset, part)
		if e != nil || next != end {
			t.Fatalf("chunk %d %v", next, e)
		}
		if offset == 0 {
			if again, e := m.UploadChunk("owner", "large", token, offset, part); e != nil || again != end {
				t.Fatalf("retry %d %v", again, e)
			}
		}
		offset = end
	}
	info, err := m.CommitUpload("owner", "large", token)
	if err != nil || info.Revision != 2 || info.Size != len(raw) {
		t.Fatalf("commit: %+v %v", info, err)
	}
	retry, err := m.CommitUpload("owner", "large", token)
	if err != nil || retry != info {
		t.Fatalf("retry %+v %v", retry, err)
	}
	var joined []byte
	for offset := 0; ; {
		c, e := m.ReadChunk("owner", "large", 2, offset, chunkSize)
		if e != nil {
			t.Fatal(e)
		}
		joined = append(joined, c.Data...)
		offset += len(c.Data)
		if c.Done {
			break
		}
	}
	if !bytes.Equal(joined, raw) {
		t.Fatal("chunk reconstruction differs")
	}
	if _, err = m.ReadChunk("owner", "large", 1, 0, chunkSize); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("stale read %v", err)
	}
	if _, err = m.ReadChunk("other", "large", 2, 0, chunkSize); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner read %v", err)
	}
	if err = m.Close("owner", "large"); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.OpenChunked("owner", "large")
	if err != nil || reopened.Revision != 2 {
		t.Fatalf("reopen %+v %v", reopened, err)
	}
	// New manager represents a daemon restart: incomplete transfers do not survive.
	newManager := New(m.store)
	if _, err = newManager.CommitUpload("owner", "large", token); !errors.Is(err, ErrInvalid) {
		t.Fatalf("restart %v", err)
	}
}
func TestChunkedConflictsAndIncompleteUploads(t *testing.T) {
	m, raw := chunkFixture(t)
	token, err := m.BeginUpload("owner", "large", "op", digest(raw), 1, len(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UploadChunk("owner", "large", token, 1, raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("out of order %v", err)
	}
	if _, err = m.CommitUpload("owner", "large", token); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial %v", err)
	}
	if _, err = m.UploadChunk("owner", "large", token, 0, raw); err != nil {
		t.Fatal(err)
	}
	updated := json.RawMessage(`{"id":"large","title":"new","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err = m.Save("owner", "large", "other", 1, updated); err != nil {
		t.Fatal(err)
	}
	if _, err = m.CommitUpload("owner", "large", token); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("stale commit %v", err)
	}
	if err = m.AbortUpload("other", "large", token); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign abort %v", err)
	}
	if err = m.AbortUpload("owner", "large", token); err != nil {
		t.Fatal(err)
	}
	if _, err = m.BeginUpload("owner", "large", "op", digest(raw), 2, maxChunkedConversationBytes+1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize %v", err)
	}
}

func TestChunkCommitBlocksRuntimeSecrets(t *testing.T) {
	m, raw := chunkFixture(t)
	runtime := json.RawMessage(`{"source":"mobile_ephemeral","provider":{"type":"openai","modelId":"m","auth":{"method":"bearer_token","value":"secret-sentinel"}}}`)
	if err := m.Configure("owner", "large", runtime); err != nil {
		t.Fatal(err)
	}
	leaked := json.RawMessage(`{"id":"large","title":"secret-sentinel","created_at":1,"updated_at":1,"messages":[]}`)
	token, err := m.BeginUpload("owner", "large", "leak", digest(leaked), 1, len(leaked))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UploadChunk("owner", "large", token, 0, leaked); err != nil {
		t.Fatal(err)
	}
	if _, err = m.CommitUpload("owner", "large", token); !errors.Is(err, ErrSecretLeak) {
		t.Fatalf("secret: %v", err)
	}
	got, err := m.Get("owner", "large")
	if err != nil || got.Revision != 1 || !bytes.Equal(got.Conversation, raw) {
		t.Fatalf("modified: %+v %v", got, err)
	}
}

func TestConcurrentChunkUploadsUseCAS(t *testing.T) {
	m, _ := chunkFixture(t)
	a := json.RawMessage(`{"id":"large","title":"A","created_at":1,"updated_at":2,"messages":[]}`)
	b := json.RawMessage(`{"id":"large","title":"B","created_at":1,"updated_at":2,"messages":[]}`)
	first, err := m.BeginUpload("owner", "large", "a", digest(a), 1, len(a))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.BeginUpload("owner", "large", "b", digest(b), 1, len(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UploadChunk("owner", "large", first, 0, a); err != nil {
		t.Fatal(err)
	}
	if _, err = m.UploadChunk("owner", "large", second, 0, b); err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{first, second} {
		go func(token string) { <-barrier; _, e := m.CommitUpload("owner", "large", token); results <- e }(id)
	}
	close(barrier)
	one, two := <-results, <-results
	if !((one == nil && errors.Is(two, chatstore.ErrConflict)) || (two == nil && errors.Is(one, chatstore.ErrConflict))) {
		t.Fatalf("results: %v %v", one, two)
	}
}

func TestChunkUploadCapacityAndExpiration(t *testing.T) {
	m, raw := chunkFixture(t)
	tokens := make([]string, 0, maxUploads)
	for i := 0; i < maxUploads; i++ {
		token, err := m.BeginUpload("owner", "large", "op", digest(raw), 1, len(raw))
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	if _, err := m.BeginUpload("owner", "large", "op", digest(raw), 1, len(raw)); !errors.Is(err, ErrLimit) {
		t.Fatalf("capacity: %v", err)
	}
	m.transferMu.Lock()
	m.uploads[tokens[0]].expires = time.Now().Add(-time.Second)
	m.transferMu.Unlock()
	if _, err := m.UploadChunk("owner", "large", tokens[0], 0, raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := m.BeginUpload("owner", "large", "op", digest(raw), 1, len(raw)); err != nil {
		t.Fatalf("expired capacity: %v", err)
	}
}
