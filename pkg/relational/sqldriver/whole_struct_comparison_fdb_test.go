package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_WholeStructComparison pins what happens when a whole STRUCT column —
// not one of its fields — is used as a comparison operand.
//
// Java rejects it at construction: RelOpValue asserts every operand satisfies
// isSupportedOperandType (primitive / enum / uuid / array / none;
// RelOpValue.java:320-322), checked before any compatibility question
// (RelOpValue.java:333,:345,:350). A RECORD comparand fails that assert.
//
// Go used to build the predicate anyway, because the SQL layer typed a struct
// column UNKNOWN and every type-keyed gate therefore admitted it. The result
// was SILENT-WRONG, not an error: the row-time comparator has no record arm,
// every row evaluated UNKNOWN, and `WHERE home = home` returned ZERO ROWS over
// a table whose every non-NULL row satisfies it. Only the struct-vs-LITERAL
// spelling was rejected, and only incidentally, by a translator decline.
//
// The dimension that was unprobed is the one this test exists for: every
// pre-existing struct test compared a FIELD (`home.city = 'sf'`), which is a
// primitive comparand and therefore could not express the defect at all.
//
// IS NULL / IS NOT NULL are deliberately NOT part of the rejection and are
// pinned here as answering correctly. Java rejects them too (its unary arm
// runs the same assert), but that rejection is upstream issue 3700 — a
// limitation filed as a bug, with the shapes commented out in vector.yamsql —
// and answering them is a read-side extension with nothing on the wire at
// stake. See RFC-204 §4.4's amendment and
// conformance/whole_struct_comparison_java_probe_test.go for the live-JVM
// measurement of both halves.
func TestFDB_WholeStructComparison(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_wholestruct")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_wholestruct")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE wholestruct_tmpl "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT) "+
			"CREATE TABLE T_S (id BIGINT, home ADDR, other ADDR, PRIMARY KEY (id))")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_wholestruct/s WITH TEMPLATE wholestruct_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_wholestruct?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	// Row 1 has EQUAL structs in both columns — that is what makes the
	// zero-rows answer unambiguously wrong rather than merely empty.
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO T_S VALUES (1, ('sf', 94100), ('sf', 94100))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx,
		"INSERT INTO T_S VALUES (2, NULL, NULL)")).Error().NotTo(gomega.HaveOccurred())

	// Every operator, and both the predicate and the projection position. The
	// defect was NOT specific to the self-compare: `home = other` over two
	// distinct struct columns was equally silent-wrong, and a simpler
	// self-compare-only pin would have left that arm unguarded.
	rejected := []struct{ name, sql string }{
		{"self_equality", "SELECT id FROM T_S WHERE home = home"},
		{"column_column_equality", "SELECT id FROM T_S WHERE home = other"},
		{"column_column_inequality", "SELECT id FROM T_S WHERE home <> other"},
		{"column_column_ordering", "SELECT id FROM T_S WHERE home > other"},
		{"column_column_ordering_eq", "SELECT id FROM T_S WHERE home <= other"},
		{"projection_position", "SELECT home = other FROM T_S"},
		{"distinct_from", "SELECT id FROM T_S WHERE home IS DISTINCT FROM other"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			rows, err := db.QueryContext(ctx, tc.sql)
			if err == nil {
				// Draining first makes the failure message carry the WRONG
				// ANSWER, which is the whole point — an empty result set from
				// a query that should match row 1 is the defect's signature.
				var got []string
				cols, _ := rows.Columns()
				for rows.Next() {
					vals := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range vals {
						ptrs[i] = &vals[i]
					}
					if scanErr := rows.Scan(ptrs...); scanErr != nil {
						break
					}
					got = append(got, fmt.Sprintf("%v", vals))
				}
				rows.Close()
				t.Fatalf("a whole-struct comparison was ACCEPTED and answered %v — "+
					"Java rejects the shape (RelOpValue.java:333,:345,:350) and Go has no "+
					"record comparator, so any answer here is manufactured: %s", got, tc.sql)
			}
			g.Expect(err.Error()).To(gomega.ContainSubstring("0AF00"),
				"whole-struct comparisons reject as unsupported: %s", tc.sql)
		})
	}

	// The struct-vs-LITERAL spelling was already rejected before the operand
	// gate existed, by a translator decline. It is pinned at the same CODE so
	// the surface is uniform: a user cannot tell which internal path refused.
	t.Run("struct_vs_record_literal", func(t *testing.T) {
		g := gomega.NewWithT(t)
		_, err := db.QueryContext(ctx, "SELECT id FROM T_S WHERE home = ('sf', 94100)")
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(err.Error()).To(gomega.ContainSubstring("0AF00"))
	})

	// The control: a FIELD comparand is primitive and must keep working. This
	// is the arm every pre-existing struct test already covered, kept here so
	// a future widening of the operand gate cannot take it out silently.
	t.Run("struct_field_comparison_still_answers", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx, "SELECT id FROM T_S WHERE home.city = 'sf'")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			g.Expect(rows.Scan(&id)).To(gomega.Succeed())
			ids = append(ids, id)
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(ids).To(gomega.Equal([]int64{1}))
	})

	// The extension the gate must NOT eat. Java errors on all three; Go
	// answers them, and the answers are the correct ones.
	t.Run("whole_struct_is_null_is_an_extension_and_answers", func(t *testing.T) {
		g := gomega.NewWithT(t)
		for _, tc := range []struct {
			sql  string
			want []int64
		}{
			{"SELECT id FROM T_S WHERE home IS NULL", []int64{2}},
			{"SELECT id FROM T_S WHERE home IS NOT NULL", []int64{1}},
		} {
			rows, err := db.QueryContext(ctx, tc.sql)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "IS NULL over a whole struct is a supported extension: %s", tc.sql)
			var ids []int64
			for rows.Next() {
				var id int64
				g.Expect(rows.Scan(&id)).To(gomega.Succeed())
				ids = append(ids, id)
			}
			g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
			rows.Close()
			g.Expect(ids).To(gomega.Equal(tc.want), "query: %s", tc.sql)
		}
	})
}
