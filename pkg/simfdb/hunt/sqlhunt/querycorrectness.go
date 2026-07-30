// This file adds a QUERY-CORRECTNESS workload to the DST bug hunter. Where the sql-dml workload
// (sqlhunt.go) hunts fault-idempotency, this one hunts PLANNER/EXECUTOR correctness — wrong rows,
// wrong aggregates, bad ORDER BY / LIMIT — with faults OFF.
//
// The shape: a three-column table (id, cat, val) with cat low-cardinality (so filters and groups
// have multiple rows) and two secondary indexes (on cat and on val) so the Cascades planner has
// real index-scan choices for the predicate/order/limit queries. A seeded stream of mutations
// (INSERT new id, absolute UPDATE SET val=<lit>/cat=<lit>, DELETE) is kept in lockstep with a Go
// row-model, and after every VerifyEvery ops a BATTERY of read queries (COUNT/SUM/MIN/MAX with
// filters, ORDER BY, LIMIT) is checked against the same result computed INDEPENDENTLY in Go by
// iterating the model. Any divergence is a real wrong-answer bug the planner or executor produced.
//
// Faults are OFF (hunt.NewSimEnv(seed, 0) / DisabledBuggifier): this workload is not probing
// commit-fault idempotency, so non-idempotent INSERT is fine and free of the known Java-matching
// commit_unknown hazard that constrains sqlhunt.go.
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

// qcOpStream is the PCG stream for the query-correctness workload's op/parameter choices, distinct
// from the record workload's stream (7), the sql-dml stream (11), and the Env's own streams.
const qcOpStream = uint64(23)

// qcCatCard is the cardinality of the cat column: cat = rng % qcCatCard. Kept small so equality
// filters and groups match several rows, giving the aggregates and ORDER BY real work.
const qcCatCard = 4

// qcValRange bounds the val column (and the derived predicate thresholds): val in [0, qcValRange).
// Small enough that val > X / val <= Y filters split the table both ways at realistic thresholds.
const qcValRange = 1000

// qcKeyCounter uniquifies the per-run cache key so concurrent workers (and repeat runs of the same
// seed) never collide. It is NOT persisted — qcDBPath is fixed — so it never perturbs determinism.
var qcKeyCounter atomic.Uint64

// qcDBPath is FIXED across runs (only the cache key varies), so the persisted keyspace for a given
// seed is identical run-to-run and hunt.Fingerprint is a valid determinism probe.
const qcDBPath = "/qcdb"

// qcRow is one row of the Go row-model.
type qcRow struct {
	cat int64
	val int64
}

// QueryCorrectnessWorkload hunts SQL read-path correctness under the DST harness. The zero value is
// ready to use.
type QueryCorrectnessWorkload struct{}

func (QueryCorrectnessWorkload) Name() string { return "sql-query" }

// QueryProfiles are the query-correctness hunt profiles. Faults are OFF (FaultProb stays 0) — this
// workload probes planner/executor answers, not fault idempotency. The runner merges these with the
// record and sql-dml profiles.
func QueryProfiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "sql-query", Cfg: hunt.Config{Workload: QueryCorrectnessWorkload{}, NumOps: 80, MaxPKs: 24, VerifyEvery: 16}},
		{Name: "sql-query-dense", Cfg: hunt.Config{Workload: QueryCorrectnessWorkload{}, NumOps: 80, MaxPKs: 8, VerifyEvery: 16}},
	}
}

func (QueryCorrectnessWorkload) Run(seed uint64, cfg hunt.Config) *hunt.Report {
	rep := &hunt.Report{Seed: seed}
	ctx := context.Background()

	h, err := qcNewHarness(seed)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer h.close()

	rng := rand.New(rand.NewPCG(seed, qcOpStream))
	model := map[int64]qcRow{}

	// --- pre-seed ~60% of the keyspace so the table starts populated and the read battery has
	// multiple rows per cat from op 0. ---
	for id := int64(0); id < cfg.MaxPKs; id++ {
		if rng.IntN(5) < 3 {
			row := qcRow{cat: int64(rng.IntN(qcCatCard)), val: int64(rng.Int32N(qcValRange))}
			if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, cat, val) VALUES (?, ?, ?)", id, row.cat, row.val); err != nil {
				rep.Err = fmt.Sprintf("seed insert id=%d: %v", id, err)
				return rep
			}
			model[id] = row
		}
	}

	// --- workload: interleave mutations (kept in lockstep with the model) with the read battery. ---
	for i := 0; i < cfg.NumOps; i++ {
		id := rng.Int64N(cfg.MaxPKs)
		r := rng.IntN(100)
		var err error
		switch {
		case r < 30:
			// INSERT a new id. If it is already present, absolute-UPDATE its val instead (both
			// paths keep the model in lockstep) so the op is never wasted.
			if _, ok := model[id]; ok {
				v := int64(rng.Int32N(qcValRange))
				_, err = h.db.ExecContext(ctx, "UPDATE t SET val = ? WHERE id = ?", v, id)
				if err == nil {
					row := model[id]
					row.val = v
					model[id] = row
				}
			} else {
				row := qcRow{cat: int64(rng.IntN(qcCatCard)), val: int64(rng.Int32N(qcValRange))}
				_, err = h.db.ExecContext(ctx, "INSERT INTO t (id, cat, val) VALUES (?, ?, ?)", id, row.cat, row.val)
				if err == nil {
					model[id] = row
				}
			}
		case r < 55:
			// absolute UPDATE of val (affects the row only if present, matching the model update).
			v := int64(rng.Int32N(qcValRange))
			_, err = h.db.ExecContext(ctx, "UPDATE t SET val = ? WHERE id = ?", v, id)
			if err == nil {
				if row, ok := model[id]; ok {
					row.val = v
					model[id] = row
				}
			}
		case r < 75:
			// absolute UPDATE of cat.
			c := int64(rng.IntN(qcCatCard))
			_, err = h.db.ExecContext(ctx, "UPDATE t SET cat = ? WHERE id = ?", c, id)
			if err == nil {
				if row, ok := model[id]; ok {
					row.cat = c
					model[id] = row
				}
			}
		default:
			// DELETE.
			_, err = h.db.ExecContext(ctx, "DELETE FROM t WHERE id = ?", id)
			if err == nil {
				delete(model, id)
			}
		}
		rep.Ops = i + 1
		if err != nil {
			// Faults are off and the schema has no domain error for these statements, so any
			// surfaced error is a real bug.
			rep.Err = fmt.Sprintf("op %d id=%d: %v", i, id, err)
			return rep
		}

		if (i+1)%cfg.VerifyEvery == 0 {
			if v := qcVerify(ctx, h.db, model, rng); len(v) > 0 {
				rep.Violations = v
				return rep
			}
		}
	}
	if v := qcVerify(ctx, h.db, model, rng); len(v) > 0 {
		rep.Violations = v
		return rep
	}

	rep.Fingerprint = hunt.Fingerprint(h.simDB)
	return rep
}

// qcHarness owns the SQL-over-SimFDB plumbing for one query-correctness run: a SimFDB-backed
// FDBDatabase registered under a unique cache key, with the (id, cat, val) schema + two secondary
// indexes created. Faults are OFF for the whole run.
type qcHarness struct {
	env    *dst.Env
	simDB  *recordlayer.FDBDatabase
	db     *sql.DB
	closes []func()
}

func qcNewHarness(seed uint64) (*qcHarness, error) {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier() // faults OFF: hunting query answers, not fault idempotency

	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://sqlqueryhunt/%d/%d", seed, qcKeyCounter.Add(1))
	h := &qcHarness{env: env, simDB: simDB}
	h.closes = append(h.closes, sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+qcDBPath+"?cluster_file="+key)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open setup: %w", err)
	}
	setup.SetMaxOpenConns(1)
	h.closes = append(h.closes, func() { setup.Close() })
	for _, stmt := range []string{
		"CREATE DATABASE " + qcDBPath,
		"CREATE SCHEMA TEMPLATE tmpl " +
			"CREATE TABLE t (id BIGINT NOT NULL, cat BIGINT, val BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX idx_cat ON t (cat) " +
			"CREATE INDEX idx_val ON t (val)",
		"CREATE SCHEMA " + qcDBPath + "/s WITH TEMPLATE tmpl",
	} {
		if _, err := setup.ExecContext(context.Background(), stmt); err != nil {
			h.close()
			return nil, fmt.Errorf("setup %q: %w", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+qcDBPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // one conn ⇒ deterministic statement ordering, no pool races
	h.db = db
	h.closes = append(h.closes, func() { db.Close() })
	return h, nil
}

func (h *qcHarness) close() {
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}

// qcVerify runs the read battery and checks each query against the model computed independently in
// Go. Parameters (X, Y, C, K) are drawn from rng so successive verifies probe different thresholds;
// the draws are read-only and never touch persisted state, so determinism is preserved. Returns
// each divergence as a "<query> : store=<g> model=<m>" string (capped), empty when all match.
func qcVerify(ctx context.Context, db *sql.DB, model map[int64]qcRow, rng *rand.Rand) []string {
	x := int64(rng.Int32N(qcValRange))
	y := int64(rng.Int32N(qcValRange))
	c := int64(rng.IntN(qcCatCard))
	k := rng.IntN(6) + 1

	var viol []string
	add := func(f string, a ...any) {
		if len(viol) < 10 {
			viol = append(viol, fmt.Sprintf(f, a...))
		}
	}

	// scalar reads an integer scalar aggregate that is never NULL here (COUNT).
	scalar := func(q string, args ...any) (int64, bool) {
		var v int64
		if err := db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
			add("%s : query error %v", q, err)
			return 0, false
		}
		return v, true
	}
	// nullScalar reads an aggregate that is NULL over the empty input set (SUM/MIN/MAX).
	nullScalar := func(q string, args ...any) (sql.NullInt64, bool) {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
			add("%s : query error %v", q, err)
			return v, false
		}
		return v, true
	}

	// --- 1. COUNT(*) ---
	if g, ok := scalar("SELECT COUNT(*) FROM t"); ok {
		if m := int64(len(model)); g != m {
			add("SELECT COUNT(*) FROM t : store=%d model=%d", g, m)
		}
	}

	// --- 2. COUNT(*) WHERE val > X ---
	if g, ok := scalar("SELECT COUNT(*) FROM t WHERE val > ?", x); ok {
		var m int64
		for _, row := range model {
			if row.val > x {
				m++
			}
		}
		if g != m {
			add("SELECT COUNT(*) WHERE val>%d : store=%d model=%d", x, g, m)
		}
	}

	// --- 3. COUNT(*) WHERE cat = C AND val <= Y ---
	if g, ok := scalar("SELECT COUNT(*) FROM t WHERE cat = ? AND val <= ?", c, y); ok {
		var m int64
		for _, row := range model {
			if row.cat == c && row.val <= y {
				m++
			}
		}
		if g != m {
			add("SELECT COUNT(*) WHERE cat=%d AND val<=%d : store=%d model=%d", c, y, g, m)
		}
	}

	// --- 4. SUM(val) WHERE cat = C (NULL over empty) ---
	if g, ok := nullScalar("SELECT SUM(val) FROM t WHERE cat = ?", c); ok {
		var sum int64
		var any bool
		for _, row := range model {
			if row.cat == c {
				sum += row.val
				any = true
			}
		}
		if !qcNullEq(g, sum, any) {
			add("SELECT SUM(val) WHERE cat=%d : store=%s model=%s", c, qcNullStr(g), qcExpStr(sum, any))
		}
	}

	// --- 5. MIN(val) (NULL over empty) ---
	if g, ok := nullScalar("SELECT MIN(val) FROM t"); ok {
		mn, any := qcMinMax(model, true)
		if !qcNullEq(g, mn, any) {
			add("SELECT MIN(val) : store=%s model=%s", qcNullStr(g), qcExpStr(mn, any))
		}
	}

	// --- 6. MAX(val) (NULL over empty) ---
	if g, ok := nullScalar("SELECT MAX(val) FROM t"); ok {
		mx, any := qcMinMax(model, false)
		if !qcNullEq(g, mx, any) {
			add("SELECT MAX(val) : store=%s model=%s", qcNullStr(g), qcExpStr(mx, any))
		}
	}

	// --- 7. id WHERE cat = C AND val > X ORDER BY id ---
	{
		q := "SELECT id FROM t WHERE cat = ? AND val > ? ORDER BY id"
		gotIDs, qerr := qcQueryIDs(ctx, db, q, c, x)
		if qerr != nil {
			add("%s : query error %v", q, qerr)
		} else {
			var want []int64
			for id, row := range model {
				if row.cat == c && row.val > x {
					want = append(want, id)
				}
			}
			sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
			if !qcEqIDs(gotIDs, want) {
				add("SELECT id WHERE cat=%d AND val>%d ORDER BY id : store=%v model=%v", c, x, gotIDs, want)
			}
		}
	}

	// --- 8. id, val ORDER BY val, id LIMIT K ---
	{
		q := "SELECT id, val FROM t ORDER BY val, id LIMIT ?"
		gotPairs, qerr := qcQueryPairs(ctx, db, q, k)
		if qerr != nil {
			add("%s : query error %v", q, qerr)
		} else {
			want := qcOrderedPairs(model)
			if len(want) > k {
				want = want[:k]
			}
			if !qcEqPairs(gotPairs, want) {
				add("SELECT id,val ORDER BY val,id LIMIT %d : store=%v model=%v", k, gotPairs, want)
			}
		}
	}

	return viol
}

// qcPair is one (id, val) row for ORDER BY comparison.
type qcPair struct {
	id  int64
	val int64
}

// qcMinMax computes MIN (min=true) or MAX (min=false) of val over the model; any=false when empty.
func qcMinMax(model map[int64]qcRow, min bool) (int64, bool) {
	var best int64
	var any bool
	for _, row := range model {
		if !any {
			best = row.val
			any = true
			continue
		}
		if min {
			if row.val < best {
				best = row.val
			}
		} else if row.val > best {
			best = row.val
		}
	}
	return best, any
}

// qcOrderedPairs returns all rows sorted by (val, id) — the SQL ORDER BY val, id order.
func qcOrderedPairs(model map[int64]qcRow) []qcPair {
	out := make([]qcPair, 0, len(model))
	for id, row := range model {
		out = append(out, qcPair{id: id, val: row.val})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].val != out[j].val {
			return out[i].val < out[j].val
		}
		return out[i].id < out[j].id
	})
	return out
}

// qcNullEq compares a store NullInt64 against the model's (value, present) expectation.
func qcNullEq(g sql.NullInt64, val int64, present bool) bool {
	if !present {
		return !g.Valid
	}
	return g.Valid && g.Int64 == val
}

func qcNullStr(g sql.NullInt64) string {
	if !g.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", g.Int64)
}

func qcExpStr(val int64, present bool) string {
	if !present {
		return "NULL"
	}
	return fmt.Sprintf("%d", val)
}

func qcQueryIDs(ctx context.Context, db *sql.DB, q string, args ...any) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func qcQueryPairs(ctx context.Context, db *sql.DB, q string, args ...any) ([]qcPair, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []qcPair
	for rows.Next() {
		var p qcPair
		if err := rows.Scan(&p.id, &p.val); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func qcEqIDs(a, b []int64) bool {
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

func qcEqPairs(a, b []qcPair) bool {
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
