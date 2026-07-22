package recordlayer

import "testing"

// TestIndex_CreatesDuplicates pins RFC-188 finding 10 M4's data source: the
// index's fan-out signal must be derived from its ROOT KEY EXPRESSION, not
// hardcoded. An index over a repeated field (FanOut) creates duplicates — one
// record yields multiple entries — so it does NOT produce distinct records; a
// scalar-field index does not. This is the signal DistinctRecordsProperty
// consumes; before M4's fix the candidate's CreatesDuplicates() was a constant
// false, which over-reported fan-out indexes as distinct (dropped dedup).
func TestIndex_CreatesDuplicates(t *testing.T) {
	t.Parallel()

	scalar := &Index{Name: "idx_name", RootExpression: Field("name")}
	if scalar.CreatesDuplicates() {
		t.Fatal("scalar-field index must NOT create duplicates")
	}

	fanOut := &Index{Name: "idx_tags", RootExpression: FanOut("tags")}
	if !fanOut.CreatesDuplicates() {
		t.Fatal("fan-out (repeated-field) index MUST create duplicates → not distinct")
	}

	// A nested fan-out is also duplicating.
	nestedFanOut := &Index{Name: "idx_nested", RootExpression: Nest("parent", FanOut("child"))}
	if !nestedFanOut.CreatesDuplicates() {
		t.Fatal("nested fan-out index MUST create duplicates")
	}

	// Unset root expression → false (safe default, no panic).
	if (&Index{Name: "empty"}).CreatesDuplicates() {
		t.Fatal("index with no root expression must not report duplicates")
	}
}
