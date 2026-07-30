// This file adds a SECOND SQL workload to the DST bug hunter, focused on SECONDARY-INDEX
// maintenance under the commit-fault schedule (commit_unknown / not_committed / too_old retry).
//
// Where the sql-dml workload (sqlhunt.go) probes only the base table's row-model, this one puts a
// secondary index (idx_k on column k) in the loop: after every batch it checks BOTH the full
// table (SELECT id,k,v … ORDER BY id) AND, for every key value in the domain, the index-covered
// point query (SELECT id … WHERE k = <K> ORDER BY id). The second check is the whole point — it
// exercises the secondary index staying consistent with the base record under retry, and the
// planner using it. Because the workload is IDEMPOTENT DML (absolute UPDATE SET k=,v= + DELETE),
// faults must be transparent under autocommit retry, so ANY drift — a stale index entry, a missing
// index entry, an orphan — is a real fault-induced bug, not a known hazard.
//
// Bare INSERT and relative UPDATE are deliberately excluded (their non-idempotency under
// commit_unknown is a KNOWN Java-matching hazard); the setup phase that populates the table runs
// with faults OFF, and only the idempotent statements run with faults ON.
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
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// siOpStream is the PCG stream for THIS workload's op choices — distinct from the sql-dml stream
// (11), the record workload's stream (7), and the Env's own streams, so the op sequence is its
// own reproducible sub-stream of the one seed.
const siOpStream = uint64(23)

// siKeyDomain is the number of distinct values column k can take. Small on purpose: many ids share
// each k, so the index-covered query WHERE k = <K> returns multi-row sets whose membership must
// track the base table under retry. The oracle probes EVERY key in [0, siKeyDomain) each verify,
// so both present and absent key-sets are checked deterministically (no rng in the oracle).
const siKeyDomain = int64(5)

// siDBPath is FIXED across runs (only the cache key varies), so the persisted keyspace for a given
// seed is byte-identical run-to-run and hunt.Fingerprint is a valid determinism probe. Each run
// gets its own SimFDB backend under a unique cache key, so concurrent runs never collide.
const siDBPath = "/sidb"

// siKeyCounter uniquifies the per-run cache key (concurrent workers + shrink re-runs of the same
// seed). It is NOT persisted — the database path is fixed — so it does not perturb determinism.
var siKeyCounter atomic.Uint64

// siRow is the Go model's per-id payload: the (k, v) a present row currently holds.
type siRow struct {
	k int64
	v int64
}

// SQLIndexWorkload hunts secondary-index maintenance under faults with idempotent DML. Zero value
// is ready to use.
type SQLIndexWorkload struct{}

func (SQLIndexWorkload) Name() string { return "sql-index" }

// SQLIndexProfiles are the secondary-index hunt profiles. The runner merges these with the other
// workloads' profiles.
func SQLIndexProfiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "sql-index", Cfg: hunt.Config{Workload: SQLIndexWorkload{}, NumOps: 60, MaxPKs: 20, VerifyEvery: 15}},
		{Name: "sql-index-dense", Cfg: hunt.Config{Workload: SQLIndexWorkload{}, NumOps: 60, MaxPKs: 8, VerifyEvery: 15, FaultProb: 0.4}},
	}
}

func (SQLIndexWorkload) Run(seed uint64, cfg hunt.Config) *hunt.Report {
	rep := &hunt.Report{Seed: seed}
	ctx := context.Background()

	h, err := siNewHarness(seed, cfg.FaultProb)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer h.close()

	rng := rand.New(rand.NewPCG(seed, siOpStream))
	model := map[int64]siRow{}

	// --- pre-seed ~60% of the keyspace (faults still OFF), each with a k in [0, siKeyDomain) so
	// several ids share each key value and the index-covered query is exercised. ---
	for id := int64(0); id < cfg.MaxPKs; id++ {
		if rng.IntN(5) < 3 {
			k := rng.Int64N(siKeyDomain)
			v := int64(rng.Int32N(1_000_000))
			if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, k, v) VALUES (?, ?, ?)", id, k, v); err != nil {
				rep.Err = fmt.Sprintf("seed insert id=%d: %v", id, err)
				return rep
			}
			model[id] = siRow{k: k, v: v}
		}
	}

	// --- workload (faults ON): only idempotent statements, so faults must be transparent. ---
	h.enableFaults()
	for i := 0; i < cfg.NumOps; i++ {
		id := rng.Int64N(cfg.MaxPKs)
		var err error
		if rng.IntN(10) < 7 {
			// Absolute UPDATE of BOTH indexed (k) and non-indexed (v) columns — idempotent under
			// retry (re-setting to the same literals is a no-op the 2nd time). Moving k across key
			// values is exactly the index-maintenance path (delete old entry, insert new) whose
			// retry consistency this workload hunts. Affects the row only if present.
			k := rng.Int64N(siKeyDomain)
			v := int64(rng.Int32N(1_000_000))
			_, err = h.db.ExecContext(ctx, "UPDATE t SET k = ?, v = ? WHERE id = ?", k, v, id)
			if err == nil {
				if _, ok := model[id]; ok {
					model[id] = siRow{k: k, v: v}
				}
			}
		} else {
			// DELETE — idempotent under retry (deleting an absent row is a no-op). Must remove the
			// index entry too, else the WHERE k=<K> query would return an orphan id.
			_, err = h.db.ExecContext(ctx, "DELETE FROM t WHERE id = ?", id)
			if err == nil {
				delete(model, id)
			}
		}
		rep.Ops = i + 1
		if err != nil {
			// Idempotent DML on this schema has no legitimate domain error, and autocommit retries
			// faults transparently — so any surfaced error is a real bug.
			rep.Err = fmt.Sprintf("op %d id=%d: %v", i, id, err)
			rep.FaultsFired = h.faults.Fired()
			return rep
		}
		if (i+1)%cfg.VerifyEvery == 0 {
			if v := siVerify(ctx, h.db, model); len(v) > 0 {
				rep.Violations = v
				rep.FaultsFired = h.faults.Fired()
				return rep
			}
		}
	}
	if v := siVerify(ctx, h.db, model); len(v) > 0 {
		rep.Violations = v
		rep.FaultsFired = h.faults.Fired()
		return rep
	}

	rep.FaultsFired = h.faults.Fired()
	rep.Fingerprint = hunt.Fingerprint(h.simDB)
	return rep
}

// siHarness owns the SQL-over-SimFDB plumbing for one run: a SimFDB-backed FDBDatabase registered
// under a unique cache key, with a table + secondary index created (faults off). enableFaults
// switches the commit-fault schedule on for the workload phase; close releases everything.
type siHarness struct {
	env    *dst.Env
	faults *dst.Buggifier
	simDB  *recordlayer.FDBDatabase
	db     *sql.DB
	closes []func()
}

func siNewHarness(seed uint64, faultProb float64) (*siHarness, error) {
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

	key := fmt.Sprintf("sim://sqlindexhunt/%d/%d", seed, siKeyCounter.Add(1))
	h := &siHarness{env: env, faults: faults, simDB: simDB}
	h.closes = append(h.closes, sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+siDBPath+"?cluster_file="+key)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open setup: %w", err)
	}
	setup.SetMaxOpenConns(1)
	h.closes = append(h.closes, func() { setup.Close() })
	for _, stmt := range []string{
		"CREATE DATABASE " + siDBPath,
		// table + secondary index live in the same template string (the fdbsql template grammar
		// chains CREATE TABLE … CREATE INDEX … ON t (k)).
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT NOT NULL, k BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_k ON t (k)",
		"CREATE SCHEMA " + siDBPath + "/s WITH TEMPLATE tmpl",
	} {
		if _, err := setup.ExecContext(context.Background(), stmt); err != nil {
			h.close()
			return nil, fmt.Errorf("setup %q: %w", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+siDBPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // one conn ⇒ deterministic statement ordering, no pool races
	h.db = db
	h.closes = append(h.closes, func() { db.Close() })
	return h, nil
}

func (h *siHarness) enableFaults() { h.env.Buggify = h.faults }

func (h *siHarness) close() {
	// release in reverse order (conns before the cache unregister)
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}

// siVerify is the two-part secondary-index oracle. (a) the full table (SELECT id,k,v ORDER BY id)
// must equal the model row-for-row and be ascending; (b) for EVERY key value K in [0, siKeyDomain)
// the index-covered query (SELECT id WHERE k = K ORDER BY id) must equal exactly the model's ids
// with k==K, ascending. Part (b) is what catches a secondary index drifting from the base table
// under retry (stale/missing/orphan entries). No randomness — deterministic, full-domain sweep.
// Returns each divergence as a string (capped); empty when store and model agree everywhere.
func siVerify(ctx context.Context, db *sql.DB, model map[int64]siRow) []string {
	var viol []string
	add := func(f string, a ...any) {
		if len(viol) < 12 {
			viol = append(viol, fmt.Sprintf(f, a...))
		}
	}

	// ---- (a) full-table row-model check ----
	rows, err := db.QueryContext(ctx, "SELECT id, k, v FROM t ORDER BY id")
	if err != nil {
		return []string{"select table: " + err.Error()}
	}
	got := map[int64]siRow{}
	var order []int64
	for rows.Next() {
		var id, k, v int64
		if err := rows.Scan(&id, &k, &v); err != nil {
			rows.Close()
			return []string{"scan table: " + err.Error()}
		}
		got[id] = siRow{k: k, v: v}
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return []string{"rows.Err table: " + err.Error()}
	}
	rows.Close()

	if len(got) != len(model) {
		add("row count: store=%d model=%d", len(got), len(model))
	}
	mids := make([]int64, 0, len(model))
	for id := range model {
		mids = append(mids, id)
	}
	sort.Slice(mids, func(i, j int) bool { return mids[i] < mids[j] })
	for _, id := range mids {
		gr, ok := got[id]
		if !ok {
			add("missing row id=%d (model k=%d v=%d)", id, model[id].k, model[id].v)
			continue
		}
		if gr != model[id] {
			add("row id=%d: store k=%d v=%d model k=%d v=%d", id, gr.k, gr.v, model[id].k, model[id].v)
		}
	}
	for _, id := range order {
		if _, ok := model[id]; !ok {
			add("extra row id=%d (store k=%d v=%d)", id, got[id].k, got[id].v)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			add("ORDER BY id not ascending at %d", i)
			break
		}
	}

	// ---- (b) index-covered query check, for every key value in the domain ----
	for K := int64(0); K < siKeyDomain; K++ {
		// model expectation: ids whose current k == K, ascending.
		var want []int64
		for id, r := range model {
			if r.k == K {
				want = append(want, id)
			}
		}
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

		idxRows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE k = ? ORDER BY id", K)
		if err != nil {
			add("select k=%d: %v", K, err)
			continue
		}
		var have []int64
		var scanErr error
		for idxRows.Next() {
			var id int64
			if err := idxRows.Scan(&id); err != nil {
				scanErr = err
				break
			}
			have = append(have, id)
		}
		if scanErr == nil {
			scanErr = idxRows.Err()
		}
		idxRows.Close()
		if scanErr != nil {
			add("scan k=%d: %v", K, scanErr)
			continue
		}

		if len(have) != len(want) {
			add("index k=%d: store %d ids %v, model %d ids %v", K, len(have), have, len(want), want)
			continue
		}
		for i := range want {
			if have[i] != want[i] {
				add("index k=%d at pos %d: store id=%d model id=%d (store %v model %v)", K, i, have[i], want[i], have, want)
				break
			}
		}
		// ascending check on the index result itself
		for i := 1; i < len(have); i++ {
			if have[i] <= have[i-1] {
				add("index k=%d ORDER BY id not ascending at %d (%v)", K, i, have)
				break
			}
		}
	}

	return viol
}
