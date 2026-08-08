package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_NestedSortKeyAmbiguityIsRejectedBeforeTheFold pins a NEGATIVE result,
// and the negative is load-bearing: it is what licenses the projected-EXISTS
// fold's re-anchor to DECLINE on an ambiguous root column name instead of
// disambiguating one.
//
// The re-anchor derives a nested sort key's root ordinal from the layout the
// fold evaluates against, and refuses when that layout holds more than one
// column of the root's name — a first-match there would be the wrong-column
// read the whole design exists to prevent. The open question was whether that
// refusal is a capability regression: a merged join row CAN hold two columns of
// the same name (a self-join does exactly that), so if such a row could also
// carry a two-segment nested sort key, the fold would decline a shape users
// write.
//
// It cannot, and the two facts are the same fact. A bare `n.sk` is ambiguous
// PRECISELY WHEN more than one leg exposes `n`, and SQL rejects that reference
// with 42702 during resolution — before the fold ever sees a key. Duplicate
// root names in the merged row and a resolvable two-segment nested key are
// therefore mutually exclusive, so the re-anchor's ambiguity arm is unreachable
// for that population and declines nothing a user can express.
//
// WHAT RE-ARMS THE HAZARD: any change that lets a bare multi-segment reference
// resolve when its root is ambiguous — first-match resolution, an implicit
// preference for the left leg, or scoping that hides one leg's column instead
// of rejecting. If this test fails, that is what happened, and the re-anchor's
// ambiguity arm becomes reachable: it must then disambiguate by IDENTITY (which
// correlation the carried path's root belongs to, matched against which leg each
// merged slot came from) and NEVER by picking a first name match.
//
// The three-segment form (`a.n.sk`) is a different, unresolved shape and is not
// what this test pins; it is refused for its own reason and has its own pin.
func TestFDB_NestedSortKeyAmbiguityIsRejectedBeforeTheFold(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nsk_ambig")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_nsk_ambig")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nsk_ambig_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t4(id BIGINT, n nst, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nsk_ambig/s WITH TEMPLATE nsk_ambig_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_nsk_ambig?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50)), (2, (40)), (3, (30))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t4 VALUES (900, (7), 1), (901, (8), 2), (902, (9), 3)")

	queryErr := func(q string) error {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() { //nolint:revive // draining is the point
		}
		return rows.Err()
	}

	// Both shapes that put two same-named nested columns in one merged row.
	for _, tc := range []struct {
		name string
		q    string
	}{
		{
			"self_join_both_legs_expose_n",
			"SELECT a.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = a.id) AS h " +
				"FROM t1 AS a JOIN t1 AS b ON b.id = a.id ORDER BY n.sk",
		},
		{
			"two_tables_both_expose_n",
			"SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
				"FROM t1 JOIN t4 ON t4.t1_id = t1.id ORDER BY n.sk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := queryErr(tc.q)
			if err == nil {
				t.Fatalf("expected an ambiguity rejection for %q, got success — "+
					"a bare nested key resolved against a merged row holding TWO `n` "+
					"columns. The re-anchor's ambiguity arm is now REACHABLE and must "+
					"disambiguate by leg IDENTITY, never by first name match", tc.q)
			}
			if !strings.Contains(err.Error(), "42702") {
				t.Fatalf("expected 42702 (ambiguous reference) for %q, got: %v — "+
					"the rejection that makes the re-anchor's ambiguity arm unreachable "+
					"is gone or has moved; re-read the arm before trusting it", tc.q, err)
			}
		})
	}

	// COUNTERWEIGHT. The 42702 above must come from genuine ambiguity, not from
	// "a nested key reaching the fold at all". With only ONE `n` in scope the
	// reference is unambiguous and must NOT be rejected as ambiguous — otherwise
	// the two assertions above would pass for a reason having nothing to do with
	// duplicate names, and this file would be pinning the wrong fact.
	//
	// THIS COUNTERWEIGHT IS THE WEAKER OF THE TWO, DELIBERATELY, AND THE STRONGER
	// ONE IS OWED. The ideal control is the same shape over a JOIN — unambiguous
	// nested key, merged row, no duplicate name — because that is the exact
	// population the ambiguity arm sits in. It cannot land yet: that query trips
	// an existing hard-zero (`existsSortSplit` DIVERGED, witness `"T1.N.SK"` vs
	// identity `"T1"`), which is the very defect the fold fix exists to remove.
	// Adding it now would put the suite red against an assertion that is correct.
	//
	// So the join control belongs to the commit that fixes the fold, and that
	// commit MUST add it — a single-table counterweight cannot show the ambiguity
	// guard is well-scoped over merged rows, only that it is not firing on every
	// nested key.
	t.Run("unambiguous_nested_key_single_table_is_not_42702", func(t *testing.T) {
		t.Parallel()
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 ORDER BY n.sk"
		if err := queryErr(q); err != nil && strings.Contains(err.Error(), "42702") {
			t.Fatalf("an UNAMBIGUOUS nested key was rejected as ambiguous: %v — "+
				"the ambiguity guard is over-broad, and the mutual exclusion this file "+
				"documents would hold vacuously", err)
		}
	})
}
