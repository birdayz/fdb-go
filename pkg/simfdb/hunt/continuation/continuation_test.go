package continuation

import (
	"context"
	"testing"
)

// TestClean sweeps every profile across a wide seed band: for every seeded dataset, paginating each
// scan (record/index, forward/reverse) at every page size — and resuming across a between-page
// delete — must agree with the independent model. A failure is a reproducible seed: a continuation
// bug (token encoding, page boundary, resume skip/dup) or a model-fidelity gap.
func TestClean(t *testing.T) {
	t.Parallel()
	for _, p := range Profiles() {
		p := p
		w := p.Cfg.Workload.(Workload)
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(0); seed < 200; seed++ {
				res := w.run(seed)
				if res.Report.Failed() {
					t.Fatalf("seed %d: %s", seed, res.Report)
				}
			}
		})
	}
}

// TestModelMatchesEngine proves the independent model is a FAITHFUL reference — a single-shot engine
// scan of a hand-known dataset returns exactly the model order for every target. This is what gives
// the driver teeth: because the model is proven correct here, a divergence in TestClean is a real
// engine bug, not a wrong expectation.
func TestModelMatchesEngine(t *testing.T) {
	t.Parallel()
	recs := []rec{{pk: 5, price: 1}, {pk: 2, price: 1}, {pk: 9, price: 0}, {pk: 7, price: 2}, {pk: 1, price: 0}}
	model := buildModel(recs)
	// Sanity-pin the model itself so a refactor of buildModel can't silently drift.
	if got := model["record-fwd"]; !eq(got, []string{"1", "2", "5", "7", "9"}) {
		t.Fatalf("model record-fwd = %v", got)
	}
	if got := model["index-fwd"]; !eq(got, []string{"0:1", "0:9", "1:2", "1:5", "2:7"}) {
		t.Fatalf("model index-fwd = %v", got)
	}

	w := Workload{Records: len(recs), PKDomain: 100}
	db, sub, md, index, err := w.setup(7, recs)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx := context.Background()
	for _, sc := range scanCases(md, index) {
		want := model[sc.key]
		// One-shot scan (page = full length): the engine order must equal the model.
		got, _, err := w.drainWithLimit(ctx, db, md, sub, sc, nil, len(want), len(want))
		if err != nil {
			t.Fatalf("%s scan: %v", sc.key, err)
		}
		if d := diff(want, got); d != "" {
			t.Fatalf("%s: engine scan disagrees with the model — %s", sc.key, d)
		}
	}
}

// TestDiffTeeth proves the ordered comparator discriminates — it flags a reorder, a length change,
// and a substitution, and passes only on an exact match.
func TestDiffTeeth(t *testing.T) {
	t.Parallel()
	base := []string{"1", "2", "3"}
	if d := diff(base, []string{"1", "2", "3"}); d != "" {
		t.Fatalf("diff of equal slices should be empty, got %q", d)
	}
	if diff(base, []string{"1", "3", "2"}) == "" {
		t.Fatal("diff missed a reorder")
	}
	if diff(base, []string{"1", "2"}) == "" {
		t.Fatal("diff missed a length change")
	}
	if diff(base, []string{"1", "2", "4"}) == "" {
		t.Fatal("diff missed a substitution")
	}
	if got := removeRow([]string{"a", "b", "c", "b"}, "b"); !eq(got, []string{"a", "c", "b"}) {
		t.Fatalf("removeRow removed the wrong occurrence: %v", got)
	}
}

// TestDeterminism: a run is a pure function of its seed — same fingerprint and same page count twice.
func TestDeterminism(t *testing.T) {
	t.Parallel()
	w := Workload{Records: 40, PKDomain: 200}
	for seed := uint64(0); seed < 100; seed++ {
		a, b := w.run(seed), w.run(seed)
		if a.Report.Failed() || b.Report.Failed() {
			t.Fatalf("seed %d unexpectedly failed: %s", seed, a.Report)
		}
		if a.Report.Fingerprint != b.Report.Fingerprint {
			t.Fatalf("seed %d nondeterministic fingerprint: %s vs %s", seed, a.Report.Fingerprint, b.Report.Fingerprint)
		}
		if a.Pages != b.Pages {
			t.Fatalf("seed %d nondeterministic page count: %d vs %d", seed, a.Pages, b.Pages)
		}
	}
}

func eq(a, b []string) bool {
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
