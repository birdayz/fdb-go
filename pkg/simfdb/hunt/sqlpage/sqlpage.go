// Package sqlpage is RFC-179 Tier 2's SQL-query pagination oracle.
//
// The continuation driver (pkg/simfdb/hunt/continuation) paginates RAW record and index scans. This
// one goes a layer up: it paginates whole SQL QUERIES — filter, ORDER BY, GROUP BY, DISTINCT, joins —
// so the executor's cursor tree (and its per-operator continuation logic) is what's under test. That
// is where a subtle bug like the FlatMap mid-inner continuation drop lived: a correlated join where
// one outer row matches several inner rows, resumed mid-inner, silently dropped rows.
//
// The oracle is metamorphic — the engine judges itself, no external reference. It runs each query
// twice on one pinned connection: once with the execution scanned-rows limit at its default (no
// pagination — the REFERENCE), once with a tiny limit that forces the executor to exhaust its budget
// and resume through an internal continuation many times over. The two results must be identical
// (multiset, or ordered for a total-ordered query). A divergence is a continuation DROP or DUPLICATE
// in some operator's resume path.
//
// Deterministic over SimFDB: a fixed database path (the path is baked into keys), a unique backend
// cache key per run (an atomic counter), a single SQL connection (SetMaxOpenConns(1) — no pool
// races), and faults off. Same seed ⇒ same data ⇒ same verdict.
package sqlpage

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync/atomic"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// dbPath is fixed because the SQL layer bakes the database path into stored keys; a per-run path
// would make the keyspace (and the determinism fingerprint) vary. Isolation comes from the unique
// backend cache key instead.
const dbPath = "/sqlpagedb"

// keyCounter uniquifies each run's backend cache key so concurrent runs never share a SimFDB.
var keyCounter atomic.Uint64

// noPageLimit is the reference scanned-rows limit: large enough that no query in a run ever hits it,
// so the reference execution never paginates.
const noPageLimit = 1_000_000_000

// pageLimits are the tiny scanned-rows budgets that force heavy continuation churn. 1 is the hard
// case — the executor resumes after almost every scanned row.
var pageLimits = []int{1, 2, 3}

// Workload is the SQL-pagination oracle, a hunt.Workload. Self-parameterized; faults off.
type Workload struct {
	Rows int // rows seeded per table; more rows = more pages = more continuation resumes (default 30)
}

func (Workload) Name() string { return "sqlpage" }

func (w Workload) Run(seed uint64, _ hunt.Config) *hunt.Report { return w.run(seed).Report }

func (w Workload) withDefaults() Workload {
	if w.Rows <= 0 {
		w.Rows = 30
	}
	return w
}

// Result is the driver's rich outcome: the hunt.Report plus the number of (query, page-limit) checks.
type Result struct {
	Report *hunt.Report
	Checks int
}

func (w Workload) run(seed uint64) *Result {
	w = w.withDefaults()
	rng := rand.New(rand.NewPCG(seed, 23))
	ctx := context.Background()

	rep := &hunt.Report{Seed: seed}
	res := &Result{Report: rep}

	h, err := newHarness(seed)
	if err != nil {
		rep.Err = fmt.Sprintf("harness: %v", err)
		return res
	}
	defer h.close()

	if err := w.seed(ctx, h.db, rng); err != nil {
		rep.Err = fmt.Sprintf("seed: %v", err)
		return res
	}

	// Pin one connection for every query so the scanned-rows option is stable and the single
	// connection is never contended (SetMaxOpenConns(1)).
	conn, err := h.db.Conn(ctx)
	if err != nil {
		rep.Err = fmt.Sprintf("pin conn: %v", err)
		return res
	}
	defer func() { _ = conn.Close() }()

	qs := queries(rng)
	for _, q := range qs {
		// Reference: no pagination.
		if err := setPageLimit(conn, noPageLimit); err != nil {
			rep.Err = fmt.Sprintf("set ref limit: %v", err)
			return res
		}
		ref, err := runQuery(ctx, conn, q.sql)
		if err != nil {
			// A query the engine can't plan at all is unsupported SQL, not a pagination bug — skip
			// it (the reference itself failed, so there's nothing to compare against).
			continue
		}
		for _, limit := range pageLimits {
			if err := setPageLimit(conn, limit); err != nil {
				rep.Err = fmt.Sprintf("set page limit %d: %v", limit, err)
				return res
			}
			paged, err := runQuery(ctx, conn, q.sql)
			res.Checks++
			if err != nil {
				// The reference planned but the paginated form errored — pagination must not change
				// whether a query executes. That is itself a finding.
				rep.Violations = append(rep.Violations, fmt.Sprintf(
					"query %q errored under scanned-rows-limit=%d but not unpaged: %v", q.sql, limit, err,
				))
				return res
			}
			if d := compare(ref, paged, q.ordered); d != "" {
				rep.Violations = append(rep.Violations, fmt.Sprintf(
					"pagination divergence: query %q at scanned-rows-limit=%d — %s (an executor "+
						"continuation drop/duplicate in resume)", q.sql, limit, d,
				))
				return res
			}
		}
	}

	rep.Ops = res.Checks
	rep.Fingerprint = hunt.Fingerprint(h.simDB)
	return res
}

// setPageLimit configures the pinned connection's execution scanned-rows limit.
func setPageLimit(conn *sql.Conn, limit int) error {
	return conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T, want *embedded.EmbeddedConnection", dc)
		}
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, limit).Build())
		return nil
	})
}

// query is a SQL statement plus whether its result order is a contract (a total ORDER BY).
type query struct {
	sql     string
	ordered bool
}

// queries is the per-run query matrix: full scan, index-eligible filters, total-ordered scans and a
// LIMIT prefix, grouped aggregates, DISTINCT, and two joins — the second (b.ref > a.id) is the
// correlated multi-inner-match shape that exercises the FlatMap continuation directly.
func queries(rng *rand.Rand) []query {
	k := rng.Int64N(4)
	n := 3 + rng.IntN(6)
	return []query{
		{"SELECT id, cat, val FROM t", false},
		{fmt.Sprintf("SELECT id, val FROM t WHERE cat = %d", k), false},
		{fmt.Sprintf("SELECT id, val FROM t WHERE val > %d", k), false},
		{"SELECT id, cat, val FROM t ORDER BY val, id", true},
		{"SELECT id, cat, val FROM t ORDER BY cat, id", true},
		{fmt.Sprintf("SELECT id, val FROM t ORDER BY val, id LIMIT %d", n), true},
		{"SELECT cat, COUNT(*) FROM t GROUP BY cat", false},
		{"SELECT cat, SUM(val) FROM t GROUP BY cat", false},
		// QUARANTINED (two known executor-continuation bugs, both TODO.md "## DST findings", pinned by
		// fix-detector tests, kept out of the sweep so it hunts other shapes):
		//  1. bare `SELECT DISTINCT cat FROM t` — streaming DISTINCT drops its dedup state across a
		//     continuation resume (TestKnownBug_DistinctContinuation). GROUP BY (correct) stays in.
		//  2. multi-value `SELECT id FROM t WHERE cat IN (2,3)` — the InJoin/concat has no per-branch
		//     continuation, so it errors 54F01 under a tiny scanned-rows limit (TestKnownBug_InJoinContinuation).
		// Re-add each here once its executor gains continuation support.
		{"SELECT a.id, b.id FROM t a, t2 b WHERE b.ref = a.id", false},
		{"SELECT a.id, b.id FROM t a, t2 b WHERE b.ref > a.id", false},
	}
}

// runQuery executes q on the connection and returns each row as a canonical "col|col|..." string
// (NULLs rendered as the literal NULL), so results of any shape compare uniformly.
func runQuery(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, 16)
	cells := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range cells {
		ptrs[i] = &cells[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			if c.Valid {
				parts[i] = c.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// compare returns "" when ref and got match — as an ordered sequence for a total-ordered query, else
// as a multiset — otherwise a short description of the first divergence.
func compare(ref, got []string, ordered bool) string {
	r, g := ref, got
	if !ordered {
		r = sortedCopy(ref)
		g = sortedCopy(got)
	}
	if len(r) != len(g) {
		return fmt.Sprintf("row count %d (unpaged) != %d (paged)", len(r), len(g))
	}
	for i := range r {
		if r[i] != g[i] {
			return fmt.Sprintf("row %d: unpaged %q != paged %q", i, r[i], g[i])
		}
	}
	return ""
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

// seed inserts w.Rows rows into t (id, cat, val — cat/val nullable) and t2 (id, ref→t.id, w),
// with ~20% NULLs so NULL ordering and NULL-skipping aggregation are on the pagination path.
func (w Workload) seed(ctx context.Context, db *sql.DB, rng *rand.Rand) error {
	for i := 0; i < w.Rows; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, cat, val) VALUES (%d, %s, %s)",
			i, nullableInt(rng, 4), nullableInt(rng, 20))); err != nil {
			return err
		}
	}
	for i := 0; i < w.Rows; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO t2 (id, ref, w) VALUES (%d, %d, %s)",
			i, rng.Int64N(int64(w.Rows)), nullableInt(rng, 20))); err != nil {
			return err
		}
	}
	return nil
}

func nullableInt(rng *rand.Rand, domain int64) string {
	if rng.IntN(5) == 0 {
		return "NULL"
	}
	return fmt.Sprintf("%d", rng.Int64N(domain))
}

// harness owns the SQL-over-SimFDB plumbing for one run: a SimFDB-backed FDBDatabase registered
// under a unique cache key, with the two-table schema + three secondary indexes created. Faults off.
type harness struct {
	simDB  *recordlayer.FDBDatabase
	db     *sql.DB
	closes []func()
}

func newHarness(seed uint64) (*harness, error) {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()

	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://sqlpage/%d/%d", seed, keyCounter.Add(1))
	h := &harness{simDB: simDB}
	h.closes = append(h.closes, sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+key)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open setup: %w", err)
	}
	setup.SetMaxOpenConns(1)
	h.closes = append(h.closes, func() { _ = setup.Close() })
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE tmpl " +
			"CREATE TABLE t (id BIGINT NOT NULL, cat BIGINT, val BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE t2 (id BIGINT NOT NULL, ref BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX idx_cat ON t (cat) " +
			"CREATE INDEX idx_val ON t (val) " +
			"CREATE INDEX idx_ref ON t2 (ref)",
		"CREATE SCHEMA " + dbPath + "/s WITH TEMPLATE tmpl",
	} {
		if _, err := setup.ExecContext(context.Background(), stmt); err != nil {
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
	h.closes = append(h.closes, func() { _ = db.Close() })
	return h, nil
}

func (h *harness) close() {
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}

// Profiles is the sqlpage sweep matrix: a balanced set and a denser one (more rows ⇒ more pages ⇒
// more continuation resumes per query).
func Profiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "sqlpage", Cfg: hunt.Config{Workload: Workload{Rows: 30}}},
		{Name: "sqlpage-dense", Cfg: hunt.Config{Workload: Workload{Rows: 60}}},
	}
}
