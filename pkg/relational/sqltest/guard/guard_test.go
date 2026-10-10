// Package guard holds the layout of the end-to-end SQL suite in place.
package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// driverTestFiles is the complete set of test files allowed in
// pkg/relational/sqldriver: the driver's own tests (DSN, connection,
// transactions, rows, errors, options) and the internal-package suites. An
// end-to-end SQL test belongs in a pkg/relational/sqltest package instead; add a
// file here only if it tests the driver itself.
var driverTestFiles = []string{
	"clusterfile_lookup_test.go",
	"column_metadata_probe_test.go",
	"connector_dsn_freeze_test.go",
	"driver_test.go",
	"dsn_ddl_scope_option_test.go",
	"dsn_execution_test.go",
	"dsn_options_test.go",
	"dsn_tags_test.go",
	"dsn_transaction_timeout_fdb_test.go",
	"embedded_fdb_errors_test.go",
	"execution_stats_fdb_test.go",
	"execution_stats_retry_test.go",
	"main_test.go",
	"memory_budget_fdb_test.go",
	"mt_saas_snippets_compile_test.go",
	"multi_statement_probe_test.go",
	"param_binding_fdb_test.go",
	"param_rendering_probe_test.go",
	"param_string_edge_probe_test.go",
	"prepared_stmt_probe_test.go",
	"resource_limits_fdb_test.go",
	"serializer_options_fdb_test.go",
	"sim_not_exists_time_limit_test.go",
	"sim_page_adaptive_budget_test.go",
	"sim_sql_autocommit_1021_exhaustion_test.go",
	"sim_sql_explicit_tx_fault_test.go",
	"sim_sql_fault_test.go",
	"sim_sql_page_commit_retry_test.go",
	"sim_sql_test.go",
	"sim_sql_tx_budget_test.go",
	"sim_sql_workload_test.go",
	"sim_tx_budget_midpage_test.go",
	"sql_integration_test.go",
	"statement_options_fdb_test.go",
	"stored_query_warmup_race_fdb_test.go",
	"strict_leg_livelock_backstop_test.go",
	"time_budget_ceiling_fdb_test.go",
	"transaction_probe_test.go",
	"transaction_tags_fdb_test.go",
	"tx_budget_retry_fdb_test.go",
	"tx_commit_rollback_probe_test.go",
	"tx_isolation_rfc198_fdb_test.go",
	"tx_ryw_index_range_fdb_test.go",
	"tx_select_isolation_probe_test.go",
	"txctl_statements_probe_test.go",
	"window_options_fdb_test.go",
}

var testFunc = regexp.MustCompile(`(?m)^func Test\w*\(t \*testing\.T\)`)

// root is the runfiles root under Bazel (declared data only), else the module root.
func root(t *testing.T) string {
	t.Helper()
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		return filepath.Join(sd, ws)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for ; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatal("module root not found")
	return ""
}

func TestSQLDriverHoldsOnlyDriverTests(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.Join(root(t), "pkg/relational/sqldriver"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_test.go") {
			got = append(got, e.Name())
		}
	}
	for _, f := range got {
		if !slices.Contains(driverTestFiles, f) {
			t.Errorf("pkg/relational/sqldriver/%s is not a driver test: end-to-end SQL tests "+
				"live in a pkg/relational/sqltest package (or, if it really tests the driver, "+
				"add it to driverTestFiles)", f)
		}
	}
	for _, f := range driverTestFiles {
		if !slices.Contains(got, f) {
			t.Errorf("driverTestFiles lists %s, which no longer exists (or the data dependency on "+
				"//pkg/relational/sqldriver:tx_budget_census_corpus was dropped)", f)
		}
	}
}

func TestEverySQLTestPackageIsInTheCorpusAndHasTests(t *testing.T) {
	t.Parallel()
	base := filepath.Join(root(t), "pkg/relational/sqltest")
	tests := map[string]int{}
	err := filepath.WalkDir(base, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, "_test.go") || e.Name() == "main_test.go" {
			return err
		}
		pkg := filepath.ToSlash(filepath.Join("pkg/relational/sqltest", must(filepath.Rel(base, filepath.Dir(p)))))
		if slices.Contains([]string{"guard", "testkit"}, filepath.Base(pkg)) {
			return nil
		}
		src, err := os.ReadFile(p)
		tests[pkg] += len(testFunc.FindAll(src, -1))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) == 0 {
		t.Fatal("no sqltest package reached: the //pkg/relational/sqltest:corpus data dependency is missing")
	}
	for pkg, n := range tests {
		if !slices.Contains(testkit.Packages, pkg) {
			t.Errorf("%s is not in testkit.Packages", pkg)
		}
		if n == 0 {
			t.Errorf("%s has no Test functions", pkg)
		}
	}
	for _, pkg := range testkit.Packages {
		if _, ok := tests[pkg]; !ok {
			t.Errorf("testkit.Packages lists %s, but //pkg/relational/sqltest:corpus does not reach it "+
				"(add its :corpus filegroup there) or it has no test files", pkg)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
