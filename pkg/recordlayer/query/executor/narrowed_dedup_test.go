package executor

import (
	"context"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// drainNarrowed runs a hash distinct over rows with the given narrowing and
// returns the emitted rows' first slot plus the number of keys the operator
// RETAINED. The retained count is the load-bearing half: R3's claim is not that
// it emits the same rows (it must) but that it holds a strict SUBSET of the
// keys, and a test that only checked the rows would pass with the narrowing
// entirely inert.
func drainNarrowed(
	t *testing.T, rows []QueryResult, narrowed *narrowedDedup,
) (emitted []any, retained int) {
	t.Helper()
	cursor := &distinctHashCursor{
		inner:    recordlayer.FromList(rows),
		seen:     newBoundedSet[string](nil),
		keyer:    distinctKey,
		narrowed: narrowed,
	}
	for {
		result, err := cursor.OnNext(context.Background())
		if err != nil {
			t.Fatalf("cursor: %v", err)
		}
		if !result.HasNext() {
			break
		}
		emitted = append(emitted, result.GetValue().Positional.Slots[0])
	}
	return emitted, len(cursor.order)
}

// TestNarrowedDedup_RetainsOnlyTheExemptSubset is R3's central pin, and it
// asserts BOTH halves of "strictly dominates":
//
//   - the emitted rows are identical to the full operator's (correctness), and
//   - the retained key set is a strict subset (the whole benefit).
//
// The fixture is a stream from a UNIQUE index on a nullable column: the
// uniqueness declaration guarantees at most one row per non-NULL value, and says
// nothing whatever about the NULL rows, which it holds as many entries as exist.
// So the duplicates are exactly the NULLs — the exempt set — and they are what
// the operator must still catch.
func TestNarrowedDedup_RetainsOnlyTheExemptSubset(t *testing.T) {
	t.Parallel()

	rows := []QueryResult{
		qr("e", "a"),
		qr("e", nil),
		qr("e", "b"),
		qr("e", nil), // duplicate of the NULL — only the operator catches this
		qr("e", "c"),
	}

	fullEmitted, fullRetained := drainNarrowed(t, rows, nil)
	narrowEmitted, narrowRetained := drainNarrowed(t, rows, &narrowedDedup{exemptSlots: []int{0}})

	if len(fullEmitted) != 4 {
		t.Fatalf("the FULL operator emitted %v, want 4 rows — the fixture is wrong "+
			"and everything below it is measuring nothing", fullEmitted)
	}
	if len(narrowEmitted) != len(fullEmitted) {
		t.Fatalf("narrowed emitted %v, full emitted %v. R3 must return the SAME rows: "+
			"a non-exempt row is provably unique already, and the exempt ones still "+
			"go through the seen-set", narrowEmitted, fullEmitted)
	}
	for i := range fullEmitted {
		if narrowEmitted[i] != fullEmitted[i] {
			t.Fatalf("row %d: narrowed emitted %v, full emitted %v", i, narrowEmitted[i], fullEmitted[i])
		}
	}

	// The benefit. Full retains every distinct key; narrowed retains only the
	// NULL. Without this assertion the test passes with the narrowing inert.
	if fullRetained != 4 {
		t.Fatalf("full operator retained %d keys, want 4", fullRetained)
	}
	if narrowRetained != 1 {
		t.Fatalf("narrowed operator retained %d keys, want 1 (the single exempt NULL). "+
			"R3's entire value is that non-exempt rows enter neither the seen-set "+
			"nor the continuation", narrowRetained)
	}
}

// TestNarrowedDedup_RetainsNothingOnAnOrdinaryTable is the 0%-density case from
// the RFC's NULL-density sweep, and it is the one the performance criteria rest
// on. A nullable column that happens to hold no NULLs is the ordinary state of
// an ordinary table; there, the narrowed operator degenerates to a pass-through
// holding nothing at all.
func TestNarrowedDedup_RetainsNothingOnAnOrdinaryTable(t *testing.T) {
	t.Parallel()

	rows := []QueryResult{qr("e", "a"), qr("e", "b"), qr("e", "c")}
	emitted, retained := drainNarrowed(t, rows, &narrowedDedup{exemptSlots: []int{0}})

	if len(emitted) != 3 {
		t.Fatalf("emitted %v, want all 3 rows", emitted)
	}
	if retained != 0 {
		t.Fatalf("the narrowed operator retained %d keys over a stream with NO exempt "+
			"rows, want 0. This is the case the sweep's 0%% row measures and the "+
			"case an ordinary table is always in", retained)
	}
}

// TestNarrowedDedup_NaNIsExemptAndSignedZeroIsNot pins the FLOAT half of the
// exempt set, in both directions, because the two look alike and are opposites.
//
// NaN is exempt: FDB preserves distinct raw NaN sign and payload encodings, so
// the index holds two entries the dedup key canonicalizes to ONE value — the
// operator is what collapses them.
//
// Signed zero is NOT exempt: packedDedupKey packs -0.0 and +0.0 verbatim and the
// index holds them as two keys, so both agree the pair is two rows. Treating it
// as exempt would be harmless for correctness and would silently widen the
// exempt set on every float column, which is why it is pinned rather than left
// to the reader.
func TestNarrowedDedup_NaNIsExemptAndSignedZeroIsNot(t *testing.T) {
	t.Parallel()

	nan := &narrowedDedup{exemptSlots: []int{0}}
	if !nan.isExempt(qr("d", math.NaN())) {
		t.Fatal("a NaN key component is not exempt. Raw NaN encodings are distinct " +
			"tuple keys the index holds separately while the dedup key canonicalizes " +
			"them to one value, so the operator is the only thing that collapses them")
	}
	if !nan.isExempt(qr("d", float32(math.NaN()))) {
		t.Fatal("a float32 NaN key component is not exempt")
	}
	if nan.isExempt(qr("d", math.Copysign(0, -1))) {
		t.Fatal("negative zero was treated as exempt. It is not: packedDedupKey packs " +
			"both zero signs verbatim and the index holds them as two keys, so the " +
			"operator and the index agree and there is nothing to reconcile")
	}
	if nan.isExempt(qr("d", 1.5)) {
		t.Fatal("an ordinary float was treated as exempt")
	}
}

// TestNarrowedDedup_ExemptSlotsAimAtTheKeyColumns pins that the test is aimed at
// the PROVING INDEX's key columns and not at the whole row. A projection may
// carry columns the index does not key, and a NULL in one of those says nothing
// about uniqueness — retaining such a row costs the benefit on every table with
// a nullable non-key column.
//
// The nil case is pinned in the same test because it is the fail-safe: when a
// position cannot be stated, EVERY slot is tested, which over-approximates the
// exempt set and therefore cannot drop a row.
func TestNarrowedDedup_ExemptSlotsAimAtTheKeyColumns(t *testing.T) {
	t.Parallel()

	// Slot 0 is the indexed key; slot 1 is an unindexed nullable passenger.
	row := qr("e", "a", "note", nil)

	keyOnly := &narrowedDedup{exemptSlots: []int{0}}
	if keyOnly.isExempt(row) {
		t.Fatal("a NULL in a NON-KEY projected column made the row exempt. The " +
			"index's uniqueness is over its KEY columns, so a passenger column's " +
			"NULL says nothing — and treating it as exempt retains rows R3 exists " +
			"to let through")
	}

	everySlot := &narrowedDedup{}
	if !everySlot.isExempt(row) {
		t.Fatal("with no statable key positions the exempt test must fall back to " +
			"testing EVERY slot. Under-approximating the exempt set drops rows; " +
			"over-approximating only costs performance, so nil has exactly one safe " +
			"meaning and this is it")
	}

	// A position the row cannot supply is treated as exempt for the same reason.
	outOfRange := &narrowedDedup{exemptSlots: []int{7}}
	if !outOfRange.isExempt(qr("e", "a")) {
		t.Fatal("a key position the row does not supply must fall back to exempt, " +
			"which is the full behaviour and cannot drop a row")
	}
}
