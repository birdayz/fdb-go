//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// UPDATE and DELETE ... RETURNING answer a select over the modification's
// rows: an UPDATE's row is the pair "old" and "new", a DELETE's the deleted
// record (QueryVisitor.visitUpdateStatement / visitDeleteStatement). Both
// engines must answer alike, Java's exception class aside.
var _ = Describe("ReturningConformance", func() {
	It("returns the modified rows as the target does", func() {
		o, done := newWSEOracle("returning_", "RETURNING")
		defer done()
		const schema = `CREATE TYPE AS STRUCT s(x BIGINT, y STRING) ` +
			`CREATE TABLE a (a1 BIGINT, a2 BIGINT, a3 BIGINT, PRIMARY KEY (a1)) ` +
			`CREATE TABLE c (id BIGINT, st s, PRIMARY KEY (id))`
		setup := []string{`INSERT INTO a VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300)`, `INSERT INTO c VALUES (1, (5, 'e'))`}
		for i, q := range []string{
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new".a3`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new".a2, "old".a2`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new".*`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING ("new".*)`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING *`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new".a3 + "new".a3`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING new.a3`,
			`UPDATE a SET a2 = 42 WHERE a1 > 100 RETURNING "new".a3`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new".a3 OPTIONS(DRY RUN)`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new"`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "new".a3 AS z`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING a3`,
			`DELETE FROM a WHERE a1 <= 1 RETURNING a2, a3`,
			`DELETE FROM a WHERE a1 = 3 RETURNING a1 + a2 + a3`,
			`DELETE FROM a WHERE a1 <= 2 RETURNING *`,
			`DELETE FROM a WHERE a1 > 200 RETURNING a1`,
			`DELETE FROM a WHERE a1 = 1 RETURNING a.a1`,
			`UPDATE c SET st = (6, 'f') WHERE id = 1 RETURNING "new".st`,
			`UPDATE c SET st = (6, 'f') WHERE id = 1 RETURNING "new".st.x, "old".st.y`,
			`UPDATE a AS t SET a2 = 1 WHERE t.a1 = 1 RETURNING "new".a2`,
			`DELETE FROM a WHERE a1 = 1 RETURNING "old".a1`,
			`UPDATE a SET a2 = a2 + 1 RETURNING COUNT(*)`,
			`DELETE FROM a WHERE a1 = 1 RETURNING a1 OPTIONS(DRY RUN)`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "old".a2 AS a2, "new".a2 AS a2`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING "old"`,
			`UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING ("old".*)`,
			`UPDATE c SET st = (6, 'f') WHERE id = 1 RETURNING ("new".st.*)`,
			`UPDATE c SET st = (6, 'f') WHERE id = 1 RETURNING "new".id.*`,
			`UPDATE a SET a2 = ? WHERE a1 <= 2 RETURNING "new".a2`,
			`DELETE FROM a WHERE a1 <= 2 RETURNING a1 * 2 AS d, a3`,
		} {
			o.named(schema, setup, fmt.Sprintf("d%02d", i), q)
		}
		// Declared divergences: a lone `*` keeps the statement an update, which
		// the Java runner's executeQuery refuses after running it and the Go
		// runner sends through Exec (d04, d14; DIVERGENCES.md, "DML
		// statement-layer routing"); Java cannot evaluate an aggregate over the
		// returned rows, which Go answers (d21).
		divergent := map[string][2]string{
			"d04": {`ERROR 02F01 SQLException "query 'UPDATE a SET a2 = 42 WHERE a1 <= 2 RETURNING *' does not return result set, use JDBC executeUpdate method instead"`, `["ROWS_AFFECTED"] OK [BIGINT] [] [[2]]`},
			"d14": {`ERROR 02F01 SQLException "query 'DELETE FROM a WHERE a1 <= 2 RETURNING *' does not return result set, use JDBC executeUpdate method instead"`, `["ROWS_AFFECTED"] OK [BIGINT] [] [[2]]`},
			"d21": {`ERROR XXXXX IllegalStateException "unable to eval an aggregation function with eval()"`, `["_0"] OK [BIGINT] [NOT NULL] [[3]]`},
		}
		javaClass := regexp.MustCompile(`^ERROR (\S+) \S+ "`)
		undefinedColumn := regexp.MustCompile(`^ERROR 42703 .*$`)
		var failures []string
		for _, name := range sortedStringKeys(o.got) {
			javaLine, goLine := o.got[name], o.goGot[name]
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
		Expect(len(o.got)).To(Equal(30))
		Expect(failures).To(BeEmpty())
	})
})
