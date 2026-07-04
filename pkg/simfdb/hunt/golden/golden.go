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
// plan/result carried nondeterministic bits and a baseline was noise. See RFC-179 Tier 2 oracles.
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
	for _, q := range s.Queries {
		fmt.Fprintf(&b, "=== %s\n", q)
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			fmt.Fprintf(&b, "PLAN-ERR: %v\n", err)
		} else {
			fmt.Fprintf(&b, "PLAN: %s\n", plan)
		}
		rowsText, err := captureRows(ctx, db, q)
		if err != nil {
			fmt.Fprintf(&b, "ROWS-ERR: %v\n", err)
		} else {
			b.WriteString(rowsText)
		}
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
