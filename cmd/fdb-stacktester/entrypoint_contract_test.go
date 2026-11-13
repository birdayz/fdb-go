//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func TestRunCancellationStopsBetweenPersistedInstructions(t *testing.T) {
	t.Parallel()
	gate := &directoryReplyGate{}
	db, ctx := stacktesterDatabase(t, client.WithDialFunc(gate.dial))
	t.Cleanup(gate.release)
	root := "between-instructions/root"
	readKey, writeKey := []byte("between-instructions/read"), []byte("between-instructions/write")
	writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
		root: {
			{"PUSH", readKey},
			{"GET"},
			{"PUSH", int64(9)},
			{"PUSH", int64(4)},
			{"SUB"},
			{"PUSH", []byte("value")},
			{"PUSH", writeKey},
			{"SET"},
		},
	}, readKey)
	sm := NewStackMachine(db, []byte(root))
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	cause := errors.New("cancel between persisted instructions")
	gate.arm()
	done := make(chan error, 1)
	go func() { done <- sm.Run(runCtx) }()
	select {
	case <-gate.reached:
	case err := <-done:
		t.Fatalf("Run ended before read barrier: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("persisted read did not reach barrier")
	}
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("Run cancellation=%v, want %v", err, cause)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	assertThreadWorkersJoined(t, sm)
	wantError := tuple.Tuple{[]byte("ERROR"), []byte("0")}.Pack()
	if sm.opRingIdx != 2 || len(sm.stack) != 1 || sm.stack[0].idx != 1 || !bytes.Equal(sm.stack[0].value.([]byte), wantError) {
		t.Fatalf("instructions after canceled GET ran: completed=%d stack=%+v", sm.opRingIdx, sm.stack)
	}
	gate.release()
	if value, err := sm.currentTr().Get(ctx, writeKey); err != nil || value != nil {
		t.Fatalf("canceled program buffered SET: value=%q err=%v", value, err)
	}

	control := NewStackMachine(db, []byte(root))
	if err := control.Run(ctx); err != nil {
		t.Fatal(err)
	}
	assertThreadWorkersJoined(t, control)
	if control.opRingIdx != 8 || len(control.stack) != 2 || control.stack[1].value.(*big.Int).Cmp(big.NewInt(-5)) != 0 {
		t.Fatalf("positive program control: completed=%d stack=%+v", control.opRingIdx, control.stack)
	}
	if value, err := control.currentTr().Get(ctx, writeKey); err != nil || !bytes.Equal(value, []byte("value")) {
		t.Fatalf("positive SET control: value=%q err=%v", value, err)
	}
}

func TestCLISelectsAPIVersionForPersistedVersionstampInstructions(t *testing.T) {
	t.Parallel()
	db, ctx, clusterFile := stacktesterDatabaseWithClusterFile(t)
	binary := filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), "cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester")
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		t.Fatalf("declared stacktester binary unavailable: %s: %v", binary, err)
	}
	for _, tc := range []struct {
		api            int
		single, nested string
	}{
		{510, "6100ff33ffffffffffffffffffff12340400", "6100ff05300100000000000000000000000000000033ffffffffffffffffffff1234001600"},
		{610, "6100ff33ffffffffffffffffffff123404000000", "6100ff05300100000000000000000000000000000033ffffffffffffffffffff12340016000000"},
		{730, "6100ff33ffffffffffffffffffff123404000000", "6100ff05300100000000000000000000000000000033ffffffffffffffffffff12340016000000"},
	} {
		t.Run(strconv.Itoa(tc.api), func(t *testing.T) {
			root := fmt.Sprintf("cli-api/%d/instructions", tc.api)
			logPrefix := []byte(fmt.Sprintf("cli-api/%d/log/", tc.api))
			// Incomplete stamps cannot be persisted with vanilla tuple.Pack.
			// Literal PUSH values also exercise the CLI instruction decoder.
			singlePush, err := hex.DecodeString("02505553480033ffffffffffffffffffff1234")
			if err != nil {
				t.Fatal(err)
			}
			nestedPush, err := hex.DecodeString("02505553480005300100000000000000000000000000000033ffffffffffffffffffff123400")
			if err != nil {
				t.Fatal(err)
			}
			program := []tuple.Tuple{
				{"PUSH"},
				{"PUSH", int64(1)},
				{"PUSH", []byte{0x61, 0, 0xff}},
				{"TUPLE_PACK_WITH_VERSIONSTAMP"},
				{"PUSH"},
				{"PUSH", int64(1)},
				{"PUSH", []byte{0x61, 0, 0xff}},
				{"TUPLE_PACK_WITH_VERSIONSTAMP"},
				{"PUSH", logPrefix},
				{"LOG_STACK"},
			}
			_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				for i, inst := range program {
					value := inst.Pack()
					if i == 0 {
						value = singlePush
					}
					if i == 4 {
						value = nestedPush
					}
					tx.Set(tuple.Tuple{[]byte(root), int64(i)}.Pack(), value)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(runCtx, binary, root, strconv.Itoa(tc.api), clusterFile)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("actual CLI API%d: %v\n%s", tc.api, err, output)
			}
			rows, more, err := db.CreateTransaction().GetRange(ctx, logPrefix, strinc(logPrefix), 10)
			if err != nil || more || len(rows) != 4 {
				t.Fatalf("CLI log population=%d more=%v err=%v; want4", len(rows), more, err)
			}
			for i, wantHex := range []string{"4f4b", tc.single, "4f4b", tc.nested} {
				want, err := hex.DecodeString(wantHex)
				if err != nil {
					t.Fatal(err)
				}
				instruction := int64(3)
				if i >= 2 {
					instruction = 7
				}
				wantKey := append(bytes.Clone(logPrefix), tuple.Tuple{int64(i), instruction}.Pack()...)
				if !bytes.Equal(rows[i].Key, wantKey) {
					t.Fatalf("log row%d key=%x want%x", i, rows[i].Key, wantKey)
				}
				value, err := tuple.Unpack(rows[i].Value)
				if err != nil || len(value) != 1 {
					t.Fatalf("log row%d value=%x decoded=%v err=%v", i, rows[i].Value, value, err)
				}
				got, ok := value[0].([]byte)
				if !ok || !bytes.Equal(got, want) {
					t.Fatalf("API%d row%d packed=%x want literal%x", tc.api, i, got, want)
				}
			}
		})
	}
}

func TestRunStartThreadSharesNamedTransactionReplacement(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	root, child, name := "registry/root", "registry/child", "registry/shared"
	oldKey, newKey := []byte("registry/old"), []byte("registry/new")
	writeThreadInstructions(t, ctx, db, map[string][]tuple.Tuple{
		root: {
			{"PUSH", []byte(name)},
			{"USE_TRANSACTION"},
			{"PUSH", []byte("discarded parent write")},
			{"PUSH", oldKey},
			{"SET"},
			{"PUSH", []byte(child)},
			{"START_THREAD"},
		},
		child: {
			{"PUSH", []byte(name)},
			{"USE_TRANSACTION"},
			{"NEW_TRANSACTION"},
			{"PUSH", []byte("child replacement write")},
			{"PUSH", newKey},
			{"SET"},
		},
	}, nil)
	sm := NewStackMachine(db, []byte(root))
	// Preselect to capture the handle that the persisted child must replace.
	sm.push(0, []byte(name))
	if err := sm.execute(ctx, 1, "USE_TRANSACTION", nil); err != nil {
		t.Fatal(err)
	}
	old := sm.currentTr()
	if err := sm.Run(ctx); err != nil {
		t.Fatal(err)
	}
	assertThreadWorkersJoined(t, sm)
	if sm.currentTr() == old {
		t.Fatal("START_THREAD child did not replace parent's named transaction")
	}
	sm.trMu.RLock()
	childInitial := sm.trMap[child]
	sm.trMu.RUnlock()
	if childInitial == nil {
		t.Fatal("START_THREAD did not register child's initial transaction in parent registry")
	}
	if err := sm.currentTr().Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		oldValue, err := tx.Get(ctx, oldKey)
		if err != nil {
			return nil, err
		}
		newValue, err := tx.Get(ctx, newKey)
		return [][]byte{oldValue, newValue}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	values := result.([][]byte)
	if len(values[0]) != 0 || !bytes.Equal(values[1], []byte("child replacement write")) {
		t.Fatalf("parent commit used wrong registry handle: old=%q new=%q", values[0], values[1])
	}
}

func TestWaitEmptyDispatchPreservesCauseWithoutSuccessMarker(t *testing.T) {
	t.Parallel()
	cause := errors.New("wait-empty distinct cancellation cause")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	sm := &StackMachine{}
	sm.push(11, []byte("unused-prefix"))
	err := sm.execute(ctx, 12, "WAIT_EMPTY", nil)
	if !errors.Is(err, cause) {
		t.Fatalf("WAIT_EMPTY error=%v want exact cancellation cause %v", err, cause)
	}
	if len(sm.stack) != 0 {
		t.Fatalf("failed WAIT_EMPTY pushed success or retained operand: %+v", sm.stack)
	}
}

func TestWaitEmptyDispatchReturnsReadAndBackoffErrors(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	prefix := []byte("wait-empty/held/")
	_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		tx.Set(prefix, []byte("held"))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		code int
	}{{"read", 2006}, {"backoff", 1020}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "read" {
				db.SetTransactionSizeLimit(1)
				defer db.SetTransactionSizeLimit(0)
			} else {
				db.SetTransactionRetryLimit(0)
				defer db.SetTransactionRetryLimit(-1)
			}
			sm := NewStackMachine(db, []byte("wait-empty/"+tc.name))
			sm.push(11, prefix)
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			err := sm.execute(bounded, 12, "WAIT_EMPTY", nil)
			var fdbErr *wire.FDBError
			if bounded.Err() != nil || !errors.As(err, &fdbErr) || fdbErr.Code != tc.code {
				t.Fatalf("WAIT_EMPTY %s: error=%v budget=%v want code%d before budget", tc.name, err, bounded.Err(), tc.code)
			}
			if len(sm.stack) != 0 {
				t.Fatalf("failed WAIT_EMPTY stack=%+v", sm.stack)
			}
		})
	}
	// Positive control: a genuinely empty prefix returns the success marker.
	sm := NewStackMachine(db, []byte("wait-empty/success"))
	sm.push(11, []byte("wait-empty/absent/"))
	if err := sm.execute(ctx, 12, "WAIT_EMPTY", nil); err != nil {
		t.Fatal(err)
	}
	if len(sm.stack) != 1 || sm.stack[0].idx != 12 || !bytes.Equal(sm.popBytes(), []byte("WAITED_FOR_EMPTY")) {
		t.Fatalf("successful WAIT_EMPTY stack=%+v", sm.stack)
	}
}
