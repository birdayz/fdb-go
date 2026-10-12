package foundationdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestStartupLogTailIsBounded(t *testing.T) {
	t.Parallel()
	frame := func(stream byte, payload string) []byte {
		header := make([]byte, 8)
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
		return append(header, payload...)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"stdout and stderr", append(frame(1, "stdout"), frame(2, "stderr")...), "stdoutstderr"},
		{"short header", []byte{1, 0}, ""},
		{"invalid stream", frame(7, "bad"), ""},
		{"partial frame", append(frame(1, "ok"), frame(2, "tail")[:10]...), "okta"},
		{"oversized frame", frame(1, strings.Repeat("x", startupLogBytes*2)), strings.Repeat("x", startupLogBytes-8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := startupLogTail(bytes.NewReader(tc.raw)); got != tc.want {
				t.Errorf("log tail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCancelledStartupKeepsDiagnosticsAndCleansUp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("FDB not available (no Docker): %v", err)
	}

	const marker = "startup-diagnostic-log-marker"
	const oldMarker = "old-startup-log-must-not-be-in-tail"
	const canary = "startup-env-canary-must-not-be-printed"
	fault := errors.New("cancelled startup fault")
	var captured testcontainers.Container
	var faultAt time.Time
	var cleanupContextOK bool
	t.Cleanup(func() {
		if captured != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			TerminateT(t, cleanupCtx, &Container{Container: captured})
		}
	})
	_, err = Run(ctx, "", testcontainers.WithEnv(map[string]string{"STARTUP_CANARY": canary}),
		testcontainers.WithAdditionalLifecycleHooks(testcontainers.ContainerLifecycleHooks{
			PostReadies: []testcontainers.ContainerHook{func(hookCtx context.Context, ctr testcontainers.Container) error {
				captured = ctr
				code, reader, err := ctr.Exec(hookCtx, []string{"/bin/sh", "-c", "{ printf '" + oldMarker + "\\n'; for i in $(seq 1 80); do printf 'startup log line %s\\n' \"$i\"; done; printf '" + marker + "\\n'; } > /proc/1/fd/1"})
				if err != nil {
					return err
				}
				if _, err := io.Copy(io.Discard, reader); err != nil {
					return err
				}
				if code != 0 {
					return errors.New("could not write startup log marker")
				}
				// Exec's pipe write can finish before Docker has ingested the log.
				if err := wait.ForLog(marker).WaitUntilReady(hookCtx, ctr); err != nil {
					return err
				}
				faultAt = time.Now()
				cancel()
				return errors.Join(context.Canceled, fault)
			}},
			PreTerminates: []testcontainers.ContainerHook{func(cleanupCtx context.Context, _ testcontainers.Container) error {
				deadline, bounded := cleanupCtx.Deadline()
				cleanupContextOK = cleanupCtx.Err() == nil && bounded && time.Until(deadline) <= 30*time.Second
				return nil
			}},
		}))
	if captured == nil || faultAt.IsZero() {
		t.Fatalf("startup did not reach the real cancellation fault: %v", err)
	}
	var terminationErr *TerminationError
	if errors.As(err, &terminationErr) {
		t.Errorf("container startup cleanup failed: %v", err)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, fault) {
		t.Errorf("startup lost the original cancellation/fault: %v", err)
	}
	if err == nil {
		t.Fatal("cancelled startup succeeded")
	}
	for _, want := range []string{"status=\"running\"", "exit=0", "oom=false", marker} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup error missing diagnostic %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), oldMarker) || len(err.Error()) > 10*1024 {
		t.Errorf("startup diagnostics exposed environment or unbounded output: %v", err)
	}
	if elapsed := time.Since(faultAt); elapsed > 45*time.Second {
		t.Errorf("diagnostics/cleanup outlived their short budgets: %v", elapsed)
	}
	if !cleanupContextOK {
		t.Error("cleanup reused the cancelled startup context or had no short deadline")
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer checkCancel()
	if _, err := cli.ContainerInspect(checkCtx, captured.GetContainerID()); !client.IsErrNotFound(err) {
		t.Errorf("cancelled startup left its container behind: %v", err)
	} else {
		// Startup already removed it; fallback cleanup must not terminate it twice.
		captured = nil
	}
}
