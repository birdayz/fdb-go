//go:build bazelrunfiles

package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire/types"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func TestRunReleasesDispatchedCommitWaitsDuringFailure(t *testing.T) {
	t.Parallel()
	gate := &directoryReplyGate{fileID: types.CommitIDFileID}
	db, ctx := stacktesterDatabase(t, client.WithDialFunc(gate.dial))
	t.Cleanup(gate.release)
	for _, mode := range []string{"named", "database", "directory", "log-stack"} {
		for _, failure := range []string{"join-timeout", "caller", "parent"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				name := mode + "/" + failure
				root, child := name+"/root", name+"/child"
				barrier, key := []byte(name+"/barrier"), []byte(name+"/key")
				program := []tuple.Tuple{{"PUSH", []byte("value")}, {"PUSH", key}, {"SET"}, {"COMMIT"}}
				switch mode {
				case "database":
					program = []tuple.Tuple{{"PUSH", []byte("value")}, {"PUSH", key}, {"SET_DATABASE"}}
				case "directory":
					program = []tuple.Tuple{{"PUSH", []byte{}}, {"PUSH", name}, {"PUSH", int64(1)}, {"DIRECTORY_CREATE_OR_OPEN_DATABASE"}}
				case "log-stack":
					program = []tuple.Tuple{{"PUSH", []byte("value")}, {"PUSH", key}, {"LOG_STACK"}}
				}
				parent := []tuple.Tuple{{"PUSH", []byte(child)}, {"START_THREAD"}, {"PUSH", barrier}, {"WAIT_EMPTY"}}
				if failure == "parent" {
					parent = append(parent, tuple.Tuple{"PARENT_FAILURE"})
				}
				writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{root: parent, child: program}, barrier)
				sm := NewStackMachine(db, []byte(root))
				sm.childJoinTimeout = 500 * time.Millisecond
				runCtx, cancel := context.WithCancelCause(ctx)
				defer cancel(context.Canceled)
				cause := errors.New("caller interrupted dispatched commit")
				gate.arm()
				defer gate.release()
				done := make(chan error, 1)
				go func() { done <- sm.Run(runCtx) }()
				select {
				case <-gate.reached:
				case err := <-done:
					t.Fatalf("Run ended before commit reply gate: %v", err)
				case <-time.After(30 * time.Second):
					t.Fatal("child commit was not dispatched")
				}
				var clearDone chan error
				if failure == "caller" {
					cancel(cause)
				} else {
					clearDone = make(chan error, 1)
					go func() {
						_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Clear(barrier); return nil, nil })
						clearDone <- err
					}()
				}
				select {
				case err := <-done:
					if err == nil || (failure == "caller" && !errors.Is(err, cause)) || (failure == "parent" && !strings.Contains(err.Error(), "PARENT_FAILURE")) || (failure == "join-timeout" && !strings.Contains(err.Error(), "timed out waiting for child threads")) {
						t.Fatalf("Run lost %s failure: %v", failure, err)
					}
				case <-time.After(12 * time.Second):
					t.Fatal("Run hung with commit reply still withheld")
				}
				assertThreadWorkersJoined(t, sm)
				gate.mu.Lock()
				held, armed := len(gate.held), gate.armed
				gate.mu.Unlock()
				if held == 0 || !armed {
					t.Fatal("test released commit reply before proving teardown")
				}
				gate.release()
				if clearDone != nil {
					select {
					case err := <-clearDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("barrier clear did not finish")
					}
				}
			})
		}
	}
}

func TestRootCommitCancellationReturnsBeforeReply(t *testing.T) {
	t.Parallel()
	gate := &directoryReplyGate{fileID: types.CommitIDFileID}
	db, ctx := stacktesterDatabase(t, client.WithDialFunc(gate.dial))
	t.Cleanup(gate.release)
	root := "root-commit/instructions"
	writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
		root: {{"PUSH", []byte("value")}, {"PUSH", []byte("root-commit/key")}, {"SET"}, {"COMMIT"}},
	}, nil)
	sm := NewStackMachine(db, []byte(root))
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	cause := errors.New("cancel root commit after dispatch")
	gate.arm()
	done := make(chan error, 1)
	go func() { done <- sm.Run(runCtx) }()
	select {
	case <-gate.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("root commit did not reach reply gate")
	}
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("root cancellation=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("root's own dispatched commit ignored cancellation")
	}
	gate.mu.Lock()
	held := len(gate.held)
	gate.mu.Unlock()
	if held == 0 {
		t.Fatal("root commit reply escaped gate")
	}
}

func TestRunFailsClosedWhenWorkerCannotJoin(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	sm := NewStackMachine(db, []byte("unjoined/root"))
	sm.childJoinTimeout = 10 * time.Millisecond
	sm.children = map[string]int{"uncooperative-worker": 1}
	sm.wg.Add(1)
	release := make(chan struct{})
	defer close(release)
	go func() { <-release; sm.wg.Done() }()
	done := make(chan error, 1)
	go func() { done <- sm.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out waiting") || !strings.Contains(err.Error(), "unjoined child threads: map[uncooperative-worker:1]") {
			t.Fatalf("unjoined worker lost failure: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("cleanup did not honor its bound")
	}
}

func TestTransactionWaitsStoppingRejectsLateRegistration(t *testing.T) {
	t.Parallel()
	var waits transactionWaits
	var first, late atomic.Int32
	canceled := make(chan struct{})
	release := waits.track(func() { first.Add(1); close(canceled) })
	waits.stop(context.Canceled)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("registered wait was not canceled")
	}
	release()
	waits.track(func() { late.Add(1) })()
	waits.stop(context.Canceled)
	if first.Load() != 1 || late.Load() != 1 {
		t.Fatalf("cancel counts=%d,%d", first.Load(), late.Load())
	}
}
