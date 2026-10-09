package embedded

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"

	"google.golang.org/protobuf/encoding/prototext"
)

// Boolean macro bodies persist as the value trees Java's ExpressionVisitor
// builds: RelOpValue (RelOpValue.encapsulate, operator typed over the
// operands as written), AndOrValue and NotValue.
func TestMacroBooleanBodiesPersistAsJavaValueTrees(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ddl  string
		want []string
	}{
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a > 5", []string{
			`binary_rel_op_value:{super:{function_name:"gt"comparison_type:GREATER_THAN`,
			`operator:GT_LI`, `int_value:5`,
		}},
		{"CREATE FUNCTION f(IN a INTEGER, IN b BIGINT) RETURNS BOOLEAN RETURN a > b", []string{`operator:GT_IL`}},
		{"CREATE TYPE AS ENUM mood ('HAPPY', 'SAD') CREATE TYPE AS STRUCT st(m mood) " +
			"CREATE FUNCTION f(IN x TYPE st) RETURNS BOOLEAN RETURN x.m = 'HAPPY'", []string{`operator:EQ_ES`, `string_value:"HAPPY"`}},
		{"CREATE FUNCTION f(IN a BIGINT, IN b BIGINT) RETURNS BOOLEAN RETURN a <> b", []string{`function_name:"notEquals"comparison_type:NOT_EQUALS`, `operator:NEQ_LL`}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a IS NULL", []string{
			`unary_rel_op_value:{super:{function_name:"isNull"comparison_type:IS_NULL`, `operator:IS_NULL_LI`,
		}},
		{"CREATE FUNCTION f(IN s STRING) RETURNS BOOLEAN RETURN s IS NOT NULL", []string{`function_name:"notNull"comparison_type:NOT_NULL`, `operator:IS_NOT_NULL_SS`}},
		{"CREATE FUNCTION f(IN a BIGINT, IN b BIGINT) RETURNS BOOLEAN RETURN a IS DISTINCT FROM b", []string{`operator:IS_DISTINCT_FROM_LL`}},
		{"CREATE FUNCTION f(IN a BOOLEAN, IN b BOOLEAN) RETURNS BOOLEAN RETURN a AND b", []string{
			`and_or_value:{function_name:"and"left_child:{quantified_object_value:`, `operator:AND}`,
		}},
		{"CREATE FUNCTION f(IN a BOOLEAN, IN b BOOLEAN) RETURNS BOOLEAN RETURN a OR NOT b", []string{
			`function_name:"or"`, `right_child:{not_value:{child:{quantified_object_value:`, `operator:OR}`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN NOT (a < 3)", []string{
			`not_value:{child:{binary_rel_op_value:{super:{function_name:"lt"`, `operator:LT_LI`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a > 2.5", []string{`operator:GT_LD`, `double_value:2.5`}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a IN (1, 2)", []string{
			`in_op_value:{probe_value:{quantified_object_value:`, `in_array_value:{promote_value:{in_value:{light_array_constructor_value:`,
			`array_coercion_bi_function:`, `child_pair:{index:-1`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT, IN b BIGINT) RETURNS BOOLEAN RETURN a NOT IN (b, 2)", []string{
			`not_value:{child:{in_op_value:`, `light_array_constructor_value:{super:{children:{quantified_object_value:`,
			`children:{promote_value:{in_value:{literal_value:`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a BETWEEN 1 AND 5", []string{
			`function_name:"lte"comparison_type:LESS_THAN_OR_EQUALSchildren:{literal_value:`, `operator:LTE_IL`, `operator:LTE_LI`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a NOT BETWEEN 1 AND 5", []string{
			`and_or_value:{function_name:"or"`, `operator:LT_LI`, `operator:GT_LI`,
		}},
		{"CREATE FUNCTION f(IN b BOOLEAN) RETURNS BOOLEAN RETURN b IS NOT FALSE", []string{
			`function_name:"or"left_child:{unary_rel_op_value:{super:{function_name:"isNull"`, `bool_value:true`, `operator:EQ_BB`,
		}},
		{"CREATE FUNCTION f(IN s STRING) RETURNS BOOLEAN RETURN s LIKE 'a%'", []string{
			`escape_child:{literal_value:{result_type:{null_type:{}}value:{primitive_object:{}}}}`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT) RETURNS STRING RETURN CASE WHEN a > 1 THEN 'x' ELSE 'y' END", []string{
			`pick_value:{selector_value:{condition_selector_value:{implications:{binary_rel_op_value:`,
			`type_url:"c.a.fdb.types/com.apple.foundationdb.record.PTautologicalValue"`,
		}},
		{"CREATE FUNCTION f(IN x INTEGER, IN b BOOLEAN) RETURNS INTEGER RETURN CASE WHEN b THEN x ELSE 5 END", []string{
			`promote_value:{in_value:{literal_value:{result_type:{primitive_type:{type_code:INTis_nullable:false}}value:{primitive_object:{int_value:5}}}}promote_to_type:{primitive_type:{type_code:INTis_nullable:true}}}`,
		}},
		{"CREATE FUNCTION f(IN a BIGINT, IN b BIGINT, IN c BIGINT) RETURNS BOOLEAN RETURN a = 1 AND b = 2 AND c = 3", []string{
			`and_or_value:{function_name:"and"left_child:{and_or_value:{function_name:"and"`,
		}},
	} {
		tmpl, err := BuildSchemaTemplateFromDDLNamed("CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) "+tc.ddl, "RELOP")
		if err != nil {
			t.Errorf("%s: %v", tc.ddl, err)
			continue
		}
		fns := tmpl.Underlying().UserDefinedFunctions()
		if len(fns) != 1 {
			t.Fatalf("%s: %d functions", tc.ddl, len(fns))
		}
		compact := strings.Join(strings.Fields(prototext.Format(fns[0])), "")
		for _, want := range tc.want {
			if !strings.Contains(compact, want) {
				t.Errorf("%s: stored body lacks %s:\n%s", tc.ddl, want, compact)
			}
		}
	}
}

// Shapes the target cannot call or has no operator for are refused: EXISTS
// (stored without its subquery, "Missing binding" when called) and a DATE
// comparison (a Go-only type, no RelOpValue operator).
func TestMacroBodiesJavaCannotPersist(t *testing.T) {
	t.Parallel()
	for ddl, want := range map[string]string{
		"CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN EXISTS (SELECT * FROM t WHERE id = a)":        "EXISTS cannot be persisted",
		"CREATE FUNCTION f() RETURNS BOOLEAN RETURN CAST('2020-01-02' AS DATE) > CAST('2020-01-01' AS DATE)": "not compatible",
	} {
		_, err := BuildSchemaTemplateFromDDLNamed("CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) "+ddl, "RELOP")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", ddl, err, want)
		}
	}
}

// A called macro is resolved as its body written inline: the stored Java
// tree lowers to the same result value and the same plan.
func TestMacroBooleanBodiesPlanAsInline(t *testing.T) {
	t.Parallel()
	const fns = "CREATE TABLE t (id BIGINT, a BIGINT, b BOOLEAN, s STRING, PRIMARY KEY (id)) " +
		"CREATE INDEX t_a AS SELECT a FROM t ORDER BY a " +
		"CREATE FUNCTION btw(IN x BIGINT) RETURNS BOOLEAN RETURN x BETWEEN 2 AND 7 " +
		"CREATE FUNCTION inl(IN x BIGINT, IN y BIGINT) RETURNS BOOLEAN RETURN x NOT IN (1, y) " +
		"CREATE FUNCTION cs(IN x BIGINT, IN y BOOLEAN) RETURNS BOOLEAN RETURN CASE WHEN x > 5 THEN y WHEN x IS NULL THEN FALSE ELSE x < 2 END " +
		"CREATE FUNCTION cn(IN x BIGINT) RETURNS BIGINT RETURN CASE WHEN x > 5 THEN 1 ELSE x END " +
		"CREATE FUNCTION cb(IN y BOOLEAN) RETURNS INTEGER RETURN CASE WHEN y THEN 1 ELSE 2 END " +
		"CREATE FUNCTION lk(IN x STRING) RETURNS BOOLEAN RETURN x LIKE 'q%' " +
		"CREATE FUNCTION ist(IN x BOOLEAN) RETURNS BOOLEAN RETURN x IS NOT TRUE"
	tmpl, err := BuildSchemaTemplateFromDDLNamed(fns, "RELOP_INLINE")
	if err != nil {
		t.Fatal(err)
	}
	for call, inline := range map[string]string{
		"btw(a)":     "a BETWEEN 2 AND 7",
		"inl(a, id)": "a NOT IN (1, id)",
		"cs(a, b)":   "CASE WHEN a > 5 THEN b WHEN a IS NULL THEN FALSE ELSE a < 2 END",
		"cn(a)":      "CASE WHEN a > 5 THEN 1 ELSE a END",
		"cb(b)":      "CASE WHEN b THEN 1 ELSE 2 END",
		"lk(s)":      "s LIKE 'q%'",
		"ist(b)":     "b IS NOT TRUE",
	} {
		for _, shape := range []string{"SELECT id, %s FROM t", "SELECT id FROM t WHERE %s"} {
			if (call == "cn(a)" || call == "cb(b)") && strings.Contains(shape, "WHERE") {
				continue
			}
			mp, err := PlanRecordQueryWithMetadata(fmt.Sprintf(shape, call), tmpl.Underlying(), nil)
			if err != nil {
				t.Fatalf("%s: %v", call, err)
			}
			ip, err := PlanRecordQueryWithMetadata(fmt.Sprintf(shape, inline), tmpl.Underlying(), nil)
			if err != nil {
				t.Fatalf("%s: %v", inline, err)
			}
			if got, want := quantifierBlind(values.ExplainValue(mp.GetResultValue())), quantifierBlind(values.ExplainValue(ip.GetResultValue())); got != want {
				t.Errorf("%s result %s, inline %s", fmt.Sprintf(shape, call), got, want)
			}
			if got, want := quantifierBlind(mp.Explain()), quantifierBlind(ip.Explain()); got != want {
				t.Errorf("%s plans as %s, inline as %s", fmt.Sprintf(shape, call), got, want)
			}
		}
	}
}

var quantifierName = regexp.MustCompile(`q\$[0-9]+`)

func quantifierBlind(s string) string { return quantifierName.ReplaceAllString(s, "q") }
