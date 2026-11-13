//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func writeThreadInstructions(t *testing.T, ctx context.Context, db *client.Database, programs map[string][]tuple.Tuple, heldKey []byte) {
	t.Helper()
	_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		for prefix, instructions := range programs {
			for i, inst := range instructions {
				tx.Set(tuple.Tuple{[]byte(prefix), int64(i)}.Pack(), inst.Pack())
			}
		}
		if heldKey != nil {
			tx.Set(heldKey, []byte("held"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertThreadWorkersJoined(t *testing.T, sm *StackMachine) {
	t.Helper()
	done := make(chan struct{})
	go func() { sm.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run returned with unfinished child threads")
	}
}

func TestRunRejectsFailedChildAndDescendant(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, nested := range []bool{false, true} {
		name := "direct"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			root, child, leaf, sibling := name+"/root", name+"/child", name+"/leaf", name+"/sibling"
			held := []byte(name + "/held")
			programs := map[string][]tuple.Tuple{
				root:    {{"PUSH", []byte(sibling)}, {"START_THREAD"}, {"PUSH", []byte(child)}, {"START_THREAD"}, {"PUSH", held}, {"WAIT_EMPTY"}},
				child:   {{"CHILD_FAILURE"}},
				sibling: {{"PUSH", held}, {"WAIT_EMPTY"}},
			}
			if nested {
				programs[child] = []tuple.Tuple{{"PUSH", []byte(leaf)}, {"START_THREAD"}}
				programs[leaf] = []tuple.Tuple{{"CHILD_FAILURE"}}
			}
			writeThreadInstructions(t, ctx, db, programs, held)
			sm := NewStackMachine(db, []byte(root))
			runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			err := sm.Run(runCtx)
			if runCtx.Err() != nil {
				t.Fatalf("child failure did not interrupt blocked parent and sibling: %v", runCtx.Err())
			}
			if err == nil || !strings.Contains(err.Error(), "CHILD_FAILURE") || !strings.Contains(err.Error(), child) {
				t.Fatalf("Run accepted failed child or lost diagnostic: %v", err)
			}
			if nested && !strings.Contains(err.Error(), leaf) {
				t.Fatalf("missing descendant diagnostic: %v", err)
			}
			assertThreadWorkersJoined(t, sm)
		})
	}
}

func TestRunRejectsBlockedChildOnJoinTimeout(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	root, child := "timeout/root", "timeout/child"
	held := []byte("timeout/held")
	completed := []byte("timeout/completed")
	writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
		root:  {{"PUSH", []byte(child)}, {"START_THREAD"}},
		child: {{"PUSH", held}, {"WAIT_EMPTY"}, {"PUSH", []byte("completed")}, {"PUSH", completed}, {"SET_DATABASE"}},
	}, held)
	sm := NewStackMachine(db, []byte(root))
	sm.childJoinTimeout = 200 * time.Millisecond
	err := sm.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for child threads") {
		t.Fatalf("blocked worker was accepted: %v", err)
	}
	assertThreadWorkersJoined(t, sm)
	result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { return tx.Get(ctx, completed) })
	if err != nil {
		t.Fatal(err)
	}
	if len(result.([]byte)) != 0 {
		t.Fatal("blocked child completed despite uncleared coordination key")
	}
}

func TestRunCancelsBlockedChildrenOnCallerOrParentFailure(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, parentFailure := range []bool{false, true} {
		name := "caller"
		if parentFailure {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			root, child := name+"/root", name+"/child"
			held, ready, parentBarrier := []byte(name+"/held"), []byte(name+"/ready"), []byte(name+"/parent-barrier")
			parent := []tuple.Tuple{{"PUSH", []byte(child)}, {"START_THREAD"}}
			if parentFailure {
				parent = append(parent, tuple.Tuple{"PUSH", parentBarrier}, tuple.Tuple{"WAIT_EMPTY"}, tuple.Tuple{"PARENT_FAILURE"})
			}
			writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
				root:  parent,
				child: {{"PUSH", []byte("ready")}, {"PUSH", ready}, {"SET_DATABASE"}, {"PUSH", held}, {"WAIT_EMPTY"}},
			}, held)
			if parentFailure {
				_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Set(parentBarrier, []byte("held")); return nil, nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			sm := NewStackMachine(db, []byte(root))
			errs := make(chan error, 1)
			go func() { errs <- sm.Run(runCtx) }()
			// Wait for a real child write before triggering cancellation/failure.
			readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
			defer readyCancel()
			for {
				result, err := db.Transact(readyCtx, func(tx *client.Transaction) (any, error) { return tx.Get(readyCtx, ready) })
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(result.([]byte), []byte("ready")) {
					break
				}
				select {
				case err := <-errs:
					t.Fatalf("Run ended before child became ready: %v", err)
				case <-readyCtx.Done():
					t.Fatal(readyCtx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if parentFailure {
				_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Clear(parentBarrier); return nil, nil })
				if err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-errs:
				if parentFailure {
					if err == nil || !strings.Contains(err.Error(), "PARENT_FAILURE") {
						t.Fatalf("parent error=%v", err)
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller cancellation=%v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not cancel blocked child")
			}
			assertThreadWorkersJoined(t, sm)
		})
	}
}

func TestSuccessfulChildDoesNotCancelParentOrSibling(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	root, child, sibling := "success/root", "success/child", "success/sibling"
	completed, siblingCompleted, held := []byte("success/completed"), []byte("success/sibling-completed"), []byte("success/held")
	writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
		root:    {{"PUSH", []byte(sibling)}, {"START_THREAD"}, {"PUSH", []byte(child)}, {"START_THREAD"}},
		child:   {{"PUSH", []byte("completed")}, {"PUSH", completed}, {"SET_DATABASE"}},
		sibling: {{"PUSH", held}, {"WAIT_EMPTY"}, {"PUSH", []byte("completed")}, {"PUSH", siblingCompleted}, {"SET_DATABASE"}},
	}, held)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sm := NewStackMachine(db, []byte(root))
	errs := make(chan error, 1)
	go func() { errs <- sm.Run(runCtx) }()
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	for {
		result, err := db.Transact(readyCtx, func(tx *client.Transaction) (any, error) { return tx.Get(readyCtx, completed) })
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(result.([]byte), []byte("completed")) {
			break
		}
		select {
		case err := <-errs:
			t.Fatalf("parent stopped before first child completed: %v", err)
		case <-readyCtx.Done():
			t.Fatal(readyCtx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case err := <-errs:
		t.Fatalf("parent abandoned blocked sibling: %v", err)
	default:
	}
	_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Clear(held); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parent did not join successful sibling")
	}
	assertThreadWorkersJoined(t, sm)
	if runCtx.Err() != nil || ctx.Err() != nil {
		t.Fatal("successful Run canceled its caller context")
	}
	result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) { return tx.Get(ctx, siblingCompleted) })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.([]byte), []byte("completed")) {
		t.Fatalf("sibling output=%q", result)
	}
}
