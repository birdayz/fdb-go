// Package sqlhunt is a SQL workload for the DST bug hunter: it drives the full relational stack
// (parser → Cascades planner → executor → record layer) over SimFDB under the same seed loop,
// fault schedule, shrink, and recording as the record-layer workload — the first example of
// "add a workload, hunt a new surface" (RFC-179 Tier 2).
//
// It sets a table up faults-free, then runs a random stream of IDEMPOTENT DML (absolute UPDATE +
// DELETE) under the commit-fault schedule, and after each batch compares the table's full
// contents to a Go row-model. Idempotent statements make faults transparent under autocommit
// retry, so any divergence is a real fault-induced SQL bug. Bare INSERT and relative
// `UPDATE … SET a=a+1` are deliberately excluded: their non-idempotency under commit_unknown is
// a KNOWN, Java-matching hazard (see TODO.md `## DST findings`), so they'd be known-hazard noise
// here — characterize those with dedicated regression tests, not this hunt.
package sqlhunt

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync/atomic"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	// registers the "fdbsql" database/sql driver and exposes RegisterBackend
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// sqlOpStream is the PCG stream for the SQL workload's op choices, distinct from the record
// workload's stream and from the Env's own streams.
const sqlOpStream = uint64(11)

// keyCounter uniquifies the per-run cache key so concurrent workers (and shrink re-runs of the
// same seed) never collide. It is NOT part of the persisted data — the database path is fixed —
// so it does not perturb determinism (same seed ⇒ same fingerprint).
var keyCounter atomic.Uint64

// SQLWorkload is the idempotent-DML SQL workload. The zero value is ready to use.
type SQLWorkload struct{}

func (SQLWorkload) Name() string { return "sql-dml" }

// Profiles are the SQL hunt profiles. SQL is heavier per op than the record layer (a full
// parse+plan+execute), so ops/keyspace are smaller. The runner merges these with the record
// profiles.
func Profiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "sql-dml", Cfg: hunt.Config{Workload: SQLWorkload{}, NumOps: 60, MaxPKs: 20, VerifyEvery: 15}},
		{Name: "sql-dml-dense", Cfg: hunt.Config{Workload: SQLWorkload{}, NumOps: 60, MaxPKs: 8, VerifyEvery: 15, FaultProb: 0.4}},
	}
}

func (SQLWorkload) Run(seed uint64, cfg hunt.Config) *hunt.Report {
	rep := &hunt.Report{Seed: seed}
	ctx := context.Background()

	h, err := newHarness(seed, cfg.FaultProb)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer h.close()

	rng := rand.New(rand.NewPCG(seed, sqlOpStream))
	model := map[int64]int64{}

	// --- pre-seed ~60% of the keyspace (faults still OFF) so the table starts populated and
	// both present/absent ids are exercised. ---
	for id := int64(0); id < cfg.MaxPKs; id++ {
		if rng.IntN(5) < 3 {
			v := int64(rng.Int32N(1_000_000))
			if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (?, ?)", id, v); err != nil {
				rep.Err = fmt.Sprintf("seed insert id=%d: %v", id, err)
				return rep
			}
			model[id] = v
		}
	}

	// --- workload (faults ON): only idempotent statements, so faults must be transparent. ---
	h.enableFaults()
	for i := 0; i < cfg.NumOps; i++ {
		id := rng.Int64N(cfg.MaxPKs)
		var err error
		if rng.IntN(10) < 7 {
			// Absolute UPDATE — idempotent under retry (re-setting a=v is a no-op the 2nd time).
			// Affects the row only if present, matching the model update below.
			v := int64(rng.Int32N(1_000_000))
			_, err = h.db.ExecContext(ctx, "UPDATE t SET a = ? WHERE id = ?", v, id)
			if err == nil {
				if _, ok := model[id]; ok {
					model[id] = v
				}
			}
		} else {
			// DELETE — idempotent under retry (deleting an absent row is a no-op).
			_, err = h.db.ExecContext(ctx, "DELETE FROM t WHERE id = ?", id)
			if err == nil {
				delete(model, id)
			}
		}
		rep.Ops = i + 1
		if err != nil {
			// Idempotent DML on this schema has no legitimate domain error, and autocommit
			// retries faults transparently — so any surfaced error is a real bug.
			rep.Err = fmt.Sprintf("op %d id=%d: %v", i, id, err)
			rep.FaultsFired = h.faults.Fired()
			return rep
		}
		if (i+1)%cfg.VerifyEvery == 0 {
			if v := verify(ctx, h.db, model); len(v) > 0 {
				rep.Violations = v
				rep.FaultsFired = h.faults.Fired()
				return rep
			}
		}
	}
	if v := verify(ctx, h.db, model); len(v) > 0 {
		rep.Violations = v
		rep.FaultsFired = h.faults.Fired()
		return rep
	}

	rep.FaultsFired = h.faults.Fired()
	rep.Fingerprint = hunt.Fingerprint(h.simDB)
	return rep
}

// harness owns the SQL-over-SimFDB plumbing for one run: a SimFDB-backed FDBDatabase registered
// under a unique cache key, with the schema created (faults off). enableFaults switches the
// commit-fault schedule on for the workload phase; close releases everything.
type harness struct {
	env    *dst.Env
	faults *dst.Buggifier
	simDB  *recordlayer.FDBDatabase
	db     *sql.DB
	closes []func()
}

// dbPath is FIXED across runs (only the cache key varies), so the persisted keyspace for a given
// seed is identical run-to-run and the fingerprint is a valid determinism probe. Backend
// isolation (a distinct SimFDB per unique key) keeps concurrent runs from colliding.
const dbPath = "/huntdb"

func newHarness(seed uint64, faultProb float64) (*harness, error) {
	env := dst.NewSim(seed)
	faults := env.Buggify
	if faultProb > 0 {
		faults.SetProbabilities(faultProb, faultProb)
	} else {
		faults = dst.DisabledBuggifier()
	}
	env.Buggify = dst.DisabledBuggifier() // setup phase runs fault-free

	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://sqlhunt/%d/%d", seed, keyCounter.Add(1))
	h := &harness{env: env, faults: faults, simDB: simDB}
	h.closes = append(h.closes, sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+key)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open setup: %w", err)
	}
	setup.SetMaxOpenConns(1)
	h.closes = append(h.closes, func() { setup.Close() })
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))",
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
	db.SetMaxOpenConns(1) // one conn ⇒ deterministic statement ordering, no pool races
	h.db = db
	h.closes = append(h.closes, func() { db.Close() })
	return h, nil
}

func (h *harness) enableFaults() { h.env.Buggify = h.faults }

func (h *harness) close() {
	// release in reverse order (conns before the cache unregister)
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}

// verify reads the whole table and compares it to the row-model — the SQL oracle. Returns each
// divergence as a string (capped), empty when the table matches the model exactly.
func verify(ctx context.Context, db *sql.DB, model map[int64]int64) []string {
	rows, err := db.QueryContext(ctx, "SELECT id, a FROM t ORDER BY id")
	if err != nil {
		return []string{"select: " + err.Error()}
	}
	defer rows.Close()

	got := map[int64]int64{}
	var order []int64
	for rows.Next() {
		var id, a int64
		if err := rows.Scan(&id, &a); err != nil {
			return []string{"scan: " + err.Error()}
		}
		got[id] = a
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		return []string{"rows.Err: " + err.Error()}
	}

	var viol []string
	add := func(f string, a ...any) {
		if len(viol) < 10 {
			viol = append(viol, fmt.Sprintf(f, a...))
		}
	}
	if len(got) != len(model) {
		add("row count: store=%d model=%d", len(got), len(model))
	}
	// deterministic iteration for stable violation text
	mids := make([]int64, 0, len(model))
	for id := range model {
		mids = append(mids, id)
	}
	sort.Slice(mids, func(i, j int) bool { return mids[i] < mids[j] })
	for _, id := range mids {
		gv, ok := got[id]
		if !ok {
			add("missing row id=%d (model a=%d)", id, model[id])
		} else if gv != model[id] {
			add("row id=%d: store a=%d model a=%d", id, gv, model[id])
		}
	}
	for _, id := range order {
		if _, ok := model[id]; !ok {
			add("extra row id=%d (store a=%d)", id, got[id])
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			add("ORDER BY id not ascending at %d", i)
			break
		}
	}
	return viol
}
