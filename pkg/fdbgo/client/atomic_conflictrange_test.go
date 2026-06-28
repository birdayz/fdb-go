package client

import (
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Per-op write-conflict-range rules for atomic mutations — C2 differential vs
// libfdb_c. These pin the CLIENT-determined conflict-range vector that ships in
// CommitTransactionRequest, which the server stores/uses verbatim.
//
// C++ reference (ReadYourWrites.actor.cpp::atomicOp, FDB 7.3):
//   :2220  addWriteConflict = !getAndResetWriteConflictDisabled()  (consume NEXT_WRITE flag)
//   :2268  SetVersionstampedKey → addWriteConflict = AddConflictRange::False  (NO range)
//   every other atomic op keeps addWriteConflict and pushes singleKeyRange(key)
//   (NativeAPI.actor.cpp:6005: `addConflictRange && operationType != SetVersionstampedKey`)
//
// The proof taps the *marshaled* CommitTransactionRequest (build → unmarshal) so
// it asserts what actually reaches the wire, not the in-memory struct.

// buildAndParse drives the given atomic op on a fresh transaction and returns the
// write-conflict ranges that survive marshaling into CommitTransactionRequest.
func buildAndParseWriteConflicts(t *testing.T, op MutationType, key, operand []byte) []types.KeyRangeRef {
	t.Helper()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true // bare test tx has no RYW cache; we only probe conflict bookkeeping

	tx.Atomic(op, key, operand)

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations)
	defer marshalBufPool.Put(bufp)
	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	return req.Transaction.WriteConflictRanges
}

// TestAtomic_SetVersionstampedKey_NoWriteConflictRange pins the divergence found
// during RFC-053 investigation: client.Atomic used to add a write-conflict range
// for SetVersionstampedKey, but C++ forces AddConflictRange::False for it
// (ReadYourWrites.actor.cpp:2268). The incomplete versionstamp key would
// otherwise spuriously conflict two txns stamping the same logical key.
func TestAtomic_SetVersionstampedKey_NoWriteConflictRange(t *testing.T) {
	t.Parallel()
	// key with a trailing 4-byte LE offset suffix (=0) as the API requires.
	key := []byte("vskey\x00\x00\x00\x00")
	wcrs := buildAndParseWriteConflicts(t, MutSetVersionstampedKey, key, []byte("v"))
	if len(wcrs) != 0 {
		t.Fatalf("SetVersionstampedKey: got %d write-conflict ranges, want 0 (C++ ReadYourWrites.actor.cpp:2268)", len(wcrs))
	}
}

// TestAtomic_NonVersionstampedKeyOps_AddOneWriteConflictRange covers the
// complement: every other atomic op (including SetVersionstampedValue, whose key
// IS complete) adds exactly one write-conflict range [key, key\x00).
func TestAtomic_NonVersionstampedKeyOps_AddOneWriteConflictRange(t *testing.T) {
	t.Parallel()
	key := []byte("k")
	want := types.KeyRangeRef{Begin: []byte("k"), End: []byte("k\x00")}
	ops := []struct {
		name string
		op   MutationType
	}{
		{"Add", MutAddValue},
		{"And", MutAnd},
		{"Or", MutOr},
		{"Xor", MutXor},
		{"Max", MutMax},
		{"Min", MutMin},
		{"MinV2", MutMinV2},
		{"AndV2", MutAndV2},
		{"ByteMin", MutByteMin},
		{"ByteMax", MutByteMax},
		{"AppendIfFits", MutAppendIfFits},
		{"CompareAndClear", MutCompareAndClear},
		{"SetVersionstampedValue", MutSetVersionstampedValue},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			operand := []byte("v")
			if tc.op == MutSetVersionstampedValue {
				// value needs a trailing 4-byte LE offset suffix; 10-byte stamp room.
				operand = append(make([]byte, 10), 0, 0, 0, 0)
			}
			wcrs := buildAndParseWriteConflicts(t, tc.op, key, operand)
			if len(wcrs) != 1 {
				t.Fatalf("%s: got %d write-conflict ranges, want 1", tc.name, len(wcrs))
			}
			if string(wcrs[0].Begin) != string(want.Begin) || string(wcrs[0].End) != string(want.End) {
				t.Errorf("%s: WCR = [%q,%q), want [%q,%q)", tc.name,
					wcrs[0].Begin, wcrs[0].End, want.Begin, want.End)
			}
		})
	}
}

// TestAtomic_SetVersionstampedKey_ConsumesNextWriteNoConflictFlag pins the second
// half of the C++ contract: even though SetVersionstampedKey adds no range, it
// STILL consumes the NEXT_WRITE_NO_WRITE_CONFLICT_RANGE flag (getAndReset at
// ReadYourWrites.actor.cpp:2220), so the flag does NOT leak onto the next write.
func TestAtomic_SetVersionstampedKey_ConsumesNextWriteNoConflictFlag(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.rywDisabled = true
	tx.nextWriteNoConflict = true

	tx.Atomic(MutSetVersionstampedKey, []byte("vskey\x00\x00\x00\x00"), []byte("v"))
	if tx.nextWriteNoConflict {
		t.Fatal("SetVersionstampedKey did not consume nextWriteNoConflict flag (C++ getAndReset, :2220)")
	}
	if len(tx.writeConflicts) != 0 {
		t.Fatalf("SetVersionstampedKey added %d write-conflict ranges, want 0", len(tx.writeConflicts))
	}

	// The next non-VSK atomic must add its range (flag was consumed, not leaked).
	tx.Atomic(MutAddValue, []byte("k2"), []byte("v"))
	if len(tx.writeConflicts) != 1 {
		t.Fatalf("follow-up Add: got %d write-conflict ranges, want 1 (flag should not have leaked)", len(tx.writeConflicts))
	}
}
