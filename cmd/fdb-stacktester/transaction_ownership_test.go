//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire/types"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func activeTransactionRegistrations(sm *StackMachine) int {
	sm.waits.mu.Lock()
	defer sm.waits.mu.Unlock()
	return len(sm.waits.active)
}

func TestNamedTransactionCommitBorrowersReleaseLastRegistration(t *testing.T) {
	t.Parallel()
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "stop-before-release"}[stop], func(t *testing.T) {
			registry := &transactionRegistry{trMap: make(map[string]*namedTransaction)}
			canceled := make(chan struct{}, 1)
			handle := &namedTransaction{refs: 1, untrack: registry.waits.track(func() { canceled <- struct{}{} })}
			registry.trMap["shared"] = handle
			first := &StackMachine{trName: "shared", transactionRegistry: registry}
			second := &StackMachine{trName: "shared", transactionRegistry: registry}
			_, releaseFirst := first.acquireCommitTransaction()
			_, releaseSecond := second.acquireCommitTransaction()
			registry.trMu.Lock()
			delete(registry.trMap, "shared")
			first.releaseTransaction(handle)
			registry.trMu.Unlock()
			if handle.refs != 2 || activeTransactionRegistrations(first) != 1 {
				t.Fatal("retired slot lost borrower ownership")
			}
			releaseFirst()
			if handle.refs != 1 || activeTransactionRegistrations(first) != 1 {
				t.Fatal("first borrower unregistered the second")
			}
			if stop {
				registry.waits.stop(context.Canceled)
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("stop did not cancel retired borrowed transaction")
				}
			}
			releaseSecond()
			if handle.refs != 0 || activeTransactionRegistrations(first) != 0 {
				t.Fatal("last borrower retained registration")
			}
			select {
			case <-canceled:
				t.Fatal("release canceled a retired transaction")
			default:
			}
		})
	}
}

func TestNamedTransactionReplacementReleasesRetiredRegistrations(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("bounded-handles"))
	value := bytes.Repeat([]byte("large-value"), 8192)
	for i := 0; i < 128; i++ {
		sm.push(i, value)
		sm.push(i, []byte("buffered-key"))
		if err := sm.execute(ctx, i, "SET", nil); err != nil {
			t.Fatal(err)
		}
		old := sm.currentTr()
		for _, op := range []string{"NEW_TRANSACTION", "RESET"} {
			if err := sm.execute(ctx, i, op, nil); err != nil {
				t.Fatal(err)
			}
			if sm.currentTr() == old || activeTransactionRegistrations(sm) != 1 {
				t.Fatalf("%s iteration %d retained retired buffered handle: registrations=%d", op, i, activeTransactionRegistrations(sm))
			}
			old = sm.currentTr()
		}
	}
	sm.push(0, []byte("bounded-handles"))
	if err := sm.execute(ctx, 0, "USE_TRANSACTION", nil); err != nil {
		t.Fatal(err)
	}
	child := newStackMachine(db, []byte("bounded-handles"), sm.transactionRegistry)
	if child.currentTr() != sm.currentTr() || activeTransactionRegistrations(sm) != 1 {
		t.Fatal("existing-name USE_TRANSACTION or child initialization retained old registration")
	}
}

func TestNamedTransactionReplacementRetainsDispatchedCommit(t *testing.T) {
	t.Parallel()
	for _, replacement := range []string{"NEW_TRANSACTION", "RESET"} {
		for _, stop := range []bool{false, true} {
			t.Run(replacement+map[bool]string{false: "/success", true: "/cancel"}[stop], func(t *testing.T) {
				gate := &directoryReplyGate{fileID: types.CommitIDFileID}
				db, ctx := stacktesterDatabase(t, client.WithDialFunc(gate.dial))
				t.Cleanup(gate.release)
				root := "replacement-commit/root"
				writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{root: {{"COMMIT"}}}, nil)
				worker := NewStackMachine(db, []byte(root))
				worker.currentTr().Set([]byte("replacement-commit/key"), []byte("value"))
				replacer := newStackMachine(db, []byte("replacement-controller"), worker.transactionRegistry)
				replacer.push(0, []byte(root))
				if err := replacer.execute(ctx, 0, "USE_TRANSACTION", nil); err != nil {
					t.Fatal(err)
				}
				old := worker.currentTr()
				runCtx, cancel := context.WithCancelCause(ctx)
				defer cancel(context.Canceled)
				gate.arm()
				done := make(chan error, 1)
				go func() { done <- worker.Run(runCtx) }()
				select {
				case <-gate.reached:
				case <-time.After(30 * time.Second):
					t.Fatal("commit did not reach held reply")
				}
				if err := replacer.execute(ctx, 0, replacement, nil); err != nil {
					t.Fatal(err)
				}
				if replacer.currentTr() == old || activeTransactionRegistrations(worker) != 3 {
					t.Fatalf("replacement lost slot or active commit ownership: registrations=%d", activeTransactionRegistrations(worker))
				}
				select {
				case err := <-done:
					t.Fatalf("replacement prematurely completed/canceled old commit: %v", err)
				default:
				}
				cause := errors.New("stop with replaced commit still dispatched")
				if stop {
					cancel(cause)
				} else {
					gate.release()
				}
				select {
				case err := <-done:
					if (stop && !errors.Is(err, cause)) || (!stop && err != nil) {
						t.Fatalf("replaced commit outcome=%v stop=%v", err, stop)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("replaced commit did not join")
				}
				if stop {
					gate.mu.Lock()
					held, armed := len(gate.held), gate.armed
					gate.mu.Unlock()
					if held == 0 || !armed {
						t.Fatal("reply released before proving replaced commit cancellation")
					}
				} else {
					if len(worker.stack) != 1 || !bytes.Equal(worker.popBytes(), []byte("RESULT_NOT_PRESENT")) {
						t.Fatal("replacement canceled the old commit instead of preserving successful result")
					}
					if n := activeTransactionRegistrations(worker); n != 2 {
						t.Fatalf("completed retired commit registration leaked: %d", n)
					}
				}
				assertThreadWorkersJoined(t, worker)
			})
		}
	}
}
