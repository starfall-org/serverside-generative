package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type openCodeBackend struct {
	s          *Session
	cmd        *exec.Cmd
	password   string
	addr       string
	apiV2      bool
	client     *http.Client
	mu         sync.Mutex
	sessionID  string
	approvals  map[string]bool
	parts      map[string]string
	done       chan struct{}
	eventsStop context.CancelFunc
}

func newOpenCode(s *Session) *openCodeBackend {
	return &openCodeBackend{s: s, done: make(chan struct{}), client: &http.Client{Timeout: 10 * time.Second}, approvals: map[string]bool{}, parts: map[string]string{}}
}

func (b *openCodeBackend) start(ctx context.Context) error {
	// Bind localhost exclusively; never expose OpenCode directly to the network.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	b.password = hex.EncodeToString(token)
	b.addr = "http://127.0.0.1:" + strconv.Itoa(port)
	cmd := exec.Command(b.s.binary, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Dir = b.s.cwd
	configureProcess(cmd)
	for _, line := range os.Environ() {
		if strings.HasPrefix(line, "GENERATIVE_HANDSHAKE_PASSWORD=") || strings.HasPrefix(line, "OPENCODE_SERVER_PASSWORD=") || strings.HasPrefix(line, "OPENCODE_SERVER_USERNAME=") || strings.HasPrefix(line, "OPENCODE_PASSWORD=") {
			continue
		}
		cmd.Env = append(cmd.Env, line)
	}
	cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+b.password, "OPENCODE_SERVER_USERNAME=opencode", "OPENCODE_PASSWORD="+b.password)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	b.cmd = cmd
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go func() {
		err := cmd.Wait()
		close(b.done)
		if err != nil {
			b.s.publish("error", "", "", "", "agent_process_exited", "failed", nil)
		}
		b.s.markExited()
	}()
	for {
		var info map[string]any
		if err := b.httpJSON(ctx, "GET", "/api/info", nil, &info); err == nil && info["version"] != nil {
			b.apiV2 = true
			break
		}
		var health map[string]any
		if err := b.httpJSON(ctx, "GET", "/global/health", nil, &health); err == nil && health["healthy"] == true {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("opencode startup: %w", ctx.Err())
		case <-b.done:
			return errors.New("opencode server exited before becoming ready")
		case <-time.After(80 * time.Millisecond):
		}
	}
	var session struct {
		ID   string `json:"id"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	path := "/session"
	if b.apiV2 {
		path = "/api/session"
	}
	if err := b.httpJSON(ctx, "POST", path, map[string]any{}, &session); err != nil {
		return err
	}
	id := session.ID
	if b.apiV2 {
		id = session.Data.ID
	}
	if id == "" {
		return errors.New("opencode returned no session id")
	}
	b.mu.Lock()
	b.sessionID = id
	b.mu.Unlock()
	// Startup uses a short-lived timeout; the SSE loop must outlive that
	// timeout while retaining context values and its own close cancellation.
	eventCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.eventsStop = cancel
	go b.stream(eventCtx)
	return nil
}

func (b *openCodeBackend) httpJSON(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.addr+path, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", b.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Do not forward arbitrary provider error bodies: they can contain
		// authorization headers, remote details or confidential prompts.
		return fmt.Errorf("opencode API %s returned HTTP %d", path, resp.StatusCode)
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(result)
}

func (b *openCodeBackend) sessionPath(suffix string) string {
	b.mu.Lock()
	id := b.sessionID
	b.mu.Unlock()
	if b.apiV2 {
		return "/api/session/" + url.PathEscape(id) + suffix
	}
	return "/session/" + url.PathEscape(id) + suffix
}
func (b *openCodeBackend) send(prompt string) error {
	b.s.setBusy(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := b.sessionPath("/prompt_async")
	body := map[string]any{"parts": []any{map[string]any{"type": "text", "text": prompt}}}
	if b.apiV2 {
		path = b.sessionPath("/prompt")
		body = map[string]any{"text": prompt, "delivery": "queue"}
	}
	err := b.httpJSON(ctx, "POST", path, body, nil)
	if err != nil {
		b.s.setBusy(false)
	}
	return err
}
func (b *openCodeBackend) steer(prompt string) error {
	if b.apiV2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return b.httpJSON(ctx, "POST", b.sessionPath("/prompt"),
			map[string]any{"text": prompt, "delivery": "steer"}, nil)
	}
	// Legacy API accepts a second asynchronous prompt, but native
	// steer semantics depend on the OpenCode version.
	return b.send(prompt)
}
func (b *openCodeBackend) interrupt() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if b.apiV2 {
		return b.httpJSON(ctx, "POST", b.sessionPath("/interrupt"), map[string]any{}, nil)
	}
	return b.httpJSON(ctx, "POST", b.sessionPath("/abort"), map[string]any{}, nil)
}
func (b *openCodeBackend) respond(id, decision string) error {
	b.mu.Lock()
	pending := b.approvals[id]
	b.mu.Unlock()
	if !pending {
		return errors.New("opencode approval not found or already answered")
	}
	response := "reject"
	switch decision {
	case "once", "accept":
		response = "once"
	case "always", "acceptForSession":
		response = "always"
	case "reject", "decline", "cancel":
		response = "reject"
	default:
		return errors.New("unsupported OpenCode permission response")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := b.sessionPath("/permissions/" + url.PathEscape(id))
	body := map[string]any{"response": response}
	if b.apiV2 {
		path = b.sessionPath("/permission/" + url.PathEscape(id) + "/reply")
		body = map[string]any{"decision": response}
	}
	err := b.httpJSON(ctx, "POST", path, body, nil)
	if err == nil {
		b.mu.Lock()
		delete(b.approvals, id)
		b.mu.Unlock()
	}
	return err
}
func (b *openCodeBackend) close() error {
	if b.eventsStop != nil {
		b.eventsStop()
	}
	if b.cmd != nil {
		killProcess(b.cmd)
		select {
		case <-b.done:
		case <-time.After(3 * time.Second):
		}
	}
	return nil
}

func (b *openCodeBackend) stream(ctx context.Context) {
	for ctx.Err() == nil {
		path := "/event"
		if b.apiV2 {
			path = "/api/event"
		}
		req, err := http.NewRequestWithContext(ctx, "GET", b.addr+path, nil)
		if err != nil {
			return
		}
		req.SetBasicAuth("opencode", b.password)
		req.Header.Set("Accept", "text/event-stream")
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			b.s.publish("error", "", "", "", "opencode_event_stream_failed", "failed", nil)
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		var data strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				// SSE permits multiple data lines per event.
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			} else if line == "" {
				if data.Len() > 0 {
					b.onEvent(json.RawMessage(data.String()))
					data.Reset()
				}
			}
		}
		_ = resp.Body.Close()
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (b *openCodeBackend) onEvent(raw json.RawMessage) {
	if b.apiV2 {
		b.onV2Event(raw)
		return
	}
	var value struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return
	}
	props := value.Properties
	sessionID := getFieldString(props, "sessionID")
	b.mu.Lock()
	own := b.sessionID
	b.mu.Unlock()
	if sessionID != "" && sessionID != own {
		return
	}
	switch value.Type {
	case "message.part.delta":
		block := getFieldString(props, "partID")
		delta := getFieldString(props, "delta")
		field := getFieldString(props, "field")
		kind := "content"
		b.mu.Lock()
		partType := b.parts[block]
		b.mu.Unlock()
		if partType == "reasoning" || field == "reasoning" || field == "thinking" {
			kind = "thinking"
		}
		b.s.publish(kind, block, "", "", delta, "streaming", raw)
	case "message.part.updated":
		part := getField(props, "part")
		partType := getFieldString(part, "type")
		id := getFieldString(part, "id")
		b.mu.Lock()
		if id != "" {
			b.parts[id] = partType
		}
		b.mu.Unlock()
		switch partType {
		case "text":
			b.s.publish("content", id, "", "", getFieldString(part, "text"), "updated", raw)
		case "reasoning":
			b.s.publish("thinking", id, "", "", getFieldString(part, "text"), "updated", raw)
		case "tool":
			callID := getFieldString(part, "callID")
			state := getField(part, "state")
			status := getFieldString(state, "status")
			kind := "tool_call"
			if status == "completed" || status == "error" {
				kind = "tool_result"
			}
			b.s.publish(kind, id, callID, "", getFieldString(part, "tool"), status, raw)
		default:
			b.s.publish("status", id, "", "", partType, "", raw)
		}
	case "permission.asked", "permission.updated":
		id := getFieldString(props, "id")
		b.mu.Lock()
		if id != "" {
			b.approvals[id] = true
		}
		b.mu.Unlock()
		b.s.publish("approval", "", getFieldString(props, "toolCallID"), id, "", "pending", raw)
	case "session.status":
		status := getFieldString(getField(props, "status"), "type")
		b.s.setBusy(status == "busy")
		if status == "idle" {
			b.s.publish("completed", "", "", "", "", "completed", raw)
		}
		b.s.publish("status", "", "", "", status, "", raw)
	case "session.idle":
		b.s.setBusy(false)
		b.s.publish("completed", "", "", "", "", "completed", raw)
	case "session.error":
		b.s.setBusy(false)
		b.s.publish("error", "", "", "", "opencode_session_error", "failed", raw)
	default:
		b.s.publish("status", "", "", "", value.Type, "", raw)
	}
}
