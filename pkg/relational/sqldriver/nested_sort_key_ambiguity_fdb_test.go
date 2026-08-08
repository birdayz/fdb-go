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
// It cannot TODAY, and the two facts are the same fact: a bare `n.sk` is
// ambiguous PRECISELY WHEN more than one leg exposes `n`, and SQL rejects that
// reference with 42702 during resolution — before the fold sees a key.
//
// THE LICENCE THIS GIVES IS CONDITIONAL, AND THE CONDITION IS LOAD-BEARING.
// Two-segment keys are only half the population. A merged row carrying two `N`
// columns is reachable RIGHT NOW through the three-segment form:
//
//	self-join, ORDER BY a.n.sk       -> row columns [ID N ID N]
//	t1 JOIN t4, ORDER BY t1.n.sk     -> row columns [ID N T1_ID ID N]
//
// Those fail only because Go's walkColumnRef refuses a 3-segment reference —
// and that refusal is a DIVERGENCE, not a rule. Java has no arity cap anywhere
// on this path: `fullId : uid (DOT uid)*` in the grammar, an unbounded
// remainingPath accessor loop in SemanticAnalyzer, and its own tests execute a
// five-segment reference. So the moment that divergence is closed — which is
// exactly what this workstream prescribes — duplicate roots and a resolvable
// nested key coexist, and the conjunction this file calls impossible becomes
// ordinary. The ambiguity arm must therefore EXIST and DECLINE before that
// lands; it is unit-driven in the values package. It must never first-match:
// RecordType.FieldIndex returns the first name match, so `b.n.sk` over a
// self-join would silently read the LEFT leg's `N`.
//
// WHAT RE-ARMS THE HAZARD:
//   - walkColumnRef gains 3-segment support (the prescribed fix, and the most
//     likely re-armer BY FAR — it is scheduled work, not a hypothetical);
//   - a bare multi-segment reference resolves when its root is ambiguous, via
//     first-match, an implicit left-leg preference, or scoping that hides a
//     column instead of rejecting;
//   - the ephemeral-derived suppression widens. Java already suppresses
//     ephemeral-derived duplicates by name before its ambiguity assert
//     (SemanticAnalyzer), so a mixed direct/ephemeral pair yields ONE match and
//     no ambiguity error. Go matching that behaviour would admit a duplicate
//     root name without any 42702 at all — this file would stay green while the
//     conjunction became reachable, which is the one re-armer these assertions
//     cannot see.
//
// In every case the answer is to disambiguate by leg IDENTITY — which
// correlation the carried path's root belongs to, matched against which leg each
// merged slot came from — and never by name.
func TestFDB_NestedSortKeyAmbiguityIsRejectedBeforeTheFold(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nsk_ambig")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_nsk_ambig")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nsk_ambig_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t4(id BIGINT, n nst, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nsk_ambig/s WITH TEMPLATE nsk_ambig_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_nsk_ambig?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50, 1)), (2, (40, 2)), (3, (30, 3))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t4 VALUES (900, (7, 1), 1), (901, (8, 2), 2), (902, (9, 3), 3)")

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

	// COUNTERWEIGHT. The 42702 above must come from genuine ambiguity and not from
	// "a nested key reaching the fold at all", or the assertions above hold for a
	// reason unrelated to duplicate names and this file pins the wrong fact.
	//
	// This asserts the ROWS, not merely the absence of an error. An earlier version
	// asserted only that the error was not 42702, which passed identically whether
	// the key resolved cleanly or exploded one stage later — and a later version
	// pinned the fold defect itself, which had to go red the moment it was fixed.
	// Now that the fold carries the resolved path, the honest assertion is the
	// answer: ids ordered by the NESTED field.
	//
	// It pins the COLUMN, not just the order. n.sk descends 50,40,30 as id ascends
	// 1,2,3, so ordering by n.sk gives [3 2 1] while ordering by id — or by any
	// column the re-anchor might land on by mistake — gives [1 2 3]. A row-order
	// assertion over a column that happens to be sorted would not tell those apart.
	t.Run("unambiguous_nested_key_orders_by_the_nested_field", func(t *testing.T) {
		t.Parallel()
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 ORDER BY n.sk"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v — an UNAMBIGUOUS nested key must plan and sort", q, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			var h bool
			if err := rows.Scan(&id, &h); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		want := []int64{3, 2, 1}
		if len(ids) != len(want) {
			t.Fatalf("got %d rows %v, want %v", len(ids), ids, want)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Fatalf("got ids %v, want %v — [1 2 3] means the sort ordered by ID "+
					"(or by another slot the root re-anchored onto), not by n.sk", ids, want)
			}
		}
	})
}
