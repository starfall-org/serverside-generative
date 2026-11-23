// Package secrets stores locally managed credentials and custom tools encrypted
// with a server-owned AES-256-GCM key. Runtime handoffs must not use this store.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/starfall-org/serverside-generative/internal/httptool"
	_ "modernc.org/sqlite"
)

var ErrMissingKey = errors.New("master key missing")

type Store struct {
	db        *sql.DB
	aead      cipher.AEAD
	unlock    func()
	closeOnce sync.Once
	closeErr  error
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe data directory")
	}
	unlock, err := lockDirectory(dir, true)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			unlock()
		}
	}()
	dbPath, keyPath := filepath.Join(dir, "data.sqlite"), filepath.Join(dir, "master.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(dbPath); e == nil {
			return nil, ErrMissingKey
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		key = make([]byte, 32)
		if _, err = io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		if _, e = f.Write(key); e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return nil, e
		}
		d, e := os.Open(dir)
		if e != nil {
			return nil, e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return nil, e
		}
	} else if err != nil {
		return nil, err
	}
	ki, err := os.Lstat(keyPath)
	if err != nil || !ki.Mode().IsRegular() || ki.Mode().Perm()&0077 != 0 || len(key) != 32 {
		return nil, errors.New("invalid master key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	di, err := os.Lstat(dbPath)
	if err != nil || !di.Mode().IsRegular() || di.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe database")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;`); err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// A stop during first startup can leave an empty SQLite file. Publish the
	// schema and its key check together, while refusing to bless an existing
	// schema whose key check is missing.
	var schemaObjects int
	if err = tx.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&schemaObjects); err != nil {
		return nil, err
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS master_key_check(id INTEGER PRIMARY KEY CHECK(id=1),nonce BLOB NOT NULL,ciphertext BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
 INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(1,unixepoch());
 CREATE TABLE IF NOT EXISTS credentials(owner_kind TEXT NOT NULL,owner_id TEXT NOT NULL,secret_id TEXT NOT NULL,label TEXT NOT NULL DEFAULT '',secret_kind TEXT NOT NULL,cipher_version INTEGER NOT NULL,nonce BLOB NOT NULL,ciphertext BLOB NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(owner_kind,owner_id,secret_id));
 CREATE TABLE IF NOT EXISTS custom_http_tools(id TEXT PRIMARY KEY,cipher_version INTEGER NOT NULL,nonce BLOB NOT NULL,ciphertext BLOB NOT NULL,updated_at INTEGER NOT NULL);`)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, aead: aead, unlock: unlock}
	var nonce, enc []byte
	err = tx.QueryRow(`SELECT nonce,ciphertext FROM master_key_check WHERE id=1`).Scan(&nonce, &enc)
	if errors.Is(err, sql.ErrNoRows) && schemaObjects != 0 {
		return nil, errors.New("database has no master key check")
	}
	if errors.Is(err, sql.ErrNoRows) {
		nonce, enc, err = store.seal([]byte("serverside-key-check-v1"), "master-key-check")
		if err == nil {
			_, err = tx.Exec(`INSERT INTO master_key_check(id,nonce,ciphertext) VALUES(1,?,?)`, nonce, enc)
		}
	} else if err == nil {
		var plain []byte
		plain, err = store.unseal(1, nonce, enc, "master-key-check")
		if err == nil && string(plain) != "serverside-key-check-v1" {
			err = errors.New("invalid master key")
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	success = true
	return store, nil
}
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
		s.unlock()
	})
	return s.closeErr
}
func aad(parts ...string) []byte { b, _ := json.Marshal(parts); return b }
func (s *Store) seal(value []byte, parts ...string) ([]byte, []byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, s.aead.Seal(nil, nonce, value, aad(parts...)), nil
}
func (s *Store) unseal(version int, nonce, ciphertext []byte, parts ...string) ([]byte, error) {
	if version != 1 || len(nonce) != s.aead.NonceSize() {
		return nil, errors.New("unsupported credential envelope")
	}
	b, err := s.aead.Open(nil, nonce, ciphertext, aad(parts...))
	if err != nil {
		return nil, errors.New("credential decryption failed")
	}
	return b, nil
}
func validID(s string) bool { return s != "" && len(s) <= 128 }
func (s *Store) Put(kind, owner, id, label, secretKind string, value []byte) error {
	if !validID(kind) || !validID(owner) || !validID(id) || !validID(secretKind) || len(label) > 512 || len(value) == 0 || len(value) > 1<<20 {
		return errors.New("invalid credential")
	}
	nonce, enc, err := s.seal(value, "credential", kind, owner, id)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.Exec(`INSERT INTO credentials VALUES(?,?,?,?,?,1,?,?,?,?) ON CONFLICT(owner_kind,owner_id,secret_id) DO UPDATE SET label=excluded.label,secret_kind=excluded.secret_kind,cipher_version=excluded.cipher_version,nonce=excluded.nonce,ciphertext=excluded.ciphertext,updated_at=excluded.updated_at`, kind, owner, id, label, secretKind, nonce, enc, now, now)
	return err
}
func (s *Store) Get(kind, owner, id string) ([]byte, error) {
	var version int
	var nonce, enc []byte
	err := s.db.QueryRow(`SELECT cipher_version,nonce,ciphertext FROM credentials WHERE owner_kind=? AND owner_id=? AND secret_id=?`, kind, owner, id).Scan(&version, &nonce, &enc)
	if err != nil {
		return nil, err
	}
	return s.unseal(version, nonce, enc, "credential", kind, owner, id)
}
func (s *Store) Delete(kind, owner, id string) error {
	_, err := s.db.Exec(`DELETE FROM credentials WHERE owner_kind=? AND owner_id=? AND secret_id=?`, kind, owner, id)
	return err
}
func (s *Store) PutTool(id string, value []byte) error {
	parsed, err := httptool.Validate(value)
	if err != nil || parsed != id || !validID(id) {
		return errors.New("invalid tool")
	}
	nonce, enc, err := s.seal(value, "tool", id)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO custom_http_tools VALUES(?,1,?,?,?) ON CONFLICT(id) DO UPDATE SET cipher_version=excluded.cipher_version,nonce=excluded.nonce,ciphertext=excluded.ciphertext,updated_at=excluded.updated_at`, id, nonce, enc, time.Now().UnixMilli())
	return err
}
func (s *Store) GetTool(id string) ([]byte, error) {
	var version int
	var nonce, enc []byte
	if err := s.db.QueryRow(`SELECT cipher_version,nonce,ciphertext FROM custom_http_tools WHERE id=?`, id).Scan(&version, &nonce, &enc); err != nil {
		return nil, err
	}
	return s.unseal(version, nonce, enc, "tool", id)
}
func (s *Store) DeleteTool(id string) error {
	_, err := s.db.Exec(`DELETE FROM custom_http_tools WHERE id=?`, id)
	return err
}

// ToolIDs returns only identifiers; encrypted tool contents are never printed.
func (s *Store) ToolIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM custom_http_tools ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// OpenReadOnly opens only an already-checkpointed store, without creating any
// key, database or SQLite sidecar. A live WAL is refused instead of ignored.
func OpenReadOnly(dir string) (*Store, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe data directory")
	}
	unlock, err := lockDirectory(dir, false)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			unlock()
		}
	}()
	keyPath, dbPath := filepath.Join(dir, "master.key"), filepath.Join(dir, "data.sqlite")
	info, err = os.Lstat(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrMissingKey
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 32 {
		return nil, errors.New("invalid master key")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	info, err = os.Lstat(dbPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe database")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(dbPath + suffix); err == nil {
			return nil, errors.New("credential database requires checkpoint")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	uri := (&url.URL{Scheme: "file", Path: dbPath}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, aead: aead, unlock: unlock}
	var nonce, enc []byte
	if err = db.QueryRow(`SELECT nonce,ciphertext FROM master_key_check WHERE id=1`).Scan(&nonce, &enc); err != nil {
		db.Close()
		return nil, err
	}
	plain, err := store.unseal(1, nonce, enc, "master-key-check")
	if err != nil || string(plain) != "serverside-key-check-v1" {
		db.Close()
		return nil, errors.New("invalid master key")
	}
	success = true
	return store, nil
}
