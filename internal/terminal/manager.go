package terminal

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/starfall-org/serverside-generative/internal/terminal/terminalid"
)

const (
	MaxBufferBytes  = 256 << 10
	MaxReadBytes    = 64 << 10
	MaxCommandBytes = 16 << 10
	MaxTerminals    = 8
	MaxWaitMS       = 3000
	IdleTimeout     = 30 * time.Minute
)

type Manager struct {
	mu      sync.Mutex
	terms   map[string]*terminal
	cwd     string
	stopped bool
}
type terminal struct {
	id       string
	cmd      *exec.Cmd
	pty      io.ReadWriteCloser
	writeMu  sync.Mutex
	mu       sync.Mutex
	output   []byte
	floor    uint64
	next     uint64
	running  bool
	exitCode *int
	updates  chan struct{}
	done     chan struct{}
	readDone chan struct{}
	lastUse  time.Time
}
type ReadResult struct {
	Type       string `json:"type"`
	TerminalID string `json:"terminal_id"`
	Offset     uint64 `json:"offset"`
	NextOffset uint64 `json:"next_offset"`
	Truncated  bool   `json:"truncated"`
	Running    bool   `json:"running"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Text       string `json:"text"`
	DataBase64 string `json:"data_base64"`
}
type Descriptor struct {
	ID      string `json:"terminal_id"`
	Running bool   `json:"running"`
	Offset  uint64 `json:"next_offset"`
}

func NewManager(cwd string) (*Manager, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("terminal working directory is not a directory")
	}
	return &Manager{terms: make(map[string]*terminal), cwd: abs}, nil
}
func (m *Manager) get(id string) (*terminal, error) {
	if id == "" || len(id) > 128 {
		return nil, errors.New("invalid terminal_id")
	}
	m.mu.Lock()
	t := m.terms[id]
	m.mu.Unlock()
	if t == nil {
		return nil, errors.New("terminal not found or already closed")
	}
	t.mu.Lock()
	t.lastUse = time.Now()
	t.mu.Unlock()
	return t, nil
}
func (m *Manager) Create(rows, cols int) (string, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	if rows < 10 || rows > 200 || cols < 20 || cols > 400 {
		return "", errors.New("invalid terminal dimensions")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return "", errors.New("terminal manager stopped")
	}
	// Reap old idle terminals before enforcing the quota.
	for id, t := range m.terms {
		t.mu.Lock()
		stale := !t.running && time.Since(t.lastUse) > IdleTimeout
		t.mu.Unlock()
		if stale {
			delete(m.terms, id)
			go t.close()
		}
	}
	if len(m.terms) >= MaxTerminals {
		return "", fmt.Errorf("terminal capacity reached (%d)", MaxTerminals)
	}
	id, err := terminalid.New()
	if err != nil {
		return "", err
	}
	cmd, stream, err := startShell(m.cwd, rows, cols)
	if err != nil {
		return "", err
	}
	t := &terminal{id: id, cmd: cmd, pty: stream, updates: make(chan struct{}, 1), done: make(chan struct{}), readDone: make(chan struct{}), running: true, lastUse: time.Now()}
	m.terms[id] = t
	go t.readLoop()
	go t.waitLoop()
	return id, nil
}
func (t *terminal) notifyLocked() {
	select {
	case t.updates <- struct{}{}:
	default:
	}
}
func (t *terminal) readLoop() {
	defer close(t.readDone)
	defer func() {
		t.mu.Lock()
		t.notifyLocked()
		t.mu.Unlock()
	}()
	buf := make([]byte, 8192)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.output = append(t.output, buf[:n]...)
			t.next += uint64(n)
			if len(t.output) > MaxBufferBytes {
				dropped := len(t.output) - MaxBufferBytes
				copy(t.output, t.output[dropped:])
				t.output = t.output[:MaxBufferBytes]
				t.floor += uint64(dropped)
			}
			t.notifyLocked()
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
func (t *terminal) waitLoop() {
	err := t.cmd.Wait()
	// On Unix PTYs the reader normally drains all trailing output and sees EIO.
	// Allow it to do so before closing the master; Windows pipe mode may need
	// an explicit close to unblock the reader.
	select {
	case <-t.readDone:
	case <-time.After(150 * time.Millisecond):
	}
	_ = t.pty.Close()
	<-t.readDone
	t.mu.Lock()
	code := 0
	if err != nil {
		if t.cmd.ProcessState != nil {
			code = t.cmd.ProcessState.ExitCode()
		} else {
			code = -1
		}
	}
	t.exitCode = &code
	t.running = false
	t.notifyLocked()
	t.mu.Unlock()
	close(t.done)
}
func (t *terminal) snapshot(offset uint64, maxBytes int) ReadResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := ReadResult{Type: "terminal_output", TerminalID: t.id, Running: t.running, ExitCode: t.exitCode}
	if offset < t.floor {
		result.Truncated = true
		offset = t.floor
	}
	if offset > t.next {
		offset = t.next
	}
	pos := int(offset - t.floor)
	length := len(t.output) - pos
	if length > maxBytes {
		length = maxBytes
	}
	data := append([]byte(nil), t.output[pos:pos+length]...)
	result.Offset = offset
	result.NextOffset = offset + uint64(length)
	result.Text = strings.ToValidUTF8(string(data), "\uFFFD")
	result.DataBase64 = base64.StdEncoding.EncodeToString(data)
	return result
}
func (t *terminal) Read(offset uint64, maxBytes, waitMS int) ReadResult {
	if maxBytes <= 0 {
		maxBytes = MaxReadBytes
	}
	if maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > MaxWaitMS {
		waitMS = MaxWaitMS
	}
	result := t.snapshot(offset, maxBytes)
	if waitMS == 0 || !result.Running || result.Truncated || result.NextOffset > offset {
		return result
	}
	timer := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-t.updates:
	case <-t.done:
	case <-timer.C:
	}
	return t.snapshot(offset, maxBytes)
}
func (m *Manager) Execute(id, command string, waitMS int) (ReadResult, error) {
	if command == "" || len(command) > MaxCommandBytes || !utf8.ValidString(command) || strings.ContainsRune(command, '\x00') {
		return ReadResult{}, errors.New("invalid or oversized terminal command")
	}
	t, err := m.get(id)
	if err != nil {
		return ReadResult{}, err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return ReadResult{}, errors.New("terminal process has exited")
	}
	start := t.next
	t.mu.Unlock()
	if _, err = io.WriteString(t.pty, command+"\n"); err != nil {
		return ReadResult{}, errors.New("terminal write failed")
	}
	result := t.Read(start, MaxReadBytes, waitMS)
	result.Type = "terminal_executed"
	return result, nil
}
func (m *Manager) Read(id string, offset uint64, maxBytes, waitMS int) (ReadResult, error) {
	t, err := m.get(id)
	if err != nil {
		return ReadResult{}, err
	}
	return t.Read(offset, maxBytes, waitMS), nil
}
func (m *Manager) List() []Descriptor {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]Descriptor, 0, len(m.terms))
	for _, t := range m.terms {
		t.mu.Lock()
		res = append(res, Descriptor{ID: t.id, Running: t.running, Offset: t.next})
		t.mu.Unlock()
	}
	return res
}
func (t *terminal) close() {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	stopShell(t.cmd)
	_ = t.pty.Close()
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
}
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	t := m.terms[id]
	if t != nil {
		delete(m.terms, id)
	}
	m.mu.Unlock()
	if t == nil {
		return errors.New("terminal not found or already closed")
	}
	t.close()
	return nil
}
func (m *Manager) CloseAll() {
	m.mu.Lock()
	m.stopped = true
	terms := m.terms
	m.terms = make(map[string]*terminal)
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range terms {
		wg.Add(1)
		go func(t *terminal) { defer wg.Done(); t.close() }(t)
	}
	wg.Wait()
}
