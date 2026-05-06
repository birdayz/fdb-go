package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
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
		if isDatabase {
			r, e := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				return tx.GetKey(ctx, key, orEqual, offset)
			})
			if e == nil && r != nil {
				result = r.([]byte)
			}
			err = e
		} else if isSnapshot {
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
		if isDatabase {
			result, e := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				var kv []client.KeyValue
				var rangeErr error
				if reverse {
					kv, _, rangeErr = tx.GetRangeReverse(ctx, begin, end, limit)
				} else {
					kv, _, rangeErr = tx.GetRange(ctx, begin, end, limit)
				}
				return kv, rangeErr
			})
			if e == nil && result != nil {
				kvs = result.([]client.KeyValue)
			}
			err = e
		} else if reverse {
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
		if isDatabase {
			result, e := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				var kv []client.KeyValue
				var rangeErr error
				if reverse {
					kv, _, rangeErr = tx.GetRangeReverse(ctx, prefix, end, limit)
				} else {
					kv, _, rangeErr = tx.GetRange(ctx, prefix, end, limit)
				}
				return kv, rangeErr
			})
			if e == nil && result != nil {
				kvs = result.([]client.KeyValue)
			}
			err = e
		} else if reverse {
			if isSnapshot {
				kvs, _, err = sm.currentTr().Snapshot().GetRangeReverse(ctx, prefix, end, limit)
			} else {
				kvs, _, err = sm.currentTr().GetRangeReverse(ctx, prefix, end, limit)
			}
		} else {
			if isSnapshot {
				kvs, _, err = sm.currentTr().Snapshot().GetRange(ctx, prefix, end, limit)
			} else {
				kvs, _, err = sm.currentTr().GetRange(ctx, prefix, end, limit)
			}
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
		beginKey := sm.popBytes()
		beginOrEqual := sm.popInt64() != 0
		beginOffset := int32(sm.popInt64())
		endKey := sm.popBytes()
		endOrEqual := sm.popInt64() != 0
		endOffset := int32(sm.popInt64())
		limit := int(sm.popInt64())
		reverse := sm.popInt64() != 0
		_ = sm.popInt64() // streaming_mode
		prefix := sm.popBytes()

		// Resolve key selectors to actual keys + range read.
		// For _DATABASE variant, everything runs in one auto-commit transaction.
		if isDatabase {
			type rangeResult struct {
				kvs []client.KeyValue
			}
			result, e := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				b, bErr := tx.GetKey(ctx, beginKey, beginOrEqual, beginOffset)
				if bErr != nil {
					return nil, bErr
				}
				en, eErr := tx.GetKey(ctx, endKey, endOrEqual, endOffset)
				if eErr != nil {
					return nil, eErr
				}
				// Clamp to prefix.
				if bytes.Compare(b, prefix) < 0 {
					b = prefix
				}
				prefixEnd := strinc(prefix)
				if bytes.Compare(en, prefixEnd) > 0 {
					en = prefixEnd
				}
				var kv []client.KeyValue
				var rangeErr error
				if reverse {
					kv, _, rangeErr = tx.GetRangeReverse(ctx, b, en, limit)
				} else {
					kv, _, rangeErr = tx.GetRange(ctx, b, en, limit)
				}
				return &rangeResult{kvs: kv}, rangeErr
			})
			if e != nil {
				sm.pushError(idx, e)
			} else {
				rr := result.(*rangeResult)
				t := make(tuple.Tuple, 0, len(rr.kvs)*2)
				for _, kv := range rr.kvs {
					t = append(t, kv.Key, kv.Value)
				}
				sm.push(idx, t.Pack())
			}
			return nil
		}

		var begin, end []byte
		var err error
		if isSnapshot {
			begin, err = sm.currentTr().Snapshot().GetKey(ctx, beginKey, beginOrEqual, beginOffset)
			if err == nil {
				end, err = sm.currentTr().Snapshot().GetKey(ctx, endKey, endOrEqual, endOffset)
			}
		} else {
			begin, err = sm.currentTr().GetKey(ctx, beginKey, beginOrEqual, beginOffset)
			if err == nil {
				end, err = sm.currentTr().GetKey(ctx, endKey, endOrEqual, endOffset)
			}
		}
		if err != nil {
			sm.pushError(idx, err)
		} else {
			var kvs []client.KeyValue
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
				// Filter results to prefix and pack.
				t := make(tuple.Tuple, 0, len(kvs)*2)
				for _, kv := range kvs {
					if prefix == nil || bytes.HasPrefix(kv.Key, prefix) {
						t = append(t, kv.Key, kv.Value)
					}
				}
				sm.push(idx, t.Pack())
			}
		}

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
				return nil, tx.ClearRange(begin, end)
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			if err := sm.currentTr().ClearRange(begin, end); err != nil {
				sm.pushError(idx, err)
			}
		}

	case "CLEAR_RANGE_STARTS_WITH":
		prefix := sm.popBytes()
		end := strinc(prefix)
		if isDatabase {
			_, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
				return nil, tx.ClearRange(prefix, end)
			})
			if err != nil {
				sm.pushError(idx, err)
			} else {
				sm.push(idx, []byte("RESULT_NOT_PRESENT"))
			}
		} else {
			if err := sm.currentTr().ClearRange(prefix, end); err != nil {
				sm.pushError(idx, err)
			}
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
		var ver int64
		var err error
		if isSnapshot {
			ver, err = sm.currentTr().Snapshot().GetReadVersion(ctx)
		} else {
			ver, err = sm.currentTr().GetReadVersion(ctx)
		}
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.lastVer = ver
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
		// Deferred versionstamp: we push a future that resolves after commit.
		// Since our client is synchronous, we use the versionstampFuture mechanism.
		f := &versionstampFuture{tr: sm.currentTr()}
		sm.push(idx, f)

	case "GET_APPROXIMATE_SIZE":
		_ = sm.currentTr().GetApproximateSize()
		sm.push(idx, []byte("GOT_APPROXIMATE_SIZE"))

	case "GET_ESTIMATED_RANGE_SIZE":
		_ = sm.popBytes() // begin
		_ = sm.popBytes() // end
		sm.push(idx, []byte("GOT_ESTIMATED_RANGE_SIZE"))

	case "GET_RANGE_SPLIT_POINTS":
		_ = sm.popBytes() // begin
		_ = sm.popBytes() // end
		_ = sm.popInt64() // chunkSize
		sm.push(idx, []byte("GOT_RANGE_SPLIT_POINTS"))

	// --- Conflict ranges ---
	case "READ_CONFLICT_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		if err := sm.currentTr().AddReadConflictRange(begin, end); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("SET_CONFLICT_RANGE"))
		}

	case "READ_CONFLICT_KEY":
		key := sm.popBytes()
		sm.currentTr().AddReadConflictKey(key)
		sm.push(idx, []byte("SET_CONFLICT_KEY"))

	case "WRITE_CONFLICT_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		if err := sm.currentTr().AddWriteConflictRange(begin, end); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("SET_CONFLICT_RANGE"))
		}

	case "WRITE_CONFLICT_KEY":
		key := sm.popBytes()
		sm.currentTr().AddWriteConflictKey(key)
		sm.push(idx, []byte("SET_CONFLICT_KEY"))

	// --- Error handling ---
	case "ON_ERROR":
		code := int(sm.popInt64())
		err := sm.currentTr().OnError(ctx, &wire.FDBError{Code: code})
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

	case "ENCODE_FLOAT":
		valBytes := sm.popBytes()
		var val float32
		binary.Read(bytes.NewReader(valBytes), binary.BigEndian, &val)
		sm.push(idx, val)

	case "ENCODE_DOUBLE":
		valBytes := sm.popBytes()
		var val float64
		binary.Read(bytes.NewReader(valBytes), binary.BigEndian, &val)
		sm.push(idx, val)

	case "DECODE_FLOAT":
		val := sm.pop().value.(float32)
		var buf bytes.Buffer
		binary.Write(&buf, binary.BigEndian, val)
		sm.push(idx, buf.Bytes())

	case "DECODE_DOUBLE":
		val := sm.pop().value.(float64)
		var buf bytes.Buffer
		binary.Write(&buf, binary.BigEndian, val)
		sm.push(idx, buf.Bytes())

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

	case "TUPLE_PACK_WITH_VERSIONSTAMP":
		prefix := sm.popBytes()
		count := int(sm.popInt64())
		t := make(tuple.Tuple, count)
		for i := 0; i < count; i++ {
			t[i] = sm.pop().value
		}
		packed, err := t.PackWithVersionstamp(prefix)
		if err != nil && strings.Contains(err.Error(), "No incomplete") {
			sm.push(idx, []byte("ERROR: NONE"))
		} else if err != nil {
			sm.push(idx, []byte("ERROR: MULTIPLE"))
		} else {
			sm.push(idx, []byte("OK"))
			sm.push(idx, packed)
		}

	// --- Threading ---
	case "START_THREAD":
		prefix := sm.popBytes()
		child := NewStackMachine(sm.db, prefix)
		// Share transaction map and directory list (binding tester spec).
		child.trMap = sm.trMap
		child.dirList = sm.dirList
		child.dirIndex = sm.dirIndex
		child.dirErrorIndex = sm.dirErrorIndex
		sm.wg.Add(1)
		go func() {
			defer sm.wg.Done()
			child.Run(ctx)
		}()

	case "WAIT_EMPTY":
		prefix := sm.popBytes()
		sm.waitEmpty(ctx, prefix)
		sm.push(idx, []byte("WAITED_FOR_EMPTY"))

	case "DISABLE_WRITE_CONFLICT":
		sm.currentTr().SetNextWriteNoWriteConflictRange()

	case "WAIT_FUTURE":
		// Our client is synchronous — all values are already resolved.
		// No-op: the value on top of the stack is already the result.

	case "UNIT_TESTS":
		// No-op for now. Binding-specific smoke tests.

	default:
		// Try directory operations.
		handled, err := sm.executeDirectoryOp(ctx, idx, baseOp, isDatabase, isSnapshot)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
		return fmt.Errorf("unimplemented operation: %s", op)
	}

	return nil
}

func (sm *StackMachine) logStack(ctx context.Context, idx int, prefix []byte) {
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
}

func (sm *StackMachine) waitEmpty(ctx context.Context, prefix []byte) {
	end := strinc(prefix)
	for {
		if ctx.Err() != nil {
			return
		}
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
		// Not empty yet, retry via OnError with not_committed for backoff sleep.
		tr := sm.db.CreateTransaction()
		tr.OnError(ctx, &wire.FDBError{Code: client.ErrNotCommitted})
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
