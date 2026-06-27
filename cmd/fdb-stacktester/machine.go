package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire"
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// traceEnabled enables verbose stack tracing via STACKTESTER_TRACE=1.
var traceEnabled = os.Getenv("STACKTESTER_TRACE") != ""

// versionstampFuture is a deferred value that resolves to a versionstamp
// after the transaction commits. When popped from the stack, it calls
// GetVersionstamp() to get the 10-byte versionstamp.
type versionstampFuture struct {
	tr *client.Transaction
}

// stackEntry is a value on the operand stack.
type stackEntry struct {
	value any
	idx   int // instruction number that pushed this
}

// StackMachine executes binding tester instructions.
type StackMachine struct {
	db      *client.Database
	prefix  []byte
	trName  string
	stack   []stackEntry
	lastVer int64

	trMap map[string]*client.Transaction
	trMu  sync.RWMutex
	wg    sync.WaitGroup

	// Directory layer state.
	dirList       []dirEntry
	dirIndex      int
	dirErrorIndex int

	// Ring buffer of last 50 operations for crash diagnostics.
	opRing    [50]string
	opRingIdx int
}

// NewStackMachine creates a stack machine with the given prefix.
func NewStackMachine(db *client.Database, prefix []byte) *StackMachine {
	sm := &StackMachine{
		db:     db,
		prefix: prefix,
		trName: string(prefix),
		trMap:  make(map[string]*client.Transaction),
	}
	sm.trMap[sm.trName] = db.CreateTransaction()
	sm.initDirectoryState()
	return sm
}

// Run reads instructions from FDB and executes them.
func (sm *StackMachine) Run(ctx context.Context) error {
	// Read all instructions via range scan on prefix.
	kvs, err := sm.readInstructions(ctx)
	if err != nil {
		return fmt.Errorf("read instructions: %w", err)
	}

	for i, kv := range kvs {
		inst, err := tuple.Unpack(kv.Value)
		if err != nil {
			return fmt.Errorf("unpack instruction %d: %w", i, err)
		}

		op, ok := inst[0].(string)
		if !ok {
			return fmt.Errorf("instruction %d: operation is %T, want string", i, inst[0])
		}

		var arg any
		if len(inst) > 1 {
			arg = inst[1]
		}

		stackDepth := len(sm.stack)
		if err := sm.execute(ctx, i, op, arg); err != nil {
			return fmt.Errorf("instruction %d (%s): %w", i, op, err)
		}
		// Ring buffer of last 50 operations for crash diagnostics.
		sm.opRing[sm.opRingIdx%len(sm.opRing)] = fmt.Sprintf("#%d %s: stack %d→%d", i, op, stackDepth, len(sm.stack))
		sm.opRingIdx++
		if traceEnabled {
			newDepth := len(sm.stack)
			fmt.Fprintf(os.Stderr, "[TRACE] #%d %s: stack %d→%d", i, op, stackDepth, newDepth)
			if newDepth > 0 {
				top := sm.stack[newDepth-1]
				fmt.Fprintf(os.Stderr, " top=(%T, pushed@%d)", top.value, top.idx)
			}
			fmt.Fprintln(os.Stderr)
		}
	}

	// Wait for child threads with a timeout. The binding tester's directory
	// test spawns threads that can deadlock on WAIT_EMPTY. A timeout prevents
	// the stacktester from hanging indefinitely.
	done := make(chan struct{})
	go func() {
		sm.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		fmt.Fprintln(os.Stderr, "[WARN] timed out waiting for child threads")
	}
	return nil
}

func (sm *StackMachine) readInstructions(ctx context.Context) ([]client.KeyValue, error) {
	var all []client.KeyValue
	begin := tuple.Tuple{sm.prefix}.Pack()
	end := strinc(tuple.Tuple{sm.prefix}.Pack())

	for {
		result, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
			kvs, more, err := tx.GetRange(ctx, begin, end, 10000)
			return &rangeResult{kvs: kvs, more: more}, err
		})
		if err != nil {
			return nil, err
		}
		rr := result.(*rangeResult)
		all = append(all, rr.kvs...)
		if !rr.more || len(rr.kvs) == 0 {
			break
		}
		// Continue from after the last key.
		lastKey := rr.kvs[len(rr.kvs)-1].Key
		begin = append(lastKey, 0)
	}
	return all, nil
}

type rangeResult struct {
	kvs  []client.KeyValue
	more bool
}

func (sm *StackMachine) currentTr() *client.Transaction {
	sm.trMu.RLock()
	defer sm.trMu.RUnlock()
	return sm.trMap[sm.trName]
}

func (sm *StackMachine) push(idx int, value any) {
	sm.stack = append(sm.stack, stackEntry{value: value, idx: idx})
}

func (sm *StackMachine) pop() stackEntry {
	n := len(sm.stack)
	e := sm.stack[n-1]
	sm.stack = sm.stack[:n-1]

	// Resolve futures.
	switch f := e.value.(type) {
	case *versionstampFuture:
		vs, err := f.tr.GetVersionstamp()
		if err != nil {
			// Pack error as tuple: ("ERROR", "code")
			var fdbErr *wire.FDBError
			code := "0"
			if errors.As(err, &fdbErr) {
				code = strconv.Itoa(fdbErr.Code)
			}
			e.value = tuple.Tuple{[]byte("ERROR"), []byte(code)}.Pack()
		} else {
			e.value = vs
		}
	}
	return e
}

func (sm *StackMachine) popBytes() []byte {
	e := sm.pop()
	switch v := e.value.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	case fdb.Key:
		return []byte(v)
	case fdb.KeyConvertible:
		return []byte(v.FDBKey())
	default:
		panic(fmt.Sprintf("expected bytes, got %T", e.value))
	}
}

func (sm *StackMachine) popInt64() int64 {
	e := sm.pop()
	switch v := e.value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case *big.Int:
		return v.Int64()
	default:
		// Dump stack state for debugging before panicking.
		fmt.Fprintf(os.Stderr, "\n=== STACK DUMP (popInt64 type mismatch) ===\n")
		if b, ok := e.value.([]byte); ok {
			fmt.Fprintf(os.Stderr, "Popped: idx=%d type=[]byte ascii=%q\n", e.idx, string(b))
		} else {
			fmt.Fprintf(os.Stderr, "Popped: idx=%d type=%T value=%v\n", e.idx, e.value, e.value)
		}
		fmt.Fprintf(os.Stderr, "Stack depth: %d\n", len(sm.stack))
		for i := len(sm.stack) - 1; i >= max(0, len(sm.stack)-10); i-- {
			se := sm.stack[i]
			fmt.Fprintf(os.Stderr, "  [%d] idx=%d type=%T value=%v\n", i, se.idx, se.value, se.value)
		}
		fmt.Fprintf(os.Stderr, "=== END STACK DUMP ===\n\n")
		// Dump last 50 operations from ring buffer.
		fmt.Fprintf(os.Stderr, "=== LAST %d OPERATIONS ===\n", min(sm.opRingIdx, len(sm.opRing)))
		start := 0
		if sm.opRingIdx > len(sm.opRing) {
			start = sm.opRingIdx % len(sm.opRing)
		}
		count := min(sm.opRingIdx, len(sm.opRing))
		for i := 0; i < count; i++ {
			fmt.Fprintf(os.Stderr, "  %s\n", sm.opRing[(start+i)%len(sm.opRing)])
		}
		fmt.Fprintf(os.Stderr, "=== END OPERATIONS ===\n\n")
		panic(fmt.Sprintf("expected int64, got %T: %v", e.value, e.value))
	}
}

func (sm *StackMachine) popString() string {
	e := sm.pop()
	switch v := e.value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		panic(fmt.Sprintf("expected string, got %T", e.value))
	}
}

func (sm *StackMachine) pushError(idx int, err error) {
	// Pack error as tuple: ("ERROR", "code")
	code := "0"
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) {
		code = strconv.Itoa(fdbErr.Code)
	}
	packed := tuple.Tuple{[]byte("ERROR"), []byte(code)}.Pack()
	sm.push(idx, packed)
}

// strinc returns the next byte string after prefix (for range end).
func strinc(prefix []byte) []byte {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] < 0xff {
			result := make([]byte, i+1)
			copy(result, prefix)
			result[i]++
			return result
		}
	}
	return nil
}
