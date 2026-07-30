// Package golden is a characterization ("golden master") harness for the SQL engine over SimFDB.
// For a curated corpus of (schema, seeded data, queries) it captures the engine's EXPLAIN plan
// AND result rows as a canonical text baseline committed under testdata/. The test diffs a fresh
// capture against the baseline every run: any behavior delta — a changed result (almost always a
// bug) or a changed plan (a cost-model / rule change) — surfaces as a reviewable golden diff at
// merge/release. Regenerate with GOLDEN_UPDATE=1 and review the diff before committing it.
//
// This catches CHANGED, not WRONG: a bug baked into the baseline stays green, so it is the
// COMPLEMENT of the metamorphic oracle, never a substitute. It is only meaningful because SimFDB
// makes the engine's output a deterministic function of (schema, data, query) — pre-SimFDB the
// plan/result carried nondeterministic bits and a baseline was noise. See RFC-199 Tier 2 oracles.
package golden

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	// registers the "fdbsql" driver and exposes RegisterBackend
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

// keyCounter uniquifies the per-capture cache key so concurrent captures never collide. The
// database path is FIXED, so the key (not persisted) never perturbs the captured bytes.
var keyCounter atomic.Uint64

const dbPath = "/goldendb"

// Scenario is one corpus entry: a schema (CREATE TABLE / CREATE INDEX bodies for a schema
// template), deterministic seed data, and the queries to capture. Seed drives SimFDB's clock/
// version determinism (the data itself is literal, not random, so the baseline is stable and
// reviewable).
type Scenario struct {
	Name    string
	Seed    uint64
	Tables  []string // CREATE TABLE / CREATE INDEX statements placed inside the schema template
	Data    []string // INSERT statements run on the workload connection, in order
	Queries []string // SELECT targets; each is captured as its EXPLAIN plan + result rows
}

// CaptureSurface names which of a query's two captured surfaces failed.
type CaptureSurface int

const (
	// SurfacePlan is the EXPLAIN. A query can fail here and run fine (an unplannable shape).
	SurfacePlan CaptureSurface = iota
	// SurfaceRows is executing the query. A query can fail here after EXPLAIN succeeded (any
	// error raised while evaluating rows — division by zero, a failing cast).
	SurfaceRows
)

func (s CaptureSurface) String() string {
	if s == SurfacePlan {
		return "EXPLAIN"
	}
	return "query"
}

// CaptureError reports a scenario query that did not run, and WHICH surface it failed on.
//
// The surface is a field rather than something to read out of the message because the two guards
// that produce it are independently defeatable: a guard on only one surface still lets the other
// bake its error into a baseline. A test asserting "this input exercises the rows guard" has to be
// able to say so structurally, or it silently stops covering that guard the day the input starts
// failing at plan time instead.
type CaptureError struct {
	Scenario string
	Query    string
	Surface  CaptureSurface
	Err      error
}

func (e *CaptureError) Error() string {
	return fmt.Sprintf("scenario %q: %s %q: %v", e.Scenario, e.Surface, e.Query, e.Err)
}

func (e *CaptureError) Unwrap() error { return e.Err }

// Capture runs the scenario over a fresh SimFDB-backed SQL stack and returns the canonical text
// baseline: for each query, its EXPLAIN plan and its result rows in returned order (so the golden
// also locks row ordering — a plan change that reorders rows is a real, reviewable delta).
func Capture(s Scenario) (string, error) {
	ctx := context.Background()

	env := dst.NewSim(s.Seed)
	env.Buggify = dst.DisabledBuggifier() // golden is a correctness/behavior snapshot, not a fault run
	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://golden/%s/%d", s.Name, keyCounter.Add(1))
	unreg := sqldriver.RegisterBackend(key, simDB)
	defer unreg()

	setup, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+key)
	if err != nil {
		return "", fmt.Errorf("open setup: %w", err)
	}
	setup.SetMaxOpenConns(1)
	defer setup.Close()
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE tmpl " + strings.Join(s.Tables, " "),
		"CREATE SCHEMA " + dbPath + "/s WITH TEMPLATE tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			return "", fmt.Errorf("setup %q: %w", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		return "", fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // one conn ⇒ deterministic ordering, no pool races
	defer db.Close()
	for _, stmt := range s.Data {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return "", fmt.Errorf("data %q: %w", stmt, err)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# scenario: %s (seed %d)\n", s.Name, s.Seed)
	for _, tbl := range s.Tables {
		fmt.Fprintf(&b, "#   %s\n", tbl)
	}
	b.WriteString("\n")
	// A failing query is a CAPTURE FAILURE, never baseline content.
	//
	// This used to write "PLAN-ERR: %v" / "ROWS-ERR: %v" into the text and return it as a
	// successful capture. Under GOLDEN_UPDATE=1 that bakes the error message into testdata/,
	// and from then on the corpus entry passes forever by reproducing its own breakage — the
	// characterization harness certifying that a query still fails the same way. Worse, it is
	// invisible on review: a baseline full of green -ERR lines looks exactly like a baseline
	// full of green results in a CI summary.
	//
	// The corpus is curated, so every query in it is expected to run. If one stops running,
	// that is the finding, and it has to surface as a red test rather than as a diff nobody
	// reads.
	for _, q := range s.Queries {
		fmt.Fprintf(&b, "=== %s\n", q)
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			return "", &CaptureError{Scenario: s.Name, Query: q, Surface: SurfacePlan, Err: err}
		}
		fmt.Fprintf(&b, "PLAN: %s\n", plan)
		rowsText, err := captureRows(ctx, db, q)
		if err != nil {
			return "", &CaptureError{Scenario: s.Name, Query: q, Surface: SurfaceRows, Err: err}
		}
		b.WriteString(rowsText)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// captureRows runs q and formats its rows in returned order.
func captureRows(ctx context.Context, db *sql.DB, q string) (string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		lines = append(lines, formatRow(vals))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ROWS(%d) cols=[%s]:\n", len(lines), strings.Join(cols, ", "))
	for _, l := range lines {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	return b.String(), nil
}

// formatRow renders one row canonically: NULL for nil, %q for bytes/strings, %v otherwise.
func formatRow(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch x := v.(type) {
		case nil:
			parts[i] = "NULL"
		case []byte:
			parts[i] = fmt.Sprintf("%q", string(x))
		case string:
			parts[i] = fmt.Sprintf("%q", x)
		default:
			parts[i] = fmt.Sprintf("%v", x)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
