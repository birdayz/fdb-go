//go:build cgo && libfdbc

// This file is the libfdb_c (raw cgo) REFERENCE side of the range BATCH DIVISION
// differential, and it exists because Apple's Go binding cannot answer the question.
//
// Comparing row sets between the two clients is easy and already done
// (bench:TestDifferential_LimitedIteratorMultiBatchRowSets). Comparing where each client
// SPLIT those rows is not: cgofdb keeps iteration/more/the advancing begin-key private on an
// unexported RangeIterator with no trace hook, so the C side's division is invisible through
// it. That left every batch-level statement about C an inference from source rather than a
// measurement, and inference is exactly what this repo's range work keeps getting wrong.
//
// CGetRangeBatch is ONE raw fdb_transaction_get_range call with the parameters the C client's
// own loop would pass — including target_bytes and iteration, which the binding hard-codes and
// hides. Driving that call in a loop from a test reproduces the C batching progression with
// every per-fetch count observable.
//
// It deliberately does NOT change pkg/fdbgo/libfdbc's production iterator (backend.go), whose
// SetTraceLog remains a no-op. That iterator delegates to cgofdb; replacing it with a
// hand-rolled loop to gain observability would put a new, less-tested read path in front of
// every caller of the cgo backend to serve a test. The measurement is what was missing, not a
// different production implementation — so the capability lands here, beside CGetMappedRange,
// which is a reference side for the same reason.
package libfdbc

/*
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
#include <stddef.h>

// fdbgo_kv_row is a NATURALLY ALIGNED copy of one FDBKeyValue, and the indirection
// is not decoration. fdb_c.h wraps FDBKeyValue in `#pragma pack(push, 4)`
// (fdb_c.h:110-115, closed at :125), so in C the struct is 24 bytes with `value`
// at offset 12 — an 8-byte pointer on a 4-byte boundary. Go cannot express that
// alignment, so cgo DROPS the field: indexing the array in Go and reading
// kvs[i].value fails to compile, and any hand-computed stride would silently read
// the wrong bytes. This mirrors fdbgo_mkv_decode_at, which exists for the same
// class of reason.
typedef struct {
	const uint8_t* key;
	int key_length;
	const uint8_t* value;
	int value_length;
} fdbgo_kv_row;

static void fdbgo_kv_decode_at(const FDBKeyValue* arr, int i, fdbgo_kv_row* out) {
	out->key          = arr[i].key;
	out->key_length   = arr[i].key_length;
	out->value        = arr[i].value;
	out->value_length = arr[i].value_length;
}
*/
import "C"

import (
	"fmt"
)

// CKeyValue is one decoded FDBKeyValue, copied into Go memory.
type CKeyValue struct {
	Key   []byte
	Value []byte
}

// Streaming-mode constants, re-exported so a differential can pass the SAME mode to both
// clients without casting a bare int through two enums. The values match fdb_c.h; the pure-Go
// client's fdb.StreamingMode uses the identical numbering.
const (
	CStreamingModeWantAll  = int(C.FDB_STREAMING_MODE_WANT_ALL)
	CStreamingModeIterator = int(C.FDB_STREAMING_MODE_ITERATOR)
	CStreamingModeExact    = int(C.FDB_STREAMING_MODE_EXACT)
	CStreamingModeSmall    = int(C.FDB_STREAMING_MODE_SMALL)
	CStreamingModeMedium   = int(C.FDB_STREAMING_MODE_MEDIUM)
	CStreamingModeLarge    = int(C.FDB_STREAMING_MODE_LARGE)
	CStreamingModeSerial   = int(C.FDB_STREAMING_MODE_SERIAL)
)

// CGetRangeBatch issues exactly ONE fdb_transaction_get_range and returns the rows it produced
// plus the future's `more` flag. It is the single-fetch primitive the C client's own loop is
// built from, exposed so a caller can drive that loop and see each step.
//
// begin and end are plain keys, resolved as firstGreaterOrEqual selectors (or_equal=false,
// offset=1) — the selectors every binding's KeyRange.ToRange produces.
//
// Every parameter the C entry point takes is passed through rather than derived here, because
// the derivation is precisely what is under measurement:
//
//   - limit is the row budget; 0 means unlimited (ROW_LIMIT_UNLIMITED to the server).
//   - targetBytes is the per-fetch BYTE budget; 0 lets the C client apply its own per-mode
//     value from mode_bytes_array. This is the dimension the pure-Go client has no equivalent
//     of at the iterator level, so leaving it caller-controlled is the whole point.
//   - iteration is the 1-based fetch number the ITERATOR progression indexes with. The C client
//     clamps it internally; passing it explicitly is what makes the progression observable.
//
// EXACT with neither a row nor a byte budget is exact_mode_without_limits (2210) from the C
// entry point, surfaced here unwrapped in CFDBError.Code like every other error in this file.
func CGetRangeBatch(tr CTxnHandle, begin, end []byte, limit, targetBytes, mode, iteration int, reverse, snapshot bool) ([]CKeyValue, bool, error) {
	if !tr.Valid() {
		return nil, false, &CFDBError{Code: 2015, Msg: "transaction is closed"}
	}

	bp, bl := cBytes(begin)
	ep, el := cBytes(end)

	f := C.fdb_transaction_get_range(tr.ptr,
		bp, bl, 0 /* begin_or_equal */, 1, /* begin_offset */
		ep, el, 0 /* end_or_equal */, 1, /* end_offset */
		C.int(limit), C.int(targetBytes),
		C.FDBStreamingMode(mode), C.int(iteration),
		cBool(snapshot), cBool(reverse))
	defer C.fdb_future_destroy(f)

	if err := cErr(C.fdb_future_block_until_ready(f)); err != nil {
		return nil, false, err
	}
	if err := cErr(C.fdb_future_get_error(f)); err != nil {
		return nil, false, err
	}

	var (
		arr   *C.FDBKeyValue
		count C.int
		more  C.fdb_bool_t
	)
	if err := cErr(C.fdb_future_get_keyvalue_array(f, &arr, &count, &more)); err != nil {
		return nil, false, err
	}
	if count < 0 {
		return nil, false, fmt.Errorf("fdb_future_get_keyvalue_array returned count %d", int(count))
	}

	rows := make([]CKeyValue, 0, int(count))
	if arr != nil && count > 0 {
		// Indexed in C — see fdbgo_kv_row. Every byte is arena memory owned by the future, so
		// goBytes copies it out before the deferred fdb_future_destroy runs.
		for i := 0; i < int(count); i++ {
			var d C.fdbgo_kv_row
			C.fdbgo_kv_decode_at(arr, C.int(i), &d)
			rows = append(rows, CKeyValue{
				Key:   goBytes(d.key, d.key_length),
				Value: goBytes(d.value, d.value_length),
			})
		}
	}
	return rows, more != 0, nil
}
