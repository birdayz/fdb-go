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
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"

	// Register the fdbsql driver for blank-import side-effects.
	_ "fdb.dev/pkg/relational/sqldriver"
)

// goSQLRunner is the in-process Go runner. It opens a database/sql
// connection to the embedded engine (via the registered fdbsql
// driver), runs the schema lifecycle + setup + query, and packages
// the result as a RowSet matching the Java side's wire shape.
type goSQLRunner struct {
	clusterFilePath string
}

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

// runEphemeral mirrors Java's runWithEphemeralSchema flow:
// CREATE SCHEMA TEMPLATE → CREATE DATABASE → CREATE SCHEMA →
// open connection on the ephemeral schema → run setup DMLs → run
// the query and capture its result. Tears the ephemeral state down
// in defer.
func (r *goSQLRunner) runEphemeral(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string) (RowSet, error) {
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
		return RowSet{}, fmt.Errorf("plandiff/go: open __SYS: %w", err)
	}
	defer sysDB.Close()

	templateCreated := false
	dbCreated := false
	defer func() {
		// Best-effort teardown. fdb-relational accepts bare identifiers
		// and paths; quoting them rejects with a parser error
		// ("database path must be /name").
		if dbCreated {
			_, _ = sysDB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath))
		}
		if templateCreated {
			_, _ = sysDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", templateName))
		}
	}()

	if schemaTemplate != "" {
		stmt := fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", templateName, schemaTemplate)
		if _, err := sysDB.ExecContext(ctx, stmt); err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: CREATE SCHEMA TEMPLATE: %w", err)
		}
		templateCreated = true

		if _, err := sysDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: CREATE DATABASE: %w", err)
		}
		dbCreated = true

		if _, err := sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schemaName, templateName)); err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: CREATE SCHEMA: %w", err)
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
		return RowSet{}, fmt.Errorf("plandiff/go: open ephemeral schema: %w", err)
	}
	defer schemaDB.Close()

	// Run setup DMLs.
	for _, setup := range setupSqls {
		if _, err := schemaDB.ExecContext(ctx, setup); err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: setup %q: %w", setup, err)
		}
	}

	// DML queries (INSERT/UPDATE/DELETE) use ExecContext and return rows-affected.
	if isDMLQuery(querySql) {
		result, err := schemaDB.ExecContext(ctx, querySql)
		if err != nil {
			return RowSet{}, fmt.Errorf("plandiff/go: exec: %w", err)
		}
		affected, _ := result.RowsAffected()
		return RowSet{
			Columns: []Column{{Name: "ROWS_AFFECTED", Type: "BIGINT"}},
			Rows:    [][]any{{float64(affected)}},
		}, nil
	}

	// Run the query and capture rows.
	sqlRows, err := schemaDB.QueryContext(ctx, querySql)
	if err != nil {
		return RowSet{}, fmt.Errorf("plandiff/go: query: %w", err)
	}
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
		Columns: make([]Column, len(colNames)),
		Rows:    [][]any{},
	}
	for i, name := range colNames {
		typeName := ""
		if i < len(colTypes) && colTypes[i] != nil {
			typeName = colTypes[i].DatabaseTypeName()
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
	}
	return v
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
