//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A record constructor's fields take the target type's names by position
// where one is in scope, else each element's own name
// (ExpressionVisitor.parseRecordFieldsUnderReorderings,
// Expressions.underlyingAsColumns).
var _ = Describe("RecordNamesConformance", func() {
	It("names record constructor fields as the target does", func() {
		o, done := newWSEOracle("recnames_", "RECORD-NAMES")
		defer done()
		const schema = `CREATE TYPE AS STRUCT S(a BIGINT, b BIGINT) ` +
			`CREATE TABLE t (id BIGINT, x BIGINT, y BIGINT, s S, sa S ARRAY, PRIMARY KEY (id)) ` +
			`CREATE TABLE u (id BIGINT, s S, PRIMARY KEY (id)) ` +
			`CREATE TABLE w (id BIGINT, sa S ARRAY, PRIMARY KEY (id))`
		setup := []string{`INSERT INTO t VALUES (1, 10, 20, (1, 2), NULL), (2, 30, 40, NULL, NULL)`}
		for i, q := range []string{
			`SELECT (x) FROM t WHERE id = 1`,
			`SELECT (x, y) FROM t WHERE id = 1`,
			`SELECT (x + 1, y) FROM t WHERE id = 1`,
			`SELECT (x AS p, y) FROM t WHERE id = 1`,
			`SELECT (t.x) FROM t WHERE id = 1`,
			`SELECT (s.a, s.b) FROM t WHERE id = 1`,
			`SELECT ((x, y)) FROM t WHERE id = 1`,
			`SELECT (s) FROM t WHERE id = 1`,
			`SELECT (x, 5) FROM t WHERE id = 1`,
			`SELECT (x, x) FROM t WHERE id = 1`,
			`SELECT r FROM (SELECT (x, y) AS r FROM t WHERE id = 1) d`,
			`SELECT d.r.x FROM (SELECT (x, y) AS r FROM t WHERE id = 1) d`,
			`SELECT coalesce(s, (x, y)) FROM t ORDER BY id`,
			`SELECT coalesce((x, y), s) FROM t ORDER BY id`,
			`SELECT [(x, y)] FROM t WHERE id = 1`,
		} {
			o.plain(schema, setup, fmt.Sprintf("q%02d", i), q)
		}
		for i, c := range []struct{ dml, follow string }{
			{`INSERT INTO u VALUES (1, (5, 6))`, `SELECT s FROM u`},
			{`INSERT INTO u SELECT id, (x, y) FROM t WHERE id = 1`, `SELECT s FROM u`},
			{`INSERT INTO u SELECT id, (y AS b, x AS a) FROM t WHERE id = 1`, `SELECT s FROM u`},
			{`UPDATE t SET s = (x, y) WHERE id = 2`, `SELECT s FROM t ORDER BY id`},
			{`UPDATE t SET s = coalesce(s, (x, y))`, `SELECT s FROM t ORDER BY id`},
			{`UPDATE t SET s = (y AS b, x AS a) WHERE id = 2`, `SELECT s FROM t ORDER BY id`},
			{`UPDATE t SET s = (x AS q, y) WHERE id = 2`, `SELECT s FROM t ORDER BY id`},
			{`INSERT INTO u VALUES (1, (6 AS b, 5 AS a))`, `SELECT s FROM u`},
			{`INSERT INTO u VALUES (1, (5 AS a, 6 AS b))`, `SELECT s FROM u`},
			{`INSERT INTO u VALUES (1, (5 AS q, 6))`, `SELECT s FROM u`},
			{`UPDATE t SET sa = [(x, y)] WHERE id = 1`, `SELECT sa FROM t ORDER BY id`},
			{`UPDATE t SET sa = [(y AS b, x AS a)] WHERE id = 1`, `SELECT sa FROM t ORDER BY id`},
			{`INSERT INTO w SELECT id, [(y AS b, x AS a)] FROM t WHERE id = 1`, `SELECT sa FROM w`},
			{`INSERT INTO w SELECT id, [(x, y), (y, x)] FROM t WHERE id = 1`, `SELECT sa FROM w`},
		} {
			o.prepared(schema, setup, wseDML(fmt.Sprintf("d%02d", i), c.dml, c.follow))
		}
		// CQ-74 (TODO.md): Go names an array column's type by its element.
		divergent := map[string][2]string{
			"q14": {`OK [ARRAY] [NOT NULL] [[[map[X:10 Y:20]]]]`, `OK [STRUCT] [NOT NULL] [[[map[X:10 Y:20]]]]`},
			"d10": {`OK [ARRAY] [NULL] [[[map[A:10 B:20]]] [NULL]] COUNT 1`, `OK [STRUCT] [NULL] [[[map[A:10 B:20]]] [NULL]] COUNT 1`},
			"d11": {`OK [ARRAY] [NULL] [[[map[A:20 B:10]]] [NULL]] COUNT 1`, `OK [STRUCT] [NULL] [[[map[A:20 B:10]]] [NULL]] COUNT 1`},
			"d12": {`OK [ARRAY] [NULL] [[[map[A:20 B:10]]]] COUNT 1`, `OK [STRUCT] [NULL] [[[map[A:20 B:10]]]] COUNT 1`},
			"d13": {`OK [ARRAY] [NULL] [[[map[A:10 B:20] map[A:20 B:10]]]] COUNT 1`, `OK [STRUCT] [NULL] [[[map[A:10 B:20] map[A:20 B:10]]]] COUNT 1`},
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
		Expect(len(o.got)).To(Equal(29))
		Expect(failures).To(BeEmpty())
	})
})
