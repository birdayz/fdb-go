package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func dmlDedupRowType() *values.RecordType {
	return values.NewRecordType("Order", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 1},
	})
}

func dmlDedupFullScan(
	t testing.TB,
	rowType values.Type,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"Order"}, rowType)
	return mustConstruct(t, scan, err)
}

func dmlDedupScanPlan(
	t testing.TB,
	rowType values.Type,
) *plans.RecordQueryScanPlan {
	t.Helper()
	scan, err := plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false)
	return mustConstruct(t, scan, err)
}

func dmlDedupDelete(
	t testing.TB,
	inner expressions.Quantifier,
) *expressions.DeleteExpression {
	t.Helper()
	del, err := expressions.NewDeleteExpression(inner, "Order")
	return mustConstruct(t, del, err)
}

func dmlDedupMustFireExpressionRule(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	yielded, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule() unexpected error: %v", err)
	}
	return yielded
}

// The DML implementation rules interpose a primary-key dedup between the access
// path and the mutation. Every arm of that decision is driven here rather than
// left to whichever shapes the corpus happens to reach, because the arms are
// asymmetric between the two rules and the corpus exercises only some of them:
//
//   - UPDATE dedups UNCONDITIONALLY (ImplementUpdateRule.java:79-80);
//   - DELETE dedups only when the access path does not already prove
//     DistinctRecordsProperty.distinctRecords() (ImplementDeleteRule.java:79-82);
//   - both bind the inner only through StoredRecordProperty partitions
//     (ImplementUpdateRule.java:54-57, ImplementDeleteRule.java:55-57).
//
// The DELETE-dedups arm in particular is unreached by the whole yamsql corpus:
// every DML access path there is distinct, so the corpus alone would let that
// arm rot untested until the first access path that is not.

// dedupInnerOf returns the plan under a DML plan's primary-key dedup, and nil
// when the DML plan's inner is not one.
func dedupInnerOf(inner plans.RecordQueryPlan) plans.RecordQueryPlan {
	d, ok := inner.(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
	if !ok {
		return nil
	}
	return d.GetInner()
}

func TestImplementUpdateRule_DedupsEvenOverADistinctAccessPath(t *testing.T) {
	t.Parallel()
	rowType := dmlDedupRowType()
	scan := dmlDedupFullScan(t, rowType)
	innerRef := expressions.InitialOf(scan)
	upd, err := expressions.NewUpdateExpression(
		expressions.ForEachQuantifier(innerRef), "Order", rowType, nil)
	upd = mustConstruct(t, upd, err)
	topRef := expressions.InitialOf(upd)

	dmlDedupMustFireExpressionRule(t, NewPrimaryScanRule(), innerRef)

	// A primary scan IS distinct — this is precisely the case a DELETE elides
	// and an UPDATE does not. Asserting it here is what keeps the two rules
	// from being "simplified" into one.
	for _, m := range innerRef.AllMembers() {
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}
		if !computeWrapperProperties(ph).GetBool(properties.PropDistinctRecords) {
			t.Fatalf("premise broken: a primary scan must report DistinctRecords, "+
				"otherwise this test no longer distinguishes the unconditional "+
				"UPDATE arm from the conditional DELETE arm (%T)", m)
		}
	}

	yielded := dmlDedupMustFireExpressionRule(t, NewImplementUpdateRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUpdateRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryUpdatePlan)
	logicalType := upd.GetResultValue().Type()
	physicalType := plan.GetResultType()
	if physicalType == nil || !physicalType.Equals(logicalType) {
		t.Fatalf("UPDATE plan result type = %v, want exact logical type %v",
			physicalType, logicalType)
	}
	resultRow, ok := physicalType.(*values.RecordType)
	if !ok || len(resultRow.Fields) != 2 ||
		resultRow.Fields[0].Name != "OLD" || resultRow.Fields[1].Name != "NEW" {
		t.Fatalf("UPDATE plan result = %v, want exact two-field {OLD, NEW} row", physicalType)
	}
	inner := dedupInnerOf(plan.GetInner())
	if inner == nil {
		t.Fatalf("UPDATE inner = %T, want the primary-key dedup Java inserts "+
			"unconditionally (ImplementUpdateRule.java:79-80)", plan.GetInner())
	}
	if _, ok := inner.(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("dedup inner = %T, want *RecordQueryScanPlan", inner)
	}
}

func TestImplementDeleteRule_ElidesDedupOverADistinctAccessPath(t *testing.T) {
	t.Parallel()
	rowType := dmlDedupRowType()
	scan := dmlDedupFullScan(t, rowType)
	innerRef := expressions.InitialOf(scan)
	del := dmlDedupDelete(t, expressions.ForEachQuantifier(innerRef))
	topRef := expressions.InitialOf(del)

	dmlDedupMustFireExpressionRule(t, NewPrimaryScanRule(), innerRef)

	yielded := dmlDedupMustFireExpressionRule(t, NewImplementDeleteRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDeleteRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryDeletePlan)
	if _, isDedup := plan.GetInner().(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan); isDedup {
		t.Fatalf("DELETE over a DistinctRecords access path kept a primary-key " +
			"dedup; Java's ImplementDeleteRule short-circuits it (:79-82) and " +
			"only ImplementUpdateRule is unconditional")
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("DELETE inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementDeleteRule_DedupsOverANonDistinctAccessPath drives the DELETE
// arm the yamsql corpus does not reach: every DML access path the SQL surface
// builds today reports DistinctRecords, so the short-circuit above always wins
// and deleting the dedup call from the rule entirely would keep the suite
// green. A projection is stored-record-flowing (its child is) and NOT
// distinct-records (two records can project to one tuple), which is exactly the
// combination the arm exists for.
func TestImplementDeleteRule_DedupsOverANonDistinctAccessPath(t *testing.T) {
	t.Parallel()
	rowType := dmlDedupRowType()
	scanPlan := dmlDedupScanPlan(t, rowType)
	proj, err := plans.NewRecordQueryProjectionPlan(nil, scanPlan)
	proj = mustConstruct(t, proj, err)
	innerRef := expressions.FinalOfAtStage(proj, expressions.StageCanonical)
	computeRefPlanProperties(innerRef)

	// Both halves of the premise, so a property change cannot make the
	// assertion below hold for the wrong reason.
	props := computeWrapperProperties(proj)
	if !props.GetBool(properties.PropStoredRecord) {
		t.Fatalf("premise broken: a projection over a scan must flow stored " +
			"records, else it is filtered out before the dedup decision is reached")
	}
	if props.GetBool(properties.PropDistinctRecords) {
		t.Fatalf("premise broken: a projection must NOT report DistinctRecords, " +
			"else this test drives the short-circuit arm instead of the dedup arm")
	}

	del := dmlDedupDelete(t, expressions.ForEachQuantifier(innerRef))
	yielded := dmlDedupMustFireExpressionRule(t, NewImplementDeleteRule(), expressions.InitialOf(del))
	if len(yielded) != 1 {
		t.Fatalf("ImplementDeleteRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryDeletePlan)
	if dedupInnerOf(plan.GetInner()) == nil {
		t.Fatalf("DELETE inner = %T over a NON-distinct access path, want a "+
			"primary-key dedup (ImplementDeleteRule.java:79-82 inserts one "+
			"whenever DistinctRecordsProperty does not already hold)",
			plan.GetInner())
	}
}

// TestStoredRecordDMLCandidates_ExcludesNonStoredAccessPaths drives the
// partition filter directly. Its arm is the one no corpus query reaches: every
// DML access path the SQL surface can build today flows stored records, so the
// filter only ever admits. A rule change that dropped the filter entirely would
// therefore pass the whole suite.
func TestStoredRecordDMLCandidates_ExcludesNonStoredAccessPaths(t *testing.T) {
	t.Parallel()
	rowType := dmlDedupRowType()
	scan := dmlDedupFullScan(t, rowType)
	innerRef := expressions.InitialOf(scan)
	dmlDedupMustFireExpressionRule(t, NewPrimaryScanRule(), innerRef)

	admitted := storedRecordDMLCandidates(innerRef)
	if len(admitted) == 0 {
		t.Fatalf("a primary scan was not admitted as a DML access path; the " +
			"exclusion assertion below would then hold vacuously")
	}
	for _, c := range admitted {
		ph, ok := c.expr.(physicalPlanExpression)
		if !ok {
			t.Fatalf("admitted a non-physical member %T", c.expr)
		}
		if !computeWrapperProperties(ph).GetBool(properties.PropStoredRecord) {
			t.Fatalf("admitted %T, whose StoredRecordProperty is false — the "+
				"partition filter Java binds on (ImplementUpdateRule.java:54-57) "+
				"is not being applied", c.expr)
		}
	}

	// The excluded member must be PHYSICAL, or the exclusion proves nothing:
	// physicalMembersForParentEnumeration already drops non-physical members,
	// so a logical-only reference yields zero candidates whether the
	// StoredRecordProperty filter is applied or not. (Checked: with the filter
	// deleted, a logical explode reference still returns zero.)
	//
	// A first-or-default plan is physical and reports StoredRecord false — Java
	// visitFirstOrDefaultPlan returns false (StoredRecordProperty.java:265-266)
	// — because it substitutes a default row for an empty stream and that row
	// is not a stored record. A mutation over it has nothing to write back.
	notStored, err := plans.NewRecordQueryFirstOrDefaultPlan(
		dmlDedupScanPlan(t, rowType), nil)
	notStored = mustConstruct(t, notStored, err)
	notStoredRef := expressions.FinalOfAtStage(notStored, expressions.StageCanonical)
	computeRefPlanProperties(notStoredRef)

	if len(physicalMembersForParentEnumeration(notStoredRef)) == 0 {
		t.Fatalf("the exclusion case holds VACUOUSLY: the reference offers no " +
			"physical member at all, so the filter is not what rejected it")
	}
	if got := storedRecordDMLCandidates(notStoredRef); len(got) != 0 {
		t.Fatalf("storedRecordDMLCandidates admitted %d candidate(s) whose "+
			"StoredRecordProperty is false; Java's matcher binds only "+
			"filterPlanPartitions(StoredRecordProperty.storedRecord()) "+
			"(ImplementUpdateRule.java:54-57) so such a partition never reaches "+
			"the rule", len(got))
	}
}
