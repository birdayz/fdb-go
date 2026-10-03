//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A recursive CTE's self-reference reads the seed's column names; its column
// list renames only what the query consuming the CTE sees
// (QueryVisitor.handleRecursiveNamedQuery, SemanticAnalyzer.getRecursiveCteType).
// Both engines must answer alike, Java's exception class aside.
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
		} {
			o.plain(schema, setup, fmt.Sprintf("r%02d", i), q)
		}
		// Declared divergences: Java cannot join (r10, an internal error) or
		// group (r11) a recursive CTE's result; Go answers both. Java types the
		// fixed point by the seed's row (r00, r02, r06, r14, r15); Go widens it
		// to the common row (TODO.md, "Recursive CTE row type").
		divergent := map[string][2]string{
			"r00": {`OK [BIGINT BIGINT INTEGER] [NULL NULL NOT NULL] [[5 2 0] [2 1 1] [1 -1 2]]`, `OK [BIGINT BIGINT INTEGER] [NULL NULL NULL] [[5 2 0] [2 1 1] [1 -1 2]]`},
			"r02": {`OK [BIGINT BIGINT INTEGER] [NULL NULL NOT NULL] [[5 2 0] [2 1 1] [1 -1 2]]`, `OK [BIGINT BIGINT INTEGER] [NULL NULL NULL] [[5 2 0] [2 1 1] [1 -1 2]]`},
			"r06": {`OK [BIGINT BIGINT INTEGER] [NULL NULL NOT NULL] [[5 2 0] [2 1 1] [1 -1 2]]`, `OK [BIGINT BIGINT INTEGER] [NULL NULL NULL] [[5 2 0] [2 1 1] [1 -1 2]]`},
			"r14": {`ERROR XXXXX IllegalArgumentException "Wrong object type used with protocol message reflection.\nField number: 1, field java type: INT, value type: java.lang.Long\n"`, `OK [BIGINT] [NULL] [[0] [1] [2]]`},
			"r15": {`ERROR XXXXX VerifyException "Cannot set a non-nullable field to the NULL value"`, `OK [INTEGER] [NULL] [[0] [NULL]]`},
			"r10": {`ERROR XXXXX IllegalArgumentException "Node Reference@N(isExplored=true) is not an element of this graph."`, `OK [BIGINT BIGINT] [NULL NULL] [[2 -1] [5 1]]`},
			"r11": {`ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`, `OK [INTEGER BIGINT] [NULL NULL] [[0 1] [1 1] [2 1]]`},
		}
		javaClass := regexp.MustCompile(`^ERROR (\S+) \S+ "`)
		// The engines word an unknown column differently; the state is shared.
		undefinedColumn := regexp.MustCompile(`^ERROR 42703 .*$`)
		identityHash := regexp.MustCompile(`@\d+\(`)
		var failures []string
		for _, name := range sortedStringKeys(o.got) {
			javaLine, goLine := identityHash.ReplaceAllString(o.got[name], `@N(`), o.goGot[name]
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
		Expect(len(o.got)).To(Equal(17))
		Expect(failures).To(BeEmpty())
	})
})
