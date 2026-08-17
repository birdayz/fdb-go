package embedded

import (
	"fmt"
	"strings"
	"sync/atomic"
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

	// Counted across the parallel shape subtests, so the positive sentinel's own
	// emptiness is a checked fact rather than an invisible pass. Local, not
	// package-level: a package variable would be summed across whichever tests
	// happened to run, which is how a census in this repo once reported three
	// concurrent tests' planning as one number.
	var sentinelPopulation atomic.Int64

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
			{Name: "P.ID", FieldType: values.NotNullLong, Ordinal: 0},
		})
		scan, err := plans.NewRecordQueryScanPlan([]string{"P"}, values.Type(rt), false)
		if err != nil {
			t.Fatalf("scan witness: %v", err)
		}
		owner, err := values.NewQuantifiedObjectValue(merged, rt)
		if err != nil {
			t.Fatalf("witness QOV: %v", err)
		}
		dotted, err := values.ResolveFieldOrdinals(owner, []int{0})
		if err != nil {
			t.Fatalf("witness field: %v", err)
		}
		witness, err := plans.NewRecordQueryPredicatesFilterPlan(
			scan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					dotted,
					predicates.Comparison{Type: predicates.ComparisonIsNotNull},
				),
			},
		)
		if err != nil {
			t.Fatalf("filter witness: %v", err)
		}
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

			// THE POSITIVE SENTINEL, walking the same winner. See
			// legLocalReadsOf for why the dotted zero above no longer covers
			// this arm.
			reads := legLocalReadsOf(plan)
			// LOGGED, not asserted, and not written into prose. The per-shape
			// population moves with any planner change that rewrites one of
			// these winners, and a comment stating it was wrong within one
			// change of being written. The run reports it; the assertion below
			// is on the only property that has to hold — that it is not zero
			// across all shapes.
			t.Logf("leg-local identity sentinel: %d qualifying read(s) in the winner", len(reads))
			for _, r := range reads {
				t.Logf("  %s", r.describe())
			}
			for _, r := range reads {
				if !r.identityOK {
					t.Errorf("a leg-correlated read reached the WINNING plan without an "+
						"identity in its OWN leg's domain: %s\n\n"+
						"  The read is a single-accessor column off a quantifier that STATES "+
						"a row, which is exactly the shape the rebase arm's pass-through "+
						"hands back — so its ordinal is about to be resolved against that "+
						"quantifier's window at runtime. An ordinal whose domain is not that "+
						"window's indexes a DIFFERENT layout, and the failure is silent: the "+
						"read returns whatever sits at that slot in the wrong row.\n\n"+
						"  Do NOT relax this into a domain-agnostic check. The domain token "+
						"is the only thing separating a leg-local ordinal from a merged-row "+
						"ordinal that happens to be in range.\n  explain:\n%s",
						r.describe(), explain)
				}
				sentinelPopulation.Add(1)
			}
		})
	}

	// A zero-population sentinel is a green that says nothing, and this one is
	// especially prone to it: the qualifying shape (single accessor, typed
	// quantifier) is exactly the shape a planner change can rewrite away. Every
	// shape carries some today and none carries a fixed number — the per-shape
	// counts are LOGGED by each subtest rather than stated here, because they move
	// with any planner change that rewrites one of these winners and a stated
	// count is wrong from the first such change onward. What the assertion needs
	// is only that the population is not empty; that is what it checks.
	//
	// WHAT A GREEN HERE DOES NOT MEAN, and it is the sharper half. The qualifying
	// reads reaching these winners did NOT come through the rebase arm's
	// pass-through: renaming arm 2's output leaves every count and every assertion
	// in this test unchanged, while making that arm panic proves it IS reached
	// during planning — so its product is built and then loses, and what the
	// sentinel is walking is reads that arrived at the winner by other routes. The
	// sentinel keys on SHAPE, not on provenance, so an arm-2 read that ever DID
	// win would be checked by it; but a green today is not evidence that arm 2's
	// output is correct. That evidence is at rule level
	// (TestRebaseOuterLegValue_PassesThroughAnAlreadyCorrectLegLocalRead,
	// TestRebaseOuterLegValue_DerivableLegKeepsTheLegLocalRead), and this is the
	// standing watch for the day the candidate stops losing.
	t.Cleanup(func() {
		if n := sentinelPopulation.Load(); n == 0 {
			t.Errorf("the leg-local identity sentinel found NO qualifying read in any " +
				"winning plan.\n" +
				"  It asserts a property of leg-correlated reads that reach execution; " +
				"over an empty population it asserts nothing, and reports green.\n" +
				"  Either the shapes stopped producing a leg-local read (find where the " +
				"reads went — a merged two-accessor re-anchor is fine and expected, an " +
				"untyped quantifier is not) or the walk stopped finding them.")
		}
	})
}

// legLocalRead is one candidate leg-local read found in a winning plan, plus
// whether its identity answers.
type legLocalRead struct {
	field      string
	corr       string
	ordinal    int
	pathDomain values.OrdinalDomain
	legDomain  values.OrdinalDomain
	identityOK bool
}

func (r legLocalRead) describe() string {
	return fmt.Sprintf("%s.%s#%d — path domain %v, quantifier domain %v",
		r.corr, r.field, r.ordinal, r.pathDomain, r.legDomain)
}

// legLocalReadsOf collects every read in a winning plan that has the shape the
// rebase arm's PASS-THROUGH hands back: a SINGLE-accessor column read off a
// QuantifiedObjectValue that STATES A ROW.
//
// WHY THIS EXISTS BESIDE THE DOTTED ZERO. The dotted zero measures the absence
// of the deleted mint's product. That mint is gone, and the arm's live output is
// something else entirely — a read that keeps its own leg correlation and its own
// leg-local ordinal. So the zero is now structurally decoupled from the arm: it
// would stay at zero whether the pass-through were correct, wrong, or absent, and
// on the day an arm-2 candidate wins there would be nothing watching what it
// carries. This is what watches it.
//
// The two DISQUALIFIERS are the content, and both are correct forms rather than
// residues:
//
//   - a FUSED multi-accessor path (`#leg#col`) is a read against the MERGED
//     quantifier, Java's own re-anchor. Its ordinals index the merge layout, not
//     a leg's, so asking for a leg-domain identity would red on a correct plan.
//     Measured on the self-join-twin shape, where both leg reads take this form.
//   - a quantifier that states NO row has no domain to compare against — the
//     existential inner's scan-bound read is this. A read there is not a
//     leg-local read; it is a bare correlated read whose binder is the
//     existential's own.
//
// What remains is exactly the population whose ordinal is about to be resolved
// against the leg window its quantifier names, and the only thing that can be
// wrong about one is that its ordinal belongs to a different layout.
func legLocalReadsOf(plan plans.RecordQueryPlan) []legLocalRead {
	var out []legLocalRead
	visit := func(v values.Value) values.Value {
		fv, ok := values.AsFieldValue(v)
		if !ok {
			return v
		}
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV {
			return v
		}
		legDomain := values.OrdinalDomainOfType(qov.FlowedType())
		if !legDomain.IsKnown() {
			return v
		}
		if fv.Path() == nil {
			return v
		}
		if fv.Path().Len() != 1 {
			return v
		}
		id, identityOK := values.CorrelatedFieldIdentityIn(v, legDomain)
		// The identity must also be IN the domain it was asked about — a
		// ColumnIdentity carries its domain, and a match on the ordinal alone
		// would accept a slot number that means something else here.
		if identityOK && id.Domain != legDomain {
			identityOK = false
		}
		out = append(out, legLocalRead{
			field:      fv.DisplayName(),
			corr:       qov.Correlation().Name(),
			ordinal:    fv.Path().Ordinals()[0],
			pathDomain: fv.Path().RootDomain(),
			legDomain:  legDomain,
			identityOK: identityOK,
		})
		return v
	}
	walkPlanValues(plan, visit)
	return out
}

// dottedMergedRowKeysOf collects every dotted FieldValue over a
// QuantifiedObjectValue in plan, rendered as "FIELD@CORR". That is the exact
// signature of the leg-rebase arm's mint; no other producer in these shapes
// emits a dotted Field, since every table here declares only flat top-level
// columns.
func dottedMergedRowKeysOf(plan plans.RecordQueryPlan) []string {
	var out []string
	walkPlanValues(plan, func(v values.Value) values.Value {
		if fv, ok := values.AsFieldValue(v); ok && strings.Contains(fv.DisplayName(), ".") {
			if q, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
				out = append(out, fmt.Sprintf("%s@%s", fv.DisplayName(), q.Correlation().Name()))
			}
		}
		return v
	})
	return out
}

// walkPlanValues applies visit to every Value surface a rebased reference can
// land on: each node's result value, the three predicate-carrying plan kinds,
// and scan/index bounds.
//
// It is ONE function rather than a copy per collector so the negative walk (the
// dotted zero) and the positive one (the identity sentinel) cannot come to cover
// different surfaces. A surface missing here makes both of them weaker in the
// same direction, which is at least a visible fact; two walks drifting apart
// would make the zero look stronger than the sentinel for no stated reason.
func walkPlanValues(plan plans.RecordQueryPlan, visit func(values.Value) values.Value) {
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
}
