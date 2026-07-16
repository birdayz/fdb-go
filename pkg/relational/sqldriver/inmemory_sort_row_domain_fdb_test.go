package sqldriver_test

// In-memory sort / merge / DISTINCT must agree with FDB tuple order and with
// the base-scan row domain — the in-memory paths are read-side extensions, so
// any disagreement with an indexed plan over the same data is a wrong-rows bug.
//
// Two regressions pinned here:
//   - F9/F13: compareValues had no []byte arm, so an unindexed ORDER BY on a
//     BYTES column fell through to the fmt.Sprintf("%v") fallback and sorted by
//     decimal-list STRING ("[0 1]" < "[0]" because ' ' < ']'), disagreeing with
//     the same query's indexed plan (bytes_column_probe_test.go) and with Java
//     (FDB tuple order = unsigned lexicographic bytes).
//   - F10: covering-index rows carried raw float32 for FLOAT columns (the
//     32-bit tuple float decodes as float32) while base-record rows widen FLOAT
//     to float64, so a covering leg sorted lexically (no float32 arm) and never
//     deduped against a base-scan leg (distinctKey/extractKey are type-tagged).
//     Fixed by normalizing float32 → float64 at the covering read boundary
//     (tupleElementToRowValue), matching Java's type consistency across access
//     paths (IndexKeyValueToPartialRecord.FieldCopier round-trips through the
//     record message).

import (
	"context"
	"strings"
	"testing"
)

// TestFDB_InMemorySortBytesOrder: unindexed ORDER BY on a BYTES column must
// return byte order — the same order an index on the column would give
// (mirrors bytes_column_probe_test.go, which HAS the index) — not the fmt
// fallback's decimal-list string order.
func TestFDB_InMemorySortBytesOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// NO index on data — forces the in-memory sort path.
	db := setupPlanShapeDB(t, "imsbytes",
		"CREATE TABLE t (id BIGINT NOT NULL, data BYTES, PRIMARY KEY (id))")

	for _, r := range []struct {
		id   int64
		data []byte
	}{
		{1, []byte{0x02}},
		{2, []byte{0x0A}},
		{3, []byte{0x00}},
		{4, []byte{0x00, 0x01}},
	} {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, data) VALUES (?, ?)", r.id, r.data); err != nil {
			t.Fatalf("insert id=%d: %v", r.id, err)
		}
	}

	const q = "SELECT id FROM t ORDER BY data ASC"

	// The sort must actually be the in-memory extension (no index exists to
	// provide the order), or this test would prove nothing about compareValues.
	plan := planExplainVia(t, ctx, db, q)
	if !strings.Contains(plan, "InMemorySort") {
		t.Fatalf("expected InMemorySort in plan, got: %s", plan)
	}

	ids := func(query string) []int64 {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return o
	}

	t.Run("asc_is_byte_order", func(t *testing.T) {
		// Byte order: {00}(3) < {00 01}(4) < {02}(1) < {0A}(2).
		// The old fmt fallback returned [4 3 2 1] ("[0 1]" < "[0]" < "[10]" < "[2]").
		got := ids(q)
		want := []int64{3, 4, 1, 2}
		if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
			t.Errorf("ORDER BY data ASC = %v, want %v (byte order, not lexical)", got, want)
		}
	})
	t.Run("asc_limit_1_is_smallest", func(t *testing.T) {
		// LIMIT pins that the wrong order is user-visible even for a single
		// row: the old code returned 4 ({00 01}), byte order says 3 ({00}).
		got := ids(q + " LIMIT 1")
		if len(got) != 1 || got[0] != 3 {
			t.Errorf("ORDER BY data ASC LIMIT 1 = %v, want [3]", got)
		}
	})
	t.Run("desc_is_reversed_byte_order", func(t *testing.T) {
		got := ids("SELECT id FROM t ORDER BY data DESC")
		want := []int64{2, 1, 4, 3}
		if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
			t.Errorf("ORDER BY data DESC = %v, want %v", got, want)
		}
	})
}

// TestFDB_CoveringFloatRowDomain: a FLOAT column read off a COVERING index
// entry must land in the same row domain as a base-record read (float64), so
// in-memory sort orders it numerically and DISTINCT/UNION dedups it against
// base-scan rows. The index is on (a, f) with the SARG on a — deliberately NOT
// on f itself (the indexed-FLOAT SARG is a separate known bug, see
// indexed_float_sarg_probe_test.go); f only rides along in the covering entry,
// which is exactly the defective surface.
func TestFDB_CoveringFloatRowDomain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := setupPlanShapeDB(t, "covfloat",
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, f FLOAT, PRIMARY KEY (id)) "+
			"CREATE INDEX af_idx ON t (a, f)")

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t (id, a, f) VALUES (1, 1, 10.5), (2, 1, 2.5), (3, 1, 3.5)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	floats := func(query string) []float64 {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		defer rows.Close()
		var o []float64
		for rows.Next() {
			var v float64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return o
	}

	t.Run("covering_order_by_float_is_numeric", func(t *testing.T) {
		// The derived table's projection sits directly on the index fetch, so
		// MergeProjectionAndFetch eliminates it (COVERING); the outer ORDER BY
		// then in-memory-sorts the covering rows. (A flat WHERE...ORDER BY puts
		// the sort between projection and fetch and the covering merge cannot
		// fire — the plan keeps the Fetch and never exercises the defect.)
		const q = "SELECT f FROM (SELECT f FROM t WHERE a > 0) AS d ORDER BY f ASC"
		plan := planExplainVia(t, ctx, db, q)
		if !strings.Contains(plan, "COVERING") {
			t.Fatalf("expected a COVERING index scan (the defect surface), got: %s", plan)
		}
		if !strings.Contains(plan, "InMemorySort") {
			t.Fatalf("expected InMemorySort over the covering rows, got: %s", plan)
		}
		got := floats(q)
		want := []float64{2.5, 3.5, 10.5}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("covering ORDER BY f = %v, want %v (numeric, not lexical %v)",
				got, want, []float64{10.5, 2.5, 3.5})
		}
	})

	t.Run("distinct_dedups_covering_against_base", func(t *testing.T) {
		// One UNION ALL leg covering (index a,f), one leg base scan; every f
		// value exists in both legs, so DISTINCT must collapse them: 3 rows.
		// The old covering rows carried float32, whose type-tagged dedup key
		// never matched the base leg's float64 key — 6 rows.
		const q = "SELECT DISTINCT f FROM " +
			"(SELECT f FROM t WHERE a > 0 UNION ALL SELECT f FROM t) AS d"
		plan := planExplainVia(t, ctx, db, q)
		if !strings.Contains(plan, "COVERING") {
			t.Fatalf("expected a COVERING leg under the union (the defect surface), got: %s", plan)
		}
		got := floats(q)
		if len(got) != 3 {
			t.Errorf("DISTINCT over covering∪base = %d rows (%v), want 3 (dedup across access paths)", len(got), got)
		}
		seen := map[float64]bool{}
		for _, f := range got {
			seen[f] = true
		}
		if !seen[2.5] || !seen[3.5] || !seen[10.5] {
			t.Errorf("distinct values = %v, want {2.5, 3.5, 10.5}", got)
		}
	})
}
