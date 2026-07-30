package simfdb

import (
	"bytes"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// allRange is the whole user keyspace as an ExactRange, for scans that must see everything a
// transaction can legally read.
func allRange() fdb.KeyRange { return fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")} }

// ---- M8(a): cancellation reaches every read entry point --------------------------------

// TestCancelledTransactionReadEntryPoints pins that every read entry point reports
// transaction_cancelled(1025) after Cancel(), not just Get and GetKey.
//
// In the client every one of these funnels through ensureReadVersion, whose first act is
// checkCancelled (client/transaction.go:662-665); the two metrics entry points bypass the read
// version and so gate explicitly (client/metrics.go:36-38, :186-188). A gap here is not
// cosmetic: a cancelled transaction that can still scan a range hands the caller data it must
// never have seen, and the sim would certify a driver that reads after cancellation as correct.
func TestCancelledTransactionReadEntryPoints(t *testing.T) {
	t.Parallel()
	db := New(nil)
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("a"), []byte("1"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	tx := db.newTxn()
	tx.Cancel()

	t.Run("Get", func(t *testing.T) {
		_, err := tx.Get(k("a")).Get()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("Get after Cancel: got %d, want transaction_cancelled(1025)", code)
		}
	})
	t.Run("GetKey", func(t *testing.T) {
		_, err := tx.GetKey(fdb.FirstGreaterOrEqual(k("a"))).Get()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("GetKey after Cancel: got %d, want transaction_cancelled(1025)", code)
		}
	})
	t.Run("GetRange_GetSlice", func(t *testing.T) {
		_, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("GetRange after Cancel: got %d, want transaction_cancelled(1025)", code)
		}
	})
	t.Run("GetRange_Iterator", func(t *testing.T) {
		// The iterator path must refuse too — a RangeResult that reports the error only on
		// GetSlice would still stream rows to a cursor.
		it := tx.GetRange(allRange(), fdb.RangeOptions{}).Iterator()
		if it.Advance() {
			t.Fatal("iterator over a cancelled transaction advanced")
		}
		if _, err := it.Get(); errCode(t, err) != 1025 {
			t.Fatalf("iterator Get after Cancel: got %d, want transaction_cancelled(1025)",
				errCode(t, err))
		}
	})
	t.Run("GetReadVersion", func(t *testing.T) {
		_, err := tx.GetReadVersion().Get()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("GetReadVersion after Cancel: got %d, want transaction_cancelled(1025)", code)
		}
	})
	t.Run("GetEstimatedRangeSizeBytes", func(t *testing.T) {
		_, err := tx.GetEstimatedRangeSizeBytes(allRange()).Get()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("GetEstimatedRangeSizeBytes after Cancel: got %d, want 1025", code)
		}
	})
	t.Run("GetRangeSplitPoints", func(t *testing.T) {
		_, err := tx.GetRangeSplitPoints(allRange(), 1000).Get()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("GetRangeSplitPoints after Cancel: got %d, want 1025", code)
		}
	})
	t.Run("Snapshot_reads_too", func(t *testing.T) {
		// The snapshot view holds the transaction, so cancellation must reach through it.
		sn := tx.Snapshot()
		if _, err := sn.Get(k("a")).Get(); errCode(t, err) != 1025 {
			t.Fatal("snapshot Get after Cancel did not report 1025")
		}
		if _, err := sn.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError(); errCode(t, err) != 1025 {
			t.Fatal("snapshot GetRange after Cancel did not report 1025")
		}
	})
	t.Run("GetVersionstamp", func(t *testing.T) {
		_, err := tx.GetVersionstamp().Get()
		if code := errCode(t, err); code != 1025 {
			t.Fatalf("GetVersionstamp after Cancel: got %d, want 1025 (it out-ranks the "+
				"not-yet-committed verdict, client/transaction.go:2217-2219)", code)
		}
	})
}

// TestCancelledTransactionApproximateSizeStillAnswers is the NEGATIVE half of the cancellation
// sweep, and it is load-bearing: it pins the one read-ish entry point that must NOT gate on
// cancellation, so a later "finish the sweep" pass cannot quietly add the gate everywhere.
//
// C++ gates getApproximateSize on the deferred error and nothing else — no resetPromise race —
// so a cancelled-but-unpoisoned transaction still returns its size (ThreadSafeTransaction.cpp:
// 715-721, ported at client/transaction.go:2510-2517, whose differential_cancel_test pins the
// parity against libfdb_c).
func TestCancelledTransactionApproximateSizeStillAnswers(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.Set(k("a"), []byte("1"))
	before, err := tx.GetApproximateSize().Get()
	if err != nil {
		t.Fatalf("GetApproximateSize before Cancel: %v", err)
	}
	tx.Cancel()
	after, err := tx.GetApproximateSize().Get()
	if err != nil {
		t.Fatalf("GetApproximateSize after Cancel must still answer, got %v", err)
	}
	if after != before {
		t.Fatalf("size changed across Cancel: %d -> %d", before, after)
	}
}

// TestGetEstimatedRangeSizeBytesInvertedRangeOutranksCancel pins the PRECEDENCE, not just the
// presence, of the cancellation gate on the two metrics entry points. libfdb_c constructs a
// KeyRangeRef from the C arguments before entering the op, and that constructor throws
// inverted_range(2005) ahead of everything else (client/metrics.go:29-33, :177-184) — so a
// cancelled transaction asked about an inverted range reports 2005, not 1025. A cancel gate
// bolted on at the top would invert that and is exactly the mistake this pins.
func TestGetEstimatedRangeSizeBytesInvertedRangeOutranksCancel(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.Cancel()
	inverted := fdb.KeyRange{Begin: k("z"), End: k("a")}
	if _, err := tx.GetEstimatedRangeSizeBytes(inverted).Get(); errCode(t, err) != 2005 {
		t.Fatalf("GetEstimatedRangeSizeBytes(inverted) on a cancelled txn: got %d, want inverted_range(2005)",
			errCode(t, err))
	}
	if _, err := tx.GetRangeSplitPoints(inverted, 1000).Get(); errCode(t, err) != 2005 {
		t.Fatal("GetRangeSplitPoints(inverted) on a cancelled txn did not report 2005")
	}
}

// ---- M8(b): Set(k, nil) is a present empty value, not a tombstone ----------------------

// TestSetNilValueIsPresentEmpty pins all four views of Set(k, nil) — the three that disagreed
// (Get, GetRange, the committed store) plus the post-commit read.
//
// Real FDB has no way to spell "Set to absent": fdb_transaction_set takes a (pointer, length)
// pair, so a nil operand and a zero-length one are the same call and both store a PRESENT,
// zero-length value. The pure-Go client makes it explicit — rywCache.set copies through
// `make([]byte, len(value))` (client/ryw.go:223-224), which is non-nil for a nil operand, and
// rywEntry's doc distinguishes `value==nil ("Set to empty bytes", a present key)` from its
// separate `absent` flag (client/ryw.go:88-91).
//
// Before the fix SimFDB gave three answers to that one question: Get returned nil (absent),
// GetRange returned the key (present), and the commit wrote a tombstone so the key was gone.
func TestSetNilValueIsPresentEmpty(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{"nil operand", nil},
		{"empty operand", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			// A neighbouring key so a range scan that wrongly drops the empty-valued key still
			// returns something and the failure reads as "1 row, wanted 2".
			if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(k("z-neighbour"), []byte("n"))
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}

			tx := db.newTxn()
			tx.Set(k("e"), tc.value)

			// View 1: the RYW point read (tx.resolveKey).
			v, err := tx.Get(k("e")).Get()
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if v == nil {
				t.Fatal("uncommitted Get of a Set-to-empty key returned nil (absent); " +
					"want a present, zero-length value")
			}
			if len(v) != 0 {
				t.Fatalf("uncommitted Get = %q, want zero-length", v)
			}

			// View 2: the merged view a range scan / key selector runs over (tx.buildView →
			// applyMutationToView).
			kvs, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				t.Fatalf("GetRange: %v", err)
			}
			if got := findKV(kvs, "e"); got == nil {
				t.Fatalf("uncommitted GetRange did not return the Set-to-empty key; got %v", kvs)
			} else if len(got.Value) != 0 {
				t.Fatalf("uncommitted GetRange value = %q, want zero-length", got.Value)
			}

			// View 3: the committed store (conflict.go applyMutations → store.put).
			if err := tx.Commit().Get(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			rtx := db.newTxn()
			cv, err := rtx.Get(k("e")).Get()
			if err != nil {
				t.Fatalf("post-commit Get: %v", err)
			}
			if cv == nil {
				t.Fatal("post-commit Get returned nil: the commit wrote a TOMBSTONE for a Set")
			}
			if len(cv) != 0 {
				t.Fatalf("post-commit Get = %q, want zero-length", cv)
			}

			// View 4: the post-commit range scan, which reads the store directly rather than
			// the write buffer.
			ckvs, err := rtx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				t.Fatalf("post-commit GetRange: %v", err)
			}
			if got := findKV(ckvs, "e"); got == nil {
				t.Fatalf("post-commit GetRange did not return the Set-to-empty key; got %v", ckvs)
			} else if got.Value == nil {
				t.Fatal("post-commit GetRange returned a nil Value for a present key: nil is " +
					"this package's spelling of absent, so the row is self-contradictory")
			}

			// And the key selector walk must count the slot: GetKey over the merged view is the
			// path a cursor uses to find the first key at or after "e".
			gk, err := rtx.GetKey(fdb.FirstGreaterOrEqual(k("e"))).Get()
			if err != nil {
				t.Fatalf("GetKey: %v", err)
			}
			if string(gk) != "e" {
				t.Fatalf("GetKey(FirstGreaterOrEqual(e)) = %q, want %q — the empty-valued key is "+
					"not in the selector's view", gk, "e")
			}
		})
	}
}

// TestClearStillRemovesAnEmptyValue is the counterweight to the test above: normalizing a nil
// Set operand to present-empty must not make Clear stop working. Clear is the ONLY way to make
// a key absent, and the store still uses nil for the tombstone.
func TestClearStillRemovesAnEmptyValue(t *testing.T) {
	t.Parallel()
	db := New(nil)
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("e"), nil)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Clear(k("e"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tx := db.newTxn()
	if v, err := tx.Get(k("e")).Get(); err != nil || v != nil {
		t.Fatalf("Get after Clear = %v, %v; want nil (absent)", v, err)
	}
	kvs, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		t.Fatal(err)
	}
	if findKV(kvs, "e") != nil {
		t.Fatalf("GetRange after Clear still returned the key: %v", kvs)
	}
}

// TestStoreRangeAtKeepsPresentEmptyValuesNonNil is a WHITE-BOX pin on mvccStore.rangeAt, and it
// exists because the public API cannot currently reach the bug.
//
// rangeAt has exactly one caller — GetEstimatedRangeSizeBytes — which only sums len(Key)+
// len(Value), so a present zero-length value handed back as a nil Value is invisible today. That
// makes the defect LATENT, not absent: nil is this package's spelling of "absent", so the moment
// any caller looks at Value (a range read routed through the store instead of buildView, a
// model-based oracle diffing store contents), a Set-to-empty key starts reading as a tombstone.
//
// If this test is what breaks after such a rewiring, the re-armed bug is: rangeAt returning a
// self-contradictory row — a key that is present with an absent value.
func TestStoreRangeAtKeepsPresentEmptyValuesNonNil(t *testing.T) {
	t.Parallel()
	s := &mvccStore{}
	s.put([]byte("empty"), []byte{}, 1)   // present, zero-length
	s.put([]byte("full"), []byte("v"), 1) // present, non-empty
	s.put([]byte("gone"), nil, 1)         // tombstone

	kvs := s.rangeAt([]byte(""), []byte("\xff"), 1)
	if len(kvs) != 2 {
		t.Fatalf("rangeAt returned %d rows, want 2 (the tombstone must be skipped): %v", len(kvs), kvs)
	}
	got := findKV(kvs, "empty")
	if got == nil {
		t.Fatal("rangeAt dropped a present, zero-length value")
	}
	if got.Value == nil {
		t.Fatal("rangeAt returned a nil Value for a present key — nil means ABSENT in this " +
			"package, so the row claims the key both exists and does not")
	}
	if len(got.Value) != 0 {
		t.Fatalf("rangeAt value = %q, want zero-length", got.Value)
	}
}

func findKV(kvs []fdb.KeyValue, key string) *fdb.KeyValue {
	for i := range kvs {
		if bytes.Equal(kvs[i].Key, []byte(key)) {
			return &kvs[i]
		}
	}
	return nil
}

// ---- M6(a): the size accounting charges the client's per-entry overhead ----------------

// TestApproximateSizeChargesPerEntryOverhead pins the exact byte formula, not a "bigger than raw
// bytes" inequality — a tolerance test would pass with any invented constant.
//
// The client charges sizeof(MutationRef)=44 per mutation and sizeof(KeyRangeRef)=24 per conflict
// range on top of the key/value bytes (client/transaction.go:2492-2504, 2510-2543), values
// verified byte-exact against libfdb_c by its TestDifferential_ApproximateSize. SimFDB summed raw
// key/value bytes only, so a transaction of many small mutations read as a fraction of its real
// size — and the record layer's "is this batch full yet?" checks run off this number.
func TestApproximateSizeChargesPerEntryOverhead(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.Set(k("kk"), []byte("vvv")) // 2 + 3 bytes, one mutation, one write conflict range [kk, kk\x00)

	// mutation: 2 + 3 + 44                 = 49
	// write conflict range [kk, kk\x00):   2 + 3 + 24 = 29   (end is keyAfter("kk"), 3 bytes)
	const want = (2 + 3 + sizeofMutationRef) + (2 + 3 + sizeofKeyRangeRef)
	got, err := tx.GetApproximateSize().Get()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("GetApproximateSize = %d, want %d (mutation %d + conflict range %d); raw "+
			"key/value bytes alone would be %d", got, want, 2+3+sizeofMutationRef,
			2+3+sizeofKeyRangeRef, 2+3)
	}
}

// TestApproximateSizeRefundsSingleKeyClear pins the ONE place the RYW counter and the commit gate
// disagree, so a future "unify the two" cleanup cannot quietly delete it.
//
// C++ models a single-key clear as a write-map RANGE entry and charges its mutation half
// sizeof(KeyRangeRef), not sizeof(MutationRef) (ReadYourWrites.actor.cpp:2431, ported at
// client/transaction.go:2536-2542). The commit-time 2101 accounting deliberately does NOT apply
// that refund (client/transaction.go:2556-2562). Two callers, one formula, one documented delta.
func TestApproximateSizeRefundsSingleKeyClear(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.Clear(k("kk"))

	// The client rewrites Clear(k) into ClearRange(k, k+\x00), so the mutation carries
	// len(k) + len(k)+1 = 5 bytes, charged sizeofKeyRangeRef by the RYW counter (the refund),
	// plus the write conflict range [kk, kk\x00) at 5 + 24.
	const wantApprox = (2 + 3 + sizeofKeyRangeRef) + (2 + 3 + sizeofKeyRangeRef)
	got, err := tx.GetApproximateSize().Get()
	if err != nil {
		t.Fatal(err)
	}
	if got != wantApprox {
		t.Fatalf("GetApproximateSize for a single-key Clear = %d, want %d (the mutation half is "+
			"charged sizeof(KeyRangeRef)=%d, not sizeof(MutationRef)=%d)",
			got, wantApprox, sizeofKeyRangeRef, sizeofMutationRef)
	}
	// The commit gate charges the full MutationRef for the same mutation — no refund.
	const wantCommit = (2 + 3 + sizeofMutationRef) + (2 + 3 + sizeofKeyRangeRef)
	if cs := commitSize(tx); cs != wantCommit {
		t.Fatalf("commitSize for a single-key Clear = %d, want %d — the 2101 gate must NOT apply "+
			"the RYW single-key-clear refund", cs, wantCommit)
	}
	// A single-key Clear costs len(k) TWICE plus the trailing zero, because the client ships it
	// as ClearRange(k, k+\x00). Charging len(k) once under-counts it by nearly half.
	if b := mutationBytes(mutation{kind: mutClear, key: []byte("kk")}); b != 5 {
		t.Fatalf("mutationBytes(Clear(\"kk\")) = %d, want 5 (= len(k) + len(k)+1)", b)
	}
}

// TestTransactionTooLargeCountsOverheadAndConflictRanges pins that the 2101 gate uses the same
// accounting. A transaction whose RAW key/value bytes sit under the 10 MB limit but whose
// per-mutation overhead pushes it over must be rejected — that is the whole failure mode of a
// byte-only counter: it certifies a commit libfdb_c refuses.
func TestTransactionTooLargeCountsOverheadAndConflictRanges(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()

	// 160k mutations of 20 raw bytes each = 3.2 MB raw — nowhere near 10 MB. With
	// sizeof(MutationRef)=44 per mutation plus a write conflict range each
	// (~21 key bytes + 24), the real request is ~14 MB.
	const n = 160_000
	val := bytes.Repeat([]byte("v"), 10)
	for i := 0; i < n; i++ {
		tx.Set(fdb.Key([]byte{byte(i), byte(i >> 8), byte(i >> 16), 'k', 'e', 'y', 'p', 'a', 'd', 'x'}), val)
	}

	var raw int64
	for _, m := range tx.buffer {
		raw += int64(len(m.key) + len(m.end) + len(m.value))
	}
	if raw > txnSizeLimit {
		t.Fatalf("test is not exercising the overhead: raw bytes %d already exceed the %d limit",
			raw, txnSizeLimit)
	}
	if cs := commitSize(tx); cs <= txnSizeLimit {
		t.Fatalf("commitSize = %d, want > %d — the per-entry overhead is not being charged",
			cs, txnSizeLimit)
	}
	if code := errCode(t, tx.Commit().Get()); code != 2101 {
		t.Fatalf("commit of a %d-raw-byte / %d-accounted-byte transaction: got %d, want "+
			"transaction_too_large(2101)", raw, commitSize(tx), code)
	}
}

// ---- M6(b): the size verdict precedes the too-old verdict ------------------------------

// TestSizeCheckPrecedesTooOld pins the ORDER with a transaction that would trigger both.
//
// The two verdicts come from different machines. 2101 is decided entirely client-side, inside
// Commit, before the request is sent (client/transaction.go:1768-1808); 1007 is the resolver's
// answer and only exists once the request reached the cluster (SkipList.cpp:837). A
// client-rejected transaction never gets a 1007.
//
// The order is not cosmetic: 1007 is retryable and 2101 is terminal, so reporting 1007 for a
// transaction that is also oversized makes a runner reset and re-send something that can never
// commit at any read version, spinning until the retry limit. SimFDB checked too-old first.
func TestSizeCheckPrecedesTooOld(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Advance the database far past the MVCC window so an ancient read version is genuinely
	// too old, and confirm that on its own it DOES report 1007 — otherwise this test could pass
	// by failing to set up the too-old condition at all.
	db.lastVersion = mvccWindow + 100

	tooOldOnly := db.newTxn()
	tooOldOnly.SetReadVersion(0)
	tooOldOnly.Get(k("x")).MustGet() // takes a read conflict range: 1007 gates on having one
	tooOldOnly.Set(k("x"), []byte("v"))
	if code := errCode(t, tooOldOnly.Commit().Get()); code != 1007 {
		t.Fatalf("setup check: an ancient read version alone must report 1007, got %d", code)
	}

	// Same ancient read version, but now also oversized. The size verdict must win.
	both := db.newTxn()
	both.SetReadVersion(0)
	both.Get(k("x")).MustGet()
	huge := bytes.Repeat([]byte("v"), valueSizeLimit)
	for i := 0; i < 120; i++ {
		both.Set(fdb.Key([]byte{byte(i), byte(i >> 8), 'b', 'i', 'g'}), huge)
	}
	if cs := commitSize(both); cs <= txnSizeLimit {
		t.Fatalf("setup check: transaction is not oversized (commitSize %d <= %d)", cs, txnSizeLimit)
	}
	code := errCode(t, both.Commit().Get())
	if code == 1007 {
		t.Fatal("too-old(1007, RETRYABLE) out-ranked transaction_too_large(2101, TERMINAL): a " +
			"runner would reset and re-send a transaction that can never commit")
	}
	if code != 2101 {
		t.Fatalf("oversized + too-old commit: got %d, want transaction_too_large(2101)", code)
	}
}

// ---- M6(c): a row limit below -1 is range_limits_invalid on the GetSlice path ----------

// TestNegativeRowLimitIsRangeLimitsInvalid pins the SLICE path specifically. The iterator path
// already rejected a limit < -1 (range_result.go Iterator), but GetSliceWithError never runs that
// check, so a bad limit was silently treated as unlimited and returned rows. The client rejects at
// the CALL, covering both consumers (client/transaction.go:1266-1268 in getRangeDir).
func TestNegativeRowLimitIsRangeLimitsInvalid(t *testing.T) {
	t.Parallel()
	db := New(nil)
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("a"), []byte("1"))
		tx.Set(k("b"), []byte("2"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tx := db.newTxn()

	t.Run("GetSlice", func(t *testing.T) {
		kvs, err := tx.GetRange(allRange(), fdb.RangeOptions{Limit: -2}).GetSliceWithError()
		if err == nil {
			t.Fatalf("GetSliceWithError with Limit=-2 returned %d rows and no error; want "+
				"range_limits_invalid(2012)", len(kvs))
		}
		if code := errCode(t, err); code != 2012 {
			t.Fatalf("GetSliceWithError with Limit=-2: got %d, want range_limits_invalid(2012)", code)
		}
	})
	t.Run("Iterator", func(t *testing.T) {
		it := tx.GetRange(allRange(), fdb.RangeOptions{Limit: -2}).Iterator()
		if it.Advance() {
			t.Fatal("iterator with Limit=-2 advanced")
		}
		if _, err := it.Get(); errCode(t, err) != 2012 {
			t.Fatalf("iterator with Limit=-2: got %d, want 2012", errCode(t, err))
		}
	})
	t.Run("valid limits still work", func(t *testing.T) {
		// -1 (ROW_LIMIT_UNLIMITED) and 0 are VALID — over-rejecting is the opposite failure.
		for _, lim := range []int{-1, 0, 1} {
			kvs, err := tx.GetRange(allRange(), fdb.RangeOptions{Limit: lim}).GetSliceWithError()
			if err != nil {
				t.Fatalf("Limit=%d must be valid, got %v", lim, err)
			}
			if lim == 1 && len(kvs) != 1 {
				t.Fatalf("Limit=1 returned %d rows", len(kvs))
			}
			if lim != 1 && len(kvs) != 2 {
				t.Fatalf("Limit=%d returned %d rows, want 2 (unlimited)", lim, len(kvs))
			}
		}
	})
}

// ---- M4: transaction_too_old is reachable through the simulated clock ------------------

// simClockDB returns a SimDB whose versions advance with a driver-controlled logical clock, plus
// the clock. Only the Clock seam is installed — no Buggifier — so no fault fires and the only
// thing under test is version-vs-time. (dst.Env's accessors are nil-safe field by field.)
func simClockDB() (*SimDB, *dst.SimClock) {
	clk := dst.NewSimClock(dst.Epoch)
	return New(&dst.Env{Clock: clk}), clk
}

// TestTooOldFromSimulatedClock is the M4 pin: a transaction that reads, is held open past the
// MVCC window of SIMULATED TIME, and then commits gets transaction_too_old(1007) — with no test
// writing db.lastVersion, or any other internal, by hand.
//
// This is the shape real code actually meets 1007 in: a long-running scan or an indexer that
// keeps one transaction open across the 5-second window. Before the clock binding, versions came
// only from the commit counter, so the window was really "5,000,000 COMMITS ago" and no amount of
// elapsed time could age anything out. 1007 was reachable only by a test assigning
// db.lastVersion directly — which exercises the comparison but proves nothing about whether the
// modelled system can ever produce the input.
//
// The arithmetic: VERSIONS_PER_SECOND = 1e6, so one version per microsecond, and
// MAX_WRITE_TRANSACTION_LIFE_VERSIONS = 5 * VERSIONS_PER_SECOND is exactly 5 seconds wide.
func TestTooOldFromSimulatedClock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		advance time.Duration
		want    int // 0 = commits
	}{
		{"inside the window commits", 4 * time.Second, 0},
		{"just inside the window commits", mvccWindow * time.Microsecond, 0},
		{"past the window is too old", 6 * time.Second, 1007},
		{"far past the window is too old", time.Hour, 1007},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, clk := simClockDB()
			if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(k("x"), []byte("v"))
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}

			tx := db.newTxn()
			tx.Get(k("x")).MustGet() // pins the read version AND takes a read conflict range
			clk.Advance(tc.advance)  // the transaction is simply held open
			tx.Set(k("x"), []byte("w"))

			err := tx.Commit().Get()
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("after %v (window is %v) the commit must succeed, got %v",
						tc.advance, mvccWindow*time.Microsecond, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a transaction held open %v — past the %v MVCC window — committed; "+
					"1007 is unreachable through the passage of time",
					tc.advance, mvccWindow*time.Microsecond)
			}
			if code := errCode(t, err); code != tc.want {
				t.Fatalf("after %v: got %d, want transaction_too_old(%d)", tc.advance, code, tc.want)
			}
		})
	}
}

// TestClockMintedVersionsFollowVersionsPerSecond pins the CONVERSION, not just the threshold. A
// binding that aged transactions out at the wrong rate would still pass the test above (any
// monotone function of time crosses the window eventually) while making every duration-sensitive
// verdict wrong.
func TestClockMintedVersionsFollowVersionsPerSecond(t *testing.T) {
	t.Parallel()

	// LITERALS, deliberately — not `versionsPerSecond` and `mvccWindow`. Asserting a constant
	// against itself proves nothing: a sim that ran at 1e3 versions/second with a
	// proportionally-shrunk window would satisfy every relative check while diverging from FDB
	// in absolute version numbers. Those numbers are not private bookkeeping — a commit version
	// IS the versionstamp written into keys and values (conflict.go versionstampBytes), so the
	// rate is observable in persisted bytes and a DST run comparing them against a real cluster
	// would disagree.
	if versionsPerSecond != 1_000_000 {
		t.Fatalf("versionsPerSecond = %d, want 1000000 (FDB SERVER_KNOBS->VERSIONS_PER_SECOND)",
			versionsPerSecond)
	}
	if mvccWindow != 5_000_000 {
		t.Fatalf("mvccWindow = %d, want 5000000 (MAX_WRITE_TRANSACTION_LIFE_VERSIONS = "+
			"5 * VERSIONS_PER_SECOND)", mvccWindow)
	}

	db, clk := simClockDB()

	if v := db.clockVersion(); v != 0 {
		t.Fatalf("a clock at dst.Epoch must be version 0, got %d", v)
	}
	clk.Advance(time.Second)
	if v := db.clockVersion(); v != 1_000_000 {
		t.Fatalf("one second = 1000000 versions (VERSIONS_PER_SECOND), got %d", v)
	}
	clk.Advance(500 * time.Millisecond)
	if v := db.clockVersion(); v != 1_500_000 {
		t.Fatalf("1.5s = 1500000 versions, got %d", v)
	}
	clk.Advance(time.Microsecond)
	if v := db.clockVersion(); v != 1_500_001 {
		t.Fatalf("one microsecond must be exactly one version: 1.500001s = 1500001, got %d", v)
	}

	// A commit at that instant takes the clock's version, and the read version a later
	// transaction pins comes from the same place — otherwise a transaction could be judged
	// too-old against a version no GRV would ever have handed it.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("a"), []byte("1"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	rv, err := db.newTxn().GetReadVersion().Get()
	if err != nil {
		t.Fatal(err)
	}
	if rv < 1_500_001 {
		t.Fatalf("read version %d is below the clock's version %d: the GRV is not clock-bound",
			rv, 1_500_001)
	}

	// Versions stay STRICTLY increasing when the clock does not move: successive commits at one
	// instant must still step, or the store's "commitVersion exceeds any existing version for
	// the key" precondition breaks.
	before := rv
	for i := 0; i < 3; i++ {
		if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			tx.Set(k("a"), []byte("2"))
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		after, err := db.newTxn().GetReadVersion().Get()
		if err != nil {
			t.Fatal(err)
		}
		if after <= before {
			t.Fatalf("commit %d did not advance the version: %d -> %d", i, before, after)
		}
		before = after
	}
}

// TestNilEnvKeepsPurelyLogicalVersions pins the ASYMMETRY, and it is the load-bearing half of the
// clock binding.
//
// A nil *Env is this repo's only spelling of production (pkg/dst/env.go), and a SimDB without an
// installed Clock must keep minting versions from the pure logical counter. Binding it to
// time.Now() instead would make version assignment — and therefore whether a transaction gets
// 1007 — depend on how long the host took to run the code, which is exactly the nondeterminism
// SimFDB exists to remove. Every differential arm builds its sim with New(nil), so this is also
// what makes the clock work provably inert for them.
func TestNilEnvKeepsPurelyLogicalVersions(t *testing.T) {
	t.Parallel()
	db := New(nil)
	if v := db.clockVersion(); v != 0 {
		t.Fatalf("New(nil).clockVersion() = %d, want 0 — production must not read a wall clock", v)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			tx.Set(k("a"), []byte("1"))
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		// The counter, and nothing else: commit i produces version i+1.
		if db.lastVersion != int64(i+1) {
			t.Fatalf("after %d commits lastVersion = %d, want %d (pure logical counter)",
				i+1, db.lastVersion, i+1)
		}
		if lv := db.latestVersion(); lv != db.lastVersion {
			t.Fatalf("latestVersion() = %d but lastVersion = %d: a clock leaked into a nil-Env db",
				lv, db.lastVersion)
		}
	}
	// And an Env with no Clock installed is production too — Env.Now() would fall back to the
	// wall clock, so the guard must test the Clock field, not just the Env pointer.
	noClock := New(&dst.Env{})
	if v := noClock.clockVersion(); v != 0 {
		t.Fatalf("New(&dst.Env{}).clockVersion() = %d, want 0 — an Env without a Clock must not "+
			"fall through to the wall clock", v)
	}
}

// ---- M5: a pending versionstamp makes its key unreadable (1036) ------------------------

// vsOperand is a versionstamped-value operand: a 3-byte prefix, the 10-byte 0xFF placeholder,
// and the 4-byte little-endian offset of the placeholder (apiVersion >= 520).
func vsOperand() []byte {
	v := append([]byte("pre"), bytes.Repeat([]byte{0xFF}, 10)...)
	off := make([]byte, 4)
	off[0] = 3
	return append(v, off...)
}

// TestVersionstampedWriteMakesKeyUnreadable pins the 1036 gate on every read shape.
//
// The stamp is assigned by the cluster at commit, so between the write and the commit there is
// no value a client could correctly report — which is exactly why FDB defines
// accessed_unreadable(1036) instead of returning something. SimFDB returned the UNSTAMPED
// OPERAND as if it were a real value: a phantom present key, holding bytes that never existed on
// any cluster, which the same transaction would then find changed after commit. A model-based
// oracle reading through SimFDB would have certified that phantom as the truth.
//
// Ported from client/ryw.go:511-517 (Get), :653-688 (the GetRange reach cap) and the RYWIterator
// throw the key-selector walk inherits.
func TestVersionstampedWriteMakesKeyUnreadable(t *testing.T) {
	t.Parallel()

	for _, w := range []struct {
		name  string
		write func(tx *simTxn)
	}{
		{"SetVersionstampedValue", func(tx *simTxn) { tx.SetVersionstampedValue(k("m"), vsOperand()) }},
		{"SetVersionstampedKey", func(tx *simTxn) { tx.SetVersionstampedKey(k("m"), vsOperand()) }},
	} {
		w := w
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(k("a"), []byte("1"))
				tx.Set(k("m"), []byte("old"))
				tx.Set(k("z"), []byte("2"))
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}

			t.Run("Get", func(t *testing.T) {
				tx := db.newTxn()
				w.write(tx)
				v, err := tx.Get(k("m")).Get()
				if err == nil {
					t.Fatalf("Get of a versionstamp-written key returned %q; the stamp is not "+
						"assigned until commit, so no value is knowable — want "+
						"accessed_unreadable(1036)", v)
				}
				if code := errCode(t, err); code != 1036 {
					t.Fatalf("Get: got %d, want accessed_unreadable(1036)", code)
				}
			})

			t.Run("GetRange over it", func(t *testing.T) {
				tx := db.newTxn()
				w.write(tx)
				kvs, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
				if err == nil {
					t.Fatalf("GetRange spanning a versionstamp-written key returned %v; "+
						"want accessed_unreadable(1036)", kvs)
				}
				if code := errCode(t, err); code != 1036 {
					t.Fatalf("GetRange: got %d, want 1036", code)
				}
			})

			t.Run("GetRange reverse over it", func(t *testing.T) {
				tx := db.newTxn()
				w.write(tx)
				_, err := tx.GetRange(allRange(), fdb.RangeOptions{Reverse: true}).GetSliceWithError()
				if code := errCode(t, err); code != 1036 {
					t.Fatalf("reverse GetRange: got %d, want 1036 — the reverse cap must exclude "+
						"the unreadable key, not include it", code)
				}
			})

			t.Run("GetKey walking over it", func(t *testing.T) {
				tx := db.newTxn()
				w.write(tx)
				_, err := tx.GetKey(fdb.FirstGreaterOrEqual(k("m"))).Get()
				if code := errCode(t, err); code != 1036 {
					t.Fatalf("GetKey: got %d, want 1036", code)
				}
			})

			t.Run("a range that stops short of it still reads", func(t *testing.T) {
				// The gate is a REACH cap, not a transaction-wide poison: a scan whose window
				// ends before the unreadable position is unaffected.
				tx := db.newTxn()
				w.write(tx)
				kvs, err := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("m")}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					t.Fatalf("a range ending before the unreadable key must still read, got %v", err)
				}
				if len(kvs) != 1 || string(kvs[0].Key) != "a" {
					t.Fatalf("got %v, want just key a", kvs)
				}
			})

			t.Run("a limit filled before the cap still reads", func(t *testing.T) {
				// The client's "reached" predicate: iteration only throws if the row limit was
				// NOT filled inside the capped window (client/ryw.go:677-681). Here the limit of
				// 1 is filled by key "a", so the scan never had to look at "m".
				tx := db.newTxn()
				w.write(tx)
				kvs, err := tx.GetRange(allRange(), fdb.RangeOptions{Limit: 1}).GetSliceWithError()
				if err != nil {
					t.Fatalf("a limit filled before the cap must not throw, got %v", err)
				}
				if len(kvs) != 1 || string(kvs[0].Key) != "a" {
					t.Fatalf("got %v, want [a]", kvs)
				}
			})
		})
	}
}

// TestUnreadableIsStickyAcrossLaterSets pins the STICKINESS, which is the half a naive
// implementation gets wrong: it is tempting to treat a later plain Set as "now I know the value".
// C++ does not — the stack-replacing SetValue fast path is gated on !is_unreadable
// (WriteMap.cpp:125), so on an unreadable entry the Set is PUSHED and the flag survives
// (:141), ported at client/ryw.go:225-229. The commit still applies the versionstamped op, so
// the committed bytes are still not the ones a reader would have been shown.
func TestUnreadableIsStickyAcrossLaterSets(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.SetVersionstampedValue(k("m"), vsOperand())
	tx.Set(k("m"), []byte("plain")) // does NOT make it readable
	if _, err := tx.Get(k("m")).Get(); errCode(t, err) != 1036 {
		t.Fatalf("a plain Set after a versionstamped write cleared the unreadable flag: got %d, "+
			"want 1036", errCode(t, err))
	}
	// Several plain Sets, and an atomic, likewise.
	tx.Set(k("m"), []byte("plain2"))
	tx.Add(k("m"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
	if _, err := tx.Get(k("m")).Get(); errCode(t, err) != 1036 {
		t.Fatal("the unreadable flag did not survive a Set/atomic chain")
	}
}

// TestClearMakesAnUnreadableKeyReadable pins the ONE operation that lifts the flag. A cleared key
// is readable because the transaction knows it is empty — there is no pending stamp left to be
// ignorant of (client/ryw.go:243-244 for Clear, :270-271/:280 for ClearRange).
func TestClearMakesAnUnreadableKeyReadable(t *testing.T) {
	t.Parallel()

	t.Run("Clear", func(t *testing.T) {
		t.Parallel()
		db := New(nil)
		tx := db.newTxn()
		tx.SetVersionstampedValue(k("m"), vsOperand())
		tx.Clear(k("m"))
		v, err := tx.Get(k("m")).Get()
		if err != nil {
			t.Fatalf("Get after Clear must succeed, got %v", err)
		}
		if v != nil {
			t.Fatalf("Get after Clear = %q, want nil (absent)", v)
		}
	})

	t.Run("ClearRange", func(t *testing.T) {
		t.Parallel()
		db := New(nil)
		tx := db.newTxn()
		tx.SetVersionstampedValue(k("m"), vsOperand())
		tx.ClearRange(fdb.KeyRange{Begin: k("a"), End: k("z")})
		if _, err := tx.Get(k("m")).Get(); err != nil {
			t.Fatalf("Get after ClearRange must succeed, got %v", err)
		}
		// And a full scan is readable again — the cap is gone with the flag.
		if _, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError(); err != nil {
			t.Fatalf("GetRange after ClearRange must succeed, got %v", err)
		}
	})

	t.Run("a ClearRange that misses the key does NOT lift it", func(t *testing.T) {
		t.Parallel()
		db := New(nil)
		tx := db.newTxn()
		tx.SetVersionstampedValue(k("m"), vsOperand())
		tx.ClearRange(fdb.KeyRange{Begin: k("a"), End: k("b")}) // does not cover "m"
		if _, err := tx.Get(k("m")).Get(); errCode(t, err) != 1036 {
			t.Fatal("a ClearRange not covering the key lifted its unreadable flag")
		}
	})
}

// TestBypassUnreadableReturnsThePlaceholder pins that SetBypassUnreadable is REAL and not
// accepted-and-ignored, and pins what it returns: the operand's bytes exactly as written,
// placeholder unfilled (client/ryw.go:55-60, :527-545). It is the caller asserting it knows the
// bytes it gets back are not the bytes that will be committed — which the second half checks.
func TestBypassUnreadableReturnsThePlaceholder(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	if err := tx.Options().SetBypassUnreadable(); err != nil {
		t.Fatal(err)
	}
	operand := vsOperand()
	tx.SetVersionstampedValue(k("m"), operand)

	v, err := tx.Get(k("m")).Get()
	if err != nil {
		t.Fatalf("with BYPASS_UNREADABLE the read must succeed, got %v — the option is being "+
			"accepted and ignored", err)
	}
	if !bytes.Equal(v, operand) {
		t.Fatalf("bypassed read = %x, want the operand AS WRITTEN %x", v, operand)
	}
	// The range and selector paths bypass too.
	if _, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError(); err != nil {
		t.Fatalf("bypassed GetRange must succeed, got %v", err)
	}
	if _, err := tx.GetKey(fdb.FirstGreaterOrEqual(k("m"))).Get(); err != nil {
		t.Fatalf("bypassed GetKey must succeed, got %v", err)
	}

	// What the bypass hands back is NOT what commits: after the commit the placeholder is
	// replaced by the real stamp. This is why the read is an error by default.
	if err := tx.Commit().Get(); err != nil {
		t.Fatal(err)
	}
	committed, err := db.newTxn().Get(k("m")).Get()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(committed, operand) {
		t.Fatal("the committed value equals the placeholder operand — the stamp was not applied, " +
			"so this test is not demonstrating the divergence it claims")
	}
}

// TestUnreadableDoesNotSurviveTheCommit pins that a reused handle is readable again: the stamps
// are resolved, so there is nothing left that is unknowable. Leaving the flags on would poison
// the handle for the rest of its life, and the record layer reuses handles across commits.
func TestUnreadableDoesNotSurviveTheCommit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.SetVersionstampedValue(k("m"), vsOperand())
	if _, err := tx.Get(k("m")).Get(); errCode(t, err) != 1036 {
		t.Fatal("setup: the key should be unreadable before the commit")
	}
	if err := tx.Commit().Get(); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Get(k("m")).Get(); err != nil {
		t.Fatalf("the same handle must read the key after the commit, got %v", err)
	}
	// Reset likewise.
	tx2 := db.newTxn()
	tx2.SetVersionstampedValue(k("n"), vsOperand())
	tx2.Reset()
	if _, err := tx2.Get(k("n")).Get(); err != nil {
		t.Fatalf("after Reset the key must be readable, got %v", err)
	}
}
