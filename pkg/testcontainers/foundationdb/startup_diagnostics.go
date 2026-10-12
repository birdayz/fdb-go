package foundationdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

const (
	startupDiagnosticTimeout = 5 * time.Second
	startupLogBytes          = 8192
)

type startupFailureError struct {
	cause       error
	cleanupErr  error
	diagnostics string
}

func (e *startupFailureError) Error() string {
	return fmt.Sprintf("%v; startup diagnostics: %s", e.cause, e.diagnostics)
}

func (e *startupFailureError) Unwrap() error { return errors.Join(e.cause, e.cleanupErr) }

// A cancelled startup still needs evidence before cleanup destroys the container.
func startupFailure(ctr testcontainers.Container, cause error) error {
	if ctr == nil {
		return cause
	}
	diagnostics := startupDiagnostics(ctr)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()
	cleanup := ""
	cleanupErr := Terminate(cleanupCtx, &Container{Container: ctr})
	if cleanupErr != nil {
		cleanup = fmt.Sprintf("; cleanup incomplete: %v", cleanupErr)
	}
	return &startupFailureError{cause: cause, cleanupErr: cleanupErr, diagnostics: diagnostics + cleanup}
}

func startupDiagnostics(ctr testcontainers.Container) string {
	ctx, cancel := context.WithTimeout(context.Background(), startupDiagnosticTimeout)
	defer cancel()
	stateText := "state unavailable"
	if state, err := ctr.State(ctx); err == nil && state != nil {
		stateText = fmt.Sprintf("status=%q exit=%d oom=%t", state.Status, state.ExitCode, state.OOMKilled)
	}
	prefix := fmt.Sprintf("container=%s %s", ctr.GetContainerID(), stateText)
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return prefix + "; logs unavailable"
	}
	defer cli.Close()
	reader, err := cli.ContainerLogs(ctx, ctr.GetContainerID(), container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "40",
	})
	if err != nil {
		return prefix + "; logs unavailable"
	}
	defer reader.Close()
	return prefix + "; logs tail (up to 40 lines, 8192 bytes):\n" + startupLogTail(reader)
}

// Decode Docker's stream frames without allocating their advertised frame sizes.
func startupLogTail(reader io.Reader) string {
	raw := io.LimitReader(reader, startupLogBytes)
	var logs bytes.Buffer
	var header [8]byte
	for {
		if _, err := io.ReadFull(raw, header[:]); err != nil {
			break
		}
		if header[0] != 1 && header[0] != 2 {
			break
		}
		if _, err := io.Copy(&logs, io.LimitReader(raw, int64(binary.BigEndian.Uint32(header[4:])))); err != nil {
			break
		}
	}
	return logs.String()
}
