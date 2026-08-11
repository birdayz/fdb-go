package embedded

import "testing"

// TestAggOutputCols_CountArmsAgreeOnNullability pins that BOTH COUNT arms of
// aggOutputCols type their output the same way.
//
// WHY THIS IS A UNIT TEST AND NOT AN FDB ONE. The field is not observable from
// the driver: `ColumnTypes()[i].Nullable()` reports true for every column on
// this path — a primary key included — so an end-to-end assertion on it is
// VACUOUS and passes whatever aggOutputCols says. MEASURED, by minting the
// SELECT-list COUNT(*) `nullable: false` again and watching the sqldriver arm
// stay green. The honest place to pin the field is where it is written.
//
// THE DISAGREEMENT THIS CATCHES. aggOutputCols has two COUNT paths written
// twenty lines apart: the SELECT-list COUNT(*) is minted inline at the top, and
// COUNT(x) goes through aggregateOutputColumn. The first minted `nullable:
// false` while the second returned `nullable: true`. Java has one rule for
// both — getResultType is `Type.primitiveType(TypeCode.LONG)`
// (CountValue.java:140-141) and the one-argument overload hardcodes
// isNullable=true (Type.java:404-405) — so both are LONG NULLABLE.
//
// Nothing observable moved when this was corrected, and that is the finding
// rather than a reason to skip the pin: the two arms are one rule expressed
// twice, and the next change to either has nothing else to hold them together.
func TestAggOutputCols_CountArmsAgreeOnNullability(t *testing.T) {
	t.Parallel()

	countStarOnly := &selectQuery{selectClassification: selectClassification{countStar: true}}
	countColOnly := &selectQuery{selectClassification: selectClassification{aggCols: []aggSelectCol{
		{outName: "C", aggFunc: "COUNT", aggArg: "COL1", aggArgBare: "COL1", visible: true},
	}}}

	// md is nil on purpose: the types are not what this pins, and a nil
	// catalog is the path that leaves every argument-derived type UNKNOWN
	// while the COUNT rules — which ignore their argument — still apply.
	star := aggOutputCols(countStarOnly, nil)
	col := aggOutputCols(countColOnly, nil)

	if len(star) != 1 || len(col) != 1 {
		t.Fatalf("expected one output column each, got %d and %d", len(star), len(col))
	}
	for _, tc := range []struct {
		arm string
		got aggOutputCol
	}{{"COUNT(*)", star[0]}, {"COUNT(col)", col[0]}} {
		if tc.got.typ != "BIGINT" {
			t.Errorf("%s types its output %q, want BIGINT — Java's CountValue "+
				"carries TypeCode.LONG for both of its operators", tc.arm, tc.got.typ)
		}
		if !tc.got.nullable {
			t.Errorf("%s mints its output NOT NULL, want nullable.\n"+
				"  Java types every aggregate result through the one-argument "+
				"Type.primitiveType, which hardcodes isNullable=true, COUNT "+
				"included. The two COUNT arms of this function are one rule "+
				"written twice and nothing else holds them together.", tc.arm)
		}
	}
	if star[0].nullable != col[0].nullable || star[0].typ != col[0].typ {
		t.Errorf("the two COUNT arms DISAGREE: COUNT(*) is (%q, nullable=%v) and "+
			"COUNT(col) is (%q, nullable=%v). They are the same Java rule.",
			star[0].typ, star[0].nullable, col[0].typ, col[0].nullable)
	}
}
