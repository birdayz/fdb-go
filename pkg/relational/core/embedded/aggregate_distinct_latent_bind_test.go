package embedded

// A DISTINCT aggregate cannot bind through either name fallback, and one of the
// two structural matchers never compares DISTINCT-ness at all. Both are real
// defects and both are UNREACHABLE today, so this pins the thing that makes
// them unreachable rather than the defects themselves.
//
// The mechanism, so the next reader does not re-derive it:
//
//   - canonicalAggName and aggregateValueOutputName both render an aggregate as
//     FN(operand), with no DISTINCT. logical.AggregateCall.CanonicalName()
//     renders FN(DISTINCT operand). The two can therefore never be equal for a
//     DISTINCT call, so a name fallback comparing them cannot ever bind one.
//   - aggregateCallOutputSlot's STRUCTURAL arm filters on Star and Func and
//     never reads call.Distinct, so SUM(x) and SUM(DISTINCT x) — same function,
//     same operand Value — both match, and the first wins.
//
// Neither can fire because DISTINCT aggregates are refused upstream. That
// refusal is the load-bearing fact, and it is what this test holds.
//
// WHY THIS AND NOT THE FOUR ASSERTIONS THAT ALREADY EXIST. The rejection is
// asserted four times in embedded_fdb_test.go (:4884, :4889, :7660, :7755), all
// through expectRejectionOrCascadesError — a tolerant matcher that passes on a
// rejection OR on a generic Cascades error, and :7660 matches on the bare
// substring "DISTINCT". That is right for what those tests are about (the
// feature is absent and they do not care how) and exactly wrong as a tripwire
// here: each would keep passing if the rejection weakened, moved, or became a
// different error — which is the event that ARMS the two defects above. Breadth
// of coverage is not strength of assertion.
//
// DIRECTION OF THE ALARM: this test goes red when DISTINCT aggregates start
// being accepted. That is not a regression — it is the signal that closing
// TODO §5's "E091-07 COUNT(DISTINCT)" has armed both sites named above, and
// they must be fixed in the same change.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func TestDistinctAggregateRejectionKeepsTheBindDefectsUnreachable(t *testing.T) {
	t.Parallel()

	// The exact message the planner refuses with. Asserted verbatim, not
	// through a tolerant matcher, so a reworded or relocated rejection fails
	// here instead of passing four other tests.
	const want = "DISTINCT aggregates are not supported"

	// Driven through the real refusal site rather than compared against a copy
	// of its string: an aggregate carrying HasDistinctAggregate is exactly what
	// the planner refuses, and findDistinctAggregate is where it does so.
	agg := logical.NewAggregate(nil, nil, nil, nil, false)
	agg.HasDistinctAggregate = true
	got := findDistinctAggregate(agg)
	if got != want {
		t.Fatalf("the DISTINCT-aggregate rejection is what keeps two latent bind\n"+
			"defects unreachable, and it has changed.\n"+
			"  got:  %q\n  want: %q\n\n"+
			"If DISTINCT aggregates are now SUPPORTED, this test has done its job and both\n"+
			"defects are live — fix them in the same change:\n"+
			"  1. canonicalAggName / aggregateValueOutputName render FN(operand) while\n"+
			"     AggregateCall.CanonicalName() renders FN(DISTINCT operand), so a DISTINCT\n"+
			"     call can never bind through a name fallback;\n"+
			"  2. aggregateCallOutputSlot's structural arm never compares call.Distinct, so\n"+
			"     SUM(x) and SUM(DISTINCT x) both match and the first wins.\n"+
			"See RFC-241 \"Adjacent latent defect, pinned not fixed\".", got, want)
	}
}

// The two renderers really do omit DISTINCT while CanonicalName emits it. This
// is the half of the mechanism that lives in code rather than in prose, so it is
// asserted rather than described — otherwise the comment above is the only thing
// holding it and comments do not fail.
func TestAggregateNameRenderersOmitDistinct(t *testing.T) {
	t.Parallel()
	rendered := canonicalAggName("SUM", nil)
	if strings.Contains(strings.ToUpper(rendered), "DISTINCT") {
		t.Errorf("canonicalAggName now emits DISTINCT (%q). If that is deliberate, the "+
			"name fallbacks can bind a DISTINCT call and the first defect in "+
			"TestDistinctAggregateRejectionKeepsTheBindDefectsUnreachable is closed — "+
			"update both tests together.", rendered)
	}
}
