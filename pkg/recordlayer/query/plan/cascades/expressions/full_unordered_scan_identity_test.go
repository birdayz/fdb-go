package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func scanIdentityFields() []values.Field {
	return []values.Field{{Name: "F", FieldType: values.NotNullLong, Ordinal: 0}}
}

func scanIdentityExpr(t *testing.T, rowType values.Type) *FullUnorderedScanExpression {
	t.Helper()
	e, err := NewFullUnorderedScanExpression([]string{"MY_TABLE"}, rowType)
	if err != nil {
		t.Fatalf("building scan over %v: %v", rowType, err)
	}
	return e
}

// TestFullUnorderedScan_MatchesAcrossRecordNames pins the property every index
// access path in the planner rests on, and which nothing else states at this
// site.
//
// rule_match_leaf.go's matchLeafWithCandidate decides subsumption by calling
// EqualsWithoutChildren. The query scan is built over an UNNAMED record type
// assembled from the SQL columns (cascades_translator.go passes
// NewRecordType("", false, cols)); the candidate scan is built "over the
// candidate's exact row" (index_expansion.go), whose record type carries the
// table name. They compare equal only because exactType's canonical identity
// omits the record name — deliberately, as "provenance, not shape", matching
// Java's Type.Record equals/hashCode.
//
// Put the name back into that canonical form and this test fails — but it is
// NOT the only thing that does, and claiming otherwise would be the kind of
// over-statement this file exists to correct. Measured: that mutation reddens
// three packages (cascades, expressions, values), so the property is already
// guarded. What this adds is the guard AT THE SITE THAT DEPENDS ON IT, with a
// failure message naming the cause — the connection between "the record name
// entered a canonical form in values" and "index access paths are gone" is
// otherwise two packages of inference away.
func TestFullUnorderedScan_MatchesAcrossRecordNames(t *testing.T) {
	t.Parallel()

	queryShaped := scanIdentityExpr(t, values.NewRecordType("", false, scanIdentityFields()))
	candidateShaped := scanIdentityExpr(t, values.NewRecordType("MY_TABLE", false, scanIdentityFields()))

	if !queryShaped.EqualsWithoutChildren(candidateShaped, nil) {
		t.Fatal("an unnamed query-shaped scan no longer matches a named candidate-shaped " +
			"scan over the same fields. Leaf subsumption goes through this comparison, so " +
			"index access paths are now unreachable. The likely cause is the record NAME " +
			"entering exactType's canonical identity, where it is deliberately excluded.")
	}
	if queryShaped.HashCodeWithoutChildren() != candidateShaped.HashCodeWithoutChildren() {
		t.Error("the two hashed apart while comparing equal — that is the memo-breaking " +
			"direction: a hash-first lookup misses the equal member")
	}

	// The control that keeps the above from passing vacuously: differing FIELDS
	// must still separate, so equality is not simply ignoring the flowed type.
	differentFields := scanIdentityExpr(t, values.NewRecordType("", false,
		[]values.Field{{Name: "G", FieldType: values.NotNullString, Ordinal: 0}}))
	if queryShaped.EqualsWithoutChildren(differentFields, nil) {
		t.Error("two scans with different flowed FIELDS compared equal — the flowed type " +
			"has stopped discriminating entirely, and the match above proves nothing")
	}
}

// TestFullUnorderedScan_RefusesAPlaceholderFlowedType pins the fact that
// replaced this type's documented UnknownType wildcard: a scan cannot carry a
// placeholder flowed type at all. NewFullUnorderedScanExpression snapshots
// through snapshotExpressionResultType, which refuses a non-exact type, and
// that constructor is the only writer of the field.
//
// This matters because both identity methods here USED to be documented in
// terms of that wildcard — "the flowed type is non-discriminating when either
// side is UnknownType", "candidate scans keep UnknownType". Structural typing
// on both sides replaced it and the prose stayed. If placeholder flowed types
// ever become constructible again, this fails and the reasoning gets revisited
// rather than silently inherited a second time.
func TestFullUnorderedScan_RefusesAPlaceholderFlowedType(t *testing.T) {
	t.Parallel()

	if _, err := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType); err == nil {
		t.Fatal("a scan over UnknownType was constructed; the identity comments here argue " +
			"from the premise that it cannot be")
	}
	// Positive control: an exact type is accepted, so the refusal above is about
	// the placeholder rather than about construction failing for everything.
	if _, err := NewFullUnorderedScanExpression(
		[]string{"T"}, values.NewRecordType("r", false, scanIdentityFields()),
	); err != nil {
		t.Fatalf("a scan over an exact record type must build, got %v", err)
	}
}

// TestFullUnorderedScan_HashIsNamesOnlyDivergingFromJava pins a DELIBERATE
// divergence, so that closing it is a visible test change rather than a quiet
// one — because closing it regresses the planner.
//
// Equality compares the flowed type; the hash does not. Java hashes both
// (Objects.hash(recordTypes, flowedType), FullUnorderedScanExpression.java:150),
// so this is not the "names-only scan hash" an earlier comment claimed it was
// matching. Folding flowedType in to align reddens TestPlanShapeGolden by 13731
// lines — measured at tree 0bf01a4fe, and a golden-line count is a fact about
// ONE tree, so re-measure rather than quote it — and breaks three memo tests,
// all selecting a LogicalSortExpression where
// a plain scan should win: scan identity is the base of every query tree, so
// changing which scans share a bucket changes group membership and winner
// selection.
//
// The divergence is safe in the only direction that matters — an under-hash
// collides unequal expressions and never scatters equal ones — which is why it
// stays. If it is ever closed deliberately, this test and the golden move
// together.
func TestFullUnorderedScan_HashIsNamesOnlyDivergingFromJava(t *testing.T) {
	t.Parallel()

	a := scanIdentityExpr(t, values.NewRecordType("", false, scanIdentityFields()))
	b := scanIdentityExpr(t, values.NewRecordType("", false,
		[]values.Field{{Name: "G", FieldType: values.NotNullString, Ordinal: 0}}))

	if a.EqualsWithoutChildren(b, nil) {
		t.Fatal("two scans with different flowed types compared EQUAL; equality is " +
			"documented to compare the flowed type, so the hash claim below is untestable")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Error("two scans differing only in flowed type now hash APART. That closes the " +
			"documented divergence from Java, and it is not free: it changes scan memo " +
			"bucketing, which at tree 0bf01a4fe moved TestPlanShapeGolden by 13731 lines and flipped three " +
			"memo tests to a LogicalSortExpression winner. Update the golden and those " +
			"tests deliberately, or restore the names-only hash.")
	}

	// The direction that would actually break the memo: equal expressions must
	// share a bucket.
	c := scanIdentityExpr(t, values.NewRecordType("", false, scanIdentityFields()))
	if !a.EqualsWithoutChildren(c, nil) {
		t.Fatal("two identically-built scans must be equal, or the check below is vacuous")
	}
	if a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
		t.Error("two EQUAL scans hashed apart — a hash-first lookup misses the equal member")
	}
}
