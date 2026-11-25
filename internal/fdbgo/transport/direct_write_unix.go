//go:build unix

package transport

import (
	"net"
	"syscall"
)

// rawWriter makes non-blocking socket writes without allocating per call;
// writeMu serializes its use.
type rawWriter struct {
	rc  syscall.RawConn
	buf []byte
	n   int
	err error
	fn  func(uintptr) bool
}

func newRawWriter(nc net.Conn) *rawWriter {
	tc, ok := nc.(*net.TCPConn)
	if !ok {
		return nil
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return nil
	}
	w := &rawWriter{rc: rc}
	w.fn = func(fd uintptr) bool {
		for {
			w.n, w.err = syscall.Write(int(fd), w.buf)
			if w.err != syscall.EINTR {
				return true // never wait for writability
			}
		}
	}
	return w
}

// write makes one attempt; a full socket takes nothing rather than waiting.
func (w *rawWriter) write(b []byte) (int, error) {
	w.buf = b
	err := w.rc.Write(w.fn)
	w.buf = nil
	switch {
	case err != nil:
		return 0, err
	case w.err == syscall.EAGAIN:
		return 0, nil
	case w.err != nil:
		return 0, w.err
	}
	return w.n, nil
}
