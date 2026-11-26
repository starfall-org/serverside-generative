//go:build linux || darwin

package service

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnitEscaping(t *testing.T) {
	c := config{Backend: "systemd", Executable: "/a path/serverside", CWD: "/a%h", Args: []string{"--agent-root", "/x/$a/%h"}}
	s := unitFile(c)
	for _, want := range []string{`ExecStart="/a path/serverside" serve "--agent-root" "/x/$$a/%%h"`, `WorkingDirectory="/a%%h"`, "Restart=always"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
}
func TestLaunchPlist(t *testing.T) {
	s := plistFile(config{Executable: "/a & b", CWD: "/a<b>", Args: []string{"--data-dir", "/x/y"}}, "/log")
	if !strings.Contains(s, "/a &amp; b") || !strings.Contains(s, "/a&lt;b&gt;") {
		t.Fatal(s)
	}
	var v any
	if err := xml.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
}
func TestRejectPasswordAndNewlines(t *testing.T) {
	c := config{Backend: "native", Executable: "/a", CWD: "/b"}
	for _, arg := range []string{"--password", "-password=secret", "--password=secret", "--web-password", "-web-password=secret", "--web-password=secret", "x\ny", "x\x00y"} {
		c.Args = []string{arg}
		if validate(c) == nil {
			t.Fatalf("accepted %q", arg)
		}
	}
}
func TestPrivateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config")
	if err := save(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := save(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("%q %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v %v", info, err)
	}
}

func TestNativeSupervisorLifecycle(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ss-supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	executable := filepath.Join(dir, "fake-server")
	if err = os.WriteFile(executable, []byte("#!/bin/sh\nexec sleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := config{Backend: "native", Executable: executable, CWD: dir}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = save(configPath(dir), raw); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- supervise(dir) }()
	defer func() {
		if err := stopNative(dir); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("supervisor failed to exit")
		}
	}()
	waitChild := func(previous int) int {
		t.Helper()
		deadline := time.Now().Add(7 * time.Second)
		for time.Now().Before(deadline) {
			s, e := request(dir, "status")
			if e == nil && s.ChildPID != 0 && s.ChildPID != previous {
				return s.ChildPID
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("child did not start")
		return 0
	}
	old := waitChild(0)
	if err = syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	restarted := waitChild(old)
	if restarted == old {
		t.Fatal("child not restarted")
	}
	if err = stopNative(dir); err != nil {
		t.Fatal(err)
	}
	if _, err = request(dir, "status"); !stopped(err) {
		t.Fatalf("service not stopped: %v", err)
	}
}
