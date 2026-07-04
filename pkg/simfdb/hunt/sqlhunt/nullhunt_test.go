package sqlhunt

import (
	"context"
	"testing"

	"fdb.dev/pkg/simfdb/hunt"
)

func nlSmokeCfg() hunt.Config {
	return hunt.Config{Workload: NullWorkload{}, NumOps: 60, MaxPKs: 16, VerifyEvery: 15, FaultProb: 0}
}

// TestNullWorkloadRunsClean drives the full SQL stack over SimFDB (faults off) for several seeds
// and asserts every one is clean — the NULL-aware oracle (COUNT(col) vs COUNT(*), NULL-skipping
// SUM/MAX/MIN, IS NULL / IS NOT NULL, `a > X` excluding NULL rows) agrees with the store on every
// batch.
func TestNullWorkloadRunsClean(t *testing.T) {
	t.Parallel()
	cfg := nlSmokeCfg()
	for seed := uint64(0); seed < 8; seed++ {
		if rep := hunt.Run(seed, cfg); rep.Failed() {
			t.Fatalf("sql-null seed %d found a bug:\n%s", seed, rep)
		}
	}
}

// TestNullWorkloadDeterminism pins that the NULL workload's persist path is deterministic: the
// same seed yields a byte-identical persisted keyspace (identical fingerprint). A mismatch would
// mean nondeterminism (plan-order, map iteration, a clock/rand leak) has crept into the stack.
func TestNullWorkloadDeterminism(t *testing.T) {
	t.Parallel()
	cfg := nlSmokeCfg()
	for _, seed := range []uint64{1, 9, 42} {
		a := hunt.Run(seed, cfg)
		b := hunt.Run(seed, cfg)
		if a.Failed() || b.Failed() {
			t.Fatalf("seed %d unexpectedly failed:\n%s\n%s", seed, a, b)
		}
		if a.Fingerprint == "" {
			t.Fatalf("seed %d empty fingerprint", seed)
		}
		if a.Fingerprint != b.Fingerprint {
			t.Fatalf("seed %d nondeterministic: fingerprint %s != %s", seed, a.Fingerprint, b.Fingerprint)
		}
	}
}

// TestNullOracleHasTeeth proves the NULL oracle catches store/model divergences on the exact
// three-valued-logic axes it exists to guard. Faults are off; we corrupt the model (or the
// store) by hand and assert a violation fires. A toothless oracle would make the clean sweep
// meaningless.
func TestNullOracleHasTeeth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := nlNewHarness(5)
	if err != nil {
		t.Fatalf("newHarness: %v", err)
	}
	defer h.close()

	// Populate: id 0 has a=NULL, ids 1..4 have a=id*100, b mixed.
	lit := func(v int64) *int64 { return &v }
	rows := []struct {
		id   int64
		a, b *int64
	}{
		{0, nil, lit(10)},
		{1, lit(100), nil},
		{2, lit(200), lit(20)},
		{3, lit(300), nil},
		{4, lit(400), lit(40)},
	}
	model := map[int64]nlRow{}
	for _, r := range rows {
		if _, err := h.db.ExecContext(ctx,
			"INSERT INTO t (id, a, b) VALUES (?, ?, ?)", r.id, nlArg(r.a), nlArg(r.b)); err != nil {
			t.Fatalf("insert id=%d: %v", r.id, err)
		}
		model[r.id] = nlRow{a: r.a, b: r.b}
	}
	if v := nlVerify(ctx, h.db, model); len(v) > 0 {
		t.Fatalf("clean state should verify, got: %v", v)
	}

	// Teeth 1: model claims a's NULL-ness is flipped for id 0 (model says non-NULL where store
	// has NULL). This must trip COUNT(a), SUM/MAX/MIN, and the IS NULL / IS NOT NULL / a>X probes.
	bad1 := map[int64]nlRow{}
	for k, v := range model {
		bad1[k] = v
	}
	bad1[0] = nlRow{a: lit(999), b: model[0].b} // store has a=NULL here
	if v := nlVerify(ctx, h.db, bad1); len(v) == 0 {
		t.Fatal("oracle has no teeth: a flipped NULL→value in the model produced no violation")
	}

	// Teeth 2: an untracked store UPDATE that sets a real value to NULL. The model still has the
	// old value; the oracle must fire (COUNT(a)/aggregates/IS NULL all shift).
	if _, err := h.db.ExecContext(ctx, "UPDATE t SET a = ? WHERE id = ?", nlArg(nil), int64(2)); err != nil {
		t.Fatalf("untracked null update: %v", err)
	}
	if v := nlVerify(ctx, h.db, model); len(v) == 0 {
		t.Fatal("oracle has no teeth: an untracked value→NULL change produced no violation")
	}
}

// TestNullProfilesExported sanity-checks the exported NULL-profile set.
func TestNullProfilesExported(t *testing.T) {
	t.Parallel()
	ps := NullProfiles()
	if len(ps) == 0 {
		t.Fatal("no NULL profiles")
	}
	for _, p := range ps {
		if p.Cfg.Workload == nil || p.Cfg.Workload.Name() != "sql-null" {
			t.Fatalf("profile %s: unexpected workload", p.Name)
		}
		if p.Cfg.FaultProb != 0 {
			t.Fatalf("profile %s: NULL hunt must run faults off, got FaultProb=%v", p.Name, p.Cfg.FaultProb)
		}
	}
}
