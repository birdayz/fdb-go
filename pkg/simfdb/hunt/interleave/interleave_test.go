package interleave

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// TestClean sweeps every profile across a wide seed band and asserts no run ever fails: SimFDB's
// resolver is correct on every interleaving AND the verdict/state oracles agree with it. A failure
// is a reproducible seed — either a real resolver bug or an oracle-fidelity gap, both worth a stop.
func TestClean(t *testing.T) {
	t.Parallel()
	for _, p := range Profiles() {
		p := p
		w := p.Cfg.Workload.(Workload)
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(0); seed < 3000; seed++ {
				res := w.run(seed)
				if res.Report.Failed() {
					t.Fatalf("seed %d: %s", seed, res.Report)
				}
			}
		})
	}
}

// TestConflictsFire is the NO-FAKE-CHECKBOX proof: the pure-RMW profile (all increments, tiny
// keyspace) MUST make SimFDB's resolver abort real transactions with 1020. If not one 1020 fires
// across the whole band, the resolver is an unexercised checkbox — the exact gap this driver
// exists to close. The verdict oracle must have predicted every one of them (Predicted==Conflicts,
// already enforced per-run by TestClean, re-summed here for visibility).
func TestConflictsFire(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 3, Txns: 5, OpsPerTxn: 2, AddPct: 0}
	var conflicts, predicted int
	for seed := uint64(0); seed < 2000; seed++ {
		res := w.run(seed)
		if res.Report.Failed() {
			t.Fatalf("seed %d: %s", seed, res.Report)
		}
		conflicts += res.Conflicts
		predicted += res.Predicted
	}
	if conflicts == 0 {
		t.Fatal("no 1020 fired across 2000 pure-RMW seeds: the SSI resolver is not being exercised")
	}
	if predicted != conflicts {
		t.Fatalf("verdict oracle drift: predicted %d conflicts, SimFDB fired %d", predicted, conflicts)
	}
	t.Logf("pure-RMW: %d real 1020s across 2000 seeds (resolver exercised, oracle in lockstep)", conflicts)
}

// TestAddNeverConflicts is the contrast: atomic adds carry no read conflict, so a pure-add profile
// can never abort — every transaction commits, and the commutative sum still lands exactly. This
// pins the "why atomic ops exist" property the driver is built to demonstrate.
func TestAddNeverConflicts(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 3, Txns: 5, OpsPerTxn: 3, AddPct: 100}
	for seed := uint64(0); seed < 2000; seed++ {
		res := w.run(seed)
		if res.Report.Failed() {
			t.Fatalf("seed %d: %s", seed, res.Report)
		}
		if res.Conflicts != 0 {
			t.Fatalf("seed %d: pure-atomic-add produced %d conflicts, want 0 (adds must not conflict)", seed, res.Conflicts)
		}
	}
}

// TestDeterminism: a run is a pure function of its seed. Two runs of the same seed must produce the
// identical fingerprint, conflict count, and op count — the property every DST tier depends on.
func TestDeterminism(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 3, Txns: 5, OpsPerTxn: 3, AddPct: 40}
	for seed := uint64(0); seed < 500; seed++ {
		a, b := w.run(seed), w.run(seed)
		if a.Report.Failed() || b.Report.Failed() {
			t.Fatalf("seed %d unexpectedly failed", seed)
		}
		if a.Report.Fingerprint != b.Report.Fingerprint {
			t.Fatalf("seed %d nondeterministic fingerprint: %s vs %s", seed, a.Report.Fingerprint, b.Report.Fingerprint)
		}
		if a.Conflicts != b.Conflicts || a.opsRun != b.opsRun {
			t.Fatalf("seed %d nondeterministic metrics: conflicts %d/%d ops %d/%d",
				seed, a.Conflicts, b.Conflicts, a.opsRun, b.opsRun)
		}
	}
}

// TestPredictConflictTeeth proves the verdict oracle's core predicate discriminates — it is not a
// constant that happens to agree with SimFDB. It must return true only for a read overwritten
// strictly after the read version by a committed writer of that key.
func TestPredictConflictTeeth(t *testing.T) {
	t.Parallel()
	log := []committedTxn{{version: 1, writeSet: map[int]bool{0: true}}}
	cases := []struct {
		name string
		ts   *txnState
		want bool
	}{
		{
			"conflict: read key written after read version",
			&txnState{readVer: 0, readSet: map[int]bool{0: true}}, true,
		},
		{
			"no conflict: writer committed at-or-before read version",
			&txnState{readVer: 1, readSet: map[int]bool{0: true}}, false,
		},
		{
			"no conflict: disjoint key",
			&txnState{readVer: 0, readSet: map[int]bool{2: true}}, false,
		},
		{
			"no conflict: write-only (empty read set)",
			&txnState{readVer: 0, readSet: map[int]bool{}}, false,
		},
	}
	for _, c := range cases {
		if got := predictConflict(c.ts, log); got != c.want {
			t.Errorf("%s: predictConflict = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStateOracleTeeth proves checkState flags a divergence: an absent key reads as 0, so against a
// nonzero expected total it must produce a violation (and against expected 0, none). Without this a
// green state oracle could be toothless.
func TestStateOracleTeeth(t *testing.T) {
	t.Parallel()
	db := simfdb.New(hunt.NewSimEnv(1, 0)) // empty keyspace
	defer db.Close()
	key := subspace.FromBytes(tuple.Tuple{"il"}.Pack()).Pack(tuple.Tuple{int64(0)})
	if v := checkState(db, [][]byte{key}, []int64{7}); len(v) == 0 {
		t.Fatal("state oracle has no teeth: absent key (0) did not violate expected 7")
	}
	if v := checkState(db, [][]byte{key}, []int64{0}); len(v) != 0 {
		t.Fatalf("state oracle false positive: absent key (0) violated expected 0: %v", v)
	}
}
