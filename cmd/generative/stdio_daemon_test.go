//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("GENERATIVE_STDIO_TEST_CLI") == "1" {
		if len(os.Args) == 2 && os.Args[1] == "serve" {
			os.Args = append(os.Args, "--tcp-listen", "127.0.0.1:0", "--http-listen", "127.0.0.1:0")
		}
		main()
		return
	}
	os.Exit(m.Run())
}

func TestStdioDaemonLifecycle(t *testing.T) {
	base := t.TempDir()
	identity := filepath.Join(base, "generative", "identity")
	config := filepath.Join(identity, "service", "config.json")
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("GENERATIVE_STDIO_TEST_CLI", "1")
	t.Setenv("GENERATIVE_HANDSHAKE_PASSWORD", "")
	// Exclude host systemctl so the test cannot touch a real service manager.
	t.Setenv("PATH", base)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		stdout []byte
		stderr string
		err    error
	}
	call := func(input []byte, args ...string) result {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Env = cliEnv()
		cmd.Dir = base
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		return result{out, stderr.String(), err}
	}
	requireOK := func(r result) []byte {
		t.Helper()
		if r.err != nil {
			logs, _ := os.ReadFile(filepath.Join(identity, "service", "daemon.log"))
			t.Fatalf("CLI failed: %v\nstdout: %s\nstderr: %s\ndaemon: %s", r.err, r.stdout, r.stderr, logs)
		}
		return r.stdout
	}
	pairArgs := []string{"pair", "--stdio", "--transport", "tcp"}
	streamArgs := []string{"stream", "--stdio"}

	for _, args := range [][]string{
		append(append([]string{}, pairArgs...), "--ipc", filepath.Join(base, "missing.sock")),
		append(append([]string{}, streamArgs...), "--data-socket", filepath.Join(base, "missing.sock")),
	} {
		if r := call(nil, args...); r.err == nil || len(r.stdout) != 0 {
			t.Fatalf("missing custom socket: %+v", r)
		}
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatalf("custom socket triggered service setup: %v", err)
	}
	t.Run("lost pairing response", func(t *testing.T) {
		if err := os.MkdirAll(identity, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(identity, "control.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		results := make(chan result, 1)
		go func() { results <- call(nil, pairArgs...) }()
		_ = listener.SetDeadline(time.Now().Add(3 * time.Second))
		conn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		var request map[string]string
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		err = json.NewDecoder(conn).Decode(&request)
		conn.Close()
		if err != nil || request["method"] != "pair_tcp" {
			t.Fatalf("unexpected control request: %v (%v)", request, err)
		}
		_ = listener.SetDeadline(time.Now().Add(250 * time.Millisecond))
		if replay, err := listener.AcceptUnix(); err == nil {
			replay.Close()
			listener.Close()
			t.Fatal("pairing made another connection after the request was sent")
		}
		if r := <-results; r.err == nil || len(r.stdout) != 0 {
			t.Fatalf("pairing should report the lost response: %+v", r)
		}
	})
	t.Cleanup(func() {
		if _, err := os.Stat(config); err == nil {
			requireOK(call(nil, "service", "uninstall"))
		}
	})

	// Cold pairing and streaming race to register and start the same service.
	const pairCalls = 4
	pairs := make(chan result, pairCalls)
	stream := make(chan result, 1)
	go func() { stream <- call(nil, streamArgs...) }()
	for i := 0; i < pairCalls; i++ {
		go func() { pairs <- call(nil, pairArgs...) }()
	}
	type credentials struct {
		ID          string `json:"client_id"`
		Secret      string `json:"secret"`
		Fingerprint string `json:"fingerprint"`
		Endpoint    string `json:"endpoint"`
		Transport   string `json:"transport"`
	}
	var first credentials
	ids := make(map[string]bool)
	for i := 0; i < pairCalls; i++ {
		var c credentials
		raw := requireOK(<-pairs)
		if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" || c.Secret == "" ||
			c.Fingerprint == "" || c.Transport != "tcp" {
			t.Fatalf("unexpected pairing output: %q (%v)", raw, err)
		}
		if host, port, err := net.SplitHostPort(c.Endpoint); err != nil || host != "127.0.0.1" || port == "0" {
			t.Fatalf("unexpected pairing endpoint: %q (%v)", c.Endpoint, err)
		}
		if ids[c.ID] || (i > 0 && (c.Fingerprint != first.Fingerprint || c.Endpoint != first.Endpoint)) {
			t.Fatalf("inconsistent pairing identity: %+v", c)
		}
		ids[c.ID] = true
		first = c
	}
	if stdout := requireOK(<-stream); len(stdout) != 0 {
		t.Fatalf("unexpected output on empty stream: %q", stdout)
	}

	auth := func() {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"type": "auth", "protocol": 5, "client_id": first.ID, "secret": first.Secret})
		if err != nil {
			t.Fatal(err)
		}
		frame := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
		stdout := requireOK(call(append(frame, payload...), streamArgs...))
		reader := bytes.NewReader(stdout)
		var size uint32
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil || int(size) != reader.Len() {
			t.Fatalf("stream did not return exactly one frame: %q", stdout)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Type     string `json:"type"`
			ID       string `json:"client_id"`
			Protocol int    `json:"protocol"`
		}
		if err := json.Unmarshal(body, &reply); err != nil || reply.Type != "paired" || reply.ID != first.ID || reply.Protocol != 5 {
			t.Fatalf("unexpected auth response: %q (%v)", body, err)
		}
	}
	status := requireOK(call(nil, "service", "status"))
	auth()
	if next := requireOK(call(nil, "service", "status")); !bytes.Equal(status, next) {
		t.Fatalf("running service changed: %s -> %s", status, next)
	}
	if r := call(nil, "serve", "--tcp-listen", "127.0.0.1:0", "--http-listen", "off"); r.err == nil ||
		!strings.Contains(r.stderr, "daemon already running") {
		t.Fatalf("duplicate manual daemon was not rejected: %+v", r)
	}
	requireOK(call(nil, "service", "uninstall"))
	requireOK(call(nil, "service", "install", "--backend", "native", "--",
		"--tcp-listen", "127.0.0.1:0", "--http-listen", "off"))
	status = requireOK(call(nil, "service", "status"))
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	requireOK(call(nil, "service", "stop"))
	auth() // starts the existing service and authenticates with its saved identity
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("auto-start changed the saved service configuration: %v", err)
	}
	if next := requireOK(call(nil, "service", "status")); bytes.Equal(status, next) {
		t.Fatalf("service did not restart after an explicit stop: %s", next)
	}
	requireOK(call(nil, "service", "uninstall"))

	// A foreground daemon with default sockets serves both commands without
	// being registered or restarted by either connection.
	manual := exec.Command(exe, "serve", "--tcp-listen", "127.0.0.1:0", "--http-listen", "off")
	manual.Env = cliEnv()
	if err := manual.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manual.Process.Signal(os.Interrupt)
		_ = manual.Wait()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		conn, err := dialPairSocket(ctx, filepath.Join(identity, "control.sock"))
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("manual daemon did not become ready: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	requireOK(call(nil, pairArgs...))
	auth()
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatalf("manual daemon triggered service registration: %v", err)
	}
}

func cliEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GENERATIVE_WEB_PASSWORD=") {
			env = append(env, entry)
		}
	}
	return env
}
