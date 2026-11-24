//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func (sm *StackMachine) executeKVOp(ctx context.Context, idx int, baseOp string, isDatabase, isSnapshot bool) (bool, error) {
	switch baseOp {
	case "GET":
		sm.execGet(ctx, idx, isDatabase, isSnapshot)
	case "GET_KEY":
		sm.execGetKey(ctx, idx, isDatabase, isSnapshot)
	case "GET_RANGE":
		sm.execGetRange(ctx, idx, isDatabase, isSnapshot)
	case "GET_RANGE_STARTS_WITH":
		sm.execGetRangeStartsWith(ctx, idx, isDatabase, isSnapshot)
	case "GET_RANGE_SELECTOR":
		sm.execGetRangeSelector(ctx, idx, isDatabase, isSnapshot)
	case "SET":
		sm.execSet(ctx, idx, isDatabase)
	case "CLEAR":
		sm.execClear(ctx, idx, isDatabase)
	case "CLEAR_RANGE":
		sm.execClearRange(ctx, idx, isDatabase)
	case "CLEAR_RANGE_STARTS_WITH":
		sm.execClearRangeStartsWith(ctx, idx, isDatabase)
	case "ATOMIC_OP":
		sm.execAtomicOp(ctx, idx, isDatabase)
	default:
		return false, nil
	}
	return true, nil
}

func (sm *StackMachine) execGet(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	key := sm.popBytes()
	var val []byte
	var err error
	if isDatabase {
		result, e := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
}

func (sm *StackMachine) execGetKey(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	key := sm.popBytes()
	orEqual := sm.popInt64() != 0
	offset := int32(sm.popInt64())
	prefix := sm.popBytes()
	var result []byte
	var err error
	if isDatabase {
		r, e := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
}

func (sm *StackMachine) execGetRange(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	begin := sm.popBytes()
	end := sm.popBytes()
	limit := int(sm.popInt64())
	reverse := sm.popInt64() != 0
	mode := sm.popInt64()
	if limit == 0 && mode == 0 {
		sm.pushError(idx, &wire.FDBError{Code: 2210})
		return
	}

	kvs, err := sm.readRange(ctx, begin, end, limit, reverse, isDatabase, isSnapshot)
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
}

func (sm *StackMachine) execGetRangeStartsWith(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	prefix := sm.popBytes()
	limit := int(sm.popInt64())
	reverse := sm.popInt64() != 0
	mode := sm.popInt64()
	if limit == 0 && mode == 0 {
		sm.pushError(idx, &wire.FDBError{Code: 2210})
		return
	}

	end := strinc(prefix)
	kvs, err := sm.readRange(ctx, prefix, end, limit, reverse, isDatabase, isSnapshot)
	if err != nil {
		sm.pushError(idx, err)
	} else {
		t := make(tuple.Tuple, 0, len(kvs)*2)
		for _, kv := range kvs {
			t = append(t, kv.Key, kv.Value)
		}
		sm.push(idx, t.Pack())
	}
}

func (sm *StackMachine) readRange(ctx context.Context, begin, end []byte, limit int, reverse, isDatabase, isSnapshot bool) ([]client.KeyValue, error) {
	if isDatabase {
		result, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
			var kv []client.KeyValue
			var rangeErr error
			if reverse {
				kv, _, rangeErr = tx.GetRangeReverse(ctx, begin, end, limit)
			} else {
				kv, _, rangeErr = tx.GetRange(ctx, begin, end, limit)
			}
			return kv, rangeErr
		})
		if err == nil && result != nil {
			return result.([]client.KeyValue), nil
		}
		return nil, err
	}
	if reverse {
		if isSnapshot {
			kvs, _, err := sm.currentTr().Snapshot().GetRangeReverse(ctx, begin, end, limit)
			return kvs, err
		}
		kvs, _, err := sm.currentTr().GetRangeReverse(ctx, begin, end, limit)
		return kvs, err
	}
	if isSnapshot {
		kvs, _, err := sm.currentTr().Snapshot().GetRange(ctx, begin, end, limit)
		return kvs, err
	}
	kvs, _, err := sm.currentTr().GetRange(ctx, begin, end, limit)
	return kvs, err
}

func (sm *StackMachine) execGetRangeSelector(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	beginKey := sm.popBytes()
	beginOrEqual := sm.popInt64() != 0
	beginOffset := int32(sm.popInt64())
	endKey := sm.popBytes()
	endOrEqual := sm.popInt64() != 0
	endOffset := int32(sm.popInt64())
	limit := int(sm.popInt64())
	reverse := sm.popInt64() != 0
	mode := sm.popInt64()
	prefix := sm.popBytes()
	if limit == 0 && mode == 0 {
		sm.pushError(idx, &wire.FDBError{Code: 2210})
		return
	}

	// Resolve key selectors to actual keys + range read.
	// For _DATABASE variant, everything runs in one auto-commit transaction.
	if isDatabase {
		sm.execGetRangeSelectorDB(ctx, idx, beginKey, beginOrEqual, beginOffset, endKey, endOrEqual, endOffset, limit, reverse, prefix)
		return
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
		return
	}
	kvs, err := sm.readRange(ctx, begin, end, limit, reverse, false, isSnapshot)
	if err != nil {
		sm.pushError(idx, err)
		return
	}
	// Filter results to prefix and pack.
	t := make(tuple.Tuple, 0, len(kvs)*2)
	for _, kv := range kvs {
		if prefix == nil || bytes.HasPrefix(kv.Key, prefix) {
			t = append(t, kv.Key, kv.Value)
		}
	}
	sm.push(idx, t.Pack())
}

func (sm *StackMachine) execGetRangeSelectorDB(
	ctx context.Context,
	idx int,
	beginKey []byte, beginOrEqual bool, beginOffset int32,
	endKey []byte, endOrEqual bool, endOffset int32,
	limit int, reverse bool, prefix []byte,
) {
	type rangeResult struct {
		kvs []client.KeyValue
	}
	result, e := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
		b, bErr := tx.GetKey(ctx, beginKey, beginOrEqual, beginOffset)
		if bErr != nil {
			return nil, bErr
		}
		en, eErr := tx.GetKey(ctx, endKey, endOrEqual, endOffset)
		if eErr != nil {
			return nil, eErr
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
		return
	}
	rr := result.(*rangeResult)
	t := make(tuple.Tuple, 0, len(rr.kvs)*2)
	for _, kv := range rr.kvs {
		if bytes.HasPrefix(kv.Key, prefix) {
			t = append(t, kv.Key, kv.Value)
		}
	}
	sm.push(idx, t.Pack())
}

func (sm *StackMachine) execSet(ctx context.Context, idx int, isDatabase bool) {
	key := sm.popBytes()
	value := sm.popBytes()
	if isDatabase {
		_, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
}

func (sm *StackMachine) execClear(ctx context.Context, idx int, isDatabase bool) {
	key := sm.popBytes()
	if isDatabase {
		_, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
}

func (sm *StackMachine) execClearRange(ctx context.Context, idx int, isDatabase bool) {
	begin := sm.popBytes()
	end := sm.popBytes()
	if isDatabase {
		_, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
}

func (sm *StackMachine) execClearRangeStartsWith(ctx context.Context, idx int, isDatabase bool) {
	prefix := sm.popBytes()
	end := strinc(prefix)
	if isDatabase {
		_, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
}

func (sm *StackMachine) execAtomicOp(ctx context.Context, idx int, isDatabase bool) {
	opType := sm.popString()
	key := sm.popBytes()
	value := sm.popBytes()
	mutType := atomicOpType(opType)
	if isDatabase {
		_, err := sm.transact(ctx, func(tx *client.Transaction) (any, error) {
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
