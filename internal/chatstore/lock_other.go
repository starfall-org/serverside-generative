//go:build !unix

package chatstore

import "errors"

// Fail closed: multi-process CAS is not implemented on this platform.
func lockDirectory(_ string, _ bool) (func(), error) {
	return nil, errors.New("chat storage locking is unsupported on this platform")
}

func (s *Store) TryJobLease(_ string) (func(), error) {
	return nil, errors.New("chat job locking is unsupported on this platform")
}
