package main

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

func (sm *StackMachine) execute(ctx context.Context, idx int, op string, arg any) error {
	// Strip _SNAPSHOT and _DATABASE suffixes for dispatch.
	baseOp := op
	isSnapshot := false
	isDatabase := false
	if strings.HasSuffix(op, "_SNAPSHOT") {
		baseOp = strings.TrimSuffix(op, "_SNAPSHOT")
		isSnapshot = true
	} else if strings.HasSuffix(op, "_DATABASE") {
		baseOp = strings.TrimSuffix(op, "_DATABASE")
		isDatabase = true
	}

	switch baseOp {
	// --- Stack operations ---
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
		sm.logStack(ctx, idx, prefix)

	// --- Transaction management ---
	case "NEW_TRANSACTION":
		sm.trMu.Lock()
		sm.trMap[sm.trName] = sm.db.CreateTransaction()
		sm.trMu.Unlock()
	case "USE_TRANSACTION":
		name := sm.popString()
		sm.trMu.Lock()
		sm.trName = name
		if _, ok := sm.trMap[name]; !ok {
			sm.trMap[name] = sm.db.CreateTransaction()
		}
		sm.trMu.Unlock()
	case "COMMIT":
		tr := sm.currentTr()
		err := tr.Commit(ctx)
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("RESULT_NOT_PRESENT"))
		}
	case "RESET":
		sm.trMu.Lock()
		sm.trMap[sm.trName] = sm.db.CreateTransaction()
		sm.trMu.Unlock()
	case "CANCEL":
		sm.currentTr().Cancel()

	// --- Reads ---
	case "GET":
		key := sm.popBytes()
		var val []byte
		var err error
		if isDatabase {
			result, e := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				return tx.Get(ctx, key)
			})
			if e == nil && result != nil {
				val = result.([]byte)
			}
			err = e
		} else if isSnapshot {
			val, err = sm.currentTr().Snapshot().Get(ctx, key)
		} else {
			val, err = sm.currentTr().Get(ctx, key)
		}
		if err != nil {
			sm.pushError(idx, err)
		} else if val == nil {
			sm.push(idx, []byte("RESULT_NOT_PRESENT"))
		} else {
			sm.push(idx, val)
		}

	case "GET_KEY":
		key := sm.popBytes()
		orEqual := sm.popInt64() != 0
		offset := int32(sm.popInt64())
		prefix := sm.popBytes()
		var result []byte
		var err error
		if isSnapshot {
			result, err = sm.currentTr().Snapshot().GetKey(ctx, key, orEqual, offset)
		} else {
			result, err = sm.currentTr().GetKey(ctx, key, orEqual, offset)
		}
		if err != nil {
			sm.pushError(idx, err)
		} else {
			// Clamp result to prefix range.
			if bytes.Compare(result, prefix) < 0 {
				result = prefix
			} else if !bytes.HasPrefix(result, prefix) && bytes.Compare(result, prefix) > 0 {
				result = strinc(prefix)
			}
			sm.push(idx, result)
		}

	case "GET_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		limit := int(sm.popInt64())
		reverse := sm.popInt64() != 0
		_ = sm.popInt64() // streaming_mode — ignored, we always do WANT_ALL

		var kvs []client.KeyValue
		var err error
		if reverse {
			if isSnapshot {
				kvs, _, err = sm.currentTr().Snapshot().GetRangeReverse(ctx, begin, end, limit)
			} else {
				kvs, _, err = sm.currentTr().GetRangeReverse(ctx, begin, end, limit)
			}
		} else {
			if isSnapshot {
				kvs, _, err = sm.currentTr().Snapshot().GetRange(ctx, begin, end, limit)
			} else {
				kvs, _, err = sm.currentTr().GetRange(ctx, begin, end, limit)
			}
		}
		if err != nil {
			sm.pushError(idx, err)
		} else {
			// Pack as single tuple: [k1, v1, k2, v2, ...]
			t := make(tuple.Tuple, 0, len(kvs)*2)
			for _, kv := range kvs {
				t = append(t, kv.Key, kv.Value)
			}
			sm.push(idx, t.Pack())
		}

	case "GET_RANGE_STARTS_WITH":
		prefix := sm.popBytes()
		limit := int(sm.popInt64())
		reverse := sm.popInt64() != 0
		_ = sm.popInt64() // streaming_mode

		end := strinc(prefix)
		var kvs []client.KeyValue
		var err error
		if reverse {
			kvs, _, err = sm.currentTr().GetRangeReverse(ctx, prefix, end, limit)
		} else {
			kvs, _, err = sm.currentTr().GetRange(ctx, prefix, end, limit)
		}
		if err != nil {
			sm.pushError(idx, err)
		} else {
			t := make(tuple.Tuple, 0, len(kvs)*2)
			for _, kv := range kvs {
				t = append(t, kv.Key, kv.Value)
			}
			sm.push(idx, t.Pack())
		}

	case "GET_RANGE_SELECTOR":
		_ = sm.popBytes() // beginKey
		_ = sm.popInt64() // beginOrEqual
		_ = sm.popInt64() // beginOffset
		_ = sm.popBytes() // endKey
		_ = sm.popInt64() // endOrEqual
		_ = sm.popInt64() // endOffset
		_ = sm.popInt64() // limit
		_ = sm.popInt64() // reverse
		_ = sm.popInt64() // streaming_mode
		_ = sm.popBytes() // prefix
		// TODO: implement with raw key selectors
		sm.push(idx, []byte("NOT_IMPLEMENTED"))

	// --- Writes ---
	case "SET":
		key := sm.popBytes()
		value := sm.popBytes()
		if isDatabase {
			_, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				tx.Set(key, value)
				return nil, nil
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			sm.currentTr().Set(key, value)
		}

	case "CLEAR":
		key := sm.popBytes()
		if isDatabase {
			_, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				tx.Clear(key)
				return nil, nil
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			sm.currentTr().Clear(key)
		}

	case "CLEAR_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		if isDatabase {
			_, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				tx.ClearRange(begin, end)
				return nil, nil
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			sm.currentTr().ClearRange(begin, end)
		}

	case "CLEAR_RANGE_STARTS_WITH":
		prefix := sm.popBytes()
		end := strinc(prefix)
		if isDatabase {
			_, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				tx.ClearRange(prefix, end)
				return nil, nil
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			sm.currentTr().ClearRange(prefix, end)
		}

	case "ATOMIC_OP":
		opType := sm.popString()
		key := sm.popBytes()
		value := sm.popBytes()
		mutType := atomicOpType(opType)
		if isDatabase {
			_, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				tx.Atomic(mutType, key, value)
				return nil, nil
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			sm.currentTr().Atomic(mutType, key, value)
		}

	// --- Version ---
	case "GET_READ_VERSION":
		tr := sm.currentTr()
		// Force a GRV — read version is obtained as part of ensureReadVersion.
		// We read a dummy key to trigger it, then grab the read version.
		_, err := tr.Get(ctx, []byte("__dummy_grv__"))
		if err != nil {
			sm.pushError(idx, err)
		} else {
			// Store lastVersion for SET_READ_VERSION.
			// The read version was set internally by ensureReadVersion.
			sm.push(idx, []byte("GOT_READ_VERSION"))
		}

	case "SET_READ_VERSION":
		sm.currentTr().SetReadVersion(sm.lastVer)

	case "GET_COMMITTED_VERSION":
		ver, err := sm.currentTr().GetCommittedVersion()
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.lastVer = ver
			sm.push(idx, []byte("GOT_COMMITTED_VERSION"))
		}

	case "GET_VERSIONSTAMP":
		// TODO: deferred versionstamp
		sm.push(idx, []byte("NOT_IMPLEMENTED"))

	// --- Conflict ranges ---
	case "READ_CONFLICT_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		sm.currentTr().AddReadConflictRange(begin, end)
		sm.push(idx, []byte("SET_CONFLICT_RANGE"))

	case "READ_CONFLICT_KEY":
		key := sm.popBytes()
		sm.currentTr().AddReadConflictKey(key)
		sm.push(idx, []byte("SET_CONFLICT_RANGE"))

	case "WRITE_CONFLICT_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		sm.currentTr().AddWriteConflictRange(begin, end)
		sm.push(idx, []byte("SET_CONFLICT_RANGE"))

	case "WRITE_CONFLICT_KEY":
		key := sm.popBytes()
		sm.currentTr().AddWriteConflictKey(key)
		sm.push(idx, []byte("SET_CONFLICT_RANGE"))

	// --- Error handling ---
	case "ON_ERROR":
		code := int(sm.popInt64())
		err := sm.currentTr().OnError(fmt.Errorf("fdb error %d", code))
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("RESULT_NOT_PRESENT"))
		}

	// --- Tuple operations ---
	case "TUPLE_PACK":
		count := int(sm.popInt64())
		t := make(tuple.Tuple, count)
		for i := 0; i < count; i++ {
			t[i] = sm.pop().value
		}
		sm.push(idx, t.Pack())

	case "TUPLE_UNPACK":
		data := sm.popBytes()
		t, err := tuple.Unpack(data)
		if err != nil {
			sm.pushError(idx, err)
		} else {
			for _, elem := range t {
				sm.push(idx, tuple.Tuple{elem}.Pack())
			}
		}

	case "TUPLE_RANGE":
		count := int(sm.popInt64())
		t := make(tuple.Tuple, count)
		for i := 0; i < count; i++ {
			t[i] = sm.pop().value
		}
		begin, end := t.FDBRangeKeys()
		sm.push(idx, begin.FDBKey())
		sm.push(idx, end.FDBKey())

	case "TUPLE_SORT":
		count := int(sm.popInt64())
		items := make([][]byte, count)
		for i := 0; i < count; i++ {
			items[i] = sm.popBytes()
		}
		// Sort by byte order.
		for i := 0; i < len(items); i++ {
			for j := i + 1; j < len(items); j++ {
				if bytes.Compare(items[i], items[j]) > 0 {
					items[i], items[j] = items[j], items[i]
				}
			}
		}
		for _, item := range items {
			sm.push(idx, item)
		}

	// --- Threading ---
	case "START_THREAD":
		prefix := sm.popBytes()
		child := NewStackMachine(sm.db, prefix)
		// Share transaction map.
		child.trMap = sm.trMap
		sm.wg.Add(1)
		go func() {
			defer sm.wg.Done()
			child.Run(ctx)
		}()

	case "WAIT_EMPTY":
		prefix := sm.popBytes()
		sm.waitEmpty(ctx, prefix)

	case "WAIT_FUTURE":
		// Our client is synchronous — all values are already resolved.
		// No-op: the value on top of the stack is already the result.

	case "UNIT_TESTS":
		// No-op for now. Binding-specific smoke tests.

	default:
		return fmt.Errorf("unimplemented operation: %s", op)
	}

	return nil
}

func (sm *StackMachine) logStack(ctx context.Context, idx int, prefix []byte) {
	// Write stack contents to FDB in batches.
	entries := make([]stackEntry, len(sm.stack))
	copy(entries, sm.stack)

	for i := 0; i < len(entries); i += 100 {
		end := i + 100
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[i:end]
		sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
			for j, e := range batch {
				globalIdx := i + j
				key := tuple.Tuple{prefix, int64(globalIdx), int64(e.idx)}.Pack()
				packed := tuple.Tuple{e.value}.Pack()
				if len(packed) > 40000 {
					packed = packed[:40000]
				}
				tx.Set(key, packed)
			}
			return nil, nil
		})
	}
	sm.stack = sm.stack[:0]
}

func (sm *StackMachine) waitEmpty(ctx context.Context, prefix []byte) {
	end := strinc(prefix)
	for {
		result, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
			kvs, _, err := tx.GetRange(ctx, prefix, end, 1)
			return kvs, err
		})
		if err != nil {
			continue
		}
		kvs := result.([]client.KeyValue)
		if len(kvs) == 0 {
			return
		}
		// Not empty yet, retry via OnError with not_committed.
		tr := sm.db.CreateTransaction()
		tr.OnError(fmt.Errorf("fdb error 1020"))
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
	case *big.Int:
		return x
	case []byte:
		return new(big.Int).SetBytes(x)
	default:
		panic(fmt.Sprintf("cannot convert %T to big.Int", v))
	}
}

func atomicOpType(name string) client.MutationType {
	switch strings.ToUpper(name) {
	case "ADD":
		return client.MutAddValue
	case "AND", "BIT_AND":
		return client.MutAnd
	case "OR", "BIT_OR":
		return client.MutOr
	case "XOR", "BIT_XOR":
		return client.MutXor
	case "APPEND_IF_FITS":
		return client.MutAppendIfFits
	case "MAX":
		return client.MutMax
	case "MIN":
		return client.MutMin
	case "SET_VERSIONSTAMPED_KEY":
		return client.MutSetVersionstampedKey
	case "SET_VERSIONSTAMPED_VALUE":
		return client.MutSetVersionstampedValue
	case "BYTE_MIN":
		return client.MutByteMin
	case "BYTE_MAX":
		return client.MutByteMax
	case "COMPARE_AND_CLEAR":
		return client.MutCompareAndClear
	default:
		panic(fmt.Sprintf("unknown atomic op: %s", name))
	}
}
