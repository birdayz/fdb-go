// Package interleave is RFC-179 Tier 2's concurrent-open-transaction interleaving driver.
//
// Every hunt workload before it is single-writer — each transaction runs and commits alone — so
// SimFDB's serializable-snapshot-isolation resolver never had to resolve an actual conflict: not
// one real not_committed(1020) ever fired. Tier 1's conflict resolution was an *unexercised
// checkbox*. This driver fills that gap. It holds several transactions open at once, interleaves
// their reads/writes/atomic-adds through a single deterministic goroutine, and commits them
// through SimFDB's serialized resolver — which is exactly what makes 1020 fire.
//
// The workload is two contrasting operations on a small hot keyspace:
//
//   - RMW-increment  v := Get(k); Set(k, v+1)   reads k (a read conflict) then writes it. Two of
//     these racing on the same key is the classic lost update — and under SSI the loser does not
//     lose its write silently, it ABORTS with 1020.
//   - atomic-add     Add(k, delta)              writes k with no read conflict. Atomic adds never
//     conflict with each other, so they commute and always commit — the FDB idiom that exists
//     precisely to avoid the RMW abort. Running the two side by side exercises both paths.
//
// Two intrinsic oracles judge each run, neither needing an external reference:
//
//   - VERDICT oracle — independently recompute the SSI verdict for every commit from point-key
//     read/write sets and a model version counter kept in lockstep with SimFDB's, and compare to
//     SimFDB's actual verdict. A disagreement is either a MISSED CONFLICT (SimFDB committed a
//     transaction whose read was overwritten after its read version — a lost update) or a SPURIOUS
//     ABORT (SimFDB aborted a transaction with no conflicting committed writer). The model resolves
//     at the point-key level while SimFDB resolves over byte-ranges; that representation gap is what
//     gives the oracle teeth against the range arithmetic (keyAfter / rangesOverlap / conflict
//     extents).
//   - STATE oracle — retry every aborted transaction to a single successful commit (true rollback
//     discarded the aborted attempt, so each program applies exactly once), then assert the final
//     keyspace equals the seed-derived sum of every program's effect (each increment +1, each add
//     +delta). This is fully independent of SimFDB's storage: it catches a lost update, a
//     rolled-back-write LEAK (a rollback that failed to discard a mutation), and any atomic-add
//     miscount.
//
// v1 runs fault-free (no injected commit faults): the resolver itself is under test, and commit-
// fault injection — already the record workload's job — would confound the serializability
// oracles by making an aborted-but-applied (1021) attempt land. A fault-enabled variant that
// teaches the state oracle about post-apply commit_unknown is a follow-up.
package interleave

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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

// ilStream is the PCG stream for this workload's program generation + scheduler, kept distinct
// from the record workload's opStream (7) so the two never draw the same sequence off one seed.
const ilStream = uint64(11)

// Workload is the concurrent-open-transaction interleaving driver, a hunt.Workload. Its knobs are
// self-contained (it ignores the record-oriented hunt.Config) and it runs fault-free by design —
// see the package doc.
type Workload struct {
	Keys      int // counter keyspace size; smaller = more contention = more conflicts (default 4)
	Txns      int // transactions held open and interleaved per run (default 4)
	OpsPerTxn int // steps in each transaction's program (default 3)
	AddPct    int // percent of steps that are atomic-add vs RMW-increment, in [0,100] (default 40)
}

func (Workload) Name() string { return "interleave" }

// Run adapts the driver to the hunt.Workload interface. cfg is ignored: the interleave workload is
// parameterized entirely by its own fields and deliberately runs without injected commit faults.
func (w Workload) Run(seed uint64, _ hunt.Config) *hunt.Report { return w.run(seed).Report }

func (w Workload) withDefaults() Workload {
	if w.Keys <= 0 {
		w.Keys = 4
	}
	if w.Txns <= 0 {
		w.Txns = 4
	}
	if w.OpsPerTxn <= 0 {
		w.OpsPerTxn = 3
	}
	if w.AddPct < 0 {
		w.AddPct = 0
	}
	if w.AddPct > 100 {
		w.AddPct = 100
	}
	return w
}

// Result is the driver's rich outcome: the standard hunt.Report (violations / error / fingerprint)
// plus resolver-exercise metrics the tests assert on (Conflicts must be > 0 for the RMW profile —
// the NO-FAKE-CHECKBOX proof that 1020 truly fires).
type Result struct {
	Report    *hunt.Report
	Conflicts int // phase-1 transactions SimFDB aborted with 1020 (real resolver activity)
	Predicted int // phase-1 transactions the verdict oracle predicted would abort
	Txns      int
	opsRun    int
}

// opKind is one program step.
type opKind int

const (
	opInc opKind = iota // v := Get(k); Set(k, v+1)
	opAdd               // Add(k, delta)
)

type step struct {
	kind  opKind
	key   int   // key index into keyBytes
	delta int64 // atomic-add amount (opAdd only)
}

// txnState is one open transaction's plumbing plus the model bookkeeping the verdict oracle needs:
// the read version pinned at the first read, and the point-key read/write sets.
type txnState struct {
	id       int
	prog     []step
	tx       fdb.WritableTransaction
	cursor   int // index of the next program step to execute
	readVer  int64
	rvPinned bool
	readSet  map[int]bool
	writeSet map[int]bool
}

// committedTxn is a model record of a successfully-committed phase-1 transaction: its assigned
// commit version and the keys it wrote — the resolver's recentWrites, at the point-key level.
type committedTxn struct {
	version  int64
	writeSet map[int]bool
}

func (w Workload) run(seed uint64) *Result {
	w = w.withDefaults()
	rng := rand.New(rand.NewPCG(seed, ilStream))

	env := hunt.NewSimEnv(seed, 0) // faultProb 0 ⇒ DisabledBuggifier: clean SSI, resolver under test
	db := simfdb.New(env)
	defer db.Close()

	sub := subspace.FromBytes(tuple.Tuple{"il"}.Pack())
	keyBytes := make([][]byte, w.Keys)
	for i := range keyBytes {
		keyBytes[i] = sub.Pack(tuple.Tuple{int64(i)})
	}

	rep := &hunt.Report{Seed: seed}
	res := &Result{Report: rep, Txns: w.Txns}

	// Generate each transaction's program and open its transaction.
	txns := make([]*txnState, w.Txns)
	for i := range txns {
		steps := make([]step, w.OpsPerTxn)
		for j := range steps {
			k := rng.IntN(w.Keys)
			if rng.IntN(100) < w.AddPct {
				steps[j] = step{kind: opAdd, key: k, delta: int64(1 + rng.IntN(9))}
			} else {
				steps[j] = step{kind: opInc, key: k}
			}
		}
		tx, err := db.CreateWritableTransaction()
		if err != nil {
			rep.Err = fmt.Sprintf("create txn %d: %v", i, err)
			return res
		}
		txns[i] = &txnState{id: i, prog: steps, tx: tx, readSet: map[int]bool{}, writeSet: map[int]bool{}}
	}

	var modelVersion int64
	var committed []committedTxn
	var aborted []*txnState

	// ---- Phase 1: interleave every transaction's program and its commit ----
	// Repeatedly pick a random still-active transaction and either run its next op or (if its
	// program is exhausted) commit it. Interleaving the commit point too — a transaction may hold
	// its writes open while others proceed, then commit late — maximizes the conflict surface.
	ready := append([]*txnState(nil), txns...)
	for len(ready) > 0 {
		i := rng.IntN(len(ready))
		ts := ready[i]

		if ts.cursor < len(ts.prog) {
			if err := w.execStep(ts, keyBytes, modelVersion); err != nil {
				rep.Ops = res.opsRun
				rep.Err = fmt.Sprintf("txn %d step %d: %v", ts.id, ts.cursor, err)
				return res
			}
			ts.cursor++
			res.opsRun++
			continue
		}

		// Program exhausted: predict the verdict, then commit and compare.
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
			modelVersion++
			committed = append(committed, committedTxn{version: modelVersion, writeSet: copySet(ts.writeSet)})
		}
		ready[i] = ready[len(ready)-1] // remove ts (swap-with-last; deterministic under fixed draws)
		ready = ready[:len(ready)-1]
	}

	// ---- Phase 2: serially drain every aborted transaction to one successful commit ----
	for _, ts := range aborted {
		if v := w.drain(db, ts, keyBytes); v != "" {
			rep.Ops = res.opsRun
			rep.Violations = append(rep.Violations, v)
			return res
		}
	}
	rep.Ops = res.opsRun

	// ---- State oracle: final keyspace == seed-derived sum of every program's effect ----
	expected := make([]int64, w.Keys)
	for _, ts := range txns {
		for _, s := range ts.prog {
			if s.kind == opAdd {
				expected[s.key] += s.delta
			} else {
				expected[s.key]++
			}
		}
	}
	if v := checkState(db, keyBytes, expected); len(v) > 0 {
		rep.Violations = append(rep.Violations, v...)
		return res
	}

	rep.Fingerprint = fingerprint(db)
	return res
}

// execStep runs one program step against the transaction and updates the model. The first read
// (an increment's Get) pins the model read version, mirroring SimFDB's lazy read-version pin at
// the first Get; modelVersion is kept equal to SimFDB's lastVersion by the single-goroutine loop.
func (w Workload) execStep(ts *txnState, keyBytes [][]byte, modelVersion int64) error {
	s := ts.prog[ts.cursor]
	key := fdb.Key(keyBytes[s.key])
	switch s.kind {
	case opInc:
		if !ts.rvPinned {
			ts.readVer = modelVersion
			ts.rvPinned = true
		}
		ts.readSet[s.key] = true
		v, err := ts.tx.Get(key).Get()
		if err != nil {
			return err
		}
		ts.tx.Set(key, encodeInt(decodeInt(v)+1))
		ts.writeSet[s.key] = true
	case opAdd:
		ts.tx.Add(key, encodeInt(s.delta))
		ts.writeSet[s.key] = true
	}
	return nil
}

// drain re-runs an aborted transaction's program in a fresh, serially-committed transaction. Being
// strictly serial (no other transaction is open), it reads at the latest version, so a correct
// resolver cannot conflict it: any 1020 here is a spurious abort. Returns "" on success or a
// violation string.
func (w Workload) drain(db *simfdb.SimDB, ts *txnState, keyBytes [][]byte) string {
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		return fmt.Sprintf("drain txn %d: create: %v", ts.id, err)
	}
	for _, s := range ts.prog {
		key := fdb.Key(keyBytes[s.key])
		switch s.kind {
		case opInc:
			v, gerr := tx.Get(key).Get()
			if gerr != nil {
				return fmt.Sprintf("drain txn %d: get: %v", ts.id, gerr)
			}
			tx.Set(key, encodeInt(decodeInt(v)+1))
		case opAdd:
			tx.Add(key, encodeInt(s.delta))
		}
	}
	conflict, other := classifyCommit(tx.Commit().Get())
	if other != nil {
		return fmt.Sprintf("drain txn %d: unexpected commit error %v", ts.id, other)
	}
	if conflict {
		return fmt.Sprintf("txn %d spuriously aborted (1020) during SERIAL drain: no concurrent "+
			"writer exists, so the resolver must not abort it", ts.id)
	}
	return ""
}

// predictConflict is the verdict oracle's model of SimFDB's SSI resolution (conflict.go): a
// transaction conflicts iff some transaction that committed AFTER its read version wrote a key it
// read. A write-only transaction (empty read set) never conflicts.
func predictConflict(ts *txnState, committed []committedTxn) bool {
	if len(ts.readSet) == 0 {
		return false
	}
	for _, c := range committed {
		if c.version <= ts.readVer {
			continue
		}
		for k := range ts.readSet {
			if c.writeSet[k] {
				return true
			}
		}
	}
	return false
}

// classifyCommit splits a commit outcome into (conflict=1020, otherError). nil ⇒ (false, nil).
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
	kind := "MISSED CONFLICT (SimFDB committed a transaction whose read key was overwritten after " +
		"its read version — a lost update the resolver failed to detect)"
	if !predicted && actual {
		kind = "SPURIOUS ABORT (SimFDB aborted 1020 a transaction with no conflicting committed writer)"
	}
	return fmt.Sprintf("verdict oracle: txn %d readVer=%d readSet=%v: model conflict=%v, SimFDB conflict=%v — %s",
		ts.id, ts.readVer, sortedKeys(ts.readSet), predicted, actual, kind)
}

// checkState reads every counter and compares it to the expected seed-derived total.
func checkState(db *simfdb.SimDB, keyBytes [][]byte, expected []int64) []string {
	var out []string
	for i, exp := range expected {
		got, err := readCounter(db, keyBytes[i])
		if err != nil {
			out = append(out, fmt.Sprintf("state oracle: read key %d: %v", i, err))
			continue
		}
		if got != exp {
			out = append(out, fmt.Sprintf("state oracle: key %d = %d, want %d (final keyspace "+
				"diverged from the sum of committed programs — a lost update, a rolled-back-write "+
				"leak, or an atomic-add miscount)", i, got, exp))
		}
	}
	return out
}

func readCounter(db *simfdb.SimDB, key []byte) (int64, error) {
	v, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		b, e := rtx.Get(fdb.Key(key)).Get()
		return b, e
	})
	if err != nil {
		return 0, err
	}
	b, _ := v.([]byte)
	return decodeInt(b), nil
}

// fingerprint hashes the whole final keyspace — the determinism probe (same seed ⇒ same hash).
func fingerprint(db *simfdb.SimDB) string {
	h := sha256.New()
	_, _ = db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		kvs := rtx.GetRange(fdb.KeyRange{Begin: fdb.Key{}, End: fdb.Key{0xff}}, fdb.RangeOptions{}).GetSliceOrPanic()
		for _, kv := range kvs {
			var lp [8]byte
			binary.BigEndian.PutUint32(lp[:4], uint32(len(kv.Key)))
			binary.BigEndian.PutUint32(lp[4:], uint32(len(kv.Value)))
			h.Write(lp[:])
			h.Write(kv.Key)
			h.Write(kv.Value)
		}
		return nil, nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// encodeInt / decodeInt store a counter as an 8-byte little-endian value, matching FDB's atomic
// Add width so a Set and an Add on the same key compose without a width mismatch. decodeInt treats
// an absent (nil) or short value as zero-extended.
func encodeInt(v int64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(v))
	return b
}

func decodeInt(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	var buf [8]byte
	copy(buf[:], b)
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

func copySet(s map[int]bool) map[int]bool {
	c := make(map[int]bool, len(s))
	for k := range s {
		c[k] = true
	}
	return c
}

func sortedKeys(s map[int]bool) []int {
	ks := make([]int, 0, len(s))
	for k := range s {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

// Profiles is the interleave sweep matrix: a balanced mix, a hot 2-key arena, a pure-RMW profile
// (maximum conflict — the resolver's stress case), and a pure-atomic-add profile (never conflicts —
// the commutativity/idempotency contrast). cmd/dst-hunt appends these to its registry.
func Profiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "interleave", Cfg: hunt.Config{Workload: Workload{Keys: 4, Txns: 4, OpsPerTxn: 3, AddPct: 40}}},
		{Name: "interleave-hot", Cfg: hunt.Config{Workload: Workload{Keys: 2, Txns: 6, OpsPerTxn: 3, AddPct: 30}}},
		{Name: "interleave-rmw", Cfg: hunt.Config{Workload: Workload{Keys: 3, Txns: 5, OpsPerTxn: 2, AddPct: 0}}},
		{Name: "interleave-add", Cfg: hunt.Config{Workload: Workload{Keys: 3, Txns: 5, OpsPerTxn: 3, AddPct: 100}}},
	}
}
