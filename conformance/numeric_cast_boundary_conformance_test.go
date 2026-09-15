//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NumericCastBoundaryConformance", func() {
	It("matches explicit Java numeric cast answers without JSON precision loss", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "castboundary_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const schema = "CREATE TABLE cast_values (id BIGINT, d DOUBLE, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO cast_values VALUES (1, 1.0E20), (2, -1.0E20), (3, 9223372036854774784.0), (4, -9223372036854775808.0), (5, 2147483647.5), (6, -2147483648.6), (7, 0.49999999999999994), (8, -0.5000000000000001), (9, 4503599627370497.0), (10, -0.0)"}
		probes := []struct {
			name, query string
			want        [][]any
		}{
			{"literal saturation", "SELECT CAST(CAST(1.0E20 AS BIGINT) AS STRING), CAST(CAST(-1.0E20 AS BIGINT) AS STRING) FROM cast_values WHERE id = 1", [][]any{{"9223372036854775807", "-9223372036854775808"}}},
			{"literal int narrowing", "SELECT CAST(CAST(1.0E20 AS INTEGER) AS STRING), CAST(CAST(-1.0E20 AS INTEGER) AS STRING), CAST(CAST(4294967296.0 AS INTEGER) AS STRING) FROM cast_values WHERE id = 1", [][]any{{"-1", "0", "0"}}},
			{"stored saturation and boundary controls", "SELECT CAST(CAST(d AS BIGINT) AS STRING) FROM cast_values WHERE id <= 4 ORDER BY id", [][]any{{"9223372036854775807"}, {"-9223372036854775808"}, {"9223372036854774784"}, {"-9223372036854775808"}}},
			{"stored int wrap", "SELECT CAST(CAST(d AS INTEGER) AS STRING) FROM cast_values WHERE id = 5 OR id = 6 ORDER BY id", [][]any{{"-2147483648"}, {"2147483647"}}},
			{"rounding and zero controls", "SELECT CAST(CAST(d AS BIGINT) AS STRING) FROM cast_values WHERE id >= 7 ORDER BY id", [][]any{{"0"}, {"-1"}, {"4503599627370497"}, {"0"}}},
			{"float width", "SELECT CAST(CAST(1.0E20 AS FLOAT) AS INTEGER), CAST(CAST(-1.0E20 AS FLOAT) AS INTEGER), CAST(CAST(0.49999999999999994 AS FLOAT) AS INTEGER), CAST(CAST(2147483648.0 AS FLOAT) AS INTEGER) FROM cast_values WHERE id = 1", [][]any{{float64(2147483647), float64(-2147483648), float64(1), float64(2147483647)}}},
			{"float long boxing", "SELECT CAST(CAST(0.49999999999999994 AS FLOAT) AS BIGINT), CAST(CAST(2147483648.0 AS FLOAT) AS BIGINT) FROM cast_values WHERE id = 1", [][]any{{float64(1), float64(2147483647)}}},
			{"computed array unnest", "SELECT CAST(x AS STRING) FROM (SELECT CAST([1.0E20, -1.0E20] AS BIGINT ARRAY) AS a FROM cast_values WHERE id = 1) q, q.a x", [][]any{{"9223372036854775807"}, {"-9223372036854775808"}}},
			{"computed array alias collision", "SELECT CAST(q AS STRING) FROM (SELECT CAST([1.0E20, -1.0E20] AS BIGINT ARRAY) AS a FROM cast_values WHERE id = 1) q, q.a q", [][]any{{"9223372036854775807"}, {"-9223372036854775808"}}},
			{"duplicate element ordinal unused", "SELECT 1 FROM (SELECT CAST([1.0E20, -1.0E20] AS BIGINT ARRAY) AS a FROM cast_values WHERE id = 1) q, q.a x AT x", [][]any{{float64(1)}, {float64(1)}}},
			{"duplicate element ordinal referenced", "SELECT x FROM (SELECT CAST([1.0E20, -1.0E20] AS BIGINT ARRAY) AS a FROM cast_values WHERE id = 1) q, q.a x AT x", nil},
			{"computed array null element", "SELECT CAST(x AS STRING) FROM (SELECT CAST([1.0E20, NULL, -1.0E20] AS BIGINT ARRAY) AS a FROM cast_values WHERE id = 1) q, q.a x", nil},
			{"computed empty array", "SELECT CAST(x AS STRING) FROM (SELECT CAST([] AS BIGINT ARRAY) AS a FROM cast_values WHERE id = 1) q, q.a x", nil},
			{"literal half ties", "SELECT CAST(CAST(0.5 AS INTEGER) AS STRING), CAST(CAST(-0.5 AS INTEGER) AS STRING), CAST(CAST(-2147483648.5 AS INTEGER) AS STRING) FROM cast_values WHERE id = 1", [][]any{{"1", "0", "-2147483648"}}},
		}
		// Rendering the integer through STRING inside SQL prevents the existing JSON
		// runner's numeric normalization from equating Long.MAX_VALUE with 2^63.
		var failures []string
		for _, probe := range probes {
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				result := runner.RunWithSetup(ctx, schema, setup, probe.query)
				fmt.Fprintf(GinkgoWriter, "CAST-BOUNDARY %s %s rows=%v err=%v\n", probe.name, result.Engine, result.Rows.Rows, result.Err)
				// Java's FLOAT_TO_LONG returns an Integer where its LONG-typed
				// result record requires Long. Pin that upstream boxing failure;
				// Go implements the same numeric operation with its LONG carrier.
				if probe.name == "float long boxing" && result.Engine == "java" {
					var je *plandiff.JavaError
					if !errors.As(result.Err, &je) || je.ExceptionClass != "IllegalArgumentException" || je.Message != "Wrong object type used with protocol message reflection.\nField number: 1, field java type: LONG, value type: java.lang.Integer\n" {
						failures = append(failures, fmt.Sprintf("FLOAT_TO_LONG Java boxing contract changed: %v, %v", result.Rows.Rows, result.Err))
					}
					continue
				}
				// SQL ARRAY targets declare non-nullable elements. Java's cast
				// preserves a NULL but the live derived query throws NPE; Go's
				// checked layout rejects that invalid element rather than panicking.
				if probe.name == "computed array null element" {
					if result.Engine == "java" {
						var je *plandiff.JavaError
						if !errors.As(result.Err, &je) || je.ExceptionClass != "NullPointerException" || je.Message != "java.lang.NullPointerException" {
							failures = append(failures, fmt.Sprintf("derived NULL-array Java failure changed: %v, %v", result.Rows.Rows, result.Err))
						}
					} else {
						var coded interface {
							Code() values.ResolutionErrorCode
						}
						if !errors.As(result.Err, &coded) || coded.Code() != values.LayoutNullabilityMismatch {
							failures = append(failures, fmt.Sprintf("derived NULL-array Go rejection changed: %v, %v", result.Rows.Rows, result.Err))
						}
					}
					continue
				}
				if probe.name == "duplicate element ordinal referenced" {
					var javaErr *plandiff.JavaError
					var goErr *api.Error
					if !(errors.As(result.Err, &javaErr) && javaErr.SQLState == "42702") && !(errors.As(result.Err, &goErr) && goErr.Code == api.ErrCodeAmbiguousColumn) {
						failures = append(failures, fmt.Sprintf("duplicate output reference/%s: got %v / %v, want 42702", result.Engine, result.Rows.Rows, result.Err))
					}
					continue
				}
				rowsMatch := reflect.DeepEqual(result.Rows.Rows, probe.want) || len(result.Rows.Rows) == 0 && len(probe.want) == 0
				if result.Err != nil || !rowsMatch {
					failures = append(failures, fmt.Sprintf("%s/%s: got %v error %v, want %v", probe.name, result.Engine, result.Rows.Rows, result.Err, probe.want))
				}
			}
		}
		const nestedSchema = "CREATE TYPE AS STRUCT leaf (tag STRING, vals DOUBLE ARRAY) CREATE TYPE AS STRUCT item (item_id BIGINT, n leaf) CREATE TABLE t (id BIGINT, items item ARRAY, PRIMARY KEY (id))"
		nestedSetup := []string{"INSERT INTO t VALUES (1, [(1, ('leaf', [1.0E20, -1.0E20]))])"}
		for _, probe := range []struct {
			query     string
			ambiguous bool
		}{
			{`SELECT CAST(x AS BIGINT) FROM t, t.items x, x.n.vals x`, true},
			{`SELECT CAST(x AS BIGINT) FROM t, t.items x AT xp, x.n.vals x AT yp`, true},
			{`SELECT 1 FROM t, t.items x, x.n.vals x`, false},
			{`SELECT 1 FROM t, t.items x AT xp, x.n.vals x AT yp`, false},
			{`SELECT 1 FROM t AS items, items.items, items.n.vals x`, false},
		} {
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				result := runner.RunWithSetup(ctx, nestedSchema, nestedSetup, probe.query)
				fmt.Fprintf(GinkgoWriter, "CAST-CHAIN %s %s rows=%v err=%v\n", probe.query, result.Engine, result.Rows.Rows, result.Err)
				if probe.ambiguous {
					var javaErr *plandiff.JavaError
					var goErr *api.Error
					if !(errors.As(result.Err, &javaErr) && javaErr.SQLState == "42702") && !(errors.As(result.Err, &goErr) && goErr.Code == api.ErrCodeAmbiguousColumn) {
						failures = append(failures, fmt.Sprintf("chained duplicate reference/%s: %v, want 42702", result.Engine, result.Err))
					}
				} else if result.Err != nil || !reflect.DeepEqual(result.Rows.Rows, [][]any{{float64(1)}, {float64(1)}}) {
					failures = append(failures, fmt.Sprintf("chained duplicate unused/%s: rows %v, err %v", result.Engine, result.Rows.Rows, result.Err))
				}
			}
		}
		const aliasBaseSchema = "CREATE TYPE AS STRUCT leaf (tag STRING, vals BIGINT ARRAY) CREATE TYPE AS STRUCT item (item_id BIGINT, nested leaf ARRAY) CREATE TABLE t (id BIGINT, a BIGINT ARRAY, items item ARRAY, PRIMARY KEY (id)) "
		for _, probe := range []struct {
			query                              string
			want                               [][]any
			state                              string
			noCompetingColumn, javaPlanFailure bool
		}{
			{`SELECT v FROM t, t.a v, u v ORDER BY v`, nil, "42702", false, false},
			{`SELECT v FROM t, t.a v, u v`, nil, "42702", false, false},
			{`SELECT v FROM u v, t, t.a v ORDER BY v`, nil, "42702", false, false},
			{`SELECT v FROM u v, t, t.a v`, nil, "42702", false, false},
			{`SELECT v FROM t, t.a v, u`, nil, "42702", false, false},
			{`SELECT a FROM (SELECT CAST(a AS BIGINT ARRAY) a FROM t) a, a.a`, nil, "42703", false, false},
			{`SELECT a FROM (SELECT CAST(a AS BIGINT ARRAY) a FROM t) a, a.a, u AS "Q$DUP1"`, nil, "42703", false, false},
			{`SELECT 1 FROM (SELECT CAST(a AS BIGINT ARRAY) a FROM t) a, a.a`, nil, "42703", false, false},
			{`SELECT a FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) a, a.a`, nil, "42702", false, false},
			{`SELECT 1 FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) a, a.a`, [][]any{{float64(1)}, {float64(1)}}, "", false, false},
			{`SELECT 1 FROM (SELECT CAST(a AS BIGINT ARRAY) a FROM t) q, q.a`, nil, "42703", false, false},
			{`SELECT 1 FROM (SELECT CAST(a AS BIGINT ARRAY) AS a FROM t) q, q.a`, [][]any{{float64(1)}, {float64(1)}}, "", false, false},
			{`SELECT q.x FROM (SELECT id x FROM t) q`, nil, "42703", false, false},
			{`SELECT q.id FROM (SELECT id x FROM t) q`, [][]any{{float64(1)}}, "", false, false},
			{`SELECT q.x FROM (SELECT id AS x FROM t) q`, [][]any{{float64(1)}}, "", false, false},
			{`SELECT v + 1 x FROM u ORDER BY x`, nil, "42703", false, false},
			{`SELECT v FROM t, t.a v, u v`, [][]any{{float64(10)}, {float64(20)}}, "", true, false},
			{`SELECT v FROM t, t.a v, u v ORDER BY v`, [][]any{{float64(10)}, {float64(20)}}, "", true, true},
			{`SELECT v FROM u v, t, t.a v ORDER BY v`, [][]any{{float64(10)}, {float64(20)}}, "", true, true},
			{`SELECT y FROM t, t.items x, x.nested.vals y`, nil, "42703", false, false},
		} {
			schema := aliasBaseSchema + "CREATE TABLE u (id BIGINT, v BIGINT, PRIMARY KEY (id))"
			setup := []string{"INSERT INTO t VALUES (1, [10, 20], [(1, [('leaf', [1, 2])])])", "INSERT INTO u VALUES (1, 999)"}
			if probe.noCompetingColumn {
				schema = aliasBaseSchema + "CREATE TABLE u (id BIGINT, PRIMARY KEY (id))"
				setup[1] = "INSERT INTO u VALUES (1)"
			}
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				result := runner.RunWithSetup(ctx, schema, setup, probe.query)
				fmt.Fprintf(GinkgoWriter, "LATERAL-CONTRACT no-competing-column=%t %s %s rows=%v err=%v\n", probe.noCompetingColumn, probe.query, result.Engine, result.Rows.Rows, result.Err)
				// Java Cascades has no physical sort fallback; Go's sanctioned
				// read-side extension answers these ordered rows with one.
				if probe.javaPlanFailure && result.Engine == "java" {
					var je *plandiff.JavaError
					if !errors.As(result.Err, &je) || je.ExceptionClass != "UnableToPlanException" {
						failures = append(failures, fmt.Sprintf("ordered lateral Java planning contract changed: %v", result.Err))
					}
					continue
				}
				if probe.state != "" {
					var je *plandiff.JavaError
					var ge *api.Error
					if !(errors.As(result.Err, &je) && je.SQLState == probe.state) && !(errors.As(result.Err, &ge) && string(ge.Code) == probe.state) {
						failures = append(failures, fmt.Sprintf("lateral %s/%s: got %v, want %s", probe.query, result.Engine, result.Err, probe.state))
					}
				} else if result.Err != nil || !reflect.DeepEqual(result.Rows.Rows, probe.want) {
					failures = append(failures, fmt.Sprintf("lateral %s/%s: rows=%v err=%v want=%v", probe.query, result.Engine, result.Rows.Rows, result.Err, probe.want))
				}
			}
		}
		const publicationSchema = "CREATE TYPE AS STRUCT elem (k BIGINT, n BIGINT) CREATE TABLE t (id BIGINT, a BIGINT ARRAY, items elem ARRAY, PRIMARY KEY (id)) CREATE TABLE u (id BIGINT, v BIGINT, PRIMARY KEY (id))"
		publicationSetup := []string{"INSERT INTO t VALUES (1, [10, 20], [(7, 70), (8, 80)])", "INSERT INTO u VALUES (1, 999)"}
		for _, probe := range []struct {
			query           string
			columns         []string
			rows            int
			want            [][]any
			state           string
			javaPlanFailure bool
		}{
			{`SELECT * FROM t, t.items x`, []string{"ID", "A", "ITEMS", "K", "N"}, 2, nil, "", false},
			{`SELECT x.* FROM t, t.items x`, []string{"K", "N"}, 2, [][]any{{float64(7), float64(70)}, {float64(8), float64(80)}}, "", false},
			{`SELECT * FROM (SELECT * FROM t, t.items x) d`, []string{"ID", "A", "ITEMS", "K", "N"}, 2, nil, "", false},
			{`WITH d AS (SELECT * FROM t, t.items x) SELECT * FROM d`, []string{"ID", "A", "ITEMS", "K", "N"}, 2, nil, "", false},
			{`SELECT d.k FROM (SELECT * FROM t, t.items x) d`, []string{"K"}, 2, [][]any{{float64(7)}, {float64(8)}}, "", false},
			{`WITH d AS (SELECT x.* FROM t, t.items x) SELECT d.k FROM d`, []string{"K"}, 2, [][]any{{float64(7)}, {float64(8)}}, "", false},
			{`SELECT d.x FROM (SELECT * FROM t, t.items x) d`, nil, 0, nil, "42703", false},
			{`SELECT x.* FROM t, t.items x AT p`, []string{"X", "P"}, 2, nil, "", false},
			{`SELECT d.x.k FROM (SELECT * FROM t, t.items x AT p) d`, []string{"K"}, 2, [][]any{{float64(7)}, {float64(8)}}, "", false},
			{`SELECT d.k FROM (SELECT * FROM t, t.items x AT p) d`, nil, 0, nil, "42703", false},
			{`SELECT "BOGUS".* FROM t`, nil, 0, nil, "42703", false},
			{`SELECT wrong.* FROM t`, nil, 0, nil, "42703", false},
			{`SELECT nope.* FROM t, u`, nil, 0, nil, "42703", false},
			{`SELECT c.*, u.v FROM t, u`, nil, 0, nil, "42703", false},
			{`SELECT v.* FROM t, t.a v`, nil, 0, nil, "42F10", false},
			{`SELECT v.* FROM t, t.a v AT p`, []string{"V", "P"}, 2, [][]any{{float64(10), float64(1)}, {float64(20), float64(2)}}, "", false},
			{`SELECT v.* FROM t, t.a v, u v`, nil, 0, nil, "42F10", false},
			{`SELECT v.* FROM u v, t, t.a v`, []string{"ID", "V"}, 2, [][]any{{float64(1), float64(999)}, {float64(1), float64(999)}}, "", false},
			{`SELECT a.* FROM t`, nil, 0, nil, "42F10", false},
			{`SELECT items.* FROM t`, nil, 0, nil, "42F10", false},
			{`SELECT * FROM u v, t, t.a v`, []string{"ID", "V", "ID", "A", "ITEMS", "V"}, 2, nil, "", false},
			{`SELECT d.v FROM (SELECT * FROM u v, t, t.a v) d`, nil, 0, nil, "42702", false},
			{`WITH d AS (SELECT * FROM u v, t, t.a v) SELECT d.v FROM d`, nil, 0, nil, "42702", false},
			{`WITH d AS (SELECT id AS k, id AS k, id AS n FROM t), e AS (SELECT DISTINCT * FROM d) SELECT e.k FROM e`, nil, 0, nil, "42702", false},
			{`WITH d AS (SELECT id AS k, id AS k, id AS n FROM t), e AS (SELECT DISTINCT * FROM d) SELECT e.k_2 FROM e`, nil, 0, nil, "42703", false},
			{`WITH d AS (SELECT id AS k, id AS k, id AS n FROM t), e AS (SELECT * FROM d) SELECT e.n FROM e`, []string{"N"}, 1, [][]any{{float64(1)}}, "", false},
			{`WITH d AS (SELECT CAST(id AS BIGINT) FROM t) SELECT * FROM d`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`WITH d AS (SELECT CAST(id AS BIGINT) FROM t) SELECT u.id, d.* FROM u, d`, []string{"ID", "_1"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
			{`SELECT t.*, v.* FROM u v, t, t.a v`, []string{"ID", "A", "ITEMS", "ID", "V"}, 2, nil, "", false},
			{`SELECT x.* FROM u x, t, t.items x`, []string{"ID", "V"}, 2, [][]any{{float64(1), float64(999)}, {float64(1), float64(999)}}, "", false},
			{`SELECT x.* FROM t, t.items x, u x`, []string{"K", "N"}, 2, [][]any{{float64(7), float64(70)}, {float64(8), float64(80)}}, "", false},
			{`SELECT v.* FROM t, t.a v AT v`, []string{"V", "V"}, 2, [][]any{{float64(10), float64(1)}, {float64(20), float64(2)}}, "", false},
			{`SELECT id x FROM t`, []string{"ID"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT id AS x FROM t`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT CAST(id AS BIGINT) x FROM t`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT CAST(id AS BIGINT) AS x FROM t`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT SUM(id) x FROM t`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT SUM(id) AS x FROM t`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT COUNT(*) x FROM t`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT COUNT(*) AS x FROM t`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT q.x FROM (SELECT COUNT(*) AS x FROM t GROUP BY id) q`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT q.x FROM (SELECT COUNT(*) x FROM t GROUP BY id) q`, nil, 0, nil, "42703", false},
			{`WITH q AS (SELECT COUNT(*) AS x FROM t GROUP BY id) SELECT q.x FROM q`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT id+1 FROM t GROUP BY id+1`, []string{"_0"}, 1, [][]any{{float64(2)}}, "", true},
			{`SELECT id+1, COUNT(*) FROM t GROUP BY id+1`, []string{"_0", "_1"}, 1, [][]any{{float64(2), float64(1)}}, "", true},
			{`SELECT q."ID+1" FROM (SELECT id+1 FROM t GROUP BY id+1) q`, nil, 0, nil, "42703", false},
			{`SELECT q."ID+1" FROM (SELECT id+1, COUNT(*) FROM t GROUP BY id+1) q`, nil, 0, nil, "42703", false},
			{`SELECT q."ID+1" FROM (SELECT id+1 x, COUNT(*) FROM t GROUP BY id+1) q`, nil, 0, nil, "42703", false},
			{`SELECT q.x FROM (SELECT id+1 AS x, COUNT(*) FROM t GROUP BY id+1) q`, []string{"X"}, 1, [][]any{{float64(2)}}, "", true},
			{`SELECT q."ID+1" FROM (SELECT id+1 FROM t GROUP BY id+1 HAVING COUNT(*) > 0) q`, nil, 0, nil, "42703", false},
			{`SELECT COUNT(*) x, SUM(id) y FROM t`, []string{"_0", "_1"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
			{`SELECT COUNT(*) AS x, SUM(id) AS y FROM t`, []string{"X", "Y"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
			{`SELECT id x FROM t ORDER BY id`, []string{"ID"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT id x FROM t ORDER BY x`, nil, 0, nil, "42703", false},
			{`SELECT CAST(id AS BIGINT) x FROM t WHERE x = 1`, nil, 0, nil, "42703", false},
			{`SELECT q.x FROM (SELECT CAST(id AS BIGINT) x FROM t) q WHERE q.x = 1`, nil, 0, nil, "42703", false},
			{`SELECT q.x FROM (SELECT CAST(id AS BIGINT) AS x FROM t) q WHERE q.x = 1`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`WITH q AS (SELECT CAST(id AS BIGINT) x FROM t) SELECT q.x FROM q WHERE q.x = 1`, nil, 0, nil, "42703", false},
			{`WITH q AS (SELECT CAST(id AS BIGINT) AS x FROM t) SELECT q.x FROM q WHERE q.x = 1`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT q."_0" FROM (SELECT CAST(id AS BIGINT) x FROM t) q`, nil, 0, nil, "42703", false},
			{`SELECT q."CAST(ID AS BIGINT)" FROM (SELECT CAST(id AS BIGINT) x FROM t) q`, nil, 0, nil, "42703", false},
			{`SELECT q.x FROM (SELECT COUNT(*) x FROM t) q`, nil, 0, nil, "42703", false},
			{`SELECT q."COUNT(*)" FROM (SELECT COUNT(*) x FROM t) q`, nil, 0, nil, "42703", false},
			{`SELECT q."_0" FROM (SELECT COUNT(*) x FROM t) q`, nil, 0, nil, "42703", false},
			{`SELECT "_0" FROM (SELECT COUNT(*) FROM t) q`, nil, 0, nil, "42703", false},
			{`SELECT _0 FROM (SELECT COUNT(*) FROM t) q`, nil, 0, nil, "42601", false},
			{`WITH c1(w, z) AS (SELECT id, id+10 AS n FROM t), c2(a, b) AS (WITH c1(x, y) AS (SELECT id+20 AS m, id+30 AS n FROM t) SELECT * FROM c1) SELECT * FROM c1, c2`, []string{"W", "Z", "A", "B"}, 1, [][]any{{float64(1), float64(11), float64(21), float64(31)}}, "", false},
			{`SELECT q."COUNT(*)" FROM (SELECT COUNT(*) AS "COUNT(*)" FROM t) q`, nil, 0, nil, "42602", false},
			{`SELECT SUM(id) AS "SUM(ID)" FROM t`, nil, 0, nil, "42602", false},
			{`SELECT q."CAST(ID AS BIGINT)" FROM (SELECT CAST(id AS BIGINT) AS "CAST(ID AS BIGINT)" FROM t) q`, nil, 0, nil, "42602", false},
			{`SELECT * FROM (SELECT COUNT(*) x FROM t) q`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT q.* FROM (SELECT CAST(id AS BIGINT) x FROM t) q`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT COUNT(*) x FROM t ORDER BY "_0"`, nil, 0, nil, "42703", false},
			{`SELECT q."_0" FROM (SELECT COUNT(*) AS "_0" FROM t) q`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT q."a.b" FROM (SELECT COUNT(*) AS "a.b" FROM t) q`, []string{"a.b"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT q."a$b" FROM (SELECT CAST(id AS BIGINT) AS "a$b" FROM t) q`, []string{"a$b"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT id AS "bad()" FROM t`, nil, 0, nil, "42602", false},
			{`SELECT missing AS "bad()" FROM t`, nil, 0, nil, "42703", false},
			{`SELECT COUNT(*) "bad()" FROM t`, []string{"_0"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT SUM(id) AS s, id FROM t GROUP BY id`, []string{"S", "ID"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
			{`SELECT q.s, q.id FROM (SELECT SUM(id) AS s, id FROM t GROUP BY id) q`, []string{"S", "ID"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
			{`SELECT g FROM t GROUP BY id AS g`, []string{"G"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT g x FROM t GROUP BY id AS g`, []string{"G"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT g AS x FROM t GROUP BY id AS g`, []string{"X"}, 1, [][]any{{float64(1)}}, "", false},
			{`SELECT g, COUNT(*) FROM t GROUP BY id AS g`, []string{"G", "_1"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
			{`SELECT COUNT(*), g FROM t GROUP BY id AS g`, []string{"_0", "G"}, 1, [][]any{{float64(1), float64(1)}}, "", false},
		} {
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				result := runner.RunWithSetup(ctx, publicationSchema, publicationSetup, probe.query)
				fmt.Fprintf(GinkgoWriter, "STAR-PUBLICATION %s %s columns=%v rows=%v err=%v\n", result.Engine, probe.query, result.Rows.Columns, result.Rows.Rows, result.Err)
				// Computed grouping requires an ordering Java cannot supply on
				// this schema. Go's physical sort extension supplies it; semantic
				// name absence is still specified by Java Expressions.getStructType.
				if probe.javaPlanFailure && result.Engine == "java" {
					var je *plandiff.JavaError
					if !errors.As(result.Err, &je) || je.ExceptionClass != "UnableToPlanException" || je.SQLState != "0AF00" {
						failures = append(failures, fmt.Sprintf("computed grouping Java planning contract changed: %s: %v", probe.query, result.Err))
					}
					continue
				}
				if probe.state != "" {
					var je *plandiff.JavaError
					var ge *api.Error
					if !(errors.As(result.Err, &je) && je.SQLState == probe.state) && !(errors.As(result.Err, &ge) && string(ge.Code) == probe.state) {
						failures = append(failures, fmt.Sprintf("star %s/%s: %v, want %s", probe.query, result.Engine, result.Err, probe.state))
					}
					continue
				}
				var names []string
				for _, column := range result.Rows.Columns {
					names = append(names, column.Name)
				}
				if result.Err != nil || !reflect.DeepEqual(names, probe.columns) || len(result.Rows.Rows) != probe.rows || (probe.want != nil && !reflect.DeepEqual(result.Rows.Rows, probe.want)) {
					failures = append(failures, fmt.Sprintf("star %s/%s: columns=%v rows=%v err=%v; want columns=%v row-count=%d rows=%v", probe.query, result.Engine, names, result.Rows.Rows, result.Err, probe.columns, probe.rows, probe.want))
				}
			}
		}

		Expect(failures).To(BeEmpty())
	})
})

// This is a Java reference run, not a shared DDL conformance claim: the Go
// execution twin uses authored protobuf metadata in TestFDB_EnumTransport.
var _ = Describe("EnumTransportReference", func() {
	It("pins the Java enum comparison and JDBC metadata contract", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "enumreference_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		const schema = "CREATE TYPE AS ENUM workflow ('ZULU', 'MIDDLE', 'ALPHA', 'CASH$') CREATE TABLE tasks (id BIGINT, state workflow, PRIMARY KEY(id)) CREATE INDEX state_idx AS SELECT state FROM tasks ORDER BY state"
		setup := []string{"INSERT INTO tasks VALUES (1, 'ALPHA'), (2, 'ZULU'), (3, 'MIDDLE'), (4, NULL)"}
		for _, tc := range []struct {
			name, query string
			rows        [][]any
			state       string
			errorClass  string
			typeName    string
		}{
			{name: "equal", query: "SELECT id FROM tasks WHERE state = 'ZULU' ORDER BY id", rows: [][]any{{float64(2)}}},
			{name: "unequal", query: "SELECT id FROM tasks WHERE state <> 'ZULU' ORDER BY id", rows: [][]any{{float64(1)}, {float64(3)}}},
			{name: "ordered", query: "SELECT id FROM tasks WHERE state < 'MIDDLE' ORDER BY id", rows: [][]any{{float64(2)}}},
			{name: "reversed", query: "SELECT id FROM tasks WHERE 'MIDDLE' > state ORDER BY id", rows: [][]any{{float64(2)}}},
			// InOpValue injects an ARRAY<ENUM> PromoteValue. Its eval's
			// descriptor selection assumes every non-enum complex target is
			// RECORD and asserts before converting the string list.
			{name: "in", query: "SELECT id FROM tasks WHERE state IN ('ALPHA', 'MIDDLE') ORDER BY id", state: "XXXXX", errorClass: "VerifyException"},
			{name: "null", query: "SELECT id FROM tasks WHERE state IS NULL", rows: [][]any{{float64(4)}}},
			{name: "cte", query: "WITH a AS (SELECT id, state FROM tasks), b AS (SELECT * FROM a) SELECT id FROM b WHERE state = 'ZULU'", rows: [][]any{{float64(2)}}},
			{name: "metadata", query: "SELECT state FROM tasks WHERE id = 1", rows: [][]any{{"ALPHA"}}, typeName: "OTHER"},
			{name: "invalid_equal", query: "SELECT id FROM tasks WHERE state = 'NOT_A_MEMBER'", state: "XX000"},
			{name: "invalid_in", query: "SELECT id FROM tasks WHERE state IN ('ALPHA', 'NOT_A_MEMBER')", state: "XX000", errorClass: "VerifyException"},
		} {
			got := runner.RunWithSetup(ctx, schema, setup, tc.query)
			fmt.Fprintf(GinkgoWriter, "ENUM-REFERENCE %s rows=%v columns=%v err=%v\n", tc.name, got.Rows.Rows, got.Rows.Columns, got.Err)
			if tc.state != "" {
				var javaErr *plandiff.JavaError
				Expect(errors.As(got.Err, &javaErr)).To(BeTrue(), tc.name)
				Expect(javaErr.SQLState).To(Equal(tc.state), tc.name)
				if tc.errorClass == "VerifyException" {
					Expect(javaErr.ExceptionClass).To(Equal("VerifyException"), tc.name)
					Expect(javaErr.Message).To(Equal("com.google.common.base.VerifyException"), tc.name)
					continue
				}
				Expect(javaErr.ExceptionClass).To(Equal("SemanticException"), tc.name)
				Expect(javaErr.Message).To(Equal("Invalid enum value for the enum type NOT_A_MEMBER"), tc.name)
				continue
			}
			Expect(got.Err).NotTo(HaveOccurred(), tc.name)
			Expect(got.Rows.Rows).To(Equal(tc.rows), tc.name)
			if tc.typeName != "" {
				Expect(got.Rows.Columns).To(HaveLen(1), tc.name)
				Expect(got.Rows.Columns[0].Type).To(Equal(tc.typeName), tc.name)
			}
		}
	})
})

// Scalar query expressions are a Go extension; Java's EXISTS path supplies the
// shared derived-source contract, not a catalog-table error for the derived alias.
var _ = Describe("DerivedSourceReference", func() {
	It("distinguishes unsupported scalar syntax from valid derived EXISTS binding", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "derivedreference_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		const schema = "CREATE TABLE ord (order_id BIGINT, cust_id BIGINT, PRIMARY KEY (order_id))"
		setup := []string{"INSERT INTO ord VALUES (1, 10), (2, 20)"}
		for _, tc := range []struct {
			name, query, state string
			rows               [][]any
		}{
			{"scalar primary", "SELECT o.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord) AS d WHERE d.order_id = o.order_id) FROM ord AS o", "42601", nil},
			{"scalar leg", "SELECT o.order_id, (SELECT a.cust_id FROM ord a, (SELECT order_id FROM ord) AS d WHERE a.order_id = o.order_id) FROM ord AS o", "42601", nil},
			{"scalar limit", "SELECT (SELECT a.cust_id FROM ord a WHERE a.order_id = o.order_id LIMIT 5) FROM ord o", "42601", nil},
			{"scalar where limit", "SELECT o.order_id FROM ord o WHERE o.cust_id = (SELECT a.cust_id FROM ord a WHERE a.order_id = o.order_id LIMIT 2)", "42601", nil},
			{"scalar distinct", "SELECT (SELECT DISTINCT a.cust_id FROM ord a WHERE a.order_id = o.order_id) FROM ord o", "42601", nil},
			{"scalar group having", "SELECT (SELECT a.cust_id FROM ord a WHERE a.order_id = o.order_id GROUP BY a.cust_id HAVING a.cust_id = 10) FROM ord o", "42601", nil},
			{"exists body where", "SELECT o.order_id, EXISTS (SELECT o.order_id FROM (SELECT order_id FROM ord) AS d WHERE d.order_id = 1) FROM ord AS o ORDER BY o.order_id", "", [][]any{{float64(1), true}, {float64(2), true}}},
			{"exists empty where", "SELECT o.order_id, EXISTS (SELECT o.order_id FROM (SELECT order_id FROM ord) AS d WHERE d.order_id = 100) FROM ord AS o ORDER BY o.order_id", "", [][]any{{float64(1), false}, {float64(2), false}}},
			{"exists derived leg", "SELECT o.order_id, EXISTS (SELECT o.order_id FROM ord a, (SELECT order_id FROM ord) AS d WHERE a.order_id = d.order_id) FROM ord AS o ORDER BY o.order_id", "", [][]any{{float64(1), true}, {float64(2), true}}},
			{"exists shadowed cluster", "SELECT o.order_id, EXISTS (SELECT o.order_id FROM (SELECT order_id FROM ord WHERE order_id = 1) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS d ORDER BY o.order_id", "", [][]any{{float64(1), true}, {float64(1), true}, {float64(2), false}, {float64(2), false}}},
			{"exists star cluster", "SELECT o.order_id, EXISTS (SELECT o.order_id FROM (SELECT * FROM ord WHERE order_id = 1) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS other ORDER BY o.order_id", "", [][]any{{float64(1), true}, {float64(1), true}, {float64(2), false}, {float64(2), false}}},
			{"exists empty star cluster", "SELECT o.order_id, EXISTS (SELECT o.order_id FROM (SELECT * FROM ord WHERE order_id = 100) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS other ORDER BY o.order_id", "", [][]any{{float64(1), false}, {float64(1), false}, {float64(2), false}, {float64(2), false}}},
			{"exists computed ordering unavailable", "SELECT o.order_id * 10 + other.order_id AS pair_id, EXISTS (SELECT o.order_id FROM (SELECT order_id FROM ord WHERE order_id = 1) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS other ORDER BY pair_id", "0AF00", nil},
			{"exists both outer rows live", "SELECT o.order_id * 10 + other.order_id, EXISTS (SELECT o.order_id FROM (SELECT order_id FROM ord WHERE order_id = 1) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS other", "", [][]any{{float64(11), true}, {float64(12), true}, {float64(21), false}, {float64(22), false}}},
		} {
			result := runner.RunWithSetup(ctx, schema, setup, tc.query)
			fmt.Fprintf(GinkgoWriter, "DERIVED-REFERENCE %s rows=%v err=%v\n", tc.name, result.Rows.Rows, result.Err)
			if tc.state != "" {
				var javaErr *plandiff.JavaError
				Expect(errors.As(result.Err, &javaErr)).To(BeTrue(), tc.name)
				Expect(javaErr.SQLState).To(Equal(tc.state), tc.name)
				continue
			}
			Expect(result.Err).NotTo(HaveOccurred(), tc.name)
			if tc.name == "exists both outer rows live" {
				// This exact Go regression has no ORDER BY. Preserve multiplicity
				// without imposing a scan order on Java's join enumeration.
				Expect(result.Rows.Rows).To(ConsistOf(tc.rows), tc.name)
			} else {
				Expect(result.Rows.Rows).To(Equal(tc.rows), tc.name)
			}
		}
	})
})

var _ = Describe("IndependentOuterBlockReference", func() {
	It("preserves predicate-free cross product rows and the scalar extension boundary", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "outerblock_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		const schema = "CREATE TABLE ta (aid BIGINT, k BIGINT, av BIGINT, PRIMARY KEY (aid)) CREATE TABLE tb (bid BIGINT, bv BIGINT, PRIMARY KEY (bid)) CREATE TABLE tc (cid BIGINT, k BIGINT, cv BIGINT, cw BIGINT, PRIMARY KEY (cid)) CREATE TABLE tp (pid BIGINT, owner BIGINT, PRIMARY KEY (pid))"
		setup := []string{"INSERT INTO ta VALUES (1,101,201)", "INSERT INTO tb VALUES (1,301)", "INSERT INTO tc VALUES (1,901,951,971)", "INSERT INTO tp VALUES (401,1)"}
		const query = "SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) FROM ta, tb, tc"
		result := runner.RunWithSetup(ctx, schema, setup, query)
		fmt.Fprintf(GinkgoWriter, "OUTER-BLOCK original rows=%v err=%v\n", result.Rows.Rows, result.Err)
		Expect(result.Err).NotTo(HaveOccurred())
		Expect(result.Rows.Rows).To(Equal([][]any{{float64(901), true}}))
		setup = append(setup, "INSERT INTO ta VALUES (2,102,202)", "INSERT INTO tb VALUES (2,302)", "INSERT INTO tc VALUES (2,902,952,972)")
		result = runner.RunWithSetup(ctx, schema, setup, query)
		fmt.Fprintf(GinkgoWriter, "OUTER-BLOCK multiplicity rows=%v err=%v\n", result.Rows.Rows, result.Err)
		Expect(result.Err).NotTo(HaveOccurred())
		Expect(result.Rows.Rows).To(ConsistOf([][]any{{float64(901), true}, {float64(901), true}, {float64(902), true}, {float64(902), true}, {float64(901), false}, {float64(901), false}, {float64(902), false}, {float64(902), false}}))
		const shadowSchema = "CREATE TABLE LA (AID BIGINT, K BIGINT, ARR INTEGER ARRAY, PRIMARY KEY (AID)) CREATE TABLE LB (BID BIGINT, K BIGINT, PRIMARY KEY (BID)) CREATE TABLE CC (CID BIGINT, CV BIGINT, PRIMARY KEY (CID))"
		shadow := runner.RunWithSetup(ctx, shadowSchema, []string{"INSERT INTO LA VALUES (1,100,[7,8]),(2,110,[9])", "INSERT INTO LB VALUES (1,5),(3,6)", "INSERT INTO CC VALUES (1,900)"}, `WITH "V" AS (SELECT "BID" AS "B" FROM LB) SELECT (WITH "V" AS (SELECT LB."K" AS "B" FROM "V" LEFT JOIN CC ON "V"."B" = CC."CID" LEFT JOIN LA ON "V"."B" = LA."AID") SELECT COUNT(*) FROM "V", CC WHERE "V"."B" = 100) FROM LB LIMIT 1`)
		fmt.Fprintf(GinkgoWriter, "OUTER-BLOCK shadow scalar err=%v\n", shadow.Err)
		var javaErr *plandiff.JavaError
		Expect(errors.As(shadow.Err, &javaErr)).To(BeTrue())
		Expect(javaErr.SQLState).To(Equal("42601"))
	})
})

var _ = Describe("AggregateMetadataIntegrationReference", func() {
	It("distinguishes repaired COUNT metadata from the grouped-sort extension", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "aggregatemetadata_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		// The T1 queries preceding aggregate-empty-table.yamsql's first planner
		// rejection all assert BIGINT COUNT metadata, not nested array metadata.
		const schema = "CREATE TABLE T1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY(id))"
		for _, tc := range []struct {
			query   string
			grouped bool
		}{
			{"SELECT COUNT(*) FROM T1", false},
			{"SELECT COUNT(*) FROM T1 WHERE col1 = 0", false},
			{"SELECT COUNT(*) FROM T1 WHERE col1 > 0", false},
			{"SELECT COUNT(*) FROM T1 WHERE col1 = 0 GROUP BY col1", true},
		} {
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				got := runner.RunWithSetup(ctx, schema, nil, tc.query)
				fmt.Fprintf(GinkgoWriter, "AGGREGATE-METADATA %s %s rows=%v columns=%v err=%v\n", tc.query, got.Engine, got.Rows.Rows, got.Rows.Columns, got.Err)
				if tc.grouped && got.Engine == "java" {
					var javaErr *plandiff.JavaError
					Expect(errors.As(got.Err, &javaErr)).To(BeTrue(), tc.query)
					Expect(javaErr.SQLState).To(Equal("0AF00"), tc.query)
					continue
				}
				Expect(got.Err).NotTo(HaveOccurred(), tc.query)
				Expect(got.Rows.Columns).To(HaveLen(1), tc.query)
				Expect(got.Rows.Columns[0].Type).To(Equal("BIGINT"), tc.query)
				if tc.grouped {
					Expect(got.Rows.Rows).To(BeEmpty(), tc.query)
				} else {
					Expect(got.Rows.Rows).To(Equal([][]any{{float64(0)}}), tc.query)
				}
			}
		}
	})
})

var _ = Describe("ProjectionLabelIntegrationReference", func() {
	It("pins the label and rejection contracts behind the integration plan diffs", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "labelintegration_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const things = "CREATE TABLE things (id BIGINT, x BIGINT, arr BIGINT ARRAY, PRIMARY KEY(id))"
		for _, tc := range []struct {
			name, schema, query string
			setup               []string
			columns             []string
			rows                [][]any
			state, javaState    string
			plan                bool
		}{
			{
				name: "ordinal chain", plan: true,
				schema: "CREATE TABLE A (id BIGINT, b_id BIGINT, PRIMARY KEY(id)) CREATE TABLE B (id BIGINT, c_id BIGINT, PRIMARY KEY(id)) CREATE TABLE C (id BIGINT, name STRING, PRIMARY KEY(id))",
				setup:  []string{"INSERT INTO A VALUES (1,2)", "INSERT INTO B VALUES (2,3)", "INSERT INTO C VALUES (3,'end')"},
				query:  "SELECT a.id, c.name FROM A a, B b, C c WHERE a.b_id = b.id AND b.c_id = c.id", columns: []string{"ID", "NAME"}, rows: [][]any{{float64(1), "end"}},
			},
			{
				name: "ordinal star", plan: true,
				schema: "CREATE TABLE H (id BIGINT, PRIMARY KEY(id)) CREATE TABLE X (id BIGINT, hid BIGINT, v BIGINT, PRIMARY KEY(id)) CREATE TABLE Y (id BIGINT, hid BIGINT, w BIGINT, PRIMARY KEY(id))",
				setup:  []string{"INSERT INTO H VALUES (1)", "INSERT INTO X VALUES (2,1,20)", "INSERT INTO Y VALUES (3,1,30)"},
				query:  "SELECT h.id, x.v, y.w FROM H h, X x, Y y WHERE h.id = x.hid AND h.id = y.hid", columns: []string{"ID", "V", "W"}, rows: [][]any{{float64(1), float64(20), float64(30)}},
			},
			{
				name: "ordinal self", plan: true,
				schema: "CREATE TABLE NODE (id BIGINT, parent BIGINT, name STRING, PRIMARY KEY(id))", setup: []string{"INSERT INTO NODE VALUES (1,NULL,'grand'),(2,1,'parent'),(3,2,'child')"},
				query: "SELECT g.id, p.name, gp.name FROM NODE g, NODE p, NODE gp WHERE g.parent = p.id AND p.parent = gp.id", columns: []string{"ID", "NAME", "NAME"}, rows: [][]any{{float64(3), "parent", "grand"}},
			},
			{name: "derived duplicate", schema: things, setup: []string{"INSERT INTO things VALUES (1,5,[7,8])"}, query: "SELECT d.x FROM (SELECT * FROM things, things.arr AS x) d", state: "42702"},
			{name: "CTE duplicate", schema: things, setup: []string{"INSERT INTO things VALUES (1,5,[7,8])"}, query: "WITH d AS (SELECT * FROM things, things.arr AS x) SELECT d.x FROM d", state: "42702"},
			{name: "derived duplicate where", schema: things, setup: []string{"INSERT INTO things VALUES (1,5,[7,8])"}, query: "SELECT d.x FROM (SELECT * FROM things, things.arr AS x) d WHERE d.x = 8", state: "42702"},
			{name: "versioned derived duplicate", schema: things + " WITH OPTIONS(store_row_versions=true)", setup: []string{"INSERT INTO things VALUES (1,5,[7,8])"}, query: "SELECT d.x FROM (SELECT * FROM things, things.arr AS x) d", state: "42702"},
			{
				name: "derived shadows CTE ordered extension", schema: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY(id))", setup: []string{"INSERT INTO t VALUES (1,10),(2,20)"},
				query: "WITH x AS (SELECT id FROM t) SELECT x.*, x.id FROM (SELECT id, v FROM t WHERE id <= 2) AS x ORDER BY x.id", javaState: "0A000", columns: []string{"ID", "V", "ID"}, rows: [][]any{{float64(1), float64(10), float64(1)}, {float64(2), float64(20), float64(2)}},
			},
			{
				name: "derived shadows CTE", schema: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY(id))", setup: []string{"INSERT INTO t VALUES (1,10),(2,20)"},
				query: "WITH x AS (SELECT id FROM t) SELECT x.*, x.id FROM (SELECT id, v FROM t) AS x", columns: []string{"ID", "V", "ID"}, rows: [][]any{{float64(1), float64(10), float64(1)}, {float64(2), float64(20), float64(2)}},
			},
			{
				name: "aggregate renderer alias", schema: "CREATE TABLE scores (id BIGINT, player STRING, game STRING, score BIGINT, PRIMARY KEY(id))",
				query: `SELECT SUM(s.score) AS total, s.player AS "SUM(S.SCORE)" FROM scores s, scores t WHERE s.id = t.id GROUP BY s.player, s.game ORDER BY SUM(s.score) DESC`, state: "42602",
			},
			{
				name: "apostrophe alias", schema: `CREATE TABLE sales (id BIGINT, "Amount" BIGINT, PRIMARY KEY(id))`,
				query: `SELECT "Q'"."Z'", "R'"."Z'" FROM (SELECT "Amount" AS "Z'" FROM sales WHERE id = 1) "Q'", (SELECT "Amount" AS "Z'" FROM sales WHERE id = 2) "R'"`, state: "42602",
			},
			{
				name: "anonymous aggregate ordinal grouped-sort extension", javaState: "0AF00", schema: `CREATE TABLE rv (id BIGINT, "__ROW_VERSION" STRING, v BIGINT, PRIMARY KEY(id)) CREATE TABLE rw (id BIGINT, w BIGINT, PRIMARY KEY(id))`,
				setup: []string{"INSERT INTO rv VALUES (1,'a',10),(2,'b',20)", "INSERT INTO rw VALUES (1,5)"},
				query: `WITH d AS (SELECT * FROM rv, rw) SELECT d."__ROW_VERSION", COUNT(*) FROM d GROUP BY d."__ROW_VERSION"`, columns: []string{"__ROW_VERSION", "_1"}, rows: [][]any{{"a", float64(1)}, {"b", float64(1)}},
			},
			{
				name: "anonymous aggregate ordinal ordered PK", schema: "CREATE TABLE rv (id BIGINT, PRIMARY KEY(id))", setup: []string{"INSERT INTO rv VALUES (1),(2)"},
				query: "SELECT id, COUNT(*) FROM rv GROUP BY id", columns: []string{"ID", "_1"}, rows: [][]any{{float64(1), float64(1)}, {float64(2), float64(1)}},
			},
		} {
			if tc.plan {
				report := plandiff.Run(ctx, []plandiff.Query{{Name: tc.name, SchemaTemplate: tc.schema, SQL: tc.query}}, plandiff.NewGoEngine(), plandiff.NewJavaEngineHTTP(javaBaseURL(srv), env.ClusterFile))
				Expect(report.Cases).To(HaveLen(1), tc.name)
				got := report.Cases[0]
				Expect(got.Go.Err).NotTo(HaveOccurred(), tc.name)
				Expect(got.Java.Err).NotTo(HaveOccurred(), tc.name)
				Expect(got.Go.Tree).NotTo(BeEmpty(), tc.name)
				Expect(got.Java.Tree).NotTo(BeEmpty(), tc.name)
				// Go emits a logical tree here; Java emits a physical pipeline.
				// Retain both, but certify the column/row contract below, not text equality.
				fmt.Fprintf(GinkgoWriter, "LABEL-PLAN %s GO:\n%s\nJAVA:\n%s\n", tc.name, got.Go.Tree, got.Java.Tree)
			}
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				got := runner.RunWithSetup(ctx, tc.schema, tc.setup, tc.query)
				fmt.Fprintf(GinkgoWriter, "LABEL-INTEGRATION %s %s rows=%v columns=%v err=%v\n", tc.name, got.Engine, got.Rows.Rows, got.Rows.Columns, got.Err)
				state := tc.state
				if got.Engine == "java" && tc.javaState != "" {
					state = tc.javaState
				}
				if state != "" {
					var javaErr *plandiff.JavaError
					var goErr *api.Error
					if got.Engine == "java" {
						Expect(errors.As(got.Err, &javaErr)).To(BeTrue(), tc.name)
						Expect(javaErr.SQLState).To(Equal(state), tc.name)
					} else {
						Expect(errors.As(got.Err, &goErr)).To(BeTrue(), tc.name)
						Expect(string(goErr.Code)).To(Equal(state), tc.name)
					}
					continue
				}
				Expect(got.Err).NotTo(HaveOccurred(), tc.name)
				var names []string
				for _, column := range got.Rows.Columns {
					names = append(names, column.Name)
				}
				Expect(names).To(Equal(tc.columns), tc.name)
				Expect(got.Rows.Rows).To(ConsistOf(tc.rows), tc.name)
			}
		}
	})
})
