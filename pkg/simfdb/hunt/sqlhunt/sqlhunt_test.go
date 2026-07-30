package sqlhunt

import (
	"context"
	"testing"

	"fdb.dev/pkg/simfdb/hunt"
)

func smokeCfg() hunt.Config {
	return hunt.Config{Workload: SQLWorkload{}, NumOps: 30, MaxPKs: 12, VerifyEvery: 10, FaultProb: 0.3}
}

// TestSQLWorkloadRunsClean drives the full SQL stack over SimFDB under faults for a few seeds
// and asserts every one is clean — idempotent DML (absolute UPDATE + DELETE) survives
// commit_unknown/conflict/too_old autocommit retry with no row/aggregate drift.
func TestSQLWorkloadRunsClean(t *testing.T) {
	t.Parallel()
	cfg := smokeCfg()
	for seed := uint64(0); seed < 4; seed++ {
		if rep := hunt.Run(seed, cfg); rep.Failed() {
			t.Fatalf("sql-dml seed %d found a bug:\n%s", seed, rep)
		}
	}
}

// TestSQLWorkloadDeterminism pins that the SQL persist path is deterministic: the same seed
// yields byte-identical state and the same fault schedule. A mismatch would mean nondeterminism
// (plan-order, map iteration, a clock/rand leak) has crept into the relational stack.
func TestSQLWorkloadDeterminism(t *testing.T) {
	t.Parallel()
	cfg := smokeCfg()
	for _, seed := range []uint64{1, 7, 42} {
		a := hunt.Run(seed, cfg)
		b := hunt.Run(seed, cfg)
		if a.Failed() || b.Failed() {
			t.Fatalf("seed %d unexpectedly failed:\n%s\n%s", seed, a, b)
		}
		if a.Fingerprint != b.Fingerprint {
			t.Fatalf("seed %d nondeterministic: fingerprint %s != %s", seed, a.Fingerprint, b.Fingerprint)
		}
		if a.FaultsFired != b.FaultsFired {
			t.Fatalf("seed %d nondeterministic fault schedule: %d != %d", seed, a.FaultsFired, b.FaultsFired)
		}
	}
}

// TestSQLOracleHasTeeth proves the row-model oracle catches a store/model divergence over the
// SQL stack — otherwise a clean sweep would be meaningless.
func TestSQLOracleHasTeeth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := newHarness(5, 0) // faults off; we corrupt by hand
	if err != nil {
		t.Fatalf("newHarness: %v", err)
	}
	defer h.close()

	model := map[int64]int64{}
	for id := int64(0); id < 5; id++ {
		if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (?, ?)", id, id*10); err != nil {
			t.Fatalf("insert id=%d: %v", id, err)
		}
		model[id] = id * 10
	}
	if v := verify(ctx, h.db, model); len(v) > 0 {
		t.Fatalf("clean state should verify, got: %v", v)
	}

	// Untracked UPDATE: the store changes but the model does not → the oracle must fire.
	if _, err := h.db.ExecContext(ctx, "UPDATE t SET a = ? WHERE id = ?", int64(999), int64(2)); err != nil {
		t.Fatalf("untracked update: %v", err)
	}
	if v := verify(ctx, h.db, model); len(v) == 0 {
		t.Fatal("oracle has no teeth: an untracked row change produced no violation")
	}
}

// TestProfiles sanity-checks the exported profile set.
func TestProfiles(t *testing.T) {
	t.Parallel()
	ps := Profiles()
	if len(ps) == 0 {
		t.Fatal("no SQL profiles")
	}
	for _, p := range ps {
		if p.Cfg.Workload == nil || p.Cfg.Workload.Name() != "sql-dml" {
			t.Fatalf("profile %s: unexpected workload", p.Name)
		}
	}
}
