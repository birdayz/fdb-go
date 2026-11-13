package plandiff

// Tests for goSQLRunner — both the stub fallback (no cluster file)
// and the real-FDB smoke path. The stub fallback runs in any
// environment; the smoke path skips when Docker / FDB is unavailable.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// goSQLClusterFilePath is set by TestMain when an FDB testcontainer is
// available. Empty value means "skip integration tests".
var goSQLClusterFilePath string

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) (code int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			fmt.Fprintf(os.Stderr, "FDB tests skip: %v\n", err)
			return m.Run()
		}
		fmt.Fprintf(os.Stderr, "FDB container startup failed: %v\n", err)
		return 1
	}
	defer func() {
		if err := foundationdbtc.Terminate(context.Background(), container); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}()

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		return 1
	}
	tmp, err := os.CreateTemp("", "fdb-plandiff-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		return 1
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		return 1
	}
	tmp.Close()
	goSQLClusterFilePath = tmp.Name()

	return m.Run()
}

// TestWithTeardown drives every arm of the teardown reporting: a teardown that
// failed is never dropped, it is the error of a run that succeeded, and it rides
// behind the error of a run that failed without changing that error's text or
// what errors.As finds first.
func TestWithTeardown(t *testing.T) {
	t.Parallel()
	dropDB := fixtureErrf(errors.New("db busy"), "DROP DATABASE")
	dropTpl := fixtureErrf(errors.New("template busy"), "DROP SCHEMA TEMPLATE")
	runErr := &FixtureError{Phase: "query", Err: errors.New("boom")}

	if got := withTeardown(nil, nil); got != nil {
		t.Fatalf("clean run, clean teardown: got %v, want nil", got)
	}
	if got := withTeardown(runErr, nil); got != runErr {
		t.Fatalf("failed run, clean teardown: got %v, want the run's error unchanged", got)
	}

	got := withTeardown(nil, []error{dropDB, dropTpl})
	if got == nil || !errors.Is(got, dropDB) || !errors.Is(got, dropTpl) {
		t.Fatalf("clean run, failed teardown: got %v, want both drop failures", got)
	}

	got = withTeardown(runErr, []error{dropTpl})
	if got.Error() != runErr.Error() {
		t.Fatalf("failed run, failed teardown: text %q, want the run's %q", got.Error(), runErr.Error())
	}
	var first *FixtureError
	if !errors.As(got, &first) || first != runErr {
		t.Fatalf("failed run, failed teardown: errors.As found %v first, want the run's error", first)
	}
	if !errors.Is(got, dropTpl) {
		t.Fatalf("failed run, failed teardown: the drop failure is not reachable from %v", got)
	}
}

// TestGoSQLRunner_NoClusterFileFallback pins the no-FDB contract:
// NewGoSQLRunner("") returns the stub runner (NewGoRunner) so callers
// in CI without FDB still get a deterministic ErrGoUnimplemented.
func TestGoSQLRunner_NoClusterFileFallback(t *testing.T) {
	t.Parallel()
	r := NewGoSQLRunner("")
	got := r.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if !errors.Is(got.Err, ErrGoUnimplemented) {
		t.Fatalf("expected ErrGoUnimplemented, got %v", got.Err)
	}

	sr := NewGoSQLSetupRunner("")
	got = sr.RunWithSetup(context.Background(), "", nil, "SELECT 1")
	if !errors.Is(got.Err, ErrGoUnimplemented) {
		t.Fatalf("expected ErrGoUnimplemented from RunWithSetup, got %v", got.Err)
	}
}

// TestGoSQLRunner_HappyPath drives a simple INSERT-then-SELECT flow
// through the real Go embedded engine: ephemeral schema lifecycle +
// setup + query, then asserts the RowSet shape and values match the
// per-entry expectation. Skips when no FDB is available.
func TestGoSQLRunner_HappyPath(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY (id))",
		[]string{
			"INSERT INTO T VALUES (1, 'alice'), (2, 'bob')",
		},
		"SELECT id, name FROM T ORDER BY id",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Columns) != 2 {
		t.Fatalf("Columns: got %d, want 2 (%+v)", len(got.Rows.Columns), got.Rows.Columns)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2 (%+v)", len(got.Rows.Rows), got.Rows.Rows)
	}
	// Row 0: {1, "alice"} — coerced to {float64(1), "alice"} for
	// JSON-compatible comparison with the Java side.
	if got.Rows.Rows[0][0] != float64(1) {
		t.Fatalf("Rows[0][0]: got %v (%T), want 1", got.Rows.Rows[0][0], got.Rows.Rows[0][0])
	}
	if got.Rows.Rows[0][1] != "alice" {
		t.Fatalf("Rows[0][1]: got %v, want alice", got.Rows.Rows[0][1])
	}
	if got.Rows.Rows[1][0] != float64(2) {
		t.Fatalf("Rows[1][0]: got %v, want 2", got.Rows.Rows[1][0])
	}
	if got.Rows.Rows[1][1] != "bob" {
		t.Fatalf("Rows[1][1]: got %v, want bob", got.Rows.Rows[1][1])
	}
}

// TestGoSQLRunner_ArrayCellIsNormalised pins the coercion against the shape the
// DRIVER actually produces, not the shape the coercion's unit tests assume.
//
// The unit tests in struct_cell_coercion_test.go drive every arm with fakes,
// and a fake proves the arm works — not that the arm is the one real results
// take. That distinction went wrong once already here: the coercion was first
// written with an api.Array arm only, and materializeDriverValue hands an ARRAY
// column back as []any, so the arm real queries needed did not exist and every
// array cell fell to the pass-through default with its elements unnormalised.
// An array of BIGINTs stayed []any{int64…}, which can never equal Java's
// []any{float64…} — a permanent false divergence on every array-valued column,
// reported as a semantic disagreement.
//
// So this asserts the END-TO-END shape: a real ARRAY column, through the real
// runner, must arrive as a []any of float64 — the JSON-decoded form the Java
// side produces — and not as int64s, a pointer, or anything else.
func TestGoSQLRunner_ArrayCellIsNormalised(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, xs BIGINT ARRAY, PRIMARY KEY (id))",
		[]string{"INSERT INTO T VALUES (1, [10, 20])"},
		"SELECT xs FROM T WHERE id = 1",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Rows) != 1 || len(got.Rows.Rows[0]) != 1 {
		t.Fatalf("row shape: got %+v, want one row of one cell", got.Rows.Rows)
	}
	cell := got.Rows.Rows[0][0]
	arr, ok := cell.([]any)
	if !ok {
		t.Fatalf("an ARRAY cell arrived as %T, not []any. Whatever the driver now returns, the "+
			"coercion needs an arm for it — the pass-through default compares it as a raw Go "+
			"value, which no Java rendering can equal.\n  cell: %#v", cell, cell)
	}
	want := []any{float64(10), float64(20)}
	if !reflect.DeepEqual(arr, want) {
		t.Fatalf("an ARRAY cell was not normalised element-wise\n  got  %#v\n  want %#v\n"+
			"  (int64 elements can never equal Java's JSON-decoded float64s, so every "+
			"array-valued column would read as a permanent divergence)", arr, want)
	}
}

// TestGoSQLRunner_NullPassThrough pins NULL handling: a SELECT on a
// NULL column produces nil in the resulting RowSet (matches Java's
// JSON-NULL encoding).
func TestGoSQLRunner_NullPassThrough(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY (id))",
		[]string{
			"INSERT INTO T VALUES (1, 'alice'), (2, NULL)",
		},
		"SELECT id, name FROM T ORDER BY id",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2 (%+v)", len(got.Rows.Rows), got.Rows.Rows)
	}
	if got.Rows.Rows[1][1] != nil {
		t.Fatalf("Rows[1][1]: got %v (%T), want nil", got.Rows.Rows[1][1], got.Rows.Rows[1][1])
	}
}

func TestGoSQLRunner_BytesINList(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE lb (a BIGINT, b BYTES, PRIMARY KEY (a))",
		[]string{
			"INSERT INTO lb VALUES (1, X'deadbeef'), (2, X'cafe'), (3, null)",
		},
		"SELECT a FROM lb WHERE b IN (X'cafe', X'deadbeef') ORDER BY a",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2 (%+v)", len(got.Rows.Rows), got.Rows.Rows)
	}
	if got.Rows.Rows[0][0] != float64(1) {
		t.Fatalf("Row[0]: got %v (%T), want 1", got.Rows.Rows[0][0], got.Rows.Rows[0][0])
	}
	if got.Rows.Rows[1][0] != float64(2) {
		t.Fatalf("Row[1]: got %v (%T), want 2", got.Rows.Rows[1][0], got.Rows.Rows[1][0])
	}
}

// cancelingArg cancels the run's context when database/sql converts it, so the
// statement fails with the context's error mid-run.
type cancelingArg struct{ cancel context.CancelFunc }

func (a cancelingArg) Value() (driver.Value, error) {
	a.cancel()
	return int64(1), nil
}

// fixturePhases lists the Phase of every FixtureError in err's tree.
func fixturePhases(err error) map[string]bool {
	found := map[string]bool{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if fx, ok := e.(*FixtureError); ok {
			found[fx.Phase] = true
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return found
}

// TestGoSQLRunner_TeardownFailureIsReported pins the WIRING of the teardown
// report, which TestWithTeardown cannot see: the deferred drops' failures reach
// the error runEphemeralFollowUp returns (through withEphemeralSchema's named
// results), on a run that itself succeeded. The drops are made to fail through
// the runner's teardownExec seam; a cleanup registered before the run passes the recorded
// drops through the real exec, so the test leaks nothing even when an
// assertion stops it.
func TestGoSQLRunner_TeardownFailureIsReported(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	r, ok := NewGoSQLSetupRunner(goSQLClusterFilePath).(*goSQLRunner)
	if !ok {
		t.Fatalf("NewGoSQLSetupRunner returned %T, want *goSQLRunner", NewGoSQLSetupRunner(goSQLClusterFilePath))
	}
	var dropped []string
	t.Cleanup(func() { dropThroughRealExec(t, dropped) })
	failing := *r
	failing.teardownExec = func(ctx context.Context, db *sql.DB, stmt string) error {
		dropped = append(dropped, stmt)
		return errors.New("injected drop failure")
	}
	_, err := failing.runEphemeralFollowUp(context.Background(), "CREATE TABLE T (id BIGINT, PRIMARY KEY (id))", nil,
		"SELECT id FROM T", "", false)
	found := fixturePhases(err)
	if !found["DROP DATABASE"] || !found["DROP SCHEMA TEMPLATE"] {
		t.Fatalf("the teardown's failed drops are not in the returned error: phases %v in %v", found, err)
	}
}

// dropThroughRealExec runs the teardown's recorded drops (each IF EXISTS, so a
// second run finds nothing to do) through the system database. It is a
// t.Cleanup, so it runs whether the test body finished or stopped at a Fatalf.
func dropThroughRealExec(t *testing.T, drops []string) {
	t.Helper()
	if len(drops) == 0 {
		return
	}
	sysDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+goSQLClusterFilePath)
	if err != nil {
		t.Errorf("cleanup: opening the system database: %v", err)
		return
	}
	defer sysDB.Close()
	for _, stmt := range drops {
		if _, derr := sysDB.ExecContext(context.Background(), stmt); derr != nil {
			t.Errorf("cleanup %q: %v", stmt, derr)
		}
	}
}

// TestGoSQLRunner_CanceledRunStillTearsDown pins that the teardown does not
// inherit the run's cancellation: a run whose context is canceled mid-statement
// reports the run's failure and no teardown failure, and leaves neither its
// database nor its template behind.
func TestGoSQLRunner_CanceledRunStillTearsDown(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	r, ok := NewGoSQLSetupRunner(goSQLClusterFilePath).(*goSQLRunner)
	if !ok {
		t.Fatalf("NewGoSQLSetupRunner returned %T, want *goSQLRunner", NewGoSQLSetupRunner(goSQLClusterFilePath))
	}
	var executed []string
	t.Cleanup(func() { dropThroughRealExec(t, executed) })
	observing := *r
	observing.teardownExec = func(ctx context.Context, db *sql.DB, stmt string) error {
		executed = append(executed, stmt)
		_, err := db.ExecContext(ctx, stmt)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := observing.runEphemeralFollowUp(ctx, "CREATE TABLE T (id BIGINT, PRIMARY KEY (id))", nil,
		"SELECT id FROM T WHERE id = ?", "", false, cancelingArg{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a run whose context was canceled mid-run reported %v, want context.Canceled in its chain", err)
	}
	if found := fixturePhases(err); found["DROP DATABASE"] || found["DROP SCHEMA TEMPLATE"] {
		t.Fatalf("the teardown failed under the canceled run's context: phases %v in %v", found, err)
	}
	if len(executed) != 2 {
		t.Fatalf("teardown ran %d drops (%v), want the database and the template", len(executed), executed)
	}
	sysDB, oerr := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+goSQLClusterFilePath)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer sysDB.Close()
	for _, stmt := range executed {
		// Each drop is IF EXISTS; re-running it without IF EXISTS must find
		// nothing, and must say so with the not-found code of what it drops:
		// any other failure (a connection error, a syntax error) says nothing
		// about whether the teardown ran.
		strict := strings.Replace(stmt, " IF EXISTS", "", 1)
		want := api.ErrCodeUnknownDatabase
		if strings.HasPrefix(strict, "DROP SCHEMA TEMPLATE") {
			want = api.ErrCodeUnknownSchemaTemplate
		}
		_, derr := sysDB.ExecContext(context.Background(), strict)
		var apiErr *api.Error
		if !errors.As(derr, &apiErr) || apiErr.Code != want {
			t.Errorf("%q = %v, want %s: the canceled run's teardown should have dropped it", strict, derr, want)
		}
	}
}
