package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestWholePlanMaxCardinalityKnown pins RFC-188 finding 10 M3: Java gates the
// data-access-cardinality criterion behind the PROVEN WHOLE-PLAN max
// cardinality (cardinalities().evaluate(plan).getMaxCardinality()), not the
// data-access maxima. The guard must DISCRIMINATE — false when the whole-plan
// bound is unknown (a bare scan), true when it is provably bounded (a
// FirstOrDefault produces exactly one row). If it were always false the
// criterion would never fire (broad regression); always true and the M3 gate
// would be a no-op that never abstains.
func TestWholePlanMaxCardinalityKnown(t *testing.T) {
	t.Parallel()

	// A bare scan has an unknown whole-plan max cardinality → guard abstains.
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if wholePlanMaxCardinalityKnown(scan) {
		t.Fatal("bare scan: whole-plan max cardinality must be unknown (unbounded)")
	}

	// FirstOrDefault produces exactly one row → provably bounded → guard engages.
	fod := plans.NewRecordQueryFirstOrDefaultPlanStrict(scan, values.NewNullValue(values.UnknownType))
	if !wholePlanMaxCardinalityKnown(fod) {
		t.Fatal("FirstOrDefault: whole-plan max cardinality is provably 1 (known)")
	}

	// Two bare scans → both unknown → the OUTER guard is false → criterion #2
	// abstains (Java's behavior; Go previously ranked on the data-access maxima
	// anyway). This is the exact condition the gate adds.
	scan2 := plans.NewRecordQueryScanPlan([]string{"U"}, values.UnknownType, false)
	if wholePlanMaxCardinalityKnown(scan) || wholePlanMaxCardinalityKnown(scan2) {
		t.Fatal("two unbounded scans: the whole-plan gate must be false → criterion #2 abstains")
	}
}
