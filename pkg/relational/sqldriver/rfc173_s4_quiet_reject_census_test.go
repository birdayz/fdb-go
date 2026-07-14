package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestRFC173QuietRejectCensus pins the RFC-173 S4 correct-or-loud CAPS: each
// box-leg-WHERE-over-a-chained-box query loud-rejects with the cap's message.
// (The companion zero-producer assertion is STRUCTURAL now — the name-model
// producers and their census observer were deleted, RFC-173 S4 item B.)
// Planning-only.
func TestRFC173QuietRejectCensus(t *testing.T) {
	t.Parallel()
	md := buildChainedUnnestMetadata(t)

	nested := `FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" LEFT JOIN T4 AS "C" ON "C"."ID" = "A"."ID" + 90`
	rejects := []struct{ name, sql, wantErr string }{
		// A WHERE on each class of box leg (preserved / first null-supplying /
		// second null-supplying) of a nested LEFT box under a chained unnest —
		// the un-ordinalizable straddle the S4 cap loud-rejects.
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
		// The single (non-nested) LEFT box twin of the same cap.
		{
			"single_leftbox_boxleg", `SELECT "Y" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" = 10`,
			"OUTER JOIN under a chained lateral unnest",
		},
	}
	for _, tc := range rejects {
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err == nil {
			t.Errorf("%s: expected the S4 cap loud-reject, got a plan\n  sql: %s", tc.name, tc.sql)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: expected %q, got: %v\n  sql: %s", tc.name, tc.wantErr, err, tc.sql)
		}
	}
}
