//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/metadata"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Live-JVM oracle for the RFC-257 WS-E shared SQL semantics. Every probe runs
// the same statement on the target 4.14.2.0 engine and on Go. The JAVA line of
// every probe is pinned verbatim: it is the measured target contract the WS-E
// design ports. The GO line is recorded, not pinned, because the point of the
// oracle is to measure the gap the port closes; each Go expectation lands with
// the port's own tests. Every Describe runs through the helpers below, so all of
// them render, bind and invoke the same way.

// wseParam is one bound parameter: the JDBC setter the Java side calls (kind, and
// the java.sql.Types name for setNull), the value, and a name for a named
// parameter (empty for a positional `?`).
type wseParam struct {
	name, kind, sqlType string
	value               any
}

func wseLong(v int) wseParam       { return wseParam{kind: "long", value: v} }
func wseInt(v int) wseParam        { return wseParam{kind: "int", value: v} }
func wseDouble(v float64) wseParam { return wseParam{kind: "double", value: v} }
func wseFloat(v float64) wseParam  { return wseParam{kind: "float", value: v} }
func wseString(v string) wseParam  { return wseParam{kind: "string", value: v} }
func wseBool(v bool) wseParam      { return wseParam{kind: "boolean", value: v} }
func wseUUID(v string) wseParam    { return wseParam{kind: "uuid", value: v} }
func wseLongArray(v ...int) wseParam {
	return wseParam{kind: "longArray", value: append([]int{}, v...)}
}
func wseNull(sqlType string) wseParam           { return wseParam{kind: "null", sqlType: sqlType} }
func wseObjectNull() wseParam                   { return wseParam{kind: "objectNull"} }
func wseNamed(name string, p wseParam) wseParam { p.name = name; return p }

// wseGoArg is the database/sql argument the Go side binds for p. An `int` kind is
// an int32 and a `long` kind an int64, so both of the target's integer lanes are
// asked; every NULL is a Go nil, because database/sql has no typed NULL.
func wseGoArg(p wseParam) any {
	var v any
	switch p.kind {
	case "null", "objectNull":
		v = nil
	case "long":
		v = int64(p.value.(int))
	case "int":
		v = int32(p.value.(int))
	case "double":
		v = p.value.(float64)
	case "float":
		v = float32(p.value.(float64))
	case "uuid":
		v = uuid.MustParse(p.value.(string))
	case "longArray":
		out := []int64{}
		for _, x := range p.value.([]int) {
			out = append(out, int64(x))
		}
		v = out
	default:
		v = p.value
	}
	if p.name != "" {
		return sql.Named(p.name, v)
	}
	return v
}

// wseCase is one statement run through the Java step runPreparedExtended and the
// Go prepared runners. A DML case (update) runs followUp in the same schema
// afterwards and reports its rows with the statement's update count; connOpts are
// options set on the CONNECTION before the statement (Java only: the Go runner has
// no connection options).
type wseCase struct {
	name, sql string
	params    []wseParam
	connOpts  map[string]any
	update    bool
	followUp  string
}

func wseQuery(name, sqlText string, params ...wseParam) wseCase {
	return wseCase{name: name, sql: sqlText, params: params}
}

func wseDML(name, sqlText, followUp string, params ...wseParam) wseCase {
	return wseCase{name: name, sql: sqlText, params: params, update: true, followUp: followUp}
}

func (c wseCase) on(opts map[string]any) wseCase { c.connOpts = opts; return c }

// wseRender renders one engine's outcome. An error is its SQLSTATE, class and
// message (Go's api.Error has no class). An EXPLAIN or DESCRIBE of a query is its
// first cell only, the textual plan: the rest carries random names and timings.
// Rows are the result types, the JDBC nullability of each column, the values
// rendered exactly (integers with all their digits, NULL as NULL) and, for DML,
// the update count the statement reported before its follow-up query.
func wseRender(sqlText string, r plandiff.RunResult) string {
	if r.Err != nil {
		var je *plandiff.JavaError
		if errors.As(r.Err, &je) {
			return fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, je.Message)
		}
		var ge *api.Error
		if errors.As(r.Err, &ge) {
			return fmt.Sprintf("ERROR %s %q", string(ge.Code), ge.Message)
		}
		return fmt.Sprintf("ERROR %v", r.Err)
	}
	upper := strings.ToUpper(sqlText)
	explained := strings.HasPrefix(upper, "EXPLAIN ") || strings.HasPrefix(upper, "DESCRIBE SELECT") || strings.HasPrefix(upper, "DESC SELECT")
	if explained && len(r.Rows.Rows) > 0 && len(r.Rows.Rows[0]) > 0 {
		return fmt.Sprintf("OK EXPLAIN %q", wsjValue(r.Rows.Rows[0][0]))
	}
	var types []string
	for _, c := range r.Rows.Columns {
		types = append(types, c.Type)
	}
	out := fmt.Sprintf("OK %v %v %s", types, r.Rows.Nullability, wsjRows(r.Rows.Rows))
	if r.Rows.UpdateCount != nil {
		out += fmt.Sprintf(" COUNT %d", *r.Rows.UpdateCount)
	}
	return out
}

// wseOracle runs probes on both engines over one tenant and one JVM, and records
// the JAVA line of each under its name.
type wseOracle struct {
	ctx         context.Context
	srv         *JavaInvoker
	clusterFile string
	java        plandiff.SetupRunner
	goRunner    plandiff.SetupRunner
	tag         string
	got         map[string]string
}

// newWSEOracle opens the tenant, the JVM and both runners; the returned function
// releases them.
func newWSEOracle(tenantPrefix, tag string) (*wseOracle, func()) {
	ctx := context.Background()
	env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantPrefix+uuid.New().String())
	Expect(err).NotTo(HaveOccurred())
	srv, err := NewIsolatedJavaInvoker()
	Expect(err).NotTo(HaveOccurred())
	clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
	o := &wseOracle{
		ctx:         ctx,
		srv:         srv,
		clusterFile: env.ClusterFile,
		java:        plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner),
		goRunner:    plandiff.NewGoSQLSetupRunner(clusterFilePath),
		tag:         tag,
		got:         map[string]string{},
	}
	return o, func() {
		_ = os.Remove(clusterFilePath)
		_ = srv.Close()
		_ = env.Cleanup(ctx)
	}
}

// plain runs one statement as SQL text on both engines.
func (o *wseOracle) plain(schema string, setup []string, name, sqlText string) {
	jr := o.java.RunWithSetup(o.ctx, schema, setup, sqlText)
	gr := o.goRunner.RunWithSetup(o.ctx, schema, setup, sqlText)
	o.record(name, sqlText, wseRender(sqlText, jr), wseRender(sqlText, gr))
}

// prepared runs one case with its parameters bound through each engine's driver.
func (o *wseOracle) prepared(schema string, setup []string, c wseCase) {
	// A nil setup would reach the Java step as JSON null, which it does not accept.
	setup = append([]string{}, setup...)
	javaParams := make([]map[string]any, len(c.params))
	goArgs := make([]any, len(c.params))
	for i, p := range c.params {
		javaParams[i] = map[string]any{"name": p.name, "kind": p.kind, "sqlType": p.sqlType, "value": p.value}
		goArgs[i] = wseGoArg(p)
	}
	connOpts := c.connOpts
	if connOpts == nil {
		connOpts = map[string]any{}
	}
	jr := plandiff.RunResult{Engine: "java"}
	var rows plandiff.RowSet
	if err := o.srv.InvokeAs(o.ctx, "runPreparedExtended", map[string]any{
		"clusterFile": o.clusterFile, "schemaTemplate": schema, "setupSqls": setup,
		"querySql": c.sql, "params": javaParams, "connectionOptions": connOpts,
		"update": c.update, "followUpSql": c.followUp,
	}, &rows); err != nil {
		var je *JavaError
		if errors.As(err, &je) {
			jr.Err = &plandiff.JavaError{
				Message: je.Message, ExceptionClass: je.ExceptionClass,
				ExceptionFullClass: je.ExceptionFullClass, SQLState: je.SQLState,
			}
		} else {
			jr.Err = err
		}
	} else {
		jr.Rows = rows
	}
	goLine := "NO-GO-CONNECTION-OPTION"
	if len(c.connOpts) == 0 {
		var gr plandiff.RunResult
		if c.update {
			gr = o.goRunner.(plandiff.PreparedDMLRunner).RunPreparedDMLWithSetup(o.ctx, schema, setup, c.sql, goArgs, c.followUp)
		} else {
			gr = o.goRunner.(plandiff.PreparedSetupRunner).RunPreparedWithSetup(o.ctx, schema, setup, c.sql, goArgs)
		}
		goLine = wseRender(c.sql, gr)
	}
	o.record(c.name, c.sql, wseRender(c.sql, jr), goLine)
}

// readScope runs the Java step snapshotReadScopeProbe: a read in an explicit
// transaction, a concurrent committed write on a second connection, the reader's own
// write and its commit. The line is the read's rows and the commit outcome. Go has no
// snapshot option yet (section 6 ports it), so the Go line is recorded as absent.
func (o *wseOracle) readScope(schema string, setup []string, name, readSQL, concurrentSQL, ownWriteSQL string) {
	var out struct {
		Read   plandiff.RowSet `json:"read"`
		Commit string          `json:"commit"`
	}
	javaLine := ""
	if err := o.srv.InvokeAs(o.ctx, "snapshotReadScopeProbe", map[string]any{
		"clusterFile": o.clusterFile, "schemaTemplate": schema, "setupSqls": setup,
		"readSql": readSQL, "concurrentSql": concurrentSQL, "ownWriteSql": ownWriteSQL,
	}, &out); err != nil {
		var je *JavaError
		if errors.As(err, &je) {
			javaLine = fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, je.Message)
		} else {
			javaLine = fmt.Sprintf("ERROR %v", err)
		}
	} else {
		javaLine = "READ " + wseRender(readSQL, plandiff.RunResult{Rows: out.Read}) + " COMMIT " + out.Commit
	}
	o.record(name, readSQL+" | "+concurrentSQL, javaLine, "NO-GO-SNAPSHOT (section 6 ports the option)")
}

// indexStateScope runs the Java step indexStateReadScopeProbe: a read in an explicit
// transaction, a concurrent committed change of one index's state key, the reader's own
// write to an untouched table and its commit. The Go line is recorded as absent: Go's
// index-state conflicts are pinned by its own FDB tests once section 6.4 lands.
func (o *wseOracle) indexStateScope(schema string, setup []string, name, readSQL, indexName, state, ownWriteSQL string) {
	var out struct {
		Read   plandiff.RowSet `json:"read"`
		Commit string          `json:"commit"`
	}
	javaLine := ""
	if err := o.srv.InvokeAs(o.ctx, "indexStateReadScopeProbe", map[string]any{
		"clusterFile": o.clusterFile, "schemaTemplate": schema, "setupSqls": setup,
		"readSql": readSQL, "indexName": indexName, "state": state, "ownWriteSql": ownWriteSQL,
	}, &out); err != nil {
		var je *JavaError
		if errors.As(err, &je) {
			javaLine = fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, je.Message)
		} else {
			javaLine = fmt.Sprintf("ERROR %v", err)
		}
	} else {
		javaLine = "READ " + wseRender(readSQL, plandiff.RunResult{Rows: out.Read}) + " COMMIT " + out.Commit
	}
	o.record(name, readSQL+" | "+indexName+" -> "+state, javaLine, "NO-GO (section 6.4, Go FDB tests)")
}

// wseAliasRE matches the target's per-planning quantifier aliases (q + a UUID with
// underscores), which appear in plan descriptions.
var wseAliasRE = regexp.MustCompile(`q[0-9a-f]{8}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{12}`)

// trace runs the Java step planRuleTrace: the target's EXPLAIN of querySql with, for each
// named rule, how many calls ended and how many final and exploratory expressions they
// yielded. It tells an alternative the planner never produced from one it produced and
// did not choose.
func (o *wseOracle) trace(schema string, setup []string, name, querySQL string, rules []string) {
	var tr struct {
		Explain string `json:"explain"`
		Rules   map[string]struct {
			Calls       int `json:"calls"`
			Finals      int `json:"finals"`
			Exploratory int `json:"exploratory"`
		} `json:"rules"`
		Comparisons []string `json:"inUnionComparisons"`
	}
	javaLine := ""
	if err := o.srv.InvokeAs(o.ctx, "planRuleTrace", map[string]any{
		"clusterFile": o.clusterFile, "schemaTemplate": schema, "setupSqls": setup,
		"querySql": querySQL, "rules": rules,
	}, &tr); err != nil {
		var je *JavaError
		if errors.As(err, &je) {
			javaLine = fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, je.Message)
		} else {
			javaLine = fmt.Sprintf("ERROR %v", err)
		}
	} else {
		parts := []string{fmt.Sprintf("EXPLAIN %q", tr.Explain)}
		for _, r := range rules {
			t := tr.Rules[r]
			parts = append(parts, fmt.Sprintf("%s calls=%d finals=%d exploratory=%d", r, t.Calls, t.Finals, t.Exploratory))
		}
		// The target's REWRITING prune of every group a simplification left with more than
		// one final member (each member's RewritingCostModel criteria, its semantic hash and
		// the model's pairwise verdict), and, for an IN-union question, the ranking of the
		// in-union plan by PlanningCostModel (quantifier aliases masked). A REWRITING
		// reference never holds physical plans, so no plan ranking is reported for it.
		var cmps []string
		for _, c := range tr.Comparisons {
			cmps = append(cmps, wseAliasRE.ReplaceAllString(c, "q#"))
		}
		if len(cmps) > 0 {
			parts = append(parts, "ranking: "+strings.Join(cmps, " | "))
		}
		javaLine = "TRACE " + strings.Join(parts, "; ")
	}
	o.record(name, querySQL, javaLine, "NO-GO-INSTRUMENT")
}

func (o *wseOracle) record(name, sqlText, javaLine, goLine string) {
	Expect(o.got).NotTo(HaveKey(name), "probe names are unique")
	o.got[name] = javaLine
	fmt.Fprintf(GinkgoWriter, "%s %s\n  JAVA %s\n  GO   %s\n  SQL  %s\n", o.tag, name, javaLine, goLine, sqlText)
}

// check prints every JAVA line in pin form, then requires the probe set to equal
// the pin set and every probe to equal its pin. A change is a change in the
// target, not noise: revisit ws-e-design.md before updating a row.
func (o *wseOracle) check(want map[string]string) {
	for _, name := range sortedStringKeys(o.got) {
		fmt.Fprintf(GinkgoWriter, "%s-JAVA %q: %q,\n", o.tag, name, o.got[name])
	}
	Expect(sortedStringKeys(o.got)).To(Equal(sortedStringKeys(want)), "every probe is pinned and every pin is probed")
	var mismatches []string
	for _, name := range sortedStringKeys(o.got) {
		if want[name] != o.got[name] {
			mismatches = append(mismatches, fmt.Sprintf("%s: want %s got %s", name, want[name], o.got[name]))
		}
	}
	Expect(strings.Join(mismatches, "\n")).To(BeEmpty())
}

var _ = Describe("WS-E target oracle", func() {
	It("records LIKE, comment, literal, variadic, IN-NULL and statement-option outcomes", func() {
		o, done := newWSEOracle("ws_e_", "WS-E")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a\nb', 5), (3, '\U0001D11Ex', 7), (4, 'a%b', 1), (5, 'a_b', 2), (6, 'ab\n', 3), (7, 'a\\b', 4)",
		}
		probes := []struct{ name, sql string }{
			// LIKE: wildcards cross line terminators; no trailing-newline tolerance;
			// '_' consumes one code point (a surrogate pair); escapes.
			{"like_percent_crosses_newline", "SELECT id FROM T WHERE s LIKE 'a%b' ORDER BY id"},
			{"like_underscore_matches_newline", "SELECT id FROM T WHERE s LIKE 'a_b' ORDER BY id"},
			{"like_no_trailing_newline_tolerance", "SELECT id FROM T WHERE s LIKE 'ab' ORDER BY id"},
			{"like_underscore_surrogate_pair", "SELECT id FROM T WHERE s LIKE '_x' ORDER BY id"},
			{"like_escape_percent", "SELECT id FROM T WHERE s LIKE 'a\\%b' ESCAPE '\\' ORDER BY id"},
			{"like_escape_underscore", "SELECT id FROM T WHERE s LIKE 'a\\_b' ESCAPE '\\' ORDER BY id"},
			{"like_escaped_escape", "SELECT id FROM T WHERE s LIKE 'a\\\\b' ESCAPE '\\' ORDER BY id"},
			{"like_escape_is_percent", "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '%' ORDER BY id"},
			{"like_escape_is_underscore", "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '_' ORDER BY id"},
			{"like_escape_two_chars", "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE 'ab' ORDER BY id"},
			{"like_escape_empty", "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '' ORDER BY id"},
			{"like_escape_supplementary", "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '\U0001D11E' ORDER BY id"},
			{"like_dangling_escape", "SELECT id FROM T WHERE s LIKE 'a\\' ESCAPE '\\' ORDER BY id"},
			{"like_escape_before_ordinary", "SELECT id FROM T WHERE s LIKE 'a\\b' ESCAPE '\\' ORDER BY id"},
			{"like_null_pattern_bad_escape", "SELECT id FROM T WHERE s LIKE NULL ESCAPE 'ab' ORDER BY id"},
			{"like_null_pattern", "SELECT id FROM T WHERE s LIKE NULL ORDER BY id"},
			{"like_not", "SELECT id FROM T WHERE s NOT LIKE 'a%' ORDER BY id"},
			{"like_projection", "SELECT id, s LIKE 'a%' FROM T ORDER BY id"},
			{"like_numeric_operand", "SELECT id FROM T WHERE n LIKE '1' ORDER BY id"},
			{"like_numeric_pattern", "SELECT id FROM T WHERE s LIKE 1 ORDER BY id"},
			{"like_empty_pattern", "SELECT id FROM T WHERE s LIKE '' ORDER BY id"},
			{"like_adjacent_literal_pattern", "SELECT id FROM T WHERE s LIKE 'a' '%' ORDER BY id"},
			// Comments.
			{"comment_dash_no_space", "SELECT id FROM T WHERE id = 1--1\n ORDER BY id"},
			{"comment_nested_block", "SELECT id FROM T /* a /* b */ c */ WHERE id = 1"},
			{"comment_unterminated_block", "SELECT id FROM T WHERE id = 1 /* open"},
			{"comment_unterminated_nested", "SELECT id FROM T WHERE id = 1 /* a /* b */"},
			{"comment_hash_not_comment", "SELECT id FROM T WHERE id = 1 # no"},
			{"comment_cr_terminates", "SELECT id FROM T -- c\rWHERE id = 1"},
			{"comment_crlf_terminates", "SELECT id FROM T -- c\r\nWHERE id = 1"},
			{"comment_mysql_executable_form", "SELECT id FROM T /*! WHERE id = 2 */ WHERE id = 1"},
			{"comment_marker_in_string", "SELECT id FROM T WHERE s = '--x' OR id = 1"},
			// Adjacent string literals decode per token, then concatenate.
			{"literal_adjacent", "SELECT 'a' 'b' FROM T WHERE id = 1"},
			{"literal_doubled_quote", "SELECT 'a''b' FROM T WHERE id = 1"},
			{"literal_adjacent_with_doubled", "SELECT 'a''' 'b' FROM T WHERE id = 1"},
			{"literal_adjacent_predicate", "SELECT id FROM T WHERE s = 'a' 'bc'"},
			// Variadic nullability, arity, all-NULL and evaluation order.
			{"coalesce_all_null", "SELECT COALESCE(NULL, NULL) FROM T WHERE id = 1"},
			{"greatest_all_null", "SELECT GREATEST(NULL, NULL) FROM T WHERE id = 1"},
			{"greatest_one_null", "SELECT GREATEST(1, NULL) FROM T WHERE id = 1"},
			{"least_nullable_column", "SELECT id, LEAST(n, 3) FROM T ORDER BY id"},
			{"coalesce_nullable_column", "SELECT id, COALESCE(n, 0) FROM T ORDER BY id"},
			{"coalesce_single_argument", "SELECT COALESCE(1) FROM T WHERE id = 1"},
			{"greatest_single_argument", "SELECT GREATEST(1) FROM T WHERE id = 1"},
			{"coalesce_evaluates_every_argument", "SELECT COALESCE(1, 1 / 0) FROM T WHERE id = 1"},
			{"greatest_mixed_numeric", "SELECT GREATEST(1, 2.5) FROM T WHERE id = 1"},
			// IN lists and array elements.
			{"in_bare_null", "SELECT id FROM T WHERE id IN (NULL)"},
			{"in_bare_null_beside_literal", "SELECT id FROM T WHERE id IN (1, NULL)"},
			{"in_wrapped_null", "SELECT id FROM T WHERE id IN ((NULL))"},
			{"not_in_bare_null", "SELECT id FROM T WHERE id NOT IN (NULL)"},
			{"in_typed_null", "SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT))"},
			{"in_column_value_null", "SELECT id FROM T WHERE 1 IN (n) ORDER BY id"},
			{"array_bare_null_element", "SELECT [1, NULL] FROM T WHERE id = 1"},
			{"array_typed_null_element", "SELECT [CAST(NULL AS BIGINT)] FROM T WHERE id = 1"},
			{"array_nullable_column_element", "SELECT id, [n] FROM T ORDER BY id"},
			// Statement options.
			{"options_snapshot_select", "SELECT id FROM T WHERE id = 1 OPTIONS (ISOLATION LEVEL SNAPSHOT)"},
			{"options_after_order_by", "SELECT id FROM T ORDER BY id OPTIONS (NOCACHE)"},
			{"options_inside_subquery", "SELECT id FROM (SELECT id FROM T OPTIONS (NOCACHE)) AS X WHERE id = 1"},
			{"options_ef_search_statement", "SELECT id FROM T WHERE id = 1 OPTIONS (EF_SEARCH 10)"},
			{"options_log_query", "SELECT id FROM T WHERE id = 1 OPTIONS (LOG QUERY)"},
		}
		for _, p := range probes {
			o.plain(schema, setup, p.name, p.sql)
		}
		// LIKE over an ENUM operand needs its own schema. The target has ENUM, and its
		// typing admits only NULL or STRING operands.
		o.plain("CREATE TYPE AS ENUM color ('RED', 'GREEN') CREATE TABLE E (id BIGINT, c color, PRIMARY KEY (id))",
			[]string{"INSERT INTO E VALUES (1, 'RED'), (2, 'GREEN')"},
			"like_enum_operand", "SELECT id FROM E WHERE c LIKE 'R%' ORDER BY id")
		// Prepared statements: parameters bound through the driver, not written into
		// the SQL. Java binds with the JDBC setters named by kind.
		for _, c := range []wseCase{
			wseQuery("prepared_in_typed_null", "SELECT id FROM T WHERE id IN (?)", wseNull("BIGINT")),
			wseQuery("prepared_in_literal_and_typed_null", "SELECT id FROM T WHERE id IN (1, ?)", wseNull("BIGINT")),
			wseQuery("prepared_in_object_null", "SELECT id FROM T WHERE id IN (?)", wseObjectNull()),
			wseQuery("prepared_in_long", "SELECT id FROM T WHERE id IN (?) ORDER BY id", wseLong(1)),
			wseQuery("prepared_eq_typed_null", "SELECT id FROM T WHERE n = ? ORDER BY id", wseNull("BIGINT")),
			wseQuery("prepared_is_null_param", "SELECT id FROM T WHERE ? IS NULL ORDER BY id", wseNull("BIGINT")),
			wseQuery("prepared_coalesce_typed_null", "SELECT COALESCE(?, 0) FROM T WHERE id = 1", wseNull("BIGINT")),
			wseQuery("prepared_array_typed_null_element", "SELECT [?] FROM T WHERE id = 1", wseNull("BIGINT")),
			wseQuery("prepared_select_long", "SELECT ? FROM T WHERE id = 1", wseLong(5)),
			wseQuery("prepared_int_overflow", "SELECT ? + 2147483647 FROM T WHERE id = 1", wseInt(1)),
			wseQuery("prepared_long_no_overflow", "SELECT ? + 2147483647 FROM T WHERE id = 1", wseLong(1)),
			wseQuery("prepared_order_by_param", "SELECT id FROM T ORDER BY ?", wseInt(1)),
			wseQuery("prepared_limit_param", "SELECT id FROM T ORDER BY id LIMIT ?", wseInt(2)),
			wseQuery("prepared_like_param_pattern", "SELECT id FROM T WHERE s LIKE ? ORDER BY id", wseString("a%")),
			wseQuery("prepared_string_eq", "SELECT id FROM T WHERE s = ? ORDER BY id", wseString("abc")),
		} {
			o.prepared(schema, setup, c)
		}
		// Measured target outcomes.
		o.check(map[string]string{
			"array_bare_null_element":            "ERROR 0A000 RelationalException \"An ARRAY value cannot have NULL elements\"",
			"array_nullable_column_element":      "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"array_typed_null_element":           "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"coalesce_all_null":                  "ERROR 22F00 SemanticException \"The function is not defined for the given argument types\"",
			"coalesce_evaluates_every_argument":  "ERROR XXXXX ArithmeticException \"/ by zero\"",
			"coalesce_nullable_column":           "OK [BIGINT BIGINT] [NULL NOT NULL] [[1 0] [2 5] [3 7] [4 1] [5 2] [6 3] [7 4]]",
			"coalesce_single_argument":           "ERROR XX000 VerifyException \"com.google.common.base.VerifyException\"",
			"comment_cr_terminates":              "OK [BIGINT] [NULL] [[1]]",
			"comment_crlf_terminates":            "OK [BIGINT] [NULL] [[1]]",
			"comment_dash_no_space":              "OK [BIGINT] [NULL] [[1]]",
			"comment_hash_not_comment":           "ERROR 42601 RelationalException \"syntax error:\\nline 1:30 token recognition error at: '# '\"",
			"comment_marker_in_string":           "OK [BIGINT] [NULL] [[1]]",
			"comment_mysql_executable_form":      "OK [BIGINT] [NULL] [[1]]",
			"comment_nested_block":               "OK [BIGINT] [NULL] [[1]]",
			"comment_unterminated_block":         "ERROR 42601 RelationalException \"syntax error:\\nline 1:36 token recognition error at: 'n'\"",
			"comment_unterminated_nested":        "ERROR 42601 RelationalException \"syntax error:\\nline 1:40 token recognition error at: '*/'\"",
			"greatest_all_null":                  "ERROR 22F00 SemanticException \"The function is not defined for the given argument types\"",
			"greatest_mixed_numeric":             "OK [DOUBLE] [NOT NULL] [[2.5]]",
			"greatest_one_null":                  "OK [INTEGER] [NULL] [[NULL]]",
			"greatest_single_argument":           "ERROR XX000 VerifyException \"com.google.common.base.VerifyException\"",
			"in_bare_null":                       "ERROR 42809 RelationalException \"NULL values are not allowed in the IN list\"",
			"in_bare_null_beside_literal":        "ERROR 42809 RelationalException \"NULL values are not allowed in the IN list\"",
			"in_column_value_null":               "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"in_typed_null":                      "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"in_wrapped_null":                    "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"least_nullable_column":              "OK [BIGINT BIGINT] [NULL NULL] [[1 NULL] [2 3] [3 3] [4 1] [5 2] [6 3] [7 3]]",
			"like_adjacent_literal_pattern":      "OK [BIGINT] [NULL] [[1] [2] [4] [5] [6] [7]]",
			"like_dangling_escape":               "ERROR 22025 SemanticException \"The like operator pattern requires all escape characters to be followed by a special character.\"",
			"like_empty_pattern":                 "OK [BIGINT] [NULL] []",
			"like_enum_operand":                  "ERROR 22F00 SemanticException \"The like operator expects string operands but was invoked with an operand of another type.\"",
			"like_escape_before_ordinary":        "ERROR 22025 SemanticException \"The like operator pattern requires all escape characters to be followed by a special character.\"",
			"like_escape_empty":                  "ERROR 22019 SemanticException \"The like operator expects an escape character of length 1.\"",
			"like_escape_is_percent":             "ERROR 2200B SemanticException \"The like operator rejects wildcards as the escape character.\"",
			"like_escape_is_underscore":          "ERROR 2200B SemanticException \"The like operator rejects wildcards as the escape character.\"",
			"like_escape_percent":                "OK [BIGINT] [NULL] [[4]]",
			"like_escape_supplementary":          "ERROR 22019 SemanticException \"The like operator expects an escape character of length 1.\"",
			"like_escape_two_chars":              "ERROR 22019 SemanticException \"The like operator expects an escape character of length 1.\"",
			"like_escape_underscore":             "OK [BIGINT] [NULL] [[5]]",
			"like_escaped_escape":                "OK [BIGINT] [NULL] [[7]]",
			"like_no_trailing_newline_tolerance": "OK [BIGINT] [NULL] []",
			"like_not":                           "OK [BIGINT] [NULL] [[3]]",
			"like_null_pattern":                  "OK [BIGINT] [NULL] []",
			"like_null_pattern_bad_escape":       "ERROR 22019 SemanticException \"The like operator expects an escape character of length 1.\"",
			"like_numeric_operand":               "ERROR 22F00 SemanticException \"The like operator expects string operands but was invoked with an operand of another type.\"",
			"like_numeric_pattern":               "ERROR 22F00 SemanticException \"The like operator expects string operands but was invoked with an operand of another type.\"",
			"like_percent_crosses_newline":       "OK [BIGINT] [NULL] [[2] [4] [5] [7]]",
			"like_projection":                    "OK [BIGINT BOOLEAN] [NULL NULL] [[1 true] [2 true] [3 false] [4 true] [5 true] [6 true] [7 true]]",
			"like_underscore_matches_newline":    "OK [BIGINT] [NULL] [[2] [4] [5] [7]]",
			"like_underscore_surrogate_pair":     "OK [BIGINT] [NULL] [[3]]",
			"literal_adjacent":                   "OK [STRING] [NOT NULL] [[ab]]",
			"literal_adjacent_predicate":         "OK [BIGINT] [NULL] [[1]]",
			"literal_adjacent_with_doubled":      "OK [STRING] [NOT NULL] [[a'b]]",
			"literal_doubled_quote":              "OK [STRING] [NOT NULL] [[a'b]]",
			"not_in_bare_null":                   "ERROR 42809 RelationalException \"NULL values are not allowed in the IN list\"",
			"options_after_order_by":             "OK [BIGINT] [NULL] [[1] [2] [3] [4] [5] [6] [7]]",
			"options_ef_search_statement":        "ERROR 42601 RelationalException \"syntax error:\\nSELECT id FROM T WHERE id = 1 OPTIONS (EF_SEARCH 10)\\n                                       ^^^^^^^^^\"",
			"options_inside_subquery":            "ERROR 42601 RelationalException \"syntax error:\\nSELECT id FROM (SELECT id FROM T OPTIONS (NOCACHE)) AS X WHERE id = 1\\n                                 ^^^^^^^\"",
			"options_log_query":                  "OK [BIGINT] [NULL] [[1]]",
			"options_snapshot_select":            "OK [BIGINT] [NULL] [[1]]",
			"prepared_array_typed_null_element":  "ERROR 0A000 RelationalException \"An ARRAY value cannot have NULL elements\"",
			"prepared_coalesce_typed_null":       "OK [INTEGER] [NOT NULL] [[0]]",
			"prepared_eq_typed_null":             "OK [BIGINT] [NULL] []",
			"prepared_in_literal_and_typed_null": "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"prepared_in_long":                   "OK [BIGINT] [NULL] [[1]]",
			"prepared_in_object_null":            "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"prepared_in_typed_null":             "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
			"prepared_int_overflow":              "ERROR XXXXX ArithmeticException \"integer overflow\"",
			"prepared_is_null_param":             "OK [BIGINT] [NULL] [[1] [2] [3] [4] [5] [6] [7]]",
			"prepared_like_param_pattern":        "ERROR 42601 RelationalException \"syntax error:\\nSELECT id FROM T WHERE s LIKE ? ORDER BY id\\n                              ^\"",
			"prepared_limit_param":               "ERROR 0AF00 RelationalException \"LIMIT clause is not supported.\"",
			"prepared_long_no_overflow":          "OK [BIGINT] [NULL] [[2147483648]]",
			"prepared_order_by_param":            "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
			"prepared_select_long":               "OK [BIGINT] [NOT NULL] [[5]]",
			"prepared_string_eq":                 "OK [BIGINT] [NULL] [[1]]",
		})
	})
})

// Second measurement round for the WS-E design (v2): the arms the v1 gates found
// resting on source reading alone. LIKE error TIMING (empty table, filtered-out
// rows, a NULL operand, EXPLAIN); literal decoding the cache key must keep apart
// (B64 case) and decorated literals; EXPLAIN with SNAPSHOT; variadic admission over
// BYTES, incompatible types and eager evaluation; result NULLABILITY through the
// result-set metadata; and parameter binding across several positional
// parameters, SELECT-then-WHERE order, named parameters, DOUBLE/BOOLEAN/LONG
// values, a LONG compared with an INTEGER column and LIMIT. The Java line is
// pinned; the Go line is recorded.
var _ = Describe("WS-E target oracle v2", func() {
	It("records timing, literal, variadic, nullability and parameter-binding outcomes", func() {
		o, done := newWSEOracle("ws_e2_", "WS-E2")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE U (id BIGINT, s STRING, PRIMARY KEY (id)) " +
			"CREATE TABLE W (id BIGINT, b BYTES, PRIMARY KEY (id)) " +
			"CREATE TABLE I (id BIGINT, i INTEGER, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (3, 'xyz', 7)",
			"INSERT INTO W VALUES (1, X'00ff'), (2, NULL)",
			"INSERT INTO I VALUES (1, 5), (2, NULL)",
		}
		probes := []struct{ name, sql string }{
			{"like_bad_escape_empty_table", "SELECT id FROM U WHERE s LIKE 'a%' ESCAPE 'ab'"},
			{"like_dangling_escape_empty_table", "SELECT id FROM U WHERE s LIKE 'a\\' ESCAPE '\\'"},
			{"like_bad_escape_filtered_out", "SELECT id FROM T WHERE id < 0 AND s LIKE 'a%' ESCAPE 'ab'"},
			{"like_bad_escape_null_operand", "SELECT id FROM T WHERE CAST(NULL AS STRING) LIKE 'a%' ESCAPE 'ab'"},
			{"like_bad_escape_explain", "EXPLAIN SELECT id FROM T WHERE s LIKE 'a%' ESCAPE 'ab'"},
			{"b64_literal_upper", "SELECT B64'YWJj' FROM T WHERE id = 1"},
			{"b64_literal_lower", "SELECT B64'ywjj' FROM T WHERE id = 1"},
			{"literal_charset_prefix", "SELECT _utf8'abc' FROM T WHERE id = 1"},
			{"literal_national", "SELECT N'abc' FROM T WHERE id = 1"},
			{"literal_collate", "SELECT 'abc' COLLATE utf8_bin FROM T WHERE id = 1"},
			{"explain_snapshot", "EXPLAIN SELECT id FROM T WHERE id = 1 OPTIONS (ISOLATION LEVEL SNAPSHOT)"},
			{"coalesce_bytes", "SELECT COALESCE(b, X'00') FROM W ORDER BY id"},
			{"greatest_bytes", "SELECT GREATEST(b, b) FROM W WHERE id = 1"},
			{"coalesce_incompatible", "SELECT COALESCE(n, 'a') FROM T WHERE id = 1"},
			{"coalesce_eager_div0_literal", "SELECT COALESCE(1, 1 / 0) FROM T WHERE id = 1"},
			{"coalesce_eager_div0_column", "SELECT COALESCE(id, 1 / 0) FROM T WHERE id = 1"},
			{"nullability_coalesce_nullable_literal", "SELECT COALESCE(n, 0) FROM T WHERE id = 1"},
			{"nullability_coalesce_nullable_nullable", "SELECT COALESCE(n, n) FROM T WHERE id = 1"},
			{"nullability_coalesce_notnull_nullable", "SELECT COALESCE(id, n) FROM T WHERE id = 1"},
			{"nullability_greatest_notnull", "SELECT GREATEST(id, 5) FROM T WHERE id = 1"},
			{"nullability_greatest_nullable", "SELECT GREATEST(n, 5) FROM T WHERE id = 1"},
			{"nullability_least_mixed", "SELECT LEAST(id, n) FROM T WHERE id = 1"},
			{"nullability_arith", "SELECT id + 1 FROM T WHERE id = 1"},
			{"nullability_scalar_function", "SELECT UPPER(s) FROM T WHERE id = 1"},
			{"int_column_eq_literal", "SELECT id FROM I WHERE i = 5"},
			{"null_array_comparison_operand", "SELECT id FROM T WHERE [1, NULL] = [1, NULL] AND id = 1"},
			{"null_array_function_argument", "SELECT CARDINALITY([1, NULL]) FROM T WHERE id = 1"},
		}
		for _, p := range probes {
			o.plain(schema, setup, p.name, p.sql)
		}
		for _, c := range []wseCase{
			wseQuery("prepared_select_then_where", "SELECT ? FROM T WHERE id = ?", wseLong(100), wseLong(1)),
			wseQuery("prepared_two_where", "SELECT id FROM T WHERE id > ? AND s <> ? ORDER BY id", wseLong(1), wseString("abc")),
			wseQuery("prepared_named", "SELECT id FROM T WHERE id = ?x", wseNamed("x", wseLong(1))),
			wseQuery("prepared_named_dollar", "SELECT id FROM T WHERE id = $x", wseNamed("x", wseLong(1))),
			wseQuery("prepared_double", "SELECT ? FROM T WHERE id = 1", wseDouble(1.5)),
			wseQuery("prepared_boolean", "SELECT ? FROM T WHERE id = 1", wseBool(true)),
			wseQuery("prepared_long_vs_int_column", "SELECT id FROM I WHERE i = ?", wseLong(5)),
			wseQuery("prepared_int_vs_int_column", "SELECT id FROM I WHERE i = ?", wseInt(5)),
			wseQuery("prepared_in_two", "SELECT id FROM T WHERE id IN (?, ?) ORDER BY id", wseLong(1), wseLong(3)),
			wseQuery("prepared_like_null_pattern", "SELECT id FROM T WHERE s LIKE ? ORDER BY id", wseNull("VARCHAR")),
		} {
			o.prepared(schema, setup, c)
		}
		// Measured target outcomes (4.14.2.0).
		o.check(map[string]string{
			"like_bad_escape_empty_table":            "OK [BIGINT] [NULL] []",
			"like_dangling_escape_empty_table":       "OK [BIGINT] [NULL] []",
			"like_bad_escape_filtered_out":           "OK [BIGINT] [NULL] []",
			"like_bad_escape_null_operand":           "ERROR 22019 SemanticException \"The like operator expects an escape character of length 1.\"",
			"like_bad_escape_explain":                "OK EXPLAIN \"SCAN([IS T]) | FILTER _.S LIKE @c7 ESCAPE 'ab' | MAP (_.ID AS ID)\"",
			"b64_literal_upper":                      "OK [BINARY] [NOT NULL] [[YWJj]]",
			"b64_literal_lower":                      "OK [BINARY] [NOT NULL] [[ywjj]]",
			"literal_charset_prefix":                 "ERROR 0AF00 RelationalException \"charset not is supported\"",
			"literal_national":                       "ERROR 0AF00 RelationalException \"national string literal is not supported\"",
			"literal_collate":                        "ERROR 0AF00 RelationalException \"collation is not supported\"",
			"explain_snapshot":                       "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)\"",
			"coalesce_bytes":                         "ERROR 22F00 SemanticException \"The function is not defined for the given argument types\"",
			"greatest_bytes":                         "ERROR 22F00 SemanticException \"The function is not defined for the given argument types\"",
			"coalesce_incompatible":                  "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
			"coalesce_eager_div0_literal":            "ERROR XXXXX ArithmeticException \"/ by zero\"",
			"coalesce_eager_div0_column":             "ERROR XXXXX ArithmeticException \"/ by zero\"",
			"nullability_coalesce_nullable_literal":  "OK [BIGINT] [NOT NULL] [[0]]",
			"nullability_coalesce_nullable_nullable": "OK [BIGINT] [NULL] [[NULL]]",
			"nullability_coalesce_notnull_nullable":  "OK [BIGINT] [NULL] [[1]]",
			"nullability_greatest_notnull":           "OK [BIGINT] [NULL] [[5]]",
			"nullability_greatest_nullable":          "OK [BIGINT] [NULL] [[NULL]]",
			"nullability_least_mixed":                "OK [BIGINT] [NULL] [[NULL]]",
			"nullability_arith":                      "OK [BIGINT] [NULL] [[2]]",
			"nullability_scalar_function":            "ERROR 0AF00 RelationalException \"Unsupported operator UPPER\"",
			"int_column_eq_literal":                  "OK [BIGINT] [NULL] [[1]]",
			"null_array_comparison_operand":          "ERROR 0A000 RelationalException \"An ARRAY value cannot have NULL elements\"",
			"null_array_function_argument":           "ERROR 0A000 RelationalException \"An ARRAY value cannot have NULL elements\"",
			"prepared_select_then_where":             "OK [BIGINT] [NOT NULL] []",
			"prepared_two_where":                     "OK [BIGINT] [NULL] [[2] [3]]",
			"prepared_named":                         "OK [BIGINT] [NULL] [[1]]",
			"prepared_named_dollar":                  "OK [BIGINT] [NULL] [[1]]",
			"prepared_double":                        "OK [DOUBLE] [NOT NULL] [[1.5]]",
			"prepared_boolean":                       "OK [BOOLEAN] [NOT NULL] [[true]]",
			"prepared_long_vs_int_column":            "OK [BIGINT] [NULL] [[1]]",
			"prepared_int_vs_int_column":             "OK [BIGINT] [NULL] [[1]]",
			"prepared_in_two":                        "OK [BIGINT] [NULL] [[1] [3]]",
			"prepared_like_null_pattern":             "ERROR 42601 RelationalException \"syntax error:\\nSELECT id FROM T WHERE s LIKE ? ORDER BY id\\n                              ^\"",
		})
	})
})

// Third measurement round: ORDER BY a constant, the SNAPSHOT isolation option set on
// the CONNECTION (Options.Name.ISOLATION_LEVEL_SNAPSHOT is "Scope: Connection,
// Query"), prepared DML storing a LONG or INT parameter into an INTEGER column,
// mixed named and positional parameters, a named parameter used twice, an array
// parameter for `IN ?`, and a UUID parameter. Java lines pinned; Go recorded.
var _ = Describe("WS-E target oracle v2 extended", func() {
	It("records constant ordering, connection snapshot, prepared DML and parameter-kind outcomes", func() {
		o, done := newWSEOracle("ws_e3_", "WS-E3")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE I (id BIGINT, i INTEGER, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (3, 'xyz', 7)",
			"INSERT INTO I VALUES (1, 5)",
		}
		o.plain(schema, setup, "order_by_string_literal", "SELECT id FROM T ORDER BY 'x'")
		o.plain(schema, setup, "order_by_constant_expression", "SELECT id FROM T ORDER BY 1 + 1")
		snapshot := map[string]any{"ISOLATION_LEVEL_SNAPSHOT": true}
		for _, c := range []wseCase{
			wseQuery("snapshot_connection_select", "SELECT id FROM T WHERE id = ?", wseLong(1)).on(snapshot),
			wseQuery("snapshot_connection_explain", "EXPLAIN SELECT id FROM T WHERE id = ?", wseLong(1)).on(snapshot),
			wseDML("snapshot_connection_insert", "INSERT INTO T VALUES (?, 'z', NULL)", "SELECT id FROM T WHERE id = 9", wseLong(9)).on(snapshot),
			wseDML("insert_long_into_int_column", "INSERT INTO I VALUES (?, ?)", "SELECT i FROM I WHERE id = 9", wseLong(9), wseLong(7)),
			wseDML("insert_long_overflow_into_int_column", "INSERT INTO I VALUES (?, ?)", "SELECT i FROM I WHERE id = 10", wseLong(10), wseLong(3000000000)),
			wseDML("insert_int_into_int_column", "INSERT INTO I VALUES (?, ?)", "SELECT i FROM I WHERE id = 11", wseLong(11), wseInt(7)),
			wseQuery("mixed_named_and_positional", "SELECT id FROM T WHERE id = ? OR id = ?x ORDER BY id", wseLong(1), wseNamed("x", wseLong(3))),
			wseQuery("named_parameter_twice", "SELECT id FROM T WHERE id = ?x OR id = ?x + 2 ORDER BY id", wseNamed("x", wseLong(1))),
			wseQuery("in_array_parameter", "SELECT id FROM T WHERE id IN ? ORDER BY id", wseLongArray(1, 3)),
			wseQuery("uuid_parameter", "SELECT ? FROM T WHERE id = 1", wseUUID("123e4567-e89b-12d3-a456-426614174000")),
		} {
			o.prepared(schema, setup, c)
		}
		// Measured target outcomes (4.14.2.0).
		o.check(map[string]string{
			"order_by_string_literal":              "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
			"order_by_constant_expression":         "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
			"snapshot_connection_select":           "OK [BIGINT] [NULL] [[1]]",
			"snapshot_connection_explain":          "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)\"",
			"snapshot_connection_insert":           "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
			"insert_long_into_int_column":          "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
			"insert_long_overflow_into_int_column": "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
			"insert_int_into_int_column":           "OK [INTEGER] [NULL] [[7]] COUNT 1",
			"mixed_named_and_positional":           "OK [BIGINT] [NULL] [[1] [3]]",
			"named_parameter_twice":                "OK [BIGINT] [NULL] [[1] [3]]",
			"in_array_parameter":                   "OK [BIGINT] [NULL] [[1] [3]]",
			"uuid_parameter":                       "OK [OTHER] [NOT NULL] [[123e4567-e89b-12d3-a456-426614174000]]",
		})
	})
})

// Round 3 of the WS-E target oracle, answering the design-v2 gate: nullability of
// all-NOT-NULL scalar calls; COALESCE folding in WHERE versus SELECT; IN and array
// NULL-element timing over an empty table; GREATEST/LEAST over negative doubles
// and floats; column-valued array elements and parenthesised array operands;
// DRY_RUN and ISOLATION_LEVEL_SNAPSHOT as CONNECTION options over DML, DDL,
// SHOW and EXPLAIN INSERT; positional parameter order in UPDATE, INSERT ... SELECT,
// HAVING, IN lists and subqueries; too few and too many parameters; and float and
// double bindings into FLOAT columns. The target's outcome is pinned per probe; the
// Go line is recorded (the Go runner has no connection options, so those rows are
// Java-only here and pinned on the Go side by FDB tests when the port lands).
var _ = Describe("WS-E target oracle v3", func() {
	It("records nullability, folding, NULL timing, connection options, parameter order and binding outcomes", func() {
		o, done := newWSEOracle("ws_e4_", "WS-E4")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE L (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE F (id BIGINT, f FLOAT, PRIMARY KEY (id)) " +
			"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, m BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (3, 'xyz', 7)",
			"INSERT INTO L VALUES (1, 10)",
			"INSERT INTO A VALUES (1, [5], 5), (2, [6], 5)",
		}
		plain := []struct{ name, sql string }{
			{"nn_greatest_literals", "SELECT GREATEST(1, 5) FROM T WHERE id = 1"},
			{"nn_coalesce_literals", "SELECT COALESCE(1, 2) FROM T WHERE id = 1"},
			{"nn_mod_literals", "SELECT 5 % 2 FROM T WHERE id = 1"},
			{"nn_bitand_literals", "SELECT 5 & 3 FROM T WHERE id = 1"},
			{"nn_add_literals", "SELECT 1 + 2 FROM T WHERE id = 1"},
			{"nn_pk_plus_literal", "SELECT id + 1 FROM T WHERE id = 1"},
			{"nn_literal", "SELECT 7 FROM T WHERE id = 1"},
			{"coalesce_true_erroring_tail_where", "SELECT id FROM T WHERE COALESCE(TRUE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_true_erroring_tail_select", "SELECT COALESCE(TRUE, 1 / 0 = 1) FROM T WHERE id = 1"},
			{"in_cast_null_empty_table", "SELECT x FROM E WHERE x IN (CAST(NULL AS BIGINT))"},
			{"in_cast_null_nonempty_table", "SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT))"},
			{"array_cast_null_empty_table", "SELECT [CAST(NULL AS BIGINT)] FROM E"},
			{"array_cast_null_nonempty_table", "SELECT [CAST(NULL AS BIGINT)] FROM T WHERE id = 1"},
			{"greatest_negative_doubles", "SELECT GREATEST(-1.5, -2.5) FROM T WHERE id = 1"},
			{"least_negative_doubles", "SELECT LEAST(-1.5, -2.5) FROM T WHERE id = 1"},
			{"greatest_negative_floats", "SELECT GREATEST(-1.5f, -2.5f) FROM T WHERE id = 1"},
			{"greatest_negative_longs", "SELECT GREATEST(-1, -2) FROM T WHERE id = 1"},
			{"array_eq_column_element", "SELECT id FROM A WHERE arr = [m] ORDER BY id"},
			{"array_eq_parenthesised_operand", "SELECT id FROM A WHERE (arr) = [5] ORDER BY id"},
			{"array_eq_literal", "SELECT id FROM A WHERE arr = [5] ORDER BY id"},
		}
		for _, p := range plain {
			o.plain(schema, setup, p.name, p.sql)
		}
		dryRun := map[string]any{"DRY_RUN": true}
		snapshot := map[string]any{"ISOLATION_LEVEL_SNAPSHOT": true}
		const readL = "SELECT id, v FROM L ORDER BY id"
		for _, c := range []wseCase{
			wseDML("dry_run_connection_insert", "INSERT INTO L VALUES (?, 20)", readL, wseLong(2)).on(dryRun),
			wseDML("dry_run_connection_update", "UPDATE L SET v = 99 WHERE id = ?", readL, wseLong(1)).on(dryRun),
			wseQuery("dry_run_connection_select", "SELECT id FROM L WHERE id = ?", wseLong(1)).on(dryRun),
			wseDML("snapshot_connection_update", "UPDATE L SET v = 99 WHERE id = ?", readL, wseLong(1)).on(snapshot),
			wseDML("snapshot_connection_delete", "DELETE FROM L WHERE id = ?", readL, wseLong(1)).on(snapshot),
			wseQuery("snapshot_connection_explain_insert", "EXPLAIN INSERT INTO L VALUES (5, 50)").on(snapshot),
			wseDML("snapshot_connection_create_database", "CREATE DATABASE /WSE4_SNAPSHOT_DDL", "SELECT id FROM L ORDER BY id").on(snapshot),
			wseDML("snapshot_connection_drop_database", "DROP DATABASE IF EXISTS /WSE4_SNAPSHOT_ABSENT", "SELECT id FROM L ORDER BY id").on(snapshot),
			wseQuery("snapshot_connection_show_databases", "SHOW DATABASES").on(snapshot),
			wseQuery("snapshot_connection_show_templates", "SHOW SCHEMA TEMPLATES").on(snapshot),
			wseDML("update_set_then_where_order", "UPDATE L SET v = ? WHERE id = ?", readL, wseLong(100), wseLong(1)),
			wseDML("insert_select_then_where_order", "INSERT INTO L SELECT ?, ? FROM T WHERE id = ?", readL, wseLong(20), wseLong(7), wseLong(1)),
			wseQuery("where_then_having_order", "SELECT n FROM T WHERE id > ? GROUP BY n HAVING n > ?", wseLong(0), wseLong(5)),
			// Bound (2, 1, 0): textual order is `id IN (2, 1) AND n > 0`, [[2]]; binding
			// the trailing conjunct first would be `n > 2 AND id IN (1, 0)`, no rows.
			wseQuery("in_list_order", "SELECT id FROM T WHERE id IN (?, ?) AND n > ? ORDER BY id", wseLong(2), wseLong(1), wseLong(0)),
			wseQuery("subquery_order", "SELECT id FROM T WHERE id = ? OR id IN (SELECT id FROM T WHERE n = ?) ORDER BY id", wseLong(1), wseLong(7)),
			wseQuery("select_expr_then_where_order", "SELECT id, n + ? AS k FROM T WHERE id = ?", wseLong(10), wseLong(2)),
			wseQuery("too_few_parameters", "SELECT id FROM T WHERE id = ? OR id = ?", wseLong(1)),
			wseQuery("too_many_parameters", "SELECT id FROM T WHERE id = ?", wseLong(1), wseLong(3)),
			wseDML("insert_double_into_float_column", "INSERT INTO F VALUES (?, ?)", "SELECT id, f FROM F ORDER BY id", wseLong(1), wseDouble(1.5)),
			wseDML("insert_float_into_float_column", "INSERT INTO F VALUES (?, ?)", "SELECT id, f FROM F ORDER BY id", wseLong(2), wseFloat(1.5)),
			wseDML("insert_int_into_bigint_column", "INSERT INTO L VALUES (?, ?)", readL, wseLong(3), wseInt(7)),
			wseDML("insert_double_literal_into_float_column", "INSERT INTO F VALUES (3, 1.5)", "SELECT id, f FROM F ORDER BY id"),
			wseDML("insert_float_literal_into_float_column", "INSERT INTO F VALUES (4, 1.5f)", "SELECT id, f FROM F ORDER BY id"),
			wseQuery("select_untyped_null_param", "SELECT ? FROM T WHERE id = 1", wseObjectNull()),
			wseQuery("arith_untyped_null_param", "SELECT ? + 1 FROM T WHERE id = 1", wseObjectNull()),
			wseDML("insert_untyped_null_param", "INSERT INTO L VALUES (?, ?)", readL, wseLong(7), wseObjectNull()),
		} {
			o.prepared(schema, setup, c)
		}
		o.check(wsE4Pins)
	})
})

// wsE4Pins is the measured target outcome of every round-3 probe (4.14.2.0).
var wsE4Pins = map[string]string{
	"arith_untyped_null_param":                "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"array_cast_null_empty_table":             "OK [ARRAY] [NOT NULL] []",
	"array_cast_null_nonempty_table":          "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"array_eq_column_element":                 "OK [BIGINT] [NULL] [[1]]",
	"array_eq_literal":                        "ERROR 42804 SemanticException \"The operands of a comparison operator are not compatible.\"",
	"array_eq_parenthesised_operand":          "ERROR 42804 SemanticException \"The operands of a comparison operator are not compatible.\"",
	"coalesce_true_erroring_tail_select":      "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_true_erroring_tail_where":       "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"dry_run_connection_insert":               "OK [BIGINT BIGINT] [NULL NULL] [[1 10]] COUNT 1",
	"dry_run_connection_select":               "OK [BIGINT] [NULL] [[1]]",
	"dry_run_connection_update":               "OK [BIGINT BIGINT] [NULL NULL] [[1 10]] COUNT 1",
	"greatest_negative_doubles":               "OK [DOUBLE] [NOT NULL] [[5e-324]]",
	"greatest_negative_floats":                "OK [FLOAT] [NOT NULL] [[1.4e-45]]",
	"greatest_negative_longs":                 "OK [INTEGER] [NOT NULL] [[-1]]",
	"in_cast_null_empty_table":                "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"in_cast_null_nonempty_table":             "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"in_list_order":                           "OK [BIGINT] [NULL] [[2]]",
	"insert_double_into_float_column":         "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_double_literal_into_float_column": "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_float_into_float_column":          "OK [BIGINT FLOAT] [NULL NULL] [[2 1.5]] COUNT 1",
	"insert_float_literal_into_float_column":  "OK [BIGINT FLOAT] [NULL NULL] [[4 1.5]] COUNT 1",
	"insert_int_into_bigint_column":           "OK [BIGINT BIGINT] [NULL NULL] [[1 10] [3 7]] COUNT 1",
	"insert_select_then_where_order":          "OK [BIGINT BIGINT] [NULL NULL] [[1 10]] COUNT 0",
	"insert_untyped_null_param":               "OK [BIGINT BIGINT] [NULL NULL] [[1 10] [7 NULL]] COUNT 1",
	"least_negative_doubles":                  "OK [DOUBLE] [NOT NULL] [[-2.5]]",
	"nn_add_literals":                         "OK [INTEGER] [NULL] [[3]]",
	"nn_bitand_literals":                      "OK [INTEGER] [NULL] [[1]]",
	"nn_coalesce_literals":                    "OK [INTEGER] [NOT NULL] [[1]]",
	"nn_greatest_literals":                    "OK [INTEGER] [NOT NULL] [[5]]",
	"nn_literal":                              "OK [INTEGER] [NOT NULL] [[7]]",
	"nn_mod_literals":                         "OK [INTEGER] [NULL] [[1]]",
	"nn_pk_plus_literal":                      "OK [BIGINT] [NULL] [[2]]",
	"select_expr_then_where_order":            "OK [BIGINT BIGINT] [NULL NULL] []",
	"select_untyped_null_param":               "ERROR XXXXX RecordCoreException \"should not be called\"",
	"snapshot_connection_create_database":     "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_delete":              "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_drop_database":       "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_explain_insert":      "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_show_databases":      "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_show_templates":      "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_update":              "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"subquery_order":                          "ERROR 0AF00 RelationalException \"IN predicate does not support nested SELECT\"",
	"too_few_parameters":                      "ERROR 42F02 RelationalException \"No value found for parameter 2\"",
	"too_many_parameters":                     "OK [BIGINT] [NULL] [[1]]",
	"update_set_then_where_order":             "OK [BIGINT BIGINT] [NULL NULL] [[1 10]] COUNT 0",
	"where_then_having_order":                 "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
}

// Round 4 of the WS-E target oracle, answering the design-v3 gates. Folding
// regimes: a COALESCE whose head is a NOT, AND, OR or arithmetic over literals,
// a NULL head, and a null-strict operator over a NULL beside an erroring operand,
// in WHERE and SELECT, plus an inline NULL in arithmetic. IN-list NULL timing
// separated from planning (EXPLAIN, a mixed list, rows filtered out). The GREATEST
// and LEAST lanes at zero, NaN, infinity and LONG. DESCRIBE, DESC and HELP on a
// snapshot connection. DRY_RUN over DELETE and DDL, with the update counts every
// DML row now carries. Assignment of expressions and column sources (INSERT
// VALUES, UPDATE SET, INSERT ... SELECT) per lane. ARRAY parameters in INSERT and
// UPDATE. Controls for the HAVING row, and array comparisons with a NULL element.
var _ = Describe("WS-E target oracle v4", func() {
	It("records folding regimes, IN timing, variadic lanes, describe admission, dry-run DDL, assignment and array parameters", func() {
		o, done := newWSEOracle("ws_e5_", "WS-E5")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE L (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE I (id BIGINT, i INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE F (id BIGINT, f FLOAT, PRIMARY KEY (id)) " +
			"CREATE TABLE D (id BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
			"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, m BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (3, 'xyz', 7)",
			"INSERT INTO L VALUES (1, 10)",
			"INSERT INTO I VALUES (1, 5)",
			"INSERT INTO F VALUES (1, 2.5f)",
			"INSERT INTO D VALUES (1, 1.5)",
			"INSERT INTO A VALUES (1, [5], 5)",
		}
		for _, p := range []struct{ name, sql string }{
			// Folding regimes. The target's predicate rule set dereferences a constant
			// object and folds promotion and COALESCE only; a head it cannot reduce to a
			// literal keeps every argument, and every argument is evaluated.
			{"coalesce_not_head_where", "SELECT id FROM T WHERE COALESCE(NOT FALSE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_and_head_where", "SELECT id FROM T WHERE COALESCE(TRUE AND TRUE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_or_head_where", "SELECT id FROM T WHERE COALESCE(FALSE OR TRUE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_not_head_select", "SELECT COALESCE(NOT FALSE, 1 / 0 = 1) FROM T WHERE id = 1"},
			{"coalesce_arith_head_where", "SELECT id FROM T WHERE COALESCE(1 + 1, 1 / 0) = 2 ORDER BY id"},
			{"coalesce_null_head_where", "SELECT id FROM T WHERE COALESCE(NULL, TRUE) ORDER BY id"},
			{"coalesce_cast_null_head_where", "SELECT id FROM T WHERE COALESCE(CAST(NULL AS BOOLEAN), TRUE) ORDER BY id"},
			{"coalesce_null_head_erroring_tail_where", "SELECT id FROM T WHERE COALESCE(NULL, TRUE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_cast_null_head_erroring_tail_where", "SELECT id FROM T WHERE COALESCE(CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_int_literal_head_where", "SELECT id FROM T WHERE COALESCE(1, 1 / 0) = 1 ORDER BY id"},
			{"coalesce_true_head_where_explain", "EXPLAIN SELECT id FROM T WHERE COALESCE(TRUE, 1 / 0 = 1)"},
			{"coalesce_not_head_where_explain", "EXPLAIN SELECT id FROM T WHERE COALESCE(NOT FALSE, 1 / 0 = 1)"},
			{"null_strict_cast_null_beside_div0_select", "SELECT (1 / 0) + CAST(NULL AS INTEGER) FROM T WHERE id = 1"},
			{"null_strict_null_beside_div0_select", "SELECT (1 / 0) + NULL FROM T WHERE id = 1"},
			{"null_strict_cast_null_beside_div0_where", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1"},
			{"null_strict_cast_null_beside_div0_where_empty_table", "SELECT id FROM E WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1"},
			{"null_strict_cast_null_beside_div0_where_explain", "EXPLAIN SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1"},
			{"null_strict_cast_null_beside_div0_select_explain", "EXPLAIN SELECT (1 / 0) + CAST(NULL AS INTEGER) FROM T WHERE id = 1"},
			{"null_strict_column_beside_cast_null_where_explain", "EXPLAIN SELECT id FROM T WHERE n + CAST(NULL AS BIGINT) = 1"},
			{"inline_null_plus_one_select", "SELECT NULL + 1 FROM T WHERE id = 1"},
			{"inline_null_plus_one_where", "SELECT id FROM T WHERE NULL + 1 = 2"},
			{"cast_null_plus_one_select", "SELECT CAST(NULL AS BIGINT) + 1 FROM T WHERE id = 1"},
			// Arithmetic lanes beyond the numeric ones: the target's operator map has
			// STRING lanes for ADD only, and no BOOLEAN lane.
			{"add_string_int_select", "SELECT 'a' + 1 FROM T WHERE id = 1"},
			{"add_string_string_select", "SELECT s + s FROM T WHERE id = 1"},
			{"sub_string_int_select", "SELECT 'a' - 1 FROM T WHERE id = 1"},
			{"add_int_boolean_select", "SELECT 1 + TRUE FROM T WHERE id = 1"},
			// IN-list NULL timing: planning, execution start, or per row.
			{"in_cast_null_explain", "EXPLAIN SELECT x FROM E WHERE x IN (CAST(NULL AS BIGINT))"},
			{"in_cast_null_mixed_empty_table", "SELECT x FROM E WHERE x IN (CAST(NULL AS BIGINT), x)"},
			{"in_cast_null_mixed_nonempty_table", "SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT), id)"},
			{"in_cast_null_filtered_out", "SELECT id FROM T WHERE id < 0 AND id IN (CAST(NULL AS BIGINT))"},
			{"in_literal_and_cast_null_explain", "EXPLAIN SELECT id FROM T WHERE id IN (1, CAST(NULL AS BIGINT))"},
			{"in_single_literal_explain", "EXPLAIN SELECT id FROM T WHERE id IN (1)"},
			// GREATEST and LEAST lanes.
			{"greatest_zero_doubles", "SELECT GREATEST(0.0, 0.0) FROM T WHERE id = 1"},
			{"greatest_zero_floats", "SELECT GREATEST(0.0f, 0.0f) FROM T WHERE id = 1"},
			{"greatest_nan_double", "SELECT GREATEST(0.0 / 0.0, -1.0) FROM T WHERE id = 1"},
			{"least_nan_double", "SELECT LEAST(0.0 / 0.0, 1.0) FROM T WHERE id = 1"},
			{"least_infinite_doubles", "SELECT LEAST(1.0 / 0.0, 1.0 / 0.0) FROM T WHERE id = 1"},
			{"least_overflowing_literal", "SELECT LEAST(1e309, 1e309) FROM T WHERE id = 1"},
			{"greatest_negative_long_literals", "SELECT GREATEST(-3000000000, -4000000000) FROM T WHERE id = 1"},
			{"least_long_literals", "SELECT LEAST(3000000000, 4000000000) FROM T WHERE id = 1"},
			{"greatest_negative_long_columns", "SELECT GREATEST(n - 100, n - 200) FROM T WHERE id = 2"},
			// DESCRIBE of a query, without options.
			{"describe_select", "DESCRIBE SELECT id FROM L WHERE id = 1"},
			// HAVING controls for where_then_having_order: the same statement with
			// literals, and the GROUP BY alone.
			{"having_literal_control", "SELECT n FROM T WHERE id > 0 GROUP BY n HAVING n > 5"},
			{"group_by_literal_control", "SELECT n FROM T WHERE id > 0 GROUP BY n"},
			// Array comparison operands with a NULL element.
			{"array_column_eq_column_and_null", "SELECT id FROM A WHERE arr = [m, NULL]"},
			{"array_null_element_eq_literal", "SELECT id FROM T WHERE [1, NULL] = [1, 2] AND id = 1"},
		} {
			o.plain(schema, setup, p.name, p.sql)
		}
		dryRun := map[string]any{"DRY_RUN": true}
		snapshot := map[string]any{"ISOLATION_LEVEL_SNAPSHOT": true}
		const (
			readL = "SELECT id, v FROM L ORDER BY id"
			readI = "SELECT id, i FROM I ORDER BY id"
			readF = "SELECT id, f FROM F ORDER BY id"
			readD = "SELECT id, d FROM D ORDER BY id"
			readA = "SELECT id, arr FROM A ORDER BY id"
		)
		for _, c := range []wseCase{
			// A bound BOOLEAN or NULL COALESCE head: a parameter is a constant object
			// the predicate regime dereferences; the projection regime does not.
			wseQuery("coalesce_bound_true_head_where", "SELECT id FROM T WHERE COALESCE(?, 1 / 0 = 1) ORDER BY id", wseBool(true)),
			wseQuery("coalesce_bound_null_head_where", "SELECT id FROM T WHERE COALESCE(?, TRUE, 1 / 0 = 1) ORDER BY id", wseObjectNull()),
			wseQuery("coalesce_bound_true_head_select", "SELECT COALESCE(?, 1 / 0 = 1) FROM T WHERE id = 1", wseBool(true)),
			// Statement classes on a snapshot connection.
			wseQuery("snapshot_connection_describe_select", "DESCRIBE SELECT id FROM L WHERE id = 1").on(snapshot),
			wseQuery("snapshot_connection_desc_select", "DESC SELECT id FROM L WHERE id = 1").on(snapshot),
			wseQuery("snapshot_connection_describe_schema", "DESCRIBE SCHEMA WSE5_ABSENT").on(snapshot),
			wseQuery("snapshot_connection_describe_template", "DESCRIBE SCHEMA TEMPLATE WSE5_ABSENT").on(snapshot),
			wseQuery("snapshot_connection_help", "HELP 'x'").on(snapshot),
			// DRY_RUN on a connection over DELETE and DDL. The DDL's follow-up describes
			// the template it would create (SHOW DATABASES ignores WITH PREFIX and lists
			// every database, so it cannot be pinned). The target ignores DRY_RUN on DDL,
			// so the template IS created in this Describe's fresh tenant; it is dropped
			// after the probe, below, and that drop is asserted.
			wseDML("dry_run_connection_delete", "DELETE FROM L WHERE id = ?", readL, wseLong(1)).on(dryRun),
			wseDML("dry_run_connection_create_template", "CREATE SCHEMA TEMPLATE WSE5_DRYRUN_TPL CREATE TABLE X (id BIGINT, PRIMARY KEY (id))",
				"DESCRIBE SCHEMA TEMPLATE WSE5_DRYRUN_TPL").on(dryRun),
			// Assignment of an expression or a column source, per lane.
			wseDML("insert_double_expr_into_float_column", "INSERT INTO F VALUES (5, 1.5 + 0.0)", readF),
			wseDML("update_float_column_plus_double", "UPDATE F SET f = f + 0.5 WHERE id = 1", readF),
			wseDML("update_float_column_plus_float", "UPDATE F SET f = f + 0.5f WHERE id = 1", readF),
			wseDML("insert_select_double_column_into_float", "INSERT INTO F SELECT id + 100, d FROM D", readF),
			wseDML("insert_select_bigint_column_into_int", "INSERT INTO I SELECT id + 100, v FROM L", readI),
			wseDML("insert_select_int_column_into_bigint", "INSERT INTO L SELECT id + 100, i FROM I", readL),
			wseDML("insert_int_expr_into_int_column", "INSERT INTO I VALUES (20, 1 + 2)", readI),
			wseDML("insert_long_expr_into_int_column", "INSERT INTO I VALUES (21, 3000000000 - 2999999999)", readI),
			wseDML("insert_long_literal_into_int_column", "INSERT INTO I VALUES (22, 3000000000)", readI),
			wseDML("update_int_column_from_bigint_column", "UPDATE I SET i = id WHERE id = 1", readI),
			wseDML("update_int_column_plus_literal", "UPDATE I SET i = i + 1 WHERE id = 1", readI),
			wseDML("insert_int_literal_into_float_column", "INSERT INTO F VALUES (6, 1)", readF),
			wseDML("insert_int_literal_into_double_column", "INSERT INTO D VALUES (7, 1)", readD),
			wseDML("insert_float_literal_into_double_column", "INSERT INTO D VALUES (8, 1.5f)", readD),
			wseDML("insert_double_literal_into_bigint_column", "INSERT INTO L VALUES (9, 1.5)", readL),
			wseDML("insert_int_param_into_float_column", "INSERT INTO F VALUES (?, ?)", readF, wseLong(10), wseInt(1)),
			// ARRAY parameters outside `IN ?`.
			wseDML("insert_array_parameter", "INSERT INTO A VALUES (?, ?, ?)", readA, wseLong(3), wseLongArray(7, 8), wseLong(5)),
			wseDML("insert_empty_array_parameter", "INSERT INTO A VALUES (?, ?, ?)", readA, wseLong(4), wseLongArray(), wseLong(5)),
			wseDML("insert_null_array_parameter", "INSERT INTO A VALUES (?, ?, ?)", readA, wseLong(5), wseNull("ARRAY"), wseLong(5)),
			wseDML("update_array_parameter", "UPDATE A SET arr = ? WHERE id = 1", readA, wseLongArray(9)),
		} {
			o.prepared(schema, setup, c)
		}
		// The template the dry-run DDL created (its pin describes it) is removed, and the
		// removal is checked by describing it again, which must now fail.
		var dropped struct {
			Dropped bool `json:"dropped"`
		}
		Expect(o.srv.InvokeAs(o.ctx, "dropSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": o.clusterFile, "templateName": "WSE5_DRYRUN_TPL",
		}, &dropped)).To(Succeed())
		Expect(dropped.Dropped).To(BeTrue(), "the dry-run template was not dropped")
		gone := o.java.RunWithSetup(o.ctx, schema, nil, "DESCRIBE SCHEMA TEMPLATE WSE5_DRYRUN_TPL")
		Expect(gone.Err).To(HaveOccurred(), "the dry-run template is still described after its drop")
		var goneJE *plandiff.JavaError
		Expect(errors.As(gone.Err, &goneJE)).To(BeTrue(), "the dry-run template's absence is a target error: %v", gone.Err)
		fmt.Fprintf(GinkgoWriter, "WS-E5 dry-run template after its drop: %s %s %q\n", goneJE.SQLState, goneJE.ExceptionClass, goneJE.Message)
		Expect(goneJE.SQLState).To(Equal("42F55"), "the dry-run template is gone (UNKNOWN_SCHEMA_TEMPLATE), not failing for another reason")
		// Read scope of the snapshot option (section 6.4): what a read leaves in its
		// transaction's read-conflict set, with a concurrent writer committing between
		// the read and the reader's own write. Three access paths, each confirmed by its
		// EXPLAIN: a covering scan of S_V (`SELECT id FROM S`), an index scan of S_V that
		// fetches each record (`w` is not in the index), and a record scan of P, which has
		// no secondary index.
		rsSchema := "CREATE TABLE S (id BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE P (id BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE WR (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX S_V AS SELECT v FROM S ORDER BY v"
		rsSetup := []string{
			"INSERT INTO S VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300)",
			"INSERT INTO P VALUES (1, 100), (2, 200), (3, 300)",
		}
		const snap = " OPTIONS (ISOLATION LEVEL SNAPSHOT)"
		const (
			coveringRead = "SELECT id FROM S"
			fetchRead    = "SELECT id, w FROM S WHERE v > 15"
			scanRead     = "SELECT id, w FROM P"
			insertS      = "INSERT INTO S VALUES (5, 25, 250)"
			updateS      = "UPDATE S SET w = 201 WHERE id = 2"
			insertP      = "INSERT INTO P VALUES (5, 250)"
			updateP      = "UPDATE P SET w = 201 WHERE id = 2"
		)
		for _, c := range []struct{ name, read, concurrent string }{
			{"read_scope_covering_serializable_insert", coveringRead, insertS},
			{"read_scope_covering_snapshot_insert", coveringRead + snap, insertS},
			{"read_scope_covering_snapshot_update", coveringRead + snap, updateS},
			{"read_scope_fetch_serializable_insert", fetchRead, insertS},
			{"read_scope_fetch_snapshot_insert", fetchRead + snap, insertS},
			{"read_scope_fetch_serializable_update_fetched", fetchRead, updateS},
			{"read_scope_fetch_snapshot_update_fetched", fetchRead + snap, updateS},
			{"read_scope_scan_serializable_insert", scanRead, insertP},
			{"read_scope_scan_snapshot_insert", scanRead + snap, insertP},
			{"read_scope_scan_snapshot_update", scanRead + snap, updateP},
		} {
			o.readScope(rsSchema, rsSetup, c.name, c.read, c.concurrent, "INSERT INTO WR VALUES (1)")
		}
		o.plain(rsSchema, rsSetup, "read_scope_covering_explain", "EXPLAIN "+coveringRead)
		o.plain(rsSchema, rsSetup, "read_scope_fetch_explain", "EXPLAIN "+fetchRead)
		o.plain(rsSchema, rsSetup, "read_scope_scan_explain", "EXPLAIN "+scanRead)
		o.check(wsE5Pins)
	})
})

// wsE5Pins is the measured target outcome of every round-4 probe (4.14.2.0).
var wsE5Pins = map[string]string{
	"add_int_boolean_select":                              "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"add_string_int_select":                               "OK [STRING] [NULL] [[a1]]",
	"add_string_string_select":                            "OK [STRING] [NULL] [[abcabc]]",
	"array_column_eq_column_and_null":                     "ERROR 0A000 RelationalException \"An ARRAY value cannot have NULL elements\"",
	"array_null_element_eq_literal":                       "ERROR 0A000 RelationalException \"An ARRAY value cannot have NULL elements\"",
	"cast_null_plus_one_select":                           "OK [BIGINT] [NULL] [[NULL]]",
	"coalesce_and_head_where":                             "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_arith_head_where":                           "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_bound_null_head_where":                      "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_bound_true_head_select":                     "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_bound_true_head_where":                      "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_cast_null_head_erroring_tail_where":         "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_cast_null_head_where":                       "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_int_literal_head_where":                     "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_not_head_select":                            "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_not_head_where":                             "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_not_head_where_explain":                     "OK EXPLAIN \"SCAN([IS T]) | FILTER coalesce_boolean(NOT 'false', @c10 / @c12 equals @c10) EQUALS true | MAP (_.ID AS ID)\"",
	"coalesce_null_head_erroring_tail_where":              "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_null_head_where":                            "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_or_head_where":                              "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"coalesce_true_head_where_explain":                    "OK EXPLAIN \"SCAN([IS T]) | MAP (_.ID AS ID)\"",
	"describe_select":                                     "OK EXPLAIN \"SCAN([IS L, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)\"",
	"dry_run_connection_create_template":                  "OK [STRING ARRAY] [NOT NULL NULL] [[WSE5_DRYRUN_TPL [map[COLUMNS:[map[COLUMN_NAME:ID COLUMN_TYPE:-5]] TABLE_NAME:X]]]] COUNT 0",
	"dry_run_connection_delete":                           "OK [BIGINT BIGINT] [NULL NULL] [[1 10]] COUNT 1",
	"greatest_nan_double":                                 "OK [DOUBLE] [NULL] [[5e-324]]",
	"greatest_negative_long_columns":                      "OK [BIGINT] [NULL] [[-95]]",
	"greatest_negative_long_literals":                     "OK [BIGINT] [NOT NULL] [[-3000000000]]",
	"greatest_zero_doubles":                               "OK [DOUBLE] [NOT NULL] [[5e-324]]",
	"greatest_zero_floats":                                "OK [FLOAT] [NOT NULL] [[1.4e-45]]",
	"group_by_literal_control":                            "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
	"having_literal_control":                              "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
	"in_cast_null_explain":                                "OK EXPLAIN \"EXPLODE arrayDistinct(array(NULL)) | FLATMAP q0 -> { SCAN([IS E]) | FILTER _.X EQUALS q0 AS q1 RETURN (q1.X AS X) }\"",
	"in_cast_null_filtered_out":                           "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"in_cast_null_mixed_empty_table":                      "OK [BIGINT] [NULL] []",
	"in_cast_null_mixed_nonempty_table":                   "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"in_literal_and_cast_null_explain":                    "OK EXPLAIN \"[IN arrayDistinct(array(promote(@c8 AS LONG), NULL))] | INJOIN q0 -> { SCAN([IS T, EQUALS q0]) | MAP (_.ID AS ID) }\"",
	"in_single_literal_explain":                           "OK EXPLAIN \"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG)))] | INJOIN q0 -> { SCAN([IS T, EQUALS q0]) | MAP (_.ID AS ID) }\"",
	"inline_null_plus_one_select":                         "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"inline_null_plus_one_where":                          "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"insert_array_parameter":                              "OK [BIGINT ARRAY] [NULL NULL] [[1 [5]] [3 [7 8]]] COUNT 1",
	"insert_double_expr_into_float_column":                "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_double_literal_into_bigint_column":            "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_empty_array_parameter":                        "OK [BIGINT ARRAY] [NULL NULL] [[1 [5]] [4 []]] COUNT 1",
	"insert_float_literal_into_double_column":             "OK [BIGINT DOUBLE] [NULL NULL] [[1 1.5] [8 1.5]] COUNT 1",
	"insert_int_expr_into_int_column":                     "OK [BIGINT INTEGER] [NULL NULL] [[1 5] [20 3]] COUNT 1",
	"insert_int_literal_into_double_column":               "OK [BIGINT DOUBLE] [NULL NULL] [[1 1.5] [7 1]] COUNT 1",
	"insert_int_literal_into_float_column":                "OK [BIGINT FLOAT] [NULL NULL] [[1 2.5] [6 1]] COUNT 1",
	"insert_int_param_into_float_column":                  "OK [BIGINT FLOAT] [NULL NULL] [[1 2.5] [10 1]] COUNT 1",
	"insert_long_expr_into_int_column":                    "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_long_literal_into_int_column":                 "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_null_array_parameter":                         "OK [BIGINT ARRAY] [NULL NULL] [[1 [5]] [5 NULL]] COUNT 1",
	"insert_select_bigint_column_into_int":                "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_select_double_column_into_float":              "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_select_int_column_into_bigint":                "OK [BIGINT BIGINT] [NULL NULL] [[1 10] [101 5]] COUNT 1",
	"least_infinite_doubles":                              "OK [DOUBLE] [NULL] [[1.7976931348623157e+308]]",
	"least_long_literals":                                 "OK [BIGINT] [NOT NULL] [[3000000000]]",
	"least_nan_double":                                    "OK [DOUBLE] [NULL] [[1]]",
	"least_overflowing_literal":                           "ERROR XXXXX NumberFormatException \"For input string: \\\"1e309\\\"\"",
	"null_strict_cast_null_beside_div0_select":            "OK [INTEGER] [NULL] [[NULL]]",
	"null_strict_cast_null_beside_div0_select_explain":    "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c18 AS LONG)]) | MAP (NULL AS _0)\"",
	"null_strict_cast_null_beside_div0_where":             "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"null_strict_cast_null_beside_div0_where_empty_table": "OK [BIGINT] [NULL] []",
	"null_strict_cast_null_beside_div0_where_explain":     "OK EXPLAIN \"SCAN([IS T]) | FILTER promote(@c6 AS INT) EQUALS @c6 / @c8 + NULL | MAP (_.ID AS ID)\"",
	"null_strict_column_beside_cast_null_where_explain":   "OK EXPLAIN \"SCAN([IS T]) | FILTER _.N + NULL EQUALS promote(@c14 AS LONG) | MAP (_.ID AS ID)\"",
	"null_strict_null_beside_div0_select":                 "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"read_scope_covering_explain":                         "OK EXPLAIN \"COVERING(S_V <,> -> [ID: KEY:[2], V: KEY:[0]]) | MAP (_.ID AS ID)\"",
	"read_scope_covering_serializable_insert":             "READ OK [BIGINT] [NULL] [[1] [2] [3]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"read_scope_covering_snapshot_insert":                 "READ OK [BIGINT] [NULL] [[1] [2] [3]] COMMIT OK",
	"read_scope_covering_snapshot_update":                 "READ OK [BIGINT] [NULL] [[1] [2] [3]] COMMIT OK",
	"read_scope_fetch_explain":                            "OK EXPLAIN \"ISCAN(S_V [[GREATER_THAN promote(@c9 AS LONG)]]) | MAP (_.ID AS ID, _.W AS W)\"",
	"read_scope_fetch_serializable_insert":                "READ OK [BIGINT BIGINT] [NULL NULL] [[2 200] [3 300]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"read_scope_fetch_serializable_update_fetched":        "READ OK [BIGINT BIGINT] [NULL NULL] [[2 200] [3 300]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"read_scope_fetch_snapshot_insert":                    "READ OK [BIGINT BIGINT] [NULL NULL] [[2 200] [3 300]] COMMIT OK",
	"read_scope_fetch_snapshot_update_fetched":            "READ OK [BIGINT BIGINT] [NULL NULL] [[2 200] [3 300]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"read_scope_scan_explain":                             "OK EXPLAIN \"SCAN([IS P])\"",
	"read_scope_scan_serializable_insert":                 "READ OK [BIGINT BIGINT] [NULL NULL] [[1 100] [2 200] [3 300]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"read_scope_scan_snapshot_insert":                     "READ OK [BIGINT BIGINT] [NULL NULL] [[1 100] [2 200] [3 300]] COMMIT OK",
	"read_scope_scan_snapshot_update":                     "READ OK [BIGINT BIGINT] [NULL NULL] [[1 100] [2 200] [3 300]] COMMIT OK",
	"snapshot_connection_desc_select":                     "OK EXPLAIN \"SCAN([IS L, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)\"",
	"snapshot_connection_describe_schema":                 "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_describe_select":                 "OK EXPLAIN \"SCAN([IS L, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)\"",
	"snapshot_connection_describe_template":               "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"snapshot_connection_help":                            "ERROR 0A000 RelationalException \"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries\"",
	"sub_string_int_select":                               "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"update_array_parameter":                              "OK [BIGINT ARRAY] [NULL NULL] [[1 [9]]] COUNT 1",
	"update_float_column_plus_double":                     "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"update_float_column_plus_float":                      "OK [BIGINT FLOAT] [NULL NULL] [[1 3]] COUNT 1",
	"update_int_column_from_bigint_column":                "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"update_int_column_plus_literal":                      "OK [BIGINT INTEGER] [NULL NULL] [[1 6]] COUNT 1",
}

var _ = Describe("WS-E target oracle v5", func() {
	It("records predicate folding, IN timing outside the explode, STRING lanes, nested assignment and index-state conflicts", func() {
		o, done := newWSEOracle("ws_e6_", "WS-E6")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (3, 'xyz', 7)"}
		// Whether the first planning of a statement shape in the JVM decides later ones:
		// shape A is executed first and then explained; shape B is explained first and
		// then executed. Each probe has its own schema template.
		for _, p := range []struct{ name, sql string }{
			{"first_planning_a_exec", "SELECT id FROM T WHERE (3 / 0) + CAST(NULL AS INTEGER) = 3 AND id > 0 ORDER BY id"},
			{"first_planning_a_explain", "EXPLAIN SELECT id FROM T WHERE (3 / 0) + CAST(NULL AS INTEGER) = 3 AND id > 0 ORDER BY id"},
			{"first_planning_b_explain", "EXPLAIN SELECT id FROM T WHERE (4 / 0) + CAST(NULL AS INTEGER) = 4 AND id >= 0 ORDER BY id"},
			{"first_planning_b_exec", "SELECT id FROM T WHERE (4 / 0) + CAST(NULL AS INTEGER) = 4 AND id >= 0 ORDER BY id"},
		} {
			o.plain(schema, setup, p.name, p.sql)
		}
		// The v4 statement over the v4 schema (seven tables), in this JVM.
		v4Schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE L (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE I (id BIGINT, i INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE F (id BIGINT, f FLOAT, PRIMARY KEY (id)) " +
			"CREATE TABLE D (id BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
			"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, m BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))"
		o.plain(v4Schema, setup, "v4_schema_div0_cast_null_eq_one_where", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1")
		o.plain(v4Schema, setup, "v4_schema_div0_cast_null_eq_one_explain", "EXPLAIN SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1")
		o.plain(schema, setup, "v5_schema_div0_cast_null_eq_one_where_first", "SELECT id FROM T WHERE (5 / 0) + CAST(NULL AS INTEGER) = 5")
		o.trace(v4Schema, setup, "trace_v4_schema_div0_cast_null_eq_one", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1",
			[]string{"QueryPredicateSimplificationRule"})
		for _, p := range []struct{ name, sql string }{
			// Predicate folding: which constant subtrees the target's predicate rule set
			// reduces, and whether an erroring sibling is then evaluated.
			{"fold_div0_or_not_false_where", "SELECT id FROM T WHERE (1 / 0 = 1) OR ((NOT FALSE) = TRUE) ORDER BY id"},
			{"fold_div0_or_not_false_where_explain", "EXPLAIN SELECT id FROM T WHERE (1 / 0 = 1) OR ((NOT FALSE) = TRUE)"},
			{"coalesce_div0_five_is_null_where", "SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NULL ORDER BY id"},
			{"coalesce_div0_five_is_null_where_explain", "EXPLAIN SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NULL"},
			{"null_strict_div0_cast_null_is_null_where", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) IS NULL ORDER BY id"},
			{"null_strict_div0_cast_null_is_null_where_explain", "EXPLAIN SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			{"coalesce_true_div0_and_column_where", "SELECT id FROM T WHERE COALESCE(TRUE, 1 / 0 = 1) AND n > 0 ORDER BY id"},
			{"coalesce_true_div0_and_column_where_explain", "EXPLAIN SELECT id FROM T WHERE COALESCE(TRUE, 1 / 0 = 1) AND n > 0"},
			{"coalesce_not_cast_null_head_where", "SELECT id FROM T WHERE COALESCE(NOT CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1) ORDER BY id"},
			{"coalesce_not_cast_null_head_where_explain", "EXPLAIN SELECT id FROM T WHERE COALESCE(NOT CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1)"},
			{"not_coalesce_false_div0_where", "SELECT id FROM T WHERE NOT COALESCE(FALSE, 1 / 0 = 1) ORDER BY id"},
			{"not_coalesce_false_div0_where_explain", "EXPLAIN SELECT id FROM T WHERE NOT COALESCE(FALSE, 1 / 0 = 1)"},
			// Nullability of a folded boolean in a projection.
			{"not_false_select", "SELECT NOT FALSE FROM T WHERE id = 1"},
			{"true_and_true_select", "SELECT TRUE AND TRUE FROM T WHERE id = 1"},
			{"not_column_comparison_select", "SELECT NOT (n > 1) FROM T WHERE id = 2"},
			// IN with a typed-NULL item outside a top-level positive conjunct: OR, NOT,
			// NOT IN, a projection, a CASE, the inner side of a join.
			{"in_cast_null_or_empty_table", "SELECT x FROM E WHERE x IN (CAST(NULL AS BIGINT)) OR id > 0"},
			{"in_cast_null_or_nonempty_table", "SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT)) OR id > 0 ORDER BY id"},
			{"in_cast_null_or_explain", "EXPLAIN SELECT id FROM T WHERE id IN (CAST(NULL AS BIGINT)) OR id > 0"},
			{"not_in_paren_cast_null_empty_table", "SELECT x FROM E WHERE NOT (x IN (CAST(NULL AS BIGINT)))"},
			{"not_in_paren_cast_null_nonempty_table", "SELECT id FROM T WHERE NOT (id IN (CAST(NULL AS BIGINT))) ORDER BY id"},
			{"not_in_cast_null_empty_table", "SELECT x FROM E WHERE x NOT IN (CAST(NULL AS BIGINT))"},
			{"not_in_cast_null_nonempty_table", "SELECT id FROM T WHERE id NOT IN (CAST(NULL AS BIGINT)) ORDER BY id"},
			{"not_in_cast_null_explain", "EXPLAIN SELECT id FROM T WHERE id NOT IN (CAST(NULL AS BIGINT))"},
			{"in_cast_null_projection_empty_table", "SELECT x IN (CAST(NULL AS BIGINT)) FROM E"},
			{"in_cast_null_projection_nonempty_table", "SELECT id IN (CAST(NULL AS BIGINT)) FROM T WHERE id = 1"},
			{"in_cast_null_case_nonempty_table", "SELECT CASE WHEN id IN (CAST(NULL AS BIGINT)) THEN 1 ELSE 0 END FROM T WHERE id = 1"},
			{"in_cast_null_join_inner_empty", "SELECT T.id FROM T, E WHERE E.x IN (CAST(NULL AS BIGINT))"},
			{"in_cast_null_join_inner_explain", "EXPLAIN SELECT T.id FROM T, E WHERE E.x IN (CAST(NULL AS BIGINT))"},
			// STRING lanes with a FLOAT or DOUBLE operand (Double.toString / Float.toString).
			{"add_string_double_select", "SELECT 'a' + 1.5 FROM T WHERE id = 1"},
			{"add_double_string_select", "SELECT 1.5 + 'a' FROM T WHERE id = 1"},
			{"add_string_float_select", "SELECT 'a' + 1.5f FROM T WHERE id = 1"},
			{"add_string_large_double_select", "SELECT 'a' + 10000000000.0 FROM T WHERE id = 1"},
			{"add_string_small_double_select", "SELECT 'a' + 0.0001 FROM T WHERE id = 1"},
			{"add_string_long_select", "SELECT 'a' + 3000000000 FROM T WHERE id = 1"},
		} {
			o.plain(schema, setup, p.name, p.sql)
		}

		// Which simplified alternative the predicate rule set produces: the exploration rule
		// QueryPredicateSimplificationRule yields the folded conjunction as an ALTERNATIVE, so
		// a comparison that keeps its null-strict arithmetic either never got one or lost.
		foldRules := []string{"QueryPredicateSimplificationRule"}
		for _, p := range []struct{ name, sql string }{
			{"trace_null_strict_div0_cast_null_eq_one", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1"},
			{"trace_null_strict_div0_cast_null_is_null", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			{"trace_null_strict_column_cast_null_eq_one", "SELECT id FROM T WHERE n + CAST(NULL AS BIGINT) = 1"},
			{"trace_coalesce_true_div0", "SELECT id FROM T WHERE COALESCE(TRUE, 1 / 0 = 1)"},
			{"trace_coalesce_not_head", "SELECT id FROM T WHERE COALESCE(NOT FALSE, 1 / 0 = 1)"},
			{"trace_coalesce_div0_five_is_null", "SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NULL"},
			{"trace_fold_div0_or_not_false", "SELECT id FROM T WHERE (1 / 0 = 1) OR ((NOT FALSE) = TRUE)"},
		} {
			o.trace(schema, setup, p.name, p.sql, foldRules)
		}
		// The same statement planned without a listener, in this run: EXPLAIN and execution.
		o.plain(schema, setup, "null_strict_div0_cast_null_eq_one_explain", "EXPLAIN SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1")
		o.plain(schema, setup, "null_strict_div0_cast_null_eq_one_where", "SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1")

		// Assignment into nested types: array elements and struct fields.
		nested := "CREATE TYPE AS STRUCT SF (f FLOAT) " +
			"CREATE TABLE AF (id BIGINT, fa FLOAT ARRAY, PRIMARY KEY (id)) " +
			"CREATE TABLE AL (id BIGINT, la BIGINT ARRAY, PRIMARY KEY (id)) " +
			"CREATE TABLE SR (id BIGINT, st SF, PRIMARY KEY (id))"
		const (
			readAF = "SELECT id, fa FROM AF ORDER BY id"
			readAL = "SELECT id, la FROM AL ORDER BY id"
			readSR = "SELECT id, st FROM SR ORDER BY id"
		)
		for _, c := range []wseCase{
			wseDML("insert_double_array_into_float_array", "INSERT INTO AF VALUES (1, [1.5])", readAF),
			wseDML("insert_double_expr_array_into_float_array", "INSERT INTO AF VALUES (2, [1.5 + 0.0])", readAF),
			wseDML("insert_int_array_into_float_array", "INSERT INTO AF VALUES (3, [1])", readAF),
			wseDML("insert_empty_array_into_float_array", "INSERT INTO AF VALUES (4, [])", readAF),
			wseDML("insert_long_array_into_bigint_array", "INSERT INTO AL VALUES (1, [3000000000])", readAL),
			wseDML("insert_double_array_into_bigint_array", "INSERT INTO AL VALUES (2, [1.5])", readAL),
			wseDML("insert_double_struct_field_into_float", "INSERT INTO SR VALUES (1, (1.5))", readSR),
			wseDML("insert_int_struct_field_into_float", "INSERT INTO SR VALUES (2, (1))", readSR),
		} {
			o.prepared(nested, nil, c)
		}
		// STRING into ENUM and UUID columns, literal and bound (separate schemas: Go cannot
		// declare the enum yet, WS-J F6, and must still be measured on UUID).
		enumSchema := "CREATE TYPE AS ENUM CLR ('RED', 'GREEN') CREATE TABLE EN (id BIGINT, c CLR, PRIMARY KEY (id))"
		for _, c := range []wseCase{
			wseDML("insert_string_literal_into_enum", "INSERT INTO EN VALUES (1, 'RED')", "SELECT id, c FROM EN ORDER BY id"),
			wseDML("insert_bound_string_into_enum", "INSERT INTO EN VALUES (2, ?)", "SELECT id, c FROM EN ORDER BY id", wseString("GREEN")),
			wseDML("insert_bad_string_into_enum", "INSERT INTO EN VALUES (3, 'BLUE')", "SELECT id, c FROM EN ORDER BY id"),
		} {
			o.prepared(enumSchema, nil, c)
		}
		uuidSchema := "CREATE TABLE U (id BIGINT, u UUID, PRIMARY KEY (id))"
		for _, c := range []wseCase{
			wseDML("insert_string_literal_into_uuid", "INSERT INTO U VALUES (1, '123e4567-e89b-12d3-a456-426614174000')", "SELECT id, u FROM U ORDER BY id"),
			wseDML("insert_bound_string_into_uuid", "INSERT INTO U VALUES (2, ?)", "SELECT id, u FROM U ORDER BY id", wseString("123e4567-e89b-12d3-a456-426614174001")),
			wseDML("insert_bad_string_into_uuid", "INSERT INTO U VALUES (3, 'not-a-uuid')", "SELECT id, u FROM U ORDER BY id"),
		} {
			o.prepared(uuidSchema, nil, c)
		}

		// Index-state read conflicts: a covering scan of X_V, a record scan, under both
		// isolation levels, against a state change of the scanned index and of an unused one.
		isSchema := "CREATE TABLE X (id BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE WR (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX X_V AS SELECT v FROM X ORDER BY v " +
			"CREATE INDEX X_W AS SELECT w FROM X ORDER BY w"
		isSetup := []string{"INSERT INTO X VALUES (1, 10, 100), (2, 20, 200)"}
		const snap = " OPTIONS (ISOLATION LEVEL SNAPSHOT)"
		const (
			indexRead  = "SELECT v FROM X WHERE v > 0"
			recordRead = "SELECT id, v, w FROM X WHERE id > 0"
			ownWrite   = "INSERT INTO WR VALUES (1)"
		)
		for _, c := range []struct{ name, read, index string }{
			{"index_state_scanned_serializable", indexRead, "X_V"},
			{"index_state_unused_serializable", indexRead, "X_W"},
			{"index_state_record_scan_serializable", recordRead, "X_V"},
			{"index_state_scanned_snapshot", indexRead + snap, "X_V"},
			{"index_state_unused_snapshot", indexRead + snap, "X_W"},
		} {
			o.indexStateScope(isSchema, isSetup, c.name, c.read, c.index, "DISABLED", ownWrite)
		}
		o.plain(isSchema, isSetup, "index_state_index_read_explain", "EXPLAIN "+indexRead)
		o.plain(isSchema, isSetup, "index_state_record_read_explain", "EXPLAIN "+recordRead)

		// Read-scope controls v4 lacked: a concurrent change of the INDEXED column under
		// a covering read, and the serializable record-scan update.
		rsSchema := "CREATE TABLE S (id BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE P (id BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE WR (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX S_V AS SELECT v FROM S ORDER BY v"
		rsSetup := []string{
			"INSERT INTO S VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300)",
			"INSERT INTO P VALUES (1, 100), (2, 200), (3, 300)",
		}
		for _, c := range []struct{ name, read, concurrent string }{
			{"read_scope_covering_serializable_update_indexed", "SELECT id FROM S", "UPDATE S SET v = 21 WHERE id = 2"},
			{"read_scope_covering_snapshot_update_indexed", "SELECT id FROM S" + snap, "UPDATE S SET v = 21 WHERE id = 2"},
			{"read_scope_scan_serializable_update", "SELECT id, w FROM P", "UPDATE P SET w = 201 WHERE id = 2"},
		} {
			o.readScope(rsSchema, rsSetup, c.name, c.read, c.concurrent, "INSERT INTO WR VALUES (1)")
		}
		o.check(wsE6Pins)
	})
})

var _ = Describe("WS-E target oracle v6", func() {
	It("records constant comparands, conjunction-level folds, NOT NULL typing under IS NULL, and UUID and ENUM string forms", func() {
		o, done := newWSEOracle("ws_e7_", "WS-E7")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (3, 'xyz', 7)"}
		foldRules := []string{"QueryPredicateSimplificationRule"}
		for _, p := range []struct{ name, sql string }{
			// A constant EXPRESSION as a comparand: whether the target folds it before
			// matching and whether the comparison stays sargable on the primary key.
			{"constant_expression_comparand_where", "SELECT id FROM T WHERE id = 1 + 2"},
			{"constant_expression_comparand_explain", "EXPLAIN SELECT id FROM T WHERE id = 1 + 2"},
			{"constant_expression_range_explain", "EXPLAIN SELECT id FROM T WHERE id > 3 - 2"},
			// Conjunction-level simplification: a fold to false or true beside another
			// conjunct (the rule simplifies the whole conjunction).
			{"conj_false_where", "SELECT id FROM T WHERE n > 0 AND COALESCE(1 / 0, 5) IS NULL ORDER BY id"},
			{"conj_false_explain", "EXPLAIN SELECT id FROM T WHERE n > 0 AND COALESCE(1 / 0, 5) IS NULL"},
			{"conj_true_where", "SELECT id FROM T WHERE n > 0 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL ORDER BY id"},
			{"conj_true_explain", "EXPLAIN SELECT id FROM T WHERE n > 0 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			// IS NULL over a value whose NULLABILITY decides it (EffectiveConstant's
			// NOT_NULL arm) while its evaluation would fail.
			{"is_null_cast_string_to_bigint_where", "SELECT id FROM T WHERE CAST('x' AS BIGINT) IS NULL ORDER BY id"},
			{"is_null_cast_div0_where", "SELECT id FROM T WHERE CAST(1 / 0 AS BIGINT) IS NULL ORDER BY id"},
			{"is_null_case_div0_condition_where", "SELECT id FROM T WHERE CASE WHEN 1 / 0 = 1 THEN 1 ELSE 2 END IS NULL ORDER BY id"},
			{"is_null_case_div0_branch_where", "SELECT id FROM T WHERE CASE WHEN id > 0 THEN 1 ELSE 1 / 0 END IS NULL ORDER BY id"},
			{"is_null_greatest_div0_where", "SELECT id FROM T WHERE GREATEST(1, 1 / 0) IS NULL ORDER BY id"},
			{"is_not_null_coalesce_div0_where", "SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NOT NULL ORDER BY id"},
			// What the CASE row above refuses: the CASE itself, and IS NULL over a CASE
			// with no erroring branch.
			{"case_div0_branch_select", "SELECT CASE WHEN id > 0 THEN 1 ELSE 1 / 0 END FROM T WHERE id = 1"},
			{"is_null_case_literals_where", "SELECT id FROM T WHERE CASE WHEN id > 0 THEN 1 ELSE 2 END IS NULL ORDER BY id"},
			{"case_literals_select", "SELECT CASE WHEN id > 0 THEN 1 ELSE 2 END FROM T WHERE id = 1"},
			// CAST to UUID goes through the same PromoteValue.stringToUuidValue.
			{"cast_uuid_short_components", "SELECT CAST('1-2-3-4-5' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_surrounding_space", "SELECT CAST(' 123e4567-e89b-12d3-a456-426614174000' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_wide_first_component", "SELECT CAST('123456789-1-1-1-1' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_braces", "SELECT CAST('{123e4567-e89b-12d3-a456-426614174000}' AS UUID) FROM T WHERE id = 1"},
			// The rest of Long.parseLong's radix-16 alphabet (Character.digit): a Unicode
			// decimal digit and a fullwidth hex letter; an empty component, a component
			// past 64 bits, and a string past 36 characters.
			{"cast_uuid_unicode_digit", "SELECT CAST('\u0661-2-3-4-5' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_fullwidth_hex", "SELECT CAST('\uFF21-2-3-4-5' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_empty_component", "SELECT CAST('1--3-4-5' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_component_past_64_bits", "SELECT CAST('1-2-3-4-10000000000000000' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_thirty_seven_chars", "SELECT CAST('123e4567-e89b-12d3-a456-4266141740000' AS UUID) FROM T WHERE id = 1"},
			// Long.parseLong's own range: 2^63 refused, 2^63 - 1 accepted (the last
			// component then masked to 48 bits); and a fifth dash, which fromString1
			// refuses (dash5 >= 0) even though parseLong would read "-5".
			{"cast_uuid_component_2_pow_63", "SELECT CAST('1-2-3-4-8000000000000000' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_component_long_max", "SELECT CAST('1-2-3-4-7fffffffffffffff' AS UUID) FROM T WHERE id = 1"},
			{"cast_uuid_fifth_dash", "SELECT CAST('1-2-3-4--5' AS UUID) FROM T WHERE id = 1"},
			// CAST's result nullability is its operand's (ExpressionVisitor.java:532): the
			// failing CAST evaluated, and the nullability of a CAST over a literal and over
			// a nullable column.
			{"cast_string_to_bigint_select", "SELECT CAST('x' AS BIGINT) FROM T WHERE id = 1"},
			{"cast_literal_nullability_select", "SELECT CAST(1 AS BIGINT) FROM T WHERE id = 1"},
			{"cast_column_nullability_select", "SELECT CAST(n AS INTEGER) FROM T WHERE id = 2"},
			// The CASE refusal's mechanism: PickValue demands EQUAL alternative types, and
			// the predicate value set's constant promotion turns the literal branch NOT
			// NULL beside a nullable arithmetic branch. With both branches arithmetic the
			// types stay equal.
			{"is_null_case_both_arithmetic_where", "SELECT id FROM T WHERE CASE WHEN id > 0 THEN 1 + 0 ELSE 1 / 0 END IS NULL ORDER BY id"},
			// An IN list with an item correlated to the select's own quantifier: the
			// explode depends on the scanned row, so it cannot be an IN-join source.
			{"in_own_column_item_where", "SELECT id FROM T WHERE n IN (id + 4, 999) ORDER BY id"},
			{"in_own_column_item_explain", "EXPLAIN SELECT id FROM T WHERE n IN (id + 4, 999)"},
		} {
			o.plain(schema, setup, p.name, p.sql)
		}
		for _, p := range []struct{ name, sql string }{
			{"trace_conj_false", "SELECT id FROM T WHERE n > 0 AND COALESCE(1 / 0, 5) IS NULL"},
			{"trace_conj_true", "SELECT id FROM T WHERE n > 0 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			{"trace_constant_expression_comparand", "SELECT id FROM T WHERE id = 1 + 2"},
		} {
			o.trace(schema, setup, p.name, p.sql, foldRules)
		}

		// STRING into UUID: the forms java.util.UUID.fromString accepts and google/uuid's
		// Parse does not, and the reverse, as a literal and bound.
		uuidSchema := "CREATE TABLE U (id BIGINT, u UUID, PRIMARY KEY (id))"
		readU := "SELECT id, u FROM U ORDER BY id"
		for i, c := range []struct{ name, text string }{
			{"short_components", "1-2-3-4-5"},
			{"uppercase", "123E4567-E89B-12D3-A456-426614174000"},
			{"braces", "{123e4567-e89b-12d3-a456-426614174000}"},
			{"urn", "urn:uuid:123e4567-e89b-12d3-a456-426614174000"},
			{"no_dashes", "123e4567e89b12d3a456426614174000"},
			{"plus_sign_component", "+1-2-3-4-5"},
			{"surrounding_space", " 123e4567-e89b-12d3-a456-426614174000"},
		} {
			o.prepared(uuidSchema, nil, wseDML("uuid_literal_"+c.name,
				fmt.Sprintf("INSERT INTO U VALUES (%d, '%s')", i+1, c.text), readU))
			o.prepared(uuidSchema, nil, wseDML("uuid_bound_"+c.name,
				fmt.Sprintf("INSERT INTO U VALUES (%d, ?)", i+1), readU, wseString(c.text)))
		}
		// STRING into ENUM: the match is by name, exact.
		enumSchema := "CREATE TYPE AS ENUM CLR ('RED', 'GREEN') CREATE TABLE EN (id BIGINT, c CLR, PRIMARY KEY (id))"
		readEN := "SELECT id, c FROM EN ORDER BY id"
		for _, c := range []wseCase{
			wseDML("enum_lowercase_literal", "INSERT INTO EN VALUES (1, 'red')", readEN),
			wseDML("enum_padded_literal", "INSERT INTO EN VALUES (2, 'RED ')", readEN),
			wseDML("enum_lowercase_bound", "INSERT INTO EN VALUES (3, ?)", readEN, wseString("green")),
		} {
			o.prepared(enumSchema, nil, c)
		}
		o.check(wsE7Pins)
	})
})

// wsE7Pins is the measured target outcome of every round-6 probe (4.14.2.0).
var wsE7Pins = map[string]string{
	"case_div0_branch_select":               "OK [INTEGER] [NULL] [[1]]",
	"case_literals_select":                  "OK [INTEGER] [NULL] [[1]]",
	"cast_column_nullability_select":        "OK [INTEGER] [NULL] [[5]]",
	"cast_literal_nullability_select":       "OK [BIGINT] [NOT NULL] [[1]]",
	"cast_string_to_bigint_select":          "ERROR 22F3H SemanticException \"Invalid cast operation Cannot cast string 'x' to LONG: For input string: \\\"x\\\"\"",
	"cast_uuid_braces":                      "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type {123e4567-e89b-12d3-a456-426614174000}\"",
	"cast_uuid_component_2_pow_63":          "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 1-2-3-4-8000000000000000\"",
	"cast_uuid_component_long_max":          "OK [OTHER] [NOT NULL] [[00000001-0002-0003-0004-ffffffffffff]]",
	"cast_uuid_component_past_64_bits":      "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 1-2-3-4-10000000000000000\"",
	"cast_uuid_empty_component":             "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 1--3-4-5\"",
	"cast_uuid_fifth_dash":                  "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 1-2-3-4--5\"",
	"cast_uuid_fullwidth_hex":               "OK [OTHER] [NOT NULL] [[0000000a-0002-0003-0004-000000000005]]",
	"cast_uuid_short_components":            "OK [OTHER] [NOT NULL] [[00000001-0002-0003-0004-000000000005]]",
	"cast_uuid_surrounding_space":           "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type  123e4567-e89b-12d3-a456-426614174000\"",
	"cast_uuid_thirty_seven_chars":          "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 123e4567-e89b-12d3-a456-4266141740000\"",
	"cast_uuid_unicode_digit":               "OK [OTHER] [NOT NULL] [[00000001-0002-0003-0004-000000000005]]",
	"cast_uuid_wide_first_component":        "OK [OTHER] [NOT NULL] [[23456789-0001-0001-0001-000000000001]]",
	"conj_false_explain":                    "OK EXPLAIN \"SCAN([IS T]) | FILTER false | MAP (_.ID AS ID)\"",
	"conj_false_where":                      "OK [BIGINT] [NULL] []",
	"conj_true_explain":                     "OK EXPLAIN \"SCAN([IS T]) | FILTER _.N GREATER_THAN promote(@c7 AS LONG) | MAP (_.ID AS ID)\"",
	"conj_true_where":                       "OK [BIGINT] [NULL] [[2] [3]]",
	"constant_expression_comparand_explain": "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 + @c9 AS LONG)]) | MAP (_.ID AS ID)\"",
	"constant_expression_comparand_where":   "OK [BIGINT] [NULL] [[3]]",
	"constant_expression_range_explain":     "OK EXPLAIN \"SCAN([IS T, [GREATER_THAN promote(@c7 - @c9 AS LONG)]]) | MAP (_.ID AS ID)\"",
	"enum_lowercase_bound":                  "ERROR XX000 SemanticException \"Invalid enum value for the enum type green\"",
	"enum_lowercase_literal":                "ERROR XX000 SemanticException \"Invalid enum value for the enum type red\"",
	"enum_padded_literal":                   "ERROR XX000 SemanticException \"Invalid enum value for the enum type RED \"",
	"in_own_column_item_explain":            "OK EXPLAIN \"SCAN([IS T]) | FLATMAP q0 -> { EXPLODE arrayDistinct(array(q0.ID + @c10, promote(@c12 AS LONG))) | FILTER q0.N EQUALS _ AS q1 RETURN (q0.ID AS ID) }\"",
	"in_own_column_item_where":              "OK [BIGINT] [NULL] [[3]]",
	"is_not_null_coalesce_div0_where":       "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"is_null_case_both_arithmetic_where":    "OK [BIGINT] [NULL] []",
	"is_null_case_div0_branch_where":        "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"is_null_case_div0_condition_where":     "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"is_null_case_literals_where":           "OK [BIGINT] [NULL] []",
	"is_null_cast_div0_where":               "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"is_null_cast_string_to_bigint_where":   "OK [BIGINT] [NULL] []",
	"is_null_greatest_div0_where":           "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"trace_conj_false":                      "TRACE EXPLAIN \"SCAN([IS T]) | FILTER false | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[false] semanticHash=1503357515 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=2 predicates=[q#.N [GREATER_THAN promote(@c7 AS LONG)], coalesce_int(@c11 / @c7, @c15) [IS_NULL]] semanticHash=1174156789 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_conj_true":                       "TRACE EXPLAIN \"SCAN([IS T]) | FILTER _.N GREATER_THAN promote(@c7 AS LONG) | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[q#.N [GREATER_THAN promote(@c7 AS LONG)]] semanticHash=1569283107 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=2 predicates=[q#.N [GREATER_THAN promote(@c7 AS LONG)], @c10 / @c7 + NULL [IS_NULL]] semanticHash=941872657 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_constant_expression_comparand":   "TRACE EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 + @c9 AS LONG)]) | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=1 finals=0 exploratory=0",
	"uuid_bound_braces":                     "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type {123e4567-e89b-12d3-a456-426614174000}\"",
	"uuid_bound_no_dashes":                  "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 123e4567e89b12d3a456426614174000\"",
	"uuid_bound_plus_sign_component":        "OK [BIGINT OTHER] [NULL NULL] [[6 00000001-0002-0003-0004-000000000005]] COUNT 1",
	"uuid_bound_short_components":           "OK [BIGINT OTHER] [NULL NULL] [[1 00000001-0002-0003-0004-000000000005]] COUNT 1",
	"uuid_bound_surrounding_space":          "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type  123e4567-e89b-12d3-a456-426614174000\"",
	"uuid_bound_uppercase":                  "OK [BIGINT OTHER] [NULL NULL] [[2 123e4567-e89b-12d3-a456-426614174000]] COUNT 1",
	"uuid_bound_urn":                        "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type urn:uuid:123e4567-e89b-12d3-a456-426614174000\"",
	"uuid_literal_braces":                   "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type {123e4567-e89b-12d3-a456-426614174000}\"",
	"uuid_literal_no_dashes":                "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type 123e4567e89b12d3a456426614174000\"",
	"uuid_literal_plus_sign_component":      "OK [BIGINT OTHER] [NULL NULL] [[6 00000001-0002-0003-0004-000000000005]] COUNT 1",
	"uuid_literal_short_components":         "OK [BIGINT OTHER] [NULL NULL] [[1 00000001-0002-0003-0004-000000000005]] COUNT 1",
	"uuid_literal_surrounding_space":        "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type  123e4567-e89b-12d3-a456-426614174000\"",
	"uuid_literal_uppercase":                "OK [BIGINT OTHER] [NULL NULL] [[2 123e4567-e89b-12d3-a456-426614174000]] COUNT 1",
	"uuid_literal_urn":                      "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type urn:uuid:123e4567-e89b-12d3-a456-426614174000\"",
}

// wsE6Pins is the measured target outcome of every round-5 probe (4.14.2.0).
var wsE6Pins = map[string]string{
	"add_double_string_select":                         "OK [STRING] [NULL] [[1.5a]]",
	"add_string_double_select":                         "OK [STRING] [NULL] [[a1.5]]",
	"add_string_float_select":                          "OK [STRING] [NULL] [[a1.5]]",
	"add_string_large_double_select":                   "OK [STRING] [NULL] [[a1.0E10]]",
	"add_string_long_select":                           "OK [STRING] [NULL] [[a3000000000]]",
	"add_string_small_double_select":                   "OK [STRING] [NULL] [[a1.0E-4]]",
	"coalesce_div0_five_is_null_where":                 "OK [BIGINT] [NULL] []",
	"coalesce_div0_five_is_null_where_explain":         "OK EXPLAIN \"SCAN([IS T]) | FILTER false | MAP (_.ID AS ID)\"",
	"coalesce_not_cast_null_head_where":                "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"coalesce_not_cast_null_head_where_explain":        "OK EXPLAIN \"SCAN([IS T]) | MAP (_.ID AS ID)\"",
	"coalesce_true_div0_and_column_where":              "ERROR XX000 VerifyException \"com.google.common.base.VerifyException\"",
	"coalesce_true_div0_and_column_where_explain":      "ERROR XX000 VerifyException \"com.google.common.base.VerifyException\"",
	"first_planning_a_exec":                            "OK [BIGINT] [NULL] []",
	"first_planning_a_explain":                         "OK EXPLAIN \"SCAN([IS T, [GREATER_THAN promote(@c8 AS LONG)]]) | FILTER null | MAP (_.ID AS ID)\"",
	"first_planning_b_exec":                            "OK [BIGINT] [NULL] []",
	"first_planning_b_explain":                         "OK EXPLAIN \"SCAN([IS T, [GREATER_THAN_OR_EQUALS promote(@c8 AS LONG)]]) | FILTER null | MAP (_.ID AS ID)\"",
	"fold_div0_or_not_false_where":                     "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"fold_div0_or_not_false_where_explain":             "OK EXPLAIN \"SCAN([IS T]) | FILTER promote(@c6 AS INT) EQUALS @c6 / @c8 OR promote(@c19 AS BOOLEAN) EQUALS NOT @c16 | MAP (_.ID AS ID)\"",
	"in_cast_null_case_nonempty_table":                 "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"in_cast_null_join_inner_empty":                    "OK [BIGINT] [NULL] []",
	"in_cast_null_join_inner_explain":                  "OK EXPLAIN \"SCAN([IS E]) | FLATMAP q0 -> { EXPLODE arrayDistinct(array(NULL)) | FLATMAP q1 -> { SCAN([IS T]) | FILTER q0.X EQUALS q1 AS q2 RETURN q2 } AS q3 RETURN (q3.ID AS ID) }\"",
	"in_cast_null_or_empty_table":                      "OK [BIGINT] [NULL] []",
	"in_cast_null_or_explain":                          "OK EXPLAIN \"[IN arrayDistinct(array(NULL)) SORTED] | INJOIN q0 -> { SCAN([IS T, EQUALS q0]) } ∪ SCAN([IS T, [GREATER_THAN promote(@c18 AS LONG)]]) COMPARE BY (_.ID) | MAP (_.ID AS ID)\"",
	"in_cast_null_or_nonempty_table":                   "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"in_cast_null_projection_empty_table":              "OK [BOOLEAN] [NULL] []",
	"in_cast_null_projection_nonempty_table":           "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"index_state_index_read_explain":                   "OK EXPLAIN \"COVERING(X_V [[GREATER_THAN promote(@c7 AS LONG)]] -> [ID: KEY:[2], V: KEY:[0]]) | MAP (_.V AS V)\"",
	"index_state_record_read_explain":                  "OK EXPLAIN \"SCAN([IS X, [GREATER_THAN promote(@c11 AS LONG)]])\"",
	"index_state_record_scan_serializable":             "READ OK [BIGINT BIGINT BIGINT] [NULL NULL NULL] [[1 10 100] [2 20 200]] COMMIT OK",
	"index_state_scanned_serializable":                 "READ OK [BIGINT] [NULL] [[10] [20]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"index_state_scanned_snapshot":                     "READ OK [BIGINT] [NULL] [[10] [20]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"index_state_unused_serializable":                  "READ OK [BIGINT] [NULL] [[10] [20]] COMMIT OK",
	"index_state_unused_snapshot":                      "READ OK [BIGINT] [NULL] [[10] [20]] COMMIT OK",
	"insert_bad_string_into_enum":                      "ERROR XX000 SemanticException \"Invalid enum value for the enum type BLUE\"",
	"insert_bad_string_into_uuid":                      "ERROR XX000 SemanticException \"Invalid UUID value for the UUID type not-a-uuid\"",
	"insert_bound_string_into_enum":                    "OK [BIGINT OTHER] [NULL NULL] [[2 GREEN]] COUNT 1",
	"insert_bound_string_into_uuid":                    "OK [BIGINT OTHER] [NULL NULL] [[2 123e4567-e89b-12d3-a456-426614174001]] COUNT 1",
	"insert_double_array_into_bigint_array":            "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_double_array_into_float_array":             "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_double_expr_array_into_float_array":        "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_double_struct_field_into_float":            "ERROR 22000 SemanticException \"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.\"",
	"insert_empty_array_into_float_array":              "OK [BIGINT ARRAY] [NULL NULL] [[4 []]] COUNT 1",
	"insert_int_array_into_float_array":                "OK [BIGINT ARRAY] [NULL NULL] [[3 [1]]] COUNT 1",
	"insert_int_struct_field_into_float":               "OK [BIGINT STRUCT] [NULL NULL] [[2 map[F:1]]] COUNT 1",
	"insert_long_array_into_bigint_array":              "OK [BIGINT ARRAY] [NULL NULL] [[1 [3e+09]]] COUNT 1",
	"insert_string_literal_into_enum":                  "OK [BIGINT OTHER] [NULL NULL] [[1 RED]] COUNT 1",
	"insert_string_literal_into_uuid":                  "OK [BIGINT OTHER] [NULL NULL] [[1 123e4567-e89b-12d3-a456-426614174000]] COUNT 1",
	"not_coalesce_false_div0_where":                    "ERROR XX000 VerifyException \"com.google.common.base.VerifyException\"",
	"not_coalesce_false_div0_where_explain":            "ERROR XX000 VerifyException \"com.google.common.base.VerifyException\"",
	"not_column_comparison_select":                     "OK [BOOLEAN] [NULL] [[false]]",
	"not_false_select":                                 "OK [BOOLEAN] [NULL] [[true]]",
	"not_in_cast_null_empty_table":                     "OK [BIGINT] [NULL] []",
	"not_in_cast_null_explain":                         "OK EXPLAIN \"SCAN([IS T]) | FILTER NOT _.ID IN array(NULL) | MAP (_.ID AS ID)\"",
	"not_in_cast_null_nonempty_table":                  "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"not_in_paren_cast_null_empty_table":               "OK [BIGINT] [NULL] []",
	"not_in_paren_cast_null_nonempty_table":            "ERROR 0A000 SemanticException \"The action is currently unsupported An ARRAY value cannot have NULL elements\"",
	"null_strict_div0_cast_null_eq_one_explain":        "OK EXPLAIN \"SCAN([IS T]) | FILTER null | MAP (_.ID AS ID)\"",
	"null_strict_div0_cast_null_eq_one_where":          "OK [BIGINT] [NULL] []",
	"null_strict_div0_cast_null_is_null_where":         "OK [BIGINT] [NULL] [[1] [2] [3]]",
	"null_strict_div0_cast_null_is_null_where_explain": "OK EXPLAIN \"SCAN([IS T]) | MAP (_.ID AS ID)\"",
	"read_scope_covering_serializable_update_indexed":  "READ OK [BIGINT] [NULL] [[1] [2] [3]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"read_scope_covering_snapshot_update_indexed":      "READ OK [BIGINT] [NULL] [[1] [2] [3]] COMMIT OK",
	"read_scope_scan_serializable_update":              "READ OK [BIGINT BIGINT] [NULL NULL] [[1 100] [2 200] [3 300]] COMMIT ERROR 40001 ContextualSQLException Transaction not committed due to conflict with another transaction",
	"trace_coalesce_div0_five_is_null":                 "TRACE EXPLAIN \"SCAN([IS T]) | FILTER false | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[coalesce_int(@c7 / @c9, @c11) [IS_NULL]] semanticHash=1104627478 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[false] semanticHash=-816198362 | REWRITING-COMPARE 0 vs 1 = 1",
	"trace_fold_div0_or_not_false":                     "TRACE EXPLAIN \"SCAN([IS T]) | FILTER promote(@c6 AS INT) EQUALS @c6 / @c8 OR promote(@c19 AS BOOLEAN) EQUALS NOT @c16 | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[@c6 EQUALS @c6 / @c8 OR 'true' EQUALS NOT 'false'] semanticHash=1145130613 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[promote(@c6 AS INT) EQUALS @c6 / @c8 OR promote(@c19 AS BOOLEAN) EQUALS NOT @c16] semanticHash=245992012 | REWRITING-COMPARE 0 vs 1 = 1",
	"trace_coalesce_not_head":                          "TRACE EXPLAIN \"SCAN([IS T]) | FILTER coalesce_boolean(NOT 'false', @c10 / @c12 equals @c10) EQUALS true | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[coalesce_boolean(NOT 'false', @c10 / @c12 equals @c10) [EQUALS true]] semanticHash=-1317608110 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[coalesce_boolean(NOT @c8, @c10 / @c12 equals @c10) [EQUALS true]] semanticHash=33718888 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_coalesce_true_div0":                         "TRACE EXPLAIN \"SCAN([IS T]) | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=0 predicates=[true] semanticHash=-816198548 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[coalesce_boolean(@c7, @c9 / @c11 equals @c9) [EQUALS true]] semanticHash=41870026 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_null_strict_column_cast_null_eq_one":        "TRACE EXPLAIN \"SCAN([IS T]) | FILTER null | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[null] semanticHash=-816236709 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[q#.N + NULL [EQUALS promote(@c14 AS LONG)]] semanticHash=1244780087 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_null_strict_div0_cast_null_eq_one":          "TRACE EXPLAIN \"SCAN([IS T]) | FILTER null | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[null] semanticHash=-816236709 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[promote(@c6 AS INT) [ AND EQUALS @c6 / @c8 + NULL]] semanticHash=2035668005 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_null_strict_div0_cast_null_is_null":         "TRACE EXPLAIN \"SCAN([IS T]) | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=0 predicates=[true] semanticHash=-816198548 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[@c6 / @c8 + NULL [IS_NULL]] semanticHash=872343346 | REWRITING-COMPARE 0 vs 1 = -1",
	"trace_v4_schema_div0_cast_null_eq_one":            "TRACE EXPLAIN \"SCAN([IS T]) | FILTER promote(@c6 AS INT) EQUALS @c6 / @c8 + NULL | MAP (_.ID AS ID)\"; QueryPredicateSimplificationRule calls=2 finals=0 exploratory=1; ranking: REWRITING-PRUNE members: 2 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[null] semanticHash=671289851 | REWRITING-MEMBER SelectExpression selects=1 conjuncts=1 predicates=[promote(@c6 AS INT) [ AND EQUALS @c6 / @c8 + NULL]] semanticHash=-771772731 | REWRITING-COMPARE 0 vs 1 = 1",
	"true_and_true_select":                             "OK [BOOLEAN] [NULL] [[true]]",
	"v4_schema_div0_cast_null_eq_one_explain":          "OK EXPLAIN \"SCAN([IS T]) | FILTER promote(@c6 AS INT) EQUALS @c6 / @c8 + NULL | MAP (_.ID AS ID)\"",
	"v4_schema_div0_cast_null_eq_one_where":            "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"v5_schema_div0_cast_null_eq_one_where_first":      "OK [BIGINT] [NULL] []",
}

// Round 8 of the WS-E target oracle, answering the design-v7 gates: WHO DECIDES a
// fold when an access path could separate the members. A fold that annuls the
// conjunction (to FALSE) or reduces it, beside a primary-key conjunct, a
// primary-key IN list and an indexed column's range and equality, each row with
// an erroring conjunct the annulment removes, so a plan that keeps the unfolded
// member raises it; and TIE folds (a NOT over a comparison, a duplicated OR),
// alone and beside a primary-key and an indexed conjunct, where the members have
// the same conjunct count and the target's hash decides, with their EXPLAIN.
var _ = Describe("WS-E target oracle v8", func() {
	It("records who decides a fold beside a primary-key, an IN-list and an indexed conjunct", func() {
		o, done := newWSEOracle("ws_e8_", "WS-E8")
		defer done()

		schema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX T_N AS SELECT n FROM T ORDER BY n"
		setup := []string{"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (5, 'xyz', 2)"}
		for _, p := range []struct{ name, sql string }{
			{"pk_annulling_fold_where", "SELECT id FROM T WHERE id = 5 AND 1 / 0 = 1 AND 1 = 2"},
			{"pk_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE id = 5 AND 1 / 0 = 1 AND 1 = 2"},
			{"pk_in_annulling_fold_where", "SELECT id FROM T WHERE id IN (1, 2) AND 1 / 0 = 1 AND 1 = 2"},
			{"pk_in_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE id IN (1, 2) AND 1 / 0 = 1 AND 1 = 2"},
			{"index_range_annulling_fold_where", "SELECT id FROM T WHERE n > 0 AND 1 / 0 = 1 AND 1 = 2"},
			{"index_range_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE n > 0 AND 1 / 0 = 1 AND 1 = 2"},
			{"index_eq_annulling_fold_where", "SELECT id FROM T WHERE n = 5 AND 1 / 0 = 1 AND 1 = 2"},
			{"index_eq_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE n = 5 AND 1 / 0 = 1 AND 1 = 2"},
			{"or_annulling_fold_where", "SELECT id FROM T WHERE (1 / 0 = 1 OR 1 = 2) AND 1 = 2"},
			{"pk_reducing_fold_where", "SELECT id FROM T WHERE id = 5 AND (1 = 1 OR 1 / 0 = 1)"},
			{"pk_reducing_fold_explain", "EXPLAIN SELECT id FROM T WHERE id = 5 AND (1 = 1 OR 1 / 0 = 1)"},
			{"index_tie_not_over_comparison_where", "SELECT id FROM T WHERE NOT (n > 3) ORDER BY id"},
			{"index_tie_not_over_comparison_explain", "EXPLAIN SELECT id FROM T WHERE NOT (n > 3)"},
			{"index_tie_duplicate_or_where", "SELECT id FROM T WHERE n = 5 OR n = 5"},
			{"index_tie_duplicate_or_explain", "EXPLAIN SELECT id FROM T WHERE n = 5 OR n = 5"},
			{"pk_beside_tie_fold_where", "SELECT id FROM T WHERE id = 5 AND NOT (n > 3)"},
			{"pk_beside_tie_fold_explain", "EXPLAIN SELECT id FROM T WHERE id = 5 AND NOT (n > 3)"},
			{"index_beside_tie_fold_where", "SELECT id FROM T WHERE n = 5 AND NOT (id > 3)"},
			{"index_beside_tie_fold_explain", "EXPLAIN SELECT id FROM T WHERE n = 5 AND NOT (id > 3)"},
			// The same shapes with folds the target's predicate set makes: a comparison
			// of literals is not one (each literal is a constant object value, not an
			// effective constant), while IS NULL over a NOT NULL COALESCE (annulment)
			// and over a null-strict collapse (reduction) are.
			{"pk_type_annulling_fold_where", "SELECT id FROM T WHERE id = 5 AND COALESCE(1 / 0, 5) IS NULL"},
			{"pk_type_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE id = 5 AND COALESCE(1 / 0, 5) IS NULL"},
			{"pk_in_type_annulling_fold_where", "SELECT id FROM T WHERE id IN (1, 2) AND COALESCE(1 / 0, 5) IS NULL"},
			{"pk_in_type_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE id IN (1, 2) AND COALESCE(1 / 0, 5) IS NULL"},
			{"index_range_type_annulling_fold_where", "SELECT id FROM T WHERE n > 0 AND COALESCE(1 / 0, 5) IS NULL"},
			{"index_range_type_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE n > 0 AND COALESCE(1 / 0, 5) IS NULL"},
			{"index_eq_type_annulling_fold_where", "SELECT id FROM T WHERE n = 5 AND COALESCE(1 / 0, 5) IS NULL"},
			{"index_eq_type_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE n = 5 AND COALESCE(1 / 0, 5) IS NULL"},
			{"pk_type_reducing_fold_where", "SELECT id FROM T WHERE id = 5 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			{"pk_type_reducing_fold_explain", "EXPLAIN SELECT id FROM T WHERE id = 5 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			{"index_eq_type_reducing_fold_where", "SELECT id FROM T WHERE n = 5 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
			{"index_eq_type_reducing_fold_explain", "EXPLAIN SELECT id FROM T WHERE n = 5 AND (1 / 0) + CAST(NULL AS INTEGER) IS NULL"},
		} {
			o.plain(schema, setup, p.name, p.sql)
		}
		o.check(wsE8Pins)
	})
})

// wsE8Pins is the measured target outcome of every round-8 probe (4.14.2.0).
var wsE8Pins = map[string]string{
	"index_beside_tie_fold_explain":           "OK EXPLAIN \"SCAN([IS T, [LESS_THAN_OR_EQUALS promote(@c13 AS LONG)]]) | FILTER _.N EQUALS promote(@c7 AS LONG) | MAP (_.ID AS ID)\"",
	"index_beside_tie_fold_where":             "OK [BIGINT] [NULL] [[2]]",
	"index_eq_annulling_fold_explain":         "OK EXPLAIN \"COVERING(T_N [EQUALS promote(@c7 AS LONG)] -> [ID: KEY:[2], N: KEY:[0]]) | FILTER promote(@c9 AS INT) EQUALS @c9 / @c11 AND @c17 EQUALS @c9 | MAP (_.ID AS ID)\"",
	"index_eq_annulling_fold_where":           "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"index_eq_type_annulling_fold_explain":    "OK EXPLAIN \"COVERING(T_N <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER false | MAP (_.ID AS ID)\"",
	"index_eq_type_annulling_fold_where":      "OK [BIGINT] [NULL] []",
	"index_eq_type_reducing_fold_explain":     "OK EXPLAIN \"COVERING(T_N [EQUALS promote(@c7 AS LONG)] -> [ID: KEY:[2], N: KEY:[0]]) | MAP (_.ID AS ID)\"",
	"index_eq_type_reducing_fold_where":       "OK [BIGINT] [NULL] [[2]]",
	"index_range_annulling_fold_explain":      "OK EXPLAIN \"COVERING(T_N [[GREATER_THAN promote(@c7 AS LONG)]] -> [ID: KEY:[2], N: KEY:[0]]) | FILTER promote(@c9 AS INT) EQUALS @c9 / @c7 AND @c17 EQUALS @c9 | MAP (_.ID AS ID)\"",
	"index_range_annulling_fold_where":        "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"index_range_type_annulling_fold_explain": "OK EXPLAIN \"COVERING(T_N <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER false | MAP (_.ID AS ID)\"",
	"index_range_type_annulling_fold_where":   "OK [BIGINT] [NULL] []",
	"index_tie_duplicate_or_explain":          "OK EXPLAIN \"COVERING(T_N <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER _.N EQUALS promote(@c7 AS LONG) OR _.N EQUALS promote(@c7 AS LONG) | MAP (_.ID AS ID)\"",
	"index_tie_duplicate_or_where":            "OK [BIGINT] [NULL] [[2]]",
	"index_tie_not_over_comparison_explain":   "OK EXPLAIN \"COVERING(T_N <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER NOT _.N GREATER_THAN promote(@c9 AS LONG) | MAP (_.ID AS ID)\"",
	"index_tie_not_over_comparison_where":     "OK [BIGINT] [NULL] [[5]]",
	"or_annulling_fold_where":                 "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"pk_annulling_fold_explain":               "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 AS LONG)]) | FILTER promote(@c9 AS INT) EQUALS @c9 / @c11 AND @c17 EQUALS @c9 | MAP (_.ID AS ID)\"",
	"pk_annulling_fold_where":                 "ERROR XXXXX ArithmeticException \"/ by zero\"",
	"pk_beside_tie_fold_explain":              "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 AS LONG)]) | FILTER _.N LESS_THAN_OR_EQUALS promote(@c13 AS LONG) | MAP (_.ID AS ID)\"",
	"pk_beside_tie_fold_where":                "OK [BIGINT] [NULL] [[5]]",
	"pk_in_annulling_fold_explain":            "OK EXPLAIN \"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG)))] | INJOIN q0 -> { SCAN([IS T, EQUALS q0]) | FILTER @c8 EQUALS @c8 / @c15 AND @c10 EQUALS @c8 | MAP (_.ID AS ID) }\"",
	"pk_in_annulling_fold_where":              "ERROR XXXXX VerifyException \"com.google.common.base.VerifyException\"",
	"pk_in_type_annulling_fold_explain":       "OK EXPLAIN \"COVERING(T_N <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER false | MAP (_.ID AS ID)\"",
	"pk_in_type_annulling_fold_where":         "OK [BIGINT] [NULL] []",
	"pk_reducing_fold_explain":                "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 AS LONG)]) | FILTER @c10 EQUALS @c10 OR promote(@c10 AS INT) EQUALS @c10 / @c16 | MAP (_.ID AS ID)\"",
	"pk_reducing_fold_where":                  "OK [BIGINT] [NULL] [[5]]",
	"pk_type_annulling_fold_explain":          "OK EXPLAIN \"COVERING(T_N <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER false | MAP (_.ID AS ID)\"",
	"pk_type_annulling_fold_where":            "OK [BIGINT] [NULL] []",
	"pk_type_reducing_fold_explain":           "OK EXPLAIN \"SCAN([IS T, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)\"",
	"pk_type_reducing_fold_where":             "OK [BIGINT] [NULL] [[5]]",
}

// Round 9 of the WS-E target oracle, answering the design-v8 gates: the plan of two
// IN lists over a two-column index (the explode route's nested_ins shape) and of two
// IN lists under a requested ordering; IN-list deduplication and equality over signed
// zeros and NaN, per row and through an index; a fold beside a UNIQUE index's
// equality (a non-unique index cannot prove one row) and inside a UNION ALL leg; a
// NULL temporal comparand; and GREATEST/LEAST over a DATE and a TIMESTAMP.
var _ = Describe("WS-E target oracle v9", func() {
	It("records nested IN plans, IN-list dedup over signed zeros and NaN, the unique and union-leg folds, and temporal NULL and variadic rows", func() {
		o, done := newWSEOracle("ws_e9_", "WS-E9")
		defer done()

		inSchema := "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, cat BIGINT, val BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX IDX_A AS SELECT a FROM T ORDER BY a " +
			"CREATE INDEX IDX_AB AS SELECT a, b FROM T ORDER BY a, b " +
			"CREATE INDEX IDX_VAL AS SELECT val FROM T ORDER BY val"
		inSetup := []string{"INSERT INTO T VALUES (1, 1, 5, 10, 100), (2, 1, 4, 20, 200), (3, 2, 5, 30, 300), (4, 2, 6, 40, 400), (5, 3, 5, 50, 500), (6, 1, 5, 20, 600)"}
		for _, p := range []struct{ name, sql string }{
			{"nested_ins_where", "SELECT id FROM T WHERE a IN (1, 2) AND b IN (5, 4) ORDER BY id"},
			{"nested_ins_explain", "EXPLAIN SELECT id FROM T WHERE a IN (1, 2) AND b IN (5, 4)"},
			{"two_in_ordered_where", "SELECT id FROM T WHERE val IN (200, 400, 600) AND cat IN (20, 40) ORDER BY val"},
			{"two_in_ordered_explain", "EXPLAIN SELECT id FROM T WHERE val IN (200, 400, 600) AND cat IN (20, 40) ORDER BY val"},
		} {
			o.plain(inSchema, inSetup, p.name, p.sql)
		}

		// F has an index on f; G is the same table with none (the per-row IN).
		fSchema := "CREATE TABLE F (id BIGINT, f DOUBLE, PRIMARY KEY (id)) CREATE INDEX F_F AS SELECT f FROM F ORDER BY f " +
			"CREATE TABLE G (id BIGINT, f DOUBLE, PRIMARY KEY (id))"
		fSetup := []string{
			"INSERT INTO F VALUES (1, 0.0), (2, -0.0), (3, 1.5), (4, CAST('NaN' AS DOUBLE))",
			"INSERT INTO G VALUES (1, 0.0), (2, -0.0), (3, 1.5), (4, CAST('NaN' AS DOUBLE))",
		}
		for _, p := range []struct{ name, sql string }{
			{"f_rows", "SELECT id, f FROM F ORDER BY id"},
			{"f_in_signed_zeros_index_where", "SELECT id FROM F WHERE f IN (-0.0, 0.0) ORDER BY id"},
			{"f_in_signed_zeros_index_explain", "EXPLAIN SELECT id FROM F WHERE f IN (-0.0, 0.0)"},
			{"f_in_signed_zeros_rows_where", "SELECT id FROM G WHERE f IN (-0.0, 0.0) ORDER BY id"},
			{"f_in_zero_twice_index_where", "SELECT id FROM F WHERE f IN (0.0, 0.0) ORDER BY id"},
			{"f_eq_zero_index_where", "SELECT id FROM F WHERE f = 0.0 ORDER BY id"},
			{"f_eq_negative_zero_index_where", "SELECT id FROM F WHERE f = -0.0 ORDER BY id"},
			{"f_eq_zero_rows_where", "SELECT id FROM G WHERE f = 0.0 ORDER BY id"},
			{"f_eq_negative_zero_rows_where", "SELECT id FROM G WHERE f = -0.0 ORDER BY id"},
			{"f_in_nan_twice_index_where", "SELECT id FROM F WHERE f IN (CAST('NaN' AS DOUBLE), CAST('NaN' AS DOUBLE)) ORDER BY id"},
			{"f_in_nan_twice_index_explain", "EXPLAIN SELECT id FROM F WHERE f IN (CAST('NaN' AS DOUBLE), CAST('NaN' AS DOUBLE))"},
			{"f_in_nan_twice_rows_where", "SELECT id FROM G WHERE f IN (CAST('NaN' AS DOUBLE), CAST('NaN' AS DOUBLE)) ORDER BY id"},
			{"f_eq_nan_index_where", "SELECT id FROM F WHERE f = CAST('NaN' AS DOUBLE) ORDER BY id"},
			{"f_eq_nan_rows_where", "SELECT id FROM G WHERE f = CAST('NaN' AS DOUBLE) ORDER BY id"},
		} {
			o.plain(fSchema, fSetup, p.name, p.sql)
		}

		uSchema := "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
			"CREATE UNIQUE INDEX T_UN AS SELECT n FROM T ORDER BY n"
		uSetup := []string{"INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a%b', 5), (5, 'xyz', 2)"}
		for _, p := range []struct{ name, sql string }{
			{"unique_eq_type_annulling_fold_where", "SELECT id FROM T WHERE n = 5 AND COALESCE(1 / 0, 5) IS NULL"},
			{"unique_eq_type_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE n = 5 AND COALESCE(1 / 0, 5) IS NULL"},
			{"union_leg_type_annulling_fold_where", "SELECT id FROM T WHERE id = 5 AND COALESCE(1 / 0, 5) IS NULL UNION ALL SELECT id FROM T WHERE id = 1"},
			{"union_leg_type_annulling_fold_explain", "EXPLAIN SELECT id FROM T WHERE id = 5 AND COALESCE(1 / 0, 5) IS NULL UNION ALL SELECT id FROM T WHERE id = 1"},
		} {
			o.plain(uSchema, uSetup, p.name, p.sql)
		}

		dSchema := "CREATE TABLE D (id BIGINT, d DATE, t TIMESTAMP, PRIMARY KEY (id)) CREATE INDEX D_D AS SELECT d FROM D ORDER BY d"
		dSetup := []string{"INSERT INTO D VALUES (1, CAST('2024-01-01' AS DATE), CAST('2024-01-01 10:00:00' AS TIMESTAMP)), (2, NULL, NULL)"}
		for _, p := range []struct{ name, sql string }{
			{"date_lt_null_timestamp_where", "SELECT id FROM D WHERE d < CAST(NULL AS TIMESTAMP) ORDER BY id"},
			{"date_not_lt_null_timestamp_where", "SELECT id FROM D WHERE NOT (d < CAST(NULL AS TIMESTAMP)) ORDER BY id"},
			{"greatest_date_timestamp_select", "SELECT GREATEST(d, t) FROM D WHERE id = 1"},
			{"least_date_timestamp_select", "SELECT LEAST(d, t) FROM D WHERE id = 1"},
		} {
			o.plain(dSchema, dSetup, p.name, p.sql)
		}
		for _, c := range []wseCase{
			wseQuery("date_lt_bound_null_timestamp", "SELECT id FROM D WHERE d < ? ORDER BY id", wseNull("TIMESTAMP")),
		} {
			o.prepared(dSchema, dSetup, c)
		}
		o.check(wsE9Pins)
	})
})

// wsE9Pins is the measured target outcome of every round-9 probe (4.14.2.0).
var wsE9Pins = map[string]string{
	"date_lt_bound_null_timestamp":          "ERROR 42F18 RelationalException \"could not find type 'DATE'\"",
	"date_lt_null_timestamp_where":          "ERROR 42F18 RelationalException \"could not find type 'DATE'\"",
	"date_not_lt_null_timestamp_where":      "ERROR 42F18 RelationalException \"could not find type 'DATE'\"",
	"f_eq_nan_index_where":                  "OK [BIGINT] [NULL] [[4]]",
	"f_eq_nan_rows_where":                   "OK [BIGINT] [NULL] [[4]]",
	"f_eq_negative_zero_index_where":        "OK [BIGINT] [NULL] [[2]]",
	"f_eq_negative_zero_rows_where":         "OK [BIGINT] [NULL] [[2]]",
	"f_eq_zero_index_where":                 "OK [BIGINT] [NULL] [[1]]",
	"f_eq_zero_rows_where":                  "OK [BIGINT] [NULL] [[1]]",
	"f_in_nan_twice_index_explain":          "OK EXPLAIN \"[IN arrayDistinct(array(CAST(@c10 AS DOUBLE), CAST(@c10 AS DOUBLE)))] | INJOIN q0 -> { COVERING(F_F [EQUALS q0] -> [F: KEY:[0], ID: KEY:[2]]) | MAP (_.ID AS ID) }\"",
	"f_in_nan_twice_index_where":            "OK [BIGINT] [NULL] [[4]]",
	"f_in_nan_twice_rows_where":             "OK [BIGINT] [NULL] [[4]]",
	"f_in_signed_zeros_index_explain":       "OK EXPLAIN \"[IN arrayDistinct(@c7)] | INJOIN q0 -> { COVERING(F_F [EQUALS q0] -> [F: KEY:[0], ID: KEY:[2]]) | MAP (_.ID AS ID) }\"",
	"f_in_signed_zeros_index_where":         "OK [BIGINT] [NULL] [[1] [2]]",
	"f_in_signed_zeros_rows_where":          "OK [BIGINT] [NULL] [[1] [2]]",
	"f_in_zero_twice_index_where":           "OK [BIGINT] [NULL] [[1]]",
	"f_rows":                                "OK [BIGINT DOUBLE] [NULL NULL] [[1 0] [2 0] [3 1.5] [4 NaN]]",
	"greatest_date_timestamp_select":        "ERROR 42F18 RelationalException \"could not find type 'DATE'\"",
	"least_date_timestamp_select":           "ERROR 42F18 RelationalException \"could not find type 'DATE'\"",
	"nested_ins_explain":                    "OK EXPLAIN \"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG)))] | INJOIN q0 -> { [IN arrayDistinct(promote(@c15 AS ARRAY(LONG)))] | INJOIN q1 -> { COVERING(IDX_AB [EQUALS q0, EQUALS q1] -> [A: KEY:[0], B: KEY:[1], ID: KEY:[3]]) | MAP (_.ID AS ID) } }\"",
	"nested_ins_where":                      "OK [BIGINT] [NULL] [[1] [2] [3] [6]]",
	"two_in_ordered_explain":                "OK EXPLAIN \"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG))) ⋈ IN arrayDistinct(promote(@c17 AS ARRAY(LONG)))] INUNION q0, q1 -> { ISCAN(IDX_VAL [EQUALS q0]) | FILTER _.CAT EQUALS q1 | MAP (_.ID AS ID, _.VAL AS VAL) } COMPARE BY (_.VAL, _.ID) | MAP (_.ID AS ID)\"",
	"two_in_ordered_where":                  "OK [BIGINT] [NULL] [[2] [4] [6]]",
	"union_leg_type_annulling_fold_explain": "OK EXPLAIN \"COVERING(T_UN <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER false | MAP (_.ID AS ID) ⊎ SCAN([IS T, EQUALS promote(@c11 AS LONG)]) | MAP (_.ID AS ID)\"",
	"union_leg_type_annulling_fold_where":   "OK [BIGINT] [NULL] [[1]]",
	"unique_eq_type_annulling_fold_explain": "OK EXPLAIN \"COVERING(T_UN <,> -> [ID: KEY:[2], N: KEY:[0]]) | FILTER false | MAP (_.ID AS ID)\"",
	"unique_eq_type_annulling_fold_where":   "OK [BIGINT] [NULL] []",
}

// Round 10 of the WS-E target oracle, answering the design-v9 gates: whether the
// target has a TIMESTAMP column type (round 9 measured DATE only) and whether it
// types DATE and TIMESTAMP VALUES without a column of that type; an equality on NaN
// at a NON-terminal index component, followed by another constrained component, and
// an ordering over the component after it; and a UNIQUE index over NaN.
var _ = Describe("WS-E target oracle v10", func() {
	It("records the TIMESTAMP column type, temporal values without such a column, NaN at a non-terminal index component, and a UNIQUE index over NaN", func() {
		o, done := newWSEOracle("ws_e10_", "WS-E10")
		defer done()

		tsSchema := "CREATE TABLE TS (id BIGINT, t TIMESTAMP, PRIMARY KEY (id))"
		tsSetup := []string{"INSERT INTO TS VALUES (1, CAST('2024-01-01 10:00:00' AS TIMESTAMP))"}
		o.plain(tsSchema, tsSetup, "timestamp_column_rows", "SELECT id, t FROM TS ORDER BY id")

		pSchema := "CREATE TABLE P (id BIGINT, s STRING, PRIMARY KEY (id))"
		pSetup := []string{"INSERT INTO P VALUES (1, '2024-01-01'), (2, '2024-02-30')"}
		for _, p := range []struct{ name, sql string }{
			{"cast_date_value_select", "SELECT CAST('2024-01-01' AS DATE) FROM P WHERE id = 1"},
			{"cast_timestamp_value_select", "SELECT CAST('2024-01-01 10:00:00' AS TIMESTAMP) FROM P WHERE id = 1"},
			{"cast_column_to_date_where", "SELECT id FROM P WHERE CAST(s AS DATE) < CAST('2025-01-01' AS DATE) ORDER BY id"},
		} {
			o.plain(pSchema, pSetup, p.name, p.sql)
		}

		fgSchema := "CREATE TABLE FG (id BIGINT, f DOUBLE, g BIGINT, PRIMARY KEY (id)) CREATE INDEX FG_FG AS SELECT f, g FROM FG ORDER BY f, g"
		fgSetup := []string{"INSERT INTO FG VALUES (1, CAST('NaN' AS DOUBLE), 2), (2, CAST('NaN' AS DOUBLE), 1), (3, 1.5, 1), (4, CAST('NaN' AS DOUBLE), 3)"}
		for _, p := range []struct{ name, sql string }{
			{"nan_then_eq_where", "SELECT id FROM FG WHERE f = CAST('NaN' AS DOUBLE) AND g = 1 ORDER BY id"},
			{"nan_then_eq_explain", "EXPLAIN SELECT id FROM FG WHERE f = CAST('NaN' AS DOUBLE) AND g = 1"},
			{"nan_in_then_in_where", "SELECT id FROM FG WHERE f IN (CAST('NaN' AS DOUBLE)) AND g IN (1, 2) ORDER BY id"},
			{"nan_in_then_in_explain", "EXPLAIN SELECT id FROM FG WHERE f IN (CAST('NaN' AS DOUBLE)) AND g IN (1, 2)"},
			{"nan_order_by_suffix_where", "SELECT id FROM FG WHERE f = CAST('NaN' AS DOUBLE) ORDER BY g"},
			{"nan_order_by_suffix_explain", "EXPLAIN SELECT id FROM FG WHERE f = CAST('NaN' AS DOUBLE) ORDER BY g"},
		} {
			o.plain(fgSchema, fgSetup, p.name, p.sql)
		}

		uSchema := "CREATE TABLE UF (id BIGINT, f DOUBLE, PRIMARY KEY (id)) CREATE UNIQUE INDEX UF_F AS SELECT f FROM UF ORDER BY f"
		o.plain(uSchema, []string{"INSERT INTO UF VALUES (1, CAST('NaN' AS DOUBLE))"}, "unique_nan_second_insert",
			"INSERT INTO UF VALUES (2, CAST('NaN' AS DOUBLE))")
		o.plain(uSchema, []string{"INSERT INTO UF VALUES (1, CAST('NaN' AS DOUBLE))"}, "unique_nan_eq_where",
			"SELECT id FROM UF WHERE f = CAST('NaN' AS DOUBLE)")
		o.check(wsE10Pins)
	})
})

// wsE10Pins is the measured target outcome of every round-10 probe (4.14.2.0).
var wsE10Pins = map[string]string{
	"cast_column_to_date_where":   "ERROR 42601 RelationalException \"syntax error:\\nSELECT id FROM P WHERE CAST(s AS DATE) < CAST('2025-01-01' AS DATE) ORDER BY id\\n                                 ^^^^\"",
	"cast_date_value_select":      "ERROR 42601 RelationalException \"syntax error:\\nSELECT CAST('2024-01-01' AS DATE) FROM P WHERE id = 1\\n                            ^^^^\"",
	"cast_timestamp_value_select": "ERROR 42601 RelationalException \"syntax error:\\nSELECT CAST('2024-01-01 10:00:00' AS TIMESTAMP) FROM P WHERE id = 1\\n                                     ^^^^^^^^^\"",
	"nan_in_then_in_explain":      "OK EXPLAIN \"[IN arrayDistinct(array(CAST(@c10 AS DOUBLE)))] | INJOIN q0 -> { [IN arrayDistinct(promote(@c18 AS ARRAY(LONG)))] | INJOIN q1 -> { COVERING(FG_FG [EQUALS q0, EQUALS q1] -> [F: KEY:[0], G: KEY:[1], ID: KEY:[3]]) | MAP (_.ID AS ID) } }\"",
	"nan_in_then_in_where":        "OK [BIGINT] [NULL] [[1] [2]]",
	"nan_order_by_suffix_explain": "OK EXPLAIN \"COVERING(FG_FG [EQUALS CAST(@c9 AS DOUBLE)] -> [F: KEY:[0], G: KEY:[1], ID: KEY:[3]]) | MAP (_.ID AS ID, _.G AS G) | MAP (_.ID AS ID)\"",
	"nan_order_by_suffix_where":   "OK [BIGINT] [NULL] [[2] [1] [4]]",
	"nan_then_eq_explain":         "OK EXPLAIN \"COVERING(FG_FG [EQUALS CAST(@c9 AS DOUBLE), EQUALS promote(@c16 AS LONG)] -> [F: KEY:[0], G: KEY:[1], ID: KEY:[3]]) | MAP (_.ID AS ID)\"",
	"nan_then_eq_where":           "OK [BIGINT] [NULL] [[2]]",
	"timestamp_column_rows":       "ERROR 42F18 RelationalException \"could not find type 'TIMESTAMP'\"",
	"unique_nan_eq_where":         "OK [BIGINT] [NULL] [[1]]",
	"unique_nan_second_insert":    "ERROR 23505 RecordIndexUniquenessViolation \"Duplicate entry for unique index\"",
}

// Round v11 (ws-e-design.md 4.2): the BITS of a NaN each engine writes. The
// target inserts NaNs made three ways into a kept store with an index on each
// column, and the raw index entries give each stored NaN's bits (the tuple
// encoding keeps them); the Go side reads the bits of the same expressions from
// its evaluator's results. The Go bits of the CAST differ today (a PENDING write
// divergence in DIVERGENCES.md, fixed with section 8 step (6)), and this round
// pins both, so the fix flips exactly its own pins.
var _ = Describe("WS-E target oracle v11", func() {
	It("records the bits of a NaN made by CAST and by division, stored by the target and evaluated by Go", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSE11_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		body := "CREATE TABLE T (id BIGINT, d DOUBLE, f FLOAT, PRIMARY KEY (id)) " +
			"CREATE INDEX T_D AS SELECT d FROM T ORDER BY d " +
			"CREATE INDEX T_F AS SELECT f FROM T ORDER BY f"
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		var store struct {
			DbPath     string `json:"dbPath"`
			SchemaName string `json:"schemaName"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()

		got := map[string]string{}
		inserts := []struct{ name, sql string }{
			{"insert_cast_nan", "INSERT INTO T VALUES (1, CAST('NaN' AS DOUBLE), CAST('NaN' AS FLOAT))"},
			{"insert_div_nan", "INSERT INTO T VALUES (2, 0.0 / 0.0, 0.0 / 0.0)"},
			{"insert_float_div_nan", "INSERT INTO T VALUES (3, 0.0 / 0.0, 0.0f / 0.0f)"},
		}
		for _, in := range inserts {
			var out struct {
				Outcome string `json:"outcome"`
			}
			Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "sql": in.sql,
			}, &out)).To(Succeed())
			got["java_"+in.name] = out.Outcome
		}
		// Each entry is (value, T's record type key, id); the value's bits are the
		// target's stored bits.
		for _, index := range []string{"T_D", "T_F"} {
			var out struct {
				Entries []struct {
					Tuple string `json:"tuple"`
					Hex   string `json:"hex"`
				} `json:"entries"`
			}
			Expect(java.InvokeAs(ctx, "wsjIndexEntriesJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "indexName": index,
			}, &out)).To(Succeed())
			for _, e := range out.Entries {
				raw, err := hex.DecodeString(e.Hex)
				Expect(err).NotTo(HaveOccurred())
				t, err := tuple.Unpack(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(t).To(HaveLen(3), e.Tuple)
				var bits string
				switch v := t[0].(type) {
				case float64:
					bits = fmt.Sprintf("%016x", math.Float64bits(v))
				case float32:
					bits = fmt.Sprintf("%08x", math.Float32bits(v))
				default:
					bits = fmt.Sprintf("%T %v", v, v)
				}
				got[fmt.Sprintf("java_%s_id%d", strings.ToLower(index), t[2])] = bits
			}
		}

		// Go's evaluator over the same expressions, through Go's database/sql driver
		// on a Go-created schema of the same table: the scanned values keep their
		// bits (the plandiff runner renders them as text, which does not).
		goClusterFile := writeClusterFileToTemp(clusterFile)
		defer func() { _ = os.Remove(goClusterFile) }()
		suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		goTemplate, goDB, goSchema := "WSE11_GO_"+suffix, "/WSE11_GO_"+suffix, "S_"+suffix
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", goClusterFile))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		_, err = sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+goTemplate+" CREATE TABLE T (id BIGINT, d DOUBLE, f FLOAT, PRIMARY KEY (id))")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goTemplate) }()
		_, err = sysDB.ExecContext(ctx, "CREATE DATABASE "+goDB)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+goDB) }()
		_, err = sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", goDB, goSchema, goTemplate))
		Expect(err).NotTo(HaveOccurred())
		schemaDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", goDB, goClusterFile, goSchema))
		Expect(err).NotTo(HaveOccurred())
		defer schemaDB.Close()
		render := func(v any) string {
			switch x := v.(type) {
			case float64:
				return fmt.Sprintf("%016x", math.Float64bits(x))
			case float32:
				return fmt.Sprintf("%08x", math.Float32bits(x))
			}
			return fmt.Sprintf("%T %v", v, v)
		}
		for i, in := range inserts {
			if _, err := schemaDB.ExecContext(ctx, in.sql); err != nil {
				got["go_"+in.name] = "ERROR " + err.Error()
				continue
			}
			var d, f any
			Expect(schemaDB.QueryRowContext(ctx, "SELECT d, f FROM T WHERE id = ?", int64(i+1)).Scan(&d, &f)).To(Succeed())
			got["go_"+in.name] = render(d) + " " + render(f)
		}
		for _, k := range sortedStringKeys(got) {
			fmt.Fprintf(GinkgoWriter, "WS-E11-PIN %q: %q,\n", k, got[k])
		}
		Expect(sortedStringKeys(got)).To(Equal(sortedStringKeys(wsE11Pins)), "every probe is pinned and every pin is probed")
		wseRequireNaNPinArch()
		for _, k := range sortedStringKeys(got) {
			Expect(got[k]).To(Equal(wsE11Pins[k]), k)
		}
	})
})

// wsE11Pins is the measured outcome of every round-11 probe: the target's (4.14.2.0)
// stored NaN bits per index entry, and Go's evaluator bits for the same
// expressions on this tree (a FLOAT column reads back widened to float64, so its
// Go pin is the widened bits). The target's CAST gives the canonical quiet NaNs,
// 0x7ff8000000000000 and 0x7fc00000; Go's CAST to DOUBLE gives math.NaN()'s
// 0x7ff8000000000001 (the PENDING divergence; step (6) makes it the target's, and
// this pin flips with it). A NaN from a division is the hardware's, the negative
// quiet NaN 0xfff8000000000000 on this machine, in both engines. The target
// refuses a DOUBLE expression into the FLOAT column (22000) where Go stores it,
// the assignment lattice of 4.3, also fixed in step (6).
var wsE11Pins = map[string]string{
	"go_insert_cast_nan":        "7ff8000000000001 7ff8000000000000",
	"go_insert_div_nan":         "fff8000000000000 fff8000000000000",
	"go_insert_float_div_nan":   "fff8000000000000 fff8000000000000",
	"java_insert_cast_nan":      "OK 1",
	"java_insert_div_nan":       "ERROR 22000 SemanticException A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.",
	"java_insert_float_div_nan": "OK 1",
	"java_t_d_id1":              "7ff8000000000000",
	"java_t_d_id3":              "fff8000000000000",
	"java_t_f_id1":              "7fc00000",
	"java_t_f_id3":              "ffc00000",
}

// Round v12 (RFC-257 WS-E design v12): the target's NaN PRODUCERS beyond the CAST and the
// division. (1) MIN and MAX over a column holding the division's NaN: the target's
// MIN_D/MAX_D are Math.min/Math.max, which return the NaN operand, where Go's aggregate
// returns math.NaN(); each result is written by INSERT ... SELECT into an indexed column
// and read back as stored bits. (2) The string-to-DOUBLE CAST's grammar: the target's
// Double.parseDouble against Go's strconv.ParseFloat, one spelling per row, each stored
// through an index so the stored bits (or the refusal) are the outcome.
var _ = Describe("WS-E target oracle v12", func() {
	It("records the bits MIN and MAX give a NaN, and which DOUBLE spellings each engine's CAST accepts", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSE12_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		const body = "CREATE TABLE T (id BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
			"CREATE TABLE U (id BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
			"CREATE INDEX T_D AS SELECT d FROM T ORDER BY d " +
			"CREATE INDEX U_D AS SELECT d FROM U ORDER BY d"
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		var store struct {
			DbPath     string `json:"dbPath"`
			SchemaName string `json:"schemaName"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()

		// The statements, in order: the aggregate inputs, the aggregates written into U,
		// and one CAST spelling per row of T from id 100 on.
		spellings := []string{
			"NaN", "-NaN", "+NaN", "nan", "NAN", "Infinity", "-Infinity", "+Infinity", "inf",
			"infinity", "1.5d", "1.5D", "1.5f", "1.5F", " 1.5 ", "0x1p3", "0x1.8p1", "1_000", "1e400", "1e-400", ".5", "5.",
		}
		type stmt struct{ name, sql string }
		stmts := []stmt{
			{"insert_div_nan", "INSERT INTO T VALUES (1, 0.0 / 0.0)"},
			{"insert_one", "INSERT INTO T VALUES (2, 1.0)"},
			{"insert_min", "INSERT INTO U SELECT 10, MIN(d) FROM T"},
			{"insert_max", "INSERT INTO U SELECT 11, MAX(d) FROM T"},
		}
		for i, sp := range spellings {
			stmts = append(stmts, stmt{fmt.Sprintf("cast_%02d", i), fmt.Sprintf("INSERT INTO T VALUES (%d, CAST('%s' AS DOUBLE))", 100+i, sp)})
		}
		got := map[string]string{}
		for _, s := range stmts {
			var out struct {
				Outcome string `json:"outcome"`
			}
			Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "sql": s.sql,
			}, &out)).To(Succeed())
			got["java_"+s.name] = out.Outcome
		}
		// The stored bits, per (index, id).
		for _, index := range []string{"T_D", "U_D"} {
			var out struct {
				Entries []struct {
					Tuple string `json:"tuple"`
					Hex   string `json:"hex"`
				} `json:"entries"`
			}
			Expect(java.InvokeAs(ctx, "wsjIndexEntriesJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "indexName": index,
			}, &out)).To(Succeed())
			for _, e := range out.Entries {
				raw, err := hex.DecodeString(e.Hex)
				Expect(err).NotTo(HaveOccurred())
				t, err := tuple.Unpack(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(t).To(HaveLen(3), e.Tuple)
				bits := fmt.Sprintf("%T %v", t[0], t[0])
				if v, ok := t[0].(float64); ok {
					bits = fmt.Sprintf("%016x", math.Float64bits(v))
				}
				got[fmt.Sprintf("java_%s_id%d", strings.ToLower(index), t[2])] = bits
			}
		}

		// Go's evaluator over the same statements, on a Go-created schema.
		goClusterFile := writeClusterFileToTemp(clusterFile)
		defer func() { _ = os.Remove(goClusterFile) }()
		suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		goTemplate, goDB, goSchema := "WSE12_GO_"+suffix, "/WSE12_GO_"+suffix, "S_"+suffix
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", goClusterFile))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		_, err = sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+goTemplate+" "+body)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goTemplate) }()
		_, err = sysDB.ExecContext(ctx, "CREATE DATABASE "+goDB)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+goDB) }()
		_, err = sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", goDB, goSchema, goTemplate))
		Expect(err).NotTo(HaveOccurred())
		schemaDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", goDB, goClusterFile, goSchema))
		Expect(err).NotTo(HaveOccurred())
		defer schemaDB.Close()
		for _, s := range stmts {
			res, err := schemaDB.ExecContext(ctx, s.sql)
			if err != nil {
				msg := err.Error()
				if r := []rune(msg); len(r) > 160 {
					msg = string(r[:160]) + "…"
				}
				got["go_"+s.name] = "ERROR " + msg
				continue
			}
			n, err := res.RowsAffected()
			Expect(err).NotTo(HaveOccurred())
			got["go_"+s.name] = fmt.Sprintf("OK %d", n)
		}
		for _, table := range []string{"T", "U"} {
			rows, err := schemaDB.QueryContext(ctx, "SELECT id, d FROM "+table)
			Expect(err).NotTo(HaveOccurred())
			for rows.Next() {
				var id int64
				var d any
				Expect(rows.Scan(&id, &d)).To(Succeed())
				bits := fmt.Sprintf("%T %v", d, d)
				if v, ok := d.(float64); ok {
					bits = fmt.Sprintf("%016x", math.Float64bits(v))
				}
				got[fmt.Sprintf("go_%s_id%d", strings.ToLower(table), id)] = bits
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(rows.Close()).To(Succeed())
		}
		for i, sp := range spellings {
			fmt.Fprintf(GinkgoWriter, "WS-E12-SPELLING cast_%02d = %q\n", i, sp)
		}
		for _, k := range sortedStringKeys(got) {
			fmt.Fprintf(GinkgoWriter, "WS-E12-PIN %q: %q,\n", k, got[k])
		}
		Expect(sortedStringKeys(got)).To(Equal(sortedStringKeys(wsE12Pins)), "every probe is pinned and every pin is probed")
		wseRequireNaNPinArch()
		for _, k := range sortedStringKeys(got) {
			Expect(got[k]).To(Equal(wsE12Pins[k]), k)
		}
	})
})

// wsE12Pins is the measured outcome of every round-12 probe (captured from
// /var/tmp/fdb-upgrade-recovery/wse12-cap1.log). The target's MIN and MAX of a column
// holding the division's NaN store that NaN's own bits (fff8000000000000), Math.min and
// Math.max returning the NaN operand; Go's store math.NaN()'s 7ff8000000000001. The
// target's CAST is Double.parseDouble: a signed "NaN" (canonical bits either sign),
// "Infinity" with a sign, the d/D/f/F suffixes, hex floats, and 1e400 as +Infinity are
// accepted, and "nan", "inf", "infinity" and "1_000" refused; Go's strconv.ParseFloat
// gives the reverse on every one of those, and math.NaN()'s bits for the NaNs it
// accepts. Section 4.1(b) of ws-e-design.md ports both; these pins flip with it.
var wsE12Pins = map[string]string{
	"go_cast_00":          "OK 1",
	"go_cast_01":          "ERROR 22F3H: Cannot cast string '-NaN' to DOUBLE: strconv.ParseFloat: parsing \"-NaN\": invalid syntax",
	"go_cast_02":          "ERROR 22F3H: Cannot cast string '+NaN' to DOUBLE: strconv.ParseFloat: parsing \"+NaN\": invalid syntax",
	"go_cast_03":          "OK 1",
	"go_cast_04":          "OK 1",
	"go_cast_05":          "OK 1",
	"go_cast_06":          "OK 1",
	"go_cast_07":          "OK 1",
	"go_cast_08":          "OK 1",
	"go_cast_09":          "OK 1",
	"go_cast_10":          "ERROR 22F3H: Cannot cast string '1.5d' to DOUBLE: strconv.ParseFloat: parsing \"1.5d\": invalid syntax",
	"go_cast_11":          "ERROR 22F3H: Cannot cast string '1.5D' to DOUBLE: strconv.ParseFloat: parsing \"1.5D\": invalid syntax",
	"go_cast_12":          "ERROR 22F3H: Cannot cast string '1.5f' to DOUBLE: strconv.ParseFloat: parsing \"1.5f\": invalid syntax",
	"go_cast_13":          "ERROR 22F3H: Cannot cast string '1.5F' to DOUBLE: strconv.ParseFloat: parsing \"1.5F\": invalid syntax",
	"go_cast_14":          "OK 1",
	"go_cast_15":          "OK 1",
	"go_cast_16":          "OK 1",
	"go_cast_17":          "OK 1",
	"go_cast_18":          "ERROR 22F3H: Cannot cast string '1e400' to DOUBLE: strconv.ParseFloat: parsing \"1e400\": value out of range",
	"go_cast_19":          "OK 1",
	"go_cast_20":          "OK 1",
	"go_cast_21":          "OK 1",
	"go_insert_div_nan":   "OK 1",
	"go_insert_max":       "OK 1",
	"go_insert_min":       "OK 1",
	"go_insert_one":       "OK 1",
	"go_t_id1":            "fff8000000000000",
	"go_t_id100":          "7ff8000000000001",
	"go_t_id103":          "7ff8000000000001",
	"go_t_id104":          "7ff8000000000001",
	"go_t_id105":          "7ff0000000000000",
	"go_t_id106":          "fff0000000000000",
	"go_t_id107":          "7ff0000000000000",
	"go_t_id108":          "7ff0000000000000",
	"go_t_id109":          "7ff0000000000000",
	"go_t_id114":          "3ff8000000000000",
	"go_t_id115":          "4020000000000000",
	"go_t_id116":          "4008000000000000",
	"go_t_id117":          "408f400000000000",
	"go_t_id119":          "0000000000000000",
	"go_t_id120":          "3fe0000000000000",
	"go_t_id121":          "4014000000000000",
	"go_t_id2":            "3ff0000000000000",
	"go_u_id10":           "7ff8000000000001",
	"go_u_id11":           "7ff8000000000001",
	"java_cast_00":        "OK 1",
	"java_cast_01":        "OK 1",
	"java_cast_02":        "OK 1",
	"java_cast_03":        "ERROR 22F3H SemanticException Invalid cast operation Cannot cast string 'nan' to DOUBLE: For input string: \"nan\"",
	"java_cast_04":        "ERROR 22F3H SemanticException Invalid cast operation Cannot cast string 'NAN' to DOUBLE: For input string: \"NAN\"",
	"java_cast_05":        "OK 1",
	"java_cast_06":        "OK 1",
	"java_cast_07":        "OK 1",
	"java_cast_08":        "ERROR 22F3H SemanticException Invalid cast operation Cannot cast string 'inf' to DOUBLE: For input string: \"inf\"",
	"java_cast_09":        "ERROR 22F3H SemanticException Invalid cast operation Cannot cast string 'infinity' to DOUBLE: For input string: \"infinity\"",
	"java_cast_10":        "OK 1",
	"java_cast_11":        "OK 1",
	"java_cast_12":        "OK 1",
	"java_cast_13":        "OK 1",
	"java_cast_14":        "OK 1",
	"java_cast_15":        "OK 1",
	"java_cast_16":        "OK 1",
	"java_cast_17":        "ERROR 22F3H SemanticException Invalid cast operation Cannot cast string '1_000' to DOUBLE: For input string: \"1_000\"",
	"java_cast_18":        "OK 1",
	"java_cast_19":        "OK 1",
	"java_cast_20":        "OK 1",
	"java_cast_21":        "OK 1",
	"java_insert_div_nan": "OK 1",
	"java_insert_max":     "OK 1",
	"java_insert_min":     "OK 1",
	"java_insert_one":     "OK 1",
	"java_t_d_id1":        "fff8000000000000",
	"java_t_d_id100":      "7ff8000000000000",
	"java_t_d_id101":      "7ff8000000000000",
	"java_t_d_id102":      "7ff8000000000000",
	"java_t_d_id105":      "7ff0000000000000",
	"java_t_d_id106":      "fff0000000000000",
	"java_t_d_id107":      "7ff0000000000000",
	"java_t_d_id110":      "3ff8000000000000",
	"java_t_d_id111":      "3ff8000000000000",
	"java_t_d_id112":      "3ff8000000000000",
	"java_t_d_id113":      "3ff8000000000000",
	"java_t_d_id114":      "3ff8000000000000",
	"java_t_d_id115":      "4020000000000000",
	"java_t_d_id116":      "4008000000000000",
	"java_t_d_id118":      "7ff0000000000000",
	"java_t_d_id119":      "0000000000000000",
	"java_t_d_id120":      "3fe0000000000000",
	"java_t_d_id121":      "4014000000000000",
	"java_t_d_id2":        "3ff0000000000000",
	"java_u_d_id10":       "fff8000000000000",
	"java_u_d_id11":       "fff8000000000000",
}

// Round v12, planning cost: the target's task count, per planner phase, for the IN-list
// family whose cost the WS-E prototype measured growing with the number of lists (ws-e-design.md
// 4.1(b)). The target's relational layer sets no task limit, so what compares across the engines
// is the growth as lists are added, not the absolute count (the engines' tasks are not the same
// units).
var _ = Describe("WS-E target oracle v12 planning cost", func() {
	It("records the target's planner task count for one to four IN lists and for repeated conjuncts", func() {
		// A fifth list is left out: the target did not finish planning it within the
		// invoker's two-minute HTTP timeout in the one run that tried
		// (/var/tmp/fdb-upgrade-recovery/wse12-cost-cap2.log), and a wall-clock bound is
		// not a pin.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		java := NewJavaInvoker()
		const sweep = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) " +
			"CREATE INDEX t_a AS SELECT a FROM t ORDER BY a CREATE INDEX t_ab AS SELECT a, b FROM t ORDER BY a, b " +
			"CREATE INDEX t_c AS SELECT c FROM t ORDER BY c CREATE INDEX t_s AS SELECT s FROM t ORDER BY s"
		const seven = "CREATE TABLE T (id BIGINT, val BIGINT, cat BIGINT, PRIMARY KEY (id)) CREATE INDEX IDX_VAL AS SELECT val FROM T ORDER BY val"
		p := "id IN (0, 1, -2) AND (NOT f AND a IN (-2, 0, 3))"
		probes := []struct{ name, schema, sql string }{
			{"in2_conjunct", sweep, "SELECT id FROM t WHERE (" + p + ") ORDER BY id"},
			{"in2_conjunct_twice", sweep, "SELECT id FROM t WHERE (" + p + ") AND (" + p + ") ORDER BY id"},
			{"in2_conjunct_thrice", sweep, "SELECT id FROM t WHERE (" + p + ") AND (" + p + ") AND (" + p + ") ORDER BY id"},
			{"in2", sweep, "SELECT id FROM t WHERE id IN (0, 1, -2) AND a IN (-2, 0, 3) ORDER BY id"},
			{"in3", sweep, "SELECT id FROM t WHERE id IN (0, 1, -2) AND a IN (-2, 0, 3) AND b IN (1, 2) ORDER BY id"},
			{"in4", sweep, "SELECT id FROM t WHERE id IN (0, 1, -2) AND a IN (-2, 0, 3) AND b IN (1, 2) AND s IN ('x', 'y') ORDER BY id"},
			{"in_id_twice", sweep, "SELECT id FROM t WHERE id IN (0, 1, -2) AND id IN (0, 1, -2) ORDER BY id"},
			{"in_a_twice", sweep, "SELECT id FROM t WHERE a IN (-2, 0, 3) AND a IN (-2, 0, 3) ORDER BY id"},
			{"in7_two_lists", seven, "SELECT id FROM T WHERE val IN (200, 400, 600) AND cat IN (20, 40) ORDER BY val"},
		}
		got := map[string]string{}
		for _, pr := range probes {
			var trace struct {
				Explain string         `json:"explain"`
				Tasks   map[string]int `json:"tasksPerPhase"`
				Kinds   map[string]int `json:"tasksPerKind"`
			}
			line := ""
			if jerr := java.InvokeAs(ctx, "planRuleTrace", map[string]any{
				"clusterFile": clusterFile, "schemaTemplate": pr.schema, "setupSqls": []string{},
				"querySql": pr.sql, "rules": []string{"TASK-COUNT"},
			}, &trace); jerr != nil {
				line = "ERROR " + jerr.Error()
			} else {
				names := make([]string, 0, len(trace.Tasks))
				for ph := range trace.Tasks {
					names = append(names, ph)
				}
				sort.Strings(names)
				var phases []string
				total := 0
				for _, ph := range names {
					phases = append(phases, fmt.Sprintf("%s=%d", ph, trace.Tasks[ph]))
					total += trace.Tasks[ph]
				}
				line = fmt.Sprintf("tasks=%d %s explain=%q", total, strings.Join(phases, " "), trace.Explain)
				// The breakdown by task class and rule is printed, not pinned: it locates
				// where each engine spends its tasks (ws-e-design.md 4.1(b)).
				kinds := make([]string, 0, len(trace.Kinds))
				for k := range trace.Kinds {
					kinds = append(kinds, k)
				}
				sort.Strings(kinds)
				for _, k := range kinds {
					fmt.Fprintf(GinkgoWriter, "WS-E12-COST-KIND %s %8d %s\n", pr.name, trace.Kinds[k], k)
				}
			}
			got[pr.name] = line
			fmt.Fprintf(GinkgoWriter, "WS-E12-COST %q: %q,\n", pr.name, line)
		}
		Expect(sortedStringKeys(got)).To(Equal(sortedStringKeys(wsE12CostPins)), "every probe is pinned and every pin is probed")
		for _, k := range sortedStringKeys(got) {
			Expect(got[k]).To(Equal(wsE12CostPins[k]), k)
		}
	})
})

// wsE12CostPins is the target's (4.14.2.0) planner task count per phase and its plan for
// each probe (captured from /var/tmp/fdb-upgrade-recovery/wse12-cost-cap1.log). Two to
// four IN lists cost the target 3835, 19366 and 117131 tasks; the target refuses the
// sweep's `NOT f` conjunct family outright, so its and-idempotent pair is Go's alone.
var wsE12CostPins = map[string]string{
	"in2_conjunct":        "ERROR java RelationalException: expected boolean expression but got BOOLEAN AS _0",
	"in2_conjunct_twice":  "ERROR java VerifyException: com.google.common.base.VerifyException",
	"in2_conjunct_thrice": "ERROR java VerifyException: com.google.common.base.VerifyException",
	"in2":                 "tasks=3835 PLANNING=3793 REWRITING=42 explain=\"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG))) SORTED] | INJOIN q0 -> { [IN arrayDistinct(promote(@c18 AS ARRAY(LONG)))] | INJOIN q1 -> { SCAN([IS T, EQUALS q0]) | FILTER _.A EQUALS q1 | MAP (_.ID AS ID) } }\"",
	"in3":                 "tasks=19366 PLANNING=19324 REWRITING=42 explain=\"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG))) SORTED] | INJOIN q0 -> { [IN arrayDistinct(promote(@c18 AS ARRAY(LONG)))] | INJOIN q1 -> { [IN arrayDistinct(promote(@c29 AS ARRAY(LONG)))] | INJOIN q2 -> { SCAN([IS T, EQUALS q0]) | FILTER _.A EQUALS q1 AND _.B EQUALS q2 | MAP (_.ID AS ID) } } }\"",
	"in4":                 "tasks=117131 PLANNING=117089 REWRITING=42 explain=\"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG))) SORTED] | INJOIN q0 -> { [IN arrayDistinct(promote(@c18 AS ARRAY(LONG)))] | INJOIN q1 -> { [IN arrayDistinct(promote(@c29 AS ARRAY(LONG)))] | INJOIN q2 -> { [IN arrayDistinct(@c37)] | INJOIN q3 -> { COVERING(T_AB [EQUALS q1, EQUALS q2] -> [A: KEY:[0], B: KEY:[1], ID: KEY:[3]]) ∩ COVERING(T_S [EQUALS q3] -> [ID: KEY:[2], S: KEY:[0]]) COMPARE BY (_.ID) | FILTER _.ID EQUALS q0 | MAP (_.ID AS ID) } } } }\"",
	"in_id_twice":         "tasks=1443 PLANNING=1401 REWRITING=42 explain=\"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG))) SORTED] | INJOIN q0 -> { SCAN([IS T, EQUALS q0]) | MAP (_.ID AS ID) }\"",
	"in_a_twice":          "tasks=641 PLANNING=599 REWRITING=42 explain=\"SCAN([IS T]) | FLATMAP q0 -> { EXPLODE arrayDistinct(promote(@c7 AS ARRAY(LONG))) | FILTER q0.A EQUALS _ AS q1 RETURN (q0.ID AS ID) }\"",
	"in7_two_lists":       "tasks=2912 PLANNING=2855 REWRITING=57 explain=\"[IN arrayDistinct(promote(@c7 AS ARRAY(LONG))) ⋈ IN arrayDistinct(promote(@c17 AS ARRAY(LONG)))] INUNION q0, q1 -> { ISCAN(IDX_VAL [EQUALS q0]) | FILTER _.CAT EQUALS q1 | MAP (_.ID AS ID, _.VAL AS VAL) } COMPARE BY (_.VAL, _.ID) | MAP (_.ID AS ID)\"",
}

// wseRequireNaNPinArch fails, loudly, when the NaN rounds run on an architecture other than
// the one their pins were measured on. A NaN made by a division is the hardware's: amd64
// gives the negative quiet NaN (fff8000000000000, ffc00000), which rounds v11 and v12 pin,
// and another architecture may give the positive one in both engines. A pin measured on
// amd64 read on arm64 would then report a divergence that is not one, so the rounds are
// re-captured per architecture rather than compared across them.
func wseRequireNaNPinArch() {
	Expect(runtime.GOARCH).To(Equal("amd64"),
		"the WS-E NaN pins were measured on amd64; a division's NaN is the hardware's, so re-capture wsE11Pins and wsE12Pins on %s", runtime.GOARCH)
}

// Round v12, the NaN write divergence across the engines: a NaN Go's SQL CAST produces,
// written by Go's record layer into a store the target created with a UNIQUE index on the
// column, then the target's index probe for its own CAST NaN and the target's insert of
// that NaN into the same UNIQUE index. Today Go's bits (7ff8000000000001) differ from the
// target's (7ff8000000000000): the probe misses Go's row and the insert does not collide.
// ws-e-design.md step (6) makes Go's CAST write the target's bits, and these pins flip:
// the probe finds the Go-written row and the insert fails with 23505.
var _ = Describe("WS-E target oracle v12 cross-engine NaN", func() {
	It("records whether the target's index probe and UNIQUE index see a NaN Go's CAST wrote", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSE12X_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		const body = "CREATE TABLE T (id BIGINT, d DOUBLE, PRIMARY KEY (id)) CREATE UNIQUE INDEX T_D AS SELECT d FROM T ORDER BY d"
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		var store struct {
			DbPath      string `json:"dbPath"`
			SchemaName  string `json:"schemaName"`
			StorePrefix []int  `json:"storePrefix"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()

		// Go's CAST('NaN' AS DOUBLE), evaluated by Go's SQL engine on a Go-created schema.
		goClusterFile := writeClusterFileToTemp(clusterFile)
		defer func() { _ = os.Remove(goClusterFile) }()
		suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		goTemplate, goDB, goSchema := "WSE12X_GO_"+suffix, "/WSE12X_GO_"+suffix, "S_"+suffix
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", goClusterFile))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		_, err = sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+goTemplate+" CREATE TABLE T (id BIGINT, d DOUBLE, PRIMARY KEY (id))")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goTemplate) }()
		_, err = sysDB.ExecContext(ctx, "CREATE DATABASE "+goDB)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+goDB) }()
		_, err = sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", goDB, goSchema, goTemplate))
		Expect(err).NotTo(HaveOccurred())
		schemaDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", goDB, goClusterFile, goSchema))
		Expect(err).NotTo(HaveOccurred())
		defer schemaDB.Close()
		_, err = schemaDB.ExecContext(ctx, "INSERT INTO T VALUES (1, CAST('NaN' AS DOUBLE))")
		Expect(err).NotTo(HaveOccurred())
		var goNaN float64
		Expect(schemaDB.QueryRowContext(ctx, "SELECT d FROM T WHERE id = 1").Scan(&goNaN)).To(Succeed())

		got := map[string]string{"go_cast_nan_bits": fmt.Sprintf("%016x", math.Float64bits(goNaN))}

		// Go's record layer writes that value as row 2 of the target's store.
		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			tmpl, err := cat.SchemaTemplateCatalog().LoadSchemaTemplate(catalog.NewFDBTransaction(rtx), name)
			if err != nil {
				return nil, err
			}
			md := tmpl.(*metadata.RecordLayerSchemaTemplate).Underlying()
			prefix := make([]byte, len(store.StorePrefix))
			for j, b := range store.StorePrefix {
				prefix[j] = byte(b)
			}
			rs, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(subspace.FromBytes(prefix)).Open()
			if err != nil {
				return nil, err
			}
			rt := md.GetRecordType("T")
			msg := dynamicpb.NewMessage(rt.Descriptor)
			msg.Set(rt.Descriptor.Fields().ByName("ID"), protoreflect.ValueOfInt64(2))
			msg.Set(rt.Descriptor.Fields().ByName("D"), protoreflect.ValueOfFloat64(goNaN))
			_, err = rs.SaveRecord(msg)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// The target's index probe for its own NaN, its plan, and its insert of that NaN.
		for _, q := range []struct{ name, sql string }{
			{"java_probe_rows", "SELECT id FROM T WHERE d = CAST('NaN' AS DOUBLE)"},
			{"java_probe_explain", "EXPLAIN SELECT id FROM T WHERE d = CAST('NaN' AS DOUBLE)"},
		} {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "querySql": q.sql,
			}, &out)).To(Succeed())
			rows, _ := out["rows"].([]any)
			if strings.HasPrefix(q.sql, "EXPLAIN") {
				// The plan text only: the other EXPLAIN columns (plan hash, graph renderings,
				// planner statistics) are not stable across runs.
				Expect(rows).NotTo(BeEmpty(), q.sql)
				first, _ := rows[0].([]any)
				Expect(first).NotTo(BeEmpty(), q.sql)
				got[q.name] = fmt.Sprint(first[0])
				continue
			}
			got[q.name] = fmt.Sprint(rows)
		}
		var ins struct {
			Outcome string `json:"outcome"`
		}
		Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName,
			"sql": "INSERT INTO T VALUES (1, CAST('NaN' AS DOUBLE))",
		}, &ins)).To(Succeed())
		got["java_insert_nan_beside_go_row"] = ins.Outcome
		var entries struct {
			Entries []struct {
				Hex string `json:"hex"`
			} `json:"entries"`
		}
		Expect(java.InvokeAs(ctx, "wsjIndexEntriesJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "indexName": "T_D",
		}, &entries)).To(Succeed())
		for _, e := range entries.Entries {
			raw, err := hex.DecodeString(e.Hex)
			Expect(err).NotTo(HaveOccurred())
			t, err := tuple.Unpack(raw)
			Expect(err).NotTo(HaveOccurred())
			bits := fmt.Sprintf("%T %v", t[0], t[0])
			if v, ok := t[0].(float64); ok {
				bits = fmt.Sprintf("%016x", math.Float64bits(v))
			}
			got[fmt.Sprintf("t_d_id%v", t[len(t)-1])] = bits
		}
		for _, k := range sortedStringKeys(got) {
			fmt.Fprintf(GinkgoWriter, "WS-E12-PIN %q: %q,\n", "x_"+k, got[k])
		}
		Expect(sortedStringKeys(got)).To(Equal(sortedStringKeys(wsE12CrossPins)), "every probe is pinned and every pin is probed")
		wseRequireNaNPinArch()
		for _, k := range sortedStringKeys(got) {
			Expect(got[k]).To(Equal(wsE12CrossPins[k]), k)
		}
	})
})

// wsE12CrossPins is the measured outcome of the cross-engine NaN round (captured from
// /var/tmp/fdb-upgrade-recovery/wse12x-cap1.log): Go's CAST wrote 7ff8000000000001, the
// target's covering probe of T_D for its own NaN answers no rows, and the target's insert of
// its NaN beside Go's row succeeds, leaving two NaN entries in the UNIQUE index.
var wsE12CrossPins = map[string]string{
	"go_cast_nan_bits":              "7ff8000000000001",
	"java_insert_nan_beside_go_row": "OK 1",
	"java_probe_explain":            "COVERING(T_D [EQUALS CAST(@c9 AS DOUBLE)] -> [D: KEY:[0], ID: KEY:[2]]) | MAP (_.ID AS ID)",
	"java_probe_rows":               "[]",
	"t_d_id1":                       "7ff8000000000000",
	"t_d_id2":                       "7ff8000000000001",
}
