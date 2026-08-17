package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

const derivedExistsSchema = `CREATE TABLE t1 (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))
CREATE TABLE t3 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))`

// EXISTS correlated to a DERIVED SOURCE — a WITH-CTE leg or a derived table —
// plans, in the SELECT list and in the WHERE alike, joined and unjoined.
//
// The matrix is the point, because the three defects it covers presented as ONE
// symptom (a projected EXISTS over a derived source fails) and were three
// unrelated causes; the arms that separated them are the ones that look
// redundant:
//
//   - the CTE arms failed 42703 "no FROM source aliased as C" because the
//     subquery's OUTER SCOPE registered every real leg and no WITH leg. The
//     where_exists arm is what showed this had nothing to do with projection.
//   - the derived-table PROJECTED arms planned and then died in the executor,
//     because the join's step-1 ordinal seed refused a PROJECTION leg. The
//     base-table twin is what showed it was the leg SHAPE.
//   - the derived-table WHERE arms lost their plan entirely once a projection
//     began stating its row: the orientation gate compared the seed's unstated
//     window types against the leg's newly stated ones and declined both
//     orientations. Nothing else in this tree covered that shape.
//
// Planning only — the rows are asserted end-to-end against real FDB by
// TestFDB_ProjectionResultTypeProbe. This test is the fast sentinel: it runs in
// milliseconds and fails on the exact decision, where the FDB test proves the
// answer.
func TestDerivedSourceCorrelatedExistsPlans(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
		// wantNLJ requires the plan to materialize the two-leg join — the shape
		// whose merged row the correlated read addresses. Without it an arm could
		// "pass" through some degenerate plan that never exercises the seed.
		wantNLJ bool
	}{
		{"cte_projected_exists", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h
			FROM c, t3 WHERE t3.t1_id = c.id ORDER BY c.id`, true},
		{"cte_where_exists", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id, t1_id FROM c, t3
			WHERE t3.t1_id = c.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) ORDER BY c.id`, true},
		{"cte_projected_exists_unjoined", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h FROM c ORDER BY c.id`, false},
		{"cte_where_exists_unjoined", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id FROM c WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) ORDER BY c.id`, false},
		// The existential leg itself reads a CTE. This differs materially from
		// the four arms above, where the CTE is the outer leg: translating the
		// projected CTE row gives the inner scan a generated UNIQUE carrier.
		// Reconstructing that carrier from its q$ spelling as a NAMED alias makes
		// the correlation predicate miss the inner leg and routes it to the outer
		// filter, where the q$ binding does not exist. Keep the direct-table twin
		// below: it proves a regression is specific to the CTE's exact carrier,
		// not ordinary correlated-EXISTS lowering.
		{"cte_where_exists_inner_reads_cte", `WITH filtered AS (SELECT t1_id FROM t2 WHERE id > 1)
			SELECT t1.id FROM t1
			WHERE EXISTS (SELECT 1 FROM filtered f WHERE f.t1_id = t1.id) ORDER BY t1.id`, false},
		{"table_where_exists_inner_carrier_control", `SELECT t1.id FROM t1
			WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id AND t2.id > 1) ORDER BY t1.id`, false},

		{"derived_projected_exists", `SELECT d.id, t1_id,
			EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id) AS h
			FROM (SELECT id, v FROM t1) AS d, t3 WHERE t3.t1_id = d.id ORDER BY d.id`, true},
		{"derived_where_exists", `SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
			WHERE t3.t1_id = d.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id) ORDER BY d.id`, true},
		{"derived_where_exists_unordered", `SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
			WHERE t3.t1_id = d.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id)`, true},

		// The base-table twins. They passed throughout, and they are what
		// localized each defect to the derived leg rather than to EXISTS, to the
		// join, or to the ORDER BY.
		{"table_projected_exists", `SELECT t1.id, t3.t1_id,
			EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h
			FROM t1, t3 WHERE t3.t1_id = t1.id ORDER BY t1.id`, true},
		{"table_where_exists", `SELECT t1.id, t3.t1_id FROM t1, t3
			WHERE t3.t1_id = t1.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) ORDER BY t1.id`, true},
	}

	// A CTE that SHADOWS a same-named catalog table, correlated from inside an
	// EXISTS. The subquery's outer scope consults the CTE registry BEFORE the
	// catalog, in lockstep with the scope the query itself is resolved in — the
	// other order analyzes the TABLE's schema for reads that execute against the
	// CTE.
	//
	// The correlated column is chosen so the two orders give DIFFERENT answers:
	// the CTE named `t1` emits T1_ID (it selects from t3), and the base table t1
	// has no such column. A catalog-first outer scope therefore cannot resolve
	// `t1.t1_id` at all and the query dies 42703, so this arm fails loudly rather
	// than passing on a column both schemas happen to share.
	cases = append(cases, struct {
		name    string
		sql     string
		wantNLJ bool
	}{"cte_shadowing_a_real_table", `WITH t1 AS (SELECT id, t1_id FROM t3)
		SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.t1_id) AS h
		FROM t1 ORDER BY t1.id`, false})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, derivedExistsSchema, nil)
			if err != nil {
				t.Fatalf("plan failed: %T: %v\n  sql: %s", err, err, c.sql)
			}
			explain := plan.Explain()
			if c.name == "cte_where_exists_inner_reads_cte" {
				var flatMap *plans.RecordQueryFlatMapPlan
				var findFlatMap func(plans.RecordQueryPlan)
				findFlatMap = func(node plans.RecordQueryPlan) {
					if found, ok := node.(*plans.RecordQueryFlatMapPlan); ok {
						flatMap = found
						return
					}
					for _, child := range node.GetChildren() {
						if flatMap == nil {
							findFlatMap(child)
						}
					}
				}
				findFlatMap(plan)
				if flatMap == nil {
					t.Fatal("CTE-inner EXISTS plan has no FlatMap")
				}

				// CorrelationIdentifier equality includes the private kind. This
				// therefore pins a NAMED F carrier, not merely a matching string,
				// and proves the synthetic CTE definition name did not replace it.
				wantInner := values.NamedCorrelationIdentifier("F")
				definition := values.NamedCorrelationIdentifier("FILTERED")
				if got := flatMap.GetInnerAlias(); got != wantInner || got == definition {
					t.Fatalf("FlatMap inner identity = %#v, want exact main alias %#v (not definition %#v)",
						got, wantInner, definition)
				}
				if _, misplaced := flatMap.GetOuter().(*plans.RecordQueryPredicatesFilterPlan); misplaced {
					t.Fatal("CTE correlation predicate was routed to the outer filter")
				}

				wantInnerType := values.NewRecordType("", false, []values.Field{{
					Name: "T1_ID", FieldType: values.NullableLong,
				}})
				foundExactCarrierRead := false
				var inspectInner func(plans.RecordQueryPlan)
				inspectInner = func(node plans.RecordQueryPlan) {
					if filter, ok := node.(*plans.RecordQueryPredicatesFilterPlan); ok {
						inputLayout, layoutErr := filter.GetInner().ProvidedOutputLayout()
						if layoutErr != nil {
							t.Fatalf("inner filter layout: %v", layoutErr)
						}
						for _, predicate := range filter.GetPredicates() {
							comparison, ok := predicate.(*predicates.ComparisonPredicate)
							if !ok {
								continue
							}
							for _, operand := range []values.Value{comparison.Operand, comparison.Comparison.Operand} {
								field, isField := values.AsFieldValue(operand)
								if !isField {
									continue
								}
								root, isQOV := values.AsQuantifiedObjectValue(field.ChildValue())
								if !isQOV || root.Correlation() != values.CurrentCorrelation() ||
									!root.FlowedType().Equals(wantInnerType) {
									continue
								}
								if root != inputLayout.Carrier() {
									t.Fatalf("inner T1_ID root = %p, want exact selected carrier %p",
										root, inputLayout.Carrier())
								}
								if !field.ResultType().Equals(values.NullableLong) {
									t.Fatalf("inner T1_ID result type = %s, want %s", field.ResultType(), values.NullableLong)
								}
								ordinals := field.Path().Ordinals()
								if len(ordinals) != 1 || ordinals[0] != 0 {
									t.Fatalf("inner T1_ID path = %v, want exact ordinal [0]", ordinals)
								}
								foundExactCarrierRead = true
							}
							if _, wrong := comparison.GetCorrelatedTo()[definition]; wrong {
								t.Fatalf("comparison retained CTE definition identity: %s", comparison.Explain())
							}
						}
					}
					for _, child := range node.GetChildren() {
						inspectInner(child)
					}
				}
				inspectInner(flatMap.GetInner())
				if !foundExactCarrierRead {
					t.Fatal("no exact selected-carrier T1_ID read below the existential FirstOrDefault")
				}
			}
			// A correlated EXISTS lowers to a FlatMap over the existential leg.
			// Without this the arm would stay green on a plan that answered the
			// query some other way and exercised none of the three decisions.
			if !strings.Contains(explain, "FlatMap") {
				t.Fatalf("no FlatMap in the plan — the correlated EXISTS did not lower to "+
					"the existential fold, so this arm witnesses nothing:\n  %s", explain)
			}
			if c.wantNLJ && !strings.Contains(explain, "NestedLoopJoin") {
				t.Fatalf("no NestedLoopJoin in the plan — the two-leg join whose merged row "+
					"the correlated read addresses is what these arms exist to cover:\n  %s", explain)
			}
		})
	}
}

// The three-quantifier JOIN+EXISTS implementation receives the existential
// source's rendered alias separately from its quantifier. A generated UNIQUE
// alias and a reconstructed NAMED alias can therefore both print as q$N while
// remaining different runtime bindings. Keep the exact quantifier identity:
// otherwise the correlation predicate is mistaken for an outer-join predicate
// and FirstOrDefault folds an unfiltered, globally non-empty table.
func TestThreeLegExistsKeepsExactExistentialAlias(t *testing.T) {
	t.Parallel()

	plan, err := PlanPhysicalForTest(`WITH c AS (SELECT id, v FROM t1)
		SELECT c.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h
		FROM c, t3 WHERE t3.t1_id = c.id ORDER BY c.id`, derivedExistsSchema, nil)
	if err != nil {
		t.Fatalf("plan failed: %T: %v", err, err)
	}

	var flatMap *plans.RecordQueryFlatMapPlan
	var findFlatMap func(plans.RecordQueryPlan)
	findFlatMap = func(node plans.RecordQueryPlan) {
		if found, ok := node.(*plans.RecordQueryFlatMapPlan); ok {
			flatMap = found
			return
		}
		for _, child := range node.GetChildren() {
			if flatMap == nil {
				findFlatMap(child)
			}
		}
	}
	findFlatMap(plan)
	if flatMap == nil {
		t.Fatal("three-leg correlated EXISTS plan has no FlatMap")
	}

	exactInner := flatMap.GetInnerAlias()
	namedTwin := values.NamedCorrelationIdentifier(exactInner.Name())
	if exactInner == namedTwin {
		t.Fatalf("existential binding %#v was reconstructed from its q$ spelling; want the exact generated quantifier identity", exactInner)
	}

	var join *plans.RecordQueryNestedLoopJoinPlan
	var findJoin func(plans.RecordQueryPlan)
	findJoin = func(node plans.RecordQueryPlan) {
		if found, ok := node.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			join = found
			return
		}
		for _, child := range node.GetChildren() {
			if join == nil {
				findJoin(child)
			}
		}
	}
	findJoin(flatMap.GetOuter())
	if join == nil {
		t.Fatal("three-leg correlated EXISTS plan has no outer NestedLoopJoin")
	}
	for _, predicate := range join.GetPredicates() {
		if _, misplaced := predicate.GetCorrelatedTo()[exactInner]; misplaced {
			t.Fatalf("existential predicate was routed to the outer join: %s", predicate.Explain())
		}
	}

	var firstOrDefault *plans.RecordQueryFirstOrDefaultPlan
	var findFirstOrDefault func(plans.RecordQueryPlan)
	findFirstOrDefault = func(node plans.RecordQueryPlan) {
		if found, ok := node.(*plans.RecordQueryFirstOrDefaultPlan); ok {
			firstOrDefault = found
			return
		}
		for _, child := range node.GetChildren() {
			if firstOrDefault == nil {
				findFirstOrDefault(child)
			}
		}
	}
	findFirstOrDefault(flatMap.GetInner())
	if firstOrDefault == nil {
		t.Fatal("existential FlatMap inner has no FirstOrDefault")
	}

	filter, ok := firstOrDefault.GetInner().(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("FirstOrDefault folded an unfiltered existential leg: %s", firstOrDefault.Explain())
	}
	foundExactCorrelation := false
	for _, predicate := range filter.GetPredicates() {
		correlatedTo := predicate.GetCorrelatedTo()
		if _, readsNamedTwin := correlatedTo[namedTwin]; readsNamedTwin {
			t.Fatalf("predicate below FirstOrDefault reads same-spelled NAMED alias %#v: %s", namedTwin, predicate.Explain())
		}
		if _, readsOuter := correlatedTo[flatMap.GetOuterAlias()]; !readsOuter {
			continue
		}
		for correlation := range correlatedTo {
			if correlation == flatMap.GetOuterAlias() {
				continue
			}
			// Extraction may relink the inner read from the logical EXISTS
			// quantifier to the selected scan edge. That edge must still be an
			// exact generated identifier, never another reconstruction from text.
			if correlation == values.NamedCorrelationIdentifier(correlation.Name()) {
				t.Fatalf("predicate below FirstOrDefault reconstructed inner-row alias from text: %s", predicate.Explain())
			}
			foundExactCorrelation = true
		}
	}
	if !foundExactCorrelation {
		t.Fatal("no predicate below FirstOrDefault references the exact inner-row and merged-outer bindings")
	}
}
