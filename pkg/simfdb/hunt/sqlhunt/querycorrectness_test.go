package sqlhunt

import (
	"context"
	"math/rand/v2"
	"testing"

	"fdb.dev/pkg/simfdb/hunt"
)

// qcTestRng builds a workload-stream PRNG for the teeth test, matching the workload's own seeding
// so the parameter draws are representative.
func qcTestRng(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, qcOpStream)) }

func qcSmokeCfg() hunt.Config {
	// Small but non-trivial: several verify batteries, enough rows per cat for filters/groups to
	// bite, faults OFF (FaultProb stays 0). Fast under `go test`.
	return hunt.Config{Workload: QueryCorrectnessWorkload{}, NumOps: 40, MaxPKs: 16, VerifyEvery: 10}
}

// TestQCWorkloadRunsClean drives the full read path (parser → Cascades planner → executor → record
// layer, over SimFDB) for several seeds and asserts every one is clean — every COUNT/SUM/MIN/MAX,
// filtered ORDER BY, and ORDER BY … LIMIT matches the independently-computed Go model.
func TestQCWorkloadRunsClean(t *testing.T) {
	t.Parallel()
	cfg := qcSmokeCfg()
	for seed := uint64(0); seed < 6; seed++ {
		if rep := hunt.Run(seed, cfg); rep.Failed() {
			t.Fatalf("sql-query seed %d found a bug:\n%s", seed, rep)
		}
	}
}

// TestQCWorkloadDeterminism pins that the query-correctness persist path is deterministic: the same
// seed yields a byte-identical persisted keyspace (fingerprint). A mismatch would mean nondeterminism
// (plan-order, map iteration, a clock/rand leak) crept into the relational stack.
func TestQCWorkloadDeterminism(t *testing.T) {
	t.Parallel()
	cfg := qcSmokeCfg()
	for _, seed := range []uint64{2, 13, 29} {
		a := hunt.Run(seed, cfg)
		b := hunt.Run(seed, cfg)
		if a.Failed() || b.Failed() {
			t.Fatalf("seed %d unexpectedly failed:\n%s\n%s", seed, a, b)
		}
		if a.Fingerprint == "" {
			t.Fatalf("seed %d: empty fingerprint on a clean run", seed)
		}
		if a.Fingerprint != b.Fingerprint {
			t.Fatalf("seed %d nondeterministic: fingerprint %s != %s", seed, a.Fingerprint, b.Fingerprint)
		}
	}
}

// TestQCOracleHasTeeth proves the read-battery oracle catches a store/model divergence on EVERY
// dimension it checks (count, filtered count, SUM, MIN, MAX, filtered ORDER BY, ORDER BY … LIMIT).
// It builds a real table, then perturbs ONLY the model (not the store) in a way that must trip each
// specific check — a toothless oracle would let a clean sweep mean nothing.
func TestQCOracleHasTeeth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := qcNewHarness(101) // faults off; we corrupt the model by hand
	if err != nil {
		t.Fatalf("qcNewHarness: %v", err)
	}
	defer h.close()

	// A fixed, known table: cats {0,1,2,3}, distinct vals, several rows per cat.
	base := map[int64]qcRow{
		0: {cat: 0, val: 100},
		1: {cat: 0, val: 200},
		2: {cat: 1, val: 150},
		3: {cat: 1, val: 50},
		4: {cat: 2, val: 300},
		5: {cat: 3, val: 250},
	}
	for id, row := range base {
		if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, cat, val) VALUES (?, ?, ?)", id, row.cat, row.val); err != nil {
			t.Fatalf("insert id=%d: %v", id, err)
		}
	}

	// clone makes a fresh copy of base so each sub-case perturbs an independent model.
	clone := func() map[int64]qcRow {
		m := make(map[int64]qcRow, len(base))
		for id, row := range base {
			m[id] = row
		}
		return m
	}

	// Baseline: the true model verifies clean.
	if v := qcVerify(ctx, h.db, base, qcTestRng(101)); len(v) > 0 {
		t.Fatalf("clean model should verify, got: %v", v)
	}

	// Each perturbation must produce at least one violation. We scan over many rng draws so the
	// teeth check does not depend on a specific parameter draw hitting the corrupted dimension.
	assertFires := func(name string, corrupt func(map[int64]qcRow)) {
		t.Helper()
		fired := false
		for s := uint64(0); s < 40 && !fired; s++ {
			m := clone()
			corrupt(m)
			if v := qcVerify(ctx, h.db, m, qcTestRng(s)); len(v) > 0 {
				fired = true
			}
		}
		if !fired {
			t.Fatalf("oracle has no teeth for %q: corrupted model produced no violation over 40 draws", name)
		}
	}

	// extra row in the model (not in the store): trips COUNT(*), and can trip filtered/ordered checks.
	assertFires("extra-row", func(m map[int64]qcRow) { m[99] = qcRow{cat: 1, val: 175} })
	// missing row: trips COUNT(*) and ordered checks.
	assertFires("missing-row", func(m map[int64]qcRow) { delete(m, 4) })
	// wrong val: trips SUM/MIN/MAX and ORDER BY … LIMIT.
	assertFires("wrong-val", func(m map[int64]qcRow) { r := m[4]; r.val = 1; /* was max 300 */ m[4] = r })
	// wrong cat: trips the cat-filtered COUNT and SUM.
	assertFires("wrong-cat", func(m map[int64]qcRow) { r := m[0]; r.cat = 3; m[0] = r })
	// bump the min downward via a new smallest row present only in the model: trips MIN + ORDER BY.
	assertFires("phantom-min", func(m map[int64]qcRow) { m[98] = qcRow{cat: 2, val: 0} })
}

// TestQCProfiles sanity-checks the exported query profiles: all faults-off, all the sql-query workload.
func TestQCProfiles(t *testing.T) {
	t.Parallel()
	ps := QueryProfiles()
	if len(ps) == 0 {
		t.Fatal("no query profiles")
	}
	for _, p := range ps {
		if p.Cfg.Workload == nil || p.Cfg.Workload.Name() != "sql-query" {
			t.Fatalf("profile %s: unexpected workload", p.Name)
		}
		if p.Cfg.FaultProb != 0 {
			t.Fatalf("profile %s: query-correctness must run faults OFF, got FaultProb=%v", p.Name, p.Cfg.FaultProb)
		}
	}
}
