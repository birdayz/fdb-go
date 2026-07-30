package embedded

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestLazyLegMintReachesNoWinningPlan is the measured NEGATIVE half of the
// leg-rebase channel's disposition, and it is load-bearing for two decisions.
//
// The rebase arm in rule_implement_nested_loop_join.go used to mint a dotted
// merged-row key (`QOV(merged)."LEG.COL"`) that the FlatMap inner's binder
// resolved by string. That mint is DELETED — the arm now hands a leg-correlated
// read back on its own correlation with its own leg-local ordinal — and this
// test is what licensed deleting it: the mint never reached a WINNING plan on
// any shape that drives the arm, so nothing at execution depended on it.
//
// The zero is kept, and it is not a formality now that the producer is gone. It
// is the standing statement that the merged row's DOTTED KEY is absent from what
// executes, and there are other producers of that spelling
// (exists_gathered_cluster_wrap.go, unnest_gather.go). If one starts reaching a
// winner, the channel has become load-bearing at execution and its retirement
// stops being a deletion.
//
// WHAT THE SAME MEASUREMENT ALSO SAYS, and it is sharper than the zero: the
// arm's product reaches no winner in EITHER form. Replacing the pass-through
// with a deliberate bind to the WRONG leg leaves the whole real-FDB sqldriver
// corpus green (measured), so the candidate carrying whatever this arm emits
// loses on every covered shape. That is why no row-level test can detect a
// change at this site, and why its disposition is pinned at RULE level
// (TestRebaseOuterLegValue_PassesThroughAnAlreadyCorrectLegLocalRead,
// TestRebaseOuterLegValue_DerivableLegKeepsTheLegLocalRead) rather than by rows.
//
// A movement here in EITHER direction is a real change in what executes, not
// a representation detail.
func TestLazyLegMintReachesNoWinningPlan(t *testing.T) {
	t.Parallel()

	const schema = `CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))
CREATE TABLE r (rid BIGINT, PRIMARY KEY (rid))
CREATE TABLE s (sid BIGINT, PRIMARY KEY (sid))
CREATE TABLE e (eid BIGINT, PRIMARY KEY (eid))
CREATE TABLE tw (id BIGINT, partner BIGINT, k BIGINT, PRIMARY KEY (id))`

	// The shapes measured to reach the rebase arm during planning. The first is
	// TestFDB_BuriedInnerJoinProjectedExists's query verbatim; the others are
	// the N-way and four-leg members of the same family
	// (pkg/relational/sqldriver, which prove the ROWS end-to-end). Each must
	// keep planning to something, so a plan error cannot be mistaken here for
	// "no dotted keys".
	shapes := []struct {
		name, sql string
	}{
		{"buried-inner-join", "SELECT p.v, EXISTS (SELECT 1 FROM e WHERE e.eid = p.id) " +
			"FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id"},
		{"nway-comma", "SELECT p.v, EXISTS (SELECT 1 FROM e WHERE e.eid = p.id) " +
			"FROM p, q, r WHERE q.qid = p.id AND r.rid = p.id"},
		{"four-leg", "SELECT p.v, EXISTS (SELECT 1 FROM e WHERE e.eid = p.id) " +
			"FROM p, q, r, s WHERE q.qid = p.id AND r.rid = p.id AND s.sid = p.id"},
		// The SELF-JOIN TWIN, correlated to the SECOND leg: both legs of the
		// self-join carry the same column at the same leg-relative ordinal in the
		// same row layout, so a dotted `TW.K` key reaching a winner would be a
		// spelling that names BOTH of them. TestFDB_SelfJoinTwinLegCorrelatedRead
		// runs the same shape for its rows.
		{"self-join-twin-second-leg", "SELECT a.id, EXISTS (SELECT 1 FROM e WHERE e.eid = b.k) " +
			"FROM tw AS a JOIN tw AS b ON b.id = a.partner JOIN q ON q.qid = a.id"},
	}

	// Positive control. Every assertion below is a ZERO, and a zero proves
	// nothing unless the detector can produce a non-zero. Hand it a plan that
	// carries exactly the mint's signature and require it to be seen — so a
	// detector that silently stopped walking predicate surfaces fails here
	// rather than reporting a clean channel for all three shapes.
	t.Run("detector-is-not-vacuous", func(t *testing.T) {
		t.Parallel()
		merged := values.NamedCorrelationIdentifier("q$control")
		rt := values.NewRecordType("P", false, []values.Field{
			{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
		})
		witness := plans.NewRecordQueryPredicatesFilterPlan(
			plans.NewRecordQueryScanPlan([]string{"P"}, values.Type(rt), false),
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					values.NewFieldValue(values.NewQuantifiedObjectValue(merged), "P.ID", values.UnknownType),
					predicates.Comparison{Type: predicates.ComparisonIsNotNull},
				),
			},
		)
		got := dottedMergedRowKeysOf(witness)
		if len(got) != 1 || got[0] != "P.ID@q$control" {
			t.Fatalf("dottedMergedRowKeysOf missed a planted dotted merged-row key: "+
				"got %v, want exactly [P.ID@q$control]. The zero-count assertions in "+
				"this test are only meaningful while this control holds.", got)
		}
	})

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(sh.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			// Guard against a vacuous zero: if the shape stopped producing the
			// existential fold, "no dotted keys" would be true for the wrong
			// reason.
			explain := plan.Explain()
			if !strings.Contains(explain, "FlatMap") || !strings.Contains(explain, "FirstOrDefault") {
				t.Fatalf("shape no longer reaches the FlatMap/FirstOrDefault "+
					"existential fold, so a zero dotted-key count says nothing "+
					"about the rebase channel; explain:\n%s", explain)
			}
			if dotted := dottedMergedRowKeysOf(plan); len(dotted) != 0 {
				t.Fatalf("a dotted merged-row key reached the WINNING plan: %v\n"+
					"The leg-rebase channel was measured to carry nothing at "+
					"execution on this shape; if that changed, the channel is now "+
					"load-bearing and retiring it is no longer a deletion — work out "+
					"which binding changed before updating this expectation.\n"+
					"explain:\n%s", dotted, explain)
			}
		})
	}
}

// dottedMergedRowKeysOf collects every dotted FieldValue over a
// QuantifiedObjectValue in plan, rendered as "FIELD@CORR". That is the exact
// signature of the leg-rebase arm's mint; no other producer in these shapes
// emits a dotted Field, since every table here declares only flat top-level
// columns.
func dottedMergedRowKeysOf(plan plans.RecordQueryPlan) []string {
	var out []string
	visit := func(v values.Value) values.Value {
		if fv, ok := v.(*values.FieldValue); ok && strings.Contains(fv.Field, ".") {
			if q, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
				out = append(out, fmt.Sprintf("%s@%s", fv.Field, q.Correlation.Name()))
			}
		}
		return v
	}
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, visit)
		}
	}
	collectRanges := func(crs []*predicates.ComparisonRange) {
		for _, cr := range crs {
			switch {
			case cr.IsEquality():
				collectComparison(cr.GetEqualityComparison())
			case cr.IsInequality():
				for _, c := range cr.GetInequalityComparisons() {
					collectComparison(c)
				}
			}
		}
	}
	plans.Walk(plan, func(p plans.RecordQueryPlan) bool {
		if rv := p.GetResultValue(); rv != nil {
			values.Replace(rv, visit)
		}
		switch t := p.(type) {
		case *plans.RecordQueryPredicatesFilterPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, visit)
			}
		case *plans.RecordQueryFilterPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, visit)
			}
		case *plans.RecordQueryNestedLoopJoinPlan:
			for _, pr := range t.GetPredicates() {
				predicates.ReplaceValues(pr, visit)
			}
		case *plans.RecordQueryScanPlan:
			collectRanges(t.GetScanComparisons())
		case *plans.RecordQueryIndexPlan:
			collectRanges(t.GetScanComparisons())
		}
		return true
	})
	return out
}
