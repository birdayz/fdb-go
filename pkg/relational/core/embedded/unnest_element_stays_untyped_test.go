package embedded

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestUnnestElementQuantifierStaysUntyped pins the boundary that
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
// is ONE array element, which is Java's isPrimitive() whole-object case.
//
// WHEN THIS GOES RED, do not re-type the element to make it pass. Either the
// resolver started stating a row for a shadowing source again (fix the
// resolver), or IsMixedSeedElementType was converted to a STRUCTURAL
// discrimination — which is the real long-term fix, since a proxy on
// record-ness cannot survive a migration whose whole direction is adding types.
// If it is the latter, retarget this test to assert the structural marker
// instead, and remember the executor's span derivation must agree BIT FOR BIT
// (that agreement is why the predicate is one function and not two).
func TestUnnestElementQuantifierStaysUntyped(t *testing.T) {
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
	seen := map[string][]string{}
	// resolverMinted is the subset read off an UNPINNED FieldValue — the
	// resolver's own source-relative bake. The distinction is load-bearing for
	// the positive control below: the PLANNER also mints quantifier objects for
	// correlation A and types them (FrontierPinned seed refs), so a control that
	// merely asks "is anything called A typed" stays green with the resolver's
	// typing removed entirely. Measured — it did.
	resolverMinted := map[string][]string{}
	collect := func(v values.Value) {
		values.WalkValue(v, func(n values.Value) bool {
			if fv, isFV := n.(*values.FieldValue); isFV && fv.Resolved != nil && !fv.Resolved.FrontierPinned {
				if qov, isQ := fv.Child.(*values.QuantifiedObjectValue); isQ {
					resolverMinted[qov.Correlation.Name()] = append(resolverMinted[qov.Correlation.Name()], fmt.Sprint(qov.Typ))
				}
			}
			if qov, ok := n.(*values.QuantifiedObjectValue); ok {
				seen[qov.Correlation.Name()] = append(seen[qov.Correlation.Name()], fmt.Sprint(qov.Typ))
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
		if strings.Contains(ty, "RECORD") {
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

	// THE BOUNDARY. The unnest element's quantifier must NOT state a row.
	for corr, types := range seen {
		if corr != "X" {
			continue
		}
		for _, ty := range types {
			if strings.Contains(ty, "RECORD") {
				t.Fatalf("the unnest ELEMENT quantifier %q states a ROW (%s).\n\n"+
					"  values.IsMixedSeedElementType discriminates the element from a join "+
					"leg by asking whether this type is a RECORD, so stating one here makes "+
					"the element read as a leg. The measured consequence is not a plan-shape "+
					"change — the plan is byte-identical — it is wrong leg windows, which "+
					"surface as a LOUD \"multi-leg row cannot serve a source-relative "+
					"ordinal\" on some shapes and as SILENTLY MISSING ROWS on others.\n\n"+
					"  Read this test's doc comment before making it pass: re-typing the "+
					"element is the wrong repair.\n  plan: %s", corr, ty, plan.Explain())
			}
		}
	}
}
