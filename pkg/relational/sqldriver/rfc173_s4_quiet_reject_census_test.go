package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
	rquery "fdb.dev/pkg/relational/core/query"
)

// TestRFC173QuietRejectCensus pins that the RFC-173 S4 correct-or-loud CAPS
// reject BEFORE any translation work runs — a capped query must fire ZERO
// P4/P5 name-model result-value producers (the producers a discarded doomed
// translation would otherwise exercise, blocking their deletion — RFC-173
// item B).
//
// RED-ON-REVERT: with the hoisted box-leg-WHERE cap moved back to the
// post-translation chained-rebase arm (its original site), each box-leg query
// below still loud-rejects but fires 4 producers (2×P4 for the nested box's
// legs, 2×P5 for the chained links) — this test trips on the census count.
// Serial (process-global observer), planning-only.
func TestRFC173QuietRejectCensus(t *testing.T) { //nolint:paralleltest // process-global observer, must be serial
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
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		rquery.SetProducerCensusObserver(nil)
		if err == nil {
			t.Errorf("%s: expected the S4 cap loud-reject, got a plan\n  sql: %s", tc.name, tc.sql)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: expected %q, got: %v\n  sql: %s", tc.name, tc.wantErr, err, tc.sql)
		}
		if n != 0 {
			t.Errorf("%s: capped query fired %d P4/P5 producer(s), want 0 (the cap must reject "+
				"BEFORE the doomed translation runs its name-model fallback)\n  sql: %s", tc.name, n, tc.sql)
		}
	}
}
