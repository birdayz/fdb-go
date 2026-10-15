//go:build linux

package hrtimer

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Timer is a timerfd read through the netpoller: Wait parks the goroutine
// without holding a thread or P, and a ready timerfd wakes epoll_wait whatever
// the runtime's own timeout.
type Timer struct {
	f *os.File
}

// New creates a disarmed timer.
func New() (*Timer, error) {
	fd, err := unix.TimerfdCreate(unix.CLOCK_MONOTONIC, unix.TFD_NONBLOCK|unix.TFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	// A non-blocking fd makes os.NewFile register it with the netpoller.
	return &Timer{f: os.NewFile(uintptr(fd), "hrtimer")}, nil
}

// Reset arms the timer to expire d from now, replacing any pending expiry.
// d <= 0 arms the shortest delay, since a zero it_value would disarm it.
// On error the timer is unchanged.
func (t *Timer) Reset(d time.Duration) error {
	if d <= 0 {
		d = 1
	}
	return t.set(unix.NsecToTimespec(int64(d)))
}

// Stop disarms the timer. A Wait already parked stays parked.
func (t *Timer) Stop() error {
	return t.set(unix.Timespec{})
}

func (t *Timer) set(value unix.Timespec) error {
	rc, err := t.f.SyscallConn()
	if err != nil {
		return ErrClosed
	}
	its := unix.ItimerSpec{Value: value}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.TimerfdSettime(int(fd), 0, &its, nil)
	}); err != nil {
		return ErrClosed
	}
	return serr
}

// Wait blocks until an expiry is consumed. It returns ErrClosed once Close
// has been called, including for a Wait parked at the time.
func (t *Timer) Wait() error {
	var buf [8]byte
	for {
		n, err := t.f.Read(buf[:])
		switch {
		case err == nil && n == len(buf):
			return nil
		case errors.Is(err, os.ErrClosed):
			return ErrClosed
		case err != nil:
			return err
		}
	}
}

// Close releases the timerfd and wakes a parked Wait.
func (t *Timer) Close() error {
	return t.f.Close()
}
