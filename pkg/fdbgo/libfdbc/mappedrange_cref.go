//go:build cgo && libfdbc

// This file is the libfdb_c (raw cgo) REFERENCE side of the mapped-range
// differential. Apple's Go binding (cgofdb) has no mapped-range call at all —
// fdb_transaction_get_mapped_range and fdb_future_get_mappedkeyvalue_array are
// simply not wrapped — so the C reference has to call libfdb_c directly.
//
// Nothing here parses or validates a mapper: libfdb_c does not either. The
// mapper string is handed to the server verbatim and the server raises
// mapper_not_tuple (2043), mapper_bad_index (2030), mapper_no_such_key (2031),
// mapper_bad_range_decriptor (2032) or unsupported_operation (2108). The whole
// point of the differential is to compare those raw codes against the pure-Go
// client, so every error returned from this file carries the fdb_error_t
// unwrapped in *CFDBError.Code.
package libfdbc

/*
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>

// ---------------------------------------------------------------------------
// FDBMappedKeyValue layout
// ---------------------------------------------------------------------------
//
// fdb_future_get_mappedkeyvalue_array hands back a straight reinterpret_cast of
// the C++ MappedKeyValueRef array (bindings/c/fdb_c.cpp:
//   *out_kvm = (FDBMappedKeyValue*)rrr.begin();
// ), and MappedKeyValueRef is
//
//   struct MappedKeyValueRef : KeyValueRef {          // 24 bytes, StringRef is #pragma pack(4)
//       MappedReqAndResultRef reqAndResult;           // std::variant<GetValueReqAndResultRef,
//   };                                                //               GetRangeReqAndResultRef>
//
// The public header models ONLY the getRange arm ("we assume the underlying
// requests are always getRange and take the shortcut") and buries the rest in
// an opaque `unsigned char buffer[32]`. That shortcut is not good enough for a
// reference implementation: a mapper whose range descriptor is a single key
// produces the getValue arm, and reading it as a getRange arm yields garbage.
//
// So we decode the variant ourselves. libstdc++ lays std::variant out as the
// union payload followed by a one-byte _M_index, padded to the variant's
// alignment (8). With the union payload at 80 bytes that gives
//
//   0    .. 11   key      (FDBKey, packed to 4)
//   12   .. 23   value    (FDBKey)
//   24   .. 103  variant union payload
//   104         variant index byte  (0 = getValue, 1 = getRange, 0xFF = valueless)
//   105  .. 111  padding
//
// which is exactly the 112-byte total the header's own memory-layout comment
// documents. The static asserts below are the tripwire: if a future libfdb_c
// changes the struct, this file stops compiling instead of silently decoding
// noise.
_Static_assert(sizeof(FDBKeyValue) == 24, "FDBKeyValue is not the packed 24-byte KeyValueRef");
_Static_assert(sizeof(FDBMappedKeyValue) == 112, "FDBMappedKeyValue is not 112 bytes; variant layout below is stale");
_Static_assert(offsetof(FDBMappedKeyValue, getRange) == 24, "FDBMappedKeyValue variant does not start at offset 24");

// fdbgo_mkv_variant_index returns the raw std::variant discriminator: 0 for
// GetValueReqAndResultRef, 1 for GetRangeReqAndResultRef, 0xFF for valueless.
static unsigned char fdbgo_mkv_variant_index(const FDBMappedKeyValue* kv) {
	return ((const unsigned char*)kv)[sizeof(FDBMappedKeyValue) - 8];
}

// fdbgo_getvalue_arm is the getValue arm the public header omits:
//
//   struct GetValueReqAndResultRef { KeyRef key; Optional<ValueRef> result; };
//
// Optional<T> wraps std::optional<T>, which is payload-then-engaged-flag, and
// StringRef is #pragma pack(4) — hence the same packing here.
#pragma pack(push, 4)
typedef struct fdbgo_getvalue_arm {
	const uint8_t* key;
	int key_length;
	const uint8_t* value;
	int value_length;
	unsigned char present;
} fdbgo_getvalue_arm;
#pragma pack(pop)
_Static_assert(sizeof(fdbgo_getvalue_arm) == 28, "getValue arm layout does not match GetValueReqAndResultRef");

static const fdbgo_getvalue_arm* fdbgo_mkv_getvalue(const FDBMappedKeyValue* kv) {
	return (const fdbgo_getvalue_arm*)(((const unsigned char*)kv) + offsetof(FDBMappedKeyValue, getRange));
}

// ---------------------------------------------------------------------------
// Naturally-aligned mirrors, so Go can actually read these structs
// ---------------------------------------------------------------------------
//
// cgo cannot express a pointer field that sits at a 4-aligned offset, so for
// every #pragma pack(4) struct above it drops the misaligned pointers and
// substitutes blank padding: FDBKeyValue.value, FDBMappedKeyValue.value and
// FDBGetRangeReqAndResult.end are all invisible from Go. Decoding therefore
// happens in C, into these naturally-aligned mirrors that cgo maps in full.
typedef struct fdbgo_kv {
	const uint8_t* key;
	int key_length;
	const uint8_t* value;
	int value_length;
} fdbgo_kv;

typedef struct fdbgo_mapped_row {
	const uint8_t* key;
	int key_length;
	const uint8_t* value;
	int value_length;

	int kind; // 0 = none, 1 = getValue, 2 = getRange

	const uint8_t* gv_key;
	int gv_key_length;
	const uint8_t* gv_value;
	int gv_value_length;
	int gv_present;

	const uint8_t* range_begin_key;
	int range_begin_key_length;
	const uint8_t* range_end_key;
	int range_end_key_length;
	const FDBKeyValue* range_data;
	int range_count;
} fdbgo_mapped_row;

static void fdbgo_mkv_decode(const FDBMappedKeyValue* kv, fdbgo_mapped_row* out) {
	memset(out, 0, sizeof(*out));
	out->key = kv->key.key;
	out->key_length = kv->key.key_length;
	out->value = kv->value.key;
	out->value_length = kv->value.key_length;

	switch (fdbgo_mkv_variant_index(kv)) {
	case 0: {
		const fdbgo_getvalue_arm* arm = fdbgo_mkv_getvalue(kv);
		out->kind = 1;
		out->gv_key = arm->key;
		out->gv_key_length = arm->key_length;
		out->gv_present = arm->present ? 1 : 0;
		if (arm->present) {
			out->gv_value = arm->value;
			out->gv_value_length = arm->value_length;
		}
		break;
	}
	case 1:
		out->kind = 2;
		out->range_begin_key = kv->getRange.begin.key.key;
		out->range_begin_key_length = kv->getRange.begin.key.key_length;
		out->range_end_key = kv->getRange.end.key.key;
		out->range_end_key_length = kv->getRange.end.key.key_length;
		out->range_data = kv->getRange.data;
		out->range_count = kv->getRange.m_size;
		break;
	default:
		out->kind = 0;
		break;
	}
}

static void fdbgo_kv_at(const FDBKeyValue* data, int i, fdbgo_kv* out) {
	out->key = data[i].key;
	out->key_length = data[i].key_length;
	out->value = data[i].value;
	out->value_length = data[i].value_length;
}

#cgo LDFLAGS: -lfdb_c
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Mapped-request kinds, matching FDBMappedKeyValue's std::variant discriminator
// (shifted by one so the Go zero value means "no arm decoded").
const (
	CMappedKindNone     = 0 // valueless-by-exception, or an index this build does not know
	CMappedKindGetValue = 1 // std::variant index 0: GetValueReqAndResultRef
	CMappedKindGetRange = 2 // std::variant index 1: GetRangeReqAndResultRef
)

// CFDBError is an fdb_error_t with its raw code exposed. The differential
// compares error CODES between the C client and the pure-Go client, so the code
// must never be flattened into a string.
type CFDBError struct {
	Code int
	Msg  string
}

func (e *CFDBError) Error() string { return fmt.Sprintf("fdb error %d (%s)", e.Code, e.Msg) }

// cErr converts a non-zero fdb_error_t into a *CFDBError. Zero returns nil.
func cErr(code C.fdb_error_t) error {
	if code == 0 {
		return nil
	}
	return &CFDBError{Code: int(code), Msg: C.GoString(C.fdb_get_error(code))}
}

// CMappedRow is one decoded FDBMappedKeyValue: the primary row plus whichever
// arm of the mapped request the server filled in.
type CMappedRow struct {
	Key, Value []byte // the primary row

	Kind int // CMappedKindNone / CMappedKindGetValue / CMappedKindGetRange

	// Kind == CMappedKindGetValue
	GetValueKey     []byte
	GetValueValue   []byte
	GetValuePresent bool

	// Kind == CMappedKindGetRange
	RangeBeginKey []byte
	RangeEndKey   []byte
	RangeRows     []struct{ Key, Value []byte }
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------
//
// fdb_setup_network / fdb_run_network may be called exactly once per process,
// and cgofdb — which the rest of this package is built on — starts the network
// itself, lazily, inside OpenDatabase. Calling fdb_setup_network here as well
// would hand whichever call came second network_already_setup (2009), and
// cgofdb's run-network goroutine panics on error, so the failure would not even
// be catchable.
//
// We therefore do NOT own the network: cNetworkOnce forces cgofdb to bring it
// up (APIVersion is idempotent for the same version, and OpenDatabase performs
// the one legal fdb_setup_network + fdb_run_network), and everything after that
// is raw cgo. Only the FDBDatabase* is ours — fdb_create_database may be called
// as often as you like, and cgofdb does not expose its own handle. The upshot:
// this file and cgofdb coexist in one process, in either order, with a single
// network thread owned by cgofdb.
var (
	cNetworkOnce sync.Once
	cNetworkErr  error
	// cNetworkKeepalive pins the cgofdb Database that started the network. It is
	// deliberately never closed: cgofdb caches one handle per cluster file and
	// Close() destroys it for every sharer.
	cNetworkKeepalive cgofdb.Database
)

func cEnsureNetwork(clusterFile string) error {
	cNetworkOnce.Do(func() {
		if err := cgofdb.APIVersion(apiVersion); err != nil {
			cNetworkErr = fmt.Errorf("select api version %d: %w", apiVersion, err)
			return
		}
		db, err := cgofdb.OpenDatabase(clusterFile)
		if err != nil {
			cNetworkErr = fmt.Errorf("start libfdb_c network via cgofdb: %w", err)
			return
		}
		cNetworkKeepalive = db
	})
	return cNetworkErr
}

// CDatabase is a raw FDBDatabase*.
type CDatabase struct {
	ptr *C.FDBDatabase
}

// CTxnHandle is a raw FDBTransaction*.
type CTxnHandle struct {
	ptr *C.FDBTransaction
}

// Valid reports whether the handle points at a live transaction.
func (tr CTxnHandle) Valid() bool { return tr.ptr != nil }

// COpenDatabase brings up the libfdb_c network (once per process, see above)
// and opens clusterFile via fdb_create_database. An empty clusterFile means the
// default cluster file.
func COpenDatabase(clusterFile string) (*CDatabase, error) {
	if err := cEnsureNetwork(clusterFile); err != nil {
		return nil, err
	}
	var cf *C.char
	if clusterFile != "" {
		cf = C.CString(clusterFile)
		defer C.free(unsafe.Pointer(cf))
	}
	var db *C.FDBDatabase
	if err := cErr(C.fdb_create_database(cf, &db)); err != nil {
		return nil, err
	}
	return &CDatabase{ptr: db}, nil
}

// Close destroys the FDBDatabase. The network thread stays up — it is cgofdb's
// and is one-shot for the process.
func (db *CDatabase) Close() {
	if db == nil || db.ptr == nil {
		return
	}
	C.fdb_database_destroy(db.ptr)
	db.ptr = nil
}

// CreateTransaction issues fdb_database_create_transaction. The caller owns the
// returned handle and must Close it.
func (db *CDatabase) CreateTransaction() (CTxnHandle, error) {
	if db == nil || db.ptr == nil {
		return CTxnHandle{}, &CFDBError{Code: 2015, Msg: "database is closed"}
	}
	var tr *C.FDBTransaction
	if err := cErr(C.fdb_database_create_transaction(db.ptr, &tr)); err != nil {
		return CTxnHandle{}, err
	}
	return CTxnHandle{ptr: tr}, nil
}

// Close destroys the FDBTransaction.
func (tr *CTxnHandle) Close() {
	if tr == nil || tr.ptr == nil {
		return
	}
	C.fdb_transaction_destroy(tr.ptr)
	tr.ptr = nil
}

// Set issues fdb_transaction_set, so a test can seed a cluster through the same
// client it reads back with.
func (tr CTxnHandle) Set(key, value []byte) {
	kp, kl := cBytes(key)
	vp, vl := cBytes(value)
	C.fdb_transaction_set(tr.ptr, kp, kl, vp, vl)
	runtime.KeepAlive(key)
	runtime.KeepAlive(value)
}

// ClearRange issues fdb_transaction_clear_range over [begin, end).
func (tr CTxnHandle) ClearRange(begin, end []byte) {
	bp, bl := cBytes(begin)
	ep, el := cBytes(end)
	C.fdb_transaction_clear_range(tr.ptr, bp, bl, ep, el)
	runtime.KeepAlive(begin)
	runtime.KeepAlive(end)
}

// Commit issues fdb_transaction_commit and blocks for the result.
func (tr CTxnHandle) Commit() error {
	f := C.fdb_transaction_commit(tr.ptr)
	defer C.fdb_future_destroy(f)
	if err := cErr(C.fdb_future_block_until_ready(f)); err != nil {
		return err
	}
	return cErr(C.fdb_future_get_error(f))
}

// cBytes yields the (pointer, length) pair libfdb_c wants for a key or value.
// libfdb_c copies key/value bytes into the request before the call returns, so
// passing Go memory is legal under the cgo pointer rules — this is exactly what
// cgofdb does. Callers must runtime.KeepAlive the slice across the call.
func cBytes(b []byte) (*C.uint8_t, C.int) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(&b[0])), C.int(len(b))
}

func cBool(b bool) C.fdb_bool_t {
	if b {
		return 1
	}
	return 0
}

// goBytes copies len bytes out of C memory. Every byte a mapped-range row
// exposes is arena memory owned by the future, so it MUST be copied before
// fdb_future_destroy; a slice aliasing it turns into garbage the moment the
// arena is freed.
func goBytes(p *C.uint8_t, n C.int) []byte {
	if p == nil || n <= 0 {
		return []byte{}
	}
	return C.GoBytes(unsafe.Pointer(p), n)
}

// CGetMappedRange issues fdb_transaction_get_mapped_range on a raw libfdb_c
// transaction and returns the decoded rows. It is the differential's C-side
// reference.
//
// begin and end are plain keys, resolved as firstGreaterOrEqual selectors
// (or_equal=false, offset=1) — the same selectors every binding's
// KeyRange.ToRange produces. limit <= 0 means unlimited, in which case the
// streaming mode is WANT_ALL with iteration 0 and target_bytes 0; a positive
// limit switches to EXACT, which is what a row limit means to the server.
//
// The bool result is the future's `more` flag.
func CGetMappedRange(tr CTxnHandle, begin, end, mapper []byte, limit int, reverse bool, snapshot bool) ([]CMappedRow, bool, error) {
	if !tr.Valid() {
		return nil, false, &CFDBError{Code: 2015, Msg: "transaction is closed"}
	}

	bp, bl := cBytes(begin)
	ep, el := cBytes(end)
	mp, ml := cBytes(mapper)

	mode := C.FDBStreamingMode(C.FDB_STREAMING_MODE_WANT_ALL)
	if limit > 0 {
		mode = C.FDBStreamingMode(C.FDB_STREAMING_MODE_EXACT)
	} else {
		limit = 0
	}

	f := C.fdb_transaction_get_mapped_range(tr.ptr,
		bp, bl, 0 /* begin_or_equal */, 1, /* begin_offset */
		ep, el, 0 /* end_or_equal */, 1, /* end_offset */
		mp, ml,
		C.int(limit), 0 /* target_bytes */, mode, 0, /* iteration */
		cBool(snapshot), cBool(reverse))
	runtime.KeepAlive(begin)
	runtime.KeepAlive(end)
	runtime.KeepAlive(mapper)
	defer C.fdb_future_destroy(f)

	if err := cErr(C.fdb_future_block_until_ready(f)); err != nil {
		return nil, false, err
	}
	if err := cErr(C.fdb_future_get_error(f)); err != nil {
		return nil, false, err
	}

	var (
		arr   *C.FDBMappedKeyValue
		count C.int
		more  C.fdb_bool_t
	)
	if err := cErr(C.fdb_future_get_mappedkeyvalue_array(f, &arr, &count, &more)); err != nil {
		return nil, false, err
	}
	if count < 0 {
		return nil, false, fmt.Errorf("fdb_future_get_mappedkeyvalue_array returned count %d", int(count))
	}

	rows := make([]CMappedRow, 0, int(count))
	if arr != nil && count > 0 {
		kvs := unsafe.Slice(arr, int(count))
		for i := range kvs {
			rows = append(rows, decodeMappedRow(&kvs[i]))
		}
	}
	// Everything is copied out by decodeMappedRow above; the deferred
	// fdb_future_destroy is now safe.
	return rows, more != 0, nil
}

// decodeMappedRow copies one FDBMappedKeyValue — primary row and whichever
// variant arm is active — into Go memory.
func decodeMappedRow(kv *C.FDBMappedKeyValue) CMappedRow {
	var d C.fdbgo_mapped_row
	C.fdbgo_mkv_decode(kv, &d)

	row := CMappedRow{
		Key:   goBytes(d.key, d.key_length),
		Value: goBytes(d.value, d.value_length),
		Kind:  int(d.kind),
	}

	switch row.Kind {
	case CMappedKindGetValue:
		row.GetValueKey = goBytes(d.gv_key, d.gv_key_length)
		row.GetValuePresent = d.gv_present != 0
		if row.GetValuePresent {
			row.GetValueValue = goBytes(d.gv_value, d.gv_value_length)
		}

	case CMappedKindGetRange:
		row.RangeBeginKey = goBytes(d.range_begin_key, d.range_begin_key_length)
		row.RangeEndKey = goBytes(d.range_end_key, d.range_end_key_length)
		row.RangeRows = []struct{ Key, Value []byte }{}
		if n := int(d.range_count); n > 0 && d.range_data != nil {
			row.RangeRows = make([]struct{ Key, Value []byte }, 0, n)
			var inner C.fdbgo_kv
			for i := 0; i < n; i++ {
				C.fdbgo_kv_at(d.range_data, C.int(i), &inner)
				row.RangeRows = append(row.RangeRows, struct{ Key, Value []byte }{
					Key:   goBytes(inner.key, inner.key_length),
					Value: goBytes(inner.value, inner.value_length),
				})
			}
		}
	}

	return row
}
