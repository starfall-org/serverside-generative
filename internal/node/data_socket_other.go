//go:build !linux

package node

import (
	"errors"
	"net"
	"os"
)

func DataSocketSupported() bool { return false }

func validateDataSocketOwner(os.FileInfo) error {
	return errors.New("local data socket unsupported on this platform")
}
func validateDataSocketDir(string) error {
	return errors.New("local data socket unsupported on this platform")
}
func isStaleSocketError(error) bool     { return false }
func authorizedLocalPeer(net.Conn) bool { return false }

func ensureDataSocketDir(string) error {
	return errors.New("local data socket unsupported on this platform")
}
