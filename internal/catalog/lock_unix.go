//go:build unix

package catalog

import (
	"golang.org/x/sys/unix"
	"os"
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
