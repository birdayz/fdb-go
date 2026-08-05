package cascades

// The rule's LICENSE ORDER — PropDistinctRecords and primary-key coverage are
// consulted BEFORE the secondary-UNIQUE proof — is a correctness requirement,
// not tidiness: a stamp records a dependency the plan's correctness rests on,
// and a plan already licensed by an unconditional fact does not rest on a
// mutable index's state. Stamping it anyway turns an unrelated index build into
// a 40001 on a statement that would have been correct regardless.
//
// TestDistinctFinal_BothLicensesHoldYieldsUnstampedPlan states that for the
// PRIMARY-KEY license, and cannot state it for the PROPERTY license: the rule
// computes the secondary proof only when the PK license is absent, so on that
// fixture the two are mutually exclusive and reordering them changes nothing
// observable. It also runs on the !handled fallback, where no partition exists
// at all.
//
// The PROPERTY license is independent of the PK license — it is a fact about the
// physical plan, computed per expression inside the partition loop — so it is
// the only one that can hold AT THE SAME TIME as a firing secondary proof. That
// coincidence is what makes the order load-bearing, and it lives on the
// partition path, which is the path this file exercises.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// recordIdentityScanFor is a physical scan whose record identity IS logical row
// identity: one record type, and a primary key whose physical uniqueness is its
// logical uniqueness. makeFakePlanWrapper states neither, so it can never carry
// the property license and cannot express this fixture.
func recordIdentityScanFor(recType string) *plans.RecordQueryScanPlan {
	return plans.NewRecordQueryScanPlan(
		[]string{recType}, distinctScanType(recType), false).
		WithPrimaryKey([]values.Value{
			values.NewFieldValueWithResolvedOrdinal("PK", 0, values.NullableLong),
		}).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
}

// TestDistinctFinal_PropertyLicenseOnPartitionPathYieldsUnstampedPlan is the
// fixture the license order is actually observable on.
//
// Every precondition is asserted rather than assumed, because each one silently
// makes the test vacuous if it stops holding: the partition must exist and be
// StoredRecord (otherwise the rule takes the !handled fallback and the partition
// loop is never entered), the property license must actually hold on the
// expression, and the secondary proof must fully elide ON ITS OWN (otherwise
// "unstamped" is trivially satisfied by there being nothing to stamp).
func TestDistinctFinal_PropertyLicenseOnPartitionPathYieldsUnstampedPlan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, distinctScanType("T"))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{distinctRead("T", "EMAIL")}, scanQ)
	projRef := expressions.InitialOf(proj)
	projRef.Insert(recordIdentityScanFor("T"))
	computeRefPlanProperties(projRef)

	partitions := ToPlanPartitions(projRef)
	if len(partitions) == 0 {
		t.Fatal("no partitions: the rule would take the !handled fallback and the " +
			"partition path this test exists for would not be entered")
	}
	licensed := false
	for _, partition := range partitions {
		if !partition.GetPartitionPropertiesMap().GetBool(properties.PropStoredRecord) {
			continue
		}
		for _, expr := range partition.GetExpressions() {
			if partition.IsDistinct() && expressionRecordIdentityMatchesLogicalEquality(expr) {
				licensed = true
			}
		}
	}
	if !licensed {
		t.Fatal("the PROPERTY license does not hold on any partitioned expression, " +
			"so this fixture cannot observe the order it was written for")
	}

	// The PK license must be ABSENT — with it, the rule never computes the
	// secondary proof and the two arms cannot coincide.
	ctx := &indexTestPlanContext{
		candidates:      secondaryUniqueTestCandidates(),
		readableIndexes: AllIndexesReadable(),
	}
	if distinctEliminatedByUniqueKey(proj, ctx) {
		t.Fatal("the PK license holds, which short-circuits the secondary proof; " +
			"this fixture requires the PROPERTY license alone")
	}
	if p := secondaryUniqueEliminationProof(proj, ctx); !p.FullElision ||
		p.IndexName != "T$email_unique" {
		t.Fatalf("the secondary-UNIQUE proof does not fully elide on its own "+
			"(%q, elision=%v), so asserting it does not STAMP proves nothing",
			p.IndexName, p.FullElision)
	}

	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(projRef))
	results := FireImplementationRuleWithContext(
		NewImplementDistinctFinalRule(), expressions.InitialOf(distinct), ctx, nil)
	if len(results) == 0 {
		t.Fatal("rule did not fire at all")
	}
	for _, result := range results {
		if _, kept := result.(*plans.RecordQueryDistinctPlan); kept {
			t.Fatal("DISTINCT was retained even though the PROPERTY license holds")
		}
		stamped, ok := result.(plans.DistinctProofStamped)
		if !ok {
			continue
		}
		if name := stamped.GetDistinctProofIndexName(); name != "" {
			t.Fatalf("the elision was licensed by PropDistinctRecords and still "+
				"recorded a dependency on index %q. That index's state can move "+
				"under a live statement; the property cannot, so transitioning it "+
				"would 40001 a statement whose correctness never rested on it", name)
		}
	}
}
