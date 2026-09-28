//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The singleton source has no visible attributes even though Java implements
// its cardinality with Explode([true]). These cases distinguish that source from
// a projected dummy column and exercise the ordinary query-block consumers.
var _ = Describe("FromlessSelectJavaProbe", func() {
	It("pins Java singleton-source rows, metadata, name resolution and composition", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "fromless_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) CREATE TABLE wide_t (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE wide_ordered_t (id BIGINT, v BIGINT, PRIMARY KEY (id, v))"
		setup := []string{"INSERT INTO t VALUES (1), (2)", "INSERT INTO wide_t VALUES (2, 7)", "INSERT INTO wide_ordered_t VALUES (2, 7)"}
		probes := []struct{ name, sql string }{
			{"literal", "SELECT 1"},
			{"arithmetic", "SELECT 1 + 2"},
			{"function", "SELECT UPPER('hi')"},
			{"pi", "SELECT PI()"},
			{"string", "SELECT 'hello'"},
			{"abc", "SELECT 'abc'"},
			{"multiple_constants", "SELECT 1 + 2, 'hello', 42"},
			{"null", "SELECT NULL"},
			{"boolean", "SELECT NOT TRUE"},
			{"cast", "SELECT CAST(1.7 AS BIGINT)"},
			{"record", "SELECT (1 AS a, 'x' AS b) AS r"},
			{"count", "SELECT COUNT(*) AS n"},
			{"sum", "SELECT SUM(2) AS n"},
			{"group", "SELECT 1 AS a, COUNT(*) AS n GROUP BY 1"},
			{"distinct_order", "SELECT DISTINCT 1 AS n ORDER BY n"},
			{"star", "SELECT *"},
			{"star_and_literal", "SELECT *, 1 AS n"},
			{"qualified_star", "SELECT t.*"},
			{"unknown", "SELECT missing"},
			{"hidden_bool", "SELECT _0"},
			{"derived", "SELECT d.n FROM (SELECT 2 AS n) d"},
			{"cte", "WITH c AS (SELECT 3 AS n) SELECT n FROM c"},
			{"union", "SELECT 1 AS n UNION ALL SELECT 2 AS n"},
			{"recursive", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT n FROM c"},
			{"recursive_order", "WITH RECURSIVE counter(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM counter WHERE n < 5) SELECT n FROM counter ORDER BY n"},
			{"recursive_filter", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT n FROM c WHERE n > 1"},
			{"recursive_sum", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT SUM(n) AS s FROM c"},
			{"recursive_group", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT n, COUNT(*) AS ct FROM c GROUP BY n ORDER BY n DESC"},
			{"recursive_join", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT c.n FROM c, t WHERE c.n = t.id AND t.id = 1"},
			{"scalar", "SELECT (SELECT 4) AS n"},
			{"exists", "SELECT EXISTS(SELECT 1) AS e"},
			{"outer_scalar", "SELECT t.id, (SELECT t.id + 1) AS n FROM t ORDER BY t.id"},
			{"outer_exists", "SELECT t.id FROM t WHERE EXISTS(SELECT t.id) ORDER BY t.id"},
			{"outer_empty", "SELECT t.id, (SELECT t.id + 1) AS n FROM t WHERE t.id = 9"},
			{"zero_limit", "SELECT 1 LIMIT 0"},
			{"offset", "SELECT 1 LIMIT 1 OFFSET 1"},
			{"quoted_hidden_bool", "SELECT \"_0\""},
			{"derived_star", "SELECT d.* FROM (SELECT *) d"},
			{"cte_star", "WITH c AS (SELECT *) SELECT * FROM c"},
			{"cte_star_alias", "WITH c(n) AS (SELECT *) SELECT * FROM c"},
			{"derived_count", "SELECT COUNT(*) AS n FROM (SELECT *) d"},
			{"star_union", "SELECT * UNION ALL SELECT *"},
			{"star_distinct", "SELECT DISTINCT *"},
			{"star_scalar", "SELECT d.*, (SELECT 4) AS n FROM (SELECT *) d"},
			{"star_cross", "SELECT d.*, t.id FROM (SELECT *) d, t ORDER BY t.id"},
			{"star_aggregate", "SELECT *, COUNT(*) AS n"},
			{"table_star_and_literal", "SELECT *, 8 AS x FROM t WHERE id = 2"},
			{"table_star_between_literals", "SELECT 8 AS x, *, 9 AS y FROM t WHERE id = 2"},
			{"table_repeated_star", "SELECT *, 8 AS x, * FROM t WHERE id = 2"},
			{"table_star_and_aggregate", "SELECT *, COUNT(*) AS n FROM t WHERE id = 2 GROUP BY id"},
			{"table_repeated_star_aggregate", "SELECT *, COUNT(*) AS n, * FROM t WHERE id = 2 GROUP BY id"},
			{"table_star_between_aggregates", "SELECT COUNT(*) AS n, *, COUNT(*) AS n2 FROM t WHERE id = 2 GROUP BY id"},
			{"wide_repeated_star", "SELECT 8 AS x, *, COUNT(*) AS n, *, 9 AS y FROM wide_t WHERE id = 2 GROUP BY id, v"},
			{"wide_ordered_repeated_star", "SELECT 8 AS x, *, COUNT(*) AS n, *, 9 AS y FROM wide_ordered_t WHERE id = 2 GROUP BY id, v"},
			{"zero_star_positional_group", "SELECT *, 1 AS a, COUNT(*) AS n GROUP BY 1"},
			{"one_star_group_no_aggregate", "SELECT *, 8 AS x FROM t WHERE id = 2 GROUP BY id"},
			{"wide_star_group_no_aggregate", "SELECT *, 8 AS x FROM wide_ordered_t GROUP BY id, v"},
			{"two_source_star_order", "SELECT a.*, b.* FROM t a, t b ORDER BY 1, 2 DESC"},
			{"star_aliased_column_order", "SELECT a.*, b.id AS id FROM t a, t b ORDER BY 1, 2 DESC"},
			{"two_source_star_named_order", "SELECT a.*, b.* FROM t a, t b ORDER BY a.id, b.id DESC"},
			{"star_aliased_column_named_order", "SELECT a.*, b.id AS id FROM t a, t b ORDER BY a.id, b.id DESC"},
			{"wide_star_positional_order", "SELECT *, 8 AS x FROM wide_t ORDER BY 3"},
			{"group_constant", "SELECT 1 AS a, COUNT(*) AS n GROUP BY a"},
			{"group_star", "SELECT * GROUP BY 1"},
		}
		render := func(result plandiff.RunResult) string {
			if result.Err != nil {
				var je *plandiff.JavaError
				var ge *api.Error
				if errors.As(result.Err, &je) {
					return fmt.Sprintf("ERROR %s %s %q", je.ExceptionClass, je.SQLState, je.Message)
				}
				if errors.As(result.Err, &ge) {
					return fmt.Sprintf("ERROR %s %q", ge.Code, ge.Message)
				}
				return fmt.Sprintf("ERROR %v", result.Err)
			}
			// Columns and rows only: these pins predate RowSet.Nullability, which the
			// WS-E oracle measures and pins on its own shapes.
			encoded, err := json.Marshal(struct {
				Columns []plandiff.Column `json:"columns"`
				Rows    [][]any           `json:"rows"`
			}{result.Rows.Columns, result.Rows.Rows})
			Expect(err).NotTo(HaveOccurred())
			return string(encoded)
		}
		wantJava := map[string]string{
			"two_source_star_order":           `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			"star_aliased_column_order":       `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			"two_source_star_named_order":     `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			"star_aliased_column_named_order": `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			// ImplementStreamingAggregationRule requires ordering on the grouping
			// values. A PK on ID alone does not supply V; the composite-PK
			// control keeps the same wide star slots while providing that order.
			"zero_star_positional_group":    `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			"one_star_group_no_aggregate":   `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"X","type":"INTEGER"}],"rows":[[2,8]]}`,
			"wide_star_group_no_aggregate":  `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"X","type":"INTEGER"}],"rows":[[2,7,8]]}`,
			"wide_star_positional_order":    `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			"wide_repeated_star":            `ERROR UnableToPlanException 0AF00 "Cascades planner could not plan query"`,
			"wide_ordered_repeated_star":    `{"columns":[{"name":"X","type":"INTEGER"},{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"},{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"Y","type":"INTEGER"}],"rows":[[8,2,7,1,2,7,9]]}`,
			"abc":                           `{"columns":[{"name":"_0","type":"STRING"}],"rows":[["abc"]]}`,
			"multiple_constants":            `{"columns":[{"name":"_0","type":"INTEGER"},{"name":"_1","type":"STRING"},{"name":"_2","type":"INTEGER"}],"rows":[[3,"hello",42]]}`,
			"table_star_and_literal":        `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"X","type":"INTEGER"}],"rows":[[2,8]]}`,
			"table_star_between_literals":   `{"columns":[{"name":"X","type":"INTEGER"},{"name":"ID","type":"BIGINT"},{"name":"Y","type":"INTEGER"}],"rows":[[8,2,9]]}`,
			"table_repeated_star":           `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"X","type":"INTEGER"},{"name":"ID","type":"BIGINT"}],"rows":[[2,8,2]]}`,
			"table_star_and_aggregate":      `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"N","type":"BIGINT"}],"rows":[[2,1]]}`,
			"table_repeated_star_aggregate": `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"N","type":"BIGINT"},{"name":"ID","type":"BIGINT"}],"rows":[[2,1,2]]}`,
			"table_star_between_aggregates": `{"columns":[{"name":"N","type":"BIGINT"},{"name":"ID","type":"BIGINT"},{"name":"N2","type":"BIGINT"}],"rows":[[1,2,1]]}`,
			"literal":                       "{\"columns\":[{\"name\":\"_0\",\"type\":\"INTEGER\"}],\"rows\":[[1]]}",
			"arithmetic":                    "{\"columns\":[{\"name\":\"_0\",\"type\":\"INTEGER\"}],\"rows\":[[3]]}",
			"function":                      "ERROR RelationalException 0AF00 \"Unsupported operator UPPER\"",
			"pi":                            "ERROR RelationalException 0AF00 \"Unsupported operator PI\"",
			"string":                        "{\"columns\":[{\"name\":\"_0\",\"type\":\"STRING\"}],\"rows\":[[\"hello\"]]}",
			"null":                          "ERROR RecordCoreException XXXXX \"should not be called\"",
			"boolean":                       "{\"columns\":[{\"name\":\"_0\",\"type\":\"BOOLEAN\"}],\"rows\":[[false]]}",
			"cast":                          "{\"columns\":[{\"name\":\"_0\",\"type\":\"BIGINT\"}],\"rows\":[[2]]}",
			"record":                        "{\"columns\":[{\"name\":\"R\",\"type\":\"STRUCT\"}],\"rows\":[[{\"A\":1,\"B\":\"x\"}]]}",
			"count":                         "{\"columns\":[{\"name\":\"N\",\"type\":\"BIGINT\"}],\"rows\":[[1]]}",
			"sum":                           "{\"columns\":[{\"name\":\"N\",\"type\":\"INTEGER\"}],\"rows\":[[2]]}",
			"group":                         "ERROR UnableToPlanException 0AF00 \"Cascades planner could not plan query\"",
			"distinct_order":                "ERROR UnableToPlanException 0AF00 \"Cascades planner could not plan query\"",
			"star":                          "{\"columns\":[],\"rows\":[[]]}",
			"star_and_literal":              "{\"columns\":[{\"name\":\"N\",\"type\":\"INTEGER\"}],\"rows\":[[1]]}",
			"qualified_star":                "ERROR RelationalException 42703 \"Unknown reference T\"",
			"unknown":                       "ERROR RelationalException 42703 \"Attempting to query non existing column MISSING\"",
			"hidden_bool":                   "ERROR RelationalException 42601 \"syntax error:\\nline 1:7 token recognition error at: '_0'\"",
			"derived":                       "{\"columns\":[{\"name\":\"N\",\"type\":\"INTEGER\"}],\"rows\":[[2]]}",
			"cte":                           "{\"columns\":[{\"name\":\"N\",\"type\":\"INTEGER\"}],\"rows\":[[3]]}",
			"union":                         "{\"columns\":[{\"name\":\"N\",\"type\":\"INTEGER\"}],\"rows\":[[1],[2]]}",
			"recursive":                     "{\"columns\":[{\"name\":\"N\",\"type\":\"INTEGER\"}],\"rows\":[[1],[2],[3]]}",
			"recursive_order":               `ERROR RelationalException 0A000 "order by is not supported in subquery"`,
			"recursive_filter":              `{"columns":[{"name":"N","type":"INTEGER"}],"rows":[[2],[3]]}`,
			"recursive_sum":                 `{"columns":[{"name":"S","type":"INTEGER"}],"rows":[[6]]}`,
			"recursive_group":               `ERROR RelationalException 0A000 "order by is not supported in subquery"`,
			"recursive_join":                `{"columns":[{"name":"N","type":"INTEGER"}],"rows":[[1]]}`,
			"scalar":                        "ERROR RelationalException 42601 \"syntax error:\\nSELECT (SELECT 4) AS n\\n        ^^^^^^\"",
			"exists":                        "{\"columns\":[{\"name\":\"E\",\"type\":\"BOOLEAN\"}],\"rows\":[[true]]}",
			"outer_scalar":                  "ERROR RelationalException 42601 \"syntax error:\\nSELECT t.id, (SELECT t.id + 1) AS n FROM t ORDER BY t.id\\n              ^^^^^^\"",
			"outer_exists":                  "{\"columns\":[{\"name\":\"ID\",\"type\":\"BIGINT\"}],\"rows\":[[1],[2]]}",
			"outer_empty":                   "ERROR RelationalException 42601 \"syntax error:\\nSELECT t.id, (SELECT t.id + 1) AS n FROM t WHERE t.id = 9\\n              ^^^^^^\"",
			"zero_limit":                    "ERROR RelationalException 0AF00 \"LIMIT clause is not supported.\"",
			"offset":                        "ERROR RelationalException 0AF00 \"OFFSET clause is not supported.\"",
			"quoted_hidden_bool":            "ERROR RelationalException 42703 \"Attempting to query non existing column _0\"",
			"derived_star":                  "{\"columns\":[],\"rows\":[[]]}",
			"cte_star":                      "{\"columns\":[],\"rows\":[[]]}",
			"cte_star_alias":                "ERROR RelationalException 42F10 \"cte query has 0 column(s), however 1 aliases defined\"",
			"derived_count":                 "{\"columns\":[{\"name\":\"N\",\"type\":\"BIGINT\"}],\"rows\":[[1]]}",
			"star_union":                    "{\"columns\":[],\"rows\":[[],[]]}",
			"star_distinct":                 "{\"columns\":[],\"rows\":[[]]}",
			"star_scalar":                   "ERROR RelationalException 42601 \"syntax error:\\nSELECT d.*, (SELECT 4) AS n FROM (SELECT *) d\\n             ^^^^^^\"",
			"star_cross":                    "{\"columns\":[{\"name\":\"ID\",\"type\":\"BIGINT\"}],\"rows\":[[1],[2]]}",
			"star_aggregate":                "{\"columns\":[{\"name\":\"N\",\"type\":\"BIGINT\"}],\"rows\":[[1]]}",
			"group_constant":                "ERROR RelationalException 42703 \"Attempting to query non existing column A\"",
			"group_star":                    "ERROR UnableToPlanException 0AF00 \"Cascades planner could not plan query\"",
		}

		// Go's existing scalar-subquery, raw NULL, grouping, sorting and LIMIT
		// extensions have independent row contracts, not Java-error agreement.
		// Invalid references keep their SQLSTATE and Go's scoped diagnostics.
		wantGoDistinct := map[string]string{
			"star_aliased_column_named_order": `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}],"rows":[[1,2],[1,1],[2,2],[2,1]]}`,
			"two_source_star_named_order":     `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}],"rows":[[1,2],[1,1],[2,2],[2,1]]}`,
			"two_source_star_order":           `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}],"rows":[[1,2],[1,1],[2,2],[2,1]]}`,
			"star_aliased_column_order":       `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"ID","type":"BIGINT"}],"rows":[[1,2],[1,1],[2,2],[2,1]]}`,
			"zero_star_positional_group":      `{"columns":[{"name":"A","type":"INTEGER"},{"name":"N","type":"BIGINT"}],"rows":[[1,1]]}`,
			"wide_star_positional_order":      `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"X","type":"INTEGER"}],"rows":[[2,7,8]]}`,
			"wide_repeated_star":              `{"columns":[{"name":"X","type":"INTEGER"},{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"},{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"Y","type":"INTEGER"}],"rows":[[8,2,7,1,2,7,9]]}`,
			"pi":                              `{"columns":[{"name":"_0","type":"DOUBLE"}],"rows":[[3.141592653589793]]}`,
			"function":                        `{"columns":[{"name":"_0","type":"STRING"}],"rows":[["HI"]]}`,
			"null":                            `{"columns":[{"name":"_0","type":"UNKNOWN"}],"rows":[[null]]}`,
			"group":                           `{"columns":[{"name":"A","type":"INTEGER"},{"name":"N","type":"BIGINT"}],"rows":[[1,1]]}`,
			"group_constant":                  `{"columns":[{"name":"A","type":"INTEGER"},{"name":"N","type":"BIGINT"}],"rows":[[1,1]]}`,
			"distinct_order":                  `{"columns":[{"name":"N","type":"INTEGER"}],"rows":[[1]]}`,
			"qualified_star":                  `ERROR 42703 "column \"T\" does not exist"`,
			"unknown":                         `ERROR 42703 "column \"MISSING\" does not exist"`,
			"hidden_bool":                     `ERROR 42601 "syntax error:\nSELECT _0\n       ^"`,
			"quoted_hidden_bool":              `ERROR 42703 "column \"_0\" does not exist"`,
			"cte_star_alias":                  `ERROR 42F10 "cte query has 0 column(s), however 1 aliases defined"`,
			"group_star":                      `ERROR 22023 "GROUP BY position 1 is out of range: SELECT list has 0 entries"`,
			"scalar":                          `{"columns":[{"name":"N","type":"INTEGER"}],"rows":[[4]]}`,
			"star_scalar":                     `{"columns":[{"name":"N","type":"INTEGER"}],"rows":[[4]]}`,
			"outer_scalar":                    `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"N","type":"BIGINT"}],"rows":[[1,2],[2,3]]}`,
			"outer_empty":                     `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"N","type":"BIGINT"}],"rows":[]}`,
			"zero_limit":                      `{"columns":[{"name":"_0","type":"INTEGER"}],"rows":[]}`,
			"offset":                          `{"columns":[{"name":"_0","type":"INTEGER"}],"rows":[]}`,
			"recursive_order":                 `{"columns":[{"name":"N","type":"INTEGER"}],"rows":[[1],[2],[3],[4],[5]]}`,
			"recursive_group":                 `{"columns":[{"name":"N","type":"INTEGER"},{"name":"CT","type":"BIGINT"}],"rows":[[3,1],[2,1],[1,1]]}`,
		}
		Expect(wantJava).To(HaveLen(len(probes)))
		completed := 0
		for _, p := range probes {
			jr := javaRunner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "FROMLESS-PROBE %s\n JAVA %s\n GO %s\n SQL %s\n", p.name, render(jr), render(gr), p.sql)
			Expect(render(jr)).To(Equal(wantJava[p.name]), p.sql)
			wantGo, distinct := wantGoDistinct[p.name]
			if !distinct {
				Expect(jr.Err).NotTo(HaveOccurred(), "Java-error cases require an independent Go contract: %s", p.sql)
				wantGo = wantJava[p.name]
			}
			Expect(render(gr)).To(Equal(wantGo), p.sql)
			completed++
		}
		Expect(completed).To(Equal(len(probes)))

		// The old no-aggregate DDL test queried SELECT 1 and mistook its
		// missing-FROM refusal for invalid DDL. IndexSpec.checkValidity admits
		// this grouping-only definition as a VALUE index, retaining duplicates.
		const indexSchema = "CREATE TABLE orders (id BIGINT, status STRING, PRIMARY KEY (id)) CREATE INDEX bad_idx AS SELECT status FROM orders GROUP BY status"
		indexSetup := []string{"INSERT INTO orders VALUES (1, 'pending'), (2, 'pending'), (3, 'done')"}
		for _, probe := range []struct{ sql, want string }{
			{"SELECT 1", `{"columns":[{"name":"_0","type":"INTEGER"}],"rows":[[1]]}`},
			{"SELECT status FROM orders WHERE status = 'pending'", `{"columns":[{"name":"STATUS","type":"STRING"}],"rows":[["pending"],["pending"]]}`},
		} {
			jr := javaRunner.RunWithSetup(ctx, indexSchema, indexSetup, probe.sql)
			gr := goRunner.RunWithSetup(ctx, indexSchema, indexSetup, probe.sql)
			fmt.Fprintf(GinkgoWriter, "FROMLESS-INDEX-PROBE %s\n JAVA %s\n GO %s\n", probe.sql, render(jr), render(gr))
			Expect(render(jr)).To(Equal(probe.want), probe.sql)
			Expect(render(gr)).To(Equal(probe.want), probe.sql)
		}
	})
})

var _ = Describe("GroupAliasIdentityJavaProbe", func() {
	It("keeps ephemeral aliases distinct from bound stars and qualified references", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "groupalias_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE wide_t (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE ordered_t (id BIGINT, v BIGINT, PRIMARY KEY (id, v)) CREATE TABLE pair_t (id BIGINT, PRIMARY KEY (id)) CREATE TYPE AS STRUCT nested_t (sk BIGINT, co BIGINT) CREATE TABLE ts (id BIGINT, n nested_t, PRIMARY KEY (id)) CREATE TABLE m (id BIGINT, a BIGINT, b BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE sort_t (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE po (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE pi (id BIGINT, po_id BIGINT, w BIGINT, PRIMARY KEY (id)) CREATE TABLE items_t (id BIGINT, items nested_t ARRAY, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO wide_t VALUES (2, 7)", "INSERT INTO ordered_t VALUES (2, 7)", "INSERT INTO pair_t VALUES (1), (2)", "INSERT INTO ts VALUES (1, (9, 0)), (2, (3, 0))", "INSERT INTO m VALUES (1, 1, 10, 10), (2, 1, 10, 25), (3, 2, 20, 10), (4, 3, 30, NULL)", "INSERT INTO sort_t VALUES (1, 9), (2, 3)", "INSERT INTO po VALUES (1, 10), (2, 20)", "INSERT INTO pi VALUES (5, 1, 100), (6, 1, 200), (7, 2, 300)"}
		const pair = `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[2,7]]}`
		const counted = `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"}],"rows":[[2,7,1]]}`
		const summed = `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"},{"name":"N","type":"BIGINT"}],"rows":[[2,7,2]]}`
		const alias = `{"columns":[{"name":"W.ID","type":"BIGINT"}],"rows":[[7]]}`
		const sumAlias = `{"columns":[{"name":"N","type":"BIGINT"}],"rows":[[7]]}`
		const orderedAliasPair = `{"columns":[{"name":"Z","type":"BIGINT"},{"name":"P.ID","type":"BIGINT"}],"rows":[[1,2],[1,1],[2,2],[2,1]]}`
		probes := []struct{ name, sql, java, goResult string }{
			// Java attaches trailing ordering to the right UNION leg and lacks
			// the in-memory sort fallback. Preserve the RFC-180 read extensions.
			{"union_quoted_lower", `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL SELECT id AS "x", v AS "X" FROM sort_t ORDER BY "x"`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[1,9],[1,9],[2,3],[2,3]]}`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[1,9],[1,9],[2,3],[2,3]]}`},
			{"union_quoted_upper", `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL SELECT id AS "x", v AS "X" FROM sort_t ORDER BY "X"`, "ERROR 0AF00", `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[2,3],[2,3],[1,9],[1,9]]}`},
			{"union_quoted_mixed", `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL SELECT id AS "x", v AS "X" FROM sort_t ORDER BY "x", "X"`, "ERROR 0AF00", `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[1,9],[1,9],[2,3],[2,3]]}`},
			{"union_quoted_lower_index", `SELECT id AS "x", v AS "X" FROM ordered_t UNION ALL SELECT id AS "x", v AS "X" FROM ordered_t ORDER BY "x"`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[2,7],[2,7]]}`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[2,7],[2,7]]}`},
			{"union_quoted_upper_index", `SELECT v AS "x", id AS "X" FROM ordered_t UNION ALL SELECT v AS "x", id AS "X" FROM ordered_t ORDER BY "X"`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[7,2],[7,2]]}`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[7,2],[7,2]]}`},
			{"union_quoted_mixed_index", `SELECT id AS "x", v AS "X" FROM ordered_t UNION ALL SELECT id AS "x", v AS "X" FROM ordered_t ORDER BY "x", "X"`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[2,7],[2,7]]}`, `{"columns":[{"name":"x","type":"BIGINT"},{"name":"X","type":"BIGINT"}],"rows":[[2,7],[2,7]]}`},
			{"qualified_whole_object_alias_precedence", `SELECT x.x, 99 AS x FROM items_t, items_t.items AS x ORDER BY x, x`, "ERROR 42702", "ERROR 42702"},
			{"union_joined_star_ambiguous", `SELECT * FROM wide_t p, wide_t q UNION ALL SELECT * FROM wide_t p, wide_t q ORDER BY id`, "ERROR 42702", "ERROR 42702"},
			{"union_joined_star_ambiguity_before_duplicate", `SELECT * FROM wide_t p, wide_t q UNION ALL SELECT * FROM wide_t p, wide_t q ORDER BY id, id`, "ERROR 42702", "ERROR 42702"},
			{"whole_object_alias_precedence", `SELECT x, 99 AS x FROM items_t, items_t.items AS x ORDER BY x, x`, "ERROR 42702", "ERROR 42702"},
			{"union_star_duplicate_name", `SELECT * FROM wide_t UNION ALL SELECT * FROM wide_t ORDER BY id, id`, "ERROR 42701", "ERROR 42701"},
			{"union_star_mixed_duplicate_name", `SELECT * FROM wide_t UNION ALL SELECT id, v FROM wide_t ORDER BY id, id`, "ERROR 42701", "ERROR 42701"},
			{"union_star_missing_name", `SELECT * FROM wide_t UNION ALL SELECT * FROM wide_t ORDER BY missing, missing`, "ERROR 42703", "ERROR 42703"},
			{"unnamed_inherited_alias", `SELECT "_0", "_1" AS "_0" FROM VALUES (9, 1), (3, 2) ORDER BY "_0"`, "ERROR 42702", "ERROR 42702"},
			{"unnamed_constant_alias", `SELECT "_0", 99 AS "_0" FROM VALUES (42) ORDER BY "_0"`, "ERROR 42702", "ERROR 42702"},
			// Java refuses these inline-source star/sort shapes with 0AF00; Go's
			// existing read-side extensions still enforce alias ownership.
			{"unnamed_star_alias", `SELECT *, 99 AS "_0" FROM VALUES (42) ORDER BY "_0"`, "ERROR 0AF00", "ERROR 42702"},
			{"named_inline_alias", `SELECT "_0", "_1" AS "_0" FROM VALUES (9, 1), (3, 2) AS v ("_0", "_1") ORDER BY "_0"`, "ERROR 0AF00", `{"columns":[{"name":"_0","type":"INTEGER"},{"name":"_0","type":"INTEGER"}],"rows":[[9,1],[3,2]]}`},
			{"alias_before_star", `SELECT s.id AS v, s.* FROM sort_t s ORDER BY v`, `{"columns":[{"name":"V","type":"BIGINT"},{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[1,1,9],[2,2,3]]}`, `{"columns":[{"name":"V","type":"BIGINT"},{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[1,1,9],[2,2,3]]}`},
			{"union_duplicate_name", `SELECT id FROM wide_t UNION ALL SELECT id FROM wide_t ORDER BY id, id`, "ERROR 42701", "ERROR 42701"},
			{"union_missing_before_duplicate", `SELECT id FROM wide_t UNION ALL SELECT id FROM wide_t ORDER BY missing, missing`, "ERROR 42703", "ERROR 42703"},
			{"golden_grouped_duplicate_name", `SELECT po.id, pi.id, COUNT(*) FROM po, pi WHERE pi.po_id = po.id GROUP BY po.id, pi.id ORDER BY id`, "ERROR 42702", "ERROR 42702"},
			{"aggregate_alias_group_name_ambiguous", `SELECT SUM(v) AS a, a FROM m GROUP BY a ORDER BY a`, "ERROR 42702", "ERROR 42702"},
			{"duplicate_group_output_name_ambiguous", `SELECT a, a, COUNT(*) FROM m GROUP BY a ORDER BY a`, "ERROR 42702", "ERROR 42702"},
			{"plain_inherited_not_alias", `SELECT v, id AS v FROM wide_t ORDER BY v`, `{"columns":[{"name":"V","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[7,2]]}`, `{"columns":[{"name":"V","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[7,2]]}`},
			{"grouped_inherited_is_alias", `SELECT v, id AS v FROM wide_t GROUP BY id, v ORDER BY v`, "ERROR 42702", "ERROR 42702"},
			{"nested_inherited_not_alias", `SELECT ts.n.sk, ts.id AS sk FROM ts ORDER BY sk`, `{"columns":[{"name":"SK","type":"BIGINT"},{"name":"SK","type":"BIGINT"}],"rows":[[9,1],[3,2]]}`, `{"columns":[{"name":"SK","type":"BIGINT"},{"name":"SK","type":"BIGINT"}],"rows":[[9,1],[3,2]]}`},
			{"ambiguity_before_duplicate_plain", `SELECT id AS z, v AS z FROM wide_t ORDER BY z, z`, "ERROR 42702", "ERROR 42702"},
			{"ambiguity_before_duplicate_grouped", `SELECT id AS z, v AS z FROM wide_t GROUP BY id, v ORDER BY z, z`, "ERROR 42702", "ERROR 42702"},
			{"ambiguous_absent_grouped", `SELECT id AS z, v AS z FROM wide_t GROUP BY id, v ORDER BY z`, "ERROR 42702", "ERROR 42702"},
			{"ambiguous_source_grouped", `SELECT id AS id, v AS id FROM wide_t GROUP BY id, v ORDER BY id`, "ERROR 42702", "ERROR 42702"},
			{"ambiguous_absent_plain", `SELECT id AS z, v AS z FROM wide_t ORDER BY z`, "ERROR 42702", "ERROR 42702"},
			{"ambiguous_source_plain", `SELECT id AS id, v AS id FROM wide_t ORDER BY id`, "ERROR 42702", "ERROR 42702"},
			{"duplicate_outputs_unordered", `SELECT id AS z, v AS z FROM ordered_t GROUP BY id, v`, `{"columns":[{"name":"Z","type":"BIGINT"},{"name":"Z","type":"BIGINT"}],"rows":[[2,7]]}`, `{"columns":[{"name":"Z","type":"BIGINT"},{"name":"Z","type":"BIGINT"}],"rows":[[2,7]]}`},
			{"duplicate_outputs_qualified", `SELECT id AS z, v AS z FROM ordered_t GROUP BY id, v ORDER BY ordered_t.id`, `{"columns":[{"name":"Z","type":"BIGINT"},{"name":"Z","type":"BIGINT"}],"rows":[[2,7]]}`, `{"columns":[{"name":"Z","type":"BIGINT"},{"name":"Z","type":"BIGINT"}],"rows":[[2,7]]}`},
			{"select_alias_owner", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY z, q.id DESC`, "ERROR 0AF00", orderedAliasPair},
			{"select_alias_owner_no_group_alias", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, "ERROR 0AF00", orderedAliasPair},
			{"select_before_group_alias", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id AS z ORDER BY z, q.id DESC`, "ERROR 0AF00", orderedAliasPair},
			// Java removes ephemeral GROUP aliases from the scope before ORDER BY
			// (QueryVisitor.visitSimpleTable); Go's existing grouped-sort extension
			// retains them. Pin the Java refusal and Go's independent exact rows.
			{"group_alias_rebased_collision", `SELECT p.id AS "Q.ID", q.id AS v FROM pair_t p, pair_t q GROUP BY p.id, q.id AS g ORDER BY g, p.id DESC`, "ERROR 42703", `{"columns":[{"name":"Q.ID","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[2,1],[1,1],[2,2],[1,2]]}`},
			{"select_alias_ambiguous_source", `SELECT p.id AS id, q.id AS v FROM pair_t p, pair_t q GROUP BY p.id, q.id AS id ORDER BY id, q.id DESC`, "ERROR 0AF00", `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"V","type":"BIGINT"}],"rows":[[1,2],[1,1],[2,2],[2,1]]}`},
			{"select_alias_owner_plain", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q ORDER BY z, q.id DESC`, "ERROR 0AF00", orderedAliasPair},
			{"invalid_computed_alias", `SELECT p.id+0 AS z, q.id AS "(P.ID + 0)", q.id AS "P.ID + 0" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, "ERROR 42602", "ERROR 42602"},
			{"invalid_aggregate_alias", `SELECT SUM(p.id) AS z, SUM(q.id) AS "SUM(P.ID)" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, "ERROR 42602", "ERROR 42602"},
			{"bound_star", `SELECT * FROM wide_t x GROUP BY 2 AS "X.ID", 1`, "ERROR 42803", pair},
			{"bound_aggregate", `SELECT w.*, COUNT(*) AS n FROM wide_t w GROUP BY 1, 2 AS "W.ID"`, "ERROR 42803", counted},
			{"ungrouped_star", `SELECT * FROM wide_t x GROUP BY 2 AS "X.ID"`, "ERROR 42803", "ERROR 42803"},
			{"ungrouped_aggregate", `SELECT w.*, COUNT(*) AS n FROM wide_t w GROUP BY 2 AS "W.ID"`, "ERROR 42803", "ERROR 42803"},
			{"named_star", `SELECT w.* FROM ordered_t w GROUP BY w.id, w.v AS "W.ID"`, pair, pair},
			{"named_aggregate", `SELECT w.*, COUNT(*) AS n FROM ordered_t w GROUP BY w.id, w.v AS "W.ID"`, counted, counted},
			{"unqualified_alias_name", `SELECT w.id, w.v FROM ordered_t w GROUP BY w.id, w.v AS id`, pair, pair},
			{"qualified_column", `SELECT w.id, w.v FROM ordered_t w GROUP BY w.id, w.v AS "W.ID"`, pair, pair},
			{"qualified_argument", `SELECT w.id, w.v, SUM(w.id) AS n FROM ordered_t w GROUP BY w.id, w.v AS "W.ID"`, summed, summed},
			{"quoted_order", `SELECT w.id, w.v AS "W.ID" FROM ordered_t w GROUP BY w.id, w.v ORDER BY w.id, "W.ID"`, `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"W.ID","type":"BIGINT"}],"rows":[[2,7]]}`, `{"columns":[{"name":"ID","type":"BIGINT"},{"name":"W.ID","type":"BIGINT"}],"rows":[[2,7]]}`},
			{"repeat_quoted_order", `SELECT w.id, w.v AS "W.ID" FROM ordered_t w GROUP BY w.id, w.v ORDER BY "W.ID", "W.ID"`, "ERROR 42701", "ERROR 42701"},
			{"repeat_qualified_order", `SELECT w.id, w.v FROM ordered_t w GROUP BY w.id, w.v ORDER BY w.id, w.id`, "ERROR 42701", "ERROR 42701"},
			{"quoted_read", `SELECT "W.ID" FROM ordered_t w WHERE w.id = 2 GROUP BY w.v AS "W.ID"`, alias, alias},
			{"quoted_argument", `SELECT SUM("W.ID") AS n FROM ordered_t w WHERE w.id = 2 GROUP BY w.v AS "W.ID"`, sumAlias, sumAlias},
		}
		render := func(result plandiff.RunResult) string {
			if result.Err != nil {
				var javaError *plandiff.JavaError
				var goError *api.Error
				if errors.As(result.Err, &javaError) {
					return "ERROR " + javaError.SQLState
				}
				if errors.As(result.Err, &goError) {
					return "ERROR " + string(goError.Code)
				}
				return fmt.Sprintf("ERROR %v", result.Err)
			}
			// Columns and rows only: these pins predate RowSet.Nullability, which the
			// WS-E oracle measures and pins on its own shapes.
			encoded, err := json.Marshal(struct {
				Columns []plandiff.Column `json:"columns"`
				Rows    [][]any           `json:"rows"`
			}{result.Rows.Columns, result.Rows.Rows})
			Expect(err).NotTo(HaveOccurred())
			return string(encoded)
		}
		// The target attaches a trailing ORDER BY to the right UNION leg, so it plans the
		// union itself unordered (measured: the right leg's primary scan already delivers id
		// order, and no ordering operator is left), and its UnorderedUnionCursor returns rows
		// as the legs deliver them: two runs of one plan may interleave the legs differently
		// (UnorderedUnionCursor.java, class comment), which this probe once did. Such a probe
		// pins the target's plan and its rows as a multiset; Go's answer is compared exactly.
		javaUnorderedUnion := map[string]string{
			"union_quoted_lower": "SCAN([IS SORT_T]) | MAP (_.ID AS x, _.V AS X) ⊎ SCAN([IS SORT_T]) | MAP (_.ID AS x, _.V AS X)",
		}
		sortedRows := func(result plandiff.RunResult) plandiff.RunResult {
			rows := append([][]any(nil), result.Rows.Rows...)
			sort.SliceStable(rows, func(i, j int) bool { return fmt.Sprint(rows[i]) < fmt.Sprint(rows[j]) })
			result.Rows.Rows = rows
			return result
		}
		var mismatches []string
		for _, p := range probes {
			javaRun := javaRunner.RunWithSetup(ctx, schema, setup, p.sql)
			goRun := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			java := render(javaRun)
			goResult := render(goRun)
			if wantPlan, unordered := javaUnorderedUnion[p.name]; unordered {
				explain := javaRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+p.sql)
				Expect(explain.Err).NotTo(HaveOccurred(), "the target's plan of %s", p.sql)
				Expect(explain.Rows.Rows).NotTo(BeEmpty(), "the target's plan of %s", p.sql)
				plan := fmt.Sprint(explain.Rows.Rows[0][0])
				fmt.Fprintf(GinkgoWriter, "GROUP-ALIAS-PROBE %s JAVA-EXPLAIN %s\n", p.name, plan)
				Expect(plan).To(Equal(wantPlan), "the target's plan of %s", p.sql)
				java = render(sortedRows(javaRun))
			}
			if p.name == "golden_grouped_duplicate_name" {
				var javaError *plandiff.JavaError
				var goError *api.Error
				Expect(errors.As(javaRun.Err, &javaError)).To(BeTrue())
				Expect(errors.As(goRun.Err, &goError)).To(BeTrue())
				Expect(javaError.Message).To(Equal("Ambiguous alias ID"))
				Expect(goError.Message).To(Equal("Ambiguous alias ID"))
				fmt.Fprintf(GinkgoWriter, "GOLDEN-DIAGNOSTIC Java=%s Go=%s\n", javaError.Message, goError.Message)
			}
			fmt.Fprintf(GinkgoWriter, "GROUP-ALIAS-PROBE %s\n JAVA %s\n GO %s\n SQL %s\n", p.name, java, goResult, p.sql)
			if java != p.java || goResult != p.goResult {
				mismatches = append(mismatches, fmt.Sprintf("%s: Java=%s (want %s), Go=%s (want %s)", p.sql, java, p.java, goResult, p.goResult))
			}
		}
		Expect(mismatches).To(BeEmpty())
	})
})
