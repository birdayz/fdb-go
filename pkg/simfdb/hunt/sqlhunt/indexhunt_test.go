package sqlhunt

import (
	"context"
	"testing"

	"fdb.dev/pkg/simfdb/hunt"
)

func siSmokeCfg() hunt.Config {
	return hunt.Config{Workload: SQLIndexWorkload{}, NumOps: 30, MaxPKs: 12, VerifyEvery: 10, FaultProb: 0.3}
}

// TestSQLIndexWorkloadRunsClean drives the full SQL stack (with a secondary index in the loop)
// over SimFDB under faults for a few seeds and asserts every one is clean — idempotent DML
// (absolute UPDATE SET k=,v= + DELETE) survives commit_unknown/conflict/too_old autocommit retry
// with the secondary index staying consistent with the base table (no stale/missing/orphan
// entries, index-covered WHERE k=K queries always match the model).
func TestSQLIndexWorkloadRunsClean(t *testing.T) {
	t.Parallel()
	cfg := siSmokeCfg()
	for seed := uint64(0); seed < 6; seed++ {
		if rep := hunt.Run(seed, cfg); rep.Failed() {
			t.Fatalf("sql-index seed %d found a bug:\n%s", seed, rep)
		}
	}
}

// TestSQLIndexWorkloadDeterminism pins that the SQL+index persist path is deterministic: the same
// seed yields byte-identical state and the same fault schedule. A mismatch would mean
// nondeterminism (plan-order, map iteration, a clock/rand leak) has crept into the relational
// stack or the index maintenance path.
func TestSQLIndexWorkloadDeterminism(t *testing.T) {
	t.Parallel()
	cfg := siSmokeCfg()
	for _, seed := range []uint64{2, 13, 29} {
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

// TestSQLIndexOracleHasTeeth proves BOTH arms of the secondary-index oracle catch a store/model
// divergence. Arm (a): an untracked value change fires the full-table check. Arm (b): an untracked
// k change (which moves the row across index key-sets) fires the index-covered WHERE k=K check —
// the arm that actually validates secondary-index consistency. A toothless oracle would let a
// clean sweep pass while an index silently drifted, which is the failure mode this guards against.
func TestSQLIndexOracleHasTeeth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := siNewHarness(5, 0) // faults off; we corrupt the model-vs-store agreement by hand
	if err != nil {
		t.Fatalf("siNewHarness: %v", err)
	}
	defer h.close()

	model := map[int64]siRow{}
	for id := int64(0); id < 6; id++ {
		k := id % siKeyDomain
		v := id * 10
		if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, k, v) VALUES (?, ?, ?)", id, k, v); err != nil {
			t.Fatalf("insert id=%d: %v", id, err)
		}
		model[id] = siRow{k: k, v: v}
	}
	if v := siVerify(ctx, h.db, model); len(v) > 0 {
		t.Fatalf("clean state should verify, got: %v", v)
	}

	// Arm (a): untracked v change — full-table check must fire.
	if _, err := h.db.ExecContext(ctx, "UPDATE t SET v = ? WHERE id = ?", int64(777), int64(1)); err != nil {
		t.Fatalf("untracked v update: %v", err)
	}
	if v := siVerify(ctx, h.db, model); len(v) == 0 {
		t.Fatal("oracle arm (a) has no teeth: an untracked value change produced no violation")
	}
	// restore agreement so arm (b) is tested in isolation
	model[1] = siRow{k: model[1].k, v: 777}
	if v := siVerify(ctx, h.db, model); len(v) > 0 {
		t.Fatalf("restore should verify, got: %v", v)
	}

	// Arm (b): move a row across index key-sets in the store only (model keeps old k). Choose a
	// target key different from the row's current k so the index membership genuinely changes.
	oldK := model[0].k
	newK := (oldK + 1) % siKeyDomain
	if _, err := h.db.ExecContext(ctx, "UPDATE t SET k = ? WHERE id = ?", newK, int64(0)); err != nil {
		t.Fatalf("untracked k update: %v", err)
	}
	if v := siVerify(ctx, h.db, model); len(v) == 0 {
		t.Fatal("oracle arm (b) has no teeth: an untracked secondary-index key change produced no violation")
	}
}

// TestSQLIndexProfiles sanity-checks the exported profile set.
func TestSQLIndexProfiles(t *testing.T) {
	t.Parallel()
	ps := SQLIndexProfiles()
	if len(ps) == 0 {
		t.Fatal("no SQL index profiles")
	}
	for _, p := range ps {
		if p.Cfg.Workload == nil || p.Cfg.Workload.Name() != "sql-index" {
			t.Fatalf("profile %s: unexpected workload", p.Name)
		}
	}
}
