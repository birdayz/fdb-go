package sqldriver_test

// Covering-index VALUE fidelity, per column type.
//
// A covering plan answers a column from the INDEX ENTRY; every other plan
// answers it from the stored RECORD. Those are two encodings of the same value,
// and the executor NORMALIZES on the way out of the index entry — tuple.UUID
// becomes [16]byte, float32 becomes float64, a Versionstamp becomes its 12 raw
// bytes (tupleElementToRowValue in executor.go). Wherever a normalization
// exists, so does the possibility that the two paths disagree — and a
// disagreement means one column reads differently depending on which plan the
// cost model picked, a wrong answer that follows the plan rather than the data
// and so does not reproduce by re-running the same query on the same rows.
//
// The existing covering tests use BIGINT and STRING only, the two types that
// pass through with NO conversion — that is, the two that cannot show this.
//
// The contrast pair is measured, not assumed:
//
//	SELECT cv     FROM t WHERE cv = lit   ->  IndexScan(..., COVERING)
//	SELECT cv,pad FROM t WHERE cv = lit   ->  IndexScan(...)          + fetch
//
// Same column, same row, same schema; only the decoder differs. Comparing those
// two to each other is what localizes a mismatch to the entry decode rather
// than to planning, which comparing each to the unindexed twin would not.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// mmMustRows runs q and fails rather than returning an error: it is the ORACLE
// side of a comparison, where an error is never an acceptable answer and
// silently returning nil would make the comparison vacuous.
func mmMustRows(t *testing.T, ctx context.Context, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := mmRows(t, ctx, db, q)
	if err != nil {
		t.Fatalf("oracle query failed, so there is nothing to compare against\n  q: %s\n  err: %v", q, err)
	}
	return rows
}

func TestFDB_CoveringIndexValueFidelityByType(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	cases := []struct {
		ddlType  string
		literals []string
	}{
		{"BIGINT", []string{"0", "-1", "9223372036854775807", "-9223372036854775808"}},
		{"INTEGER", []string{"0", "-1", "2147483647", "-2147483648"}},
		// FLOAT is stored as float32 and WIDENED to float64 leaving an index
		// entry. 0.1 has no exact float32 form, so a widening that took a
		// different route than the record path shows here and nowhere else.
		{"FLOAT", []string{"0.1", "-0.0", "0.0", "3.0E38", "-3.0E38"}},
		{"DOUBLE", []string{"0.1", "-0.0", "0.0", "1.7976931348623157E308"}},
		{"STRING", []string{"''", "'a'", "'ünïcödé'", "'  padded  '"}},
		{"BOOLEAN", []string{"true", "false"}},
		// UUID is the other normalized type: tuple.UUID -> [16]byte.
		{"UUID", []string{
			"'550e8400-e29b-41d4-a716-446655440000'",
			"'00000000-0000-0000-0000-000000000000'",
			"'ffffffff-ffff-ffff-ffff-ffffffffffff'",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.ddlType, func(t *testing.T) {
			t.Parallel()
			tag := strings.ToLower(tc.ddlType)
			w := mmNewTwin(t, ctx, "/testdb_covfid_"+tag, "covfid"+tag,
				fmt.Sprintf("CREATE TABLE t (id BIGINT, cv %s, pad STRING, PRIMARY KEY (id)) ", tc.ddlType),
				"CREATE INDEX t_cv ON t (cv) ")

			var rows []string
			for i, lit := range tc.literals {
				rows = append(rows, fmt.Sprintf("(%d, %s, 'padpad')", i+1, lit))
			}
			// A NULL row so the IS NULL arm has something to find, and so the
			// value rows are not the whole table.
			rows = append(rows, fmt.Sprintf("(%d, NULL, 'padpad')", len(tc.literals)+1))
			w.Exec("INSERT INTO t (id, cv, pad) VALUES " + strings.Join(rows, ", "))

			for _, lit := range tc.literals {
				coveredQ := fmt.Sprintf("SELECT id, cv FROM t WHERE cv = %s ORDER BY id", lit)
				fetchedQ := fmt.Sprintf("SELECT id, cv, pad FROM t WHERE cv = %s ORDER BY id", lit)

				// The premise: without a covering plan this compares the record
				// path with itself and says nothing about entry decode.
				//
				// A signed ZERO is the documented exception and is asserted the
				// other way round. +0.0 and -0.0 pack to two DISTINCT adjacent
				// index keys, while SQL numeric equality says they are equal, so
				// an equality probe on either has its range WIDENED to span both
				// and carries a residual filter — which costs it the covering
				// plan. Asserting that it is NOT covering pins the widening
				// itself: if one of these ever comes back COVERING, the probe
				// has narrowed to a single sign and `cv = 0.0` has silently
				// stopped matching rows stored as -0.0.
				plan := w.Explain(coveredQ)
				isSignedZero := lit == "0.0" || lit == "-0.0"
				switch {
				case isSignedZero && strings.Contains(plan, "COVERING"):
					t.Errorf("an equality probe on %s is now COVERING, which means it is no longer "+
						"range-widened across both signed zeros. Check that `cv = %s` still matches "+
						"rows stored with the OTHER sign.\n  q: %s\n  plan: %s",
						lit, lit, coveredQ, plan)
				case isSignedZero:
					// Expected: no entry-vs-record comparison is possible, but
					// the twin comparison below still checks the ANSWER.
					w.Want("widened read of "+lit, coveredQ, mmMustRows(t, ctx, w.plain, coveredQ))
					continue
				case !strings.Contains(plan, "COVERING"):
					t.Errorf("the projection for %s is not COVERING, so the comparison below "+
						"exercises the record path twice and proves nothing about how an index "+
						"entry decodes a %s\n  q: %s\n  plan: %s", lit, tc.ddlType, coveredQ, plan)
					continue
				}

				// Both readings agree with the unindexed oracle …
				w.Want("covered read of "+lit, coveredQ, mmMustRows(t, ctx, w.plain, coveredQ))
				w.Want("fetched read of "+lit, fetchedQ, mmMustRows(t, ctx, w.plain, fetchedQ))

				// … and, decisively, with each other.
				fromEntry := mmMustRows(t, ctx, w.idx, coveredQ)
				fromRecord := mmMustRows(t, ctx, w.idx, fetchedQ)
				if len(fromEntry) != len(fromRecord) {
					t.Errorf("covered and fetched reads of %s returned different row counts (%d vs %d)",
						lit, len(fromEntry), len(fromRecord))
					continue
				}
				for i := range fromEntry {
					// fetched rows carry the extra pad column; drop it.
					rec := fromRecord[i]
					if j := strings.LastIndex(rec, "|"); j >= 0 {
						rec = rec[:j]
					}
					if fromEntry[i] != rec {
						t.Errorf("a %s column reads differently depending on the plan:\n"+
							"  literal:                      %s\n"+
							"  decoded from the index entry: %q\n"+
							"  decoded from the record:      %q",
							tc.ddlType, lit, fromEntry[i], rec)
					}
				}
			}

			// The NULL row, whose covered read has no value to decode at all.
			nullQ := "SELECT id, cv FROM t WHERE cv IS NULL ORDER BY id"
			w.Want("covered IS NULL", nullQ, mmMustRows(t, ctx, w.plain, nullQ))

			// A range covering every stored value, so the entry decode is
			// exercised in one scan rather than one probe per literal — a
			// different range shape reaching the same decoder.
			rangeQ := "SELECT id, cv FROM t WHERE cv IS NOT NULL ORDER BY cv, id"
			w.Want("every value in one scan", rangeQ, mmMustRows(t, ctx, w.plain, rangeQ))

			// Extrema go through the index ordering rather than the row decode,
			// cross-checking the same values by a third route.
			mq := "SELECT MIN(cv), MAX(cv) FROM t"
			if _, err := mmRows(t, ctx, w.plain, mq); err == nil {
				w.Want("extrema", mq, mmMustRows(t, ctx, w.plain, mq))
			}
		})
	}
}
