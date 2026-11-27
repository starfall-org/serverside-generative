// Package chatstore implements the revisioned file format for opt-in chat sync.
// It does not persist runtime configuration or manage transport sessions.
package chatstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/starfall-org/generative-server/internal/compat"
)

const MaxConversationBytes = 16 << 20
const maxSnapshotBytes = MaxConversationBytes + (64 << 10)

var ErrConflict = errors.New("revision conflict")
var ErrInvalid = errors.New("invalid conversation")

type Commit struct {
	ClientID     string `json:"client_id"`
	OpID         string `json:"op_id"`
	BaseRevision uint64 `json:"base_revision"`
}

type RunReceipt struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type Snapshot struct {
	SchemaVersion int             `json:"schema_version"`
	OwnerID       string          `json:"owner_id"`
	Revision      uint64          `json:"revision"`
	LastCommit    Commit          `json:"last_commit"`
	Runs          []RunReceipt    `json:"runs,omitempty"`
	Conversation  json.RawMessage `json:"conversation"`
}

// Store serializes writers in-process and uses a directory lock across processes.
// External edits are not automatically reconciled with active sessions.
type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) *Store { return &Store{dir: filepath.Join(dir, "chats")} }

func filename(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:]) + ".json"
}

// Validate preserves the original JSON, including unknown fields and null values.
// Runtime connection settings are separate from the conversation and must never
// be passed to this function as part of the snapshot.
func Validate(id string, raw json.RawMessage) error {
	if !validID(id) {
		return ErrInvalid
	}
	if _, err := compat.ParseConversation(raw); err != nil {
		return ErrInvalid
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.ID != id {
		return ErrInvalid
	}
	return nil
}

func (s *Store) load(id string) (Snapshot, error) {
	var snap Snapshot
	if err := s.checkDirs(false); err != nil {
		return snap, err
	}
	if _, err := s.tombstone(id); err == nil {
		return snap, ErrDeleted
	} else if !errors.Is(err, os.ErrNotExist) {
		return snap, err
	}
	path := filepath.Join(s.dir, filename(id))
	data, err := readBoundedRegular(path, true)
	if err != nil {
		return snap, err
	}
	if err = json.Unmarshal(data, &snap); err != nil || snap.SchemaVersion != 1 || snap.OwnerID == "" || snap.Revision == 0 || Validate(id, snap.Conversation) != nil {
		return Snapshot{}, ErrInvalid
	}
	if len(snap.Runs) > 128 {
		return Snapshot{}, ErrInvalid
	}
	seen := map[string]bool{}
	for _, run := range snap.Runs {
		if !validID(run.ID) || seen[run.ID] || (run.Status != "started" && run.Status != "completed" && run.Status != "interrupted") {
			return Snapshot{}, ErrInvalid
		}
		seen[run.ID] = true
	}
	return snap, nil
}

func (s *Store) Get(owner, id string) (Snapshot, error) {
	if !validID(owner) || !validID(id) {
		return Snapshot{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirs(false); err != nil {
		return Snapshot{}, err
	}
	unlock, err := lockDirectory(s.dir, false)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	snap, err := s.load(id)
	if err == nil && snap.OwnerID != owner {
		return Snapshot{}, ErrConflict
	}
	return snap, err
}

// Save publishes one complete snapshot after a revision compare-and-swap.
// Replaying the most recent operation is accepted only when its contents match.
func (s *Store) Save(id, clientID, opID string, expected uint64, conversation json.RawMessage) (Snapshot, error) {
	return s.save(id, clientID, opID, expected, conversation, "", "", nil, nil)
}

// SaveRun records the run ID in the server envelope, not in the mobile DTO.
func (s *Store) SaveRun(id, clientID, opID string, expected uint64, conversation json.RawMessage, runID, status string) (Snapshot, error) {
	if !validID(runID) || (status != "started" && status != "completed" && status != "interrupted") {
		return Snapshot{}, ErrInvalid
	}
	return s.save(id, clientID, opID, expected, conversation, runID, status, nil, nil)
}

// Reload publishes a validated hand-edited snapshot as a new revision only if
// the complete envelope is unchanged since the caller inspected it.
func (s *Store) Reload(id, owner, opID string, inspected Snapshot) (Snapshot, error) {
	if inspected.OwnerID != owner || inspected.Revision == 0 {
		return Snapshot{}, ErrInvalid
	}
	return s.save(id, owner, opID, inspected.Revision, inspected.Conversation, "", "", &inspected, nil)
}

// SaveCompared also rejects hand edits that retained the old revision.
func (s *Store) SaveCompared(id, owner, opID string, expected uint64, prior, conversation json.RawMessage) (Snapshot, error) {
	if len(prior) == 0 {
		return Snapshot{}, ErrInvalid
	}
	return s.save(id, owner, opID, expected, conversation, "", "", nil, prior)
}

// SaveRunCompared applies the same content check to durable job receipts.
func (s *Store) SaveRunCompared(id, owner, opID string, expected uint64, prior, conversation json.RawMessage, runID, status string) (Snapshot, error) {
	if len(prior) == 0 || !validID(runID) || (status != "started" && status != "completed" && status != "interrupted") {
		return Snapshot{}, ErrInvalid
	}
	return s.save(id, owner, opID, expected, conversation, runID, status, nil, prior)
}

func (s *Store) save(id, clientID, opID string, expected uint64, conversation json.RawMessage, runID, status string, inspected *Snapshot, prior json.RawMessage) (Snapshot, error) {
	if !validID(clientID) || !validID(opID) || Validate(id, conversation) != nil {
		return Snapshot{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirs(true); err != nil {
		return Snapshot{}, err
	}
	unlock, err := lockDirectory(s.dir, true)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	old, err := s.load(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	if err == nil && old.OwnerID != clientID {
		return Snapshot{}, ErrConflict
	}
	if inspected != nil && (err != nil || !reflect.DeepEqual(old, *inspected)) {
		return Snapshot{}, ErrConflict
	}
	if prior != nil && (err != nil || !jsonEqual(old.Conversation, prior)) {
		return Snapshot{}, ErrConflict
	}
	if err == nil && old.LastCommit.ClientID == clientID && old.LastCommit.OpID == opID {
		if old.LastCommit.BaseRevision == expected && jsonEqual(old.Conversation, conversation) {
			return old, nil
		}
		return Snapshot{}, ErrConflict
	}
	if expected == math.MaxUint64 || old.Revision != expected {
		return Snapshot{}, ErrConflict
	}
	runs := append([]RunReceipt(nil), old.Runs...)
	if runID != "" {
		index := -1
		for i, run := range runs {
			if run.ID == runID {
				index = i
				break
			}
		}
		if status == "started" {
			if index >= 0 {
				return Snapshot{}, ErrConflict
			}
			if len(runs) >= 128 {
				return Snapshot{}, ErrInvalid
			}
			runs = append(runs, RunReceipt{ID: runID, Status: status})
		} else {
			if index < 0 || runs[index].Status != "started" {
				return Snapshot{}, ErrConflict
			}
			runs[index].Status = status
		}
	}
	snap := Snapshot{SchemaVersion: 1, OwnerID: clientID, Revision: expected + 1, LastCommit: Commit{clientID, opID, expected}, Runs: runs, Conversation: append(json.RawMessage(nil), conversation...)}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return Snapshot{}, err
	}
	data = append(data, '\n')
	if len(data) > maxSnapshotBytes {
		return Snapshot{}, ErrInvalid
	}
	if inspected != nil {
		if err = s.backupReload(id, opID, old); err != nil {
			return Snapshot{}, err
		}
	}
	if old.Revision != 0 {
		if err = s.backupPrevious(id, old); err != nil {
			return Snapshot{}, err
		}
	}
	if err = s.checkDirs(true); err != nil {
		return Snapshot{}, err
	}
	// Reject a symlink for the final directory; do not follow an attacker-controlled chat directory.
	info, err := os.Lstat(s.dir)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return Snapshot{}, fmt.Errorf("unsafe chat directory")
	}
	f, err := os.CreateTemp(s.dir, ".chat-*")
	if err != nil {
		return Snapshot{}, err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err = os.Rename(name, filepath.Join(s.dir, filename(id))); err != nil {
		return Snapshot{}, err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return Snapshot{}, err
	}
	err = d.Sync()
	closeErr = d.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// backupPrevious keeps one validated previous snapshot per chat. It is written
// before publishing the next revision, and never follows a backup symlink.
func (s *Store) backupPrevious(id string, old Snapshot) error {
	raw, err := readBoundedRegular(filepath.Join(s.dir, filename(id)), true)
	if err != nil {
		return err
	}
	var current Snapshot
	if json.Unmarshal(raw, &current) != nil || !reflect.DeepEqual(current, old) {
		return ErrConflict
	}
	root := filepath.Dir(s.dir)
	backupRoot := filepath.Join(root, ".backups")
	dir := filepath.Join(backupRoot, "previous")
	for _, path := range []string{backupRoot, dir} {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return ErrInvalid
		}
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, filename(id))); err != nil {
		return err
	}
	return syncDirs(dir, backupRoot, root)
}

// Previous returns the retained envelope for inspection. It never restores a
// revision or bypasses a deletion tombstone.
func (s *Store) Previous(owner, id string) (Snapshot, error) {
	if !validID(owner) || !validID(id) {
		return Snapshot{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirs(false); err != nil {
		return Snapshot{}, err
	}
	unlock, err := lockDirectory(s.dir, false)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	if _, err := s.tombstone(id); err == nil {
		return Snapshot{}, ErrDeleted
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	root := filepath.Join(filepath.Dir(s.dir), ".backups")
	dir := filepath.Join(root, "previous")
	for _, path := range []string{root, dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return Snapshot{}, err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return Snapshot{}, ErrInvalid
		}
	}
	raw, err := readBoundedRegular(filepath.Join(dir, filename(id)), true)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SchemaVersion != 1 || snap.OwnerID != owner || snap.Revision == 0 || Validate(id, snap.Conversation) != nil {
		return Snapshot{}, ErrInvalid
	}
	return snap, nil
}

// backupReload preserves the exact validated pre-reload envelope before any
// replacement. Backups contain plaintext user chat and are private by default.
func (s *Store) backupReload(id, opID string, inspected Snapshot) error {
	source := filepath.Join(s.dir, filename(id))
	raw, err := readBoundedRegular(source, false)
	if err != nil {
		return err
	}
	var current Snapshot
	if len(raw) > maxSnapshotBytes || json.Unmarshal(raw, &current) != nil || !reflect.DeepEqual(current, inspected) {
		return ErrConflict
	}
	root := filepath.Dir(s.dir)
	backupRoot := filepath.Join(root, ".backups")
	backupDir := filepath.Join(backupRoot, "chats")
	var info os.FileInfo
	for _, dir := range []string{backupRoot, backupDir} {
		if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return ErrInvalid
		}
	}
	f, err := os.CreateTemp(backupDir, ".pending-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	opHash := sha256.Sum256([]byte(opID))
	name := fmt.Sprintf("%s-%d-%x.json", strings.TrimSuffix(filename(id), ".json"), inspected.Revision, opHash[:])
	destination := filepath.Join(backupDir, name)
	if err = os.Link(temp, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		prior, err := readBoundedRegular(destination, true)
		if err != nil {
			return err
		}
		if !bytes.Equal(prior, raw) {
			return ErrConflict
		}
	}
	return syncDirs(backupDir, backupRoot, root)
}

// syncDirs persists directory entries in order, including close errors.
func syncDirs(paths ...string) error {
	for _, path := range paths {
		d, err := os.Open(path)
		if err != nil {
			return err
		}
		err = d.Sync()
		if closeErr := d.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func readBoundedRegular(path string, private bool) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maxSnapshotBytes || (private && before.Mode().Perm()&0077 != 0) {
		return nil, ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || (private && after.Mode().Perm()&0077 != 0) {
		return nil, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSnapshotBytes {
		return nil, ErrInvalid
	}
	return data, nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	// Comparing canonical encodings keeps object key ordering irrelevant.
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}

func validID(id string) bool {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

// Tombstone records a revision-checked deletion. A tombstone takes precedence
// over an old snapshot if a process stops between publication and removal.
type Tombstone struct {
	ID        string `json:"id"`
	OwnerID   string `json:"owner_id"`
	Revision  uint64 `json:"revision"`
	DeletedAt int64  `json:"deleted_at"`
	OpID      string `json:"op_id"`
}

var ErrDeleted = errors.New("conversation deleted")
var ErrJobBusy = errors.New("synced chat job is active")

func (s *Store) tombstone(id string) (Tombstone, error) {
	var t Tombstone
	dir := filepath.Join(s.dir, ".tombstones")
	info, err := os.Lstat(dir)
	if err != nil {
		return t, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return t, ErrInvalid
	}
	path := filepath.Join(dir, filename(id))
	info, err = os.Lstat(path)
	if err != nil {
		return t, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return t, ErrInvalid
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	if json.Unmarshal(b, &t) != nil || t.ID != id || t.OwnerID == "" || t.Revision == 0 {
		return Tombstone{}, ErrInvalid
	}
	return t, nil
}
func (s *Store) Delete(owner, id, opID string, expected uint64) (Tombstone, error) {
	return s.delete(owner, id, opID, expected, nil)
}

// DeleteCompared protects an active session's view from same-revision edits.
func (s *Store) DeleteCompared(owner, id, opID string, expected uint64, prior json.RawMessage) (Tombstone, error) {
	if len(prior) == 0 {
		return Tombstone{}, ErrInvalid
	}
	return s.delete(owner, id, opID, expected, prior)
}

func (s *Store) delete(owner, id, opID string, expected uint64, prior json.RawMessage) (Tombstone, error) {
	if !validID(owner) || !validID(id) || !validID(opID) || expected == math.MaxUint64 {
		return Tombstone{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirs(false); err != nil {
		return Tombstone{}, err
	}
	unlock, err := lockDirectory(s.dir, true)
	if err != nil {
		return Tombstone{}, err
	}
	defer unlock()
	if old, err := s.tombstone(id); err == nil {
		if old.OwnerID == owner && old.OpID == opID && old.Revision == expected+1 {
			return old, nil
		}
		return Tombstone{}, ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return Tombstone{}, err
	}
	snap, err := s.load(id)
	if err != nil {
		return Tombstone{}, err
	}
	if snap.OwnerID != owner || snap.Revision != expected || (prior != nil && !jsonEqual(snap.Conversation, prior)) {
		return Tombstone{}, ErrConflict
	}
	tomb := Tombstone{ID: id, OwnerID: owner, Revision: expected + 1, DeletedAt: time.Now().UnixMilli(), OpID: opID}
	data, err := json.MarshalIndent(tomb, "", "  ")
	if err != nil {
		return Tombstone{}, err
	}
	dir := filepath.Join(s.dir, ".tombstones")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Tombstone{}, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return Tombstone{}, ErrInvalid
	}
	f, err := os.CreateTemp(dir, ".delete-*")
	if err != nil {
		return Tombstone{}, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Tombstone{}, err
	}
	if err = os.Rename(tmp, filepath.Join(dir, filename(id))); err != nil {
		return Tombstone{}, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return Tombstone{}, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return Tombstone{}, err
	}
	if err = os.Remove(filepath.Join(s.dir, filename(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Tombstone{}, err
	}
	d, err = os.Open(s.dir)
	if err != nil {
		return Tombstone{}, err
	}
	err = d.Sync()
	d.Close()
	return tomb, err
}

// List returns authoritative snapshots and tombstones owned by this client.
// List retains the legacy aggregate API for callers that need full snapshots.
func (s *Store) List(owner string) ([]Snapshot, []Tombstone, error) {
	snapshots := []Snapshot{}
	tombs := []Tombstone{}
	err := s.Visit(owner, func(snap Snapshot) error { snapshots = append(snapshots, snap); return nil }, func(t Tombstone) error { tombs = append(tombs, t); return nil })
	if err != nil {
		return nil, nil, err
	}
	return snapshots, tombs, nil
}

// Visit holds the store lock and validates one snapshot at a time; callbacks must
// not reenter the store or retain complete conversation bodies unnecessarily.
func (s *Store) Visit(owner string, visitSnapshot func(Snapshot) error, visitTombstone func(Tombstone) error) error {
	if !validID(owner) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDirs(false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	unlock, lockErr := lockDirectory(s.dir, false)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || len(entry.Name()) != 69 {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		data, err := readBoundedRegular(path, true)
		if err != nil {
			return err
		}
		var snap Snapshot
		if json.Unmarshal(data, &snap) != nil {
			return ErrInvalid
		}
		var c struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(snap.Conversation, &c) != nil || filename(c.ID) != entry.Name() {
			return ErrInvalid
		}
		verified, err := s.load(c.ID)
		if errors.Is(err, ErrDeleted) {
			continue
		}
		if err != nil {
			return err
		}
		if verified.OwnerID == owner {
			if err := visitSnapshot(verified); err != nil {
				return err
			}
		}
	}
	tombDir := filepath.Join(s.dir, ".tombstones")
	info, dirErr := os.Lstat(tombDir)
	if errors.Is(dirErr, os.ErrNotExist) {
		return nil
	}
	if dirErr != nil {
		return dirErr
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	entries, err = os.ReadDir(tombDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || len(entry.Name()) != 69 {
			continue
		}
		path := filepath.Join(s.dir, ".tombstones", entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return ErrInvalid
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var t Tombstone
		if json.Unmarshal(b, &t) != nil || filename(t.ID) != entry.Name() {
			return ErrInvalid
		}
		t, err = s.tombstone(t.ID)
		if err != nil {
			return err
		}
		if t.OwnerID == owner {
			if err := visitTombstone(t); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDirs rejects symlinks at both managed directory boundaries. The data
// directory must remain private because chat filenames and metadata are private.
func (s *Store) checkDirs(create bool) error {
	root := filepath.Dir(s.dir)
	if create {
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
	}
	if !create {
		if _, err := os.Lstat(s.dir); err != nil {
			return err
		}
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	if create {
		if err = os.Mkdir(s.dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err = os.Lstat(s.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	return nil
}

// Equal compares JSON semantically while preserving numeric precision.
func Equal(a, b json.RawMessage) bool { return jsonEqual(a, b) }
