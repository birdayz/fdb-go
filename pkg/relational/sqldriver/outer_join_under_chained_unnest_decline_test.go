package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestOuterJoinUnderChainedUnnestDeclines pins the correct-or-loud contract for
// a box leg that can't be ordinalized: any WHERE clause on a leg of an OUTER
// JOIN nested under a chained lateral unnest must loud-reject with a clear
// message rather than silently produce wrong rows. There is no name-model
// fallback for this shape any more, so declining is the only correct
// behavior. Planning-only.
func TestOuterJoinUnderChainedUnnestDeclines(t *testing.T) {
	t.Parallel()
	md := buildChainedUnnestMetadata(t)

	nested := `FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" LEFT JOIN T4 AS "C" ON "C"."ID" = "A"."ID" + 90`
	rejects := []struct{ name, sql, wantErr string }{
		// A WHERE on each class of box leg (preserved / first null-supplying /
		// second null-supplying) of a nested LEFT box under a chained unnest —
		// none of these are ordinalizable, so all must loud-reject.
		{
			"boxlegA_preserved", `SELECT "Y" ` + nested + `, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 10`,
			"OUTER JOIN under a chained lateral unnest",
		},
		{
			"boxlegB_nullsupplying", `SELECT "Y" ` + nested + `, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "B"."ID" = 20`,
			"OUTER JOIN under a chained lateral unnest",
		},
		{
			"boxlegC_nullsupplying", `SELECT "Y" ` + nested + `, "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "C"."ID" = 110`,
			"OUTER JOIN under a chained lateral unnest",
		},
		// The single (non-nested) LEFT box twin of the same shape.
		{
			"single_leftbox_boxleg", `SELECT "Y" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 10`,
			"OUTER JOIN under a chained lateral unnest",
		},
	}
	for _, tc := range rejects {
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err == nil {
			t.Errorf("%s: expected a loud reject, got a plan\n  sql: %s", tc.name, tc.sql)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: expected %q, got: %v\n  sql: %s", tc.name, tc.wantErr, err, tc.sql)
		}
	}
}
