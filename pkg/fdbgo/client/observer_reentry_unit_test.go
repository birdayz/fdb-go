package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"testing"
	"testing/synctest"
	"time"
	"weak"

	"fdb.dev/pkg/fdbgo/wire"
)

func TestOnErrorReappliedTimeoutKeepsOverallLifetime(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"within-budget", "expired-before-first-read", "user-reset-fresh-budget"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The queue and its timers must belong to this fake-clock bubble.
				db := &Database{db: &database{timeouts: newTimeoutQueue()}}
				// An established version floor keeps explicit-version validation local.
				db.db.minAcceptableReadVersion.Store(&minAcceptableStamp{epoch: db.db.grvCache.epochNow(), version: 1})
				tx := db.CreateTransaction()
				defer tx.Cancel()
				originalCreation := tx.creationTime
				originalDeadline := originalCreation.Add(10 * time.Second)
				expectedCreation, expectedDeadline := originalCreation, originalDeadline.UnixNano()
				entries, readCalls, completed, injected := 0, 0, 0, 0
				checkLifetime := func() {
					t.Helper()
					if !tx.creationTime.Equal(expectedCreation) || tx.deadlineNs.Load() != expectedDeadline {
						t.Fatalf("entry %d creation=%v deadline=%d, want %v / %d", entries, tx.creationTime, tx.deadlineNs.Load(), expectedCreation, expectedDeadline)
					}
				}
				configureEntry := func() {
					t.Helper()
					entries++
					tx.SetRetryLimit(0)
					if !tx.hasRetryLimit || tx.retryLimit != 0 {
						t.Fatal("entry did not first apply the RL zero retry limit")
					}
					tx.SetTimeout(10000)
					tx.SetRetryLimit(-1)
					if tx.hasRetryLimit || tx.timeoutDur() != 10*time.Second {
						t.Fatalf("entry policy retry-limit=%t timeout=%v", tx.hasRetryLimit, tx.timeoutDur())
					}
					checkLifetime()
				}
				read := func() (int64, error) {
					readCalls++
					// A supplied version exercises the real read entry without a wire peer.
					tx.SetReadVersion(123)
					return tx.GetReadVersion(context.Background())
				}
				for range 12 {
					configureEntry()
					if version, err := read(); err != nil || version != 123 {
						t.Fatalf("entry %d read=%d, %v, want 123 / nil", entries, version, err)
					}
					completed++
					injected++
					if err := tx.OnError(context.Background(), &wire.FDBError{Code: 1009}); err != nil {
						t.Fatalf("entry %d native OnError(1009)=%v", entries, err)
					}
					checkLifetime()
				}
				if entries != 12 || readCalls != 12 || completed != 12 || injected != 12 || tx.retryCount != 12 || time.Since(originalCreation) != 120*time.Millisecond {
					t.Fatalf("retry prefix entries=%d reads=%d completed=%d injected=%d native-retries=%d elapsed=%v", entries, readCalls, completed, injected, tx.retryCount, time.Since(originalCreation))
				}
				switch mode {
				case "expired-before-first-read":
					// Retry 12 has admitted entry 13, but its first read has not begun.
					time.Sleep(10 * time.Second)
					synctest.Wait()
				case "user-reset-fresh-budget":
					time.Sleep(9 * time.Second)
					tx.Reset()
					expectedCreation = tx.creationTime
					expectedDeadline = expectedCreation.Add(10 * time.Second).UnixNano()
					if !expectedCreation.After(originalCreation) || !expectedCreation.Equal(time.Now()) || tx.retryCount != 0 {
						t.Fatalf("user Reset did not establish a fresh creation time / retry count: %v / %d", expectedCreation, tx.retryCount)
					}
				}
				configureEntry()
				if mode == "user-reset-fresh-budget" {
					time.Sleep(2 * time.Second)
					synctest.Wait()
					if !time.Now().After(originalDeadline) || !time.Now().Before(time.Unix(0, expectedDeadline)) {
						t.Fatal("Reset control did not cross the old deadline inside its fresh budget")
					}
				}
				version, err := read()
				if mode == "expired-before-first-read" {
					var timedOut *wire.FDBError
					if !errors.As(err, &timedOut) || timedOut.Code != 1031 || version != 0 {
						t.Fatalf("first read after admitted retry and elapsed lifetime=%d, %v, want literal 1031", version, err)
					}
				} else {
					if err != nil || version != 123 {
						t.Fatalf("entry 13 control read=%d, %v, want 123 / nil", version, err)
					}
					completed++
				}
				checkLifetime()
				wantCompleted := 13
				if mode == "expired-before-first-read" {
					wantCompleted = 12
				}
				metrics := db.Metrics()
				if entries != 13 || readCalls != 13 || completed != wantCompleted || injected != 12 || metrics.TransactionRetries != 12 || metrics.TransactionsFutureVersions != 12 {
					t.Fatalf("population entries=%d reads=%d completed=%d injected=%d native-retries=%d native-1009=%d", entries, readCalls, completed, injected, metrics.TransactionRetries, metrics.TransactionsFutureVersions)
				}
				t.Logf("lifetime proof mode=%s entries=%d reads=%d completed=%d injected=%d native-1009=%d virtual-elapsed=%v final-error=%v", mode, entries, readCalls, completed, injected, metrics.TransactionsFutureVersions, time.Since(originalCreation), err)
			})
		})
	}
}

// The ownership guard makes the retained-outer-token mutation fail without
// entering its impossible wait cycle. The passing implementation must execute
// the actual same-stack public reentry and preserve the successor afterward.
func TestOnErrorObserverReentry(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1007, 1042, 1021, 1020} {
		for _, site := range []string{"disabled", "enabled-first", "enabled-second", "handle"} {
			for _, action := range []string{"reset", "onerror", "commit", "cancel", "timeout", "caller", "concurrent-reset", "timeout-reset", "reset-caller"} {
				t.Run(fmt.Sprintf("%d/%s/%s", code, site, action), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						caller, cancel := context.WithCancelCause(context.Background())
						defer cancel(nil)
						tx := &Transaction{db: &database{timeouts: newTimeoutQueue()}}
						tx.backoffJitter = func() float64 { return 0 }
						defer tx.Cancel()
						initial := tx.enterState()
						inc := initial.inc
						initial.release()
						var successorWatch context.Context
						var successor *readIncarnation
						var nestedErr error
						called, enabledCalls, handled := false, 0, 0
						guardFailed := false
						reenter := func(ctx context.Context) {
							if called {
								return
							}
							called = true
							tx.readErrMu.Lock()
							users := inc.users
							tx.readErrMu.Unlock()
							if users != 0 {
								guardFailed = true
								t.Errorf("OBSERVER_HELD_LEASE: %d users before synchronous reentry", users)
								return // mutant still unwinds and releases its token
							}
							if ctx != caller {
								t.Error("reentry was not passed the raw public caller")
							}
							switch action {
							case "reset", "reset-caller":
								tx.Reset()
							case "onerror":
								nestedErr = tx.OnError(ctx, &wire.FDBError{Code: 1020})
							case "commit":
								nestedErr = tx.Commit(ctx)
							case "cancel":
								tx.Cancel()
							case "timeout", "timeout-reset":
								tx.SetTimeout(1)
								time.Sleep(2 * time.Millisecond)
								synctest.Wait()
								if action == "timeout-reset" {
									tx.Reset()
								}
							case "caller":
								cancel(errors.New("caller interruption"))
							case "concurrent-reset":
								done := make(chan struct{})
								go func() { tx.Reset(); close(done) }()
								<-done
							}
							if action == "reset-caller" {
								cancel(errors.New("caller outranks retirement"))
							}
							if action == "reset" || action == "onerror" || action == "commit" || action == "concurrent-reset" || action == "timeout-reset" || action == "reset-caller" {
								tx.Set([]byte("successor"), []byte("untouched"))
								tx.SetPriority(PriorityBatch)
								lease := tx.enterState()
								successor = lease.inc
								tx.retryCount, tx.backoff = 97, 77*time.Millisecond
								successorWatch, _ = tx.newWatchCtx(context.Background())
								lease.release()
							}
						}
						h := &scopeObserver{enabled: func(ctx context.Context) bool {
							enabledCalls++
							if site == "disabled" || site == "enabled-first" && enabledCalls == 1 || site == "enabled-second" && enabledCalls == 2 {
								reenter(ctx)
							}
							return site != "disabled"
						}}
						h.handle = func(ctx context.Context, r slog.Record) {
							handled++
							if site == "handle" {
								reenter(ctx)
							}
							// Reentry may have changed retryCount to 97. The outer
							// event must still report its pre-observer count of 1.
							r.Attrs(func(a slog.Attr) bool {
								if a.Key == "retry_count" && a.Value.Int64() == 1 {
									return true
								}
								if a.Key == "retry_count" && action != "onerror" {
									t.Errorf("event count changed after observer reentry: %v", a)
								}
								return true
							})
						}
						tx.db.logger = slog.New(h)
						err := tx.OnError(caller, &wire.FDBError{Code: code})
						if !called || site != "disabled" && handled == 0 {
							t.Fatalf("observer population empty: called=%v handled=%d", called, handled)
						}
						if guardFailed {
							return
						}
						if nestedErr != nil {
							t.Errorf("raw-caller nested operation failed: %v", nestedErr)
						}
						if action == "caller" || action == "reset-caller" {
							if err != context.Canceled {
								t.Errorf("outer result=%v, want caller Err (not custom Cause)", err)
							}
						} else if action == "timeout" || action == "timeout-reset" {
							requireScopeCode(t, err, 1031)
						} else {
							requireScopeCode(t, err, 1025)
						}
						tx.readErrMu.Lock()
						users, issued := inc.users, inc.issuedReads
						current := tx.readLife
						tx.readErrMu.Unlock()
						if users != 0 || issued != 0 {
							t.Errorf("outer authority leaked: users=%d issued=%d", users, issued)
						}
						if successor != nil {
							if current != successor || tx.state.Load() != int32(txStateActive) || tx.retryCount != 97 || tx.backoff != 77*time.Millisecond || tx.priority != PriorityBatch || successorWatch.Err() != nil || len(tx.mutations) != 1 {
								t.Errorf("stale outer changed successor: current=%v state=%v retries=%d backoff=%v priority=%v watch=%v mutations=%d", current == successor, tx.state.Load(), tx.retryCount, tx.backoff, tx.priority, successorWatch.Err(), len(tx.mutations))
							}
						}
					})
				})
			}
		}
	}
}

func TestOnErrorObserverPanicReleasesOwnership(t *testing.T) {
	t.Parallel()
	for _, site := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(site), func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			initial := tx.enterState()
			inc := initial.inc
			initial.release()
			marker := errors.New("observer panic")
			calls := 0
			invoke := func() {
				calls++
				if calls == site {
					panic(marker)
				}
			}
			tx.db = &database{logger: slog.New(&scopeObserver{
				enabled: func(context.Context) bool { invoke(); return true },
				handle:  func(context.Context, slog.Record) { invoke() },
			})}
			func() {
				defer func() {
					if got := recover(); got != marker {
						t.Errorf("panic=%v, want original marker", got)
					}
				}()
				_ = tx.OnError(context.Background(), &wire.FDBError{Code: 1020})
			}()
			tx.readErrMu.Lock()
			users, cause := inc.users, inc.cause
			tx.readErrMu.Unlock()
			if users != 0 || cause != nil || calls != site {
				t.Errorf("panic cleanup changed/retained authority: users=%d cause=%v calls=%d", users, cause, calls)
			}
			tx.Reset() // same-stack completion proves no surviving execution token
		})
	}
}

func retainedObserverWithoutOwner(t *testing.T, caller context.Context) (context.Context, weak.Pointer[Transaction]) {
	t.Helper()
	var observed context.Context
	tx := &Transaction{db: &database{logger: slog.New(&scopeObserver{enabled: func(ctx context.Context) bool { observed = ctx; return false }})}}
	tx.backoffJitter = func() float64 { return 0 }
	if err := tx.OnError(caller, &wire.FDBError{Code: 1020}); err != nil {
		t.Fatal(err)
	}
	return observed, weak.Make(tx)
}

func TestOnErrorRetainedObserverDoesNotRootTransaction(t *testing.T) {
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed, owner := retainedObserverWithoutOwner(t, caller)
	if observed != caller {
		t.Error("retained observer is not the public caller")
	}
	deadline := time.Now().Add(5 * time.Second)
	for owner.Value() != nil && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if owner.Value() != nil {
		t.Fatal("retained observer rooted the abandoned transaction")
	}
	if caller.Err() != nil || observed.Err() != nil {
		t.Fatal("collection depended on caller/observer cancellation")
	}
	runtime.KeepAlive(observed)
}
