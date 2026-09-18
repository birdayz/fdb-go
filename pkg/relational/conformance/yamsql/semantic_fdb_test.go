package yamsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

func runSemanticScenario(t *testing.T, s *yamsql.Scenario) *yamsql.Result {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	name := sanitize(t.Name())
	path := "/_" + name
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=conf", path, clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scenarioBudget)
	defer cancel()
	r, err := yamsql.Run(ctx, s, yamsql.RunConfig{DB: db, DBPath: path, TemplateName: "TMPL_" + name})
	if err != nil {
		t.Fatal(err)
	}
	if r.SetupError != nil {
		t.Fatal(r.SetupError)
	}
	return r
}

func TestNumericEnvelopeFDB(t *testing.T) {
	t.Parallel()
	s := yamsql.NumericScenarioForTest(t)
	r := runSemanticScenario(t, s)
	yamsql.CheckNumericResultForTest(t, s, r)
}

func TestExactRunnerFDB(t *testing.T) {
	t.Parallel()
	scalar := func(kind, value string) yamsql.Scalar { return yamsql.Scalar{Kind: kind, Value: &value} }
	exact := func(values ...yamsql.Scalar) *[][]yamsql.Scalar { rows := [][]yamsql.Scalar{values}; return &rows }
	one := int64(1)
	empty := [][]yamsql.Scalar{}
	s := &yamsql.Scenario{Name: "exact-routes", SchemaTemplate: "CREATE TABLE t (id BIGINT, x DOUBLE, PRIMARY KEY(id))", Tests: []yamsql.Test{
		{Exec: "INSERT INTO t VALUES (?, ?)", Args: []yamsql.Scalar{scalar("int64", "1"), scalar("float64", "8000000000000000")}, Rowcount: &one},
		{Query: "SELECT ? / 2 FROM t", Args: []yamsql.Scalar{scalar("float64", "4008000000000000")}, ExactRows: exact(scalar("float64", "3ff8000000000000")), ColumnTypes: []string{"DOUBLE"}, PlanContains: "Project([(3 / 2)]"},
		{Query: "SELECT x FROM t", ExactRows: exact(scalar("float64", "8000000000000000")), ColumnTypes: []string{"DOUBLE"}},
		{Query: "SELECT x FROM t WHERE id = ?", Args: []yamsql.Scalar{scalar("int64", "2")}, ExactRows: &empty, ColumnTypes: []string{"DOUBLE"}},
		{Query: "SELECT 1 / ? FROM t", Args: []yamsql.Scalar{scalar("int64", "0")}, ErrorCode: "22012"},
		{Exec: "INSERT INTO t VALUES (?, ?)", Args: []yamsql.Scalar{scalar("int64", "1"), scalar("float64", "0000000000000000")}, ErrorCode: "23505"},
		{Query: "SELECT x FROM t", ExactRows: exact(scalar("float64", "0000000000000000"))},
		{Query: "SELECT x FROM t", ExactRows: exact(scalar("int64", "0"))},
		{Query: "SELECT x FROM t", ExactRows: exact(scalar("float64", "8000000000000000")), ColumnTypes: []string{"BIGINT"}},
		{Query: "SELECT x FROM t", ExactRows: &empty},
		{Exec: "CREATE SCHEMA TEMPLATE rejected_exact CREATE TABLE q (id BIGINT NOT NULL, PRIMARY KEY(id))", ErrorCode: "0A000", ErrorMessage: "wrong message"},
	}}
	r := runSemanticScenario(t, s)
	if r.TestsRun != 11 || r.TestsPass != 6 || r.TestsFail != 5 {
		t.Fatalf("run=%d pass=%d fail=%d: %+v", r.TestsRun, r.TestsPass, r.TestsFail, r.Failures)
	}
	reasons := []string{"exact row 0 mismatch", "exact row 0 mismatch", "column types:", "exact row count", "expected error message"}
	for i, f := range r.Failures {
		if !strings.HasPrefix(f.Message, reasons[i]) {
			t.Errorf("failure %d: wanted %q, got %q", i, reasons[i], f.Message)
		}
		if f.Index != i+6 {
			t.Errorf("failure index %d, expected %d: %+v", f.Index, i+6, f)
		}
	}
}
