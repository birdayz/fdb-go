//go:build bazelrunfiles

package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"fdb.dev/internal/fdbgo/client"
	"fdb.dev/internal/fdbwire"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func splitOpSuffix(op string) (baseOp string, isSnapshot, isDatabase bool) {
	// Strip _SNAPSHOT and _DATABASE suffixes for dispatch.
	if strings.HasSuffix(op, "_SNAPSHOT") {
		return strings.TrimSuffix(op, "_SNAPSHOT"), true, false
	}
	if strings.HasSuffix(op, "_DATABASE") {
		return strings.TrimSuffix(op, "_DATABASE"), false, true
	}
	return op, false, false
}

func (sm *StackMachine) execute(ctx context.Context, idx int, op string, arg any) error {
	baseOp, isSnapshot, isDatabase := splitOpSuffix(op)

	if handled, err := sm.executeStackOp(ctx, idx, baseOp, arg); handled || err != nil {
		return err
	}
	if handled, err := sm.executeTxnOp(ctx, idx, baseOp, isDatabase, isSnapshot); handled || err != nil {
		return err
	}
	if handled, err := sm.executeKVOp(ctx, idx, baseOp, isDatabase, isSnapshot); handled || err != nil {
		return err
	}
	if handled, err := sm.executeTupleOp(idx, baseOp); handled || err != nil {
		return err
	}
	if handled, err := sm.executeThreadOp(ctx, idx, baseOp); handled || err != nil {
		return err
	}
	if handled, err := sm.executeDirectoryOp(ctx, idx, baseOp, isDatabase, isSnapshot); handled || err != nil {
		return err
	}
	return fmt.Errorf("unimplemented operation: %s", op)
}

func (sm *StackMachine) executeStackOp(ctx context.Context, idx int, baseOp string, arg any) (bool, error) {
	switch baseOp {
	case "PUSH":
		sm.push(idx, arg)
	case "DUP":
		e := sm.stack[len(sm.stack)-1]
		sm.push(idx, e.value)
	case "EMPTY_STACK":
		sm.stack = sm.stack[:0]
	case "SWAP":
		depth := int(sm.popInt64())
		n := len(sm.stack)
		sm.stack[n-1], sm.stack[n-1-depth] = sm.stack[n-1-depth], sm.stack[n-1]
	case "POP":
		sm.pop()
	case "SUB":
		a := sm.pop().value
		b := sm.pop().value
		sm.push(idx, subValues(a, b))
	case "CONCAT":
		a := sm.popBytes()
		b := sm.popBytes()
		sm.push(idx, append(a, b...))
	case "LOG_STACK":
		prefix := sm.popBytes()
		return true, sm.logStack(ctx, idx, prefix)
	default:
		return false, nil
	}
	return true, nil
}

func (sm *StackMachine) executeThreadOp(ctx context.Context, idx int, baseOp string) (bool, error) {
	switch baseOp {
	case "START_THREAD":
		sm.startThread(ctx)
	case "WAIT_EMPTY":
		prefix := sm.popBytes()
		if err := sm.waitEmpty(ctx, prefix); err != nil {
			return true, err
		}
		sm.push(idx, []byte("WAITED_FOR_EMPTY"))
	case "WAIT_FUTURE":
		// Our client is synchronous — all values are already resolved.
		// No-op: the value on top of the stack is already the result.
	case "UNIT_TESTS":
		// No-op for now. Binding-specific smoke tests.
	default:
		return false, nil
	}
	return true, nil
}

func (sm *StackMachine) startThread(ctx context.Context) {
	prefix := sm.popBytes()
	child := newStackMachine(sm.db, prefix, sm.transactionRegistry)
	child.childJoinTimeout = sm.childJoinTimeout
	child.threadPath = sm.threadPath + " -> " + string(prefix)
	sm.childMu.Lock()
	if sm.children == nil {
		sm.children = make(map[string]int)
	}
	sm.children[string(prefix)]++
	sm.childMu.Unlock()
	sm.wg.Add(1)
	go func() {
		defer sm.wg.Done()
		defer func() {
			sm.childMu.Lock()
			sm.children[string(prefix)]--
			if sm.children[string(prefix)] == 0 {
				delete(sm.children, string(prefix))
			}
			sm.childMu.Unlock()
		}()
		if err := child.Run(ctx); err != nil {
			err = fmt.Errorf("child thread %q: %w", prefix, err)
			sm.childMu.Lock()
			if sm.childErr == nil {
				sm.childErr = err
			}
			sm.childMu.Unlock()
			if sm.cancelRun != nil {
				sm.cancelRun(err)
			}
		}
	}()
}

func (sm *StackMachine) logStack(ctx context.Context, idx int, prefix []byte) error {
	// Pop all entries (resolving futures) and write to FDB in batches.
	n := len(sm.stack)
	entries := make([]stackEntry, n)
	for i := n - 1; i >= 0; i-- {
		entries[i] = sm.pop()
	}

	for i := 0; i < len(entries); i += 100 {
		end := i + 100
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[i:end]
		_, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
			for j, e := range batch {
				globalIdx := i + j
				key := append(append([]byte{}, prefix...), tuple.Tuple{int64(globalIdx), int64(e.idx)}.Pack()...)
				packed := tuple.Tuple{e.value}.Pack()
				if len(packed) > 40000 {
					packed = packed[:40000]
				}
				tx.Set(key, packed)
			}
			return nil, nil
		})
		if err != nil {
			return fmt.Errorf("log stack: %w", err)
		}
	}
	return nil
}

func (sm *StackMachine) waitEmpty(ctx context.Context, prefix []byte) error {
	end := strinc(prefix)
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		result, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
			kvs, _, err := tx.GetRange(ctx, prefix, end, 1)
			return kvs, err
		})
		if err != nil {
			return err
		}
		kvs := result.([]client.KeyValue)
		if len(kvs) == 0 {
			return nil
		}
		// Not empty yet, retry via OnError with not_committed for backoff sleep.
		tr := sm.db.CreateTransaction()
		untrack := sm.waits.track(tr.Cancel)
		err = tr.OnError(ctx, &wire.FDBError{Code: client.ErrNotCommitted})
		untrack()
		if err != nil {
			return err
		}
	}
}

func subValues(a, b any) any {
	ai := toBigInt(a)
	bi := toBigInt(b)
	return new(big.Int).Sub(ai, bi)
}

func toBigInt(v any) *big.Int {
	switch x := v.(type) {
	case int64:
		return big.NewInt(x)
	case int:
		return big.NewInt(int64(x))
	case uint64:
		return new(big.Int).SetUint64(x)
	case *big.Int:
		return x
	case []byte:
		return new(big.Int).SetBytes(x)
	default:
		panic(fmt.Sprintf("cannot convert %T to big.Int", v))
	}
}
