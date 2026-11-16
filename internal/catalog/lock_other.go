//go:build !unix

package catalog

import "errors"

// Fail closed: multi-process CAS is not implemented on this platform.
func lockDirectory(_ string, _ bool) (func(), error) {
	return nil, errors.New("chat storage locking is unsupported on this platform")
}
