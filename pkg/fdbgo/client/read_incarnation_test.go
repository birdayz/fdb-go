package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

func TestReadIncarnationRetirement(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cancel", "reset", "commit-reuse", "retry-success", "retry-domain", "retry-limit", "retry-caller", "retry-timeout"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			defer tx.Cancel()
			old, release := tx.opContext(context.Background())
			defer release()
			oldInc := tx.readOperation(old).inc
			tx.readOperation(old).lease.release() // observe the future, not a live execution phase
			rotated := false
			switch name {
			case "cancel":
				tx.Cancel()
				tx.Cancel()
			case "reset":
				tx.Reset()
				rotated = true
			case "commit-reuse":
				tx.postCommitReset()
				rotated = true
			case "retry-success":
				tx.SetMaxRetryDelay(0)
				if err := tx.OnError(context.Background(), &wire.FDBError{Code: 1020}); err != nil {
					t.Fatal(err)
				}
				rotated = true
			case "retry-domain":
				want := &wire.FDBError{Code: 2004}
				if err := tx.OnError(context.Background(), want); err != want {
					t.Fatalf("domain error=%v, want original %v", err, want)
				}
			case "retry-limit":
				tx.SetRetryLimit(0)
				want := &wire.FDBError{Code: 1020}
				if err := tx.OnError(context.Background(), want); err != want {
					t.Fatalf("retry exhaustion=%v, want original %v", err, want)
				}
			case "retry-caller":
				caller, cancel := context.WithCancel(context.Background())
				cancel()
				if err := tx.OnError(caller, &wire.FDBError{Code: 1020}); !errors.Is(err, context.Canceled) {
					t.Fatalf("retry cancellation=%v, want caller cancellation", err)
				}
			case "retry-timeout":
				want := &wire.FDBError{Code: 1031}
				if err := tx.OnError(context.Background(), want); err != want {
					t.Fatalf("retry timeout=%v, want original %v", err, want)
				}
			}
			select {
			case <-old.Done():
			case <-time.After(time.Second):
				t.Fatal("old operation not interrupted")
			}
			if code := fdbCodeOf(tx.mapReadError(old, old.Err())); code != 1025 {
				t.Fatalf("old operation code=%d, want 1025", code)
			}
			current, stop := tx.opContext(context.Background())
			defer stop()
			currentInc := tx.readOperation(current).inc
			if (currentInc != oldInc) != rotated {
				t.Fatalf("incarnation replacement=%v, want %v", currentInc != oldInc, rotated)
			}
			if rotated {
				if err := tx.readEntryError(current); err != nil {
					t.Fatalf("old failure poisoned new operation: %v", err)
				}
				tx.trackReadOperation(old, &wire.FDBError{Code: 2004})
				tx.readErrMu.Lock()
				err := tx.readErr
				tx.readErrMu.Unlock()
				if err != nil {
					t.Fatalf("late old error poisoned new read ledger: %v", err)
				}
			} else if code := fdbCodeOf(tx.readEntryError(current)); code != 1025 {
				t.Fatalf("terminal transaction accepted another operation: %d", code)
			}
			// An internal retry on an old context must not bind currentInc.
			nested, end := tx.opContext(old)
			defer end()
			if tx.readOperation(nested).inc != oldInc {
				t.Fatal("nested operation rebound the retired incarnation")
			}
		})
	}
}

func TestReadIncarnationTimerRetirement(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, release := tx.opContext(context.Background())
	defer release()
	tx.SetTimeout(60_000)
	tx.readErrMu.Lock()
	inc, gen := tx.readLife, tx.readLife.timerGen
	tx.readErrMu.Unlock()
	tx.SetTimeout(0)
	tx.fireReadTimeout(inc, gen) // callback already scheduled when Stop lost the race
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("retired timeout poisoned a cleared configuration: %v", err)
	}
	tx.SetTimeout(60_000)
	tx.readErrMu.Lock()
	gen = inc.timerGen
	tx.readErrMu.Unlock()
	tx.SetTimeout(120_000)
	tx.fireReadTimeout(inc, gen)
	if err := tx.readEntryError(ctx); err != nil {
		t.Fatalf("retired timeout poisoned rearmed configuration: %v", err)
	}
	tx.readErrMu.Lock()
	gen = inc.timerGen
	timer := inc.timer
	tx.readErrMu.Unlock()
	timer.Stop() // Deliver this callback explicitly, without leaving a live timer.
	tx.fireReadTimeout(inc, gen)
	tx.SetTimeout(0)
	if got := fdbCodeOf(tx.readEntryError(ctx)); got != 1031 {
		t.Fatalf("clearing timeout erased terminal cause: code=%d", got)
	}
	tx.Cancel()
	if got := fdbCodeOf(tx.readEntryError(ctx)); got != 1031 {
		t.Fatalf("Cancel overwrote earlier timeout: code=%d", got)
	}
	tx.readOperation(ctx).lease.release()
	tx.Reset()
	fresh, stop := tx.opContext(context.Background())
	defer stop()
	tx.fireReadTimeout(inc, gen)
	if err := tx.readEntryError(fresh); err != nil {
		t.Fatalf("old timer poisoned replacement incarnation: %v", err)
	}
}

func TestReadIncarnationCompletionPrecedence(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := &Transaction{}
	ctx, release := tx.opContext(parent)
	defer release()
	tx.Cancel()
	cancel()
	if got := tx.mapReadError(ctx, context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("caller interruption=%v, want context.Canceled", got)
	}
	if got := fdbCodeOf(tx.readEntryError(ctx)); got != 1025 {
		t.Fatalf("entry did not prioritize already-terminal incarnation: %d", got)
	}
	for _, err := range []error{nil, &wire.FDBError{Code: 2004}, &wire.FDBError{Code: 1007}} {
		if got := tx.mapReadError(ctx, err); got != err {
			t.Fatalf("completed outcome %v replaced with %v", err, got)
		}
	}
}

func TestReadReplyReadyOutranksCancellation(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"rpc", "single", "hedge", "race-primary", "race-secondary"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for _, failed := range []bool{false, true} {
				for range 32 {
					ch := make(chan transport.Response, 1)
					var wantErr error
					if failed {
						wantErr = &wire.FDBError{Code: 2004}
					}
					ch <- transport.Response{Body: []byte("completed"), Err: wantErr}
					rpc := inFlightRPC{replyCh: ch, replyHandle: &transport.ReplyHandle{}, addr: "ready"}
					other := inFlightRPC{replyCh: make(chan transport.Response), replyHandle: &transport.ReplyHandle{}, addr: "held"}
					var body []byte
					var err error
					switch route {
					case "rpc":
						resp, waitErr := waitReply(ch, ctx, time.Hour)
						if waitErr != nil {
							t.Fatal(waitErr)
						}
						body, err = resp.Body, resp.Err
					case "single":
						result := waitForReply(ctx, rpc, time.Hour)
						body, err = result.body, result.err
					case "hedge":
						result := sendFrameWithHedge(ctx, time.Hour, func() inFlightRPC { return rpc }, func() inFlightRPC {
							t.Error("sent secondary despite queued primary reply")
							return other
						}, time.Hour)
						body, err = result.body, result.err
					case "race-primary", "race-secondary":
						a, b := rpc, other
						if route == "race-secondary" {
							a, b = other, rpc
						}
						result := raceReplies(ctx, a, b, time.Hour)
						body, err = result.body, result.err
						if len(result.others) != 1 || result.others[0].addr != "held" {
							t.Fatalf("loser accounting=%v, want held request", result.others)
						}
					}
					if err != wantErr || (!failed && string(body) != "completed") {
						t.Fatalf("ready outcome=(%q, %v), want completed/%v", body, err, wantErr)
					}
				}
			}
		})
	}
}

func TestReadIncarnationAuxiliaryReadsKeepCapturedCause(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	old, release := tx.opContext(context.Background())
	defer release()
	tx.Cancel()
	tx.readOperation(old).lease.release()
	tx.Reset()
	tx.creationTime = time.Now().Add(-time.Second)
	tx.SetTimeout(1) // Current incarnation is 1031; captured old cause remains 1025.
	for _, read := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"locations", func(ctx context.Context) error {
			_, err := tx.GetLocations(ctx, []byte("a"), []byte("b"), 1)
			return err
		}},
		{"addresses", func(ctx context.Context) error { _, err := tx.GetAddressesForKey(ctx, []byte("a")); return err }},
		{"metrics", func(ctx context.Context) error {
			_, err := tx.GetEstimatedRangeSizeBytes(ctx, []byte("a"), []byte("b"))
			return err
		}},
		{"split points", func(ctx context.Context) error {
			_, err := tx.GetRangeSplitPoints(ctx, []byte("a"), []byte("b"), 10)
			return err
		}},
	} {
		if got := fdbCodeOf(read.run(old)); got != 1025 {
			t.Errorf("%s rebound old context to current cause: got %d, want 1025", read.name, got)
		}
	}
	if _, err := tx.GetVersionstamp(); fdbCodeOf(err) != 1031 {
		t.Fatalf("current versionstamp must observe current timeout, got %v", err)
	}
}
