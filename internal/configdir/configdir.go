// Package configdir owns the default on-disk layout of the CLI and daemon.
package configdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const Name = "generative"

// Root fails closed when legacy storage is still present rather than silently
// starting a new identity or losing access to the existing chats.
func Root() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	for _, name := range []string{"serverside", "serverside-generative"} {
		path := filepath.Join(base, name)
		if _, err := os.Lstat(path); err == nil {
			return "", fmt.Errorf("legacy configuration at %s: stop and uninstall the old service, then run generative config relocate", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return filepath.Join(base, Name), nil
}

// Relocate performs an explicit offline move. Never overwrite an existing
// destination, follow symlinks, or move a registered/running daemon.
func Relocate() error {
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	return relocate(base)
}
func relocate(base string) error {
	root := filepath.Join(base, Name)
	data := filepath.Join(base, "serverside")
	identity := filepath.Join(base, "serverside-generative")
	present := func(path string) (bool, error) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return false, fmt.Errorf("unsafe configuration directory: %s", path)
		}
		return true, nil
	}
	hasData, err := present(data)
	if err != nil {
		return err
	}
	hasIdentity, err := present(identity)
	if err != nil {
		return err
	}
	if !hasData && !hasIdentity {
		return nil
	}
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("destination already exists: %s; back up and reconcile manually", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hasIdentity && hasData {
		if _, err := os.Lstat(filepath.Join(data, "identity")); err == nil {
			return errors.New("legacy data already contains an identity directory; reconcile manually")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if hasIdentity {
		for _, rel := range []string{"service/config.json", "service/supervisor.sock", "control.sock", "run/data.sock"} {
			path := filepath.Join(identity, rel)
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("old service must be uninstalled and daemon stopped before relocation (remaining %s)", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if hasData {
		if err := os.Rename(data, root); err != nil {
			return err
		}
	} else if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	if hasIdentity {
		if err := os.Rename(identity, filepath.Join(root, "identity")); err != nil {
			var rollback error
			if hasData {
				rollback = os.Rename(root, data)
			} else {
				rollback = os.Remove(root)
			}
			return errors.Join(err, rollback)
		}
	}
	return nil
}
