package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_RecordConstructorInExpressionPosition pins the record constructor as
// a VALUE — `SELECT (1, 1.0, 'a', true)` and, through it, `COALESCE(null,
// (...))` — rather than only as a DML target.
//
// Java builds it in ExpressionVisitor.visitRecordConstructor
// (ExpressionVisitor.java:918-925 → RecordConstructorValue.ofColumns), with
// unnamed elements taking the ordinal field keys `_0`, `_1`, … Go had only the
// one-element unwrap (the parser's shape for a parenthesised expression) and
// declined everything else with "RecordConstructor with N elements", so every
// query carrying a struct literal outside DML died 0AF00 at the planner.
//
// The COALESCE half needs no new mechanism and that is the point of pinning it
// here: NULL→RECORD is already an edge of the promotion lattice
// (Java PromoteValue.java:89) and the result type is already the field-name
// merge in MaximumType (Java Type.java:638-666). Only the producer was
// missing, so a test that exercised the lattice directly would have passed
// with the defect fully present.
func TestFDB_RecordConstructorInExpressionPosition(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_rcexpr")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_rcexpr")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE rcexpr_tmpl "+
			"CREATE TYPE AS STRUCT S4 (a BIGINT, b DOUBLE, c STRING, d BOOLEAN) "+
			"CREATE TABLE C (id BIGINT, s S4, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_rcexpr/s WITH TEMPLATE rcexpr_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_rcexpr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()
	g.Expect(db.ExecContext(ctx, "INSERT INTO C VALUES (1, (7, 2.5, 'x', true))")).Error().NotTo(gomega.HaveOccurred())

	// one() runs a single-column, single-row query and returns the field map
	// of the struct it produced.
	one := func(t *testing.T, query string) map[string]any {
		t.Helper()
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx, query)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "query: %s", query)
		defer rows.Close()
		g.Expect(rows.Next()).To(gomega.BeTrue(), "query returned no rows: %s", query)
		var v any
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		m, ok := v.(map[string]any)
		g.Expect(ok).To(gomega.BeTrue(), "expected a record, got %T from %s", v, query)
		return m
	}

	// The MIXED-WIDTH shape is deliberate. A record of four same-typed fields
	// would pass even if the constructor collapsed every element to one type,
	// so the pin carries a BIGINT, a DOUBLE, a STRING and a BOOLEAN and
	// asserts the Go type of each: the DOUBLE is the one that would silently
	// arrive as an integer if the element types were not carried through.
	wantMixed := map[string]any{
		"_0": int64(1),
		"_1": float64(1.0),
		"_2": "a",
		"_3": true,
	}

	t.Run("bare_record_constructor_is_a_value", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(one(t, "SELECT (1, 1.0, 'a', true) FROM C")).To(gomega.Equal(wantMixed))
	})

	// The corpus shape (functions.yamsql): the NULL operand promotes to the
	// record type through the NULL→RECORD lattice edge and COALESCE returns
	// the second operand unchanged.
	t.Run("coalesce_null_then_record_promotes", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(one(t, "SELECT COALESCE(null, (1, 1.0, 'a', true)) FROM C")).To(gomega.Equal(wantMixed))
	})

	// The reverse operand order — the record is FIRST and non-NULL, so
	// COALESCE short-circuits on it. This is the arm that would still pass if
	// promotion ran only on the trailing operand.
	t.Run("coalesce_record_then_null_short_circuits", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(one(t, "SELECT COALESCE((1, 2), null) FROM C")).To(gomega.Equal(
			map[string]any{"_0": int64(1), "_1": int64(2)}))
	})

	// Named elements take their given names rather than the ordinal keys —
	// Java's expressionWithOptionalName arm. Without it every constructor
	// would be positionally named and an `AS` in a struct literal would be
	// silently dropped.
	t.Run("named_elements_keep_their_names", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(one(t, "SELECT (1 AS x, 'q' AS y) FROM C")).To(gomega.Equal(
			map[string]any{"X": int64(1), "Y": "q"}))
	})

	// The negative that keeps the unwrap intact: a ONE-element unnamed
	// constructor is the parser's shape for a parenthesised expression, so it
	// must NOT become a one-field record. SQL has no one-tuple literal to
	// distinguish it from, and turning it into a record would re-type every
	// parenthesised scalar in the language.
	t.Run("single_element_paren_still_unwraps_to_a_scalar", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx, "SELECT (1 + 2) FROM C")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		g.Expect(rows.Next()).To(gomega.BeTrue())
		var v any
		g.Expect(rows.Scan(&v)).To(gomega.Succeed())
		g.Expect(v).To(gomega.Equal(int64(3)), "a parenthesised scalar must stay a scalar, not become a record")
	})

	// The coercion half. An ANONYMOUS record literal assigned to a struct
	// column has to bind POSITIONALLY: it carries the ordinal keys `_0`, `_1`,
	// which no target struct declares, so a name-keyed coercion reports the
	// whole literal as undeclared fields. Java never reaches that state — it
	// applies the target type's field names by position while visiting the
	// constructor — but Go builds the constructor in expression position with
	// no target (a COALESCE operand has none until the assignment coerces it),
	// so the binding happens at the coercion instead.
	//
	// The shape is the corpus's (inserts-updates-deletes.yamsql): the literal
	// is an operand of a COALESCE, not the assignment's direct right-hand
	// side, which is what puts it out of reach of the DML target-type
	// push-down.
	t.Run("anonymous_literal_coerces_positionally_through_coalesce", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(db.ExecContext(ctx, "INSERT INTO C VALUES (2, null)")).Error().NotTo(gomega.HaveOccurred())
		_, err := db.ExecContext(ctx,
			"UPDATE C SET s = COALESCE(s, (9, 8.5, 'z', false)) WHERE id = 2")
		g.Expect(err).NotTo(gomega.HaveOccurred())

		rows, err := db.QueryContext(ctx, "SELECT s.a, s.b, s.c, s.d FROM C WHERE id = 2")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		g.Expect(rows.Next()).To(gomega.BeTrue())
		var a int64
		var b float64
		var c string
		var d bool
		g.Expect(rows.Scan(&a, &b, &c, &d)).To(gomega.Succeed())
		// Every field asserted, and each of a DIFFERENT type: a positional
		// bind that shifted by one, or a coercion that dropped a field, is
		// only visible when the values differ across positions.
		g.Expect([]any{a, b, c, d}).To(gomega.Equal([]any{int64(9), 8.5, "z", false}))
	})

	// A NAMED literal in DECLARATION ORDER assigns. This is the control for
	// the out-of-order case below: with names agreeing with positions, a
	// positional rule and a by-name rule are indistinguishable, so this arm
	// proves only that naming the fields does not itself break the assignment.
	t.Run("named_literal_in_declaration_order_assigns", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(db.ExecContext(ctx, "INSERT INTO C VALUES (3, null)")).Error().NotTo(gomega.HaveOccurred())
		g.Expect(db.ExecContext(ctx,
			"UPDATE C SET s = (11 AS a, 3.5 AS b, 'w' AS c, true AS d) WHERE id = 3")).
			Error().NotTo(gomega.HaveOccurred())

		rows, err := db.QueryContext(ctx, "SELECT s.a, s.b, s.c, s.d FROM C WHERE id = 3")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		g.Expect(rows.Next()).To(gomega.BeTrue())
		var a int64
		var b float64
		var c string
		var d bool
		g.Expect(rows.Scan(&a, &b, &c, &d)).To(gomega.Succeed())
		g.Expect([]any{a, b, c, d}).To(gomega.Equal([]any{int64(11), 3.5, "w", true}))
	})

	// The out-of-order named literal is REJECTED, and that is Java's answer
	// too — measured, not assumed. Java binds a targeted record constructor by
	// POSITION and overrides the element's own name with the target field's
	// (ExpressionVisitor.java:1040-1075; the "reorderings" that would honour
	// names come from an INSERT's explicit column list, never from `AS` inside
	// the constructor), so `(11 AS a, 'w' AS c, 3.5 AS b, true AS d)` tries to
	// put the STRING into the DOUBLE field and dies on the type.
	//
	// Live-JVM (conformance/record_constructor_java_probe_test.go, probe
	// update_named_out_of_order): JAVA "A value cannot be assigned to a
	// variable because the type of the value does not match the type of the
	// variable and cannot be promoted to the type of the variable."; GO 22000
	// "field \"C\" cannot be assigned to target field \"B\"". Same refusal,
	// and Go names which field it was.
	//
	// This arm is what keeps the positional coercion HONEST: without it, a
	// rule that bound every literal positionally regardless of names would
	// pass every other test in this file while silently transposing a row.
	t.Run("named_literal_out_of_order_is_rejected", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(db.ExecContext(ctx, "INSERT INTO C VALUES (4, null)")).Error().NotTo(gomega.HaveOccurred())
		_, err := db.ExecContext(ctx,
			"UPDATE C SET s = (11 AS a, 'w' AS c, 3.5 AS b, true AS d) WHERE id = 4")
		g.Expect(err).To(gomega.HaveOccurred(),
			"a named literal whose names disagree with the target's positions must not silently transpose")
		g.Expect(err.Error()).To(gomega.ContainSubstring(`field "C" cannot be assigned to target field "B"`))
	})

	// A parenthesised PREDICATE takes the same unwrap through the same
	// function, in predicate position. It is pinned because the multi-element
	// arm added below it shares the entry point, and a regression there would
	// turn `WHERE (id = 1)` into a record-typed WHERE.
	t.Run("parenthesised_predicate_still_unwraps", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx, "SELECT id FROM C WHERE (id = 1)")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		defer rows.Close()
		g.Expect(rows.Next()).To(gomega.BeTrue())
		var id int64
		g.Expect(rows.Scan(&id)).To(gomega.Succeed())
		g.Expect(id).To(gomega.Equal(int64(1)))
	})
}
