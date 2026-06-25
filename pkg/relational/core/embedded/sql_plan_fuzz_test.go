package embedded

import (
	"context"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"
)

// FuzzSQLPlan closes the front-end half of the P0.3-F gap: a SQL *string* driven
// through parse → semantic analysis → Cascades SELECT planning, asserting no
// panic. Unlike FuzzConvertAndPlan (which starts from a programmatically-built
// LogicalOperator, skipping the parser + SQL→logical conversion) and FuzzParse
// (parser only), this exercises the panic-prone SQL→plan front-end end to end.
//
// It deliberately calls planSelectCascades DIRECTLY, NOT through QueryContext, so
// a planner/semantic panic is NOT swallowed by the db/sql boundary recover — the
// fuzzer sees it. (The complementary FuzzSQL_QueryContext in pkg/relational/
// sqldriver proves no panic ESCAPES that boundary, the never-panic-to-caller
// guarantee.) No FDB needed: with sess.DB == nil, statistics fetch no-ops.
func FuzzSQLPlan(f *testing.F) {
	tmpl, err := buildSchemaTemplateFromDDL(ordersSchema)
	if err != nil {
		f.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	conn := &EmbeddedConnection{
		sess:                     &session.Session{Schema: "s"},
		planCache:                NewPlanCache(256),
		slowQueryThresholdMicros: defaultSlowQueryThresholdMicros(),
	}
	g := newCascadesGenerator(conn)
	ctx := context.Background()

	seeds := []string{
		"SELECT 1",
		"SELECT id, amount FROM orders WHERE id = 1",
		"SELECT status, COUNT(*) FROM orders GROUP BY status HAVING COUNT(*) > 1",
		"SELECT a.id FROM orders a, orders b WHERE a.id = b.id",
		"SELECT * FROM orders ORDER BY amount DESC LIMIT 5 OFFSET 2",
		"SELECT UPPER(status) FROM orders",
		"SELECT id FROM orders WHERE id IN (SELECT customer_id FROM orders)",
		"SELECT CASE WHEN amount > 0 THEN 'p' ELSE 'n' END FROM orders",
		"SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM orders b WHERE b.id = orders.id)",
		"SELECT amount / 0 FROM orders",
		"SELECT id FROM orders UNION ALL SELECT id FROM orders",
		"WITH c AS (SELECT id FROM orders) SELECT id FROM c",
		"",
		"SELECT",
		"SELECT FROM",
		"SELECT id FROM nonexistent_table",
		"SELECT no_such_col FROM orders",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		root, perr := parser.Parse(sql)
		if perr != nil {
			return // parse errors are fine — FuzzParse covers the parser itself
		}
		stmts := root.Statements()
		if stmts == nil {
			return
		}
		all := stmts.AllStatement()
		if len(all) == 0 {
			return
		}
		sel := all[0].SelectStatement()
		if sel == nil {
			return // only SELECT is FDB-free here; DML/DDL covered by the e2e fuzz
		}
		q := sel.Query()
		if q == nil {
			return
		}
		// A returned error is fine (bad column, unsupported shape, type error, …);
		// only a panic is a bug. The fuzzer reports any panic as a crash.
		_, _ = g.planSelectCascades(ctx, q, md, false)
	})
}
