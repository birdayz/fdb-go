//go:build linux

package bench

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// pacer waits for a scheduled start on a timerfd read through the netpoller.
// time.Sleep cannot wake within a millisecond while every P is idle (the
// runtime then blocks in epoll_wait with a millisecond timeout), which would
// bunch the starts of a client that leaves the Go side idle.
type pacer struct{ f *os.File }

func newPacer() (*pacer, error) {
	fd, err := unix.TimerfdCreate(unix.CLOCK_MONOTONIC, unix.TFD_NONBLOCK|unix.TFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &pacer{f: os.NewFile(uintptr(fd), "pacer")}, nil
}

// wait returns after d, or false once close has been called.
func (p *pacer) wait(d time.Duration) bool {
	rc, err := p.f.SyscallConn()
	if err != nil {
		return false
	}
	its := unix.ItimerSpec{Value: unix.NsecToTimespec(max(int64(d), 1))}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.TimerfdSettime(int(fd), 0, &its, nil) }); err != nil || serr != nil {
		return false
	}
	var buf [8]byte
	n, err := p.f.Read(buf[:])
	return err == nil && n == len(buf)
}

// close ends a wait in progress and every later one.
func (p *pacer) close() { _ = p.f.Close() }
