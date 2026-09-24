package plandiff

// Go-side runner — drives the in-process embedded engine via the
// fdbsql driver (`pkg/relational/sqldriver`). Unblocks cross-engine
// equivalence checks for the SeedRunCorpus shapes the Go engine
// already supports.
//
// Track A1 / A3 progress: when a corpus query passes through both
// runners with byte-equal RowSet results, the harness has caught a
// real Go-vs-Java semantic agreement. Discrepancies (Status=Diverge)
// surface as per-entry assertion failures with both sides' rows
// shown side-by-side.
//
// Limitations of the seed:
//   - Schema-template / database / schema lifecycle is per-call —
//     each Run / RunWithSetup constructs a fresh ephemeral schema,
//     mirrors Java's `SqlPlanSteps.runWithEphemeralSchema`.
//   - Column type names use a coarse Go-type-to-JDBC-name mapping
//     (int64 → "BIGINT", string → "STRING", bool → "BOOLEAN", …)
//     because database/sql doesn't expose driver-specific type names
//     unless the driver implements ColumnTypeDatabaseTypeName. The
//     embedded driver doesn't yet, so the runner does best-effort.
//     Corpus entries that pin precise type names should match this
//     coarse mapping.
//   - Numeric values arrive as int64 / float64 / etc. The runner
//     converts to float64 for cross-engine comparability with Java's
//     JSON number representation.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"

	// Register the fdbsql driver for blank-import side-effects.
	_ "fdb.dev/pkg/relational/sqldriver"
)

// goSQLRunner is the in-process Go runner. It opens a database/sql
// connection to the embedded engine (via the registered fdbsql
// driver), runs the schema lifecycle + setup + query, and packages
// the result as a RowSet matching the Java side's wire shape.
type goSQLRunner struct {
	clusterFilePath string
	// teardownExec runs one teardown drop; nil is sysDB.ExecContext. It exists
	// so a test can make the drops fail without making the RUN fail, which is
	// the only way to see that their failures reach the returned error.
	teardownExec func(ctx context.Context, db *sql.DB, stmt string) error
}

// teardownTimeout bounds the ephemeral teardown, which runs detached from the
// run's context (see withEphemeralSchema).
const teardownTimeout = 30 * time.Second

// NewGoSQLRunner returns a Runner that drives the in-process Go
// embedded engine. clusterFilePath must point at an FDB cluster
// file (the runner builds DSNs of the form
// `fdbsql://<dbPath>?cluster_file=<path>`).
//
// Returns the unwired form (NewGoRunner / ErrGoUnimplemented) when
// clusterFilePath is empty — same Go-only-CI contract as the
// runsql.go stubs.
func NewGoSQLRunner(clusterFilePath string) Runner {
	if clusterFilePath == "" {
		return NewGoRunner()
	}
	return &goSQLRunner{clusterFilePath: clusterFilePath}
}

// NewGoSQLSetupRunner returns the same runner but typed as a
// SetupRunner. Convenience for callers that want the
// RunWithSetup method specifically.
func NewGoSQLSetupRunner(clusterFilePath string) SetupRunner {
	if clusterFilePath == "" {
		return NewGoRunner().(SetupRunner)
	}
	return &goSQLRunner{clusterFilePath: clusterFilePath}
}

func (r *goSQLRunner) Run(ctx context.Context, q Query) RunResult {
	rows, err := r.runEphemeral(ctx, q.SchemaTemplate, nil, q.SQL)
	if err != nil {
		return RunResult{Engine: "go", Err: err}
	}
	return RunResult{Engine: "go", Rows: rows}
}

func (r *goSQLRunner) RunWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string) RunResult {
	rows, err := r.runEphemeral(ctx, schemaTemplate, setupSqls, querySql)
	if err != nil {
		return RunResult{Engine: "go", Err: err}
	}
	return RunResult{Engine: "go", Rows: rows}
}

// PreparedSetupRunner runs a query with driver-bound parameters (database/sql
// args), after the same ephemeral schema and setup as RunWithSetup.
type PreparedSetupRunner interface {
	RunPreparedWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string, args []any) RunResult
}

// RunPreparedWithSetup is RunWithSetup with args bound through database/sql.
func (r *goSQLRunner) RunPreparedWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string, args []any) RunResult {
	rows, err := r.runEphemeral(ctx, schemaTemplate, setupSqls, querySql, args...)
	if err != nil {
		return RunResult{Engine: "go", Err: err}
	}
	return RunResult{Engine: "go", Rows: rows}
}

// FixtureError marks a failure in the ephemeral-FIXTURE lifecycle — the DDL and
// setup DML that build the throwaway schema a corpus entry runs against — as
// distinct from a failure of the query under test.
//
// The distinction is load-bearing for the cross-engine harnesses. A fixture that
// never got built produced no engine answer to compare against Java's, so it
// cannot be a cross-engine divergence; reporting it as one manufactures a
// phantom semantic disagreement out of a CI-load artifact. A failure on the
// QUERY (or on a DML statement under test) is deliberately NOT a FixtureError:
// that IS the engine's answer and must stay comparable, so a real Go defect can
// never hide behind this type.
//
// Phase is the human-readable step name and rides the rendered message, so the
// text a harness logs is unchanged from the plain fmt.Errorf wrapping it
// replaces. Wrap chains through it, so callers can reach the underlying cause
// with errors.Is / errors.As.
type FixtureError struct {
	Phase string
	Err   error
}

func (e *FixtureError) Error() string { return "plandiff/go: " + e.Phase + ": " + e.Err.Error() }

func (e *FixtureError) Unwrap() error { return e.Err }

// fixtureErrf builds a FixtureError whose Phase is formatted from args, so call
// sites keep reading like the fmt.Errorf they replaced.
func fixtureErrf(err error, format string, args ...any) error {
	return &FixtureError{Phase: fmt.Sprintf(format, args...), Err: err}
}

// suppressedTeardownError is a run's own failure with the failure of its teardown
// attached, the Go form of Java's addSuppressed: its text is the run's error and
// errors.As reaches the run's error first, so callers comparing outcomes see the
// run's failure, while the teardown failure stays reachable through Unwrap.
type suppressedTeardownError struct{ primary, teardown error }

func (e *suppressedTeardownError) Error() string   { return e.primary.Error() }
func (e *suppressedTeardownError) Unwrap() []error { return []error{e.primary, e.teardown} }

// withTeardown combines a run's error with the failures of its teardown: none
// leaves the run's error as it is, a failed teardown of a successful run is the
// run's error, and a failed teardown of a failed run rides behind the run's error.
func withTeardown(runErr error, teardown []error) error {
	if len(teardown) == 0 {
		return runErr
	}
	if runErr == nil {
		return errors.Join(teardown...)
	}
	return &suppressedTeardownError{primary: runErr, teardown: errors.Join(teardown...)}
}

// runEphemeral mirrors Java's runWithEphemeralSchema flow:
// CREATE SCHEMA TEMPLATE → CREATE DATABASE → CREATE SCHEMA →
// open connection on the ephemeral schema → run setup DMLs → run
// the query and capture its result. Tears the ephemeral state down
// in defer.
func (r *goSQLRunner) runEphemeral(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string, args ...any) (RowSet, error) {
	return r.runEphemeralFollowUp(ctx, schemaTemplate, setupSqls, querySql, "", isDMLQuery(querySql), args...)
}

// PreparedDMLRunner runs a prepared DML statement and then a plain follow-up query
// in the same ephemeral schema, so what the DML stored can be read back.
type PreparedDMLRunner interface {
	RunPreparedDMLWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, dml string, args []any, followUpSql string) RunResult
}

// RunPreparedDMLWithSetup executes dml with args, then returns followUpSql's rows.
func (r *goSQLRunner) RunPreparedDMLWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, dml string, args []any, followUpSql string) RunResult {
	rows, err := r.runEphemeralFollowUp(ctx, schemaTemplate, setupSqls, dml, followUpSql, true, args...)
	if err != nil {
		return RunResult{Engine: "go", Err: err}
	}
	return RunResult{Engine: "go", Rows: rows}
}

// PreparedSequenceRunner runs one statement text several times, once per argument set, on
// ONE connection of one ephemeral schema, so a plan cached under the statement's
// value-free text by one execution is the plan the next execution runs. With
// reuseStatement the statement is prepared once and re-bound; without it the text is
// re-submitted each time. Each execution's answer is its own RunResult; a fixture failure
// is every execution's answer.
type PreparedSequenceRunner interface {
	RunPreparedSequenceWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string, argSets [][]any, reuseStatement bool) []RunResult
}

// RunPreparedSequenceWithSetup implements PreparedSequenceRunner.
func (r *goSQLRunner) RunPreparedSequenceWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string, argSets [][]any, reuseStatement bool) []RunResult {
	results := make([]RunResult, len(argSets))
	_, err := r.withEphemeralSchema(ctx, schemaTemplate, setupSqls, func(schemaDB *sql.DB) (RowSet, error) {
		conn, err := schemaDB.Conn(ctx)
		if err != nil {
			return RowSet{}, fixtureErrf(err, "open connection")
		}
		defer conn.Close()
		// The plan cache's part in each execution, recorded by a logger on this
		// connection: a sequence meant to measure a cached plan must show that the
		// plan WAS cached, or its rows say nothing about the cache.
		cacheLog := &planCacheLog{}
		if err := conn.Raw(func(dc any) error {
			ec, ok := dc.(*embedded.EmbeddedConnection)
			if !ok {
				return fmt.Errorf("driver conn is %T, want *embedded.EmbeddedConnection", dc)
			}
			ec.SetPlanLogger(cacheLog)
			return nil
		}); err != nil {
			return RowSet{}, fixtureErrf(err, "install plan logger")
		}
		var stmt *sql.Stmt
		if reuseStatement {
			// A prepare failure is the fixture's, reported as such, not rendered as
			// each execution's answer.
			if stmt, err = conn.PrepareContext(ctx, querySql); err != nil {
				return RowSet{}, fixtureErrf(err, "prepare")
			}
			defer stmt.Close()
		}
		for i, args := range argSets {
			cacheLog.last = ""
			var sqlRows *sql.Rows
			var qerr error
			if stmt != nil {
				sqlRows, qerr = stmt.QueryContext(ctx, args...)
			} else {
				sqlRows, qerr = conn.QueryContext(ctx, querySql, args...)
			}
			if qerr != nil {
				results[i] = RunResult{Engine: "go", Err: fmt.Errorf("plandiff/go: query: %w", qerr), PlanCache: cacheLog.last}
				continue
			}
			rows, rerr := rowSetFrom(sqlRows, nil)
			if rerr != nil {
				results[i] = RunResult{Engine: "go", Err: rerr, PlanCache: cacheLog.last}
				continue
			}
			results[i] = RunResult{Engine: "go", Rows: rows, PlanCache: cacheLog.last}
		}
		return RowSet{}, nil
	})
	if err != nil {
		for i := range results {
			results[i] = RunResult{Engine: "go", Err: err}
		}
	}
	return results
}

// runEphemeralFollowUp is runEphemeral with an optional follow-up query run after a
// DML statement in the same schema; its rows replace the rows-affected result.
func (r *goSQLRunner) runEphemeralFollowUp(ctx context.Context, schemaTemplate string, setupSqls []string, querySql, followUpSql string, update bool, args ...any) (RowSet, error) {
	return r.withEphemeralSchema(ctx, schemaTemplate, setupSqls, func(schemaDB *sql.DB) (RowSet, error) {
		// An update statement (DML or DDL) runs through ExecContext and reports its update
		// count. The caller says which, as the Java runner's `update` flag does: the prepared
		// DML path always passes true (a DDL statement with a follow-up included), and the
		// plain path passes its statement classification.
		var updateCount *int64
		if update {
			result, err := schemaDB.ExecContext(ctx, querySql, args...)
			if err != nil {
				return RowSet{}, fmt.Errorf("plandiff/go: exec: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return RowSet{}, fmt.Errorf("plandiff/go: rows affected: %w", err)
			}
			if followUpSql == "" {
				return RowSet{
					Columns: []Column{{Name: "ROWS_AFFECTED", Type: "BIGINT"}},
					Rows:    [][]any{{float64(affected)}},
				}, nil
			}
			updateCount = &affected
			querySql, args = followUpSql, nil
		}

		// Run the query and capture rows.
		sqlRows, err := schemaDB.QueryContext(ctx, querySql, args...)
		if err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: query: %w", err)
		}
		return rowSetFrom(sqlRows, updateCount)
	})
}

// withEphemeralSchema mirrors Java's runWithEphemeralSchema flow: CREATE SCHEMA
// TEMPLATE → CREATE DATABASE → CREATE SCHEMA → open a connection pool on the ephemeral
// schema → run the setup statements → run fn against the pool. It tears the ephemeral
// state down on return.
func (r *goSQLRunner) withEphemeralSchema(ctx context.Context, schemaTemplate string, setupSqls []string, fn func(schemaDB *sql.DB) (RowSet, error)) (rows RowSet, err error) {
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	templateName := "PLAN_DIFF_T_" + suffix
	// Go embedded engine requires a single-segment database path
	// (`/name`); fdb-relational's parser rejects multi-segment forms.
	dbPath := "/PLAN_DIFF_" + suffix
	schemaName := "S_" + suffix

	// Use the __SYS database for DDL — same as Java's
	// `__SYS?schema=CATALOG` flow.
	sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", r.clusterFilePath))
	if err != nil {
		return RowSet{}, fixtureErrf(err, "open __SYS")
	}
	defer sysDB.Close()

	templateCreated := false
	dbCreated := false
	defer func() {
		// Teardown through the connection that created the objects. fdb-relational
		// accepts bare identifiers and paths; quoting them rejects with a parser
		// error ("database path must be /name"). A failed drop is reported, never
		// swallowed, so a leak cannot pass silently: it is the run's error when the
		// run succeeded, and rides along behind the run's own error otherwise.
		//
		// The drops run under the run's context WITHOUT its cancellation (and
		// under their own timeout): a run that was canceled or timed out still
		// created the objects, and dropping them with the dead context would fail
		// both drops and leak them.
		dropCtx, cancelDrops := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancelDrops()
		exec := r.teardownExec
		if exec == nil {
			exec = func(ctx context.Context, db *sql.DB, stmt string) error {
				_, err := db.ExecContext(ctx, stmt)
				return err
			}
		}
		var teardown []error
		if dbCreated {
			if dropErr := exec(dropCtx, sysDB, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath)); dropErr != nil {
				teardown = append(teardown, fixtureErrf(dropErr, "DROP DATABASE"))
			}
		}
		if templateCreated {
			if dropErr := exec(dropCtx, sysDB, fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", templateName)); dropErr != nil {
				teardown = append(teardown, fixtureErrf(dropErr, "DROP SCHEMA TEMPLATE"))
			}
		}
		if len(teardown) > 0 {
			rows, err = RowSet{}, withTeardown(err, teardown)
		}
	}()

	if schemaTemplate != "" {
		stmt := fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", templateName, schemaTemplate)
		if _, err := sysDB.ExecContext(ctx, stmt); err != nil {
			return RowSet{}, fixtureErrf(err, "CREATE SCHEMA TEMPLATE")
		}
		templateCreated = true

		if _, err := sysDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
			return RowSet{}, fixtureErrf(err, "CREATE DATABASE")
		}
		dbCreated = true

		if _, err := sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schemaName, templateName)); err != nil {
			return RowSet{}, fixtureErrf(err, "CREATE SCHEMA")
		}
	}

	// Open the per-query connection on the ephemeral schema.
	var schemaDB *sql.DB
	if schemaTemplate != "" {
		schemaDB, err = sql.Open("fdbsql",
			fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, r.clusterFilePath, schemaName))
	} else {
		schemaDB, err = sql.Open("fdbsql",
			fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", r.clusterFilePath))
	}
	if err != nil {
		return RowSet{}, fixtureErrf(err, "open ephemeral schema")
	}
	defer schemaDB.Close()

	// Run setup DMLs.
	for _, setup := range setupSqls {
		if _, err := schemaDB.ExecContext(ctx, setup); err != nil {
			return RowSet{}, fixtureErrf(err, "setup %q", setup)
		}
	}
	return fn(schemaDB)
}

// rowSetFrom drains sqlRows (and closes them) into a RowSet in the Java side's wire
// shape, carrying updateCount when a DML statement preceded the query.
func rowSetFrom(sqlRows *sql.Rows, updateCount *int64) (RowSet, error) {
	defer sqlRows.Close()

	colNames, err := sqlRows.Columns()
	if err != nil {
		return RowSet{}, fmt.Errorf("plandiff/go: column names: %w", err)
	}

	// Pull JDBC-style type names directly from the driver — the
	// embedded driver implements RowsColumnTypeDatabaseTypeName, so
	// we get authoritative type names for typed columns and "" only
	// for projections whose result type wasn't inferred (e.g.
	// arithmetic expressions). This replaces the earlier value-based
	// inference, which couldn't distinguish DOUBLE from BIGINT after
	// numeric coercion.
	colTypes, ctErr := sqlRows.ColumnTypes()
	if ctErr != nil {
		return RowSet{}, fmt.Errorf("plandiff/go: column types: %w", ctErr)
	}

	out := RowSet{
		Columns:     make([]Column, len(colNames)),
		Rows:        [][]any{},
		UpdateCount: updateCount,
	}
	out.Nullability = make([]string, len(colNames))
	for i, name := range colNames {
		typeName := ""
		out.Nullability[i] = "UNKNOWN"
		if i < len(colTypes) && colTypes[i] != nil {
			typeName = colTypes[i].DatabaseTypeName()
			if nullable, ok := colTypes[i].Nullable(); ok {
				out.Nullability[i] = "NOT NULL"
				if nullable {
					out.Nullability[i] = "NULL"
				}
			}
		}
		out.Columns[i] = Column{Name: name, Type: typeName}
	}

	for sqlRows.Next() {
		row := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := sqlRows.Scan(ptrs...); err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: scan: %w", err)
		}
		// Convert row values for cross-engine comparability. Java
		// arrives via JSON, so numbers are float64, NULL is nil,
		// booleans are bool, strings are string. The Go embedded
		// driver returns int64 / float64 / string / bool / nil
		// natively — coerce numerics to float64 so the comparison
		// is uniform.
		for i, v := range row {
			row[i] = coerceForComparison(v)
		}
		out.Rows = append(out.Rows, row)
	}
	if err := sqlRows.Err(); err != nil {
		return RowSet{}, fmt.Errorf("plandiff/go: rows.Err: %w", err)
	}
	// If no rows, infer column types from NULL — record as empty.
	return out, nil
}

// coerceForComparison normalises Go driver values to match the Java
// side's JSON-decoded representation. Numbers → float64, nil → nil,
// strings/booleans pass through, []byte → base64 string (Java
// encodes bytes as base64 in encodeValue). IEEE-754 specials
// (±Infinity, NaN) are encoded as strings to match the Java conformance
// server's JSON encoder (which can't emit those as bare JSON numbers).
//
// STRUCT and ARRAY columns are the two cell types that are NOT Go scalars:
// the driver hands them over as api.Struct / api.Array (see
// pkg/relational/sqldriver/record_constructor_expression_fdb_test.go, which
// type-asserts exactly that). The Java side arrives through json.Unmarshal
// into []any, so a struct is a map[string]any and an array is a []any there.
// Without the two arms below those cells fell to the default `return v` and
// were compared — and PRINTED — as Go POINTERS, which no Java rendering can
// ever equal and which carry a per-run heap address, so every struct-valued
// column read as a permanent, unreadable divergence. That is worse than a
// missing comparison: the harness cannot see the column at all, and a genuine
// struct divergence hides behind the same `0x…` that agreement produces.
//
// Both arms recurse through coerceForComparison, because the driver nests
// (api.Array documents that nested STRUCTs become Struct and nested arrays
// become Array), and because the element scalars need the same int64→float64
// normalisation the top level gets — otherwise a struct holding a BIGINT would
// compare int64(5) against Java's float64(5).
func coerceForComparison(v any) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	case float32:
		return floatSpecialOrFloat(float64(x))
	case float64:
		return floatSpecialOrFloat(x)
	case bool:
		return x
	case string:
		return x
	case []byte:
		// Match Java's base64 encoding for BYTES.
		return base64Encode(x)
	case []any:
		// THE SHAPE THE DRIVER ACTUALLY RETURNS for an ARRAY column.
		// materializeDriverValue (cascades_generator.go) converts a row value
		// element-wise and hands arrays back as []any — NOT as api.Array — so
		// this is the arm real query results take, and without it a []any fell
		// to the pass-through default below with its elements unnormalized: an
		// array of BIGINTs stayed []any{int64…} and could never equal Java's
		// []any{float64…}, which is a permanent false divergence on every
		// array-valued column.
		return coerceSlice(x)
	case api.Struct:
		return coerceStruct(x)
	case api.Array:
		// The public interface, kept as a SECOND arm rather than as the only
		// one. The Go SQL runner does not produce it today — materializeDriverValue
		// flattens to []any first — but api.Array is what the driver's public
		// contract names, a Struct's attribute may carry one, and a runner that
		// scanned into api.Array would otherwise fall through to a pointer.
		// Defensive, and marked as such so nobody reads its presence as evidence
		// that arrays arrive this way.
		return coerceArray(x)
	}
	return v
}

// coerceSlice renders a driver []any the way Java's JSON decode renders a list,
// recursing so nested arrays, structs and scalars all normalise. Distinct from
// coerceArray only in the type it accepts; both produce the same shape.
func coerceSlice(s []any) any {
	out := make([]any, 0, len(s))
	for _, e := range s {
		out = append(out, coerceForComparison(e))
	}
	return out
}

// coerceStruct renders a STRUCT cell the way Java's JSON decode renders one:
// a map from attribute NAME to coerced value. Java's encoder emits a JSON
// object keyed by the struct's field names, so keying on MetaData's names is
// what makes the two sides comparable; the positional fallback below only
// applies when the metadata cannot name an attribute, which would otherwise
// silently drop that attribute from the map and make a shorter struct compare
// equal to a longer one.
func coerceStruct(s api.Struct) any {
	md := s.MetaData()
	attrs := s.Attributes()
	out := make(map[string]any, len(attrs))
	for i, a := range attrs {
		name := ""
		if md != nil {
			if n, err := md.AttributeName(i + 1); err == nil {
				name = n
			}
		}
		if name == "" {
			name = fmt.Sprintf("_%d", i)
		}
		out[name] = coerceForComparison(a)
	}
	return out
}

// coerceArray renders an ARRAY cell as Java's JSON decode does — a []any of
// coerced elements. A nil slice would render as `[]` either way, but an empty
// array and a NULL array are different answers, and NULL is handled by the nil
// check in coerceForComparison before this is reached.
func coerceArray(a api.Array) any {
	elems := a.Elements()
	out := make([]any, 0, len(elems))
	for _, e := range elems {
		out = append(out, coerceForComparison(e))
	}
	return out
}

// floatSpecialOrFloat returns "Infinity"/"-Infinity"/"NaN" for IEEE-754
// specials, matching how Java's encodeValue serialises them as strings
// (Gson's JsonPrimitive emits bare invalid-JSON tokens for these
// otherwise). Plain float64 values pass through unchanged.
func floatSpecialOrFloat(f float64) any {
	if math.IsInf(f, +1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	return f
}

// base64Encode renders a BYTES value to match Java's base64 wire
// representation (see encodeValue in conformance/sql_plan_steps.java).
func base64Encode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// isDMLQuery returns true if the SQL starts with INSERT/UPDATE/DELETE
// (after stripping whitespace). These need ExecContext, not QueryContext.
func isDMLQuery(sql string) bool {
	s := strings.TrimSpace(strings.ToUpper(sql))
	return strings.HasPrefix(s, "INSERT") ||
		strings.HasPrefix(s, "UPDATE") ||
		strings.HasPrefix(s, "DELETE")
}

// Compile-time assertion that goSQLRunner satisfies SetupRunner.
var _ SetupRunner = (*goSQLRunner)(nil)

// planCacheLog records the plan cache's part in the last planning call on its
// connection. The sequence runner resets last before each execution.
type planCacheLog struct{ last string }

func (l *planCacheLog) LogPlanGeneration(_ context.Context, info embedded.PlanGenerationInfo) {
	l.last = info.Cache.String()
}
