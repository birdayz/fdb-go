package yamsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/yamsql"
	_ "fdb.dev/pkg/relational/sqldriver"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// RFC-180 Y6: in CI a startup failure must be FATAL, never a silent
		// all-skip — a Docker hiccup would otherwise convert the 319-scenario
		// corpus into a green run that executed nothing (the dark-safety-net
		// pattern the corpus un-skip exists to kill). The skip path survives
		// only for genuinely Docker-less local machines.
		if os.Getenv("CI") != "" {
			fmt.Fprintf(os.Stderr, "FATAL: FDB container startup failed in CI — the yamsql corpus would silently skip: %v\n", err)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-yamsql-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// TestYamsqlConformance walks testdata/*.yaml and runs each scenario
// against the Go SQL driver. Any expected/actual row mismatch is a
// correctness regression.
//
// This is a Go-only harness — expected rows in the corpus are the
// Java-authoritative reference, recorded when the scenario was
// authored. Adding a new scenario means documenting "this is what
// Java returns" and pinning our behaviour against it.
func TestYamsqlConformance(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	matches, err := filepath.Glob("testdata/*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no scenarios found under testdata/")
	}

	for _, path := range matches {
		path := path
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, path, name)
		})
	}
}

// scenarioBudget bounds one scenario's FDB work. It is a wall-clock
// budget, so a starved runner spends it without any single statement
// being slow — which is why scenarioReport names the budget in the
// failure rather than letting a bare "context deadline exceeded" send
// the reader hunting for a logic bug.
const scenarioBudget = 2 * time.Minute

// scenarioOutcome is everything runScenario learned about one scenario.
// Splitting it out keeps reporting a pure function of the outcome, so the
// message format is testable without an FDB cluster.
//
// Every error field here is a distinct failure path. scenarioReport must
// render each one; TestScenarioReportNamesTheScenario walks these fields
// reflectively so a new path cannot be added without a named message.
type scenarioOutcome struct {
	LoadErr  error // yamsql.Load failed
	OpenErr  error // sql.Open failed
	RunErr   error // yamsql.Run itself failed (bad RunConfig)
	SetupErr error // schema/setup DDL failed

	Failures  []yamsql.Failure
	TestsRun  int
	TestsPass int
	TestsFail int

	Path    string
	Elapsed time.Duration
	Expired bool // the per-scenario deadline fired
}

// scenarioReport renders an outcome into the lines runScenario emits and
// reports whether the scenario failed.
//
// Every line, pass or fail, is prefixed with "scenario <name>:". Go
// buffers the "--- FAIL: <subtest>" result lines of parallel subtests and
// flushes them in one block once all of them finish, so a failure message
// is written where it happened — routinely a thousand lines away from the
// only marker that names the scenario. Grepping the scenario name is the
// search anybody actually runs, and it finds the failure only if the
// failure carries the name too. A CI report was once misdiagnosed as a
// message-less FAIL for exactly this reason.
func scenarioReport(name string, o scenarioOutcome) (lines []string, failed bool) {
	prefix := "scenario " + name + ": "
	failf := func(format string, args ...any) {
		lines = append(lines, prefix+fmt.Sprintf(format, args...))
		failed = true
	}

	switch {
	case o.LoadErr != nil:
		failf("load %s: %v", o.Path, o.LoadErr)
	case o.OpenErr != nil:
		failf("sql.Open: %v", o.OpenErr)
	case o.RunErr != nil:
		failf("Run: %v", o.RunErr)
	case o.SetupErr != nil:
		failf("setup: %v", o.SetupErr)
	case o.TestsFail > 0:
		for _, f := range o.Failures {
			failf("test[%d] %q:\n%s", f.Index, f.Query, f.Message)
		}
		failf("%d/%d tests failed", o.TestsFail, o.TestsRun)
	}

	if failed {
		if o.Expired {
			lines = append(lines, fmt.Sprintf(
				"%sexhausted the %s per-scenario budget after %s — a starved runner burns this "+
					"budget with no statement being slow, so rule the machine out before the query",
				prefix, scenarioBudget, o.Elapsed.Round(time.Second)))
		}
		return lines, true
	}
	return []string{fmt.Sprintf("%s%d/%d passed", prefix, o.TestsPass, o.TestsRun)}, false
}

func runScenario(t *testing.T, path, name string) {
	t.Helper()
	lines, failed := scenarioReport(name, execScenario(path, name))
	for _, line := range lines {
		if failed {
			t.Error(line)
		} else {
			t.Log(line)
		}
	}
	if failed {
		t.FailNow()
	}
}

// execScenario runs one scenario against the shared FDB cluster and
// reports what happened. It never touches *testing.T: reporting lives in
// scenarioReport so the message format stays testable on its own.
func execScenario(path, name string) scenarioOutcome {
	o := scenarioOutcome{Path: path}

	scenario, err := yamsql.Load(path)
	if err != nil {
		o.LoadErr = err
		return o
	}

	// Unique DSN path + template per test to keep parallel runs isolated.
	dbPath := "/_conf_" + sanitize(name)
	tmplName := "CONF_TMPL_" + strings.ToUpper(sanitize(name))
	schemaName := "conf"
	// schema= is a lazy default — the schema need not exist at sql.Open
	// time; the driver resolves it on the first DML statement. DDL
	// (CREATE DATABASE / SCHEMA / TEMPLATE) runs on the catalog path and
	// ignores schema=, so one DSN serves both setup and test phases.
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		o.OpenErr = err
		return o
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), scenarioBudget)
	defer cancel()

	start := time.Now()
	res, err := yamsql.Run(ctx, scenario, yamsql.RunConfig{
		DB:           db,
		DBPath:       dbPath,
		SchemaName:   schemaName,
		TemplateName: tmplName,
	})
	o.Elapsed = time.Since(start)
	o.Expired = ctx.Err() != nil
	if err != nil {
		o.RunErr = err
		return o
	}
	o.SetupErr = res.SetupError
	o.Failures = res.Failures
	o.TestsRun, o.TestsPass, o.TestsFail = res.TestsRun, res.TestsPass, res.TestsFail
	return o
}

// sanitize makes a scenario name safe for use in a database path and
// SQL identifier: alphanumerics + underscore only.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
