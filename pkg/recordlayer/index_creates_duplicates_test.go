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

	// A grouping/aggregate index delegates to its whole key (Java
	// GroupingKeyExpression.createsDuplicates → getWholeKey().createsDuplicates).
	// A grouping key over a fan-out field fans out; over scalars it does not.
	groupFanOut := &Index{Name: "idx_grp_fan", RootExpression: GroupBy(Field("total"), FanOut("tags"))}
	if !groupFanOut.CreatesDuplicates() {
		t.Fatal("grouping index whose grouping key fans out MUST create duplicates (whole-key delegation)")
	}
	groupScalar := &Index{Name: "idx_grp_scalar", RootExpression: GroupBy(Field("total"), Field("region"))}
	if groupScalar.CreatesDuplicates() {
		t.Fatal("grouping index over scalar keys must NOT create duplicates")
	}

	// Unset root expression → false (safe default, no panic).
	if (&Index{Name: "empty"}).CreatesDuplicates() {
		t.Fatal("index with no root expression must not report duplicates")
	}

	// Empty / Literal roots do NOT fan out — explicit false arms.
	if (&Index{Name: "idx_empty", RootExpression: &EmptyKeyExpression{}}).CreatesDuplicates() {
		t.Fatal("EmptyKeyExpression must not create duplicates")
	}
	if (&Index{Name: "idx_lit", RootExpression: &LiteralKeyExpression{}}).CreatesDuplicates() {
		t.Fatal("LiteralKeyExpression must not create duplicates")
	}

	// An UNRECOGNIZED (custom/external) key expression FAILS CLOSED at the INDEX
	// level: Index.CreatesDuplicates()==true (conservatively duplicating) so the
	// M4 DistinctRecords signal never mis-reports an unknown-fan-out index as
	// distinct. The SHARED helper createsDuplicates() still returns false for it
	// (validateSplitKeyExpression reads that as positive proof — a fail-closed
	// true there would wrongly admit Split(customScalar)). customKeyExpression
	// (bug_bounty_test.go) stands in for such a type.
	if !(&Index{Name: "idx_custom", RootExpression: &customKeyExpression{}}).CreatesDuplicates() {
		t.Fatal("unrecognized index root must FAIL CLOSED to CreatesDuplicates()=true (M4 safety)")
	}
	if createsDuplicates(&customKeyExpression{}) {
		t.Fatal("shared createsDuplicates() must keep default false for Split validation")
	}
}
