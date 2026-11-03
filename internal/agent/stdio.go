package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type stdioProcess struct {
	session   *Session
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	mu        sync.Mutex
	next      uint64
	waiting   map[uint64]chan rpcReply
	writeMu   sync.Mutex
	done      chan struct{}
	onMessage func(map[string]json.RawMessage, json.RawMessage)
}

func newProcess(s *Session, args []string, onMessage func(map[string]json.RawMessage, json.RawMessage)) *stdioProcess {
	cmd := exec.Command(s.binary, args...)
	cmd.Dir = s.cwd
	// External CLIs authenticate from their normal local credentials.
	// Do not disclose the serverside pairing/handshake password.
	env := os.Environ()
	cmd.Env = make([]string, 0, len(env))
	for _, line := range env {
		if strings.HasPrefix(line, "GENERATIVE_HANDSHAKE_PASSWORD=") {
			continue
		}
		cmd.Env = append(cmd.Env, line)
	}
	configureProcess(cmd)
	return &stdioProcess{session: s, cmd: cmd, waiting: make(map[uint64]chan rpcReply), done: make(chan struct{}), onMessage: onMessage}
}

func (p *stdioProcess) start() error {
	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err = p.cmd.Start(); err != nil {
		return err
	}
	// The transport never merges stderr into a JSON protocol stream. Drain
	// without forwarding it: native CLIs sometimes log credentials/errors.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go p.readLoop(stdout)
	go func() {
		err := p.cmd.Wait()
		close(p.done)
		p.mu.Lock()
		for id, ch := range p.waiting {
			delete(p.waiting, id)
			select {
			case ch <- rpcReply{Error: &rpcError{Message: "agent process exited"}}:
			default:
			}
		}
		p.mu.Unlock()
		if err != nil {
			p.session.publish("error", "", "", "", "agent_process_exited", "failed", nil)
		}
		p.session.markExited()
	}()
	return nil
}

func (p *stdioProcess) readLoop(out io.Reader) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var msg map[string]json.RawMessage
		if json.Unmarshal(line, &msg) != nil {
			// Never treat arbitrary CLI banners as model content.
			continue
		}
		if id, ok := msg["id"]; ok && msg["method"] == nil {
			var idNum uint64
			if err := json.Unmarshal(id, &idNum); err == nil {
				var reply rpcReply
				if json.Unmarshal(line, &reply) == nil {
					p.mu.Lock()
					ch := p.waiting[idNum]
					delete(p.waiting, idNum)
					p.mu.Unlock()
					if ch != nil {
						select {
						case ch <- reply:
						default:
						}
					}
					continue
				}
			}
		}
		if p.onMessage != nil {
			p.onMessage(msg, line)
		}
	}
	if err := scanner.Err(); err != nil {
		p.session.publish("error", "", "", "", "agent_protocol_read_failed", "failed", nil)
	}
}

func (p *stdioProcess) write(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > maxBytes {
		return errors.New("agent command too large")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	raw = append(raw, '\n')
	_, err = p.stdin.Write(raw)
	return err
}

func (p *stdioProcess) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.next++
	id := p.next
	ch := make(chan rpcReply, 1)
	p.waiting[id] = ch
	p.mu.Unlock()
	if err := p.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		if reply.Error != nil {
			return nil, fmt.Errorf("agent protocol %s failed (%d): %s", method, reply.Error.Code, reply.Error.Message)
		}
		return reply.Result, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("agent process exited")
	}
}

func (p *stdioProcess) call(method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.request(ctx, method, params)
}

func (p *stdioProcess) stop() error {
	killProcess(p.cmd)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// The protocol's request IDs are opaque. Preserve their JSON representation
// exactly: both numeric and string IDs can occur.
func approvalKey(id json.RawMessage) string {
	return string(id)
}

func mustJSONNumber(s string) (json.RawMessage, error) {
	if len(s) == 0 || len(s) > 128 {
		return nil, errors.New("invalid request id")
	}
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return json.RawMessage(s), nil
	}
	if len(s) > 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var text string
		if json.Unmarshal([]byte(s), &text) == nil {
			return json.RawMessage(s), nil
		}
	}
	return nil, errors.New("invalid JSON-RPC request id")
}
