//go:build !linux && !darwin

package service

import "errors"

func Run(args []string, executable string) error {
	return errors.New("service management is supported on Linux and macOS")
}
