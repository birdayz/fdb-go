package embedded

import (
	"strings"
	"testing"

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
