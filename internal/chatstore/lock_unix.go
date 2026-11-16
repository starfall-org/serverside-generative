//go:build unix

package chatstore

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func lockDirectory(dir string, exclusive bool) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	if err = unix.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

// TryJobLease holds a process-scoped lock for the lifetime of a synced job.
// The stable, private lock file is never removed: unlinking it can split locks.
func (s *Store) TryJobLease(id string) (func(), error) {
	if !validID(id) {
		return nil, ErrInvalid
	}
	if err := s.checkDirs(false); err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(s.dir), ".job-leases")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(dirFD), "job lease directory")
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	fd, err := unix.Openat(dirFD, filename(id)+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "job lease")
	info, err = f.Stat()
	var stat unix.Stat_t
	statErr := unix.Fstat(fd, &stat)
	if err != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 0 || stat.Nlink != 1 {
		f.Close()
		return nil, ErrInvalid
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrJobBusy
		}
		return nil, err
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = f.Close() }, nil
}
