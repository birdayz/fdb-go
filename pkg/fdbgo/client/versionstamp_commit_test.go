package client

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestAtomic_SVKCommitsMinBoundTransformedKey pins finding #26: a SetVersionstampedKey mutation must
// be COMMITTED with the key transformed to carry the cached-read-version min-bound stamp at the
// placeholder — matching libfdb_c/Java, which capture getCachedReadVersion().orDefault(0), mutate the
// key in place (ReadYourWrites.actor.cpp:2276), store it in the write map (:2295), and ship THAT key
// on commit (:2059). Go previously buffered the user's RAW zero placeholder. The commit proxy
// overwrites [pos,pos+10) with the assigned stamp and strips the 4-byte offset, so the STORED record
// is byte-identical either way (the wire-compat hard line held) — but the commit-REQUEST bytes now
// match the reference clients. Revert-proof: buffer the raw key and the placeholder is all-zero, not
// the min-bound stamp. Deterministic, no container.
func TestAtomic_SVKCommitsMinBoundTransformedKey(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.rywDisabled = true // isolate the commit buffer; the RYW model path is exercised separately
	const rv = int64(0x0102030405060708)
	tx.readVersionMu.Lock()
	tx.hasReadVersion = true
	tx.readVersion = rv // C++ tr.getCachedReadVersion().orDefault(0)
	tx.readVersionMu.Unlock()

	// Well-formed SVK key: 1-byte prefix + 10-byte placeholder + 4-byte LE offset=1 → placeholder [1,11).
	key := make([]byte, 15)
	key[0] = 'k'
	binary.LittleEndian.PutUint32(key[11:], 1)
	tx.Atomic(MutSetVersionstampedKey, key, []byte("v"))

	if len(tx.mutations) != 1 {
		t.Fatalf("want 1 buffered mutation, got %d", len(tx.mutations))
	}
	got := tx.mutations[0].Key
	if len(got) != 15 {
		t.Fatalf("committed SVK key length changed: %d", len(got))
	}
	// [1,11) must be the min-bound stamp: 8-byte BE read version + 2-byte BE txn number (0).
	var wantStamp [10]byte
	binary.BigEndian.PutUint64(wantStamp[:8], uint64(rv))
	if !bytes.Equal(got[1:11], wantStamp[:]) {
		t.Fatalf("SVK commit-key placeholder = %x, want min-bound stamp %x (BE readVersion + BE 0) — "+
			"Go must transform the committed key like C++, not ship the raw zero placeholder", got[1:11], wantStamp[:])
	}
	// Prefix and the 4-byte LE offset suffix are preserved unchanged.
	if got[0] != 'k' || binary.LittleEndian.Uint32(got[11:]) != 1 {
		t.Fatalf("SVK commit-key prefix/offset not preserved: %x", got)
	}
}

// TestAtomic_SVKSystemKeyPlaceholderStaysRawForValidation pins codex #17's P2 on the #26 fix: a
// SetVersionstampedKey whose placeholder is at offset 0 and whose raw placeholder bytes are \xff (a
// raw system key) must NOT be transformed for a non-system txn — it stays RAW so the commit-path
// validateMutation checks the raw key and reports key_outside_legal_range (2004). Transforming first
// would hide the leading \xff behind the read-version/zero stamp and let the mutation commit without
// system-key access. Revert-proof: drop the `key < maxWriteKey` guard and the transformed key's
// leading byte is the stamp (not \xff), so validateMutation passes and the check is bypassed.
func TestAtomic_SVKSystemKeyPlaceholderStaysRawForValidation(t *testing.T) {
	t.Parallel()
	tx := newTestTx() // non-system txn: maxWriteKey == \xff
	tx.rywDisabled = true
	tx.readVersionMu.Lock()
	tx.hasReadVersion = true
	tx.readVersion = 0x0102030405060708 // non-zero, so the transform WOULD change the leading placeholder
	tx.readVersionMu.Unlock()

	// SVK key: 10 x \xff placeholder + LE offset 0 → placeholder at [0,10), raw key is a system key.
	key := make([]byte, 14)
	for i := 0; i < 10; i++ {
		key[i] = 0xff
	}
	binary.LittleEndian.PutUint32(key[10:], 0)
	tx.Atomic(MutSetVersionstampedKey, key, []byte("v"))

	if len(tx.mutations) != 1 {
		t.Fatalf("want 1 buffered mutation, got %d", len(tx.mutations))
	}
	if got := tx.mutations[0].Key; got[0] != 0xff {
		t.Fatalf("out-of-range \\xff-placeholder SVK was transformed (leading byte %#x, not 0xff) — the "+
			"legal-range check on the raw key is bypassed (codex #17)", got[0])
	}
	if err := tx.validateMutation(tx.mutations[0], tx.maxWriteKey()); fdbCodeOf(err) != 2004 {
		t.Fatalf("a \\xff-placeholder SVK on a non-system txn must be key_outside_legal_range (2004), got %v", err)
	}
}

// TestAtomic_SVKWriteOnlyTxnKeepsZeroPlaceholder pins the orDefault(0) edge: a write-only transaction
// with NO cached read version transforms the placeholder with version 0 — which is byte-identical to
// the raw zero placeholder, so Go and libfdb_c agree (C++ getCachedReadVersion().orDefault(0) == 0).
func TestAtomic_SVKWriteOnlyTxnKeepsZeroPlaceholder(t *testing.T) {
	t.Parallel()
	tx := newTestTx() // no read version set → hasReadVersion == false
	tx.rywDisabled = true
	key := make([]byte, 15)
	key[0] = 'k'
	binary.LittleEndian.PutUint32(key[11:], 1)
	tx.Atomic(MutSetVersionstampedKey, key, []byte("v"))

	if len(tx.mutations) != 1 {
		t.Fatalf("want 1 buffered mutation, got %d", len(tx.mutations))
	}
	got := tx.mutations[0].Key
	var zero [10]byte
	if !bytes.Equal(got[1:11], zero[:]) {
		t.Fatalf("write-only-txn SVK placeholder = %x, want all-zero (min-bound stamp of version 0)", got[1:11])
	}
}
