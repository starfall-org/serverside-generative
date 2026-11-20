//go:build linux || darwin

// Package service manages the per-user ServerSide daemon.
package service

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"github.com/starfall-org/serverside-generative/internal/configdir"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const unit = "serverside.service"
const label = "org.starfall.serverside"

type config struct {
	Backend    string
	Executable string
	Args       []string
	CWD        string
}
type status struct {
	PID      int
	ChildPID int
}

func dirPath() (string, error) {
	base, err := configdir.Root()
	return filepath.Join(base, "identity", "service"), err
}
func configPath(dir string) string { return filepath.Join(dir, "config.json") }
func logPath(dir string) string    { return filepath.Join(dir, "daemon.log") }
func sockPath(dir string) string   { return filepath.Join(dir, "supervisor.sock") }
func lockPath(dir string) string   { return filepath.Join(dir, "supervisor.lock") }
func save(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".service-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func load(dir string) (config, error) {
	var c config
	b, err := os.ReadFile(configPath(dir))
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, validate(c)
}
func validate(c config) error {
	if c.Backend != "native" && c.Backend != "systemd" && c.Backend != "launchd" {
		return errors.New("invalid service backend")
	}
	if !filepath.IsAbs(c.Executable) || !filepath.IsAbs(c.CWD) {
		return errors.New("service paths must be absolute")
	}
	for _, s := range append([]string{c.Executable, c.CWD}, c.Args...) {
		if strings.ContainsAny(s, "\x00\n\r") {
			return errors.New("NUL or newline in service argument")
		}
	}
	for _, s := range c.Args {
		if s == "--password" || s == "-password" || strings.HasPrefix(s, "--password=") || strings.HasPrefix(s, "-password=") {
			return errors.New("do not persist passwords in service arguments")
		}
	}
	return nil
}
func invoke(bin string, args ...string) error {
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
func autoBackend() string {
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil && exec.Command("systemctl", "--user", "show-environment").Run() == nil {
		return "systemd"
	}
	return "native"
}
func unitPath() string { d, _ := os.UserConfigDir(); return filepath.Join(d, "systemd", "user", unit) }
func plistPath() string {
	d, _ := os.UserHomeDir()
	return filepath.Join(d, "Library", "LaunchAgents", label+".plist")
}
func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func unitQuote(s string) string {
	return strconv.Quote(strings.NewReplacer("%", "%%", "$", "$$").Replace(s))
}
func unitFile(c config) string {
	args := []string{unitQuote(c.Executable), "serve"}
	for _, s := range c.Args {
		args = append(args, unitQuote(s))
	}
	return "[Unit]\nDescription=ServerSide Generative\nAfter=network.target\n[Service]\nType=simple\nExecStart=" + strings.Join(args, " ") + "\nWorkingDirectory=" + strconv.Quote(strings.ReplaceAll(c.CWD, "%", "%%")) + "\nRestart=always\nRestartSec=2\nUMask=0077\n[Install]\nWantedBy=default.target\n"
}
func escape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func plistFile(c config, log string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array>`)
	for _, a := range append([]string{c.Executable, "serve"}, c.Args...) {
		b.WriteString("<string>" + escape(a) + "</string>")
	}
	b.WriteString("</array><key>WorkingDirectory</key><string>" + escape(c.CWD) + "</string><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer><key>Umask</key><integer>63</integer><key>StandardOutPath</key><string>" + escape(log) + "</string><key>StandardErrorPath</key><string>" + escape(log) + "</string></dict></plist>\n")
	return b.String()
}

// Run handles service commands. Serve flags follow --.
func Run(args []string, exe string) error {
	if len(args) == 0 {
		return errors.New("use service install/start/stop/restart/status/logs/uninstall")
	}
	dir, err := dirPath()
	if err != nil {
		return err
	}
	if args[0] == "supervise" && len(args) == 1 {
		return supervise(dir)
	}
	if args[0] == "install" {
		f := flag.NewFlagSet("service install", flag.ContinueOnError)
		kind := f.String("backend", "auto", "auto, systemd, launchd, or native")
		if err = f.Parse(args[1:]); err != nil {
			return err
		}
		if *kind == "auto" {
			*kind = autoBackend()
		}
		if (*kind == "systemd" && runtime.GOOS != "linux") || (*kind == "launchd" && runtime.GOOS != "darwin") {
			return errors.New("backend unsupported on this OS")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		c := config{Backend: *kind, Executable: exe, Args: f.Args(), CWD: cwd}
		if err = validate(c); err != nil {
			return err
		}
		if old, e := load(dir); e == nil {
			if old.Backend != c.Backend || old.Executable != c.Executable || old.CWD != c.CWD || strings.Join(old.Args, "\x00") != strings.Join(c.Args, "\x00") {
				return errors.New("service already installed with different settings; uninstall first")
			}
			return action(dir, old, "start")
		} else if !os.IsNotExist(e) {
			return e
		}
		raw, _ := json.MarshalIndent(c, "", "  ")
		if err = save(configPath(dir), raw); err != nil {
			return err
		}
		switch c.Backend {
		case "systemd":
			if err = save(unitPath(), []byte(unitFile(c))); err != nil {
				return err
			}
			if err = invoke("systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			if err = invoke("systemctl", "--user", "enable", "--now", unit); err != nil {
				return err
			}
			fmt.Println("For startup before login, enable user lingering with loginctl if supported.")
		case "launchd":
			if err = save(plistPath(), []byte(plistFile(c, logPath(dir)))); err != nil {
				return err
			}
			if err = action(dir, c, "start"); err != nil {
				return err
			}
		case "native":
			if err = startNative(dir, c); err != nil {
				return err
			}
			fmt.Println("Native supervisor restarts the daemon; configure a host startup hook for startup after machine reboot.")
		}
		return nil
	}
	if len(args) != 1 {
		return errors.New("unexpected service arguments")
	}
	c, err := load(dir)
	if err != nil {
		return fmt.Errorf("service configuration: %w", err)
	}
	return action(dir, c, args[0])
}
func action(dir string, c config, a string) error {
	switch a {
	case "start", "stop", "restart", "status", "logs", "uninstall":
	default:
		return errors.New("unknown service action")
	}
	switch c.Backend {
	case "systemd":
		if a == "logs" {
			return invoke("journalctl", "--user", "-u", unit, "-n", "100", "--no-pager")
		}
		if a == "uninstall" {
			if err := invoke("systemctl", "--user", "disable", "--now", unit); err != nil {
				return err
			}
			if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := invoke("systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			return os.Remove(configPath(dir))
		}
		return invoke("systemctl", "--user", a, unit)
	case "launchd":
		target := domain() + "/" + label
		switch a {
		case "start":
			if exec.Command("launchctl", "print", target).Run() == nil {
				return nil
			}
			return invoke("launchctl", "bootstrap", domain(), plistPath())
		case "stop":
			if exec.Command("launchctl", "print", target).Run() != nil {
				return nil
			}
			return invoke("launchctl", "bootout", target)
		case "restart":
			if err := action(dir, c, "stop"); err != nil {
				return err
			}
			return action(dir, c, "start")
		case "status":
			return invoke("launchctl", "print", target)
		case "uninstall":
			if err := action(dir, c, "stop"); err != nil {
				return err
			}
			if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
				return err
			}
			return os.Remove(configPath(dir))
		case "logs":
			return showLog(dir)
		}
	case "native":
		switch a {
		case "start":
			return startNative(dir, c)
		case "stop":
			return stopNative(dir)
		case "restart":
			if err := stopNative(dir); err != nil {
				return err
			}
			return startNative(dir, c)
		case "status":
			s, err := request(dir, "status")
			if err != nil {
				return fmt.Errorf("service not running: %w", err)
			}
			fmt.Printf("supervisor pid=%d; server pid=%d\n", s.PID, s.ChildPID)
			if s.ChildPID == 0 {
				return errors.New("server restarting")
			}
			return nil
		case "logs":
			return showLog(dir)
		case "uninstall":
			if err := stopNative(dir); err != nil {
				return err
			}
			return os.Remove(configPath(dir))
		}
	}
	return errors.New("unsupported service action")
}
func showLog(dir string) error {
	f, err := os.Open(logPath(dir))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 128*1024 {
		if _, err = f.Seek(-128*1024, io.SeekEnd); err != nil {
			return err
		}
	}
	_, err = io.Copy(os.Stdout, f)
	return err
}
func request(dir, command string) (status, error) {
	var s status
	conn, err := net.DialTimeout("unix", sockPath(dir), time.Second)
	if err != nil {
		return s, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err = json.NewEncoder(conn).Encode(command); err != nil {
		return s, err
	}
	err = json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&s)
	return s, err
}
func stopped(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
func startNative(dir string, c config) error {
	if s, err := request(dir, "status"); err == nil {
		if s.ChildPID == 0 {
			return errors.New("supervisor running, server restarting; see service logs")
		}
		return nil
	} else if !stopped(err) {
		return err
	}
	log, err := os.OpenFile(logPath(dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(c.Executable, "service", "supervise")
	cmd.Dir = c.CWD
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, e := request(dir, "status"); e == nil && s.ChildPID != 0 {
			time.Sleep(500 * time.Millisecond)
			if next, e := request(dir, "status"); e == nil && next.ChildPID == s.ChildPID {
				fmt.Printf("ServerSide service started (pid %d)\n", s.ChildPID)
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("server did not stabilize; run service logs or service stop")
}
func stopNative(dir string) error {
	if _, err := request(dir, "stop"); err != nil && !stopped(err) {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := request(dir, "status"); stopped(err) {
			f, e := os.OpenFile(lockPath(dir), os.O_CREATE|os.O_RDWR, 0600)
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			f.Close()
			if e == nil {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timed out waiting for service shutdown")
}
func supervise(dir string) error {
	c, err := load(dir)
	if err != nil {
		return err
	}
	if c.Backend != "native" {
		return errors.New("native backend not configured")
	}
	lock, err := os.OpenFile(lockPath(dir), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("supervisor already running: %w", err)
	}
	// Only the lock owner may remove a stale control socket.
	if err = os.Remove(sockPath(dir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", sockPath(dir))
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(sockPath(dir), 0600); err != nil {
		return err
	}
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var mu sync.Mutex
	state := status{PID: os.Getpid()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var command string
			if json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&command) == nil {
				mu.Lock()
				s := state
				mu.Unlock()
				if command == "status" || command == "stop" {
					_ = json.NewEncoder(conn).Encode(s)
				}
				if command == "stop" {
					cancel()
				}
			}
			conn.Close()
		}
	}()
	for ctx.Err() == nil {
		cmd := exec.Command(c.Executable, append([]string{"serve"}, c.Args...)...)
		cmd.Dir = c.CWD
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err = cmd.Start()
		if err == nil {
			mu.Lock()
			state.ChildPID = cmd.Process.Pid
			mu.Unlock()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err = <-done:
			case <-ctx.Done():
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(8 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
				}
				return nil
			}
		}
		mu.Lock()
		state.ChildPID = 0
		mu.Unlock()
		fmt.Fprintf(os.Stderr, "%s: server exited (%v); restarting in 2s\n", time.Now().Format(time.RFC3339), err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}
