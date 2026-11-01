//go:build windows

package terminal

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type pipeTerminal struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	stdin  io.WriteCloser
	once   sync.Once
}

func (s *pipeTerminal) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *pipeTerminal) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *pipeTerminal) Close() error {
	s.once.Do(func() { _ = s.stdin.Close(); _ = s.writer.Close(); _ = s.reader.Close() })
	return nil
}
func startShell(cwd string, rows, cols int) (*exec.Cmd, io.ReadWriteCloser, error) {
	cmd := exec.Command("cmd.exe", "/Q")
	cmd.Dir = cwd
	env := make([]string, 0)
	for _, line := range os.Environ() {
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(k)
		if upper == "GENERATIVE_HANDSHAKE_PASSWORD" || strings.Contains(upper, "API_KEY") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") {
			continue
		}
		env = append(env, line)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	r, w := io.Pipe()
	cmd.Stdout = w
	cmd.Stderr = w
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = r.Close()
		_ = w.Close()
		return nil, nil, err
	}
	return cmd, &pipeTerminal{reader: r, writer: w, stdin: stdin}, nil
}
func stopShell(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
