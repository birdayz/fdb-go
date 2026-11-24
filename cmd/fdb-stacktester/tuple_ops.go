//go:build bazelrunfiles

package main

import (
	"bytes"
	"encoding/binary"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func (sm *StackMachine) executeTupleOp(idx int, baseOp string) (bool, error) {
	switch baseOp {
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
		// Use the pure-Go API version, not the uninitialized C binding's state.
		t := sm.popTupleRaw()
		packed, err := t.PackWithVersionstamp(prefix)
		if err != nil && strings.Contains(err.Error(), "No incomplete") {
			sm.push(idx, []byte("ERROR: NONE"))
		} else if err != nil {
			sm.push(idx, []byte("ERROR: MULTIPLE"))
		} else {
			sm.push(idx, []byte("OK"))
			sm.push(idx, packed)
		}

	default:
		return false, nil
	}
	return true, nil
}
