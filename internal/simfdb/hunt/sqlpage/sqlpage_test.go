package sqlpage

import (
	"testing"
)

// TestClean sweeps a seed band: for every seeded dataset, running each query under a tiny execution
// scanned-rows limit (forcing the executor to resume through many internal continuations) must yield
// the identical rows as the unpaginated reference. A failure is a reproducible seed — a continuation
// drop/duplicate in some operator's resume path (the FlatMap-continuation-drop class). SQL runs are
// heavy, so the in-suite band is modest; volume is the hunter's job.
func TestClean(t *testing.T) {
	t.Parallel()
	for _, p := range Profiles() {
		p := p
		w := p.Cfg.Workload.(Workload)
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(0); seed < 15; seed++ {
				res := w.run(seed)
				if res.Report.Failed() {
					t.Fatalf("seed %d: %s", seed, res.Report)
				}
				if res.Checks == 0 {
					t.Fatalf("seed %d: no pagination checks ran (harness or query set is empty)", seed)
				}
			}
		})
	}
}

// TestDeterminism: a run is a pure function of its seed — same fingerprint and same check count twice.
func TestDeterminism(t *testing.T) {
	t.Parallel()
	w := Workload{Rows: 30}
	for seed := uint64(0); seed < 8; seed++ {
		a, b := w.run(seed), w.run(seed)
		if a.Report.Failed() || b.Report.Failed() {
			t.Fatalf("seed %d unexpectedly failed: %s", seed, a.Report)
		}
		if a.Report.Fingerprint != b.Report.Fingerprint {
			t.Fatalf("seed %d nondeterministic fingerprint: %s vs %s", seed, a.Report.Fingerprint, b.Report.Fingerprint)
		}
		if a.Checks != b.Checks {
			t.Fatalf("seed %d nondeterministic check count: %d vs %d", seed, a.Checks, b.Checks)
		}
	}
}
