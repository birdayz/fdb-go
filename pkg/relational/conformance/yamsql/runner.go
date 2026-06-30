package yamsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/pkg/relational/api"
)

// RunConfig controls scenario execution.
type RunConfig struct {
	// DB is the target database. Must have fdbsql driver registered.
	DB *sql.DB
	// DBPath is the SQL-layer database path (e.g. "/confdb"). The runner
	// creates it, runs the scenario, and drops it at the end.
	DBPath string
	// SchemaName is the schema to instantiate under DBPath. Defaults
	// to "CONF".
	SchemaName string
	// TemplateName is the schema-template identifier. Must be unique per
	// scenario to avoid collisions across parallel runs.
	TemplateName string
}

// Result is the outcome of running one scenario.
type Result struct {
	Name       string
	TestsRun   int
	TestsPass  int
	TestsFail  int
	TestsSkip  int
	Failures   []Failure
	SetupError error // non-nil if schema/setup failed before any test ran
}

// Failure describes one mismatched test.
type Failure struct {
	Index   int    // 0-based position in Scenario.Tests
	Query   string // the failing query
	Message string // diff or error mismatch detail
}

// Run executes the scenario against cfg.DB and returns per-test results.
func Run(ctx context.Context, s *Scenario, cfg RunConfig) (*Result, error) {
	if cfg.DB == nil {
		return nil, errors.New("RunConfig.DB is required")
	}
	if cfg.DBPath == "" {
		return nil, errors.New("RunConfig.DBPath is required")
	}
	if cfg.TemplateName == "" {
		return nil, errors.New("RunConfig.TemplateName is required")
	}
	schema := cfg.SchemaName
	if schema == "" {
		schema = "conf"
	}

	r := &Result{Name: s.Name}

	if err := setup(ctx, cfg.DB, cfg.DBPath, schema, cfg.TemplateName, s); err != nil {
		r.SetupError = err
		return r, nil
	}
	defer teardown(ctx, cfg.DB, cfg.DBPath, schema, cfg.TemplateName)

	for i, t := range s.Tests {
		r.TestsRun++
		msg := runTest(ctx, cfg.DB, &t)
		if msg == "" {
			r.TestsPass++
			continue
		}
		r.TestsFail++
		r.Failures = append(r.Failures, Failure{
			Index:   i,
			Query:   t.Query,
			Message: msg,
		})
	}
	return r, nil
}

func setup(ctx context.Context, db *sql.DB, dbPath, schema, tmpl string, s *Scenario) error {
	// Best-effort cleanup of any prior run (idempotent reruns during dev).
	for _, stmt := range []string{
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath),
		fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", tmpl),
	} {
		_, _ = db.ExecContext(ctx, stmt)
	}

	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		return fmt.Errorf("CREATE DATABASE: %w", err)
	}
	ddl := fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, s.SchemaTemplate)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("CREATE SCHEMA TEMPLATE: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl)); err != nil {
		return fmt.Errorf("CREATE SCHEMA: %w", err)
	}
	for i, stmt := range s.Setup {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("setup[%d] %q: %w", i, stmt, err)
		}
	}
	return nil
}

func teardown(ctx context.Context, db *sql.DB, dbPath, schema, tmpl string) {
	_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", tmpl))
}

func runTest(ctx context.Context, db *sql.DB, t *Test) string {
	if t.EffectiveErrorCode() != "" {
		return runErrorTest(ctx, db, t)
	}
	// Non-query statements (UPDATE/DELETE/INSERT) go through Exec and
	// must not be sent to Query — the driver rejects them there. They
	// are sequenced steps that mutate state for a subsequent SELECT;
	// the scenario declares them with rows: absent or [] and the runner
	// asserts only that they succeed.
	if !IsQuery(t.Query) {
		if _, err := db.ExecContext(ctx, t.Query); err != nil {
			return fmt.Sprintf("exec error: %v", err)
		}
		if len(t.Rows) != 0 {
			return fmt.Sprintf("non-query statement returned no rows but scenario expects %d", len(t.Rows))
		}
		return ""
	}
	rows, err := db.QueryContext(ctx, t.Query)
	if err != nil {
		return fmt.Sprintf("query error: %v", err)
	}
	defer rows.Close()

	actual, err := scanAll(rows)
	if err != nil {
		return fmt.Sprintf("scan error: %v", err)
	}
	if d := diffRows(t.Rows, actual, t.Unordered); d != "" {
		return d
	}
	if t.PlanContains != "" || t.PlanNotContains != "" {
		return checkPlanAssertions(ctx, db, t.Query, t.PlanContains, t.PlanNotContains)
	}
	return ""
}

func checkPlanAssertions(ctx context.Context, db *sql.DB, query, contains, notContains string) string {
	rows, err := db.QueryContext(ctx, "EXPLAIN "+query)
	if err != nil {
		return fmt.Sprintf("EXPLAIN error: %v", err)
	}
	defer rows.Close()
	planRows, err := scanAll(rows)
	if err != nil {
		return fmt.Sprintf("EXPLAIN scan error: %v", err)
	}
	if len(planRows) == 0 || len(planRows[0]) == 0 {
		return "EXPLAIN returned no plan"
	}
	plan := fmt.Sprint(planRows[0][0])
	if contains != "" && !strings.Contains(plan, contains) {
		return fmt.Sprintf("plan does not contain %q:\n  %s", contains, plan)
	}
	if notContains != "" && strings.Contains(plan, notContains) {
		return fmt.Sprintf("plan should not contain %q:\n  %s", notContains, plan)
	}
	return ""
}

func runErrorTest(ctx context.Context, db *sql.DB, t *Test) string {
	var err error
	if IsQuery(t.Query) {
		rows, qerr := db.QueryContext(ctx, t.Query)
		if qerr == nil {
			// SELECT errors may surface only during row iteration (e.g.
			// div/0 in a projection), not at query-prepare time.
			_, err = scanAll(rows)
			rows.Close()
			if err == nil {
				return fmt.Sprintf("expected error %s, got nil", t.EffectiveErrorCode())
			}
		} else {
			err = qerr
		}
	} else {
		_, err = db.ExecContext(ctx, t.Query)
		if err == nil {
			return fmt.Sprintf("expected error %s, got nil", t.EffectiveErrorCode())
		}
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return fmt.Sprintf("expected *api.Error with code %s, got %T: %v", t.EffectiveErrorCode(), err, err)
	}
	gotCode := strings.TrimSpace(string(apiErr.Code))
	wantCode := strings.TrimSpace(t.EffectiveErrorCode())
	if gotCode != wantCode {
		return fmt.Sprintf("expected error code %q, got %q (msg: %s)", wantCode, gotCode, apiErr.Message)
	}
	return ""
}

// IsQuery reports whether stmt should be routed through database/sql's
// Query path. SELECT (and its lead keywords WITH / VALUES) return
// result sets; everything else goes through Exec. Strips a leading
// paren so `(SELECT ...)` counts as a query.
func IsQuery(stmt string) bool {
	s := strings.TrimLeft(stmt, " \t\r\n(")
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '(' {
			s = s[:i]
			break
		}
	}
	switch strings.ToUpper(s) {
	case "SELECT", "WITH", "VALUES":
		return true
	}
	return false
}

func scanAll(rows *sql.Rows) ([][]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, dest)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
