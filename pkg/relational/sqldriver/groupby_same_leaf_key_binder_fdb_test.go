package sqldriver_test

// Two GROUP BY keys that share a LEAF name, re-read from a HAVING predicate
// that must stay ABOVE the aggregate.
//
// The binder that resolves such a re-read to its output slot
// (`groupByOutputBaker`, cascades_translator.go) looks the reference up in a
// map keyed by the group key's rendered output name. That name comes from
// `AggregateKeyColumnName`, which for a FieldValue is its BARE leaf — so both
// keys of `GROUP BY o.k, i.k` render "K" and `keys["K"]` is LAST-WINS: it
// holds the SECOND key's ordinal and the first key is not in the map under its
// own leaf at all.
//
// What separates them today is a QUALIFIER SEGMENT the binder builds at both
// ends from the reference's QuantifiedObjectValue child — `O.K` / `I.K` are
// registered as aliases of the right ordinals, and the re-read is looked up
// under the same spelling. That is a (correlation, leaf) pair encoded into a
// string, i.e. the qualified-name channel RFC-197 item 6 removes. Delete it and
// the lookup falls back to the last-wins leaf: every re-read of EITHER key
// binds to the SECOND key's slot.
//
// This is the pin RFC-197 item 5 requires to land BEFORE item 6, because
// nothing else in the tree sees the conflation:
//
//   - `ambiguous_group_key_reread.yaml` is the closest existing coverage and it
//     does NOT cover this. Its qualified control (`HAVING po.id > 1`) is a PURE
//     group-key predicate, and with the qualifier segment deleted that scenario
//     still passes 4/4 while every yamsql/rowdiff target stays green; the only
//     reds are one plan shape and the golden.
//
//     Why it cannot cover this, stated correctly: the predicate is NOT pushed
//     below the aggregate. `plan_shape.golden` shows `PredicatesFilter` ABOVE
//     `StreamingAgg` for that scenario, and the pushdown decider asserts the
//     same shape independently — so the re-read never reaches the binder path
//     this test exercises. (An earlier reading of this had
//     `PushFilterThroughGroupByRule` pushing it down and
//     `rebindGroupKeyRefToInner` rescuing it by first-match name path. That
//     mechanism does not occur; the rebind never runs. The conclusion is the
//     same and stronger.)
//
//     `rebindGroupKeyRefToInner` is nonetheless qualifier-blind in exactly this
//     way — `AccessorNamePath` stops at the QOV root, so `o.k` and `i.k` both
//     render `["K"]`. Measured over `//pkg/relational/...`: it fires 20 times
//     and every firing has ONE grouping key, against 1094 predicates rejected
//     because a qualified reference's path key is the flat `"I.K"` and no
//     grouping key's is. So its scan never had two candidates to choose between,
//     which is why mutating first-match to last-match changed no test. Item 6
//     removes that qualifier segment and would have handed it two. Both that
//     rule and its decider now decline an ambiguous key set outright; the pins
//     are `TestFDB_GroupBySameLeafKeys_PushedHavingStaysAboveTheAggregate` (this
//     package) and the two boundary tests in
//     `//pkg/recordlayer/query/plan/cascades`.
//   - The predicates below reference an AGGREGATE, so they are not pushable and
//     the output-baked ordinal stays live all the way to the executor. That is
//     what makes the wrong slot observable as wrong ROWS.
//
// Both directions are pinned. Under last-wins the SECOND key's ordinal is the
// one that survives, so a re-read of the FIRST key is what breaks — and which
// table is "first" is the GROUP BY order, so the two scenarios below swap it.
// A single direction would let a fix that merely re-orders the map pass.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func gslkMustExec(t *testing.T, db *sql.DB, ctx context.Context, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func TestFDB_GroupBySameLeafKeys_HavingRereadBindsItsOwnSlot(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gb_same_leaf")
	gslkMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gb_same_leaf")
	gslkMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gb_same_leaf "+
			"CREATE TABLE outer_t (k BIGINT NOT NULL, PRIMARY KEY (k)) "+
			"CREATE TABLE inner_t (k BIGINT NOT NULL, o_k BIGINT, PRIMARY KEY (k))")
	gslkMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gb_same_leaf/s WITH TEMPLATE gb_same_leaf")
	dsn := fmt.Sprintf("fdbsql:///testdb_gb_same_leaf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The two K columns are separated by an ORDER OF MAGNITUDE so a predicate
	// threshold can only be satisfied by the intended one. outer_t.k ∈ {1,2},
	// inner_t.k ∈ {10,20}, joined 1:1.
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

	// GROUP BY o.k, i.k — last-wins holds I.K's ordinal, so a re-read of O.K is
	// what a leaf-only lookup gets wrong. `o.k + COUNT(*) > 2` selects only the
	// o.k=2 group (1+1=2, 2+1=3); read off I.K's slot instead it is 10+1 and
	// 20+1, and BOTH groups survive.
	t.Run("first_key_reread_o_k", func(t *testing.T) {
		t.Parallel()
		const want = "2/20/1"
		if got := triples(t, sel+"GROUP BY o.k, i.k HAVING o.k + COUNT(*) > 2"); got != want {
			t.Errorf("HAVING o.k + COUNT(*) > 2 over GROUP BY o.k, i.k = %q, want %q\n"+
				"The HAVING re-read of O.K bound to a group-key output slot that is not O.K's. "+
				"Both keys render the leaf \"K\", so the binder's output-name map is last-wins "+
				"and only the QUALIFIER segment tells them apart (RFC-197 item 5 / item 6 "+
				"ordering constraint). %q is I.K's slot.", got, want, "10/20 in the predicate")
		}
	})

	// The mirror: GROUP BY i.k, o.k puts O.K last, so now the re-read of I.K is
	// the one a leaf-only lookup gets wrong. `i.k + COUNT(*) > 12` selects only
	// the i.k=20 group (10+1=11, 20+1=21); read off O.K's slot it is 1+1 and
	// 2+1, and NEITHER group survives.
	t.Run("first_key_reread_i_k", func(t *testing.T) {
		t.Parallel()
		const want = "2/20/1"
		if got := triples(t, sel+"GROUP BY i.k, o.k HAVING i.k + COUNT(*) > 12"); got != want {
			t.Errorf("HAVING i.k + COUNT(*) > 12 over GROUP BY i.k, o.k = %q, want %q\n"+
				"The HAVING re-read of I.K bound to a group-key output slot that is not I.K's "+
				"(RFC-197 item 5 / item 6 ordering constraint).", got, want)
		}
	})

	// Controls, so neither assertion above can be satisfied by a binder that
	// resolves nothing and a plan that returns everything (or nothing). The
	// LAST group key is the one last-wins keeps, so these two stay correct even
	// with the qualifier segment gone — they pin the rows, not the separation.
	t.Run("last_key_reread_is_a_control", func(t *testing.T) {
		t.Parallel()
		if got := triples(t, sel+"GROUP BY o.k, i.k HAVING i.k + COUNT(*) > 12"); got != "2/20/1" {
			t.Errorf("control (last key, GROUP BY o.k, i.k) = %q, want %q", got, "2/20/1")
		}
		if got := triples(t, sel+"GROUP BY i.k, o.k HAVING o.k + COUNT(*) > 2"); got != "2/20/1" {
			t.Errorf("control (last key, GROUP BY i.k, o.k) = %q, want %q", got, "2/20/1")
		}
	})

	// Ungrouped-by-HAVING baseline: both groups are present before the HAVING
	// filters, so a failure above is the PREDICATE binding to the wrong slot and
	// not the join or the grouping producing the wrong rows.
	t.Run("no_having_baseline", func(t *testing.T) {
		t.Parallel()
		if got := triples(t, sel+"GROUP BY o.k, i.k"); got != "1/10/1 2/20/1" {
			t.Errorf("baseline without HAVING = %q, want %q", got, "1/10/1 2/20/1")
		}
	})
}
