//go:build !linux && !darwin

package service

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
)

func Run(args []string, executable string) error {
	return errors.New("service management is supported on Linux and macOS")
}

func LockDaemon() (io.Closer, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func Connect(ctx context.Context, path, _ string, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	return dial(ctx, path)
}
