//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/starfall-org/serverside-generative/internal/configdir"
)

const socketPollInterval = 100 * time.Millisecond

func missingSocket(err error) bool {
	return errors.Is(err, os.ErrNotExist) || stopped(err)
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func tryLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func lockService(ctx context.Context, dir string) (*os.File, error) {
	for ctx.Err() == nil {
		lock, err := tryLock(filepath.Join(dir, "commands.lock"))
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return lock, err
		}
		if err := pause(ctx, socketPollInterval); err != nil {
			return nil, err
		}
	}
	return nil, ctx.Err()
}

func tryDaemonLock() (*os.File, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	// Keep the lock outside the data and identity directories so it also
	// coordinates custom paths and survives moving the legacy directories.
	return tryLock(filepath.Join(base, "."+configdir.Name+"-daemon.lock"))
}

// LockDaemon reserves the daemon slot for this user configuration directory.
// The caller holds it until all listeners and server state have been closed.
func LockDaemon() (io.Closer, error) {
	lock, err := tryDaemonLock()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, errors.New("Generative daemon already running or starting")
	}
	if err != nil {
		return nil, err
	}
	return lock, nil
}

// Connect opens the requested default socket, starting the configured service
// if needed. The dial function must only connect and must not send a request.
func Connect(ctx context.Context, path, alternate string, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	conn, err := dial(ctx, path)
	if !missingSocket(err) {
		return conn, err
	}
	conn, err = connectOrStart(ctx, path, alternate, dial)
	if conn != nil || err != nil {
		return conn, err
	}
	for ctx.Err() == nil {
		conn, err := dial(ctx, path)
		if !missingSocket(err) {
			return conn, err
		}
		if err := pause(ctx, socketPollInterval); err != nil {
			break
		}
	}
	return nil, fmt.Errorf("wait for daemon socket %s: %w", path, ctx.Err())
}

// Hold the service configuration lock only while deciding whether to start.
func connectOrStart(ctx context.Context, path, alternate string, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	dir, err := dirPath()
	if err != nil {
		return nil, err
	}
	lock, err := lockService(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("wait for service setup: %w", err)
	}
	defer lock.Close()

	conn, err := dial(ctx, path)
	if !missingSocket(err) {
		return conn, err
	}
	running, err := daemonRunning(ctx, alternate)
	if err != nil {
		return nil, err
	}
	if !running {
		output := commandIO{ctx: ctx, out: os.Stderr, err: os.Stderr}
		if err := output.ensureStarted(dir); err != nil {
			return nil, fmt.Errorf("start Generative service: %w", err)
		}
	}
	return nil, nil
}

func daemonRunning(ctx context.Context, alternate string) (bool, error) {
	lock, err := tryDaemonLock()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	lock.Close()

	// Older daemons did not hold the daemon lock. The other socket can
	// still show that a daemon is serving while the requested socket is absent.
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", alternate)
	if err != nil {
		if missingSocket(err) {
			return false, nil
		}
		return false, fmt.Errorf("check daemon socket %s: %w", alternate, err)
	}
	conn.Close()
	return true, nil
}

func (output commandIO) ensureStarted(dir string) error {
	c, err := load(dir)
	if err == nil {
		return output.action(dir, c, "start")
	}
	if !os.IsNotExist(err) {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	c = config{Backend: autoBackend(output.ctx), Executable: exe, CWD: cwd}
	if err := output.ctx.Err(); err != nil {
		return err
	}
	if err := validate(c); err != nil {
		return err
	}
	var definition string
	switch c.Backend {
	case "systemd":
		definition = unitPath()
	case "launchd":
		definition = plistPath()
	}
	if definition != "" {
		if _, err := os.Lstat(definition); err == nil {
			return fmt.Errorf("existing service definition at %s without Generative service configuration", definition)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return output.install(dir, c)
}
