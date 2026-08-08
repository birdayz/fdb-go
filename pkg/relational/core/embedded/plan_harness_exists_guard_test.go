package embedded

import (
	"strings"
	"testing"
)

// nestedStructExistsSchema is the RFC-218 fold fixture: a struct-valued column
// so an ORDER BY can carry a MULTI-ACCESSOR path, plus a second table for the
// existential subquery and a third to join against.
const nestedStructExistsSchema = `
CREATE TYPE AS STRUCT NST (sk BIGINT, co BIGINT)
CREATE TABLE T1 (id BIGINT, n NST, PRIMARY KEY (id))
CREATE TABLE T2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))
CREATE TABLE T3 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))
`

// TestPlanPhysicalForTest_RunsTheProjectedExistsGuards pins that the SELECT
// plan harness REFUSES a projected-EXISTS shape production refuses.
//
// It calls PlanPhysicalForTest by name because that is the symbol the
// plan-shape corpus dump goes through: a pin that exercised the production
// generator instead would prove nothing about what the golden records.
//
// The gap this closes was invisible for as long as it existed. The harness ran
// FindUnsupportedFunction, findDistinctAggregate and ValidateCTEAliasArities
// but NEITHER half of the RFC-141 §8 projected-EXISTS guard, while its DML twin
// (planPhysicalDMLForTest) ran both. Nothing noticed because ZERO of the 2506
// corpus queries projected an EXISTS into the SELECT list, so no corpus query
// could reach the missing check. The harness consequently planned this query
// and emitted a plan whose projected ExistsValue sits where its existential
// binding is dead — the precise shape the guard exists to keep out — under a
// corpus stanza pinned to 0AF00.
// THE FIXTURE CHANGED WITH RFC-220, and the reason matters more than the query.
// This test used to drive the INNER-join nested-sort-key shape, which the fold
// declined because the leg-window re-anchor refused a multi-accessor path. That
// arm plans now, so the old fixture would have asserted a refusal that no longer
// exists — a test that reds on a capability being ADDED.
//
// Two shapes still hit the RFC-141 §8 guard for reasons unrelated to the
// re-anchor, and BOTH are driven, because a single fixture would leave this pin
// one capability-fix away from being wrong again:
//
//   - a LEFT source carrying any ORDER BY (Java's Cascades cannot plan it either)
//   - a COMPUTED key that is not among the projected outputs
func TestPlanPhysicalForTest_RunsTheProjectedExistsGuards(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, q string }{
		{
			"left_source_with_an_order_by",
			"SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 LEFT JOIN t3 ON t3.t1_id = t1.id ORDER BY t1.id",
		},
		{
			"computed_key_absent_from_the_projection",
			"SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 ORDER BY id + 1",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(tc.q, nestedStructExistsSchema, nil)
			if err == nil {
				t.Fatalf("the harness PLANNED a projected EXISTS the driver refuses with "+
					"0AF00: %v — the plan-shape golden would record a plan that cannot be "+
					"executed, and an error-pinned corpus stanza would render a bogus shape",
					plan)
			}
			if !strings.Contains(err.Error(), "projected EXISTS in this query shape is not yet supported") {
				t.Fatalf("refused for the wrong reason: %v — this must be the RFC-141 §8 "+
					"guard's message, the same one the driver returns, not an incidental "+
					"planning failure that happens to also be an error", err)
			}
		})
	}
}

// TestPlanPhysicalForTest_GuardDoesNotRefuseTheFoldableShapes is the CONTROL,
// and it is what makes the assertion above non-vacuous. A harness that refused
// every projected EXISTS would satisfy that test while destroying the corpus
// coverage this whole exercise exists to add.
//
// All three shapes are the single-table arm RFC-218 fixes: the fold threads the
// nested sort key through, so the guard must let them past.
func TestPlanPhysicalForTest_GuardDoesNotRefuseTheFoldableShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, q string }{
		{
			"nested_key_not_projected",
			"SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY n.sk",
		},
		{
			"struct_root_also_projected",
			"SELECT id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY n.sk",
		},
		{
			"struct_root_projected_under_an_alias",
			"SELECT id, n AS nn, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY n.sk",
		},
		{
			"single_accessor_key_already_in_output",
			"SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY id",
		},
		// RFC-220: the INNER-join nested-key arm. It moved from the refusal test
		// above to here, which is the whole point of keeping both lists — the
		// harness must track the driver in BOTH directions, and a shape that
		// starts planning has to be re-pinned as accepted rather than merely
		// deleted from the refusal side.
		{
			"nested_key_over_an_inner_join",
			"SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 JOIN t3 ON t3.t1_id = t1.id ORDER BY n.sk",
		},
		{
			"nested_key_over_an_inner_join_other_member",
			"SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 JOIN t3 ON t3.t1_id = t1.id ORDER BY n.co",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := PlanPhysicalForTest(tc.q, nestedStructExistsSchema, nil); err != nil {
				t.Fatalf("a FOLDABLE projected EXISTS was refused: %v — the guard is "+
					"over-broad, and the corpus entries this shape backs would all "+
					"collapse to <PLAN-ERROR>, which reads as coverage while being none",
					err)
			}
		})
	}
}
