package rowdiff

import (
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// THE FALSE-POSITIVE-FREEDOM CLAIM, PINNED IN BOTH DIRECTIONS.
//
// The header asserts this axis never raises a false alarm. That claim went
// unmeasured for a week while the axis produced 12 false positives out of 16
// reported occurrences, so it is pinned here rather than asserted there.
//
// Every case is a REAL planner-produced plan via embedded.PlanPhysicalForTest —
// no FDB, no containers, no hand-built plan trees. Hand-built trees would not
// exercise the fix at all: they carry empty GetKeyComponentTypes(), so
// values.TypeTerminatesOrderingClaim is never consulted and the float cases
// silently pass for the wrong reason.
//
// THE SILENT CASES ARE THE REGRESSION GUARD. Each was a real false positive,
// and each fails for its own reason:
//
//   - 3943193 / 3944227: a ZERO-VALUED FLOAT EQUALITY. The executor widens it
//     across both signed-zero blocks, so the PK suffix RESTARTS mid-scan and is
//     not ordered. plans.EqualityPinsSinglePhysicalKey answers false; the old
//     hand-rolled leadingEqualityCount counted it as pinning, dropped the
//     column, and claimed the suffix was ordered.
//   - 3943308: a FLOAT INEQUALITY over the whole non-null domain, which reaches
//     BOTH NaN blocks. values.TypeTerminatesOrderingClaim answers true; the old
//     walk never asked.
//
// THE FIRING DIRECTION IS NOT HERE, AND THAT IS A MEASURED RESULT RATHER THAN AN
// OMISSION. It needs a plan the CANONICAL rules say keeps an unnecessary sort,
// and no real SQL over this generator's schema produces one — which is good news
// about the engine, not a gap in the pin. The nearest candidate,
// `WHERE a = 7 ORDER BY id`, plans as IndexScan(IDX_AB, [=, *]): the index has a
// SECOND key column B that the query leaves unbound, so the scan provides
// (B, ID) order and the sort on ID is genuinely required. The axis correctly
// stays silent, and a case that fires would be an engine bug we do not currently
// have.
//
// The firing direction is pinned by TestCheckPlanOrdering_RedundantSort, which
// builds plan trees by hand and can therefore construct a single-key index the
// planner never chooses. Note what that costs and why both tests are needed:
// hand-built plans carry EMPTY GetKeyComponentTypes(), so
// values.TypeTerminatesOrderingClaim is never consulted there and the float
// cases would pass for the wrong reason. That test proves the axis still FIRES;
// this one proves the canonical predicates are actually reached.
func TestOrderingAxisIsFalsePositiveFree(t *testing.T) {
	ddl := Generate(3943193).DDL()

	for _, tc := range []struct {
		name string
		sql  string
		want bool // true = MUST report, false = MUST stay silent
		why  string
	}{
		{
			name: "zero float equality does not pin the PK suffix (seed 3943193)",
			sql:  "SELECT * FROM t_rd WHERE (b >= 3) AND (s LIKE 'al%') AND (e = 0) ORDER BY id LIMIT 11",
			why: "e = 0 widens across both signed-zero blocks, so ID restarts at the block " +
				"boundary and the scan does NOT provide ID order",
		},
		{
			name: "zero float equality, unfiltered (seed 3944227)",
			sql:  "SELECT * FROM t_rd WHERE d = 0.0 ORDER BY id",
			why:  "same shape as above with no residual filter",
		},
		{
			name: "float inequality spanning both NaN blocks (seed 3943308)",
			sql: "SELECT * FROM t_rd WHERE (s <= 'beta') AND (f = TRUE) AND (d IS NOT NULL) " +
				"ORDER BY d NULLS FIRST, id LIMIT 7 OFFSET 3",
			why: "D is a float over the whole non-null domain: key order and CompareFloat64 " +
				"order disagree, so D claims no order and neither does anything after it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := embedded.PlanPhysicalForTest(tc.sql, ddl, nil)
			if err != nil {
				t.Fatalf("plan %q: %v", tc.sql, err)
			}
			q := Query{OrderBy: parseOrderByForPin(tc.sql)}
			got := checkPlanOrdering(plan, q)

			if tc.want && len(got) == 0 {
				t.Fatalf("axis stayed SILENT on a genuinely redundant sort.\n  sql:  %s\n  plan: %s\n  %s\n"+
					"  This is the firing direction. If it goes silent the axis has stopped "+
					"detecting anything and the three cases below it pass vacuously — a "+
					"detector that never fires is trivially false-positive-free.",
					tc.sql, plan.Explain(), tc.why)
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("axis raised a FALSE POSITIVE.\n  sql:  %s\n  plan: %s\n  finding: %v\n  %s\n"+
					"  The header claims this axis never raises a false alarm in a nightly net. "+
					"That claim was unmeasured while 12 of 16 reported occurrences were false, "+
					"and it is this test's whole job. Do NOT relax it — consult the canonical "+
					"predicates (plans.EqualityPinsSinglePhysicalKey, "+
					"values.TypeTerminatesOrderingClaim) rather than re-deriving their rules here.",
					tc.sql, plan.Explain(), got, tc.why)
			}
		})
	}
}

// parseOrderByForPin builds the ORDER BY key list the detector compares against.
// Kept deliberately literal — the pin states its own expectation rather than
// re-using generator internals that could drift with it.
func parseOrderByForPin(sql string) []OrderKey {
	switch {
	case contains(sql, "ORDER BY d NULLS FIRST, id"):
		return []OrderKey{{Col: "D", Nulls: NullsFirst}, {Col: "ID"}}
	default:
		return []OrderKey{{Col: "ID"}}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
