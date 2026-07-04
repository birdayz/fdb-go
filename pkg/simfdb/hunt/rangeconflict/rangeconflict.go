// Package rangeconflict is RFC-179 Tier 2's range-conflict interleaving driver.
//
// The interleave driver (pkg/simfdb/hunt/interleave) only makes POINT-width conflict ranges — every
// read and write is a single key, so its conflict range is [k, keyAfter(k)). That leaves SimFDB's
// resolver arithmetic over genuine SPANS untested: rangesOverlap on wide ranges, the GetRange
// read-conflict extent, ClearRange write-conflict ranges, the keyAfter boundary between adjacent
// keys. This driver fills that gap. It interleaves point AND range operations — GetRange (a read
// conflict over a span), ClearRange (a write conflict over a span), point Get/Set/Clear — across
// several open transactions through one deterministic goroutine, and commits through the serialized
// SSI resolver.
//
// The oracle stays airtight because, for keys laid out at ascending tuple-encoded indices, byte-
// range overlap maps EXACTLY onto integer half-open-interval overlap: a point key i is the interval
// [i, i+1); a range [lo, hi) is the interval [lo, hi); and two byte ranges overlap iff their index
// intervals overlap (keyAfter(k_i) sorts strictly between k_i and k_{i+1}, so it never bridges a
// gap). So the model can resolve conflicts over plain integer intervals — a far simpler
// representation than the resolver's byte ranges, and that gap is its teeth.
//
// Two intrinsic oracles, no external reference:
//
//   - VERDICT — independently recompute each commit's SSI verdict from the transaction's read
//     INTERVALS versus the write INTERVALS of transactions that committed after its read version.
//     Catches a missed conflict (a span the resolver failed to detect overlapping) and a spurious
//     abort — specifically over ranges, where keyAfter / rangesOverlap / the GetRange conflict extent
//     could be off by a boundary.
//   - STATE — writes are BLIND (a Set stores a value fixed by (txn, step); a Clear removes a key or
//     span; no read-modify-write), so after retry-draining every abort to a single commit, the final
//     keyspace is the deterministic replay of every committed transaction's writes IN COMMIT ORDER.
//     Catches a lost/late write, a ClearRange that clears too much or too little, a leaked
//     rolled-back write, or a wrong last-writer-in-commit-order result.
//
// Unlimited GetRange reads (no row limit) keep the read-conflict extent the full requested span
// (SimFDB narrows it only for a limit-truncated read), so the interval model matches the resolver
// exactly. Fault-free by design — the resolver is what's under test.
package rangeconflict

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// rcStream is this workload's PCG stream, distinct from the record workload's (7), interleave's
// (11), and continuation's (17).
const rcStream = uint64(19)

// Workload is the range-conflict interleaving driver, a hunt.Workload. Self-parameterized; runs
// fault-free (the resolver is under test).
type Workload struct {
	Keys      int // key domain [0,Keys); small = more overlap = more conflicts (default 8)
	Txns      int // transactions held open and interleaved per run (default 4)
	OpsPerTxn int // steps per transaction program (default 4)
	MaxSpan   int // largest range width a range op may cover (default 4; clamped to Keys)
}

func (Workload) Name() string { return "rangeconflict" }

func (w Workload) Run(seed uint64, _ hunt.Config) *hunt.Report { return w.run(seed).Report }

func (w Workload) withDefaults() Workload {
	if w.Keys <= 0 {
		w.Keys = 8
	}
	if w.Txns <= 0 {
		w.Txns = 4
	}
	if w.OpsPerTxn <= 0 {
		w.OpsPerTxn = 4
	}
	if w.MaxSpan <= 0 {
		w.MaxSpan = 4
	}
	if w.MaxSpan > w.Keys {
		w.MaxSpan = w.Keys
	}
	return w
}

// Result is the driver's rich outcome: the hunt.Report plus resolver-exercise metrics.
type Result struct {
	Report    *hunt.Report
	Conflicts int // phase-1 transactions aborted 1020 (real range-conflict resolution)
	Predicted int // phase-1 transactions the verdict oracle predicted would abort
	RangeOps  int // range (GetRange/ClearRange) operations issued — the span coverage metric
	opsRun    int
}

// interval is a half-open [lo,hi) span in key-index space.
type interval struct{ lo, hi int }

func (a interval) overlaps(b interval) bool { return a.lo < b.hi && b.lo < a.hi }

type opKind int

const (
	opPointGet   opKind = iota // read conflict [i, i+1)
	opRangeGet                 // read conflict [lo, hi)
	opPointSet                 // write [i, i+1); stores val
	opPointClear               // write [i, i+1); removes key i
	opRangeClear               // write [lo, hi); removes keys [lo, hi)
)

func (k opKind) isRead() bool  { return k == opPointGet || k == opRangeGet }
func (k opKind) isRange() bool { return k == opRangeGet || k == opRangeClear }

type step struct {
	kind opKind
	lo   int    // point key, or range lo
	hi   int    // range hi (range ops)
	val  []byte // set value (opPointSet)
}

func (s step) span() interval {
	if s.kind.isRange() {
		return interval{s.lo, s.hi}
	}
	return interval{s.lo, s.lo + 1}
}

type txnState struct {
	id       int
	prog     []step
	tx       fdb.WritableTransaction
	cursor   int
	readVer  int64
	rvPinned bool
	reads    []interval
	writes   []interval
}

type committedTxn struct {
	version int64
	writes  []interval
}

func (w Workload) run(seed uint64) *Result {
	w = w.withDefaults()
	rng := rand.New(rand.NewPCG(seed, rcStream))

	env := hunt.NewSimEnv(seed, 0)
	db := simfdb.New(env)
	defer db.Close()

	sub := subspace.FromBytes(tuple.Tuple{"rc"}.Pack())
	keyBytes := make([][]byte, w.Keys)
	for i := range keyBytes {
		keyBytes[i] = sub.Pack(tuple.Tuple{int64(i)})
	}

	rep := &hunt.Report{Seed: seed}
	res := &Result{Report: rep, RangeOps: 0}

	txns := make([]*txnState, w.Txns)
	for i := range txns {
		txns[i] = &txnState{id: i, prog: w.genProgram(rng, i)}
		tx, err := db.CreateWritableTransaction()
		if err != nil {
			rep.Err = fmt.Sprintf("create txn %d: %v", i, err)
			return res
		}
		txns[i].tx = tx
	}

	var modelVersion int64
	var committed []committedTxn
	var aborted []*txnState
	var commitOrder []int

	// ---- Phase 1: interleave programs + commits ----
	ready := append([]*txnState(nil), txns...)
	for len(ready) > 0 {
		i := rng.IntN(len(ready))
		ts := ready[i]

		if ts.cursor < len(ts.prog) {
			if err := w.execStep(ts, keyBytes, modelVersion, res); err != nil {
				rep.Ops = res.opsRun
				rep.Err = fmt.Sprintf("txn %d step %d: %v", ts.id, ts.cursor, err)
				return res
			}
			ts.cursor++
			res.opsRun++
			continue
		}

		predict := predictConflict(ts, committed)
		if predict {
			res.Predicted++
		}
		conflict, other := classifyCommit(ts.tx.Commit().Get())
		if other != nil {
			rep.Ops = res.opsRun
			rep.Err = fmt.Sprintf("txn %d commit: unexpected error %v", ts.id, other)
			return res
		}
		if conflict != predict {
			rep.Ops = res.opsRun
			rep.Violations = append(rep.Violations, verdictViolation(ts, predict, conflict))
			return res
		}
		if conflict {
			res.Conflicts++
			aborted = append(aborted, ts)
		} else {
			// A read-only transaction commits via the fast path: it takes NO commit version
			// (SimFDB assigns -1, leaving lastVersion unchanged) and logs no writes. Bumping the
			// model version here would drift it ahead of SimFDB's and corrupt every later verdict.
			if len(ts.writes) > 0 {
				modelVersion++
				committed = append(committed, committedTxn{version: modelVersion, writes: ts.writes})
			}
			commitOrder = append(commitOrder, ts.id)
		}
		ready[i] = ready[len(ready)-1]
		ready = ready[:len(ready)-1]
	}

	// ---- Phase 2: serially drain aborts to one commit each ----
	for _, ts := range aborted {
		if v := w.drain(db, ts, keyBytes); v != "" {
			rep.Ops = res.opsRun
			rep.Violations = append(rep.Violations, v)
			return res
		}
		commitOrder = append(commitOrder, ts.id)
	}
	rep.Ops = res.opsRun

	// ---- State oracle: final keyspace == commit-order replay of blind writes ----
	if v := w.checkState(db, keyBytes, txns, commitOrder); len(v) > 0 {
		rep.Violations = append(rep.Violations, v...)
		return res
	}

	rep.Fingerprint = fingerprint(db, keyBytes)
	return res
}

func (w Workload) genProgram(rng *rand.Rand, txid int) []step {
	steps := make([]step, w.OpsPerTxn)
	for j := range steps {
		steps[j] = w.genStep(rng, txid, j)
	}
	return steps
}

// op weights: reads and writes balanced, with a healthy fraction of range ops so spans overlap.
const (
	wPointGet   = 20
	wRangeGet   = 20
	wPointSet   = 25
	wPointClear = 10
	wRangeClear = 20
	wTotal      = wPointGet + wRangeGet + wPointSet + wPointClear + wRangeClear
)

func (w Workload) genStep(rng *rand.Rand, txid, j int) step {
	r := rng.IntN(wTotal)
	switch {
	case r < wPointGet:
		return step{kind: opPointGet, lo: rng.IntN(w.Keys)}
	case r < wPointGet+wRangeGet:
		lo, hi := w.genRange(rng)
		return step{kind: opRangeGet, lo: lo, hi: hi}
	case r < wPointGet+wRangeGet+wPointSet:
		return step{kind: opPointSet, lo: rng.IntN(w.Keys), val: []byte{byte(txid), byte(j)}}
	case r < wPointGet+wRangeGet+wPointSet+wPointClear:
		return step{kind: opPointClear, lo: rng.IntN(w.Keys)}
	default:
		lo, hi := w.genRange(rng)
		return step{kind: opRangeClear, lo: lo, hi: hi}
	}
}

// genRange returns a non-empty [lo,hi) with width in [1,MaxSpan].
func (w Workload) genRange(rng *rand.Rand) (lo, hi int) {
	lo = rng.IntN(w.Keys)
	width := 1 + rng.IntN(w.MaxSpan)
	hi = lo + width
	if hi > w.Keys {
		hi = w.Keys
	}
	if hi <= lo {
		hi = lo + 1
	}
	return lo, hi
}

func (w Workload) execStep(ts *txnState, keyBytes [][]byte, modelVersion int64, res *Result) error {
	s := ts.prog[ts.cursor]
	if s.kind.isRead() {
		if !ts.rvPinned {
			ts.readVer = modelVersion
			ts.rvPinned = true
		}
		ts.reads = append(ts.reads, s.span())
	} else {
		ts.writes = append(ts.writes, s.span())
	}
	if s.kind.isRange() {
		res.RangeOps++
	}
	switch s.kind {
	case opPointGet:
		if _, err := ts.tx.Get(fdb.Key(keyBytes[s.lo])).Get(); err != nil {
			return err
		}
	case opRangeGet:
		// Unlimited read ⇒ the read-conflict extent is the full [lo,hi) span.
		ts.tx.GetRange(rangeOf(keyBytes, s.lo, s.hi), fdb.RangeOptions{}).GetSliceOrPanic()
	case opPointSet:
		ts.tx.Set(fdb.Key(keyBytes[s.lo]), s.val)
	case opPointClear:
		ts.tx.Clear(fdb.Key(keyBytes[s.lo]))
	case opRangeClear:
		ts.tx.ClearRange(rangeOf(keyBytes, s.lo, s.hi))
	}
	return nil
}

// drain re-runs an aborted transaction's program in a fresh, serially-committed transaction. Serial
// ⇒ no concurrent writer ⇒ a correct resolver cannot conflict it; any 1020 is a spurious abort. The
// program's writes are blind and deterministic, so the replayed effect is identical to the original.
func (w Workload) drain(db *simfdb.SimDB, ts *txnState, keyBytes [][]byte) string {
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		return fmt.Sprintf("drain txn %d: create: %v", ts.id, err)
	}
	for _, s := range ts.prog {
		switch s.kind {
		case opPointGet:
			if _, gerr := tx.Get(fdb.Key(keyBytes[s.lo])).Get(); gerr != nil {
				return fmt.Sprintf("drain txn %d: get: %v", ts.id, gerr)
			}
		case opRangeGet:
			tx.GetRange(rangeOf(keyBytes, s.lo, s.hi), fdb.RangeOptions{}).GetSliceOrPanic()
		case opPointSet:
			tx.Set(fdb.Key(keyBytes[s.lo]), s.val)
		case opPointClear:
			tx.Clear(fdb.Key(keyBytes[s.lo]))
		case opRangeClear:
			tx.ClearRange(rangeOf(keyBytes, s.lo, s.hi))
		}
	}
	conflict, other := classifyCommit(tx.Commit().Get())
	if other != nil {
		return fmt.Sprintf("drain txn %d: unexpected commit error %v", ts.id, other)
	}
	if conflict {
		return fmt.Sprintf("txn %d spuriously aborted (1020) during SERIAL drain: no concurrent writer "+
			"exists, so the resolver must not abort it", ts.id)
	}
	return ""
}

// predictConflict is the verdict oracle: a transaction conflicts iff some transaction that committed
// after its read version wrote an interval overlapping one it read.
//
// A transaction with NO writes never reaches conflict resolution — FDB completes a read-only commit
// client-side with no commit-proxy round-trip (SimFDB's read-only fast path, conflict.go), so its
// read conflicts are never checked. A write-only transaction (no reads) has no read conflict ranges,
// so it also can't conflict. Either way: no conflict.
func predictConflict(ts *txnState, committed []committedTxn) bool {
	if len(ts.writes) == 0 || len(ts.reads) == 0 {
		return false
	}
	for _, c := range committed {
		if c.version <= ts.readVer {
			continue
		}
		for _, r := range ts.reads {
			for _, wr := range c.writes {
				if r.overlaps(wr) {
					return true
				}
			}
		}
	}
	return false
}

// checkState replays every committed transaction's blind writes in commit order and compares the
// resulting keyspace to SimFDB's.
func (w Workload) checkState(db *simfdb.SimDB, keyBytes [][]byte, txns []*txnState, commitOrder []int) []string {
	byID := make(map[int]*txnState, len(txns))
	for _, ts := range txns {
		byID[ts.id] = ts
	}
	expected := make(map[int][]byte, w.Keys)
	for _, id := range commitOrder {
		for _, s := range byID[id].prog {
			switch s.kind {
			case opPointSet:
				expected[s.lo] = s.val
			case opPointClear:
				delete(expected, s.lo)
			case opRangeClear:
				for j := s.lo; j < s.hi; j++ {
					delete(expected, j)
				}
			}
		}
	}

	var out []string
	for i := 0; i < w.Keys; i++ {
		got, err := readKey(db, keyBytes[i])
		if err != nil {
			out = append(out, fmt.Sprintf("state oracle: read key %d: %v", i, err))
			continue
		}
		want := expected[i]
		if !bytes.Equal(got, want) {
			out = append(out, fmt.Sprintf("state oracle: key %d = %v, want %v (commit-order replay of "+
				"blind writes diverged — a lost/late write, a ClearRange over/under-clear, or a "+
				"rolled-back-write leak)", i, got, want))
		}
	}
	return out
}

func classifyCommit(err error) (conflict bool, other error) {
	if err == nil {
		return false, nil
	}
	var fe fdb.Error
	if errors.As(err, &fe) && fe.Code == 1020 {
		return true, nil
	}
	return false, err
}

func verdictViolation(ts *txnState, predicted, actual bool) string {
	kind := "MISSED CONFLICT (SimFDB committed a transaction whose read span was overwritten after its " +
		"read version — a range the resolver failed to detect)"
	if !predicted && actual {
		kind = "SPURIOUS ABORT (SimFDB aborted 1020 a transaction with no committed writer overlapping its read spans)"
	}
	return fmt.Sprintf("verdict oracle: txn %d readVer=%d reads=%s: model conflict=%v, SimFDB conflict=%v — %s",
		ts.id, ts.readVer, fmtIntervals(ts.reads), predicted, actual, kind)
}

func rangeOf(keyBytes [][]byte, lo, hi int) fdb.KeyRange {
	begin := fdb.Key(keyBytes[lo])
	var end fdb.Key
	if hi >= len(keyBytes) {
		// Exclusive upper bound just past the last key.
		end = fdb.Key(append(append([]byte(nil), keyBytes[len(keyBytes)-1]...), 0x00))
	} else {
		end = fdb.Key(keyBytes[hi])
	}
	return fdb.KeyRange{Begin: begin, End: end}
}

func readKey(db *simfdb.SimDB, key []byte) ([]byte, error) {
	v, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		b, e := rtx.Get(fdb.Key(key)).Get()
		return b, e
	})
	if err != nil {
		return nil, err
	}
	b, _ := v.([]byte)
	return b, nil
}

func fingerprint(db *simfdb.SimDB, keyBytes [][]byte) string {
	// A compact, deterministic digest of the final keyspace: present/absent + value per key.
	var sb []byte
	for i, k := range keyBytes {
		v, _ := readKey(db, k)
		sb = append(sb, byte(i))
		if v == nil {
			sb = append(sb, 0)
		} else {
			sb = append(sb, 1)
			sb = append(sb, v...)
		}
	}
	return fmt.Sprintf("%x", sb)
}

func fmtIntervals(ivs []interval) string {
	sorted := append([]interval(nil), ivs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].lo != sorted[j].lo {
			return sorted[i].lo < sorted[j].lo
		}
		return sorted[i].hi < sorted[j].hi
	})
	return fmt.Sprintf("%v", sorted)
}

// Profiles is the range-conflict sweep matrix: a balanced set, a hot small domain with wide spans
// (maximum overlap), and a wider domain that leans on range ops. cmd/dst-hunt appends these.
func Profiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "rangeconflict", Cfg: hunt.Config{Workload: Workload{Keys: 8, Txns: 4, OpsPerTxn: 4, MaxSpan: 4}}},
		{Name: "rangeconflict-hot", Cfg: hunt.Config{Workload: Workload{Keys: 4, Txns: 6, OpsPerTxn: 4, MaxSpan: 4}}},
		{Name: "rangeconflict-wide", Cfg: hunt.Config{Workload: Workload{Keys: 12, Txns: 5, OpsPerTxn: 5, MaxSpan: 8}}},
	}
}
