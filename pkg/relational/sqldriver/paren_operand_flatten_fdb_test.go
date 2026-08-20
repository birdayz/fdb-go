package sqldriver_test

// `( expr )` is a one-field RECORD in this grammar, and consumption AS AN
// ARGUMENT is what turns it back into a scalar.
//
// That is Java's design, not a Go workaround: SQL gives `(expr)` and a
// one-tuple literal the same parse and no syntax separates them, so
// RecordConstructorValue always builds the record and
// SemanticAnalyzer.resolveScalarFunction flattens it back at every function
// argument (flattenSingleItemRecords, defaulted true by
// BaseVisitor.resolveFunction). `SELECT (val)` is a struct because a projection
// consumes nothing; `(val) + 1` is a BIGINT because `+` consumes it.
//
// Go mirrored that with walkFunctionOperand — for arithmetic, comparison,
// bitwise, logical, NOT, IS NULL, LIKE, BETWEEN, IN's LEFT operand, and a named
// scalar call's arguments. Java's ExpressionVisitor calls resolveFunction at
// more positions than that, and TWO of them had no Go counterpart:
//
//	ExpressionVisitor.java:425  resolveFunction("__pick_value", …)      CASE arms
//	ExpressionVisitor.java:656  resolveFunction("__internal_array", …)  IN-list items
//
// Both were measured against the live JVM before the repair
// (conformance/paren_flatten_java_probe_test.go): Java answered every shape
// below, and Go either failed to plan or returned a struct where a scalar
// belonged.
//
//	CASE WHEN a = 1 THEN (5) ELSE 6 END     java 5    go 0AF00 "placeholder type is not exact"
//	CASE WHEN a = 1 THEN (5) ELSE (6) END   java 5    go a STRUCT column
//	b IN ((10), 20)                         java rows go 0AF00 "comparison operand of complex type"
//
// The two failure modes are worth separating. The 0AF00 is loud and merely
// blocks a query. The struct-typed column is the dangerous one: it plans, it
// runs, and it hands back a value of the wrong TYPE — which is why the
// all-parenthesized spelling is pinned here explicitly rather than being
// treated as the same case as the mixed one.
//
// WHAT MUST NOT FLATTEN is pinned just as hard. FlattenRecordWithOneField
// collapses a ONE-field record only, so `THEN (5, 6)` stays the struct both
// engines return, and `IN ((NULL))` still raises the IN-list NULL rejection —
// the flatten runs BEFORE that check precisely so a parenthesized NULL is not
// smuggled past it as a record.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"github.com/onsi/gomega"
)

func TestFDB_ParenthesizedOperandFlattens(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_paren_flatten", "parenflat",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_b ON t (b) ")
	//  id=1: a=1  b=10  s='x'   f=true
	//  id=2: a=2  b=20  s='y'   f=false
	//  id=3: a=1  b=30  s=NULL  f=NULL
	w.Exec("INSERT INTO t (id, a, b, s, f) VALUES " +
		"(1, 1, 10, 'x', true), (2, 2, 20, 'y', false), (3, 1, 30, NULL, NULL)")

	// ---- searched CASE arms -------------------------------------------------
	//
	// Each case pins the parenthesized spelling AND its bare twin to the same
	// expected rows, so a failure is attributable to the parentheses and to
	// nothing about CASE itself.
	t.Run("searched_case_arms", func(t *testing.T) {
		w := w.Sub(t)
		cases := []struct {
			name       string
			bare, pars string
			want       []string
		}{
			{
				name: "THEN parenthesized",
				bare: "CASE WHEN a = 1 THEN 5 ELSE 6 END",
				pars: "CASE WHEN a = 1 THEN (5) ELSE 6 END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "ELSE parenthesized",
				bare: "CASE WHEN a = 1 THEN 5 ELSE 6 END",
				pars: "CASE WHEN a = 1 THEN 5 ELSE (6) END",
				want: []string{"5", "6", "5"},
			},
			{
				// The shape that PLANNED and returned a struct column rather
				// than failing loudly.
				name: "both arms parenthesized",
				bare: "CASE WHEN a = 1 THEN 5 ELSE 6 END",
				pars: "CASE WHEN a = 1 THEN (5) ELSE (6) END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "nested parentheses collapse all the way",
				bare: "CASE WHEN a = 1 THEN 5 ELSE 6 END",
				pars: "CASE WHEN a = 1 THEN ((5)) ELSE (((6))) END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "a parenthesized COLUMN, not a literal",
				bare: "CASE WHEN a = 1 THEN b ELSE 0 END",
				pars: "CASE WHEN a = 1 THEN (b) ELSE 0 END",
				want: []string{"10", "0", "30"},
			},
			{
				name: "a parenthesized arithmetic expression",
				bare: "CASE WHEN a = 1 THEN b + 1 ELSE 0 END",
				pars: "CASE WHEN a = 1 THEN (b + 1) ELSE 0 END",
				want: []string{"11", "0", "31"},
			},
			{
				name: "STRING arms",
				bare: "CASE WHEN a = 1 THEN 'p' ELSE 'q' END",
				pars: "CASE WHEN a = 1 THEN ('p') ELSE ('q') END",
				want: []string{"p", "q", "p"},
			},
			{
				// No ELSE: the implicit default is NULL, and the parenthesized
				// THEN must not disturb it.
				name: "no ELSE arm",
				bare: "CASE WHEN a = 2 THEN 5 END",
				pars: "CASE WHEN a = 2 THEN (5) END",
				want: []string{"NULL", "5", "NULL"},
			},
			{
				name: "a parenthesized NULL arm",
				bare: "CASE WHEN a = 1 THEN NULL ELSE 7 END",
				pars: "CASE WHEN a = 1 THEN (NULL) ELSE 7 END",
				want: []string{"NULL", "7", "NULL"},
			},
			{
				name: "two WHEN alternatives, both parenthesized",
				bare: "CASE WHEN a = 1 THEN 5 WHEN a = 2 THEN 6 ELSE 7 END",
				pars: "CASE WHEN a = 1 THEN (5) WHEN a = 2 THEN (6) ELSE (7) END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "a CASE nested inside a parenthesized arm",
				bare: "CASE WHEN a = 1 THEN CASE WHEN b > 20 THEN 8 ELSE 9 END ELSE 0 END",
				pars: "CASE WHEN a = 1 THEN (CASE WHEN b > 20 THEN 8 ELSE 9 END) ELSE 0 END",
				want: []string{"9", "0", "8"},
			},
			{
				// The condition arm keeps its own repair (predicate-first); this
				// pins that the two repairs compose rather than fight.
				name: "parenthesized CONDITION and parenthesized ARMS together",
				bare: "CASE WHEN a = 1 AND b = 10 THEN 5 ELSE 6 END",
				pars: "CASE WHEN (a = 1 AND b = 10) THEN (5) ELSE (6) END",
				want: []string{"5", "6", "6"},
			},
		}
		for _, c := range cases {
			w.Want(c.name+" [bare]",
				fmt.Sprintf("SELECT %s FROM t ORDER BY id", c.bare), c.want)
			w.Want(c.name+" [parenthesized]",
				fmt.Sprintf("SELECT %s FROM t ORDER BY id", c.pars), c.want)
		}
	})

	// ---- simple CASE --------------------------------------------------------
	//
	// A Go EXTENSION: Java's BaseVisitor.visitCaseExpressionFunctionCall is
	// `visitChildren`, so the visitor does not implement this form at all and
	// there is no Java answer to match. It is pinned anyway, and flattens the
	// same way, because an operand position that behaves differently from every
	// other operand position is a trap regardless of what Java does.
	t.Run("simple_case_operands", func(t *testing.T) {
		w := w.Sub(t)
		cases := []struct {
			name       string
			bare, pars string
			want       []string
		}{
			{
				name: "parenthesized DISCRIMINATOR",
				bare: "CASE a WHEN 1 THEN 5 ELSE 6 END",
				pars: "CASE (a) WHEN 1 THEN 5 ELSE 6 END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "parenthesized WHEN operand",
				bare: "CASE a WHEN 1 THEN 5 ELSE 6 END",
				pars: "CASE a WHEN (1) THEN 5 ELSE 6 END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "parenthesized THEN and ELSE",
				bare: "CASE a WHEN 1 THEN 5 ELSE 6 END",
				pars: "CASE a WHEN 1 THEN (5) ELSE (6) END",
				want: []string{"5", "6", "5"},
			},
			{
				name: "every operand parenthesized at once",
				bare: "CASE a WHEN 1 THEN 5 WHEN 2 THEN 6 ELSE 7 END",
				pars: "CASE (a) WHEN (1) THEN (5) WHEN (2) THEN (6) ELSE (7) END",
				want: []string{"5", "6", "5"},
			},
		}
		for _, c := range cases {
			w.Want(c.name+" [bare]",
				fmt.Sprintf("SELECT %s FROM t ORDER BY id", c.bare), c.want)
			w.Want(c.name+" [parenthesized]",
				fmt.Sprintf("SELECT %s FROM t ORDER BY id", c.pars), c.want)
		}
	})

	// ---- IN-list items ------------------------------------------------------
	t.Run("in_list_items", func(t *testing.T) {
		w := w.Sub(t)
		cases := []struct {
			name       string
			bare, pars string
			want       []string
		}{
			{
				name: "first item parenthesized",
				bare: "b IN (10, 20)", pars: "b IN ((10), 20)",
				want: []string{"1", "2"},
			},
			{
				name: "every item parenthesized",
				bare: "b IN (10, 20)", pars: "b IN ((10), (20))",
				want: []string{"1", "2"},
			},
			{
				name: "nested parentheses on an item",
				bare: "b IN (10, 20)", pars: "b IN (((10)), 20)",
				want: []string{"1", "2"},
			},
			{
				name: "a single parenthesized item",
				bare: "b IN (30)", pars: "b IN ((30))",
				want: []string{"3"},
			},
			{
				name: "NOT IN with parenthesized items",
				bare: "b NOT IN (10, 20)", pars: "b NOT IN ((10), (20))",
				want: []string{"3"},
			},
			{
				name: "a parenthesized ARITHMETIC item",
				bare: "b IN (5 + 5, 20)", pars: "b IN ((5 + 5), 20)",
				want: []string{"1", "2"},
			},
			{
				name: "STRING items",
				bare: "s IN ('x', 'y')", pars: "s IN (('x'), ('y'))",
				want: []string{"1", "2"},
			},
			{
				// Both operands parenthesized — the LEFT one already flattened
				// through walkOperand before this repair, so this arm proves the
				// two flattens compose.
				name: "both sides parenthesized",
				bare: "b IN (10, 20)", pars: "(b) IN ((10), (20))",
				want: []string{"1", "2"},
			},
		}
		for _, c := range cases {
			w.Want(c.name+" [bare]",
				fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", c.bare), c.want)
			w.Want(c.name+" [parenthesized]",
				fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", c.pars), c.want)
		}
	})
}

// TestFDB_ParenthesizedOperandDoesNotOverFlatten is the other half, and the
// half a repair like this gets wrong: flattening MORE than Java does.
//
// FlattenRecordWithOneField collapses a one-field record and nothing else, so a
// multi-element `( … , … )` keeps its struct type and a parenthesized NULL is
// still a NULL where a NULL is rejected. Without these two arms the repair could
// be "unwrap every paren" and every test above would still pass.
func TestFDB_ParenthesizedOperandDoesNotOverFlatten(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupErrorTestDB(t, "/testdb_paren_noflat", "parennoflat",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1, 1, 10), (2, 2, 20)")

	t.Run("a two-element arm stays a STRUCT", func(t *testing.T) {
		g := gomega.NewWithT(t)
		rows, err := db.QueryContext(ctx,
			"SELECT CASE WHEN a = 1 THEN (5, 6) ELSE (7, 8) END FROM t ORDER BY id")
		g.Expect(err).NotTo(gomega.HaveOccurred(),
			"a multi-element CASE arm must still plan — Java answers it as a struct")
		defer rows.Close()

		var got [][]any
		for rows.Next() {
			var v any
			g.Expect(rows.Scan(&v)).To(gomega.Succeed())
			s, ok := v.(api.Struct)
			g.Expect(ok).To(gomega.BeTrue(),
				"a TWO-element `( … , … )` arm must stay a struct; got %T. If this became a scalar "+
					"the flatten stopped checking the field count and is now unwrapping every "+
					"parenthesis, which changes the TYPE of a column Java returns as a struct", v)
			got = append(got, s.Attributes())
		}
		g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
		g.Expect(got).To(gomega.Equal([][]any{
			{int64(5), int64(6)},
			{int64(7), int64(8)},
		}))
	})

	// A COLUMN as an IN-list item does not plan in Go — `b IN (a, 20)` comes
	// back 0AF00 "Cascades planner could not plan query". That is a limitation
	// of the IN path and NOT of this repair: the bare spelling fails exactly the
	// same way, which is the whole content of the assertion below.
	//
	// It is pinned rather than deleted because it was found by writing the
	// parenthesized case and expecting it to work. Without the pin the next
	// person writes the same case, sees the same 0AF00, and has to re-derive
	// that the parentheses are innocent. If the IN path later learns column
	// items, this test fails and says which arm to move back.
	t.Run("a COLUMN item is unsupported in BOTH spellings, identically", func(t *testing.T) {
		g := gomega.NewWithT(t)
		_, bareErr := db.QueryContext(ctx, "SELECT id FROM t WHERE b IN (a, 20) ORDER BY id")
		_, parErr := db.QueryContext(ctx, "SELECT id FROM t WHERE b IN ((a), 20) ORDER BY id")
		g.Expect(bareErr).To(gomega.HaveOccurred(),
			"a column IN-list item now PLANS. This arm exists only to say the parentheses are "+
				"innocent of the rejection; move both spellings up to the positive cases above")
		g.Expect(parErr).To(gomega.HaveOccurred())
		g.Expect(parenFlattenErrCode(parErr)).To(gomega.Equal(parenFlattenErrCode(bareErr)),
			"the two spellings of an unsupported IN-list item must fail the SAME way. A different "+
				"code for the parenthesized form would mean the flatten changed the shape the "+
				"planner sees rather than restoring it\n  bare: %v\n  paren: %v", bareErr, parErr)
	})

	t.Run("a parenthesized NULL is still rejected in an IN list", func(t *testing.T) {
		g := gomega.NewWithT(t)
		// The bare spelling is the control: it is what the rejection is
		// supposed to look like, and if IT ever stopped erroring the arm below
		// would be pinning the wrong thing.
		_, bareErr := db.QueryContext(ctx, "SELECT id FROM t WHERE b IN (NULL, 10)")
		g.Expect(bareErr).To(gomega.HaveOccurred(),
			"the control changed: a bare NULL in an IN list is no longer rejected, so the "+
				"parenthesized arm below is no longer measuring a smuggled NULL")

		_, parErr := db.QueryContext(ctx, "SELECT id FROM t WHERE b IN ((NULL), 10)")
		g.Expect(parErr).To(gomega.HaveOccurred(),
			"a parenthesized NULL slipped past the IN-list NULL rejection. The item flatten runs "+
				"BEFORE that check for exactly this reason — reorder them and `(NULL)` arrives as a "+
				"one-field record, is not recognised as a NullValue, and reaches the comparison")
		g.Expect(strings.ToUpper(parErr.Error())).To(gomega.ContainSubstring(
			strings.ToUpper(parenFlattenErrCode(bareErr))),
			"the parenthesized NULL is rejected, but not with the rejection the bare spelling "+
				"gets — the two spellings must fail the same way\n  bare: %v\n  paren: %v",
			bareErr, parErr)
	})
}

// parenFlattenErrCode extracts the SQLSTATE from an engine error so two
// rejections can be compared by CODE rather than by message wording, which is
// free to differ. A non-api.Error falls back to the message, which can never
// accidentally match a real code.
func parenFlattenErrCode(err error) string {
	if err == nil {
		return ""
	}
	var e *api.Error
	if errors.As(err, &e) {
		return string(e.Code)
	}
	return err.Error()
}
