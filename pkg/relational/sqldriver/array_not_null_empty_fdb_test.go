package sqldriver_test

// CQ-89 — a stored NOT NULL array that is EMPTY must read back as the EMPTY
// ARRAY, never as SQL NULL.
//
// This is NOT a wire concern. A non-nullable array is a FLAT repeated field in
// both engines, and an empty repeated field writes nothing, so empty and
// absent are byte-identical — Go's bytes already match Java's. The distinction
// is therefore made on READ, from the TYPE: a column whose type forbids NULL
// must materialize an absent repeated field as []. Java does it by branching
// on isRepeated() BEFORE any presence test, in the one helper every field read
// funnels through (MessageHelpers.getFieldOnMessage, MessageHelpers.java:125):
//
//	if (field.isRepeated()) {
//	    int count = message.getRepeatedFieldCount(field);
//	    List<Object> list = new ArrayList<>(count);
//	    ...
//	    return list;                       // count == 0 -> empty list, never null
//	}
//	if (field.hasDefaultValue() || message.hasField(field)) { ... } else { return null; }
//
// Java states the intent verbatim at MessageHelpers.java:590-600: "A repeated
// field represents an array and must be treated as always present … even when
// getRepeatedFieldCount() is 0 (which is a valid state that represents the
// empty array [])."
//
// Go's row builder (executor.protoToPositional) tested presence FIRST, and
// protoreflect's Has() is len(list) > 0 for a repeated field, so an empty NOT
// NULL array became a nil slot = SQL NULL. The symptom: `IS NULL` answered TRUE
// and `= []` answered UNKNOWN on a column the type forbids to be NULL.
//
// The NULLABLE sibling is a different encoding and must NOT be flattened into
// this: it is stored as the singular NullableArrayWrapper message, where
// presence is REAL — an absent wrapper is NULL, a present wrapper holding zero
// values is []. Both arms fall out of the repeated-first branch, and this test
// pins both directions so a fix to one cannot silently break the other.
//
// Expected values are the upstream corpus's own assertions
// (arrays-operators.yamsql), i.e. measured Java behaviour, not a prediction.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func TestFDB_NonNullableArrayEmptyReadsBack(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nnarr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nnarr")
	// `arr` is NULLABLE (wrapper-encoded), `arr_nn` is NOT NULL (flat
	// repeated). `tag` gives an indexed, non-array access path so the same
	// column can be reached through an index scan rather than a full scan.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nnarr "+
			"CREATE TABLE t (pk BIGINT, tag BIGINT, arr INTEGER ARRAY, arr_nn INTEGER ARRAY NOT NULL, PRIMARY KEY (pk)) "+
			"CREATE INDEX t_tag AS SELECT tag FROM t")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nnarr/s WITH TEMPLATE nnarr")
	dsn := fmt.Sprintf("fdbsql:///testdb_nnarr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The three storage states, one per row:
	//   pk 1 — NULL nullable array,  EMPTY non-nullable array
	//   pk 2 — EMPTY nullable array, EMPTY non-nullable array
	//   pk 3 — populated both
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (pk, tag, arr, arr_nn) VALUES (1, 10, NULL, []), (2, 20, [], []), (3, 30, [7], [7, 8])")

	scanCol := func(t *testing.T, q string) any {
		t.Helper()
		var got any
		if err := db.QueryRowContext(ctx, q).Scan(&got); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return got
	}
	empty := []any{}

	t.Run("projection", func(t *testing.T) {
		t.Parallel()
		// The NOT NULL column is the empty array on both empty rows, and
		// carries its elements when populated. A nil here is the defect.
		for _, pk := range []int64{1, 2} {
			q := fmt.Sprintf("SELECT arr_nn FROM t WHERE pk = %d", pk)
			got := scanCol(t, q)
			if !reflect.DeepEqual(got, empty) {
				t.Errorf("%s: got %#v (%T), want %#v — an empty NOT NULL array read back as NULL", q, got, got, empty)
			}
		}
		if got := scanCol(t, "SELECT arr_nn FROM t WHERE pk = 3"); !reflect.DeepEqual(got, []any{int64(7), int64(8)}) {
			t.Errorf("populated arr_nn: got %#v, want [7 8]", got)
		}
		// The NULLABLE column keeps its three-way distinction: a stored
		// NULL stays NULL, a stored [] is the empty array.
		if got := scanCol(t, "SELECT arr FROM t WHERE pk = 1"); got != nil {
			t.Errorf("NULL nullable arr: got %#v, want nil — the wrapper's real absence must survive the repeated-first branch", got)
		}
		if got := scanCol(t, "SELECT arr FROM t WHERE pk = 2"); !reflect.DeepEqual(got, empty) {
			t.Errorf("empty nullable arr: got %#v, want %#v", got, empty)
		}
		if got := scanCol(t, "SELECT arr FROM t WHERE pk = 3"); !reflect.DeepEqual(got, []any{int64(7)}) {
			t.Errorf("populated nullable arr: got %#v, want [7]", got)
		}
	})

	t.Run("index_backed_projection", func(t *testing.T) {
		t.Parallel()
		// Same column reached through the t_tag index rather than a full
		// scan: the array is not covered by the index, so the row is
		// fetched and materialized by the same row builder. Pins that the
		// fix is at a layer the index path also funnels through.
		got := scanCol(t, "SELECT arr_nn FROM t WHERE tag = 10")
		if !reflect.DeepEqual(got, empty) {
			t.Errorf("index-backed arr_nn: got %#v, want %#v", got, empty)
		}
		if got := scanCol(t, "SELECT arr FROM t WHERE tag = 10"); got != nil {
			t.Errorf("index-backed nullable arr: got %#v, want nil", got)
		}
	})

	t.Run("predicates", func(t *testing.T) {
		t.Parallel()
		pks := func(t *testing.T, q string) []int64 {
			t.Helper()
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("query %q: %v", q, err)
			}
			defer rows.Close()
			out := []int64{}
			for rows.Next() {
				var pk int64
				if err := rows.Scan(&pk); err != nil {
					t.Fatalf("scan %q: %v", q, err)
				}
				out = append(out, pk)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows %q: %v", q, err)
			}
			return out
		}
		for _, c := range []struct {
			q    string
			want []int64
		}{
			// The corpus's own shape: `= []` selects the empty rows.
			{`SELECT pk FROM t WHERE arr_nn = [] ORDER BY pk`, []int64{1, 2}},
			{`SELECT pk FROM t WHERE arr_nn = CAST([] AS INTEGER ARRAY) ORDER BY pk`, []int64{1, 2}},
			// A NOT NULL column is never NULL, on any row.
			{`SELECT pk FROM t WHERE arr_nn IS NULL`, []int64{}},
			{`SELECT pk FROM t WHERE arr_nn IS NOT NULL ORDER BY pk`, []int64{1, 2, 3}},
			// The nullable column still distinguishes all three states.
			{`SELECT pk FROM t WHERE arr IS NULL`, []int64{1}},
			{`SELECT pk FROM t WHERE arr = []`, []int64{2}},
			{`SELECT pk FROM t WHERE arr IS NOT NULL ORDER BY pk`, []int64{2, 3}},
		} {
			if got := pks(t, c.q); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: got %v, want %v", c.q, got, c.want)
			}
		}
	})

	t.Run("join_projects_the_column", func(t *testing.T) {
		t.Parallel()
		// A join merges leg rows into a new positional row; the empty
		// array has to survive that copy on both legs. Row 1's NOT NULL
		// array is empty and row 3's is populated.
		var lhs, rhs any
		q := `SELECT L.arr_nn, R.arr_nn FROM t L, t R WHERE L.pk = 1 AND R.pk = 3`
		if err := db.QueryRowContext(ctx, q).Scan(&lhs, &rhs); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if !reflect.DeepEqual(lhs, empty) {
			t.Errorf("join lhs arr_nn: got %#v, want %#v", lhs, empty)
		}
		if !reflect.DeepEqual(rhs, []any{int64(7), int64(8)}) {
			t.Errorf("join rhs arr_nn: got %#v, want [7 8]", rhs)
		}
		// Equality across legs: two empty NOT NULL arrays are EQUAL, and
		// an empty one is UNEQUAL to a populated one. Under the defect
		// both legs were NULL and the comparison was UNKNOWN, so neither
		// row came back.
		var eq bool
		q = `SELECT L.arr_nn = R.arr_nn FROM t L, t R WHERE L.pk = 1 AND R.pk = 2`
		if err := db.QueryRowContext(ctx, q).Scan(&eq); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if !eq {
			t.Errorf("%s: got false, want true ([] = [])", q)
		}
		q = `SELECT L.arr_nn = R.arr_nn FROM t L, t R WHERE L.pk = 1 AND R.pk = 3`
		if err := db.QueryRowContext(ctx, q).Scan(&eq); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if eq {
			t.Errorf("%s: got true, want false ([] <> [7,8])", q)
		}
	})
}
