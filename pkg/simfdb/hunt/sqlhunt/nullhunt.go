// This file adds the "sql-null" workload to the DST bug hunter: a correctness hunt for SQL
// NULL / three-valued-logic semantics over the full relational stack (parser → Cascades planner
// → executor → record layer) on SimFDB.
//
// Unlike the fault-idempotency SQL workload (sqlhunt.go), this one runs with FAULTS OFF — the
// target is *query correctness*, not retry survival, so bare INSERT and every DML shape are fair
// game (their non-idempotency under commit_unknown is irrelevant with no faults). It drives a
// random stream of INSERT (with sometimes-NULL columns) / absolute UPDATE / DELETE, keeps a
// NULL-aware Go row-model in lockstep, and after every batch checks the store against the model
// on the axes where NULL semantics hide bugs: COUNT(col) vs COUNT(*), NULL-skipping aggregates
// (SUM/MAX/MIN), IS NULL / IS NOT NULL, and comparison predicates that must exclude NULL rows by
// three-valued logic.
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

// nlOpStream is the PCG stream for the NULL workload's op choices — distinct from the record
// workload's stream (7), the idempotent-DML SQL workload's stream (11), and the Env's own
// streams, so this workload's op sequence is independent yet reproducible from the one seed.
const nlOpStream = uint64(23)

// nlGtThreshold is the fixed literal used by the `a > X` correctness probe. Fixed (not drawn
// from the workload rng) so the oracle never perturbs the persisted-state determinism, and
// chosen mid-range against the [0, nlValRange) value domain so the predicate splits the rows.
const nlGtThreshold = int64(500)

// nlValRange bounds the non-NULL values inserted into a and b. Small enough that SUM never
// overflows int64 across the keyspace, wide enough that MAX/MIN/`a > X` are non-trivial.
const nlValRange = int32(1000)

// nlDBPath is FIXED across runs (only the cache key varies), so the persisted keyspace for a
// given seed is byte-identical run-to-run and hunt.Fingerprint is a valid determinism probe.
const nlDBPath = "/nulldb"

// nlKeyCounter uniquifies the per-run cache key so concurrent workers never collide on the
// backend registry. It is NOT part of the persisted data (the database path is fixed), so it
// does not perturb determinism.
var nlKeyCounter atomic.Uint64

// NullWorkload is the SQL NULL three-valued-logic correctness workload. Faults are always OFF
// (correctness focus). The zero value is ready to use.
type NullWorkload struct{}

func (NullWorkload) Name() string { return "sql-null" }

// NullProfiles are the NULL-hunt profiles. Correctness-focused, so no fault probability; a
// normal and a dense (few hot keys ⇒ frequent NULL↔value transitions) keyspace.
func NullProfiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "sql-null", Cfg: hunt.Config{Workload: NullWorkload{}, NumOps: 80, MaxPKs: 24, VerifyEvery: 20, FaultProb: 0}},
		{Name: "sql-null-dense", Cfg: hunt.Config{Workload: NullWorkload{}, NumOps: 80, MaxPKs: 8, VerifyEvery: 20, FaultProb: 0}},
	}
}

// nlRow is one modeled row. A nil pointer means SQL NULL for that column.
type nlRow struct {
	a *int64
	b *int64
}

func (NullWorkload) Run(seed uint64, cfg hunt.Config) *hunt.Report {
	rep := &hunt.Report{Seed: seed}
	ctx := context.Background()

	h, err := nlNewHarness(seed)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer h.close()

	rng := rand.New(rand.NewPCG(seed, nlOpStream))
	model := map[int64]nlRow{}

	for i := 0; i < cfg.NumOps; i++ {
		id := rng.Int64N(cfg.MaxPKs)
		_, present := model[id]
		roll := rng.IntN(10)

		var err error
		switch {
		case !present && roll < 6:
			// INSERT with sometimes-NULL columns. Only reached when the id is absent, so it
			// never hits a duplicate-PK error.
			a := nlNullableVal(rng)
			b := nlNullableVal(rng)
			_, err = h.db.ExecContext(ctx,
				"INSERT INTO t (id, a, b) VALUES (?, ?, ?)", id, nlArg(a), nlArg(b))
			if err == nil {
				model[id] = nlRow{a: a, b: b}
			}
		case roll < 8:
			// Absolute UPDATE of both columns (either may be set to NULL). Affects the row only
			// if present, mirrored by the model update below.
			a := nlNullableVal(rng)
			b := nlNullableVal(rng)
			_, err = h.db.ExecContext(ctx,
				"UPDATE t SET a = ?, b = ? WHERE id = ?", nlArg(a), nlArg(b), id)
			if err == nil {
				if _, ok := model[id]; ok {
					model[id] = nlRow{a: a, b: b}
				}
			}
		default:
			// DELETE (no-op if absent).
			_, err = h.db.ExecContext(ctx, "DELETE FROM t WHERE id = ?", id)
			if err == nil {
				delete(model, id)
			}
		}

		rep.Ops = i + 1
		if err != nil {
			// Faults are off and the schema has no UNIQUE constraint, so no DML on an
			// absent/present id has a legitimate domain error — any surfaced error is a real bug.
			rep.Err = fmt.Sprintf("op %d id=%d: %v", i, id, err)
			return rep
		}
		if (i+1)%cfg.VerifyEvery == 0 {
			if v := nlVerify(ctx, h.db, model); len(v) > 0 {
				rep.Violations = v
				return rep
			}
		}
	}
	if v := nlVerify(ctx, h.db, model); len(v) > 0 {
		rep.Violations = v
		return rep
	}

	rep.Fingerprint = hunt.Fingerprint(h.simDB)
	return rep
}

// nlNullableVal draws a nullable value: ~35% NULL (nil), else a pointer into [0, nlValRange).
func nlNullableVal(rng *rand.Rand) *int64 {
	if rng.IntN(100) < 35 {
		return nil
	}
	v := int64(rng.Int32N(nlValRange))
	return &v
}

// nlArg converts a modeled nullable value to a database/sql bind argument. A nil pointer binds
// as SQL NULL (sql.NullInt64 zero value); a non-nil pointer binds its value.
func nlArg(p *int64) any {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// nlHarness owns the SQL-over-SimFDB plumbing for one NULL-hunt run: a SimFDB-backed FDBDatabase
// registered under a unique cache key, with the (id, a, b) schema created. Faults are OFF for the
// whole lifetime — this is a correctness hunt.
type nlHarness struct {
	env    *dst.Env
	simDB  *recordlayer.FDBDatabase
	db     *sql.DB
	closes []func()
}

func nlNewHarness(seed uint64) (*nlHarness, error) {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier() // correctness hunt: no faults, ever

	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://sqlnullhunt/%d/%d", seed, nlKeyCounter.Add(1))
	h := &nlHarness{env: env, simDB: simDB}
	h.closes = append(h.closes, sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+nlDBPath+"?cluster_file="+key)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open setup: %w", err)
	}
	setup.SetMaxOpenConns(1)
	h.closes = append(h.closes, func() { setup.Close() })
	for _, stmt := range []string{
		"CREATE DATABASE " + nlDBPath,
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + nlDBPath + "/s WITH TEMPLATE tmpl",
	} {
		if _, err := setup.ExecContext(context.Background(), stmt); err != nil {
			h.close()
			return nil, fmt.Errorf("setup %q: %w", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+nlDBPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		h.close()
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // one conn ⇒ deterministic statement ordering, no pool races
	h.db = db
	h.closes = append(h.closes, func() { db.Close() })
	return h, nil
}

func (h *nlHarness) close() {
	for i := len(h.closes) - 1; i >= 0; i-- {
		h.closes[i]()
	}
}

// nlVerify is the NULL-aware SQL oracle. It compares the store against the row-model on every
// axis where three-valued logic hides bugs. Returns each divergence as a string (capped), empty
// when every check agrees.
func nlVerify(ctx context.Context, db *sql.DB, model map[int64]nlRow) []string {
	var viol []string
	add := func(f string, a ...any) {
		if len(viol) < 12 {
			viol = append(viol, fmt.Sprintf(f, a...))
		}
	}

	// --- expected values from the NULL-aware model ---
	total := int64(len(model))
	var cntA, cntB int64
	var sumA int64
	haveA := false
	var maxA, minA int64
	var idsNullA, idsNotNullA, idsGtA []int64
	for id, r := range model {
		if r.a != nil {
			cntA++
			sumA += *r.a
			if !haveA {
				maxA, minA, haveA = *r.a, *r.a, true
			} else {
				if *r.a > maxA {
					maxA = *r.a
				}
				if *r.a < minA {
					minA = *r.a
				}
			}
			idsNotNullA = append(idsNotNullA, id)
			if *r.a > nlGtThreshold {
				idsGtA = append(idsGtA, id)
			}
		} else {
			idsNullA = append(idsNullA, id)
		}
		if r.b != nil {
			cntB++
		}
	}
	sortI64(idsNullA)
	sortI64(idsNotNullA)
	sortI64(idsGtA)

	// --- scalar aggregate checks ---
	if got, err := nlScalar(ctx, db, "SELECT COUNT(*) FROM t"); err != nil {
		add("COUNT(*): %v", err)
	} else if !got.Valid || got.Int64 != total {
		add("COUNT(*): store=%s model=%d", nlShow(got), total)
	}
	if got, err := nlScalar(ctx, db, "SELECT COUNT(a) FROM t"); err != nil {
		add("COUNT(a): %v", err)
	} else if !got.Valid || got.Int64 != cntA {
		add("COUNT(a): store=%s model=%d", nlShow(got), cntA)
	}
	if got, err := nlScalar(ctx, db, "SELECT COUNT(b) FROM t"); err != nil {
		add("COUNT(b): %v", err)
	} else if !got.Valid || got.Int64 != cntB {
		add("COUNT(b): store=%s model=%d", nlShow(got), cntB)
	}
	// SUM/MAX/MIN skip NULLs and are NULL when there is no non-NULL a.
	if got, err := nlScalar(ctx, db, "SELECT SUM(a) FROM t"); err != nil {
		add("SUM(a): %v", err)
	} else if !nlMatch(got, sumA, haveA) {
		add("SUM(a): store=%s model=%s", nlShow(got), nlShowExp(sumA, haveA))
	}
	if got, err := nlScalar(ctx, db, "SELECT MAX(a) FROM t"); err != nil {
		add("MAX(a): %v", err)
	} else if !nlMatch(got, maxA, haveA) {
		add("MAX(a): store=%s model=%s", nlShow(got), nlShowExp(maxA, haveA))
	}
	if got, err := nlScalar(ctx, db, "SELECT MIN(a) FROM t"); err != nil {
		add("MIN(a): %v", err)
	} else if !nlMatch(got, minA, haveA) {
		add("MIN(a): store=%s model=%s", nlShow(got), nlShowExp(minA, haveA))
	}

	// --- predicate / three-valued-logic checks ---
	if got, err := nlIDs(ctx, db, "SELECT id FROM t WHERE a IS NULL ORDER BY id"); err != nil {
		add("a IS NULL: %v", err)
	} else if d := nlDiff(got, idsNullA); d != "" {
		add("a IS NULL: %s", d)
	}
	if got, err := nlIDs(ctx, db, "SELECT id FROM t WHERE a IS NOT NULL ORDER BY id"); err != nil {
		add("a IS NOT NULL: %v", err)
	} else if d := nlDiff(got, idsNotNullA); d != "" {
		add("a IS NOT NULL: %s", d)
	}
	// `a > X` must exclude NULL a (NULL > X is UNKNOWN, so the row is not returned).
	if got, err := nlIDs(ctx, db,
		fmt.Sprintf("SELECT id FROM t WHERE a > %d ORDER BY id", nlGtThreshold)); err != nil {
		add("a > %d: %v", nlGtThreshold, err)
	} else if d := nlDiff(got, idsGtA); d != "" {
		add("a > %d: %s", nlGtThreshold, d)
	}

	return viol
}

// nlMatch reports whether a scalar aggregate result matches the expected (value, present) pair,
// where present=false means the model expects SQL NULL.
func nlMatch(got sql.NullInt64, want int64, present bool) bool {
	if !present {
		return !got.Valid
	}
	return got.Valid && got.Int64 == want
}

func nlShow(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", v.Int64)
}

func nlShowExp(v int64, present bool) string {
	if !present {
		return "NULL"
	}
	return fmt.Sprintf("%d", v)
}

// nlScalar runs a single-row, single-column aggregate query and returns it as a NullInt64.
func nlScalar(ctx context.Context, db *sql.DB, q string) (sql.NullInt64, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, q).Scan(&v)
	return v, err
}

// nlIDs runs an id-projection query and returns the ids in row order.
func nlIDs(ctx context.Context, db *sql.DB, q string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q)
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

// nlDiff compares an actual id sequence to the expected sorted sequence and returns a short
// description of the first divergence (element mismatch, length, or non-ascending order), or ""
// when they are identical.
func nlDiff(got, want []int64) string {
	if len(got) != len(want) {
		return fmt.Sprintf("count store=%d model=%d (store=%v model=%v)", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("at %d store=%d model=%d (store=%v model=%v)", i, got[i], want[i], got, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			return fmt.Sprintf("ORDER BY id not ascending at %d (%v)", i, got)
		}
	}
	return ""
}

func sortI64(s []int64) { sort.Slice(s, func(i, j int) bool { return s[i] < s[j] }) }
