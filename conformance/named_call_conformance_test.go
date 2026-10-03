//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A user-defined function call binds named or positional arguments
// (ExpressionVisitor.visitNamedOrUnnamedFunctionArgs,
// UserDefinedFunctionCatalog.lookup, CatalogedFunction.validateCall and
// resolveParameterValuesFromArguments). Both engines must answer alike, Java's
// exception class aside.
var _ = Describe("NamedCallConformance", func() {
	It("binds and refuses named and positional calls as the target does", func() {
		o, done := newWSEOracle("namedcall_", "NAMED-CALL")
		defer done()
		const schema = `CREATE TYPE AS STRUCT st1(y BIGINT, z BIGINT) ` +
			`CREATE TABLE t (id BIGINT, a BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) ` +
			`CREATE FUNCTION st1_d(IN y BIGINT, IN z BIGINT DEFAULT 2L) RETURNS st1 RETURN (y, z) ` +
			`CREATE FUNCTION add2(IN a BIGINT, IN b BIGINT) RETURNS BIGINT RETURN a + b ` +
			`CREATE FUNCTION zero() RETURNS BIGINT RETURN 0L ` +
			`CREATE FUNCTION st1_z(IN s TYPE st1) RETURNS BIGINT RETURN s.z ` +
			`CREATE FUNCTION tf(IN lo BIGINT, IN hi BIGINT DEFAULT 10) AS SELECT id FROM t WHERE id BETWEEN lo AND hi ` +
			`CREATE FUNCTION many(IN p1 BIGINT DEFAULT 0, IN p2 BIGINT DEFAULT 0, IN p3 BIGINT DEFAULT 0, IN p4 BIGINT DEFAULT 0, IN p5 BIGINT DEFAULT 0, IN p6 BIGINT DEFAULT 0, IN p7 BIGINT DEFAULT 0, IN p8 BIGINT DEFAULT 0, IN p9 BIGINT DEFAULT 0, IN p10 BIGINT DEFAULT 0, IN p11 BIGINT DEFAULT 0, IN p12 BIGINT DEFAULT 0, IN p13 BIGINT DEFAULT 0, IN p14 BIGINT DEFAULT 0) RETURNS BIGINT RETURN p1`
		setup := []string{`INSERT INTO t VALUES (1, 10, [1, 2]), (2, 20, [3])`}
		for i, q := range []string{
			`SELECT st1_d(y => 4) FROM t WHERE id = 1`,
			`SELECT st1_d(z => 5, y => 4) FROM t WHERE id = 1`,
			`SELECT st1_d(4) FROM t WHERE id = 1`,
			`SELECT st1_d(4, 5) FROM t WHERE id = 1`,
			`SELECT st1_d(z => 5) FROM t WHERE id = 1`,
			`SELECT st1_d() FROM t WHERE id = 1`,
			`SELECT st1_d(4, 5, 6) FROM t WHERE id = 1`,
			`SELECT st1_d(y => 4, w => 1) FROM t WHERE id = 1`,
			`SELECT st1_d(y => 4, y => 5) FROM t WHERE id = 1`,
			`SELECT st1_d(y => 4, z => 5, y => 6, z => 7) FROM t WHERE id = 1`,
			`SELECT st1_d(4, z => 5) FROM t WHERE id = 1`,
			`SELECT st1_d(y => 4, 5) FROM t WHERE id = 1`,
			`SELECT st1_d("Y" => 4) FROM t WHERE id = 1`,
			`SELECT st1_d("y" => 4) FROM t WHERE id = 1`,
			`SELECT st1_d(y => 'x') FROM t WHERE id = 1`,
			`SELECT st1_d(y => a) FROM t WHERE id = 1`,
			`SELECT add2(b => 1, a => a) FROM t WHERE id = 2`,
			`SELECT zero(x => 1) FROM t WHERE id = 1`,
			`SELECT zero() FROM t WHERE id = 1`,
			`SELECT cardinality(x => arr) FROM t WHERE id = 1`,
			`SELECT nope(x => 1) FROM t WHERE id = 1`,
			`SELECT * FROM tf(lo => 1)`,
			`SELECT * FROM tf(hi => 1, lo => 1)`,
			`SELECT * FROM tf(hi => 1)`,
			`SELECT * FROM tf(lo => 1, lo => 2)`,
			`SELECT * FROM tf(lo => 1, x => 2)`,
			`SELECT * FROM tf(1, 2, 3)`,
			`SELECT * FROM tf()`,
			// Fourteen repeated names resize the HashMap the repeats are listed from.
			`SELECT st1_z(a) FROM t WHERE id = 1`,
			`SELECT add2(a => (1, 2), b => 1) FROM t WHERE id = 1`,
			`SELECT many(p1 => 1, p2 => 2, p3 => 3, p4 => 4, p5 => 5, p6 => 6, p7 => 7, p8 => 8, p9 => 9, p10 => 10, p11 => 11, p12 => 12, p13 => 13, p14 => 14, p1 => 1, p2 => 2, p3 => 3, p4 => 4, p5 => 5, p6 => 6, p7 => 7, p8 => 8, p9 => 9, p10 => 10, p11 => 11, p12 => 12, p13 => 13, p14 => 14) FROM t WHERE id = 1`,
		} {
			o.plain(schema, setup, fmt.Sprintf("n%02d", i), q)
		}
		// Declared divergence (DIVERGENCES.md, "Named macro arguments"): Java
		// binds a named call's values in call order.
		divergent := map[string][2]string{
			"n01": {`OK [STRUCT] [NOT NULL] [[map[Y:5 Z:4]]]`, `OK [STRUCT] [NOT NULL] [[map[Y:4 Z:5]]]`},
		}
		javaClass := regexp.MustCompile(`^ERROR (\S+) \S+ "`)
		var failures []string
		for _, name := range sortedStringKeys(o.got) {
			javaLine, goLine := o.got[name], o.goGot[name]
			if want, ok := divergent[name]; ok {
				if javaLine != want[0] || goLine != want[1] {
					failures = append(failures, fmt.Sprintf("%s: java %s, go %s; want %s and %s", name, javaLine, goLine, want[0], want[1]))
				}
				continue
			}
			if javaClass.ReplaceAllString(javaLine, `ERROR $1 "`) != goLine {
				failures = append(failures, fmt.Sprintf("%s: java %s, go %s", name, javaLine, goLine))
			}
		}
		Expect(len(o.got)).To(Equal(31))
		Expect(failures).To(BeEmpty())
	})
})
