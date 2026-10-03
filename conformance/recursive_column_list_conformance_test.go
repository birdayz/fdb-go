//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A recursive CTE's self-reference reads the seed's column names; its column
// list renames only what the query consuming the CTE sees, and every iteration
// keeps the seed's row type (QueryVisitor.handleRecursiveNamedQuery,
// SemanticAnalyzer.getRecursiveCteType). Both engines must answer alike, Java's
// exception class aside.
var _ = Describe("RecursiveColumnListConformance", func() {
	It("scopes a recursive CTE's column list to its consumers", func() {
		o, done := newWSEOracle("reccols_", "REC-COLS")
		defer done()
		const schema = `CREATE TABLE adj(me BIGINT, par BIGINT, PRIMARY KEY(me))`
		setup := []string{`INSERT INTO adj VALUES (1, -1), (2, 1), (5, 2)`}
		for i, q := range []string{
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, d.y AS lvl FROM adj, (SELECT me, par, lvl + 1 AS y FROM r) AS d WHERE d.par = adj.me) SELECT a, b, c FROM r`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, d.y AS lvl FROM adj, (SELECT a, b, c + 1 AS y FROM r) AS d WHERE d.b = adj.me) SELECT a, b, c FROM r`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT a, b, c FROM r`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.c + 1 FROM adj, r WHERE r.b = adj.me) SELECT a, b, c FROM r`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT me FROM r`,
			`WITH r (a, b) AS (SELECT me, par FROM adj) SELECT me FROM r`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT * FROM r`,
			`WITH RECURSIVE c (v) AS (SELECT me AS x FROM adj WHERE me = 1 UNION ALL SELECT x + 1 FROM c WHERE x < 4) SELECT v FROM c`,
			`WITH RECURSIVE c (v) AS (SELECT me AS x FROM adj WHERE me = 1 UNION ALL SELECT v + 1 FROM c WHERE v < 4) SELECT v FROM c`,
			`WITH RECURSIVE c (me) AS (SELECT me FROM adj WHERE me = 1 UNION ALL SELECT me + 1 FROM c WHERE me < 4) SELECT me FROM c`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT r.a, adj.par FROM r, adj WHERE r.b = adj.me`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT c, COUNT(*) FROM r GROUP BY c`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT a FROM r WHERE c > 0`,
			`WITH RECURSIVE r (a, b) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) SELECT a FROM r`,
			`WITH RECURSIVE r AS (SELECT 0 AS x FROM adj WHERE me = 1 UNION ALL SELECT CAST(x + 1 AS BIGINT) AS x FROM r WHERE x < 2) SELECT x FROM r`,
			`WITH RECURSIVE r AS (SELECT 0 AS x FROM adj WHERE me = 1 UNION ALL SELECT CAST(NULL AS INTEGER) AS x FROM r WHERE x = 0) SELECT x FROM r`,
			`WITH RECURSIVE r AS (SELECT me, par FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par FROM adj, r WHERE r.par = adj.me) SELECT me FROM r`,
			`WITH RECURSIVE r AS (SELECT me AS x FROM adj WHERE me = 1 UNION ALL SELECT 2 AS x FROM r WHERE x = 1) SELECT x FROM r`,
			`WITH RECURSIVE r AS (SELECT 0 AS x FROM adj WHERE me = 1 UNION ALL SELECT CAST(x + 1 AS BIGINT) AS x FROM r WHERE x = 5) SELECT x FROM r`,
			`WITH RECURSIVE r AS (SELECT 0 AS x FROM adj WHERE me = 1 UNION ALL SELECT CAST(NULL AS INTEGER) AS x FROM r WHERE x = 5) SELECT x FROM r`,
			`WITH RECURSIVE r AS (SELECT 0 AS x FROM adj WHERE me = 1 UNION ALL SELECT CAST(NULL AS INTEGER) AS x FROM r WHERE x = 0) SELECT COUNT(*) FROM r`,
			`WITH RECURSIVE r AS (SELECT par AS x, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.par, r.lvl + 1 FROM adj, r WHERE adj.me = r.x) SELECT x, lvl FROM r`,
			`WITH RECURSIVE r AS (SELECT 1.5 AS x FROM adj WHERE me = 1 UNION ALL SELECT 3 FROM r WHERE x < 2) SELECT x FROM r`,
			`WITH RECURSIVE r (a, b, c) AS (SELECT me, par, 0 AS lvl FROM adj WHERE me = 5 UNION ALL SELECT adj.me, adj.par, r.lvl + 1 FROM adj, r WHERE r.par = adj.me) TRAVERSAL ORDER pre_order SELECT a, b, c FROM r`,
			`WITH RECURSIVE r AS (SELECT 0 AS x FROM adj WHERE me = 1 UNION ALL SELECT CAST(NULL AS INTEGER) AS x FROM r WHERE x = 0) SELECT x FROM r WHERE x IS NULL`,
		} {
			o.plain(schema, setup, fmt.Sprintf("r%02d", i), q)
		}
		// Declared divergences: Java cannot join (r10, an internal error) or
		// group (r11) a recursive CTE's result; Go answers both. Java writes
		// later iterations into the seed-typed temporary table unconverted:
		// it fails on reading an INT into a BIGINT or DOUBLE column, which Go
		// promotes (r17, r22), and it keeps a NULL under a NOT NULL type until
		// something reads it (r20, r24), which Go refuses when written. A
		// BIGINT into an INT column fails in both, worded apart (r14).
		// DIVERGENCES.md, "Recursive CTE rows that do not fit the seed".
		// Java's r24 depends on its plan: folding `x IS NULL` on the NOT NULL
		// column answers no row (measured alone), while reading x first fails
		// as Go does (measured within the whole suite).
		divergent := map[string][2]string{
			"r10": {`ERROR XXXXX IllegalArgumentException "Node Reference@N(isExplored=true) is not an element of this graph."`, `OK [BIGINT BIGINT] [NULL NULL] [[2 -1] [5 1]]`},
			"r11": {`ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`, `OK [INTEGER BIGINT] [NOT NULL NULL] [[0 1] [1 1] [2 1]]`},
			"r14": {`ERROR XXXXX IllegalArgumentException "Wrong object type used with protocol message reflection.\nField number: 1, field java type: INT, value type: java.lang.Long\n"`, `ERROR XXXXX "BIGINT value cannot be stored in a column of type INT"`},
			"r17": {`ERROR XXXXX IllegalArgumentException "Wrong object type used with protocol message reflection.\nField number: 1, field java type: LONG, value type: java.lang.Integer\n"`, `OK [BIGINT] [NULL] [[1] [2]]`},
			"r20": {`OK [BIGINT] [NOT NULL] [[2]]`, `ERROR XXXXX "Cannot set a non-nullable field to the NULL value"`},
			"r22": {`ERROR XXXXX IllegalArgumentException "Wrong object type used with protocol message reflection.\nField number: 1, field java type: DOUBLE, value type: java.lang.Integer\n"`, `OK [DOUBLE] [NOT NULL] [[1.5] [3]]`},
			"r24": {`OK [INTEGER] [NOT NULL] []`, `ERROR XXXXX "Cannot set a non-nullable field to the NULL value"`},
		}
		javaClass := regexp.MustCompile(`^ERROR (\S+) \S+ "`)
		// The engines word an unknown column differently; the state is shared.
		undefinedColumn := regexp.MustCompile(`^ERROR 42703 .*$`)
		identityHash := regexp.MustCompile(`@\d+\(`)
		var failures []string
		for _, name := range sortedStringKeys(o.got) {
			javaLine, goLine := identityHash.ReplaceAllString(o.got[name], `@N(`), o.goGot[name]
			if name == "r24" && javaLine == `ERROR XXXXX VerifyException "Cannot set a non-nullable field to the NULL value"` {
				javaLine = divergent[name][0]
			}
			if want, ok := divergent[name]; ok {
				if javaLine != want[0] || goLine != want[1] {
					failures = append(failures, fmt.Sprintf("%s: java %s, go %s; want %s and %s", name, javaLine, goLine, want[0], want[1]))
				}
				continue
			}
			javaLine = javaClass.ReplaceAllString(javaLine, `ERROR $1 "`)
			if undefinedColumn.ReplaceAllString(javaLine, `ERROR 42703`) != undefinedColumn.ReplaceAllString(goLine, `ERROR 42703`) {
				failures = append(failures, fmt.Sprintf("%s: java %s, go %s", name, javaLine, goLine))
			}
		}
		Expect(len(o.got)).To(Equal(25))
		Expect(failures).To(BeEmpty())
	})
})
