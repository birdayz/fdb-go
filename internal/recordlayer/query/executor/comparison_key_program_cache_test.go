package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// comparisonKeyPrograms hands the same program to every row of a leg. That is a
// CACHE in a merge's hottest path, so the two things that would make it wrong are
// pinned here: it must not hand one leg's program to a differently shaped leg, and
// it must not grow without bound if a producer mints a fresh row type per row.
//
// Both failures are silent. A program taken from the wrong leg reads the sibling's
// slot layout, which yields a plausible comparison key rather than an error, and a
// merge ordered on a wrong key drops or duplicates rows.

func compKeyRow(t *testing.T, names ...string) *PositionalRow {
	t.Helper()
	fields := make([]values.Field, len(names))
	for i, name := range names {
		fields[i] = values.Field{Name: name, FieldType: values.NotNullLong, Ordinal: i}
	}
	rowType := values.NewRecordType("leg", false, fields)
	layout, err := values.NewOrdinalLayoutForCarrierType(rowType,
		[]values.OrdinalTileSpec{{Start: 0, Width: len(fields), Kind: values.OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("layout over %v: %v", names, err)
	}
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatalf("row over %v: %v", names, err)
	}
	return row
}

func TestComparisonKeyProgramsAreKeyedToTheirLeg(t *testing.T) {
	t.Parallel()

	// Two legs whose row types and layouts are independently constructed objects
	// of the same shape — what a merge over co-grouped aggregate indexes presents,
	// and the case the pointer key has to keep apart because each leg's program is
	// re-anchored onto ITS OWN layout carrier.
	legA := compKeyRow(t, "CUSTOMER_ID", "SUM(AMOUNT)")
	legB := compKeyRow(t, "CUSTOMER_ID", "SUM(AMOUNT)")
	if legA.Layout == legB.Layout || legA.Type == legB.Type {
		t.Fatal("the two legs share an object, so this test cannot see the key at work")
	}

	keyVals := []values.Value{legA.Layout.Carrier()}
	var programs comparisonKeyPrograms

	keysA, _, err := programs.forRow(keyVals, legA)
	if err != nil {
		t.Fatalf("leg A program: %v", err)
	}
	keysB, _, err := programs.forRow(keyVals, legB)
	if err != nil {
		t.Fatalf("leg B program: %v", err)
	}
	if len(programs.entries) != 2 {
		t.Fatalf("two independently built legs produced %d cached programs, want 2 — "+
			"one leg is being served the other's program", len(programs.entries))
	}
	// Each leg's program must be anchored on ITS OWN carrier: serving leg B the
	// program built for leg A's carrier is the silent failure this key prevents,
	// and it would read leg B's row through leg A's layout handle.
	if len(keysA) != 1 || len(keysB) != 1 {
		t.Fatalf("expected one key per leg, got %d and %d", len(keysA), len(keysB))
	}
	if keysA[0] == keysB[0] {
		t.Error("both legs were handed the same key program value")
	}

	// Asked again, each leg must get ITS OWN program back, by identity: that is
	// what makes the cache a cache rather than a rebuild.
	againA, _, err := programs.forRow(keyVals, legA)
	if err != nil {
		t.Fatalf("leg A again: %v", err)
	}
	againB, _, err := programs.forRow(keyVals, legB)
	if err != nil {
		t.Fatalf("leg B again: %v", err)
	}
	if len(againA) != len(keysA) || (len(keysA) > 0 && againA[0] != keysA[0]) {
		t.Error("leg A was rebuilt instead of served from the cache")
	}
	if len(againB) != len(keysB) || (len(keysB) > 0 && againB[0] != keysB[0]) {
		t.Error("leg B was rebuilt instead of served from the cache")
	}
	if len(programs.entries) != 2 {
		t.Errorf("re-asking grew the cache to %d entries", len(programs.entries))
	}
}

func TestComparisonKeyProgramsStopGrowingAtTheCap(t *testing.T) {
	t.Parallel()

	base := compKeyRow(t, "CUSTOMER_ID", "SUM(AMOUNT)")
	keyVals := []values.Value{base.Layout.Carrier()}
	var programs comparisonKeyPrograms

	// A producer that mints a fresh row type per row would otherwise add an entry
	// per row for the lifetime of the cursor. Past the cap the cache must simply
	// stop recording and let every row rebuild — the behaviour it replaced.
	for i := 0; i < comparisonKeyProgramCap*3; i++ {
		row := compKeyRow(t, "CUSTOMER_ID", "SUM(AMOUNT)")
		if _, _, err := programs.forRow(keyVals, row); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	if got := len(programs.entries); got != comparisonKeyProgramCap {
		t.Errorf("cache holds %d entries after %d distinct row shapes, want the cap %d",
			got, comparisonKeyProgramCap*3, comparisonKeyProgramCap)
	}

	// A nil row is still answered, and is not recorded.
	before := len(programs.entries)
	if _, _, err := programs.forRow(keyVals, nil); err != nil {
		t.Fatalf("nil row: %v", err)
	}
	if len(programs.entries) != before {
		t.Error("a nil row was recorded in the cache")
	}
}
