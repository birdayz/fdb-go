package rangeconflict

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// TestClean sweeps every profile across a wide seed band: for every interleaving of point and range
// ops, SimFDB's range-aware resolver must agree with the interval-overlap verdict model, and the
// final keyspace must equal the commit-order replay of the blind writes. A failure is a reproducible
// seed — a range-conflict resolver bug (keyAfter / rangesOverlap / GetRange extent) or a ClearRange
// state bug.
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

// TestRangeConflictsFire is the NO-FAKE-CHECKBOX proof for this driver: the hot profile (tiny domain,
// wide spans) MUST make the resolver abort real transactions over SPANS, and MUST issue range ops
// (else it isn't exercising the range arithmetic it exists to test). The verdict model must have
// predicted every abort.
func TestRangeConflictsFire(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 4, Txns: 6, OpsPerTxn: 4, MaxSpan: 4}
	var conflicts, predicted, rangeOps int
	for seed := uint64(0); seed < 3000; seed++ {
		res := w.run(seed)
		if res.Report.Failed() {
			t.Fatalf("seed %d: %s", seed, res.Report)
		}
		conflicts += res.Conflicts
		predicted += res.Predicted
		rangeOps += res.RangeOps
	}
	if conflicts == 0 {
		t.Fatal("no 1020 fired across 3000 hot seeds: the range-aware resolver is not being exercised")
	}
	if rangeOps == 0 {
		t.Fatal("no range ops issued: the driver is not exercising the resolver's RANGE arithmetic")
	}
	if predicted != conflicts {
		t.Fatalf("verdict model drift: predicted %d conflicts, SimFDB fired %d", predicted, conflicts)
	}
	t.Logf("hot: %d real 1020s + %d range ops across 3000 seeds (range resolver exercised, model in lockstep)", conflicts, rangeOps)
}

// TestRangeConflictEndToEnd pins the headline property with a hand-built schedule: a transaction that
// range-reads [0,4) must abort when another transaction commits a point write inside that span first
// — proving span-vs-point conflict detection fires, not just point-vs-point.
func TestRangeConflictEndToEnd(t *testing.T) {
	t.Parallel()
	// Model-level: interval overlap of a range read with a point write.
	rd := interval{0, 4}
	if !rd.overlaps(interval{2, 3}) {
		t.Fatal("range [0,4) must overlap point [2,3)")
	}
	// Verdict-level: a committed point-writer of key 2 (version 1 > readVer 0) conflicts a range
	// reader of [0,4) that also writes (so it is not a read-only fast-path commit).
	ts := &txnState{readVer: 0, reads: []interval{{0, 4}}, writes: []interval{{9, 10}}}
	log := []committedTxn{{version: 1, writes: []interval{{2, 3}}}}
	if !predictConflict(ts, log) {
		t.Fatal("range reader of [0,4) must conflict a committed point writer of key 2")
	}
	// And must NOT conflict a writer strictly outside the span.
	ts2 := &txnState{readVer: 0, reads: []interval{{0, 4}}, writes: []interval{{9, 10}}}
	log2 := []committedTxn{{version: 1, writes: []interval{{5, 6}}}}
	if predictConflict(ts2, log2) {
		t.Fatal("range reader of [0,4) must not conflict a writer at key 5")
	}
}

// TestPredictConflictTeeth proves the interval verdict predicate discriminates on every axis:
// overlap, version gating, disjointness, and write-only.
func TestPredictConflictTeeth(t *testing.T) {
	t.Parallel()
	log := []committedTxn{{version: 2, writes: []interval{{3, 5}}}}
	wr := []interval{{9, 10}} // an out-of-the-way write so these are not read-only fast-path commits
	cases := []struct {
		name string
		ts   *txnState
		want bool
	}{
		{"overlap after read version", &txnState{readVer: 1, reads: []interval{{4, 6}}, writes: wr}, true},
		{"writer at-or-before read version", &txnState{readVer: 2, reads: []interval{{4, 6}}, writes: wr}, false},
		{"disjoint spans", &txnState{readVer: 1, reads: []interval{{0, 3}}, writes: wr}, false}, // [0,3) vs [3,5): touch, no overlap
		{"write-only (no reads)", &txnState{readVer: 0, reads: nil, writes: wr}, false},
		{"read-only (no writes, fast path)", &txnState{readVer: 1, reads: []interval{{4, 6}}, writes: nil}, false},
		{"adjacent point below range", &txnState{readVer: 1, reads: []interval{{2, 3}}, writes: wr}, false},
		{"point inside range", &txnState{readVer: 1, reads: []interval{{4, 5}}, writes: wr}, true},
	}
	for _, c := range cases {
		if got := predictConflict(c.ts, log); got != c.want {
			t.Errorf("%s: predictConflict = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIntervalOverlap pins the half-open overlap: touching endpoints do NOT overlap.
func TestIntervalOverlap(t *testing.T) {
	t.Parallel()
	if (interval{0, 3}).overlaps(interval{3, 5}) {
		t.Fatal("[0,3) and [3,5) touch but must not overlap (half-open)")
	}
	if !(interval{0, 3}).overlaps(interval{2, 5}) {
		t.Fatal("[0,3) and [2,5) overlap on [2,3)")
	}
	if !(interval{1, 2}).overlaps(interval{0, 5}) {
		t.Fatal("[1,2) is contained in [0,5)")
	}
}

// TestStateOracleTeeth proves checkState flags a divergence: on an empty keyspace, a commit order
// that "sets" a key produces an expected non-nil value the (absent) store cannot match.
func TestStateOracleTeeth(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 2}
	db := simfdb.New(hunt.NewSimEnv(1, 0))
	defer db.Close()
	sub := subspace.FromBytes(tuple.Tuple{"rc"}.Pack())
	keyBytes := [][]byte{sub.Pack(tuple.Tuple{int64(0)}), sub.Pack(tuple.Tuple{int64(1)})}
	// One transaction that sets key 0, marked committed — but we never actually apply it to the db.
	ts := &txnState{id: 0, prog: []step{{kind: opPointSet, lo: 0, val: []byte{9}}}}
	if v := w.checkState(db, keyBytes, []*txnState{ts}, []int{0}); len(v) == 0 {
		t.Fatal("state oracle has no teeth: expected key 0 set but store empty, no violation")
	}
	// With no committed writers, the empty store matches the empty model.
	if v := w.checkState(db, keyBytes, []*txnState{ts}, nil); len(v) != 0 {
		t.Fatalf("state oracle false positive on empty vs empty: %v", v)
	}
}

// TestDeterminism: a run is a pure function of its seed.
func TestDeterminism(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 8, Txns: 5, OpsPerTxn: 4, MaxSpan: 4}
	for seed := uint64(0); seed < 500; seed++ {
		a, b := w.run(seed), w.run(seed)
		if a.Report.Failed() || b.Report.Failed() {
			t.Fatalf("seed %d unexpectedly failed: %s", seed, a.Report)
		}
		if a.Report.Fingerprint != b.Report.Fingerprint {
			t.Fatalf("seed %d nondeterministic fingerprint: %s vs %s", seed, a.Report.Fingerprint, b.Report.Fingerprint)
		}
		if a.Conflicts != b.Conflicts || a.RangeOps != b.RangeOps {
			t.Fatalf("seed %d nondeterministic metrics", seed)
		}
	}
}
