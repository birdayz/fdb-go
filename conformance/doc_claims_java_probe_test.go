//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/conformance/plandiff/seedcorpus"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// docClaimSchemas are the schemas the DocClaimsJavaProbe cases run under, with
// their setup; each DDL claim has a schema of its own.
var docClaimSchemas = map[string]struct {
	ddl   string
	setup []string
}{
	"main": {
		ddl: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, d DOUBLE, b BOOLEAN, i INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE u (id BIGINT, t_id BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE arr (id BIGINT, xs BIGINT ARRAY, PRIMARY KEY (id)) " +
			"CREATE TABLE bt (id BIGINT, bs BYTES, PRIMARY KEY (id)) " +
			"CREATE TABLE g (id BIGINT, k BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX t_v AS SELECT v FROM t ORDER BY v " +
			"CREATE UNIQUE INDEX u_w AS SELECT w FROM u ORDER BY w",
		setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a', 1.5, true, 1), (2, 20, 'b', -2.5, false, 2), (3, 10, 'c', 0.0, NULL, 3)",
			"INSERT INTO u VALUES (1, 1, 5), (2, 3, 6), (3, 9, 7)",
			"INSERT INTO arr VALUES (1, [1, 2, 3]), (2, [])",
			"INSERT INTO bt VALUES (1, x'cafe'), (2, x'00')",
			"INSERT INTO g VALUES (1, 5), (2, 5), (3, 6)",
		},
	},
	"tree": {
		ddl: "CREATE TABLE t1 (id BIGINT, parent BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX parent_idx AS SELECT parent, id FROM t1 ORDER BY parent, id " +
			"CREATE INDEX child_idx AS SELECT id, parent FROM t1 ORDER BY id, parent",
		setup: []string{"INSERT INTO t1 VALUES (1, -1), (10, 1), (20, 1), (40, 10), (50, 10)"},
	},
	"diamond": {
		ddl: "CREATE TABLE e (id BIGINT, parent BIGINT, PRIMARY KEY (id, parent)) " +
			"CREATE INDEX parent_idx AS SELECT parent FROM e ORDER BY parent",
		setup: []string{"INSERT INTO e VALUES (1, -1), (2, 1), (3, 1), (4, 2), (4, 3)"},
	},
	"date_column":      {ddl: "CREATE TABLE dt (id BIGINT, d DATE, PRIMARY KEY (id))"},
	"timestamp_column": {ddl: "CREATE TABLE ts (id BIGINT, d TIMESTAMP, PRIMARY KEY (id))"},
	"scalar_not_null":  {ddl: "CREATE TABLE nn (id BIGINT, v BIGINT NOT NULL, PRIMARY KEY (id))"},
	"array_not_null":   {ddl: "CREATE TABLE na (id BIGINT, xs BIGINT ARRAY NOT NULL, PRIMARY KEY (id))"},
}

// docClaimCase is one claim about Java in DIVERGENCES.md ("Go-Only Extensions")
// or SQL_CONFORMANCE.md, named by its row. setup runs after the schema's own.
type docClaimCase struct {
	name, schema string
	setup        []string
	sql          string
}

// nullV gives t a NULL v, for the cases that measure where NULLs sort.
const nullV = "INSERT INTO t VALUES (4, NULL, 'd', 0.5, true, 4)"

var docClaimCases = []docClaimCase{
	// DML.
	{name: "select", schema: "main", sql: "SELECT id, v FROM t WHERE id = 1"},
	{name: "insert_select", schema: "main", setup: []string{"INSERT INTO u SELECT id + 10, id, v FROM t WHERE id = 2"}, sql: "SELECT id, t_id, w FROM u WHERE id = 12"},
	{name: "insert_select_column_list", schema: "main", setup: []string{"INSERT INTO u (id, t_id, w) SELECT id + 10, id, v FROM t WHERE id = 2"}, sql: "SELECT id FROM u WHERE id = 12"},
	{name: "update", schema: "main", setup: []string{"UPDATE t SET v = v + 1 WHERE id = 1"}, sql: "SELECT v FROM t WHERE id = 1"},
	{name: "delete", schema: "main", setup: []string{"DELETE FROM t WHERE id = 1"}, sql: "SELECT id FROM t"},
	// Expressions.
	{name: "arithmetic", schema: "main", sql: "SELECT v + 1 AS a, v - 1 AS b, v * 2 AS c, v / 3 AS e, v % 3 AS f FROM t WHERE id = 2"},
	{name: "searched_case", schema: "main", sql: "SELECT CASE WHEN v > 15 THEN 'hi' ELSE 'lo' END AS c FROM t WHERE id = 2"},
	{name: "simple_case", schema: "main", sql: "SELECT CASE v WHEN 10 THEN 'ten' ELSE 'other' END AS c FROM t WHERE id = 1"},
	{name: "simple_case_no_else", schema: "main", sql: "SELECT CASE v WHEN 10 THEN 'ten' WHEN 20 THEN 'twenty' END AS c FROM t WHERE id = 1"},
	{name: "cast", schema: "main", sql: "SELECT CAST(v AS STRING) AS c FROM t WHERE id = 1"},
	{name: "cast_overflow", schema: "main", sql: "SELECT CAST(9223372036854775807 AS INTEGER) AS c FROM t WHERE id = 1"},
	{name: "coalesce", schema: "main", sql: "SELECT COALESCE(b, true) AS c FROM t WHERE id = 3"},
	{name: "nullif", schema: "main", sql: "SELECT NULLIF(v, 10) AS c FROM t WHERE id = 1"},
	{name: "greatest_least", schema: "main", sql: "SELECT GREATEST(v, 15) AS g, LEAST(v, 15) AS l FROM t WHERE id = 1"},
	{name: "cardinality", schema: "main", sql: "SELECT CARDINALITY(xs) AS n FROM arr WHERE id = 1"},
	{name: "upper", schema: "main", sql: "SELECT UPPER(s) AS c FROM t WHERE id = 1"},
	{name: "position_in", schema: "main", sql: "SELECT POSITION('zz' IN s) AS p FROM t WHERE id = 1"},
	{name: "abs", schema: "main", sql: "SELECT ABS(d) AS c FROM t WHERE id = 2"},
	{name: "current_timestamp", schema: "main", sql: "SELECT CURRENT_TIMESTAMP IS NOT NULL AS c FROM t WHERE id = 1"},
	{name: "current_date", schema: "main", sql: "SELECT CURRENT_DATE IS NOT NULL AS c FROM t WHERE id = 1"},
	{name: "date_part", schema: "main", sql: "SELECT YEAR(CURRENT_DATE) > 2000 AS c FROM t WHERE id = 1"},
	{name: "date_part_of_integer", schema: "main", sql: "SELECT YEAR(v) AS y FROM t WHERE id = 1"},
	{name: "extract_year", schema: "main", sql: "SELECT EXTRACT(YEAR FROM v) AS y FROM t WHERE id = 1"},
	{name: "cast_date", schema: "main", sql: "SELECT CAST('2024-03-15' AS DATE) AS c FROM t WHERE id = 1"},
	{name: "cast_timestamp", schema: "main", sql: "SELECT CAST('2024-03-15 10:00:00' AS TIMESTAMP) AS c FROM t WHERE id = 1"},
	{name: "symbolic_and", schema: "main", sql: "SELECT id FROM t WHERE v = 10 && id = 1"},
	{name: "symbolic_or", schema: "main", sql: "SELECT id FROM t WHERE v = 20 || id = 1"},
	{name: "xor", schema: "main", sql: "SELECT id FROM t WHERE (v = 10) XOR (id = 1)"},
	// Predicates.
	{name: "and_or_not", schema: "main", sql: "SELECT id FROM t WHERE NOT (v = 10 AND id = 1) OR id = 1"},
	{name: "between", schema: "main", sql: "SELECT id FROM t WHERE v BETWEEN 5 AND 15"},
	{name: "in_list", schema: "main", sql: "SELECT id FROM t WHERE v IN (10, 30)"},
	{name: "in_subquery", schema: "main", sql: "SELECT id FROM t WHERE id IN (SELECT t_id FROM u)"},
	{name: "like", schema: "main", sql: "SELECT id FROM t WHERE s LIKE 'a%'"},
	{name: "is_null", schema: "main", sql: "SELECT id FROM t WHERE b IS NULL"},
	{name: "is_distinct_from", schema: "main", sql: "SELECT id FROM t WHERE b IS DISTINCT FROM true"},
	{name: "byte_literal", schema: "main", sql: "SELECT id FROM bt WHERE bs = x'cafe'"},
	// Aggregation.
	{name: "group_by_ordered", schema: "main", sql: "SELECT v, COUNT(*) AS n FROM t GROUP BY v"},
	{name: "group_by_primary_key", schema: "main", sql: "SELECT id, COUNT(*) AS n FROM t GROUP BY id"},
	{name: "group_by_unordered", schema: "main", sql: "SELECT s, COUNT(*) AS n FROM t GROUP BY s"},
	{name: "group_by_expression", schema: "main", sql: "SELECT v + 1 AS k, COUNT(*) AS n FROM t GROUP BY v + 1"},
	{name: "aggregates", schema: "main", sql: "SELECT COUNT(*) AS c, SUM(v) AS s, AVG(v) AS a, MIN(v) AS mn, MAX(v) AS mx FROM t"},
	{name: "having", schema: "main", sql: "SELECT v, COUNT(*) AS n FROM t GROUP BY v HAVING COUNT(*) > 1"},
	{name: "count_distinct", schema: "main", sql: "SELECT COUNT(DISTINCT v) AS n FROM t"},
	{name: "empty_aggregates", schema: "main", sql: "SELECT COUNT(*) AS c, SUM(v) AS s FROM t WHERE id > 100"},
	// Set operations.
	{name: "union_all", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT id FROM u"},
	{name: "union_distinct", schema: "main", sql: "SELECT id FROM t UNION SELECT id FROM u"},
	{name: "intersect", schema: "main", sql: "SELECT id FROM t INTERSECT SELECT id FROM u"},
	{name: "except", schema: "main", sql: "SELECT id FROM t EXCEPT SELECT id FROM u"},
	{name: "union_order_by_position", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT id FROM u ORDER BY 1"},
	// Joins.
	{name: "inner_join", schema: "main", sql: "SELECT t.id, u.id FROM t JOIN u ON t.id = u.t_id"},
	{name: "comma_join", schema: "main", sql: "SELECT t.id, u.id FROM t, u WHERE t.id = u.t_id"},
	{name: "cross_join", schema: "main", sql: "SELECT t.id, u.id FROM t CROSS JOIN u"},
	{name: "left_join", schema: "main", sql: "SELECT t.id, u.id FROM t LEFT JOIN u ON t.id = u.t_id"},
	{name: "right_join", schema: "main", sql: "SELECT t.id, u.id FROM t RIGHT JOIN u ON t.id = u.t_id"},
	{name: "full_join", schema: "main", sql: "SELECT t.id, u.id FROM t FULL OUTER JOIN u ON t.id = u.t_id"},
	{name: "full_join_without_outer", schema: "main", sql: "SELECT u.id FROM t FULL JOIN u ON u.t_id = 1"},
	{name: "self_join", schema: "main", sql: "SELECT a.id, b.id FROM t a, t b WHERE a.v = b.v AND a.id < b.id"},
	{name: "three_way_join", schema: "main", sql: "SELECT a.id, b.id, c.id FROM t a, u b, t c WHERE a.id = b.t_id AND c.id = a.id"},
	// Subqueries.
	{name: "scalar_subquery", schema: "main", sql: "SELECT id, (SELECT MAX(v) FROM t) AS m FROM t"},
	{name: "correlated_scalar_subquery", schema: "main", sql: "SELECT id, (SELECT MAX(w) FROM u WHERE u.t_id = t.id) AS m FROM t"},
	{name: "exists", schema: "main", sql: "SELECT id FROM t WHERE EXISTS (SELECT id FROM u WHERE u.t_id = t.id)"},
	{name: "not_exists", schema: "main", sql: "SELECT id FROM t WHERE NOT EXISTS (SELECT id FROM u WHERE u.t_id = t.id)"},
	{name: "exists_projection", schema: "main", sql: "SELECT id, EXISTS (SELECT id FROM u WHERE u.t_id = t.id) AS e FROM t"},
	{name: "derived_table", schema: "main", sql: "SELECT x.k FROM (SELECT id AS k FROM t) AS x WHERE x.k > 1"},
	{name: "unnest", schema: "main", sql: "SELECT arr.id, e FROM arr, arr.xs AS e"},
	{name: "unnest_ordinality", schema: "main", sql: "SELECT arr.id, e, at FROM arr, arr.xs AS e AT at"},
	// CTEs.
	{name: "with", schema: "main", sql: "WITH c AS (SELECT id FROM t) SELECT id FROM c"},
	{name: "with_column_rename", schema: "main", sql: "WITH c(k) AS (SELECT id FROM t) SELECT k FROM c"},
	{name: "with_chained", schema: "main", sql: "WITH c1 AS (SELECT id FROM t), c2 AS (SELECT id FROM c1 WHERE id > 1) SELECT id FROM c2"},
	{name: "recursive_level_order", schema: "tree", sql: "WITH RECURSIVE c(id, parent) AS (SELECT id, parent FROM t1 WHERE id = 1 UNION ALL SELECT b.id, b.parent FROM t1 AS b, c WHERE b.parent = c.id) TRAVERSAL ORDER LEVEL_ORDER SELECT id FROM c"},
	{name: "recursive_pre_order", schema: "tree", sql: "WITH RECURSIVE c(id, parent) AS (SELECT id, parent FROM t1 WHERE id = 1 UNION ALL SELECT b.id, b.parent FROM t1 AS b, c WHERE b.parent = c.id) TRAVERSAL ORDER PRE_ORDER SELECT id FROM c"},
	{name: "recursive_union_distinct", schema: "tree", sql: "WITH RECURSIVE c(id, parent) AS (SELECT id, parent FROM t1 WHERE id = 1 UNION SELECT b.id, b.parent FROM t1 AS b, c WHERE b.parent = c.id) SELECT id FROM c"},
	{name: "recursive_union_distinct_diamond", schema: "diamond", sql: "WITH RECURSIVE c AS (SELECT id FROM e WHERE id = 1 UNION SELECT e.id FROM e, c WHERE e.parent = c.id) SELECT id FROM c"},
	// Ordering, limiting, distinct.
	{name: "order_by_index", schema: "main", sql: "SELECT v FROM t ORDER BY v"},
	{name: "order_by_no_index", schema: "main", sql: "SELECT id, s FROM t ORDER BY s"},
	{name: "order_by_aggregate", schema: "main", sql: "SELECT v, COUNT(*) AS n FROM t GROUP BY v ORDER BY COUNT(*)"},
	{name: "order_by_nulls_first", schema: "main", setup: []string{nullV}, sql: "SELECT v FROM t ORDER BY v NULLS FIRST"},
	{name: "order_by_nulls_last", schema: "main", setup: []string{nullV}, sql: "SELECT v FROM t ORDER BY v NULLS LAST"},
	{name: "limit", schema: "main", sql: "SELECT id FROM t ORDER BY id LIMIT 2"},
	{name: "limit_offset", schema: "main", sql: "SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 1"},
	{name: "limit_in_subquery", schema: "main", sql: "SELECT x.k FROM (SELECT id AS k FROM t LIMIT 1) AS x"},
	{name: "distinct_ordered", schema: "main", sql: "SELECT DISTINCT v FROM t"},
	{name: "distinct_unordered", schema: "main", sql: "SELECT DISTINCT k FROM g"},
	{name: "distinct_order_by", schema: "main", sql: "SELECT DISTINCT v FROM t ORDER BY v"},
	// Schema and types.
	{name: "core_types", schema: "main", sql: "SELECT id, v, s, d, b, i FROM t WHERE id = 1"},
	{name: "bytes", schema: "main", sql: "SELECT bs FROM bt WHERE id = 1"},
	{name: "array_column", schema: "main", sql: "SELECT xs FROM arr WHERE id = 1"},
	{name: "date_column", schema: "date_column", sql: "SELECT id FROM dt"},
	{name: "timestamp_column", schema: "timestamp_column", sql: "SELECT id FROM ts"},
	{name: "scalar_not_null", schema: "scalar_not_null", sql: "SELECT id FROM nn"},
	{name: "information_schema_tables", schema: "main", sql: "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.\"TABLES\" WHERE TABLE_NAME = 'T'"},
	{name: "information_schema_columns", schema: "main", sql: "SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = 'T' AND COLUMN_NAME = 'V'"},
	// Error codes.
	{name: "unknown_table", schema: "main", sql: "SELECT id FROM nosuch"},
	{name: "unknown_column", schema: "main", sql: "SELECT nosuch FROM t"},
	{name: "ambiguous_column", schema: "main", sql: "SELECT id FROM t, u"},
	{name: "unknown_qualifier", schema: "main", sql: "SELECT x.id FROM t"},
	{name: "comparison_type_mismatch", schema: "main", sql: "SELECT id FROM t WHERE s = 1"},
	{name: "between_type_mismatch", schema: "main", sql: "SELECT id FROM t WHERE s BETWEEN 1 AND 2"},
	{name: "in_list_type_mismatch", schema: "main", sql: "SELECT id FROM t WHERE s IN (1, 2)"},
	{name: "group_by_violation", schema: "main", sql: "SELECT id, COUNT(*) AS n FROM t GROUP BY v"},
	{name: "duplicate_order_by", schema: "main", sql: "SELECT v FROM t ORDER BY v, v"},
	{name: "union_column_mismatch", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT id, w FROM u"},
	{name: "union_type_mismatch", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT s FROM t"},
	{name: "not_null_violation", schema: "array_not_null", setup: []string{"INSERT INTO na VALUES (1, NULL)"}, sql: "SELECT id FROM na"},
	{name: "not_null_omitted", schema: "array_not_null", setup: []string{"INSERT INTO na (id) VALUES (1)"}, sql: "SELECT id FROM na"},
	{name: "primary_key_violation", schema: "main", setup: []string{"INSERT INTO t VALUES (1, 1, 'x', 0.0, true, 1)"}, sql: "SELECT id FROM t"},
	{name: "unique_index_violation", schema: "main", setup: []string{"INSERT INTO u VALUES (9, 1, 5)"}, sql: "SELECT id FROM u"},
	{name: "integer_overflow", schema: "main", sql: "SELECT v * 9223372036854775807 AS x FROM t WHERE id = 1"},
	{name: "int32_overflow", schema: "main", setup: []string{"INSERT INTO t VALUES (9, 1, 'z', 0.0, true, 3000000000)"}, sql: "SELECT i FROM t WHERE id = 9"},
	{name: "unknown_function", schema: "main", sql: "SELECT nosuchfn(v) FROM t"},
	{name: "unknown_table_function", schema: "main", sql: "SELECT * FROM nosuchtvf(1)"},
}

var generatedAlias = regexp.MustCompile(`q[0-9a-f]{8}(_[0-9a-f]{4}){3}_[0-9a-f]{12}`)

// renderDocClaim renders one engine's answer. Rows are sorted unless the query
// orders them, so a probe pins what an engine answers, not its plan's order.
func renderDocClaim(result plandiff.RunResult, sql string) string {
	if result.Err != nil {
		var je *plandiff.JavaError
		var ge *api.Error
		if errors.As(result.Err, &je) {
			// Java names a quantifier by a random UUID in some messages.
			msg := generatedAlias.ReplaceAllString(je.Message, "q<uuid>")
			return fmt.Sprintf("ERROR %s %s %q", je.ExceptionClass, je.SQLState, msg)
		}
		if errors.As(result.Err, &ge) {
			return fmt.Sprintf("ERROR %s %q", ge.Code, ge.Message)
		}
		return fmt.Sprintf("ERROR %v", result.Err)
	}
	rows := make([]string, 0, len(result.Rows.Rows))
	for _, r := range result.Rows.Rows {
		encoded, err := json.Marshal(r)
		Expect(err).NotTo(HaveOccurred())
		rows = append(rows, string(encoded))
	}
	if upper := strings.ToUpper(sql); !strings.Contains(upper, "ORDER BY") && !strings.Contains(upper, "TRAVERSAL ORDER") {
		sort.Strings(rows)
	}
	columns, err := json.Marshal(result.Rows.Columns)
	Expect(err).NotTo(HaveOccurred())
	return fmt.Sprintf("%s [%s]", columns, strings.Join(rows, ","))
}

// DocClaimsJavaProbe pins Java 4.14.2.0's answer to every Java claim of
// DIVERGENCES.md's "Go-Only Extensions" table and SQL_CONFORMANCE.md's matrix.
var _ = Describe("DocClaimsJavaProbe", func() {
	It("pins Java's and Go's answer to each documented feature claim", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "docclaims_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)

		// Every case runs before the verdict, so one run reports all drifted pins.
		var drift []string
		for _, c := range docClaimCases {
			schema, ok := docClaimSchemas[c.schema]
			Expect(ok).To(BeTrue(), c.name)
			setup := append(append([]string(nil), schema.setup...), c.setup...)
			jr := javaRunner.RunWithSetup(ctx, schema.ddl, setup, c.sql)
			gr := goRunner.RunWithSetup(ctx, schema.ddl, setup, c.sql)
			java, goAnswer := renderDocClaim(jr, c.sql), renderDocClaim(gr, c.sql)
			fmt.Fprintf(GinkgoWriter, "DOC-CLAIM %s\n JAVA %s\n GO %s\n SQL %s\n", c.name, java, goAnswer, c.sql)
			if want, ok := docClaimWantJava[c.name]; !ok || java != want {
				drift = append(drift, fmt.Sprintf("Java %q: %q,", c.name, java))
			}
			wantGo, distinct := docClaimWantGo[c.name]
			if !distinct {
				wantGo = docClaimWantJava[c.name]
				if jr.Err != nil {
					drift = append(drift, fmt.Sprintf("Go (Java errored, so Go needs its own pin) %q: %q,", c.name, goAnswer))
					continue
				}
			}
			if goAnswer != wantGo {
				drift = append(drift, fmt.Sprintf("Go %q: %q,", c.name, goAnswer))
			}
		}
		cases := map[string]bool{}
		for _, c := range docClaimCases {
			Expect(cases).NotTo(HaveKey(c.name), "duplicate case %s", c.name)
			cases[c.name] = true
		}
		for _, pins := range []map[string]string{docClaimWantJava, docClaimWantGo} {
			for name := range pins {
				Expect(cases).To(HaveKey(name), "pin %s names no case", name)
			}
		}
		Expect(drift).To(BeEmpty(), "answers that differ from their pins:\n%s", strings.Join(drift, "\n"))
	})
})

// docClaimCitation is a backticked multi-word name in a document; a DSN key is
// not a citation.
var docClaimCitation = regexp.MustCompile("(DSN )?`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")

// findAbove reads the repository file rel from the nearest directory above the
// working directory that has it.
func findAbove(rel string) string {
	dir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	for {
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err == nil {
			return string(raw)
		}
		parent := filepath.Dir(dir)
		Expect(parent).NotTo(Equal(dir), "no %s above the working directory", rel)
		dir = parent
	}
}

var _ = Describe("DocClaimsJavaProbe citations", func() {
	It("names every case in the documents, and cites only cases or cross-engine entries", func() {
		sqlConformance := findAbove("SQL_CONFORMANCE.md")
		divergences := findAbove("DIVERGENCES.md")
		// DIVERGENCES.md cites cases in the Go-only extensions table and the
		// Java upstream bugs rows above the signed-zero row.
		section := func(from, to string) string {
			begin := strings.Index(divergences, from)
			end := strings.Index(divergences, to)
			Expect(begin).To(BeNumerically(">=", 0), from)
			Expect(end).To(BeNumerically(">", begin), to)
			return divergences[begin:end]
		}
		cited := sqlConformance + section("## Go-Only Extensions", "### Go-only SQLSTATEs") +
			section("## Java Upstream Bugs", "| Signed-zero comparison")

		known := map[string]bool{}
		for _, c := range docClaimCases {
			known[c.name] = true
			Expect(cited).To(ContainSubstring("`"+c.name+"`"), "case %s is cited by neither document", c.name)
		}
		for _, q := range seedcorpus.SeedRunCorpus() {
			known[q.Name] = true
		}
		for _, m := range docClaimCitation.FindAllStringSubmatch(cited, -1) {
			if m[1] == "" {
				Expect(known).To(HaveKey(m[2]), "the documents cite %s, which is neither a case nor a cross-engine entry", m[2])
			}
		}
	})
})

// docClaimWantJava is Java 4.14.2.0's answer to each case.
var docClaimWantJava = map[string]string{
	"select":                           `[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"}] [[1,10]]`,
	"insert_select":                    `[{"name":"ID","type":"BIGINT"},{"name":"T_ID","type":"BIGINT"},{"name":"W","type":"BIGINT"}] [[12,2,20]]`,
	"insert_select_column_list":        `ERROR RelationalException 0AF00 "setting column ordering for insert with select is not supported"`,
	"update":                           `[{"name":"V","type":"BIGINT"}] [[11]]`,
	"delete":                           `[{"name":"ID","type":"BIGINT"}] [[2],[3]]`,
	"arithmetic":                       `[{"name":"A","type":"BIGINT"},{"name":"B","type":"BIGINT"},{"name":"C","type":"BIGINT"},{"name":"E","type":"BIGINT"},{"name":"F","type":"BIGINT"}] [[21,19,40,6,2]]`,
	"searched_case":                    `[{"name":"C","type":"STRING"}] [["hi"]]`,
	"simple_case":                      `[{"name":"C","type":"STRING"}] [["other"]]`,
	"simple_case_no_else":              `[{"name":"C","type":"STRING"}] [["twenty"]]`,
	"cast":                             `[{"name":"C","type":"STRING"}] [["10"]]`,
	"cast_overflow":                    `ERROR SemanticException 22F3H "Invalid cast operation Value out of range for INT: 9223372036854775807"`,
	"coalesce":                         `[{"name":"C","type":"BOOLEAN"}] [[true]]`,
	"nullif":                           `ERROR RelationalException 0AF00 "Unsupported operator NULLIF"`,
	"greatest_least":                   `[{"name":"G","type":"BIGINT"},{"name":"L","type":"BIGINT"}] [[15,10]]`,
	"cardinality":                      `[{"name":"N","type":"INTEGER"}] [[3]]`,
	"upper":                            `ERROR RelationalException 0AF00 "Unsupported operator UPPER"`,
	"position_in":                      `[{"name":"P","type":"STRING"}] [["a"]]`,
	"abs":                              `ERROR RelationalException 0AF00 "Unsupported operator ABS"`,
	"current_timestamp":                `ERROR RelationalException XX000 "expected Expression but got null"`,
	"current_date":                     `ERROR RelationalException XX000 "expected Expression but got null"`,
	"date_part":                        `ERROR RelationalException XX000 "expected Expression but got null"`,
	"date_part_of_integer":             `ERROR RelationalException 0AF00 "Unsupported operator YEAR"`,
	"extract_year":                     `[{"name":"Y","type":"BIGINT"}] [[10]]`,
	"cast_date":                        `ERROR RelationalException 42601 "syntax error:\nSELECT CAST('2024-03-15' AS DATE) AS c FROM t WHERE id = 1\n                            ^^^^"`,
	"cast_timestamp":                   `ERROR RelationalException 42601 "syntax error:\nSELECT CAST('2024-03-15 10:00:00' AS TIMESTAMP) AS c FROM t WHERE id = 1\n                                     ^^^^^^^^^"`,
	"symbolic_and":                     `ERROR RelationalException 0AF00 "Unsupported operator &&"`,
	"symbolic_or":                      `ERROR RelationalException 0AF00 "Unsupported operator ||"`,
	"xor":                              `ERROR RelationalException 0AF00 "Unsupported operator XOR"`,
	"and_or_not":                       `[{"name":"ID","type":"BIGINT"}] [[1],[2],[3]]`,
	"between":                          `[{"name":"ID","type":"BIGINT"}] [[1],[3]]`,
	"in_list":                          `[{"name":"ID","type":"BIGINT"}] [[1],[3]]`,
	"in_subquery":                      `ERROR RelationalException 0AF00 "IN predicate does not support nested SELECT"`,
	"like":                             `[{"name":"ID","type":"BIGINT"}] [[1]]`,
	"is_null":                          `[{"name":"ID","type":"BIGINT"}] [[3]]`,
	"is_distinct_from":                 `[{"name":"ID","type":"BIGINT"}] [[2],[3]]`,
	"byte_literal":                     `[{"name":"ID","type":"BIGINT"}] [[1]]`,
	"group_by_ordered":                 `[{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"}] [[10,2],[20,1]]`,
	"group_by_primary_key":             `[{"name":"ID","type":"BIGINT"},{"name":"N","type":"BIGINT"}] [[1,1],[2,1],[3,1]]`,
	"group_by_unordered":               `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
	"group_by_expression":              `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
	"aggregates":                       `[{"name":"C","type":"BIGINT"},{"name":"S","type":"BIGINT"},{"name":"A","type":"DOUBLE"},{"name":"MN","type":"BIGINT"},{"name":"MX","type":"BIGINT"}] [[3,40,13.333333333333334,10,20]]`,
	"having":                           `[{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"}] [[10,2]]`,
	"count_distinct":                   `ERROR RelationalException 0AF00 "aggregator DISTINCT is not supported"`,
	"empty_aggregates":                 `[{"name":"C","type":"BIGINT"},{"name":"S","type":"BIGINT"}] [[0,null]]`,
	"union_all":                        `[{"name":"ID","type":"BIGINT"}] [[1],[1],[2],[2],[3],[3]]`,
	"union_distinct":                   `ERROR RelationalException 0AF00 "only UNION ALL is supported"`,
	"intersect":                        `ERROR RelationalException 42601 "syntax error:\nSELECT id FROM t INTERSECT SELECT id FROM u\n                           ^^^^^^"`,
	"except":                           `ERROR RelationalException 42601 "syntax error:\nSELECT id FROM t EXCEPT SELECT id FROM u\n                        ^^^^^^"`,
	"union_order_by_position":          `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
	"inner_join":                       `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1],[3,2]]`,
	"comma_join":                       `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1],[3,2]]`,
	"cross_join":                       `ERROR NullPointerException XXXXX "Cannot invoke \"com.apple.foundationdb.relational.generated.RelationalParser$ExpressionContext.accept(org.antlr.v4.runtime.tree.ParseTreeVisitor)\" because \"expressionCtx\" is null"`,
	"left_join":                        `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1],[2,null],[3,2]]`,
	"right_join":                       `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1],[3,2],[null,3]]`,
	"full_join":                        `ERROR RelationalException 42601 "syntax error:\nSELECT t.id, u.id FROM t FULL OUTER JOIN u ON t.id = u.t_id\n                              ^^^^^"`,
	"full_join_without_outer":          `[{"name":"ID","type":"BIGINT"}] [[1],[1],[1]]`,
	"self_join":                        `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,3]]`,
	"three_way_join":                   `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1,1],[3,2,3]]`,
	"scalar_subquery":                  `ERROR RelationalException 42601 "syntax error:\nSELECT id, (SELECT MAX(v) FROM t) AS m FROM t\n            ^^^^^^"`,
	"correlated_scalar_subquery":       `ERROR RelationalException 42601 "syntax error:\nSELECT id, (SELECT MAX(w) FROM u WHERE u.t_id = t.id) AS m FROM t\n            ^^^^^^"`,
	"exists":                           `[{"name":"ID","type":"BIGINT"}] [[1],[3]]`,
	"not_exists":                       `[{"name":"ID","type":"BIGINT"}] [[2]]`,
	"exists_projection":                `[{"name":"ID","type":"BIGINT"},{"name":"E","type":"BOOLEAN"}] [[1,true],[2,false],[3,true]]`,
	"derived_table":                    `[{"name":"K","type":"BIGINT"}] [[2],[3]]`,
	"unnest":                           `[{"name":"ID","type":"BIGINT"},{"name":"E","type":"BIGINT"}] [[1,1],[1,2],[1,3]]`,
	"unnest_ordinality":                `[{"name":"ID","type":"BIGINT"},{"name":"E","type":"BIGINT"},{"name":"AT","type":"INTEGER"}] [[1,1,1],[1,2,2],[1,3,3]]`,
	"with":                             `[{"name":"ID","type":"BIGINT"}] [[1],[2],[3]]`,
	"with_column_rename":               `[{"name":"K","type":"BIGINT"}] [[1],[2],[3]]`,
	"with_chained":                     `[{"name":"ID","type":"BIGINT"}] [[2],[3]]`,
	"recursive_level_order":            `[{"name":"ID","type":"BIGINT"}] [[1],[10],[20],[40],[50]]`,
	"recursive_pre_order":              `[{"name":"ID","type":"BIGINT"}] [[1],[10],[40],[50],[20]]`,
	"recursive_union_distinct":         `[{"name":"ID","type":"BIGINT"}] [[10],[1],[20],[40],[50]]`,
	"recursive_union_distinct_diamond": `[{"name":"ID","type":"BIGINT"}] [[1],[2],[3],[4],[4]]`,
	"order_by_index":                   `[{"name":"V","type":"BIGINT"}] [[10],[10],[20]]`,
	"order_by_no_index":                `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
	"order_by_aggregate":               `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
	"order_by_nulls_first":             `[{"name":"V","type":"BIGINT"}] [[null],[10],[10],[20]]`,
	"order_by_nulls_last":              `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
	"limit":                            `ERROR RelationalException 0AF00 "LIMIT clause is not supported."`,
	"limit_offset":                     `ERROR RelationalException 0AF00 "OFFSET clause is not supported."`,
	"limit_in_subquery":                `ERROR RelationalException 0AF00 "LIMIT clause is not supported."`,
	"distinct_ordered":                 `[{"name":"V","type":"BIGINT"}] [[10],[10],[20]]`,
	"distinct_unordered":               `[{"name":"K","type":"BIGINT"}] [[5],[5],[6]]`,
	"distinct_order_by":                `[{"name":"V","type":"BIGINT"}] [[10],[10],[20]]`,
	"core_types":                       `[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"S","type":"STRING"},{"name":"D","type":"DOUBLE"},{"name":"B","type":"BOOLEAN"},{"name":"I","type":"INTEGER"}] [[1,10,"a",1.5,true,1]]`,
	"bytes":                            `[{"name":"BS","type":"BINARY"}] [["yv4="]]`,
	"array_column":                     `[{"name":"XS","type":"ARRAY"}] [[[1,2,3]]]`,
	"date_column":                      `ERROR RelationalException 42F18 "could not find type 'DATE'"`,
	"timestamp_column":                 `ERROR RelationalException 42F18 "could not find type 'TIMESTAMP'"`,
	"scalar_not_null":                  `ERROR RelationalException 0A000 "NOT NULL is only allowed for ARRAY column type"`,
	"information_schema_tables":        `ERROR RelationalException 42703 "Unknown reference INFORMATION_SCHEMA.TABLES"`,
	"information_schema_columns":       `ERROR RelationalException 42703 "Unknown reference INFORMATION_SCHEMA.COLUMNS"`,
	"unknown_table":                    `ERROR RelationalException 42F01 "Unknown table NOSUCH"`,
	"unknown_column":                   `ERROR RelationalException 42703 "Attempting to query non existing column NOSUCH"`,
	"ambiguous_column":                 `ERROR RelationalException 42702 "Ambiguous reference ID"`,
	"unknown_qualifier":                `ERROR RelationalException 42703 "Attempting to query non existing column X.ID"`,
	"comparison_type_mismatch":         `ERROR SemanticException 42804 "The operands of a comparison operator are not compatible."`,
	"between_type_mismatch":            `ERROR SemanticException 42804 "The operands of a comparison operator are not compatible."`,
	"in_list_type_mismatch":            `ERROR SemanticException 22000 "A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable."`,
	"group_by_violation":               `ERROR RelationalException 42803 "Invalid reference to non-grouping expression T.ID|long ∪ ∅| ⇾ q<uuid>._0.ID"`,
	"duplicate_order_by":               `ERROR RelationalException 42701 "Order by column T.V is duplicated in the order by clause"`,
	"union_column_mismatch":            `ERROR RelationalException 42F64 "UNION legs do not have the same number of columns"`,
	"union_type_mismatch":              `ERROR RelationalException 42F65 "Incompatible column types in UNION legs"`,
	"not_null_violation":               `ERROR VerifyException XX000 "Cannot set a non-nullable field to the NULL value"`,
	"not_null_omitted":                 `ERROR RelationalException 23502 "null value in column \"XS\" violates not-null constraint"`,
	"primary_key_violation":            `ERROR RecordAlreadyExistsException 23505 "record already exists"`,
	"unique_index_violation":           `ERROR RecordIndexUniquenessViolation 23505 "Duplicate entry for unique index"`,
	"integer_overflow":                 `ERROR ArithmeticException XXXXX "long overflow"`,
	"int32_overflow":                   `ERROR SemanticException 22000 "A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable."`,
	"unknown_function":                 `ERROR RelationalException 0AF00 "Unsupported operator NOSUCHFN"`,
	"unknown_table_function":           `ERROR RelationalException 42883 "Unknown function NOSUCHTVF"`,
}

// docClaimWantGo is Go's answer where it differs from Java's or Java errors.
var docClaimWantGo = map[string]string{
	"insert_select_column_list":        `ERROR 0AF00 "setting column ordering for insert with select is not supported"`,
	"simple_case":                      `[{"name":"C","type":"STRING"}] [["ten"]]`,
	"simple_case_no_else":              `[{"name":"C","type":"STRING"}] [["ten"]]`,
	"cast_overflow":                    `ERROR 22F3H "Invalid cast operation Value out of range for INT: 9223372036854775807"`,
	"nullif":                           `ERROR 0AF00 "Unsupported operator NULLIF"`,
	"upper":                            `[{"name":"C","type":"STRING"}] [["A"]]`,
	"position_in":                      `ERROR 0AF00 "projection slot 0 has no resolved Value"`,
	"abs":                              `[{"name":"C","type":"DOUBLE"}] [[2.5]]`,
	"current_timestamp":                `[{"name":"C","type":"BOOLEAN"}] [[true]]`,
	"current_date":                     `[{"name":"C","type":"BOOLEAN"}] [[true]]`,
	"date_part":                        `[{"name":"C","type":"BOOLEAN"}] [[true]]`,
	"date_part_of_integer":             `[{"name":"Y","type":"BIGINT"}] [[null]]`,
	"extract_year":                     `ERROR 0AF00 "projection slot 0 has no resolved Value"`,
	"cast_date":                        `[{"name":"C","type":"DATE"}] [["2024-03-15"]]`,
	"cast_timestamp":                   `[{"name":"C","type":"TIMESTAMP"}] [["2024-03-15 10:00:00"]]`,
	"symbolic_and":                     `[{"name":"ID","type":"BIGINT"}] [[1]]`,
	"symbolic_or":                      `[{"name":"ID","type":"BIGINT"}] [[1],[2]]`,
	"xor":                              `[{"name":"ID","type":"BIGINT"}] [[3]]`,
	"in_subquery":                      `ERROR 0AF00 "Cascades planner could not plan query"`,
	"group_by_unordered":               `[{"name":"S","type":"STRING"},{"name":"N","type":"BIGINT"}] [["a",1],["b",1],["c",1]]`,
	"group_by_expression":              `[{"name":"K","type":"BIGINT"},{"name":"N","type":"BIGINT"}] [[11,2],[21,1]]`,
	"count_distinct":                   `ERROR 0AF00 "DISTINCT aggregates are not supported"`,
	"union_distinct":                   `ERROR 0AF00 "only UNION ALL is supported"`,
	"intersect":                        `ERROR 42601 "syntax error:\nSELECT id FROM t INTERSECT SELECT id FROM u\n                           ^^^^^^"`,
	"except":                           `ERROR 42601 "syntax error:\nSELECT id FROM t EXCEPT SELECT id FROM u\n                        ^^^^^^"`,
	"union_order_by_position":          `[{"name":"ID","type":"BIGINT"}] [[1],[1],[2],[2],[3],[3]]`,
	"cross_join":                       `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1],[1,2],[1,3],[2,1],[2,2],[2,3],[3,1],[3,2],[3,3]]`,
	"full_join":                        `[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}] [[1,1],[2,null],[3,2],[null,3]]`,
	"full_join_without_outer":          `[{"name":"ID","type":"BIGINT"}] [[1],[1],[1],[2],[3]]`,
	"scalar_subquery":                  `[{"name":"ID","type":"BIGINT"},{"name":"M","type":"BIGINT"}] [[1,20],[2,20],[3,20]]`,
	"correlated_scalar_subquery":       `[{"name":"ID","type":"BIGINT"},{"name":"M","type":"BIGINT"}] [[1,5],[2,null],[3,6]]`,
	"recursive_union_distinct_diamond": `[{"name":"ID","type":"BIGINT"}] [[1],[2],[3],[4]]`,
	"order_by_no_index":                `[{"name":"ID","type":"BIGINT"},{"name":"S","type":"STRING"}] [[1,"a"],[2,"b"],[3,"c"]]`,
	"order_by_aggregate":               `[{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"}] [[20,1],[10,2]]`,
	"order_by_nulls_last":              `[{"name":"V","type":"BIGINT"}] [[10],[10],[20],[null]]`,
	"limit":                            `[{"name":"ID","type":"BIGINT"}] [[1],[2]]`,
	"limit_offset":                     `[{"name":"ID","type":"BIGINT"}] [[2],[3]]`,
	"limit_in_subquery":                `[{"name":"K","type":"BIGINT"}] [[1]]`,
	"distinct_ordered":                 `[{"name":"V","type":"BIGINT"}] [[10],[20]]`,
	"distinct_unordered":               `[{"name":"K","type":"BIGINT"}] [[5],[6]]`,
	"distinct_order_by":                `[{"name":"V","type":"BIGINT"}] [[10],[20]]`,
	"date_column":                      `[{"name":"ID","type":"BIGINT"}] []`,
	"timestamp_column":                 `[{"name":"ID","type":"BIGINT"}] []`,
	"scalar_not_null":                  `ERROR 0A000 "NOT NULL is only allowed for ARRAY column type"`,
	"information_schema_tables":        `[{"name":"TABLE_NAME","type":""}] [["T"]]`,
	"information_schema_columns":       `[{"name":"COLUMN_NAME","type":""}] [["V"]]`,
	"unknown_table":                    `ERROR 42F01 "table \"NOSUCH\" does not exist"`,
	"unknown_column":                   `ERROR 42703 "Attempting to query non existing column NOSUCH"`,
	"ambiguous_column":                 `ERROR 42702 "Ambiguous reference ID"`,
	"unknown_qualifier":                `ERROR 42703 "Attempting to query non existing column X.ID"`,
	"comparison_type_mismatch":         `ERROR 42804 "The operands of a comparison operator are not compatible."`,
	"between_type_mismatch":            `ERROR 42804 "The operands of a comparison operator are not compatible."`,
	"in_list_type_mismatch":            `ERROR 22000 "A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable."`,
	"group_by_violation":               `ERROR 42803 "column \"ID\" must appear in the GROUP BY clause or be used in an aggregate function"`,
	"duplicate_order_by":               `ERROR 42701 "duplicate column \"V\" in ORDER BY"`,
	"union_column_mismatch":            `ERROR 42F64 "UNION legs do not have the same number of columns"`,
	"union_type_mismatch":              `ERROR 42F65 "Incompatible column types in UNION legs"`,
	"not_null_violation":               `ERROR 23502 "NULL value in column \"XS\" violates NOT NULL constraint"`,
	"not_null_omitted":                 `ERROR 23502 "null value in column \"XS\" violates not-null constraint"`,
	"primary_key_violation":            `ERROR 23505 "record already exists"`,
	"unique_index_violation":           `ERROR 23505 "unique index \"U_W\" violated: value (5) already exists"`,
	"integer_overflow":                 `ERROR 22003 "long overflow"`,
	"int32_overflow":                   `ERROR 22000 "A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable."`,
	"unknown_function":                 `ERROR 0AF00 "Unsupported operator NOSUCHFN"`,
	"unknown_table_function":           `ERROR 42883 "Unknown table function NOSUCHTVF"`,
}
