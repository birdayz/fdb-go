package testkit

// Runtime pins for the boundary between ordinal (positional) and name-model
// join execution, E2E over real FDB. A companion test in the query package
// covers the TRANSLATION half (the cluster-arity walk + gate decisions);
// these pin what the user actually observes:
//
//   - A 2-way join consumed INSIDE a 3-way inner cluster stays name-model —
//     correct rows AND the same plan shape a plain name-model plan would
//     produce (the nested FlatMap-over-FlatMap anchored merge chain).
//   - The flattening-evasion shape `FROM (a JOIN b) t1, (c JOIN d) t2 WHERE
//     t1.aid = t2.cid` stays name-model end-to-end — see
//     TestFDB_FourWayFlatteningEvasionStaysNameModel. It used to be
//     unplannable (0AF00) and the pin was the clean rejection; now the
//     JOIN-bodied derived table derives its output row from its own legs,
//     so what is pinned is the ANSWER: exactly the one row where
//     t1.aid = t2.cid, in every spelling. The row COUNT is load-bearing —
//     a cross-product degrade returns four. The CTE spelling still
//     declines, and that decline is pinned too so the gap stays visible.
//   - A dedicated GROUP BY/HAVING-over-2-way-join pin: aggregation, HAVING
//     (on the aggregate and on the group key), and ORDER BY/LIMIT sit
//     correctly over a GATED (ordinal) 2-way join, with EXPLAIN fragments
//     proving the plan shape (StreamingAgg over the FlatMap join).
//   - A duplicate-name SELECT * pin over a gated join (beyond
//     TestFDB_AmbiguousColumnStar's a/b+name shape): both legs carry
//     bare-colliding ID and V columns; all four come back per-leg-correct
//     in positional scan order.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// pinExplain returns the EXPLAIN output for q.
func PinExplain(t *testing.T, db *sql.DB, ctx context.Context, q string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

// pinRows runs q and renders every row as "v1|v2|...|vN" in scan
// order (callers sort when the query has no ORDER BY). A query error here is
// also the guard against the ordinal model's loud internal errors
// (OrdinalResolutionError / BakedNameContextError / OrdinalBakeError) — those
// surface as query failures, so err==nil proves none fired.
func PinRows(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %q: %v", q, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	return out
}
