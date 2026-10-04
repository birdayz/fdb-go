//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A window's OPTIONS reach the function as call-site options
// (SemanticAnalyzer.toCallSiteOptions, CallSiteArguments.Options), each value
// parsed by ParseHelpers.parseDecimal, which also parses every decimal constant
// of a statement (AstNormalizer). Both engines must answer alike, Java's
// exception class aside.
var _ = Describe("WindowOptionsConformance", func() {
	It("parses, checks and applies EF_SEARCH and decimal literals as the target does", func() {
		o, done := newWSEOracle("winopts_", "WINDOW-OPTIONS")
		defer done()
		const schema = `CREATE TABLE v (id BIGINT, emb VECTOR(3, FLOAT), PRIMARY KEY (id)) CREATE VECTOR INDEX vi USING HNSW ON v (emb)`
		// Java's INSERT cannot take a vector literal (its array constructor
		// casts the column's VECTOR type to an Array), so the rows are bound.
		const insert = `INSERT INTO v VALUES (?, ?), (?, ?), (?, ?), (?, ?)`
		rows := []wseParam{
			wseLong(1),
			{kind: "floatVector", value: []float64{1, 0, 0}},
			wseLong(2),
			{kind: "floatVector", value: []float64{0, 1, 0}},
			wseLong(3),
			{kind: "floatVector", value: []float64{0, 0, 1}},
			wseLong(4),
			{kind: "floatVector", value: []float64{0.8, 0.2, 0}},
		}
		for i, options := range []string{
			``,
			` OPTIONS EF_SEARCH = 100`,
			` OPTIONS EF_SEARCH = 10, EF_SEARCH = 10`,
			` OPTIONS EF_SEARCH = 10, EF_SEARCH = 20`,
			` OPTIONS EF_SEARCH = 1`,
			` OPTIONS EF_SEARCH = 0`,
			` OPTIONS EF_SEARCH = 2`,
			` OPTIONS EF_SEARCH = 10L`,
			` OPTIONS EF_SEARCH = 10I`,
			` OPTIONS EF_SEARCH = 2147483646`,
			` OPTIONS EF_SEARCH = 2147483647`,
			` OPTIONS EF_SEARCH = 2147483648`,
			` OPTIONS EF_SEARCH = 2147483648L`,
			` OPTIONS EF_SEARCH = 3000000000I`,
			` OPTIONS EF_SEARCH = 99999999999999999999`,
			` OPTIONS EF_SEARCH = 3000000000, EF_SEARCH = 3000000000`,
			` OPTIONS EF_SEARCH = 3000000000I, EF_SEARCH = 1, EF_SEARCH = 1`,
		} {
			query := `SELECT id FROM v QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(emb, CAST([0.9, 0.1, 0.0] AS VECTOR(3, FLOAT)))` + options + `) <= 3`
			o.prepared(schema, nil, wseDML(fmt.Sprintf("w%02d", i), insert, query, rows...))
		}
		o.plain(schema, nil, "desc_and_duplicate", `SELECT id FROM v QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(emb, CAST([0.9, 0.1, 0.0] AS VECTOR(3, FLOAT))) DESC OPTIONS EF_SEARCH = 1, EF_SEARCH = 1) <= 3`)
		o.plain(schema, nil, "desc_and_unparsable", `SELECT id FROM v QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(emb, CAST([0.9, 0.1, 0.0] AS VECTOR(3, FLOAT))) DESC OPTIONS EF_SEARCH = 3000000000I) <= 3`)
		for i, q := range []string{
			`SELECT 3000000000I FROM v`,
			`SELECT 99999999999999999999 FROM v`,
			`SELECT id FROM v WHERE id < 99999999999999999999`,
			`SELECT id FROM v WHERE id < 3000000000I`,
			`SELECT 1e309 FROM v`,
			`SELECT 9223372036854775808L FROM v`,
			`SELECT -2147483648I FROM v`,
			`SELECT -9223372036854775808 FROM v`,
			`SELECT 1e5 FROM v`,
			`SELECT 1.5e3 FROM v`,
			`SELECT 1.0e400 FROM v`,
			`SELECT -1.0e400 FROM v`,
			`SELECT 1.5f FROM v`,
			`SELECT .5 FROM v`,
			`SELECT 1.0e40f FROM v`,
			`SELECT -9223372036854775809 FROM v`,
			`SELECT -2147483649I FROM v`,
			`SELECT 2147483648 FROM v`,
			`SELECT 007 FROM v`,
			`SELECT id FROM v WHERE id = 1e5`,
			`SELECT -1e5 FROM v`,
			`SELECT 1e5f FROM v`,
			`SELECT id FROM v WHERE id IN (1, 1e5)`,
			`EXPLAIN SELECT 1e5 FROM v`,
		} {
			o.plain(schema, nil, fmt.Sprintf("lit%02d", i), q)
		}
		// Java cannot allocate the beam it sizes efSearch + 1 up front; Go
		// searches the whole layer (DIVERGENCES.md, "HNSW efSearch beyond memory").
		divergent := map[string][2]string{
			"w09": {`ERROR XXXXX OutOfMemoryError "Requested array size exceeds VM limit"`, `OK [BIGINT] [NULL] [[4] [1] [2]] COUNT 4`},
			"w10": {`ERROR XXXXX IllegalArgumentException "java.lang.IllegalArgumentException"`, `OK [BIGINT] [NULL] [[4] [1] [2]] COUNT 4`},
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
		Expect(len(o.got)).To(Equal(43))
		Expect(failures).To(BeEmpty())
	})
})
