package client

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// Client-side key/value size-limit parity with libfdb_c — C2 differential.
// The C binding aborts the process on an oversized set/atomicOp; we reject the
// commit (set/atomic) or clamp the range (clear) instead. See sizelimits.go.

func sizeErrCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var fe *wire.FDBError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *wire.FDBError, got %T: %v", err, err)
	}
	return int(fe.Code)
}

func TestGetMaxWriteKeySize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		key       []byte
		rawAccess bool
		want      int
	}{
		{"normal", []byte("foo"), false, keySizeLimit},                             // 10000
		{"normal+rawAccess", []byte("foo"), true, keySizeLimit + tenantPrefixSize}, // 10008
		{"system", []byte("\xfffoo"), false, systemKeySizeLimit},                   // 30000
		{"system+rawAccess", []byte("\xfffoo"), true, systemKeySizeLimit},          // 30000 (raw doesn't add for system)
		{"empty", []byte{}, false, keySizeLimit},
	}
	for _, tc := range cases {
		if got := getMaxWriteKeySize(tc.key, tc.rawAccess); got != tc.want {
			t.Errorf("%s: getMaxWriteKeySize=%d, want %d", tc.name, got, tc.want)
		}
	}
	// getMaxClearKeySize == getMaxKeySize == getMaxWriteKeySize(key, true).
	if got := getMaxClearKeySize([]byte("foo")); got != keySizeLimit+tenantPrefixSize {
		t.Errorf("getMaxClearKeySize(normal)=%d, want %d", got, keySizeLimit+tenantPrefixSize)
	}
	if got := getMaxClearKeySize([]byte("\xfffoo")); got != systemKeySizeLimit {
		t.Errorf("getMaxClearKeySize(system)=%d, want %d", got, systemKeySizeLimit)
	}
}

func TestCommit_RejectsOversizedSetValue(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.Set([]byte("k"), make([]byte, valueSizeLimit+1)) // 100001 > VALUE_SIZE_LIMIT
	if code := sizeErrCode(t, tx.Commit(context.Background())); code != 2103 {
		t.Fatalf("oversized Set value: Commit code=%d, want 2103 (value_too_large)", code)
	}
}

func TestCommit_AcceptsValueAtLimit(t *testing.T) {
	t.Parallel()
	// A value of exactly VALUE_SIZE_LIMIT must NOT be rejected (strict `>`); it
	// proceeds past validation (and then fails on the absent connection, never with
	// the size error). We only assert it is not 2103.
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.Set([]byte("k"), make([]byte, valueSizeLimit)) // exactly 100000
	err := commitNoConn(tx)
	if code := sizeErrCode(t, err); code == 2103 {
		t.Fatalf("value of exactly VALUE_SIZE_LIMIT must NOT be value_too_large; got 2103")
	}
}

func TestCommit_RejectsOversizedKey(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.Set(bytes.Repeat([]byte("a"), keySizeLimit+1), []byte("v")) // 10001 > KEY_SIZE_LIMIT
	if code := sizeErrCode(t, tx.Commit(context.Background())); code != 2102 {
		t.Fatalf("oversized Set key: Commit code=%d, want 2102 (key_too_large)", code)
	}
}

func TestCommit_RejectsOversizedAtomicOperand(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.Atomic(MutAddValue, []byte("k"), make([]byte, valueSizeLimit+1))
	if code := sizeErrCode(t, tx.Commit(context.Background())); code != 2103 {
		t.Fatalf("oversized atomic operand: Commit code=%d, want 2103 (value_too_large)", code)
	}
}

func TestCommit_SystemKeyHasHigherLimit(t *testing.T) {
	t.Parallel()
	// With writeSystemKeys, a \xff key is allowed up to SYSTEM_KEY_SIZE_LIMIT
	// (30000). A normal key of the same length (>10000) would be rejected — proving
	// the system/normal distinction. Here we reject only at >30000.
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.writeSystemKeys = true
	key := append([]byte{0xff}, bytes.Repeat([]byte{0}, systemKeySizeLimit+1)...) // len 30002, system
	tx.Set(key, []byte("v"))
	if code := sizeErrCode(t, tx.Commit(context.Background())); code != 2102 {
		t.Fatalf("oversized system key: Commit code=%d, want 2102", code)
	}
}

// TestCommit_RawAccessRaisesNonSystemKeyLimit pins the commit-path wiring of the
// key-size check. C++ sets options.rawAccess for ANY of RAW_ACCESS,
// ACCESS_SYSTEM_KEYS, or READ_SYSTEM_KEYS (NativeAPI.actor.cpp:7159-7170), and
// rawAccess raises the non-system key limit to KEY_SIZE_LIMIT+8. So with
// writeSystemKeys (ACCESS_SYSTEM_KEYS) or readSystemKeys (READ_SYSTEM_KEYS) set, a
// non-system key of 10001-10008 bytes must be ACCEPTED (libfdb_c accepts it);
// passing false would wrongly reject it (the regression codex caught). A key past
// 10008 is still rejected.
func TestCommit_RawAccessRaisesNonSystemKeyLimit(t *testing.T) {
	t.Parallel()
	mk := func(n int) []byte { return bytes.Repeat([]byte("a"), n) } // non-system

	for _, opt := range []string{"writeSystemKeys", "readSystemKeys"} {
		opt := opt
		t.Run(opt, func(t *testing.T) {
			t.Parallel()
			// 10008 (== KEY_SIZE_LIMIT+8): accepted under raw access.
			tx := newTestTx()
			tx.tenantId = NoTenantID
			tx.rywDisabled = true
			if opt == "writeSystemKeys" {
				tx.writeSystemKeys = true
			} else {
				tx.readSystemKeys = true
			}
			tx.Set(mk(keySizeLimit+tenantPrefixSize), []byte("v")) // 10008
			if code := sizeErrCode(t, commitNoConn(tx)); code == 2102 {
				t.Fatalf("%s: non-system key of KEY_SIZE_LIMIT+8 must be accepted (rawAccess), got 2102", opt)
			}
			// 10009 (> KEY_SIZE_LIMIT+8): still rejected.
			tx2 := newTestTx()
			tx2.tenantId = NoTenantID
			tx2.rywDisabled = true
			if opt == "writeSystemKeys" {
				tx2.writeSystemKeys = true
			} else {
				tx2.readSystemKeys = true
			}
			tx2.Set(mk(keySizeLimit+tenantPrefixSize+1), []byte("v")) // 10009
			if code := sizeErrCode(t, tx2.Commit(context.Background())); code != 2102 {
				t.Fatalf("%s: non-system key past KEY_SIZE_LIMIT+8 must be rejected, got %d", opt, code)
			}
		})
	}
}

// TestClear_NoOpConsumesNextWriteNoConflict pins codex's second finding: C++ RYW
// clear consumes the NEXT_WRITE_NO_WRITE_CONFLICT_RANGE flag at the top
// (ReadYourWrites.actor.cpp:2407), above the size no-op. So an oversized single-key
// Clear (dropped) and a clamped-to-empty ClearRange must STILL consume the flag —
// otherwise it leaks and the next real write wrongly skips its conflict range.
func TestClear_NoOpConsumesNextWriteNoConflict(t *testing.T) {
	t.Parallel()
	t.Run("oversized_single_key", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.tenantId = NoTenantID
		tx.rywDisabled = true
		tx.nextWriteNoConflict = true
		tx.Clear(bytes.Repeat([]byte("a"), keySizeLimit+tenantPrefixSize+1)) // dropped
		if tx.nextWriteNoConflict {
			t.Fatal("oversized Clear did not consume nextWriteNoConflict (C++ RYW :2407)")
		}
		tx.Set([]byte("k2"), []byte("v")) // next real write must add its range
		if len(tx.writeConflicts) != 1 {
			t.Fatalf("flag leaked: follow-up Set added %d conflict ranges, want 1", len(tx.writeConflicts))
		}
	})
	t.Run("clamped_empty_range", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.tenantId = NoTenantID
		tx.rywDisabled = true
		tx.nextWriteNoConflict = true
		// Two oversized keys that clamp to the same prefix → empty range, dropped.
		big := bytes.Repeat([]byte("a"), 20000)
		if err := tx.ClearRange(big, append(append([]byte(nil), big...), 0xff)); err != nil {
			t.Fatalf("ClearRange: %v", err)
		}
		if len(tx.mutations) != 0 {
			t.Fatalf("clamped-empty ClearRange should record no mutation, got %d", len(tx.mutations))
		}
		if tx.nextWriteNoConflict {
			t.Fatal("clamped-empty ClearRange did not consume nextWriteNoConflict (C++ RYW)")
		}
		tx.Set([]byte("k2"), []byte("v"))
		if len(tx.writeConflicts) != 1 {
			t.Fatalf("flag leaked: follow-up Set added %d conflict ranges, want 1", len(tx.writeConflicts))
		}
	})
}

func TestClear_DropsOversizedKey(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.Clear(bytes.Repeat([]byte("a"), keySizeLimit+tenantPrefixSize+1)) // > getMaxClearKeySize
	if len(tx.mutations) != 0 {
		t.Fatalf("oversized single-key Clear must be dropped (C++ NativeAPI:6045-6047); got %d mutations", len(tx.mutations))
	}
	if len(tx.writeConflicts) != 0 {
		t.Fatalf("dropped Clear must add no write-conflict range; got %d", len(tx.writeConflicts))
	}
}

func TestClearRange_ClampsOversizedKeys(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	maxSz := keySizeLimit + tenantPrefixSize // getMaxClearKeySize for a normal key
	begin := bytes.Repeat([]byte("a"), 20000)
	end := bytes.Repeat([]byte("b"), 20000)
	if err := tx.ClearRange(begin, end); err != nil {
		t.Fatalf("ClearRange: %v", err)
	}
	if len(tx.mutations) != 1 {
		t.Fatalf("ClearRange should record 1 mutation, got %d", len(tx.mutations))
	}
	m := tx.mutations[0]
	if len(m.Key) != maxSz+1 || len(m.Value) != maxSz+1 {
		t.Fatalf("ClearRange keys not clamped to maxSize+1=%d: begin=%d end=%d (C++ NativeAPI:6019-6028)",
			maxSz+1, len(m.Key), len(m.Value))
	}
}

// commitNoConn runs Commit on a connectionless test tx, recovering the panic that
// ensureReadVersion/commit raise on the nil database. A validation error (e.g. a
// size rejection) returns BEFORE the network path, so it surfaces as the returned
// error; anything that gets past validation panics and is reported as nil here.
func commitNoConn(tx *Transaction) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil // got past validation → no size error
		}
	}()
	return tx.Commit(context.Background())
}
