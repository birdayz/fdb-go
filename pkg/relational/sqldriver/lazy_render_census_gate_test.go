package sqldriver_test

import (
	"flag"
	"fmt"
	"io"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The two censuses that measured the lazy-render retirement were PRINTED and
// never asserted, unlike the ~15 siblings in the same TestMain block that flip
// the exit code. Their numbers — `lazy-EXPLAIN-RENDERED 21,865 -> 0` and
// `declines 4 -> 1` — lived in exactly one place in the tree: a prose `why`
// string on the field-decision ratchet, which nothing parses. A regression
// straight back to 21,865 mints and 4 declines would have failed NOTHING. What
// has to survive is the measurement, not the prose.
//
// Both wrappers follow the local convention rather than inventing one: a
// whole-corpus POPULATION claim is withheld under `-test.run` narrowing, where it
// would red on the filter rather than on a defect, while a CEILING survives —
// a subset cannot exceed the whole, so a ceiling is exact under any filter. The
// unresolved-result-type gate next door is the sibling this pair is modelled on,
// and the dotted-witness gate is the one that keeps a hard zero across narrowing.
//
// The arms themselves are pinned as a UNIT, away from this corpus and away from
// the process globals, in the values package's census_gate_test.go: every
// ceiling, every floor, both no-op shapes and the mint census's partition check.
// A corpus reading exercises only the arms the corpus happens to reach, so it is
// not a substitute for that.

// narrowed reports whether -test.run cut the corpus down, in which case no
// whole-corpus population floor here can be honestly decided.
func narrowed() (string, bool) {
	f := flag.Lookup("test.run")
	if f == nil || f.Value.String() == "" {
		return "", false
	}
	return f.Value.String(), true
}

// assertAccessorPathCensus gates the ratchet arm of the accessor-name-path
// census over this corpus.
//
// MaxDeclineDotted is 0, and getting there is a MEASURED reconciliation rather
// than the number this gate was written with. It was written as a BAND [1,1],
// because the population at the time was one decline whose witness was `N.SK` —
// `GROUP BY n.sk`, a genuinely NESTED path (struct column `n`, field `sk`) the
// resolver FUSES into a single flat Field, i.e. the real `addr.city` versus
// `T.city` ambiguity the guard exists for. That reading is now stale: over the
// whole corpus the arm reports ZERO in 317,738 calls, because a nested-path
// GROUP BY key is REJECTED before it reaches the planner and the query that
// produced `N.SK` no longer plans at all.
//
// So the guard's direction inverted the moment its expected value did. A floor of
// 1 is now unsatisfiable, and the alarm is GROWTH alone: the arm coming back
// means either the lazy render returned, or the nested-path GROUP BY rejection
// was relaxed without the resolver keeping the path segmented. The instrument
// dying — the other thing a floor on the arm would have caught — is covered by
// MinTotal, which is why the floor is superseded rather than merely dropped.
//
// The three declines before that were `q$510549.AID#0` and `q$510687.AID#0` —
// RENDERED EXPLAIN LABELS, not column names, minted by RecordQueryInMemorySortPlan.
// HintOrdering re-entering a display string as an identity. That arm is gone too.
//
// MinTotal is floored well over an order of magnitude below the measurement
// (317,738 calls this run; 269,979 and 318,449 on earlier ones) because what it
// guards is the instrument being ALIVE, not its exact traffic.
func assertAccessorPathCensus(w io.Writer) bool {
	gates := &values.AccessorPathGates{
		MinTotal:         10000,
		MaxDeclineDotted: intPtr(0),
	}
	if pat, cut := narrowed(); cut {
		fmt.Fprintf(w, "accessor-path census: whole-corpus vacuity FLOOR not checked "+
			"(-test.run=%q narrowed the population; the call total is a claim about the whole "+
			"suite). The dotted-decline CEILING is still checked — a subset cannot exceed the "+
			"whole.\n", pat)
		gates.MinTotal = 0
	}
	return values.AssertAccessorPathCensus(w, gates)
}

// assertFieldValueMintCensus gates the producer half.
//
// MaxExplainRendered is 0 with NO floor beside it, and the asymmetry with its
// consumer-side twin is deliberate: zero is the STEADY STATE for this class.
// A Field carrying both '.' and '#' is the shape explainValueOrdinals emits and
// nothing else does, so any non-zero is Explain output re-entered as identity —
// a defect, not debt. The whole measured population was 21,865, and all 64
// captured origins pointed at ONE line: HintOrdering minting a lazy FieldValue
// from SortKey.Field while SortKey.ValueExpr carried the baked identity all
// along. Growth is the only alarm direction there is here.
//
// MinTotal is the vacuity guard on the INDEPENDENT denominator, and it is what
// makes the ceiling mean anything: `lazy-EXPLAIN-RENDERED = 0` over zero observed
// mints passes perfectly while measuring nothing, which is this repo's dominant
// false positive.
func assertFieldValueMintCensus(w io.Writer) bool {
	gates := &values.FieldValueMintGates{
		MinTotal:           5000,
		MaxExplainRendered: intPtr(0),
	}
	if pat, cut := narrowed(); cut {
		fmt.Fprintf(w, "field-value mint census: whole-corpus vacuity FLOOR not checked "+
			"(-test.run=%q narrowed the population). The lazy-EXPLAIN-RENDERED CEILING and the "+
			"partition check both survive narrowing and are still checked.\n", pat)
		gates.MinTotal = 0
	}
	return values.AssertFieldValueMintCensus(w, gates)
}

func intPtr(n int) *int { return &n }
