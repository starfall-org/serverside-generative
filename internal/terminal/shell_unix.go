//go:build !windows

package terminal

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

func scrubEnvironment(env []string) []string {
	result := make([]string, 0, len(env)+3)
	for _, line := range env {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if upper == "HISTFILE" || upper == "HISTSIZE" || upper == "HISTFILESIZE" || upper == "GENERATIVE_HANDSHAKE_PASSWORD" || strings.Contains(upper, "API_KEY") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") {
			continue
		}
		result = append(result, line)
	}
	return append(result, "HISTFILE=/dev/null", "HISTSIZE=0", "TERM=xterm-256color")
}

func startShell(cwd string, rows, cols int) (*exec.Cmd, io.ReadWriteCloser, error) {
	shell := "/bin/sh"
	if v := os.Getenv("SERVERSIDE_TERMINAL_SHELL"); v != "" {
		shell = v
	}
	cmd := exec.Command(shell, "-i")
	cmd.Dir = cwd
	cmd.Env = scrubEnvironment(os.Environ())
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, nil, err
	}
	return cmd, master, nil
}
func stopShell(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// PTY starts in a new session, so terminate the entire shell process group
	// including children started by the terminal, not only the shell.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
