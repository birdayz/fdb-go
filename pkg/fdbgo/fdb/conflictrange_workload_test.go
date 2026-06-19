package fdb_test

// ConflictRange workload — a two-directional read-conflict-range oracle on key-selector getRange.
//
// Go port of FoundationDB's fdbserver/workloads/ConflictRange.actor.cpp @ tag 7.3.75 (the non-RYW
// variant, testReadYourWrites=false — the core read-conflict-range oracle; RFC-125). It rides FDB's own
// workload as an oracle on the read-conflict range Go generates for a getRange(beginSel, endSel, limit,
// reverse): a concurrent writer (tr2) commits between a reader's pinned read version and its commit, and
// the resolver's verdict is checked against ground truth (a fresh re-read on tr4).
//
// THE LOAD-BEARING FACT (RFC-125 §3): Go does NOT resolve a selector range server-side. The fdb facade
// (range_result.go resolveSelector) resolves each non-trivial selector with a separate GetKey round-trip
// (the two TRIVIAL forms — firstGreaterOrEqual = !orEqual,offset 1; and firstGreaterThan = orEqual,offset 1
// → k+\x00 — resolve client-side with NO GetKey and so NO getKey conflict; the random onEqual exercises
// both), then issues getRange over the resolved [begin,end). So Go's read-conflict coverage for a selector
// getRange is the UNION of getKey(beginSel) ∪ getKey(endSel) ∪ getRange conflicts — vs C++'s single
// combined addConflictRange(GetRangeReq). The FDB-C-dev review proved the union ⊇ C++'s range across the
// offset/orEqual/reverse/limit/more cross-product (no under-conflict hole, incl. the trivial-selector
// fast paths that skip GetKey). This test is the empirical proof of that across the random selector space.
//
// THE ORACLE (RFC-125 §4.3), asymmetric and deliberate:
//   - UNDER-conflict — `!foundConflict && resultChanged` → t.Fatalf. The serializability teeth: a
//     concurrent write that changed the visible result MUST have caused a conflict. Robust to Go's
//     conflict-generation architecture (it only asserts "no conflict ⟹ your read was still valid").
//   - OVER-conflict — `foundConflict && !resultChanged && no documented C++ exception` → SAFE for Go
//     (aborted where it could have committed; never a correctness defect, per RFC-121), so it is counted
//     and logged, NOT fatal: Go's getKey-then-range union is architecturally wider than C++'s combined
//     resolution.
//   - ANTI-VACUITY (the teeth must be LOADED): resultChangedCount>0 (the under-conflict check ran with a
//     true antecedent), withConflicts>0, withoutConflicts>0.
//
// This test drives the REAL fdb facade (no mirrored resolveSelector → no drift): GetSliceWithError →
// resolveRange → resolveSelector → tx.GetKey/tx.GetRange is the one production conflict-generation path.
//
// Determinism / flake-freedom: seeded RNG, no timing assertions (the conflict verdict is decided by
// read-version ordering via tr3.SetReadVersion, not the clock). Guard-key isolation (§4.1) bounds every
// selector resolution inside one unique prefix so a parallel test's keys can never leak into a resolution.

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// conflictRangeWorkload holds the (unique-prefix) keyspace geometry. maxKeySpace/maxOffset mirror the
// C++ defaults (100 / 5). Guard counts are maxOffset+1 on each side — see guard-band proof below.
type conflictRangeWorkload struct {
	prefix      string
	maxKeySpace int
	maxOffset   int
}

// Keyspace layout inside the unique prefix (RFC-125 §4.1). The "C"<"D" (0x43<0x44) prefix byte keeps
// floor guards strictly below every data key and ceiling guards/sentinel strictly above, with lexical
// order == numeric order (avoiding the negative-%010d trap where "-000000001" < "-000000006").
//
//	floor guards:  prefix + "C" + %010d(j),  j ∈ [0, maxOffset]              (maxOffset+1 keys)
//	data keys:     prefix + "D" + %010d(i),  i ∈ [0, maxKeySpace)            (~50% present)
//	sentinel:      prefix + "D" + %010d(maxKeySpace)
//	ceiling guards:prefix + "D" + %010d(i),  i ∈ [maxKeySpace, maxKeySpace+maxOffset]  (maxOffset+1 keys)
//
// THE BOUND, PROVEN: a selector (base, orEqual, offset) resolves to the present key at index
// anchorIndex + offset − 1 (index 0 = bottom-most present key; the −1 is the off-by-one). Base keys are
// data positions [0,maxKeySpace-1]; offsets are [-maxOffset, maxOffset-1]. Backward worst case: base=D(0)
// present, !orEqual ⇒ anchorIndex = G (the floor-guard count); offset=-maxOffset ⇒ resolvedIndex =
// G-maxOffset-1, which is ≥0 iff G=maxOffset+1 (lands on floor[0], never below → no readToBegin escape).
// G=maxOffset would give -1 → escape. Forward worst case lands inside the maxOffset+1 ceiling band, never
// past → no readThroughEnd. So every resolution stays in [floor[0], ceil[maxOffset]] ⊂ prefix.
func (w *conflictRangeWorkload) dkey(i int) string { return fmt.Sprintf("%sD%010d", w.prefix, i) }
func (w *conflictRangeWorkload) fkey(j int) string { return fmt.Sprintf("%sC%010d", w.prefix, j) }
func (w *conflictRangeWorkload) dataLow() string   { return w.dkey(0) }
func (w *conflictRangeWorkload) sentinel() string  { return w.dkey(w.maxKeySpace) }

// dummyKey sorts after every data/ceiling key ("Z">"D"), so clearing it makes tr3 non-read-only (a
// write-conflict range) WITHOUT being a guard or a query target (no selector resolves there). The C++
// workload uses clear(%010d(maxKeySpace+1)) for the same purpose; a dedicated post-data key is the clean
// prefixed equivalent that never disturbs a guard.
func (w *conflictRangeWorkload) dummyKey() string { return w.prefix + "Zdummy" }

// val is the (stable, per-slot) value of data key i. tr2 only inserts empty slots / clears filled ones —
// it never overwrites — so a surviving key has the same value in tr1's and tr4's reads; resultChanged is
// driven purely by insert/delete within the read window, and value comparison stays consistent.
func (w *conflictRangeWorkload) val(i int) []byte {
	return []byte(fmt.Sprintf("%sv%010d", w.prefix, i))
}

// setGuards writes all floor + ceiling guards (incl. the sentinel). Called ONCE before the loop: the
// guards live outside [dataLow, sentinel) (the per-iteration reset's clear range) and are never targeted
// by tr2 (data band) or tr3 (the post-data dummy), so they persist untouched across all iterations.
func (w *conflictRangeWorkload) setGuards(tr fdb.WritableTransaction) {
	for j := 0; j <= w.maxOffset; j++ {
		tr.Set(fdb.Key(w.fkey(j)), []byte("floor"))
	}
	for i := w.maxKeySpace; i <= w.maxKeySpace+w.maxOffset; i++ {
		tr.Set(fdb.Key(w.dkey(i)), []byte("ceil"))
	}
}

// reset clears the data band and re-seeds ~50% of data keys (C++ ConflictRange.actor.cpp:104-141, minus
// the guards which we keep persistent). Returns the set of present data-key indices (insertedSet).
func (w *conflictRangeWorkload) reset(tr fdb.Transaction, rng *rand.Rand) map[int]bool {
	tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(w.dataLow()), End: fdb.Key(w.sentinel())})
	inserted := make(map[int]bool)
	for i := 0; i < w.maxKeySpace; i++ {
		if rng.Float64() > 0.5 {
			tr.Set(fdb.Key(w.dkey(i)), w.val(i))
			inserted[i] = true
		}
	}
	return inserted
}

// query runs one selector getRange and materializes it (GetSliceWithError triggers the conflict
// generation: resolveRange → resolveSelector(GetKey for non-trivial) → getRange). The selectors mirror
// C++ KeySelectorRef(myKey, onEqual, offset) exactly.
func (w *conflictRangeWorkload) query(tr fdb.Transaction, q selectorQuery) ([]fdb.KeyValue, error) {
	begin := fdb.KeySelector{Key: fdb.Key(w.dkey(q.keyA)), OrEqual: q.onEqualA, Offset: q.offsetA}
	end := fdb.KeySelector{Key: fdb.Key(w.dkey(q.keyB)), OrEqual: q.onEqualB, Offset: q.offsetB}
	rr := tr.GetRange(fdb.SelectorRange{Begin: begin, End: end},
		fdb.RangeOptions{Limit: q.limit, Reverse: q.reverse})
	return rr.GetSliceWithError()
}

// selectorQuery is one random selector getRange. Bounds mirror C++ deterministicRandom()->randomInt(a,b)
// EXACTLY, whose upper bound is EXCLUSIVE (FDB-C-dev condition): keyA/B ∈ [0,maxKeySpace-1],
// offsetA/B ∈ [-maxOffset, maxOffset-1], limit ∈ [1, maxKeySpace-1].
type selectorQuery struct {
	keyA, keyB         int
	onEqualA, onEqualB bool
	offsetA, offsetB   int
	limit              int
	reverse            bool
}

func (w *conflictRangeWorkload) randomQuery(rng *rand.Rand) selectorQuery {
	return selectorQuery{
		// All bounds mirror C++ randomInt's EXCLUSIVE upper bound (the comment shows the C++ call → the
		// actual inclusive Go range it produces).
		keyA:     rng.Intn(w.maxKeySpace),               // randomInt(0, maxKeySpace) → [0, maxKeySpace-1]
		keyB:     rng.Intn(w.maxKeySpace),               //
		onEqualA: rng.Intn(2) != 0,                      // randomInt(0,2) != 0
		onEqualB: rng.Intn(2) != 0,                      //
		offsetA:  rng.Intn(2*w.maxOffset) - w.maxOffset, // randomInt(-maxOffset, maxOffset) → [-maxOffset, maxOffset-1]
		offsetB:  rng.Intn(2*w.maxOffset) - w.maxOffset, //
		limit:    rng.Intn(w.maxKeySpace-1) + 1,         // randomInt(1, maxKeySpace) → [1, maxKeySpace-1]
		reverse:  rng.Intn(2) != 0,                      // coinflip
	}
}

// codeOf extracts an fdb.Error code from an error (Commit/OnError surface fdb.Error via convertError).
func codeOf(err error) (int, bool) {
	var fe fdb.Error
	if errors.As(err, &fe) {
		return fe.Code, true
	}
	return 0, false
}

const errNotCommitted = 1020 // not_committed (conflict) — flow/error_definitions.h

// kvEqual reports whether two result slices are element-wise equal (key AND value). The C++ ignore-pair
// rule for both-\xff keys (ReadYourWrites.actor.cpp:355-356) is DEAD here — our keys are all
// prefix+"C"/"D"/"Z", never \xff; boundary drift is handled instead by the two-sided guard-band skip
// (touchesGuardBand) before the oracle ever runs.
func kvEqual(a, b []fdb.KeyValue) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if string(a[i].Key) != string(b[i].Key) || string(a[i].Value) != string(b[i].Value) {
			return false
		}
	}
	return true
}

// touchesGuardBand reports whether a result reaches the guard band (smallest < dataLow, i.e. a floor
// guard, or largest >= sentinel, i.e. a ceiling guard). Generalizes C++'s single top sentinel (:286-290)
// to both sides: a result that drifted to the boundary is "can't evaluate" → skip. (Order-independent
// min/max so it works for forward and reverse results.)
func (w *conflictRangeWorkload) touchesGuardBand(kvs []fdb.KeyValue) bool {
	dataLow, sentinel := w.dataLow(), w.sentinel()
	for _, kv := range kvs {
		k := string(kv.Key)
		if k < dataLow || k >= sentinel {
			return true
		}
	}
	return false
}

// conflictExplained ports the four documented conservative-conflict exceptions (C++
// ConflictRange.actor.cpp:273-305): cases where a justified conflict's visible result coincidentally did
// not change. Used only to classify a foundConflict-but-unchanged outcome as expected-vs-unexplained for
// the (non-fatal) over-conflict count — Go over-conflicts are always SAFE.
func (w *conflictRangeWorkload) conflictExplained(q selectorQuery, original []fdb.KeyValue, firstElementIdx, curMinIdx int) bool {
	if len(original) == 0 {
		return true
	}
	// min/max key over the result (order-independent).
	smallest, largest := string(original[0].Key), string(original[0].Key)
	for _, kv := range original {
		k := string(kv.Key)
		if k < smallest {
			smallest = k
		}
		if k > largest {
			largest = k
		}
	}
	// :273-278 — hit limit but the end-side offset reaches into the range.
	if len(original) == q.limit && ((q.offsetB <= 0 && !q.reverse) || (q.offsetA > 1 && q.reverse)) {
		return true
	}
	// :286-290 — results reach the (top) sentinel / server keyspace.
	if largest >= w.sentinel() {
		return true
	}
	// :292-298 — results include the first element and the begin offset is negative.
	if (smallest == w.dkey(firstElementIdx) || smallest == w.dkey(curMinIdx)) && q.offsetA < 0 {
		return true
	}
	// :300-305 — begin>end so the change affects only the end selector, but the limit masks it.
	if (q.keyA > q.keyB || (q.keyA == q.keyB && q.onEqualA && !q.onEqualB)) && len(original) == q.limit {
		return true
	}
	return false
}

func minKeyIdx(set map[int]bool) int {
	m := -1
	for i := range set {
		if m == -1 || i < m {
			m = i
		}
	}
	return m
}

// retryBackoff runs the faithful onError backoff for a retryable error on the given txn. We recreate the
// txns on restart, so we only want the delay; OnError(fe).Get() returns nil for a retryable code (and the
// non-nil/non-retryable path is handled by the caller before calling this).
func retryBackoff(tr fdb.Transaction, code int) {
	_ = tr.OnError(fdb.Error{Code: code}).Get()
}

// runConflictRange is the main loop: it performs `targetEvaluated` non-skipped dances (each a faithful
// tr0/tr1/tr2/tr3/tr4 choreography) and asserts the oracle, then the anti-vacuity floor. Returns the
// tallies for the caller to log.
func (w *conflictRangeWorkload) run(t *testing.T, db fdb.Database, seed int64, targetEvaluated int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))

	// Guards once (persist across iterations). A plain write with no read-version pinning, so it uses the
	// production db.Transact retry helper (the canonical path the rest of package fdb_test uses) — only
	// the tr1/tr2/tr3/tr4 dance needs raw transactions (pinning), which db.Transact cannot express.
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		w.setGuards(tr)
		return nil, nil
	}); err != nil {
		t.Fatalf("commit guards: %v", err)
	}

	var withConflicts, withoutConflicts, resultChangedCount, overConflictUnexplained int
	evaluated := 0
	randomSets := true    // first evaluated dance is insert-mode, matching C++ (randomSets flips to true at loop top, :101)
	const restartCap = 50 // per-evaluated-dance restart bound (retryable errors / empty-query retries)

	// Overall attempt bound: a skip (empty query / boundary drift) does not advance `evaluated`, so cap
	// total attempts to prevent an infinite loop if the keyspace geometry ever stops producing evaluable
	// dances. Generous headroom (skips are rare with ~50% density).
	maxAttempts := targetEvaluated * 20
	for attempts := 0; evaluated < targetEvaluated; attempts++ {
		if attempts >= maxAttempts {
			t.Fatalf("could not complete %d evaluable dances in %d attempts (got %d) — skips dominate, geometry broken",
				targetEvaluated, maxAttempts, evaluated)
		}
		out := w.attempt(t, db, rng, randomSets, restartCap)
		if !out.ran {
			// Skipped (empty query after the retry budget, or original drifted to the guard band). Not an
			// evaluation; retry with the SAME mode (toggle only on evaluated dances, below) so the
			// insert/delete alternation tracks executed dances — matching C++'s per-loop toggle — instead
			// of being skewed by skip parity.
			continue
		}
		evaluated++
		randomSets = !randomSets
		if out.conflicted {
			withConflicts++
			if !out.changed && !out.explained {
				overConflictUnexplained++
			}
		} else {
			withoutConflicts++
		}
		if out.changed {
			resultChangedCount++
		}
	}

	t.Logf("[ConflictRange] evaluated=%d withConflicts=%d withoutConflicts=%d resultChanged=%d overConflictUnexplained=%d",
		evaluated, withConflicts, withoutConflicts, resultChangedCount, overConflictUnexplained)

	// Anti-vacuity (RFC-125 §4.3): the under-conflict teeth must have run with a true antecedent, and
	// both verdicts must occur.
	if resultChangedCount == 0 {
		t.Fatalf("vacuous: no concurrent write ever changed the visible result — the under-conflict oracle never ran with a true antecedent (raise iterations)")
	}
	if withConflicts == 0 {
		t.Fatalf("vacuous: no conflicts detected — the resolver was never exercised in the conflict direction")
	}
	if withoutConflicts == 0 {
		t.Fatalf("vacuous: no non-conflicting commits — the resolver was never exercised in the no-conflict direction")
	}
}

type danceOutcome struct {
	ran        bool // the oracle was evaluated (not skipped)
	conflicted bool // tr3.Commit hit not_committed
	changed    bool // tr4 re-read differs from tr1's original
	explained  bool // a documented C++ conservative-conflict exception applies (only meaningful if conflicted && !changed)
}

// attempt runs ONE dance: reset → pick non-empty query (tr1) → pin tr2/tr3 to one read version → tr2
// writes+commit → tr3 same query+commit (catch not_committed) → tr4 re-read → oracle. Retryable errors
// restart the whole dance (fresh txns, bounded by cap). The UNDER-conflict assertion is a hard t.Fatalf
// in here. Returns ran=false when the query stays empty past the budget or the result drifts to the
// guard band (skip).
func (w *conflictRangeWorkload) attempt(t *testing.T, db fdb.Database, rng *rand.Rand, randomSets bool, maxRestart int) danceOutcome {
	t.Helper()
	for restart := 0; restart < maxRestart; restart++ {
		// --- reset (tr0) ---
		tr0, err := db.CreateTransaction()
		if err != nil {
			t.Fatalf("CreateTransaction(tr0): %v", err)
		}
		inserted := w.reset(tr0, rng)
		if cerr := tr0.Commit().Get(); cerr != nil {
			if code, ok := codeOf(cerr); ok && fdb.IsRetryable(code) {
				retryBackoff(tr0, code)
				continue
			}
			t.Fatalf("commit reset: %v", cerr)
		}
		if len(inserted) == 0 {
			continue // empty keyspace — re-seed
		}
		firstElementIdx := minKeyIdx(inserted)

		// --- pick a non-empty query (tr1, fresh per empty attempt) ---
		var q selectorQuery
		var original []fdb.KeyValue
		gotNonEmpty := false
		for try := 0; try < 64; try++ {
			tr1, err := db.CreateTransaction()
			if err != nil {
				t.Fatalf("CreateTransaction(tr1): %v", err)
			}
			q = w.randomQuery(rng)
			res, qerr := w.query(tr1, q)
			if qerr != nil {
				if code, ok := codeOf(qerr); ok && fdb.IsRetryable(code) {
					continue
				}
				t.Fatalf("tr1 query: %v", qerr)
			}
			if len(res) > 0 {
				original = res
				gotNonEmpty = true
				break
			}
		}
		if !gotNonEmpty {
			return danceOutcome{ran: false} // no non-empty query found — skip
		}
		// Boundary drift → can't evaluate (two-sided guard-band sentinel).
		if w.touchesGuardBand(original) {
			return danceOutcome{ran: false}
		}

		// --- pin tr2 and tr3 to one read version ---
		tr2, err := db.CreateTransaction()
		if err != nil {
			t.Fatalf("CreateTransaction(tr2): %v", err)
		}
		tr3, err := db.CreateTransaction()
		if err != nil {
			t.Fatalf("CreateTransaction(tr3): %v", err)
		}
		rv, rverr := tr2.GetReadVersion().Get()
		if rverr != nil {
			if code, ok := codeOf(rverr); ok && fdb.IsRetryable(code) {
				retryBackoff(tr2, code)
				continue
			}
			t.Fatalf("tr2 GetReadVersion: %v", rverr)
		}
		tr3.SetReadVersion(rv)

		// --- tr2: the concurrent writer (all sets-to-empty OR all clears-of-filled) ---
		w.applyWrites(tr2, rng, inserted, randomSets)
		if cerr := tr2.Commit().Get(); cerr != nil {
			if code, ok := codeOf(cerr); ok && fdb.IsRetryable(code) {
				retryBackoff(tr2, code)
				continue
			}
			t.Fatalf("tr2 commit: %v", cerr)
		}

		// --- tr3 (pinned, sees pre-tr2 data): dummy write + same query + commit ---
		tr3.Clear(fdb.Key(w.dummyKey())) // make tr3 non-read-only so it can conflict
		if _, qerr := w.query(tr3, q); qerr != nil {
			if code, ok := codeOf(qerr); ok && fdb.IsRetryable(code) {
				retryBackoff(tr3, code)
				continue
			}
			t.Fatalf("tr3 query: %v", qerr)
		}
		foundConflict := false
		if cerr := tr3.Commit().Get(); cerr != nil {
			if code, ok := codeOf(cerr); ok && code == errNotCommitted {
				foundConflict = true
			} else if ok && fdb.IsRetryable(code) {
				retryBackoff(tr3, code)
				continue
			} else {
				t.Fatalf("tr3 commit (non-conflict error): %v", cerr)
			}
		}

		// --- tr4 (fresh version, post-tr2): re-read; compute resultChanged ---
		// Retry ONLY the re-read on a transient retryable error (fresh tr4 each try, each at a fresh GRV
		// ≥ tr2's commit) — NOT a whole-dance restart, which would discard the tr3 conflict verdict
		// already obtained for this dance.
		var reread []fdb.KeyValue
		gotReread := false
		for try := 0; try < 64; try++ {
			tr4, err := db.CreateTransaction()
			if err != nil {
				t.Fatalf("CreateTransaction(tr4): %v", err)
			}
			res, qerr := w.query(tr4, q)
			if qerr != nil {
				if code, ok := codeOf(qerr); ok && fdb.IsRetryable(code) {
					continue
				}
				t.Fatalf("tr4 query: %v", qerr)
			}
			reread = res
			gotReread = true
			break
		}
		if !gotReread {
			return danceOutcome{ran: false} // re-read never succeeded — skip (rare)
		}
		// NOTE: we do NOT skip when `reread` drifts into the guard band. The under-conflict oracle MUST
		// run regardless: tr2 is the only writer between tr1's `original` and tr4's `reread`, so any
		// difference (including a resolution that tr2 shifted to now include an inert guard key) is
		// genuinely tr2-caused and therefore conflict-relevant — if tr3 did not conflict on it, that is a
		// real missed conflict. (Skipping reread-drift here was an earlier bug: it silently swallowed the
		// exact serializability violation this test exists to catch. The original-side skip is fine — it
		// only filters which query is picked, before any conflict is adjudicated.)
		changed := !kvEqual(original, reread)

		// --- the oracle (RFC-125 §4.3) ---
		if !foundConflict && changed {
			// UNDER-conflict: tr2 changed the result but tr3 did not conflict → a missed conflict =
			// serializability violation. Hard failure (robust to Go's conflict-generation architecture).
			t.Fatalf("UNDER-CONFLICT (missed serializability conflict): tr3 committed without conflict, "+
				"but the query result changed after tr2.\n  query=%+v randomSets=%v\n  original=%s\n  reread=%s",
				q, randomSets, fmtKVs(original), fmtKVs(reread))
		}
		explained := false
		if foundConflict && !changed {
			curMinIdx := minKeyIdx(inserted) // post-tr2 min (applyWrites mutated `inserted`), matching C++ insertedSet.begin() at :293
			explained = w.conflictExplained(q, original, firstElementIdx, curMinIdx)
			if !explained {
				// SAFE for Go (over-conflict): aborted where it could have committed. Logged, not fatal.
				t.Logf("[ConflictRange] over-conflict (safe, unexplained): query=%+v randomSets=%v size=%d",
					q, randomSets, len(original))
			}
		}
		return danceOutcome{ran: true, conflicted: foundConflict, changed: changed, explained: explained}
	}
	// Exhausted the restart budget on retryable churn — treat as a skip (not a failure; rare).
	return danceOutcome{ran: false}
}

// applyWrites does the tr2 mutation set: randomInt(min,max) ops (here min=2,max=4 per C++ default), each
// either a set to an EMPTY slot (randomSets) or a clear of a FILLED slot, mirroring
// ConflictRange.actor.cpp:192-219. Mutates `inserted` so successive ops pick distinct valid slots.
func (w *conflictRangeWorkload) applyWrites(tr fdb.Transaction, rng *rand.Rand, inserted map[int]bool, randomSets bool) {
	nOps := rng.Intn(3) + 2 // randomInt(minOps=2, maxOps+1=5) → [2,4]
	for n := 0; n < nOps; n++ {
		for j := 0; j < 5; j++ {
			idx := rng.Intn(w.maxKeySpace)
			if randomSets {
				if !inserted[idx] {
					inserted[idx] = true
					tr.Set(fdb.Key(w.dkey(idx)), w.val(idx))
					break
				}
			} else {
				if inserted[idx] {
					delete(inserted, idx)
					tr.Clear(fdb.Key(w.dkey(idx)))
					break
				}
			}
		}
	}
}

func fmtKVs(kvs []fdb.KeyValue) string {
	keys := make([]string, len(kvs))
	for i, kv := range kvs {
		keys[i] = string(kv.Key)
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d keys %v", len(kvs), keys)
}

// TestConflictRange_SelectorGetRange_NoMissedConflict is the workload: across the random selector/offset/
// onEqual/reverse/limit space, every concurrent write that changes the visible result MUST have caused a
// conflict (no under-conflict). Anti-vacuity proves the teeth ran loaded.
func TestConflictRange_SelectorGetRange_NoMissedConflict(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	w := &conflictRangeWorkload{prefix: "crworkload_" + t.Name() + "_", maxKeySpace: 100, maxOffset: 5}
	w.run(t, db, 1, 120)
}

// TestConflictRange_TrivialBeginReverseMore is the FDB-C-dev-requested deterministic regression (RFC-125
// §5.2): the closest-to-a-hole shape — a TRIVIAL firstGreaterOrEqual begin selector (skips GetKey),
// reverse=true, and a limit small enough to force more=true — with a concurrent write inside the returned
// (reverse, limited) window. This is where C++'s rangeBegin = (begin.offset<=1 && more) ? end.getKey() :
// begin.getKey() collapse (ReadYourWrites.actor.cpp:295) and Go's rangeConflictExtent reverse+more clamp
// (transaction.go:1059) must agree. The write must conflict.
func TestConflictRange_TrivialBeginReverseMore(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	w := &conflictRangeWorkload{prefix: "crtrivrev_" + t.Name() + "_", maxKeySpace: 20, maxOffset: 5}

	// Seed a dense, fully-known keyspace: data keys 0..19 all present, plus guards.
	mustCommit(t, db, func(tr fdb.WritableTransaction) {
		w.setGuards(tr)
		for i := 0; i < w.maxKeySpace; i++ {
			tr.Set(fdb.Key(w.dkey(i)), w.val(i))
		}
	})

	// tr3 reads reverse over [D0, D20) with a small limit (forces more=true): returns the TOP `limit`
	// keys in descending order — D19, D18, ... A trivial firstGreaterOrEqual begin (Offset 1, !OrEqual)
	// and firstGreaterOrEqual end. The returned window's lowest key is D(20-limit); a concurrent insert
	// just BELOW that boundary, or a delete inside the window, must conflict the pinned reader.
	const limit = 5
	q := selectorQuery{
		keyA: 0, onEqualA: false, offsetA: 1, // firstGreaterOrEqual(D0) — trivial, skips GetKey
		keyB: w.maxKeySpace, onEqualB: false, offsetB: 1, // firstGreaterOrEqual(D20) = end
		limit: limit, reverse: true,
	}

	// tr3 pinned to a read version BEFORE the concurrent writer commits.
	tr3, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction(tr3): %v", err)
	}
	rv := mustReadVersion(t, db)
	tr3.SetReadVersion(rv)

	// Concurrent writer: delete D19 (the TOP of the reverse window — definitely inside the returned set).
	mustCommit(t, db, func(tr fdb.WritableTransaction) {
		tr.Clear(fdb.Key(w.dkey(w.maxKeySpace - 1)))
	})

	tr3.Clear(fdb.Key(w.dummyKey())) // non-read-only
	if _, qerr := w.query(tr3, q); qerr != nil {
		t.Fatalf("tr3 query: %v", qerr)
	}
	cerr := tr3.Commit().Get()
	if cerr == nil {
		t.Fatalf("UNDER-CONFLICT: tr3 (trivial-begin reverse more, limit=%d) committed despite a concurrent "+
			"delete of D%d inside its reverse window — the read-conflict range missed it", limit, w.maxKeySpace-1)
	}
	if code, ok := codeOf(cerr); !ok || code != errNotCommitted {
		t.Fatalf("tr3 commit: expected not_committed (1020), got %v", cerr)
	}
}

// mustCommit runs fn as a plain write (no read-version pinning) via the production db.Transact retry
// helper — the canonical path the rest of package fdb_test uses. (The tr1/tr2/tr3/tr4 dance can't use
// this: it needs raw transactions for read-version pinning.)
func mustCommit(t *testing.T, db fdb.Database, fn func(fdb.WritableTransaction)) {
	t.Helper()
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		fn(tr)
		return nil, nil
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// mustReadVersion returns a fresh read version (the pin point for a deterministic conflict test).
func mustReadVersion(t *testing.T, db fdb.Database) int64 {
	t.Helper()
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	rv, rverr := tr.GetReadVersion().Get()
	if rverr != nil {
		t.Fatalf("GetReadVersion: %v", rverr)
	}
	return rv
}
