// Package metamorphic is the WRONG-catching SQL oracle for the DST harness (RFC-179 Tier 2). It
// tests the query engine's correctness WITHOUT an external reference: a Scenario asserts that the
// queries in each equivalence Group return the same result, and the deterministic engine (over
// SimFDB) is the judge — run them all, compare. A mismatch is a real finding: EITHER an engine bug
// OR a wrong equivalence claim (triage). No known-correct answer is needed; a single buggy rule or
// executor path cannot keep two logically-equivalent queries returning the same rows.
//
// These Scenarios are the natural target of the LLM-adversarial generator (RFC-179): the LLM's
// semantic knowledge proposes the equivalence claims; this package's deterministic execution is the
// ground truth. Hand-written seed relations live in SeedCorpus; generated ones load from JSON.
package metamorphic

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	// registers the "fdbsql" driver + exposes RegisterBackend
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

var keyCounter atomic.Uint64

const dbPath = "/mmdb"

// Scenario is a schema + seed data + equivalence groups. Field tags let the LLM generator emit it
// as JSON.
type Scenario struct {
	Name   string   `json:"name"`
	Seed   uint64   `json:"seed"`
	Tables []string `json:"tables"` // CREATE TABLE / CREATE INDEX bodies for the schema template
	Data   []string `json:"data"`   // INSERT statements
	Groups []Group  `json:"groups"`
}

// Group asserts every query returns the same result. Reason records WHY the author (LLM or human)
// believes them equivalent — read during triage. Ordered compares rows in returned order; the
// default compares them as a multiset (order-independent), which is what "equivalent query" means
// for most rewrites.
type Group struct {
	Name    string   `json:"name"`
	Reason  string   `json:"reason"`
	Ordered bool     `json:"ordered,omitempty"`
	Queries []string `json:"queries"`
}

// Violation is one failed equivalence (or an errored query within a group).
type Violation struct {
	Scenario string
	Group    string
	Detail   string
}

func (v Violation) String() string {
	return fmt.Sprintf("[%s/%s] %s", v.Scenario, v.Group, v.Detail)
}

// Check sets up the scenario over a fresh SimFDB-backed SQL stack (faults off, deterministic) and
// evaluates every equivalence group. Returns all violations (empty = the engine satisfied every
// asserted equivalence). A setup error (bad schema/data) is returned separately.
func Check(s Scenario) ([]Violation, error) {
	ctx := context.Background()
	h, err := setup(s)
	if err != nil {
		return nil, err
	}
	defer h.close()

	var viols []Violation
	for _, g := range s.Groups {
		if len(g.Queries) < 2 {
			continue // nothing to compare
		}
		results := make([][]string, len(g.Queries))
		errored := make([]bool, len(g.Queries))
		for i, q := range g.Queries {
			rows, err := runQuery(ctx, h.db, q, g.Ordered)
			if err != nil {
				errored[i] = true
				viols = append(viols, Violation{s.Name, g.Name, fmt.Sprintf("query[%d] errored (%s): %q: %v", i, g.Reason, q, err)})
				continue
			}
			results[i] = rows
		}
		// Compare each query to the first non-errored one.
		base := -1
		for i := range g.Queries {
			if !errored[i] {
				base = i
				break
			}
		}
		if base < 0 {
			continue
		}
		for i := base + 1; i < len(g.Queries); i++ {
			if errored[i] || equalRows(results[base], results[i]) {
				continue
			}
			viols = append(viols, Violation{s.Name, g.Name, fmt.Sprintf(
				"NOT equivalent (%s): q[%d] %q → %d rows; q[%d] %q → %d rows\n      base=%s\n      other=%s",
				g.Reason, base, g.Queries[base], len(results[base]), i, g.Queries[i], len(results[i]),
				sample(results[base]), sample(results[i]))})
		}
	}
	return viols, nil
}

func runQuery(ctx context.Context, db *sql.DB, q string, ordered bool) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, formatRow(vals))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !ordered {
		sort.Strings(out) // multiset comparison: order-independent
	}
	return out, nil
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// formatRow renders a row's VALUES (not column names, so aliases don't matter for equivalence):
// NULL for nil, %q for bytes/strings, %v otherwise.
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

func sample(rows []string) string {
	const max = 6
	if len(rows) <= max {
		return "{" + strings.Join(rows, " ") + "}"
	}
	return "{" + strings.Join(rows[:max], " ") + " …+" + fmt.Sprint(len(rows)-max) + "}"
}

// harness owns one scenario's SQL-over-SimFDB plumbing.
type harness struct {
	db     *sql.DB
	closes []func()
}

func setup(s Scenario) (*harness, error) {
	ctx := context.Background()
	env := dst.NewSim(s.Seed)
	env.Buggify = dst.DisabledBuggifier()
	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://metamorphic/%s/%d", s.Name, keyCounter.Add(1))
	h := &harness{}
	h.closes = append(h.closes, sqldriver.RegisterBackend(key, simDB))

	setupConn, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+key)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open setup: %w", err)
	}
	setupConn.SetMaxOpenConns(1)
	h.closes = append(h.closes, func() { setupConn.Close() })
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE tmpl " + strings.Join(s.Tables, " "),
		"CREATE SCHEMA " + dbPath + "/s WITH TEMPLATE tmpl",
	} {
		if _, err := setupConn.ExecContext(ctx, stmt); err != nil {
			h.close()
			return nil, fmt.Errorf("setup %q: %w", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	h.db = db
	h.closes = append(h.closes, func() { db.Close() })
	for _, stmt := range s.Data {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			h.close()
			return nil, fmt.Errorf("data %q: %w", stmt, err)
		}
	}
	return h, nil
}

func (h *harness) close() {
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}
