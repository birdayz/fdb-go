//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	"fdb.dev/pkg/relational/conformance/plandiff"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A row that flows out of a quantifier names its columns by its record type,
// where a name two columns share, or no name, becomes the column's position
// (Expressions.underlyingAsColumns, Type.Record.normalizeFields). A table
// function call (SemanticAnalyzer.resolveTableFunction) and a recursive CTE
// publish that row; a derived table, a CTE and a view publish their select
// list. Both engines must answer alike, Java's exception class aside.
var _ = Describe("FunctionColumnsConformance", func() {
	It("names a function's columns as the target does", func() {
		o, done := newWSEOracle("fncols_", "FN-COLS")
		defer done()
		const schema = `CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id)) ` +
			`CREATE FUNCTION fd(IN x BIGINT) AS SELECT a.id, b.id FROM t a, t b WHERE a.id = x AND b.id = x ` +
			`CREATE FUNCTION fe(IN x BIGINT) AS SELECT id + 1, id FROM t WHERE id = x ` +
			`CREATE FUNCTION fs(IN x BIGINT) AS SELECT * FROM t a, t b WHERE a.id = x AND b.id = x ` +
			`CREATE FUNCTION fa(IN x BIGINT) AS SELECT id * 2 AS id, id, v FROM t WHERE id = x ` +
			`CREATE FUNCTION fn() AS SELECT a.id, b.id FROM t a, t b WHERE a.id = b.id ` +
			`CREATE VIEW vd AS SELECT a.id, b.id FROM t a, t b WHERE a.id = b.id`
		setup := []string{`INSERT INTO t VALUES (1, 10), (2, 20)`}
		for i, q := range []string{
			`SELECT * FROM fd(1)`,
			`SELECT * FROM fe(1)`,
			`SELECT * FROM fs(1)`,
			`SELECT * FROM fa(1)`,
			`SELECT * FROM fn()`,
			`SELECT * FROM vd`,
			`SELECT q.id FROM fd(1) q`,
			`SELECT q."_0" FROM fd(1) q`,
			`SELECT * FROM (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1) q`,
			`SELECT q.v FROM fa(1) q`,
			`WITH RECURSIVE r AS (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1 UNION ALL SELECT r."_0" + 1, r."_1" FROM r WHERE r."_0" < 3) SELECT * FROM r`,
			`WITH c AS (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1) SELECT * FROM c`,
			`SELECT q."_0" FROM vd q`,
			`SELECT q.id FROM vd q`,
			`SELECT q.id FROM fe(1) q`,
			`SELECT q."_0" FROM fe(1) q`,
			`SELECT q."_1" FROM fa(1) q`,
			`SELECT q.id FROM fa(1) q`,
			`WITH RECURSIVE r AS (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1 UNION ALL SELECT r."_0" + 1, r."_1" FROM r WHERE r."_0" < 3) SELECT r."_0" FROM r`,
			`SELECT c."_0" FROM (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1) c`,
			`WITH c AS (SELECT a.id, b.id FROM t a, t b WHERE a.id = 1 AND b.id = 1) SELECT c."_0" FROM c`,
			`SELECT * FROM fs(1) q`,
			`SELECT q.v FROM fs(1) q`,
		} {
			o.named(schema, setup, fmt.Sprintf("f%02d", i), q)
		}
		javaClass := regexp.MustCompile(`^ERROR (\S+) \S+ "`)
		undefinedColumn := regexp.MustCompile(`^ERROR 42703 .*$`)
		var failures []string
		for _, name := range sortedStringKeys(o.got) {
			javaLine, goLine := o.got[name], o.goGot[name]
			javaLine = javaClass.ReplaceAllString(javaLine, `ERROR $1 "`)
			if undefinedColumn.ReplaceAllString(javaLine, `ERROR 42703`) != undefinedColumn.ReplaceAllString(goLine, `ERROR 42703`) {
				failures = append(failures, fmt.Sprintf("%s: java %s, go %s", name, javaLine, goLine))
			}
		}
		Expect(len(o.got)).To(Equal(23))
		Expect(failures).To(BeEmpty())
	})
})

// named is plain with the result's column names in front of each line.
func (o *wseOracle) named(schema string, setup []string, name, sqlText string) {
	jr := o.java.RunWithSetup(o.ctx, schema, setup, sqlText)
	gr := o.goRunner.RunWithSetup(o.ctx, schema, setup, sqlText)
	render := func(r plandiff.RunResult) string {
		line := wseRender(sqlText, r)
		if r.Err != nil {
			return line
		}
		names := make([]string, len(r.Rows.Columns))
		for i, c := range r.Rows.Columns {
			names[i] = c.Name
		}
		return fmt.Sprintf("%q %s", names, line)
	}
	o.record(name, sqlText, render(jr), render(gr))
}
