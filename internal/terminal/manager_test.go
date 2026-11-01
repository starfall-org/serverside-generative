package terminal

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func readUntil(t *testing.T, m *Manager, id string, offset uint64, needle string) (string, uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var collected strings.Builder
	for time.Now().Before(deadline) {
		r, err := m.Read(id, offset, MaxReadBytes, 200)
		if err != nil {
			t.Fatal(err)
		}
		if r.NextOffset < offset {
			t.Fatalf("cursor regressed %d < %d", r.NextOffset, offset)
		}
		collected.WriteString(r.Text)
		offset = r.NextOffset
		if strings.Contains(collected.String(), needle) {
			return collected.String(), offset
		}
	}
	t.Fatalf("terminal output did not contain %q; got %.400s", needle, collected.String())
	return "", 0
}
func TestCreateExecuteReadClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test uses Unix shell")
	}
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	id, err := m.Create(24, 80)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("missing terminal ID")
	}
	if len(m.List()) != 1 {
		t.Fatal("terminal missing from list")
	}
	start, err := m.Read(id, 0, 4096, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Execute(id, "printf '__FIRST_TERMINAL_MARKER__\\n'", 100)
	if err != nil {
		t.Fatal(err)
	}
	_, cursor := readUntil(t, m, id, start.NextOffset, "__FIRST_TERMINAL_MARKER__")
	_, err = m.Execute(id, "printf '__SECOND_TERMINAL_MARKER__\\n'", 100)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = readUntil(t, m, id, cursor, "__SECOND_TERMINAL_MARKER__")
	if err = m.Close(id); err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 {
		t.Fatal("closed terminal still in list")
	}
	if _, err = m.Read(id, 0, 4096, 0); err == nil {
		t.Fatal("read closed terminal succeeded")
	}
	if _, err = m.Execute(id, "echo never", 0); err == nil {
		t.Fatal("execute closed terminal succeeded")
	}
}
func TestParallelTerminalsAndCapacity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test uses Unix shell")
	}
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	ids := make([]string, MaxTerminals)
	for i := range ids {
		ids[i], err = m.Create(24, 80)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = m.Create(24, 80); err == nil {
		t.Fatal("max active terminals not enforced")
	}
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			token := fmt.Sprintf("parallel-marker-%d", i)
			if _, err := m.Execute(id, "printf '"+token+"\\n'", 150); err != nil {
				t.Error(err)
				return
			}
			readUntil(t, m, id, 0, token)
		}(i, id)
	}
	wg.Wait()
	if err = m.Close(ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Create(24, 80); err != nil {
		t.Fatal("capacity not returned after close:", err)
	}
}
func TestTerminalBoundsAndNonPersistent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test uses Unix shell")
	}
	cwd := t.TempDir()
	m, err := NewManager(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	if _, err := m.Create(1, 1); err == nil {
		t.Fatal("bad dimensions accepted")
	}
	id, err := m.Create(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"", "x\x00y", strings.Repeat("x", MaxCommandBytes+1)} {
		if _, err := m.Execute(id, command, 0); err == nil {
			t.Fatalf("invalid command accepted len=%d", len(command))
		}
	}
	if _, err := m.Execute(id, "printf '__NON_PERSISTENT_OUTPUT__\\n'", 100); err != nil {
		t.Fatal(err)
	}
	_, _ = readUntil(t, m, id, 0, "__NON_PERSISTENT_OUTPUT__")
	contents, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) > 0 {
		t.Fatalf("terminal output unexpectedly wrote to cwd: %+v", contents)
	}
}
func TestTerminalReadBufferBound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test uses Unix shell")
	}
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	id, err := m.Create(24, 80)
	if err != nil {
		t.Fatal(err)
	}
	// Read result truncation works even if the process produced excessive output.
	if _, err := m.Execute(id, "head -c 350000 /dev/zero | tr '\\000' x", 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := m.Read(id, 0, MaxReadBytes, 300)
		if err != nil {
			t.Fatal(err)
		}
		if r.Truncated {
			if r.NextOffset <= r.Offset {
				t.Fatal("truncated read returned nothing")
			}
			if len(r.DataBase64) > MaxReadBytes*2 {
				t.Fatal("read exceeded output bound")
			}
			return
		}
	}
	t.Fatal("output buffer did not enforce 256 KiB limit")
}
