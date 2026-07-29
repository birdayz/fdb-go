package sqldriver_test

// Two GROUP BY keys sharing a leaf, re-read from a PURE group-key HAVING — the
// predicate shape that PushFilterThroughGroupByRule moves BELOW the aggregate.
//
// The sibling file `groupby_same_leaf_key_binder_fdb_test.go` covers the HAVING
// that references an AGGREGATE and therefore stays above. This one covers the
// other branch, and it exists because that branch has a second, independent
// place to bind the wrong key: once a predicate is pushed below the aggregate,
// `rebindGroupKeyRefToInner` (rule_push_filter_through_groupby.go) rewrites its
// group-key reference to the grouping key's own pre-aggregate Value by matching
// ACCESSOR NAME PATH — and that path excludes the QOV root, so `o.k` and `i.k`
// both render ["K"]. A scan that takes the first hit binds to whichever key
// GROUP BY listed first.
//
// NO SQL REACHES THAT PICK TODAY. Two independent gates stop it, and this file
// pins the one that lives in the SQL layer plus the rows both gates protect:
//
//   - A QUALIFIED reference carries its qualifier INSIDE the accessor name (one
//     accessor whose Field is the flat string "I.K"), so its path key is "I.K",
//     matches no grouping key's "K", and the predicate is never classified
//     pushable. It stays a residual filter above the aggregate — which is why
//     the rows below are right regardless of how the rebind would have picked.
//     That gate is the qualified-name channel RFC-197 item 6 REMOVES: strip the
//     qualifier segment and `i.k` renders ["K"], the predicate becomes pushable
//     with two same-path keys in hand, and the pick goes live. The rule-side
//     refusal that holds it shut is pinned at its own boundary in
//     //pkg/recordlayer/query/plan/cascades:
//     TestPredicatePushesBelowGroupBy_TwoKeysOneNamePath_RefusesToPush and
//     TestRebindGroupKeyRefToInner_TwoKeysOneNamePath_LeavesTheRefAlone.
//   - An UNQUALIFIED reference is the only spelling that could carry the bare
//     path today, and the resolver rejects it with 42702 before planning. That
//     is the gate `bare_reference_is_ambiguous` pins. Relaxing that rejection —
//     e.g. teaching HAVING to prefer a SELECT alias over the FROM scope — hands
//     the rebind a bare ["K"] reference and re-arms the pick on its own, without
//     item 6.
//
// A negative result is a load-bearing claim, so it is pinned rather than
// asserted: if either gate opens and nothing else changes, these go red.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_GroupBySameLeafKeys_PushedHavingStaysAboveTheAggregate(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gb_same_leaf_push")
	gslkMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gb_same_leaf_push")
	gslkMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gb_same_leaf_push "+
			"CREATE TABLE outer_t (k BIGINT NOT NULL, PRIMARY KEY (k)) "+
			"CREATE TABLE inner_t (k BIGINT NOT NULL, o_k BIGINT, PRIMARY KEY (k))")
	gslkMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gb_same_leaf_push/s WITH TEMPLATE gb_same_leaf_push")
	dsn := fmt.Sprintf("fdbsql:///testdb_gb_same_leaf_push?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Same magnitude separation as the sibling file: outer_t.k ∈ {1,2},
	// inner_t.k ∈ {10,20}, joined 1:1. A threshold between them can only be
	// satisfied by the intended column, so a wrong pick cannot coincide with a
	// right one.
	gslkMustExec(t, db, ctx, "INSERT INTO outer_t (k) VALUES (1), (2)")
	gslkMustExec(t, db, ctx, "INSERT INTO inner_t (k, o_k) VALUES (10, 1), (20, 2)")

	triples := func(t *testing.T, q string) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b, c int64
			if err := rows.Scan(&a, &b, &c); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d/%d/%d", a, b, c))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	const sel = "SELECT o.k, i.k, COUNT(*) FROM outer_t o, inner_t i WHERE i.o_k = o.k "

	// The reference denotes the SECOND grouping key. A first-match pick rebinds
	// it to the FIRST (o.k), turning `i.k > 15` into `o.k > 15` — o.k ∈ {1,2},
	// so NO group survives and the result is empty instead of one row.
	t.Run("second_key_reread_o_then_i", func(t *testing.T) {
		t.Parallel()
		const want = "2/20/1"
		if got := triples(t, sel+"GROUP BY o.k, i.k HAVING i.k > 15"); got != want {
			t.Errorf("HAVING i.k > 15 over GROUP BY o.k, i.k = %q, want %q\n"+
				"A pure group-key HAVING over two same-leaf grouping keys resolved to the "+
				"WRONG key. Empty means the predicate became `o.k > 15` — a first-match "+
				"rebind onto the first grouping key. Either the predicate is now being "+
				"pushed below the aggregate (RFC-197 item 6 removed the qualifier segment "+
				"that stopped it) or the rule's ambiguous-key refusal was dropped.", got, want)
		}
	})

	// The mirror, so a fix that merely re-orders the grouping keys cannot pass.
	// Here the reference denotes the second key again but the tables are swapped:
	// a first-match pick turns `o.k > 1` into `i.k > 1`, which BOTH groups
	// satisfy (10, 20 > 1) — the failure is extra rows, not missing ones.
	t.Run("second_key_reread_i_then_o", func(t *testing.T) {
		t.Parallel()
		const want = "2/20/1"
		if got := triples(t, sel+"GROUP BY i.k, o.k HAVING o.k > 1"); got != want {
			t.Errorf("HAVING o.k > 1 over GROUP BY i.k, o.k = %q, want %q\n"+
				"%q (both groups) means the predicate became `i.k > 1` — a first-match "+
				"rebind onto the first grouping key.", got, want, "1/10/1 2/20/1")
		}
	})

	// Controls: the reference denotes the FIRST key, where first-match happens to
	// be right. These stay green under the wrong pick, so they cannot carry the
	// assertions above — they are here so a binder that resolves nothing (or a
	// plan that returns everything) fails somewhere.
	t.Run("first_key_reread_is_a_control", func(t *testing.T) {
		t.Parallel()
		if got := triples(t, sel+"GROUP BY o.k, i.k HAVING o.k > 1"); got != "2/20/1" {
			t.Errorf("control (first key, GROUP BY o.k, i.k) = %q, want %q", got, "2/20/1")
		}
		if got := triples(t, sel+"GROUP BY i.k, o.k HAVING i.k > 15"); got != "2/20/1" {
			t.Errorf("control (first key, GROUP BY i.k, o.k) = %q, want %q", got, "2/20/1")
		}
	})

	// GATE 2. The bare spelling is the one that would carry the ambiguous path
	// into the rule, and it never gets there: the resolver rejects it. Pinned in
	// both forms — a raw column, and a SELECT alias that spells the same leaf —
	// because HAVING resolving against the alias instead of the FROM scope would
	// open the route without touching the ambiguity rule itself.
	t.Run("bare_reference_is_ambiguous", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{
			sel + "GROUP BY o.k, i.k HAVING k > 15",
			"SELECT i.k AS k, o.k AS ok, COUNT(*) AS c FROM outer_t o, inner_t i " +
				"WHERE i.o_k = o.k GROUP BY o.k, i.k HAVING k > 15",
			"SELECT o.k AS k, i.k AS ik, COUNT(*) AS c FROM outer_t o, inner_t i " +
				"WHERE i.o_k = o.k GROUP BY i.k, o.k HAVING k > 1",
		} {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				rows.Close()
				t.Errorf("query %q was PLANNED; want 42702 Ambiguous reference.\n"+
					"A bare reference to a leaf owned by two grouping keys now resolves. "+
					"That hands PushFilterThroughGroupByRule a reference whose accessor "+
					"path matches BOTH keys, which is the wrong-pick shape — the rule's "+
					"ambiguous-key refusal is now the only thing holding it, and it needs "+
					"to be re-checked before this rejection is relaxed.", q)
				continue
			}
			if !strings.Contains(err.Error(), "42702") {
				t.Errorf("query %q failed with %v, want 42702 Ambiguous reference", q, err)
			}
		}
	})

	// Baseline: both groups exist before HAVING filters, so a failure above is
	// the predicate binding, not the join or the grouping.
	t.Run("no_having_baseline", func(t *testing.T) {
		t.Parallel()
		if got := triples(t, sel+"GROUP BY o.k, i.k"); got != "1/10/1 2/20/1" {
			t.Errorf("baseline without HAVING = %q, want %q", got, "1/10/1 2/20/1")
		}
	})
}
