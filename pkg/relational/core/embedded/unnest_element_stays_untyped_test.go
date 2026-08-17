package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestUnnestElementQuantifierCarriesExactScalar pins the boundary that
// values.IsMixedSeedElementType rests on, from the side that can break it.
//
// That predicate discriminates a lateral unnest's whole-object ELEMENT from a
// join LEG by asking whether the quantifier's type is a RECORD — a PROXY, and
// its own doc says so. The proxy holds only while nothing types an element's
// quantifier object, and the whole RFC-197 direction is to type MORE quantifier
// objects. So it is a landmine sitting directly on the migration path, and this
// test is the tripwire.
//
// It was not hypothetical. Typing the resolver's correlated quantifier-object
// mints from the source's declared column list — the fix that made every
// leg-correlated read state its own identity — also typed the unnest binding,
// whose scope entry is a VIRTUAL one-column table (RFC-142). The merged seed's
// element slot went from
//
//	_1 UNKNOWN NULL          ->   _1 RECORD<X UNKNOWN NULL> NULL
//
// and the element stopped being recognised as an element. MEASURED consequence
// over the real-FDB corpus: 6 test families red (buried-element predicates,
// enclosed middle unnest EXISTS, projected-EXISTS enclosure lift, within-box
// dup, unnest EXISTS gather, array unnest ordinality), in two flavours — LOUD
// ("multi-leg row cannot serve a source-relative ordinal") and, worse, SILENT
// WRONG ROWS: `SELECT A."K", "X" FROM A, C, C."ARR" AS "X" WHERE A."K" = "X"`
// returned NO rows where one was due.
//
// The virtual table's column list is a RESOLUTION convenience — it is what makes
// `SELECT "X"` resolve — never the row the quantifier flows. An unnest element
// is ONE array element, which is Java's isPrimitive() whole-object case. Under
// RFC-232 that object is no longer UNKNOWN: it is the exact declared element
// type, LONG NOT NULL in this fixture.
//
// A RECORD here still confuses the element with a leg; UNKNOWN now fails earlier
// because an exact QuantifiedObjectValue cannot be built. Both are regressions.
func TestUnnestElementQuantifierCarriesExactScalar(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE a (aid BIGINT, k BIGINT, PRIMARY KEY (aid))
CREATE TABLE c (cid BIGINT, arr BIGINT ARRAY, PRIMARY KEY (cid))`
	const sql = `SELECT A."K", "X" FROM A, C, C."ARR" AS "X" WHERE A."K" = "X"`

	plan, err := PlanPhysicalForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Every quantifier object anywhere in the plan's value surfaces, by
	// correlation, with the type it states.
	seen := map[string][]values.Type{}
	// resolverMinted is the subset read off an UNPINNED FieldValue — the
	// resolver's own source-relative bake. The distinction is load-bearing for
	// the positive control below: the PLANNER also mints quantifier objects for
	// correlation A and types them (FrontierPinned seed refs), so a control that
	// merely asks "is anything called A typed" stays green with the resolver's
	// typing removed entirely. Measured — it did.
	resolverMinted := map[string][]values.Type{}
	collect := func(v values.Value) {
		values.WalkValue(v, func(n values.Value) bool {
			if fv, isFV := values.AsFieldValue(n); isFV && !fv.Path().IsFrontierPinned() {
				if qov, isQ := values.AsQuantifiedObjectValue(fv.ChildValue()); isQ {
					resolverMinted[qov.Correlation().Name()] = append(resolverMinted[qov.Correlation().Name()], qov.FlowedType())
				}
			}
			if qov, ok := values.AsQuantifiedObjectValue(n); ok {
				seen[qov.Correlation().Name()] = append(seen[qov.Correlation().Name()], qov.FlowedType())
			}
			return true
		})
	}
	plans.Walk(plan, func(n plans.RecordQueryPlan) bool {
		collect(n.GetResultValue())
		if nlj, isNLJ := n.(*plans.RecordQueryNestedLoopJoinPlan); isNLJ {
			for _, p := range nlj.GetPredicates() {
				predicates.ReplaceValues(p, func(v values.Value) values.Value { collect(v); return v })
			}
		}
		if pf, isPF := n.(*plans.RecordQueryPredicatesFilterPlan); isPF {
			for _, p := range pf.GetPredicates() {
				predicates.ReplaceValues(p, func(v values.Value) values.Value { collect(v); return v })
			}
		}
		return true
	})

	// POSITIVE CONTROL. The real table's quantifier MUST state its row — that is
	// the fix this boundary sits beside, and without it the assertion below holds
	// for the uninteresting reason that nothing is typed at all.
	aTypes, sawA := resolverMinted["A"]
	if !sawA {
		t.Fatalf("no UNPINNED (resolver-minted) quantifier object for correlation A; the "+
			"shape changed and this test no longer probes what it was written for.\n"+
			"  seen: %v\n  plan: %s", seen, plan.Explain())
	}
	typedA := false
	for _, ty := range aTypes {
		if ty != nil && ty.Code() == values.TypeCodeRecord {
			typedA = true
		}
	}
	if !typedA {
		t.Fatalf("correlation A's RESOLVER-MINTED quantifier object states no ROW (%v). The resolver's "+
			"correlated mint is supposed to carry the source's declared row so a "+
			"leg-correlated read can state its own identity; if that regressed, the "+
			"leg-local identity census falls back to identityOtherDomain and the "+
			"assertion below is vacuous.\n  plan: %s", aTypes, plan.Explain())
	}

	// THE BOUNDARY. The unnest element's quantifier must state the exact scalar.
	sawX := false
	for corr, types := range seen {
		if corr != "X" {
			continue
		}
		sawX = true
		for _, ty := range types {
			if ty == nil || ty.Code() != values.TypeCodeLong || ty.IsNullable() {
				t.Fatalf("the unnest ELEMENT quantifier %q states %v, want exact LONG NOT NULL. "+
					"The virtual lookup row must not become the flowed type, and UNKNOWN is "+
					"not an admissible RFC-232 QOV.\n  plan: %s", corr, ty, plan.Explain())
			}
		}
	}
	if !sawX {
		t.Fatalf("no exact quantified object for unnest binding X; the virtual scope source "+
			"likely still exposes UNKNOWN and construction declined.\n  seen: %v\n  plan: %s", seen, plan.Explain())
	}
}
