//go:build unix

package secrets

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStoreRetainsAndReleasesDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := unix.Flock(int(fd.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("writer lock not held: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(fd.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = unix.Flock(int(fd.Fd()), unix.LOCK_UN)
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(fd.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("reader lock not held: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(fd.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("reader lock not released: %v", err)
	}
	_ = unix.Flock(int(fd.Fd()), unix.LOCK_UN)
}
