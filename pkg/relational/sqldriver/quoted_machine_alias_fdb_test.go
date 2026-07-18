package sqldriver_test

// A QUOTED, machine-shaped table alias (`AS "q$1"`, `AS "Q$DUP1"` — the
// lexer admits $ inside quoted identifiers) must resolve IDENTICALLY in
// every position. The parse capture strips quotes early
// (StripIdentifierQuotes keeps quoted text verbatim), and the embedded
// builders used to re-wrap those captured strings with NewUnquoted —
// re-FOLDING the verbatim `q$1` to `Q$1` — so the FROM registration and
// the quote-aware reference channel disagreed on the same alias: the
// projection resolved (both sides folded) while WHERE / correlated-EXISTS
// failed 42703 "no FROM source aliased as q$1" on a LEGAL query.
// FromNormalized preserves the captured text; these pins hold every
// position together.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func TestFDB_QuotedMachineShapedAliases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_qmsa")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_qmsa")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE qmsa CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
			" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
			" CREATE TABLE sink (sid BIGINT, PRIMARY KEY (sid))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_qmsa/s WITH TEMPLATE qmsa")
	dsn := fmt.Sprintf("fdbsql:///testdb_qmsa?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO p VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO q VALUES (5), (7)")

	ints := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("must ANSWER: %v\n  sql: %s", err, q)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !v.Valid {
				t.Fatalf("NULL — the quoted alias silently missed its leg\n  sql: %s", q)
			}
			out = append(out, v.Int64)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	t.Run("projection", func(t *testing.T) {
		if got := ints(t, `SELECT "q$1".id FROM p AS "q$1"`); !reflect.DeepEqual(got, []int64{1, 2}) {
			t.Errorf("= %v, want [1 2]", got)
		}
	})
	t.Run("where", func(t *testing.T) {
		if got := ints(t, `SELECT "q$1".id FROM p AS "q$1" WHERE "q$1".v = 10`); !reflect.DeepEqual(got, []int64{1}) {
			t.Errorf("= %v, want [1]", got)
		}
	})
	t.Run("dup_shaped_alias", func(t *testing.T) {
		if got := ints(t, `SELECT "Q$DUP1".id FROM p AS "Q$DUP1"`); !reflect.DeepEqual(got, []int64{1, 2}) {
			t.Errorf("= %v, want [1 2]", got)
		}
	})
	t.Run("join_legs", func(t *testing.T) {
		got := ints(t, `SELECT "q$2".qid FROM p AS "q$1", q AS "q$2"`)
		if !reflect.DeepEqual(got, []int64{5, 5, 7, 7}) {
			t.Errorf("= %v, want [5 5 7 7]", got)
		}
	})
	t.Run("correlated_exists", func(t *testing.T) {
		if got := ints(t, `SELECT "q$3".id FROM p AS "q$3" WHERE EXISTS (SELECT 1 FROM q WHERE "q$3".id = 1)`); !reflect.DeepEqual(got, []int64{1}) {
			t.Errorf("= %v, want [1]", got)
		}
	})
	t.Run("order_by", func(t *testing.T) {
		if got := ints(t, `SELECT "q$1".v FROM p AS "q$1" ORDER BY "q$1".v DESC`); !reflect.DeepEqual(got, []int64{20, 10}) {
			t.Errorf("= %v, want [20 10]", got)
		}
	})
	// FOLD COLLISION: two DIFFERENT quoted aliases whose canonical
	// correlation keys would collide ("q$9" and "Q$9" both fold to Q$9).
	// Java keeps quoted identifiers case-distinct end-to-end and answers
	// with per-alias binding; Go reaches the same answer through the mint
	// layer — the FROM-walk's folded seen-map treats the pair as
	// duplicates and mints the later leg a distinct binding, so the
	// runtime keys never collide and each alias resolves its own leg
	// (EqualsIgnoreQuoting is case-exact). Both legs must answer their
	// OWN columns; a first-span-wins misbind would serve the wrong leg.
	// (Scope.AddSource additionally rejects an UNMINTED same-key pair —
	// defense for scope builders without the mint layer.)
	t.Run("fold_collision_per_alias_binding", func(t *testing.T) {
		got := ints(t, `SELECT "q$9".id FROM p AS "q$9", q AS "Q$9"`)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if !reflect.DeepEqual(got, []int64{1, 1, 2, 2}) {
			t.Errorf("first alias = %v, want multiset [1 1 2 2] (p.id per cross-join row)", got)
		}
		got = ints(t, `SELECT "Q$9".qid FROM p AS "q$9", q AS "Q$9"`)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if !reflect.DeepEqual(got, []int64{5, 5, 7, 7}) {
			t.Errorf("second alias = %v, want multiset [5 5 7 7] (q.qid per cross-join row)", got)
		}
	})
	// The FOLDED RETRY (resolveColumnRefStructural): a quoted-lowercase
	// reference over a DERIVED table resolves through the folded
	// registration ("id" registers as ID) — the retry must fold
	// (NewUnquoted on purpose); re-issuing the verbatim spelling turned
	// this legal query into a spurious 42703.
	t.Run("quoted_lower_over_derived", func(t *testing.T) {
		got := ints(t, `SELECT "id" FROM (SELECT id FROM p) AS d ORDER BY "id"`)
		if !reflect.DeepEqual(got, []int64{1, 2}) {
			t.Errorf("= %v, want [1 2]", got)
		}
	})
	t.Run("group_by", func(t *testing.T) {
		if got := ints(t, `SELECT "q$1".v FROM p AS "q$1" GROUP BY "q$1".v ORDER BY "q$1".v`); !reflect.DeepEqual(got, []int64{10, 20}) {
			t.Errorf("= %v, want [10 20]", got)
		}
	})
	// The catalog post-build path (INSERT ... SELECT): duplicate FROM
	// aliases bake per-binding like the SELECT path — the retired
	// carve-out's guard clause here silently survived and discarded the
	// valid baked value (UnresolvableOrdinalError on a query the base
	// revision planned). The source filters to ONE p row so each qid
	// inserts once (a full cross product would 23505 on the sink PK).
	// The quoted-LOWERCASE unnest-alias twin lives in the array unnest
	// suite (its fixture can seed arrays).
	t.Run("insert_select_dup_aliases", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `INSERT INTO sink SELECT a.qid FROM p AS a, q AS a WHERE a.id = 1`); err != nil {
			t.Fatalf("INSERT...SELECT over dup aliases must ANSWER: %v", err)
		}
		got := ints(t, `SELECT sid FROM sink ORDER BY sid`)
		if !reflect.DeepEqual(got, []int64{5, 7}) {
			t.Errorf("= %v, want [5 7]", got)
		}
	})
}
