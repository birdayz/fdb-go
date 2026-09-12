package foundationdb

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// TestRun_InterruptedFileAllocation pins the server-side failure behind the
// factory-corpus CI container exit. FDB 7.3.77's AsyncFileKAIO::truncate maps
// fallocate's EINTR directly to io_error; AsyncFileEIO uses eio_ftruncate instead.
// Inject at the syscall boundary of a real FDB container, not in the Go client.
func TestRun_InterruptedFileAllocation(t *testing.T) {
	t.Parallel()
	for _, onDisk := range []bool{false, true} {
		name := "tmpfs"
		if onDisk {
			name = "disk"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			// This profile is confined to this disposable test container. It
			// deterministically returns the same errno recorded by the failed
			// server, independent of load or signal delivery timing.
			opts := []testcontainers.ContainerCustomizer{withInterruptedFallocate()}
			if onDisk {
				opts = append(opts, WithDataOnDisk())
			}
			container, err := Run(ctx, "", opts...)
			if err != nil {
				t.Fatalf("FDB must initialize with interrupted fallocate: %v", err)
			}
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()
				if err := container.Terminate(cleanupCtx); err != nil {
					t.Errorf("terminate: %v", err)
				}
			}()

			// Positive control: prove the injected syscall actually fails in
			// this container. A backend that never calls fallocate must not
			// make a silently missing fault look like successful coverage.
			code, reader, err := container.Exec(ctx, []string{
				"env", "LC_ALL=C", "timeout", "5", "fallocate", "-l", "4096", "/tmp/eintr-control",
			}, tcexec.Multiplexed())
			if err != nil {
				t.Fatalf("fallocate control exec: %v", err)
			}
			output, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if code != 1 || !strings.Contains(string(output), "Interrupted system call") {
				t.Fatalf("fault did not fire: exit=%d output=%q", code, output)
			}

			outputText, err := container.FDBCLIExec(ctx, "writemode on; set eintr-key survived; get eintr-key")
			if err != nil {
				t.Fatalf("commit/read with fault active: %v", err)
			}
			if !strings.Contains(outputText, "`eintr-key' is `survived'") {
				t.Fatalf("committed value not returned: %s", outputText)
			}
			t.Log("FDB-EINTR: fault control fired; committed value read back")
		})
	}
}

// An explicit override must still select KAIO. With the same injected errno,
// the pinned upstream version dies on that path. If an upstream upgrade fixes
// this, revisit the workaround rather than silently retaining a stale default.
func TestRun_KAIOOverride(t *testing.T) {
	t.Parallel()
	for _, onDisk := range []bool{false, true} {
		name := "tmpfs"
		if onDisk {
			name = "disk"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			var serverLog []byte
			var logErr error
			captureLog := func(ctx context.Context, ctr testcontainers.Container) error {
				reader, err := ctr.Logs(ctx)
				if err != nil {
					logErr = err
					return err
				}
				defer reader.Close()
				serverLog, logErr = io.ReadAll(reader)
				return logErr
			}
			// One attempt suffices for the negative control; do not retry a server
			// deliberately configured to exercise the upstream failure.
			opts := []testcontainers.ContainerCustomizer{
				WithKnob("disable_posix_kernel_aio", "0"),
				withInterruptedFallocate(),
				testcontainers.WithLifecycleHooks(testcontainers.ContainerLifecycleHooks{
					PreTerminates: []testcontainers.ContainerHook{captureLog},
				}),
			}
			if onDisk {
				opts = append(opts, WithDataOnDisk())
			}
			container, err := runOnce(ctx, "", opts...)
			if container != nil {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()
				if err := container.Terminate(cleanupCtx); err != nil {
					t.Errorf("terminate: %v", err)
				}
			}
			if err == nil || ctx.Err() != nil || logErr != nil || !strings.Contains(string(serverLog), "Fatal Error: Disk i/o operation failed") {
				t.Fatalf("KAIO override did not reproduce the upstream failure: run=%v context=%v logs=%v\n%s", err, ctx.Err(), logErr, serverLog)
			}
			t.Log("FDB-EINTR: explicit KAIO override reproduced the fatal allocation error")
		})
	}
}

func withInterruptedFallocate() testcontainers.ContainerCustomizer {
	return testcontainers.WithHostConfigModifier(func(hc *dockercontainer.HostConfig) {
		hc.SecurityOpt = append(hc.SecurityOpt, `seccomp={"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["fallocate"],"action":"SCMP_ACT_ERRNO","errnoRet":4}]}`)
	})
}
