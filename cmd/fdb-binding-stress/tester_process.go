package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type testerProcess struct {
	cmd           *exec.Cmd
	mu            sync.Mutex
	signalsClosed bool
	kill          func(int, syscall.Signal) error
}

func configureTesterProcess(cmd *exec.Cmd) *testerProcess {
	p := &testerProcess{cmd: cmd, kill: syscall.Kill}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = p.cancel
	cmd.WaitDelay = 5 * time.Second
	return p
}

func (p *testerProcess) cancel() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.signalsClosed {
		return os.ErrProcessDone
	}
	return killTesterGroupWithKill(p.cmd, p.kill)
}

func (p *testerProcess) Run() error {
	if err := p.cmd.Start(); err != nil {
		return err
	}
	return p.Wait()
}

func (p *testerProcess) CombinedOutput() ([]byte, error) {
	var output bytes.Buffer
	p.cmd.Stdout, p.cmd.Stderr = &output, &output
	err := p.Run()
	return output.Bytes(), err
}

func (p *testerProcess) Wait() error {
	var info unix.Siginfo
	var err error
	for {
		err = unix.Waitid(unix.P_PID, p.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	// WNOWAIT leaves the exited leader unreaped, pinning its PID and group ID.
	// Close cancellation signalling under the same lock before Wait can reap it.
	p.mu.Lock()
	if !errors.Is(err, syscall.ECHILD) {
		_ = killTesterGroupWithKill(p.cmd, p.kill)
	}
	p.signalsClosed = true
	p.mu.Unlock()
	waitErr := p.cmd.Wait()
	if err != nil {
		return err
	}
	return waitErr
}
