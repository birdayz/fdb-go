package recordlayer

import "testing"

// TestIndex_CreatesDuplicates pins RFC-188 finding 10 M4's data source: an
// index scan's duplicate-production for the DistinctRecords signal. For a plain
// VALUE index it is derived from the ROOT KEY EXPRESSION's fan-out (a repeated
// FanOut field yields multiple entries per record → creates duplicates; a scalar
// field does not). For ANY OTHER index type reaching a value candidate (TEXT
// tokenizes to one entry per token, etc.) it fails closed to duplicate-producing
// — over-reporting distinct there would let DistinctRecordsProperty drop a
// required DISTINCT. Before M4 the candidate's CreatesDuplicates() was a constant
// false, over-reporting every index as distinct.
func TestIndex_CreatesDuplicates(t *testing.T) {
	t.Parallel()

	// --- VALUE index: governed by the root key expression's fan-out ---

	scalar := &Index{Name: "idx_name", Type: IndexTypeValue, RootExpression: Field("name")}
	if scalar.CreatesDuplicates() {
		t.Fatal("scalar-field VALUE index must NOT create duplicates")
	}

	fanOut := &Index{Name: "idx_tags", Type: IndexTypeValue, RootExpression: FanOut("tags")}
	if !fanOut.CreatesDuplicates() {
		t.Fatal("fan-out (repeated-field) VALUE index MUST create duplicates → not distinct")
	}

	// A nested fan-out is also duplicating.
	nestedFanOut := &Index{Name: "idx_nested", Type: IndexTypeValue, RootExpression: Nest("parent", FanOut("child"))}
	if !nestedFanOut.CreatesDuplicates() {
		t.Fatal("nested fan-out VALUE index MUST create duplicates")
	}

	// A grouping/aggregate key delegates to its whole key (Java
	// GroupingKeyExpression.createsDuplicates → getWholeKey().createsDuplicates).
	groupFanOut := &Index{Name: "idx_grp_fan", Type: IndexTypeValue, RootExpression: GroupBy(Field("total"), FanOut("tags"))}
	if !groupFanOut.CreatesDuplicates() {
		t.Fatal("VALUE index whose grouping key fans out MUST create duplicates (whole-key delegation)")
	}
	groupScalar := &Index{Name: "idx_grp_scalar", Type: IndexTypeValue, RootExpression: GroupBy(Field("total"), Field("region"))}
	if groupScalar.CreatesDuplicates() {
		t.Fatal("VALUE index over scalar grouping keys must NOT create duplicates")
	}

	// Empty / Literal roots do NOT fan out — explicit false arms.
	if (&Index{Name: "idx_empty", Type: IndexTypeValue, RootExpression: &EmptyKeyExpression{}}).CreatesDuplicates() {
		t.Fatal("EmptyKeyExpression must not create duplicates")
	}
	if (&Index{Name: "idx_lit", Type: IndexTypeValue, RootExpression: &LiteralKeyExpression{}}).CreatesDuplicates() {
		t.Fatal("LiteralKeyExpression must not create duplicates")
	}

	// Unset root expression → false (safe default, no panic).
	if (&Index{Name: "empty", Type: IndexTypeValue}).CreatesDuplicates() {
		t.Fatal("index with no root expression must not report duplicates")
	}

	// An UNRECOGNIZED (custom/external) root FAILS CLOSED at the INDEX level even
	// for a VALUE index: Index.CreatesDuplicates()==true (conservatively
	// duplicating) so the M4 signal never mis-reports an unknown-fan-out index as
	// distinct. The SHARED helper createsDuplicates() still returns false (Split
	// validation reads it as positive proof — a fail-closed true there would
	// wrongly admit Split(customScalar)).
	if !(&Index{Name: "idx_custom", Type: IndexTypeValue, RootExpression: &customKeyExpression{}}).CreatesDuplicates() {
		t.Fatal("unrecognized VALUE-index root must FAIL CLOSED to CreatesDuplicates()=true (M4 safety)")
	}
	if createsDuplicates(&customKeyExpression{}) {
		t.Fatal("shared createsDuplicates() must keep default false for Split validation")
	}

	// --- Non-VALUE index type: fails closed regardless of a scalar root ---

	// A TEXT index tokenizes (one entry per token) → a scan can return the same
	// record multiple times, so it creates duplicates DESPITE a scalar root.
	textIdx := &Index{Name: "idx_text", Type: IndexTypeText, RootExpression: Field("body")}
	if !textIdx.CreatesDuplicates() {
		t.Fatal("TEXT index must create duplicates (one entry per token) regardless of scalar root")
	}

	// --- RFC-189 B2: VERSION indexes compute real root fan-out (Java builds a
	// value candidate for them). A VERSION index is one entry per record, so it
	// does NOT create duplicates — the earlier blanket non-VALUE fail-closed
	// wrongly reported it as duplicate-producing and lost a DISTINCT elision. ---

	versionScalar := &Index{Name: "idx_ver", Type: IndexTypeVersion, RootExpression: VersionKey()}
	if versionScalar.CreatesDuplicates() {
		t.Fatal("VERSION index (one entry per record) must NOT create duplicates")
	}
	// A VERSION index over a fan-out field still fans out.
	versionFanOut := &Index{Name: "idx_ver_fan", Type: IndexTypeVersion, RootExpression: FanOut("tags")}
	if !versionFanOut.CreatesDuplicates() {
		t.Fatal("VERSION index over a fan-out field must create duplicates")
	}
	// A VERSION index with an UNRECOGNIZED root still fails closed (self-protected
	// by createsDuplicatesRec's unrecognized=true default).
	versionCustom := &Index{Name: "idx_ver_custom", Type: IndexTypeVersion, RootExpression: &customKeyExpression{}}
	if !versionCustom.CreatesDuplicates() {
		t.Fatal("VERSION index with an unrecognized root must FAIL CLOSED to true")
	}
}
