//go:build !unix

package transport

import (
	"errors"
	"net"
)

type rawWriter struct{}

func newRawWriter(net.Conn) *rawWriter { return nil }

func (*rawWriter) write([]byte) (int, error) {
	return 0, errors.New("transport: direct writes are unix-only")
}
