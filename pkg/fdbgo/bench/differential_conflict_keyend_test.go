package bench

import (
	"fmt"
	"os"
	"sync"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Single-key conflict END exactness — that AddReadConflictKey(k) / AddWriteConflictKey(k) register
// [k, k\x00) and not [k, strinc(k)).
//
// The client builds that end itself (Go: addReadConflictForKey / addWriteConflictForKeyLocked lay
// down key||key||0x00 and slice the end out of it), so it is a place a port can get wrong, and
// TestDifferential_{Read,Write}ConflictRange CANNOT see the mistake. strinc("…r5") is exactly
// "…r6", so under the wrong construction the range is [r5, r6) and that suite's key_other_r6 probe
// still sits on the EXCLUSIVE end — it does not conflict either way. Verified by mutation: with
// addWriteConflictForKeyLocked ending at strinc(key), all seven TestDifferential_WriteConflictRange
// subtests still PASS. The gap is dimensional — every existing probe sits at or beyond strinc(k),
// and the two constructions only disagree STRICTLY INSIDE (k, strinc(k)).
//
// Probing that interval needs care, because the natural instrument — race a concurrent writer and
// look for not_committed — is not clean here. This package's version-pinned conflict differentials
// take spurious not_committed(1020) at roughly 1% of full-suite runs, on BOTH clients (libfdb_c
// included) and with the client-shipped conflict ranges measured to be exactly correct, so a
// single observed conflict is not evidence of a wrong range. Hence the split:
//
//	CgoGroundTruth — race-free. libfdb_c reports its own registered conflict ranges through
//	                 \xff\xff/transaction/{read,write}_conflict_range/, so the spec side is read
//	                 back directly rather than inferred from a commit outcome.
//	GoBehavioral   — the Go client exposes no such read-back, so its ranges are probed through a
//	                 commit race; every observed conflict is then CONFIRMED on fresh prefixes.
//	                 A wrong end is deterministic (measured 1440/1440 under mutation); the
//	                 environmental conflict is well under 1% and cannot survive the confirmations.

// The reader probes below are chosen so that each one is sensitive to exactly one of the two
// single-key end constructions. Writer registers W on k="r5"; reader registers R on the probe;
// they conflict iff W ∩ R ≠ ∅.
//
//	W_correct = [r5, r5\x00)   W_strinc = [r5, r6)
//	R_correct = [P, P\x00)     R_strinc = [P, strinc(P))
//
//	P = "r5\x00" / "r5a" (above k): disjoint from W_correct and from R_strinc, but INSIDE
//	    W_strinc → fires only if the WRITE end is strinc.
//	P = "r" (a prefix of k): R_correct = [r, r\x00) misses k, while R_strinc = [r, s) CONTAINS
//	    k → fires only if the READ end is strinc.
//
// Both are needed: a probe above k cannot see a widened READ end, because widening R moves it
// further away from k, not toward it. Verified by mutation — the write-side mutation reds only
// the first pair, the read-side mutation only the last.
const keyEndProbeKey = "r5" // the key whose single-key conflict range is registered

var keyEndProbes = []struct {
	probe string
	side  string
}{
	{"r5\x00", "write"}, // == keyAfter(k): the correct EXCLUSIVE end of the writer's range
	{"r5a", "write"},    // strictly between keyAfter(k) and strinc(k)
	{"r", "read"},       // strinc("r")="s" spans k; keyAfter("r")="r\x00" does not
}

// TestConflictKeyEnd_CgoGroundTruth reads libfdb_c's registered conflict ranges back out of the
// special key space and asserts the single-key end is keyAfter(k). This is the spec the Go client
// is held to by TestConflictKeyEnd_GoBehavioral below; pinning it here means a libfdb_c change to
// that construction shows up as this test failing rather than as an unexplained Go "regression".
func TestConflictKeyEnd_CgoGroundTruth(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("crkeyend_gt_%d_", os.Getpid())
	k := pfx + keyEndProbeKey

	cases := []struct {
		name   string
		module string
		add    func(tr cgofdb.Transaction)
	}{
		{"read", "\xff\xff/transaction/read_conflict_range/", func(tr cgofdb.Transaction) {
			_ = tr.AddReadConflictKey(cgofdb.Key(k))
		}},
		{"write", "\xff\xff/transaction/write_conflict_range/", func(tr cgofdb.Transaction) {
			_ = tr.AddWriteConflictKey(cgofdb.Key(k))
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, err := cgoClient.CreateTransaction()
			if err != nil {
				t.Fatalf("cgo create: %v", err)
			}
			defer tr.Cancel()
			if err := tr.Options().SetSpecialKeySpaceRelaxed(); err != nil {
				t.Fatalf("special_key_space_relaxed: %v", err)
			}
			tc.add(tr)

			kvs, err := tr.GetRange(
				cgofdb.KeyRange{Begin: cgofdb.Key(tc.module), End: cgofdb.Key(tc.module + "\xff\xff")},
				cgofdb.RangeOptions{},
			).GetSliceWithError()
			if err != nil {
				t.Fatalf("read %s: %v", tc.module, err)
			}
			// The module lists boundaries: value "1" opens a registered range, "0" closes it.
			var begin, end string
			for _, kv := range kvs {
				bare := string(kv.Key)[len(tc.module):]
				if begin == "" {
					if string(kv.Value) == "1" && bare == k {
						begin = bare
					}
					continue
				}
				end = bare
				break
			}
			if begin != k {
				t.Fatalf("%s conflict range for %q not registered; module reported %d boundaries", tc.name, k, len(kvs))
			}
			if want := k + "\x00"; end != want {
				t.Fatalf("libfdb_c single-key %s conflict range is [%q, %q), want end %q (keyAfter, not strinc=%q) — "+
					"the spec TestConflictKeyEnd_GoBehavioral holds the Go client to has changed",
					tc.name, begin, end, want, pfx+"r6")
			}
		})
	}
}

// TestConflictKeyEnd_GoBehavioral drives the Go client's single-key conflict ranges through a
// commit race and asserts nothing strictly inside (k, strinc(k)) conflicts. Every observed
// conflict is re-run on fresh prefixes before it is believed: a wrong end conflicts on every
// attempt, the package's environmental spurious conflict does not.
func TestConflictKeyEnd_GoBehavioral(t *testing.T) {
	t.Parallel()
	const workers = 6
	iters := 40
	if testing.Short() {
		iters = 5
	}

	type finding struct {
		probe string
		side  string
		pfx   string
	}
	var mu sync.Mutex
	var findings []finding

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				for pi, p := range keyEndProbes {
					pfx := fmt.Sprintf("crkeyend_%d_w%d_%d_p%d_", os.Getpid(), id, i, pi)
					if conflicted, ok := keyEndProbeGo(pfx, p.probe); !ok || !conflicted {
						continue
					}
					// Confirm: a wrong end reproduces on every fresh attempt.
					if !keyEndConfirm(id, i, pi, p.probe) {
						continue
					}
					mu.Lock()
					findings = append(findings, finding{probe: p.probe, side: p.side, pfx: pfx})
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	if len(findings) > 0 {
		f := findings[0]
		t.Fatalf("Go single-key %s conflict range on %q reaches %q — reproduced on %d independent "+
			"confirmation runs. The end must be keyAfter(k)=k\\x00, not strinc(k). (first pfx=%s)",
			f.side, keyEndProbeKey, f.probe, len(findings), f.pfx)
	}
}

// keyEndConfirm re-runs the probe on fresh prefixes. It returns true only if EVERY conclusive
// attempt conflicted, which a deterministic wrong end does and a well-under-1% environmental
// conflict does not (five independent confirmations put that below 1e-10).
func keyEndConfirm(worker, iter, probeIdx int, probe string) bool {
	const confirmations = 5
	got := 0
	for c := 0; c < confirmations*3 && got < confirmations; c++ {
		pfx := fmt.Sprintf("crkeyendc_%d_w%d_%d_p%d_c%d_", os.Getpid(), worker, iter, probeIdx, c)
		conflicted, ok := keyEndProbeGo(pfx, probe)
		if !ok {
			continue // transient — no verdict
		}
		if !conflicted {
			return false
		}
		got++
	}
	return got == confirmations
}

// keyEndProbeGo registers a single-key WRITE conflict on k through the Go client, then has a
// reader pinned before that commit read-conflict `probe` and commit. Set/Clear and the explicit
// AddWriteConflictKey share addWriteConflictForKeyLocked, and the reader exercises
// addReadConflictForKey, so both single-key end constructions are on this path. ok=false means the
// attempt hit a transient error and carries no verdict.
func keyEndProbeGo(pfx, probe string) (conflicted bool, ok bool) {
	rng, err := gofdb.PrefixRange([]byte(pfx))
	if err != nil {
		return false, false
	}
	setup, err := goClient.CreateTransaction()
	if err != nil {
		return false, false
	}
	setup.ClearRange(rng)
	setup.Set(gofdb.Key(pfx+"seed"), []byte("s"))
	if err := setup.Commit().Get(); err != nil {
		setup.Cancel()
		return false, false
	}
	v, err := setup.GetCommittedVersion()
	if err != nil {
		return false, false
	}

	a, err := goClient.CreateTransaction()
	if err != nil {
		return false, false
	}
	defer a.Cancel()
	a.SetReadVersion(v)
	_ = a.AddWriteConflictKey(gofdb.Key(pfx + keyEndProbeKey))
	if fdbErrorCode(a.Commit().Get()) != 0 {
		return false, false
	}

	r, err := goClient.CreateTransaction()
	if err != nil {
		return false, false
	}
	defer r.Cancel()
	r.SetReadVersion(v)
	r.AddReadConflictKey(gofdb.Key(pfx + probe))
	r.Set(gofdb.Key(pfx+"~rsentinel"), []byte("x"))
	switch fdbErrorCode(r.Commit().Get()) {
	case 0:
		return false, true
	case 1020:
		return true, true
	default:
		return false, false
	}
}
