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

// TestCommit_SystemKeysOptionDoesNotRaiseNonSystemKeyLimit pins the commit-path
// wiring of the key-size check: ACCESS_SYSTEM_KEYS (tx.writeSystemKeys) must NOT be
// passed as RAW_ACCESS to getMaxWriteKeySize. A NON-system key of 10001 bytes must
// be rejected even with writeSystemKeys set — otherwise it would wrongly get the
// rawAccess limit (KEY_SIZE_LIMIT+8) and slip through (the divergence Torvalds/
// FDB-C-dev flagged). Fails pre-fix (the key is accepted → no 2102).
func TestCommit_SystemKeysOptionDoesNotRaiseNonSystemKeyLimit(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.writeSystemKeys = true                                      // ACCESS_SYSTEM_KEYS — must not raise the non-system limit
	tx.Set(bytes.Repeat([]byte("a"), keySizeLimit+1), []byte("v")) // 10001, non-system
	if code := sizeErrCode(t, tx.Commit(context.Background())); code != 2102 {
		t.Fatalf("non-system key 10001 with writeSystemKeys: Commit code=%d, want 2102 "+
			"(ACCESS_SYSTEM_KEYS must not be treated as RAW_ACCESS)", code)
	}
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
