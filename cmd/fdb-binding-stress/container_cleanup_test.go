//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
)

func TestRealRunSeedRequestsLiteralPinnedImageBeforeHelperDefaults(t *testing.T) {
	t.Parallel()
	marker := errors.New("container-start observer stopped before Docker")
	d := defaultRunnerDeps(realRunSeed)
	calls := 0
	var image string
	d.startFDB = func(ctx context.Context, requested string) (*foundationdb.Container, error) {
		calls++
		image = requested
		return nil, marker
	}
	r := realRunSeedWithDeps(context.Background(), 146, workload{"api", 20, 1, false, 730}, "unused", t.TempDir(), d)
	// Independently literal: do not derive expected image from testedFDBVersion,
	// FDB_VERSION, or the foundationdb helper's default.
	if calls != 1 || image != "foundationdb/foundationdb:7.3.77" || r.Pass || r.FDBAlive || !strings.Contains(r.Error, marker.Error()) || !strings.HasPrefix(r.Error, "container start failed:") {
		t.Fatalf("start calls=%d requested=%q result=%+v", calls, image, r)
	}
}

func TestRealRunSeedCleanupHasIndependent30SecondDeadline(t *testing.T) {
	t.Parallel()
	for _, blocked := range []bool{false, true} {
		name := "real_default_deadline"
		if blocked {
			name = "blocked_cleanup_expires"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seedCtx, cancelSeed := context.WithCancel(context.Background())
			defer cancelSeed()
			d := defaultRunnerDeps(realRunSeed)
			container := &foundationdb.Container{}
			starts, logs, terminations, factories := 0, 0, 0, 0
			d.startFDB = func(context.Context, string) (*foundationdb.Container, error) {
				starts++
				cancelSeed() // cleanup must NOT inherit this cancellation
				return container, nil
			}
			// Force a deterministic error before tester/liveness methods. ClusterFile
			// merely returns its stored string; a file as seedDir makes WriteFile fail.
			seedDir := filepath.Join(t.TempDir(), "case-is-a-file")
			if err := os.WriteFile(seedDir, []byte("prior evidence"), 0o644); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			factory := d.newTimeoutContext
			if blocked {
				factory = guardTimeoutAfterEntry(entered, 30*time.Second)
			}
			var requested time.Duration
			var factoryParent context.Context
			var factoryBefore, factoryAfter time.Time
			var cleanupCtx context.Context
			var entryErr, waitErr error
			d.newTimeoutContext = func(parent context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
				factories++
				requested = duration
				factoryParent = parent
				factoryBefore = time.Now()
				ctx, cancel := factory(parent, duration)
				factoryAfter = time.Now()
				return ctx, cancel
			}
			d.collectLogs = func(ctx context.Context, c *foundationdb.Container, dir string) {
				logs++
				cleanupCtx = ctx
				entryErr = ctx.Err()
				if c != container || dir != seedDir {
					t.Errorf("wrong cleanup capture container=%p dir=%q", c, dir)
				}
				if blocked {
					close(entered)
					watchdog := time.NewTimer(10 * time.Second)
					defer watchdog.Stop()
					select {
					case <-ctx.Done():
						waitErr = ctx.Err()
					case <-watchdog.C:
						waitErr = errors.New("cleanup context was not deadline bounded")
					}
				}
			}
			d.terminateFDB = func(ctx context.Context, c *foundationdb.Container) error {
				terminations++
				if ctx != cleanupCtx || c != container {
					t.Error("termination did not use the same captured cleanup context/container")
				}
				return ctx.Err()
			}
			r := realRunSeedWithDeps(seedCtx, 146, workload{"api", 20, 1, false, 730}, t.TempDir(), seedDir, d)
			if starts != 1 || logs != 1 || terminations != 1 || factories != 1 || requested != 30*time.Second || factoryParent == nil || factoryParent.Done() != nil || entryErr != nil || !errors.Is(seedCtx.Err(), context.Canceled) || !strings.HasPrefix(r.Error, "write cluster file:") {
				t.Fatalf("starts/logs/terminations/factories=%d/%d/%d/%d request=%v parent=%v entry=%v seed=%v result=%+v", starts, logs, terminations, factories, requested, factoryParent, entryErr, seedCtx.Err(), r)
			}
			if blocked {
				if !errors.Is(waitErr, context.DeadlineExceeded) || !strings.Contains(r.Error, context.DeadlineExceeded.Error()) {
					t.Fatalf("blocked cleanup=%v result=%+v", waitErr, r)
				}
			} else {
				// This checks the actual default WithTimeout result, not admissionReserve
				// and not a custom test context which could fabricate a Deadline method.
				deadline, ok := cleanupCtx.Deadline()
				if !ok || deadline.Before(factoryBefore.Add(30*time.Second)) || deadline.After(factoryAfter.Add(30*time.Second)) {
					t.Fatalf("default cleanup deadline=%v,%v factory bracket=[%v,%v]", deadline, ok, factoryBefore, factoryAfter)
				}
			}
			data, err := os.ReadFile(seedDir)
			if err != nil || string(data) != "prior evidence" {
				t.Fatalf("setup evidence=%q err=%v", data, err)
			}
		})
	}
}
