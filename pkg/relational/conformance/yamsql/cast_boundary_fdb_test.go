package yamsql_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

func TestNumericCastBoundaryFDB(t *testing.T) {
	t.Parallel()
	// Decimal spellings and answers are explicit, independent of the cast code.
	// The separate unit oracle uses exact rationals; the live JVM probe pins the
	// overflow/narrowing rule without the JSON runner's float64 normalization.
	cases := []struct {
		id, literal                string
		integer, long, floatResult int64
	}{
		{"negative-zero", "-0.0", 0, 0, 0},
		{"positive-zero", "0.0", 0, 0, 0},
		{"below-positive-half", "0.49999999999999994", 0, 0, 1},
		{"positive-half", "0.5", 1, 1, 1},
		{"negative-half", "-0.5", 0, 0, 0},
		{"below-negative-half", "-0.5000000000000001", -1, -1, 0},
		{"positive-int32-tie", "2147483647.5", -2147483648, 2147483648, 2147483647},
		{"negative-int32-tie", "-2147483648.5", -2147483648, -2147483648, -2147483648},
		{"negative-int32-wrap", "-2147483648.6", 2147483647, -2147483649, -2147483648},
		{"uint32-modulus", "4294967296.0", 0, 4294967296, 2147483647},
		{"odd-large-integer", "4503599627370497.0", 1, 4503599627370497, 2147483647},
		{"below-int64-max", "9223372036854774784.0", -1024, 9223372036854774784, 2147483647},
		{"positive-int64-limit", "9223372036854775808.0", -1, 9223372036854775807, 2147483647},
		{"negative-int64-limit", "-9223372036854775808.0", 0, -9223372036854775808, -2147483648},
		{"below-int64-min", "-9223372036854777856.0", 0, -9223372036854775808, -2147483648},
		{"positive-huge", "1.0E20", -1, 9223372036854775807, 2147483647},
		{"negative-huge", "-1.0E20", 0, -9223372036854775808, -2147483648},
	}
	required := []string{"negative-zero", "positive-zero", "below-positive-half", "positive-half", "negative-half", "below-negative-half", "positive-int32-tie", "negative-int32-tie", "negative-int32-wrap", "uint32-modulus", "odd-large-integer", "below-int64-max", "positive-int64-limit", "negative-int64-limit", "below-int64-min", "positive-huge", "negative-huge"}
	var ids []string
	for _, c := range cases {
		ids = append(ids, c.id)
	}
	if !reflect.DeepEqual(ids, required) {
		t.Fatalf("CAST boundary population changed: %v, want %v", ids, required)
	}
	scalar := func(kind, value string) yamsql.Scalar { return yamsql.Scalar{Kind: kind, Value: &value} }
	s := &yamsql.Scenario{Name: "cast-boundaries", SchemaTemplate: "CREATE TABLE t (id BIGINT, d DOUBLE, f FLOAT, PRIMARY KEY(id))"}
	for i, c := range cases {
		s.Setup = append(s.Setup, fmt.Sprintf("INSERT INTO t VALUES (%d, %s, CAST(%s AS FLOAT))", i, c.literal, c.literal))
		d, err := strconv.ParseFloat(c.literal, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"DOUBLE", "FLOAT"} {
			v := d
			wantInt, wantLong := c.integer, c.long
			if source == "FLOAT" {
				v = float64(float32(d))
				wantInt, wantLong = c.floatResult, c.floatResult
			}
			want := [][]yamsql.Scalar{{scalar("float64", fmt.Sprintf("%016x", math.Float64bits(v))), scalar("int64", strconv.FormatInt(wantInt, 10)), scalar("int64", strconv.FormatInt(wantLong, 10))}}
			for _, route := range []string{"literal", "driver-text", "stored"} {
				operand := c.literal
				var args []yamsql.Scalar
				if route == "driver-text" {
					operand = "?"
					input := scalar("float64", fmt.Sprintf("%016x", math.Float64bits(d)))
					args = []yamsql.Scalar{input, input, input}
				}
				if source == "FLOAT" {
					operand = "CAST(" + operand + " AS FLOAT)"
				}
				if route == "stored" {
					operand = "d"
					if source == "FLOAT" {
						operand = "f"
					}
				}
				s.Tests = append(s.Tests, yamsql.Test{Query: fmt.Sprintf("SELECT %s, CAST(%s AS INTEGER), CAST(%s AS BIGINT) FROM t WHERE id = %d", operand, operand, operand, i), Args: args, ExactRows: &want, ColumnTypes: []string{source, "INTEGER", "BIGINT"}})
			}
		}
	}
	// Fixed independently of the generator: 17 inputs × 2 source widths × 3 routes.
	// Every statement asserts the input bits/type and both integer outputs/types.
	if len(s.Tests) != 102 {
		t.Fatalf("required 102 CAST statements, generated %d", len(s.Tests))
	}
	r := runSemanticScenario(t, s)
	if r.TestsRun != 102 || r.TestsPass != 102 || r.TestsFail != 0 {
		t.Fatalf("CAST: run=%d pass=%d fail=%d: %+v", r.TestsRun, r.TestsPass, r.TestsFail, r.Failures)
	}
	t.Logf("CAST boundary envelope: 204 required/exercised/validated integer-consumer cells in %d statements; signed-zero inputs witnessed separately", r.TestsPass)
}

func TestNumericCastArrayFDB(t *testing.T) {
	t.Parallel()
	s := &yamsql.Scenario{
		Name:           "cast-array-boundaries",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, d DOUBLE ARRAY, f FLOAT ARRAY, PRIMARY KEY(id))",
		Setup:          []string{"INSERT INTO t VALUES (1, [1.0E20, -1.0E20], CAST([1.0E20, -1.0E20] AS FLOAT ARRAY))"},
		// Java MessageHelpers.coerceArray rejects null elements at the protobuf
		// write boundary. Keep the original rejected fixture as a negative pin;
		// SQL ARRAY targets also declare their elements non-nullable.
		Tests: []yamsql.Test{{Exec: "INSERT INTO t VALUES (1, [1.0E20, NULL, -1.0E20], CAST([1.0E20, NULL, -1.0E20] AS FLOAT ARRAY))", ErrorCode: "XX000", ErrorMessage: "NULL as elements of a collection are currently not supported"}},
	}
	var nullElementQueries []string
	for _, tc := range []struct{ source, target, positive, negative string }{
		{"d", "BIGINT", "9223372036854775807", "-9223372036854775808"},
		{"d", "INTEGER", "-1", "0"},
		{"f", "BIGINT", "2147483647", "-2147483648"},
		{"f", "INTEGER", "2147483647", "-2147483648"},
	} {
		want := [][]yamsql.Scalar{{{Kind: "int64", Value: &tc.positive}}, {{Kind: "int64", Value: &tc.negative}}}
		s.Tests = append(s.Tests, yamsql.Test{
			Query:     fmt.Sprintf("SELECT x FROM (SELECT CAST(%s AS %s ARRAY) AS a FROM t) q, q.a x", tc.source, tc.target),
			ExactRows: &want, Unordered: true, ColumnTypes: []string{tc.target},
		})
		for _, literal := range []string{"[1.0E20, -1.0E20]", "[]", "NULL", "[1.0E20, NULL, -1.0E20]"} {
			operand := literal
			if tc.source == "f" {
				operand = "CAST(" + operand + " AS FLOAT ARRAY)"
			}
			query := fmt.Sprintf("SELECT x FROM (SELECT CAST(%s AS %s ARRAY) AS a FROM t) q, q.a x", operand, tc.target)
			if literal == "[1.0E20, NULL, -1.0E20]" {
				nullElementQueries = append(nullElementQueries, query)
				continue
			}
			rows := want
			if literal == "[]" || literal == "NULL" {
				rows = [][]yamsql.Scalar{}
			}
			s.Tests = append(s.Tests, yamsql.Test{Query: query, ExactRows: &rows, Unordered: true, ColumnTypes: []string{tc.target}})
		}
	}
	r := runSemanticScenario(t, s)
	if r.TestsRun != 17 || r.TestsPass != 17 || r.TestsFail != 0 {
		t.Fatalf("ARRAY CAST: %+v", r)
	}
	// These original NULL-element candidates are negative, not positive rows:
	// SQL ARRAY targets have non-nullable elements (SemanticAnalyzer.lookupType).
	// Live Java throws NPE on the DOUBLE-to-LONG shape; Go reports its checked
	// layout violation instead. Nullable containers above remain valid empties.
	if len(nullElementQueries) != 4 {
		t.Fatalf("required four NULL-element rejection cases, got %d", len(nullElementQueries))
	}
	ctx, cancel := context.WithTimeout(context.Background(), scenarioBudget)
	defer cancel()
	runner := plandiff.NewGoSQLSetupRunner(clusterFilePath)
	for _, query := range nullElementQueries {
		result := runner.RunWithSetup(ctx, s.SchemaTemplate, s.Setup, query)
		var coded interface {
			Code() values.ResolutionErrorCode
		}
		if !errors.As(result.Err, &coded) || coded.Code() != values.LayoutNullabilityMismatch {
			t.Errorf("NULL-element query %s: wanted layout nullability error, got %v / %v", query, result.Rows.Rows, result.Err)
		}
	}
}

func TestBoundArrayRoutesFDB(t *testing.T) {
	t.Parallel()
	s := &yamsql.Scenario{
		Name: "bound-array-routes",
		SchemaTemplate: `CREATE TYPE AS STRUCT leaf (tag STRING, vals DOUBLE ARRAY)
CREATE TYPE AS STRUCT item (item_id BIGINT, n leaf)
CREATE TABLE t (id BIGINT, a DOUBLE ARRAY, items item ARRAY, PRIMARY KEY(id))
CREATE TABLE q (id BIGINT, a DOUBLE ARRAY, PRIMARY KEY(id))`,
		Setup: []string{
			"INSERT INTO t VALUES (1, [1.0E20, -1.0E20], [(7, ('n', [1.0E20, -1.0E20]))])",
			"INSERT INTO q VALUES (1, [99.0])",
		},
	}
	positive, negative := "9223372036854775807", "-9223372036854775808"
	want := [][]yamsql.Scalar{{{Kind: "int64", Value: &positive}}, {{Kind: "int64", Value: &negative}}}
	cases := []struct{ name, query string }{
		{"quoted_owner", `SELECT x FROM (SELECT id, CAST(a AS BIGINT ARRAY) AS "a.b" FROM t) AS "q.q", "q.q"."a.b" x`},
		{"quoted_owner_rebuild", `SELECT x FROM (SELECT "q.q".*, x FROM (SELECT CAST(a AS BIGINT ARRAY) AS "a.b" FROM t) AS "q.q", "q.q"."a.b" x) d`},
		{"quoted_cte", `WITH "q.q" AS (SELECT CAST(a AS BIGINT ARRAY) AS "a.b" FROM t) SELECT x FROM "q.q", "q.q"."a.b" x`},
		{"quoted_solo_star", `SELECT id FROM (SELECT "q.q".* FROM (SELECT CAST(x AS BIGINT) AS id FROM t, t.a x) AS "q.q", q) e`},
		{"session_schema", `SELECT x FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM conf.t) d, d.a x`},
		{"cte_labels", `WITH c("a.b") AS (SELECT CAST(a AS BIGINT ARRAY) FROM t) SELECT x FROM c, c."a.b" x`},
		{"chained_cte", `WITH c AS (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t), d AS (SELECT a FROM c) SELECT x FROM d, d.a x`},
		{"cte_shadows_table", `WITH q AS (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) SELECT x FROM q, q.a x`},
		{"join_body", `SELECT x FROM (SELECT CAST(t.a AS BIGINT ARRAY) AS a FROM t JOIN q ON t.id = q.id) d, d.a x`},
		{"earlier_owner", `SELECT x FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) d, q, d.a x`},
		{"later_source", `SELECT x FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) d, d.a x, q`},
		{"alias_collision", `SELECT q FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) q, q.a q`},
		{"alias_collision_at", `SELECT q FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) q, q.a q AT pos`},
		{"alias_later_source", `SELECT CAST(x AS BIGINT) FROM t, t.a x, q x`},
		{"alias_mint_shaped", `SELECT x FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) x, x.a x, q AS "Q$DUP1"`},
		{"alias_explicit_rendered_path", `SELECT CAST("T.A" AS BIGINT) FROM t, t.a AS "T.A"`},
		{"alias_chained_minted_owner", `SELECT CAST(x AS BIGINT) FROM t, t.items t AT pos, t.n.vals x`},
		{"alias_chained_default_owner", `SELECT CAST(x AS BIGINT) FROM t AS items, items.items, items.n.vals x`},
		{"chained_struct", `SELECT CAST(y AS BIGINT) FROM t, t.items x, x.n.vals y`},
		{"chained_struct_at", `SELECT CAST(y AS BIGINT) FROM t, t.items x AT xp, x.n.vals y AT yp`},
		{"derived_struct", `SELECT CAST(y AS BIGINT) FROM (SELECT id, x FROM t, t.items x) d, d.x.n.vals y`},
		{"cte_struct", `WITH c(id, x) AS (SELECT t.id, x FROM t, t.items x) SELECT CAST(y AS BIGINT) FROM c, c.x.n.vals y`},
	}
	if len(cases) != 22 {
		t.Fatalf("required 22 numeric bound array routes, got %d", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenario := *s
			scenario.Name = tc.name
			scenario.Tests = []yamsql.Test{{Query: tc.query, ExactRows: &want, Unordered: true, ColumnTypes: []string{"BIGINT"}, PlanContains: "Explode("}}
			r := runSemanticScenario(t, &scenario)
			if r.TestsRun != 1 || r.TestsPass != 1 || r.TestsFail != 0 {
				t.Fatalf("bound array route: %+v", r)
			}
		})
	}
	// Reusing a chained element alias is legal until a reference names both
	// outputs. Live Java pins the unused and ambiguous expression forms.
	for _, tc := range []struct {
		name string
		test yamsql.Test
	}{
		{"implicit_select_alias", yamsql.Test{Query: `SELECT 1 FROM (SELECT CAST(a AS BIGINT ARRAY) a FROM t) a, a.a`, ErrorCode: "42703"}},
		{"alias_default_collision", yamsql.Test{Query: `SELECT a FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) a, a.a`, ErrorCode: "42702"}},
		{"alias_mint_default", yamsql.Test{Query: `SELECT a FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) a, a.a, q AS "Q$DUP1"`, ErrorCode: "42702"}},
		{"alias_chained_ambiguous", yamsql.Test{Query: `SELECT CAST(x AS BIGINT) FROM t, t.items x, x.n.vals x`, ErrorCode: "42702"}},
		{"alias_chained_ambiguous_at", yamsql.Test{Query: `SELECT CAST(x AS BIGINT) FROM t, t.items x AT xp, x.n.vals x AT yp`, ErrorCode: "42702"}},
		{"alias_chained_unused", yamsql.Test{Query: `SELECT 1 FROM t, t.items x, x.n.vals x`, Rows: [][]any{{1}, {1}}, ColumnTypes: []string{"INTEGER"}, PlanContains: "Explode("}},
		{"alias_chained_unused_at", yamsql.Test{Query: `SELECT 1 FROM t, t.items x AT xp, x.n.vals x AT yp`, Rows: [][]any{{1}, {1}}, ColumnTypes: []string{"INTEGER"}, PlanContains: "Explode("}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenario := *s
			scenario.Name, scenario.Tests = tc.name, []yamsql.Test{tc.test}
			r := runSemanticScenario(t, &scenario)
			if r.TestsRun != 1 || r.TestsPass != 1 || r.TestsFail != 0 {
				t.Fatalf("alias reference route: %+v", r)
			}
		})
	}
}
