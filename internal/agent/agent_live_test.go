package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRealCodexAppServerStartup(t *testing.T) {
	binary := os.Getenv("SERVER_TEST_CODEX_BIN")
	if binary == "" {
		t.Skip("set SERVER_TEST_CODEX_BIN to run the native Codex test")
	}
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	d, err := m.Open(ctx, Spec{Type: Codex, Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Running {
		t.Fatal("Codex app-server exited")
	}
	if err := m.Close(d.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRealClaudeStructuredStartup(t *testing.T) {
	binary := os.Getenv("SERVER_TEST_CLAUDE_BIN")
	if binary == "" {
		t.Skip("set SERVER_TEST_CLAUDE_BIN to run the native Claude test")
	}
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	d, err := m.Open(ctx, Spec{Type: Claude, Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	state, err := m.Status(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running {
		t.Fatal("Claude process exited during structured initialization")
	}
	if err := m.Close(d.ID); err != nil {
		t.Fatal(err)
	}
}
