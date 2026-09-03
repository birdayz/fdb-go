package embedded

// validateAggCallProvenance refuses a recorded call-to-column correspondence
// that no longer describes the columns in hand.
//
// Every arm is driven here as explicit state rather than left to whatever the
// corpus reaches. That is the point: the corpus exercises the SUCCESS path
// twenty thousand times a run and none of the refusals, so a validator whose
// refusals were never driven would be believed without being tested — and its
// first real firing would be read as a finding rather than as an untested
// branch.
//
// The PERMUTATION arm is the one that matters most. An earlier design checked
// only the recorded length and the per-index function, and reasoned that any
// mutation preserving both left the indices correct. That is false for two
// same-function entries swapped in place — and every shape in RFC-241's defect
// table is same-function (COUNT/COUNT, SUM/SUM), so a function check has zero
// discriminating power exactly where it is needed. It is not hypothetical
// either: aggSelectCol.selectOrdinal's own doc says "Reclassification may
// reorder aggSelectCol storage", so reordering is an anticipated operation, and
// sorting aggCols by selectOrdinal instead of returning an index list is the
// plausible edit that performs it.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

// aggProvCols builds parsed columns for `COUNT(CASE 'us')`, `COUNT(CASE 'US')`
// — the RFC-241 pair, which shares a function and differs only in a literal's
// case — plus a bare-column SUM.
func aggProvCols() []aggSelectCol {
	return []aggSelectCol{
		{aggFunc: "COUNT", aggArg: "CASEWHENRegion='us'THEN1END", visible: true, selectOrdinal: 1},
		{aggFunc: "COUNT", aggArg: "CASEWHENRegion='US'THEN1END", visible: true, selectOrdinal: 2},
		{aggFunc: "SUM", aggArg: "AMOUNT", aggArgBare: "AMOUNT", visible: true, selectOrdinal: 3},
	}
}

func aggProvAggregate(cols []aggSelectCol) *logical.LogicalAggregate {
	calls, callToAggCol, _ := logicalAggregateCalls(cols, false, func(s string) string { return s })
	agg := logical.NewAggregate(nil, nil, calls, make([]string, len(calls)), false)
	agg.CallProvenance = callToAggCol
	agg.CallProvenanceCols = len(cols)
	agg.HasCallProvenance = true
	return agg
}

func TestValidateAggCallProvenance(t *testing.T) {
	t.Parallel()

	t.Run("accepts what the producer recorded", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		if err := validateAggCallProvenance(agg, cols); err != nil {
			t.Fatalf("unmodified provenance must validate: %v", err)
		}
		// NON-VACUITY. A validator that checks nothing also returns nil, so the
		// success arm has to assert it actually had indices to check. Three
		// columns, no COUNT(*) prepend, so three calls each naming its column.
		if len(agg.CallProvenance) != 3 {
			t.Fatalf("expected 3 recorded indices, got %d — the success arm is "+
				"passing without validating anything", len(agg.CallProvenance))
		}
		for i, p := range agg.CallProvenance {
			j := p.AggColIdx
			if j != i {
				t.Errorf("call %d should name column %d, got %d", i, i, j)
			}
		}
	})

	t.Run("refuses a table that is not parallel to the calls", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		agg.CallProvenance = agg.CallProvenance[:2]
		assertProvenanceRefused(t, agg, cols, "entries for")
	})

	t.Run("refuses when the column population moved", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		// The shape an append between production and consumption produces.
		grown := append(cols, aggSelectCol{aggFunc: "MIN", aggArg: "AMOUNT", visible: true})
		assertProvenanceRefused(t, agg, grown, "moved between lowering and operand resolution")
	})

	t.Run("refuses an index past the end", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		agg.CallProvenance[1].AggColIdx = len(cols)
		assertProvenanceRefused(t, agg, cols, "names parsed column")
	})

	t.Run("refuses when the function at an index changed", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		cols[1].aggFunc = "MIN"
		assertProvenanceRefused(t, agg, cols, "no longer matches parsed column")
	})

	t.Run("refuses a PERMUTATION of two same-function columns", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		// The RFC-241 pair swapped. Length is unchanged, both are COUNT, both
		// indices are in range — everything a length-plus-function check looks
		// at still holds, and the operands are now bound the wrong way round.
		// Only the operand text separates them.
		cols[0], cols[1] = cols[1], cols[0]
		assertProvenanceRefused(t, agg, cols, "now renders")
	})

	t.Run("refuses a column that gained a qualifier", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		// A column recorded as `AMOUNT` that now renders `S.AMOUNT`. An earlier
		// draft offered a second spelling — the argument with its leading segment
		// dropped — inherited from the pre-RFC-241 matcher, where it existed to
		// accommodate the producer's qualifier strip. Comparing the PRE-strip text
		// gave that accommodation nothing to do, and it became a hole: the first
		// spelling matches by construction, so the second could only ever match
		// when the first did not, which is exactly when the column HAS changed.
		// This shape validated clean.
		cols[2].aggArg = "S.AMOUNT"
		cols[2].aggArgSegs = []string{"S", "AMOUNT"}
		assertProvenanceRefused(t, agg, cols, "now renders")
	})

	t.Run("absent table is not an error", func(t *testing.T) {
		t.Parallel()
		cols := aggProvCols()
		agg := aggProvAggregate(cols)
		// The correlated scalar-subquery producer builds its calls itself and
		// resolves its own operands, so it records nothing and never reaches
		// the operand loop. That must stay legal rather than become a refusal.
		agg.CallProvenance = nil
		agg.HasCallProvenance = false
		if err := validateAggCallProvenance(agg, cols); err != nil {
			t.Fatalf("an absent provenance table means a producer that resolves "+
				"its own operands, not a corrupt one: %v", err)
		}
	})
}

func assertProvenanceRefused(t *testing.T, agg *logical.LogicalAggregate, cols []aggSelectCol, want string) {
	t.Helper()
	err := validateAggCallProvenance(agg, cols)
	if err == nil {
		t.Fatalf("expected a refusal naming %q; got nil. A corrupt correspondence "+
			"that validates is a silent rebind — the wrong-answer class RFC-241 removed", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal should name %q, got: %v", want, err)
	}
}

// The COUNT(*) prepend has no parsed column, and the two slices must stay
// parallel across it — a drift there rebinds every call after the prepend by
// one, which is the same silent misbinding RFC-241 removed.
func TestLogicalAggregateCallsRecordsTheCountStarPrepend(t *testing.T) {
	t.Parallel()
	cols := []aggSelectCol{
		{aggFunc: "SUM", aggArg: "AMOUNT", aggArgBare: "AMOUNT", visible: true},
	}
	calls, callToAggCol, _ := logicalAggregateCalls(cols, true, func(s string) string { return s })
	if len(calls) != len(callToAggCol) {
		t.Fatalf("calls and provenance must stay parallel: %d vs %d", len(calls), len(callToAggCol))
	}
	if len(calls) != 2 {
		t.Fatalf("expected the synthesized COUNT(*) plus one column call, got %d", len(calls))
	}
	if callToAggCol[0].AggColIdx != -1 {
		t.Errorf("the synthesized COUNT(*) has no parsed column; want -1, got %d", callToAggCol[0].AggColIdx)
	}
	if callToAggCol[1].AggColIdx != 0 {
		t.Errorf("the SUM call comes from column 0, got %d", callToAggCol[1].AggColIdx)
	}
}

// The duplicate-COUNT(*) skip is the one `continue` that fires AFTER a call has
// been built, so it is where the two slices are most likely to drift: skip the
// call append and not the provenance append and every later call is off by one.
// Driven explicitly because the corpus reaches it rarely and a drift there is
// silent.
func TestLogicalAggregateCallsSkipsTheDuplicateCountStarInBothSlices(t *testing.T) {
	t.Parallel()
	cols := []aggSelectCol{
		// The harvested duplicate of the synthesized COUNT(*).
		{aggFunc: "COUNT", aggArg: "*", visible: true},
		{aggFunc: "SUM", aggArg: "AMOUNT", aggArgBare: "AMOUNT", visible: true},
	}
	calls, prov, _ := logicalAggregateCalls(cols, true, func(s string) string { return s })
	if len(calls) != len(prov) {
		t.Fatalf("the duplicate-COUNT(*) skip must skip BOTH slices: %d calls, %d provenance",
			len(calls), len(prov))
	}
	if len(calls) != 2 {
		t.Fatalf("expected the synthesized COUNT(*) plus the SUM — the harvested duplicate is "+
			"suppressed — got %d: %+v", len(calls), calls)
	}
	if prov[0].AggColIdx != -1 {
		t.Errorf("call 0 is the synthesized COUNT(*); want -1, got %d", prov[0].AggColIdx)
	}
	// The SUM is aggCols[1]. If the skip dropped only the call, this reads 0 —
	// the off-by-one that would bind the SUM's operand to the COUNT(*) column.
	if prov[1].AggColIdx != 1 {
		t.Errorf("call 1 is the SUM at column 1; want 1, got %d — the slices drifted "+
			"across the duplicate-COUNT(*) skip", prov[1].AggColIdx)
	}
}
