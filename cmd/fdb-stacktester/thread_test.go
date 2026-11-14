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

func TestDescendantJoinTimeoutInterruptsBlockedRoot(t *testing.T) {
	t.Parallel()
	gate := &directoryReplyGate{}
	db, ctx := stacktesterDatabase(t, client.WithDialFunc(gate.dial))
	t.Cleanup(gate.release)
	root, child, leaf := "inherited/root", "inherited/child", "inherited/leaf"
	rootBarrier := []byte("inherited/root-barrier")
	childBarrier := []byte("inherited/child-barrier")
	rootReady := []byte("inherited/root-ready")
	scope := "inherited-directory-scope"
	sm := NewStackMachine(db, []byte(root))
	if _, err := sm.dirList[0].dir.CreateOrOpen(sm.fdbDB(ctx), []string{scope}, nil); err != nil {
		t.Fatal(err)
	}
	writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
		root: {
			{"PUSH", []byte(child)},
			{"START_THREAD"},
			{"PUSH", rootReady},
			{"CLEAR_DATABASE"},
			{"PUSH", rootBarrier},
			{"WAIT_EMPTY"},
		},
		child: {
			{"PUSH", []byte(leaf)},
			{"START_THREAD"},
			{"PUSH", childBarrier},
			{"WAIT_EMPTY"},
		},
		leaf: {
			{"PUSH", scope},
			{"PUSH", int64(1)},
			{"PUSH", int64(1)},
			{"DIRECTORY_EXISTS_DATABASE"},
		},
	}, rootBarrier)
	if _, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		tx.Set(childBarrier, []byte("held"))
		tx.Set(rootReady, []byte("held"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// While armed, inspect coordination keys with ranges: a point Get would
	// itself be caught by the directory reply gate and invalidate readiness.
	readCoordination := func(key []byte) []byte {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		tr := db.CreateTransaction()
		defer tr.Cancel()
		rows, more, err := tr.GetRange(readCtx, key, append(bytes.Clone(key), 0), 2)
		if err != nil || more || len(rows) > 1 {
			t.Fatalf("coordination range%q rows=%v more=%v err=%v", key, rows, more, err)
		}
		if len(rows) == 0 {
			return nil
		}
		if !bytes.Equal(rows[0].Key, key) {
			t.Fatalf("coordination key=%q want%q", rows[0].Key, key)
		}
		return rows[0].Value
	}
	heldReply := func() {
		t.Helper()
		gate.mu.Lock()
		armed, held := gate.armed, len(gate.held)
		gate.mu.Unlock()
		if !armed || held == 0 {
			t.Fatalf("directory reply released before join proof: armed=%v held=%d", armed, held)
		}
	}

	sm.childJoinTimeout = 2 * time.Second
	runCtx, cancel := context.WithCancel(ctx)
	gate.arm()
	errs := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		errs <- sm.Run(runCtx)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		gate.release()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("root worker did not stop during test cleanup")
		}
	})
	select {
	case <-gate.reached:
	case err := <-errs:
		t.Fatalf("Run ended before grandchild directory read: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("grandchild never reached real held directory read")
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := sm.waitEmpty(readyCtx, rootReady); err != nil {
		t.Fatalf("root did not reach barrier setup: %v", err)
	}
	heldReply()
	if !bytes.Equal(readCoordination(rootBarrier), []byte("held")) || !bytes.Equal(readCoordination(childBarrier), []byte("held")) {
		t.Fatal("root/child barriers not both held before starting descendant join")
	}
	select {
	case err := <-errs:
		t.Fatalf("Run finished while child barrier still held: %v", err)
	default:
	}
	// Release ONLY the child barrier. The root cannot finish instructions or
	// start its own timer; the leaf cannot finish its held directory read.
	if _, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		tx.Clear(childBarrier)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err == nil || !strings.Contains(err.Error(), child) || !strings.Contains(err.Error(), "timed out waiting for child threads after 2s") || strings.Contains(err.Error(), "unjoined child threads") {
			t.Fatalf("descendant timeout=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not use inherited join timeout while root barrier/directory reply remained held")
	}
	assertThreadWorkersJoined(t, sm)
	sm.childMu.Lock()
	activeChildren := len(sm.children)
	sm.childMu.Unlock()
	if activeChildren != 0 {
		t.Fatalf("root retained%d children after join", activeChildren)
	}
	heldReply()
	if !bytes.Equal(readCoordination(rootBarrier), []byte("held")) || len(readCoordination(childBarrier)) != 0 {
		t.Fatal("wrong barrier released: root must remain held and child must be clear")
	}
	if runCtx.Err() != nil || ctx.Err() != nil {
		t.Fatalf("descendant timeout canceled caller context: run=%v db=%v", runCtx.Err(), ctx.Err())
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
