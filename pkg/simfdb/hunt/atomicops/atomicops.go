// Package atomicops is a DST driver that differentials FoundationDB's ten atomic mutation types
// (Add / And / Or / Xor / Max / Min / ByteMax / ByteMin / AppendIfFits / CompareAndClear)
// end-to-end through the SimFDB backend against an INDEPENDENT Go reference model.
//
// Why it earns its keep on top of the existing hunt coverage:
//   - The interleave driver exercises atomic-ADD only; the other nine ops — and every
//     width / endianness / missing-key edge — are otherwise unprobed by any hunt.
//   - SimFDB's applyAtomic is already differential-tested against the pure-Go client's applyAtomic
//     (both are ports of C++ Atomic.h). This driver adds a genuinely THIRD, independent reference:
//     the numeric ops (Add/Max/Min) run through a math/big little-endian path instead of a byte
//     carry-loop, so a bug common to BOTH byte-loop ports still surfaces here.
//   - It drives the ops END-TO-END through the backend — the commit path, the clear→delete wiring
//     for CompareAndClear, and RYW folding inside an open transaction — none of which the
//     pure-function differential touches.
//
// Two absolute oracles (the independent model is the reference; zero violations == agreement):
//
//	STATE  after each committed transaction a fresh read of the whole keyspace equals the model
//	       — this is where key PRESENCE is checked (absent vs present-empty).
//	RYW    a read of a key inside the open transaction, after its pending atomic ops, equals the
//	       model's in-transaction projection (SimFDB folds RYW via resolveKey).
//
// The load-bearing distinction the model tracks and the STATE oracle probes is ABSENT (no key) vs
// PRESENT-EMPTY (key present with a zero-length value): AndV2 / MinV2 / ByteMin / CompareAndClear
// all branch on it. A store that dropped a present-empty value on commit or on read would flip an
// atomic op onto its absent branch — exactly the divergence this driver is built to catch.
//
// This is RFC-199 Tier 2. It plugs into the shared hunt machinery via hunt.Workload.
package atomicops

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand/v2"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// aoStream is this driver's PCG stream, distinct from the other drivers' streams so its op
// sequence is independent yet reproducible from the one seed.
const aoStream = uint64(0xA70A70)

// valueSizeLimit mirrors CLIENT_KNOBS->VALUE_SIZE_LIMIT — the 100 KB cap AppendIfFits refuses to
// exceed. Declared here independently of simfdb.atomicValueSizeLimit (which is unexported) so the
// model is a true second source, not a reference into the code under test.
const valueSizeLimit = 100_000

// Workload is a seeded atomic-op differential over SimFDB. It ignores the record-oriented
// hunt.Config; its knobs live on the struct.
type Workload struct {
	Keys      int  // distinct keys (default 6)
	Txns      int  // transactions per run (default 12)
	OpsPerTxn int  // ops per transaction (default 8)
	RYWChecks bool // also assert in-transaction RYW reads (default true)

	// EdgeBias skews param widths to {0,1} and Set to empty values, hammering the absent-vs-
	// present-empty branches (AndV2 / MinV2 / ByteMin / CompareAndClear) far harder than the
	// balanced profile does.
	EdgeBias bool
}

// Name implements hunt.Workload.
func (Workload) Name() string { return "atomicops" }

// Run implements hunt.Workload; the record-oriented Config is ignored.
func (w Workload) Run(seed uint64, _ hunt.Config) *hunt.Report { return w.run(seed).Report }

func (w Workload) withDefaults() Workload {
	if w.Keys <= 0 {
		w.Keys = 6
	}
	if w.Txns <= 0 {
		w.Txns = 12
	}
	if w.OpsPerTxn <= 0 {
		w.OpsPerTxn = 8
	}
	return w
}

// Result is the rich outcome: the standard hunt.Report plus per-op counts so a test can assert
// every one of the ten atomic ops actually fired (the NO-FAKE-CHECKBOX proof that the driver
// exercises the whole surface, not just add).
type Result struct {
	Report *hunt.Report
	Txns   int
	OpsRun int
	Counts map[opcode]int // op -> times executed
}

// opcode is one workload step. The first two are non-atomic state setters; the rest are the ten
// FDB atomic mutations under test.
type opcode int

const (
	opSet opcode = iota
	opClear
	opAdd
	opAnd
	opOr
	opXor
	opMax
	opMin
	opByteMax
	opByteMin
	opAppendIfFits
	opCompareAndClear
)

func (o opcode) String() string {
	switch o {
	case opSet:
		return "set"
	case opClear:
		return "clear"
	case opAdd:
		return "add"
	case opAnd:
		return "and"
	case opOr:
		return "or"
	case opXor:
		return "xor"
	case opMax:
		return "max"
	case opMin:
		return "min"
	case opByteMax:
		return "bytemax"
	case opByteMin:
		return "bytemin"
	case opAppendIfFits:
		return "appendiffits"
	default:
		return "compareandclear"
	}
}

// atomicOps is the set of folding ops (everything except Set/Clear), used by the picker.
var atomicOps = []opcode{
	opAdd, opAnd, opOr, opXor, opMax, opMin, opByteMax, opByteMin, opAppendIfFits, opCompareAndClear,
}

func (w Workload) run(seed uint64) *Result {
	w = w.withDefaults()
	rng := rand.New(rand.NewPCG(seed, aoStream))

	// No faults: transactions are opened, filled and committed one at a time, so there is never an
	// SSI conflict and (Buggify disabled) never an injected commit fault. Every commit succeeds and
	// the STATE oracle is exact. Fault×atomic durability is interleave's job, not this driver's.
	env := hunt.NewSimEnv(seed, 0)
	db := simfdb.New(env)
	defer db.Close()

	sub := subspace.FromBytes(tuple.Tuple{"ao"}.Pack())
	keyBytes := make([][]byte, w.Keys)
	for i := range keyBytes {
		keyBytes[i] = sub.Pack(tuple.Tuple{int64(i)})
	}

	rep := &hunt.Report{Seed: seed}
	res := &Result{Report: rep, Txns: w.Txns, Counts: map[opcode]int{}}

	// committed is the reference model after the last successful commit. absent key == not in map;
	// present-empty == present with a zero-length (never nil) value.
	committed := map[string][]byte{}

	for t := 0; t < w.Txns; t++ {
		tx, err := db.CreateWritableTransaction()
		if err != nil {
			rep.Err = fmt.Sprintf("create txn %d: %v", t, err)
			return res
		}

		// pending projects the committed model forward through this transaction's queued ops so a
		// RYW read can be checked against it before the commit lands.
		pending := cloneModel(committed)

		for j := 0; j < w.OpsPerTxn; j++ {
			k := rng.IntN(w.Keys)
			keyStr := string(keyBytes[k])
			op := w.pickOp(rng)
			param := w.genParam(rng, op, pending[keyStr])

			applyToTxn(tx, op, keyBytes[k], param)
			modelApply(pending, op, keyStr, param)
			res.Counts[op]++
			res.OpsRun++

			// RYW oracle: read the key back inside the open transaction and compare its value to the
			// pending projection. Value bytes only — nil and empty compare equal here, so present-
			// empty-vs-absent is left to the STATE oracle where GetRange makes presence observable.
			if w.RYWChecks && rng.IntN(3) == 0 {
				got, gerr := tx.Get(fdb.Key(keyBytes[k])).Get()
				if gerr != nil {
					rep.Ops = res.OpsRun
					rep.Err = fmt.Sprintf("txn %d ryw get key %d: %v", t, k, gerr)
					return res
				}
				want := pending[keyStr]
				if !bytes.Equal(got, want) {
					rep.Ops = res.OpsRun
					rep.Violations = []string{fmt.Sprintf(
						"RYW oracle: txn %d op %d %s key %d: in-txn Get = %s, model = %s "+
							"(SimFDB's resolveKey folded the pending atomic ops differently from the "+
							"independent reference)",
						t, j, op, k, hexOrAbsent(got), hexOrAbsent(want))}
					return res
				}
			}
		}

		if err := tx.Commit().Get(); err != nil {
			rep.Ops = res.OpsRun
			rep.Err = fmt.Sprintf("txn %d commit: %v", t, err)
			return res
		}
		committed = pending

		// STATE oracle: read the whole keyspace and compare it to the committed model, presence
		// included. This is where a dropped present-empty value or a miscomputed fold surfaces.
		if v := checkState(db, sub, committed); v != "" {
			rep.Ops = res.OpsRun
			rep.Violations = []string{fmt.Sprintf("after txn %d: %s", t, v)}
			return res
		}
	}

	rep.Ops = res.OpsRun
	rep.Fingerprint = fingerprint(db, sub)
	return res
}

// pickOp draws a Set/Clear/atomic step. Set and Clear keep the keyspace churning through varied
// existing states (absent / present-empty / present); the ten atomic ops are drawn uniformly so a
// long sweep exercises each roughly equally.
func (w Workload) pickOp(rng *rand.Rand) opcode {
	switch r := rng.IntN(100); {
	case r < 22:
		return opSet
	case r < 30:
		return opClear
	default:
		return atomicOps[rng.IntN(len(atomicOps))]
	}
}

// genParam produces the operand bytes for op. For opSet it is the value stored (Clear ignores it).
// For CompareAndClear it half the time reuses the key's current pending value so the clear branch
// actually fires; otherwise a random operand exercises the no-match branch.
func (w Workload) genParam(rng *rand.Rand, op opcode, current []byte) []byte {
	if op == opClear {
		return nil
	}
	if op == opCompareAndClear && current != nil && rng.IntN(2) == 0 {
		return append([]byte(nil), current...) // force a match -> clear
	}

	width := w.pickWidth(rng, op)
	if op == opSet && w.EdgeBias && rng.IntN(2) == 0 {
		return []byte{} // present-empty value
	}
	return genBytes(rng, width)
}

// pickWidth chooses an operand width, biased to boundary sizes: 0 (empty), 1, 8 (uint64), and 9/17
// (multi-word, so the big.Int path is exercised across word boundaries). EdgeBias collapses to the
// {0,1} nil-vs-empty edges.
func (w Workload) pickWidth(rng *rand.Rand, op opcode) int {
	if w.EdgeBias {
		return rng.IntN(2) // 0 or 1
	}
	widths := []int{0, 1, 2, 4, 8, 9, 16, 17}
	// ByteMax/ByteMin/Set/AppendIfFits/CompareAndClear are byte-string ops; a zero width is a
	// legitimate empty operand for them too, so no special-casing is needed.
	return widths[rng.IntN(len(widths))]
}

// genBytes returns width bytes, occasionally all-zero or all-0xff so identity/annihilator edges
// (AND with 0xff, OR/XOR with 0x00, Max/Min at the extremes) are hit deliberately.
func genBytes(rng *rand.Rand, width int) []byte {
	if width == 0 {
		return []byte{}
	}
	b := make([]byte, width)
	switch rng.IntN(4) {
	case 0:
		// all zero
	case 1:
		for i := range b {
			b[i] = 0xff
		}
	default:
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
	}
	return b
}

// applyToTxn dispatches op onto the SimFDB writable transaction.
func applyToTxn(tx fdb.WritableTransaction, op opcode, key, param []byte) {
	k := fdb.Key(key)
	switch op {
	case opSet:
		tx.Set(k, param)
	case opClear:
		tx.Clear(k)
	case opAdd:
		tx.Add(k, param)
	case opAnd:
		tx.And(k, param)
	case opOr:
		tx.Or(k, param)
	case opXor:
		tx.Xor(k, param)
	case opMax:
		tx.Max(k, param)
	case opMin:
		tx.Min(k, param)
	case opByteMax:
		tx.ByteMax(k, param)
	case opByteMin:
		tx.ByteMin(k, param)
	case opAppendIfFits:
		tx.AppendIfFits(k, param)
	case opCompareAndClear:
		tx.CompareAndClear(k, param)
	}
}

// modelApply folds one op into the reference model. It is a deliberately independent
// reimplementation of FDB atomic semantics (C++ Atomic.h) — the numeric ops go through math/big
// rather than a byte carry-loop — so it is a true differential against SimFDB's applyAtomic.
func modelApply(m map[string][]byte, op opcode, key string, param []byte) {
	existing, present := m[key]
	switch op {
	case opSet:
		m[key] = append([]byte{}, param...) // stored verbatim; empty param -> present-empty (never nil)
	case opClear:
		delete(m, key)
	case opCompareAndClear:
		// Clear iff absent or the value equals param; otherwise unchanged.
		if !present || bytes.Equal(existing, param) {
			delete(m, key)
		}
	default:
		nv := modelFold(op, existing, present, param)
		if nv == nil {
			// Mirror applyAtomic's normalisation: a non-clear nil result is a PRESENT empty value.
			nv = []byte{}
		}
		m[key] = nv
	}
}

// modelFold computes the new stored value for one of the eight folding atomic ops, from first
// principles. present distinguishes an absent key (existing==nil, present==false) from a
// present-empty value (existing==[]byte{}, present==true) — load-bearing for And/Min/etc.
func modelFold(op opcode, existing []byte, present bool, param []byte) []byte {
	switch op {
	case opAdd:
		return modelAdd(existing, param)
	case opAnd:
		return modelAnd(existing, present, param)
	case opOr:
		return modelOrXor(existing, present, param, false)
	case opXor:
		return modelOrXor(existing, present, param, true)
	case opMax:
		return modelMax(existing, param)
	case opMin:
		return modelMin(existing, present, param)
	case opByteMax:
		return modelByteMax(existing, present, param)
	case opByteMin:
		return modelByteMin(existing, present, param)
	case opAppendIfFits:
		return modelAppendIfFits(existing, param)
	default:
		return existing
	}
}

// --- numeric ops via math/big (the independent path) ---

// leToBig interprets b as a little-endian unsigned integer.
func leToBig(b []byte) *big.Int {
	be := make([]byte, len(b))
	for i := range b {
		be[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(be)
}

// bigToLE serialises n little-endian into a width-byte slice, keeping the low width bytes (mod
// 2^(8*width)); a shorter n is zero-padded on the high end.
func bigToLE(n *big.Int, width int) []byte {
	out := make([]byte, width)
	tmp := new(big.Int).Set(n)
	mask := big.NewInt(0xff)
	for i := 0; i < width; i++ {
		out[i] = byte(new(big.Int).And(tmp, mask).Int64())
		tmp.Rsh(tmp, 8)
	}
	return out
}

// atWidth reinterprets existing at operand width w: FDB drops existing bytes beyond w and
// zero-extends when shorter, since the numeric ops operate purely at param width.
func atWidth(existing []byte, w int) []byte {
	if len(existing) > w {
		return existing[:w]
	}
	return existing
}

// modelAdd — little-endian add, result width = len(param), overflow wraps. Absent/empty existing
// folds to param.
func modelAdd(existing, param []byte) []byte {
	w := len(param)
	if w == 0 {
		return []byte{}
	}
	sum := new(big.Int).Add(leToBig(atWidth(existing, w)), leToBig(param))
	return bigToLE(sum, w)
}

// modelMax — unsigned little-endian max at param width. Absent/empty existing folds to param (both
// are 0). On a tie param wins.
func modelMax(existing, param []byte) []byte {
	w := len(param)
	if w == 0 {
		return []byte{}
	}
	e := leToBig(atWidth(existing, w))
	p := leToBig(param)
	if e.Cmp(p) > 0 {
		return bigToLE(e, w)
	}
	return append([]byte(nil), param...)
}

// modelMin — MinV2: an absent key stores param; a present key (incl. present-empty) runs the V1
// unsigned little-endian min at param width. On a tie param wins.
func modelMin(existing []byte, present bool, param []byte) []byte {
	if !present {
		return append([]byte(nil), param...) // MinV2 absent -> param
	}
	w := len(param)
	if w == 0 {
		return []byte{}
	}
	e := leToBig(atWidth(existing, w))
	p := leToBig(param)
	if e.Cmp(p) < 0 {
		return bigToLE(e, w)
	}
	return append([]byte(nil), param...)
}

// --- bitwise ops ---

// modelAnd — AndV2: an absent key stores param; else bitwise AND to param width (existing bytes
// past param width dropped, positions past existing are 0 & x == 0).
func modelAnd(existing []byte, present bool, param []byte) []byte {
	if !present {
		return append([]byte(nil), param...) // AndV2 absent -> param
	}
	if len(param) == 0 {
		return []byte{}
	}
	out := make([]byte, len(param))
	for i := range param {
		var e byte
		if i < len(existing) {
			e = existing[i]
		}
		out[i] = e & param[i]
	}
	return out
}

// modelOrXor — OR (xor=false) or XOR (xor=true). Result width = len(param). Absent/empty existing,
// or empty param, folds to param; positions past existing take param's byte verbatim (0 op x == x).
func modelOrXor(existing []byte, present bool, param []byte, xor bool) []byte {
	e := existing
	if !present {
		e = []byte{}
	}
	if len(e) == 0 || len(param) == 0 {
		return append([]byte(nil), param...)
	}
	out := make([]byte, len(param))
	for i := range param {
		var ev byte
		if i < len(e) {
			ev = e[i]
		}
		if xor {
			out[i] = ev ^ param[i]
		} else {
			out[i] = ev | param[i]
		}
	}
	return out
}

// --- byte-string ops ---

// modelByteMax — lexicographic (memcmp) max; the greater raw string stored verbatim. Absent
// existing folds to param.
func modelByteMax(existing []byte, present bool, param []byte) []byte {
	if !present {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(existing, param) > 0 {
		return append([]byte(nil), existing...)
	}
	return append([]byte(nil), param...)
}

// modelByteMin — lexicographic (memcmp) min. Absent existing folds to param; a present-empty
// existing is the lexicographically smallest string and therefore wins.
func modelByteMin(existing []byte, present bool, param []byte) []byte {
	if !present {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(existing, param) < 0 {
		return append([]byte(nil), existing...)
	}
	return append([]byte(nil), param...)
}

// modelAppendIfFits — append param to existing iff the result fits VALUE_SIZE_LIMIT; else a no-op.
// Absent/empty existing folds to param; empty param leaves existing unchanged.
func modelAppendIfFits(existing, param []byte) []byte {
	if len(existing) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), existing...)
	}
	if len(existing)+len(param) > valueSizeLimit {
		return append([]byte(nil), existing...)
	}
	out := make([]byte, 0, len(existing)+len(param))
	out = append(out, existing...)
	out = append(out, param...)
	return out
}

// --- oracle plumbing ---

// checkState reads the whole subspace and compares it to the model, presence included. Returns ""
// on agreement or a descriptive violation on the first (sorted) divergence.
func checkState(db *simfdb.SimDB, sub subspace.Subspace, model map[string][]byte) string {
	got := readAll(db, sub)

	keys := map[string]struct{}{}
	for k := range got {
		keys[k] = struct{}{}
	}
	for k := range model {
		keys[k] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	for _, k := range ordered {
		gv, gok := got[k]
		mv, mok := model[k]
		switch {
		case gok && !mok:
			return fmt.Sprintf("STATE oracle: SimFDB has key %s = %s but the model has it ABSENT "+
				"(a phantom write or a failed clear)", keyHex(sub, k), hexOrAbsent(gv))
		case !gok && mok:
			return fmt.Sprintf("STATE oracle: model has key %s = %s but SimFDB read it back ABSENT "+
				"(a dropped present-empty value or a lost write — this is the load-bearing "+
				"absent-vs-present-empty case)", keyHex(sub, k), hexOrAbsent(mv))
		case gok && mok && !bytes.Equal(gv, mv):
			return fmt.Sprintf("STATE oracle: key %s: SimFDB = %s, model = %s "+
				"(an atomic fold computed a different value than the independent reference)",
				keyHex(sub, k), hexOrAbsent(gv), hexOrAbsent(mv))
		}
	}
	return ""
}

// readAll snapshots the whole subspace into a map keyed by the full raw key bytes. A present-empty
// value is retained as a zero-length (non-nil) slice so the presence comparison has teeth.
func readAll(db *simfdb.SimDB, sub subspace.Subspace) map[string][]byte {
	out := map[string][]byte{}
	begin, end := sub.FDBRangeKeys()
	_, _ = db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		kvs := rtx.GetRange(fdb.KeyRange{
			Begin: fdb.Key(begin.FDBKey()),
			End:   fdb.Key(end.FDBKey()),
		}, fdb.RangeOptions{}).GetSliceOrPanic()
		for _, kv := range kvs {
			v := kv.Value
			if v == nil {
				v = []byte{}
			}
			out[string(kv.Key)] = append([]byte{}, v...)
		}
		return nil, nil
	})
	return out
}

// fingerprint hashes the whole final keyspace so a test can assert byte-identical replay. Keys and
// values are length-prefixed so their boundaries can't alias across entries.
func fingerprint(db *simfdb.SimDB, sub subspace.Subspace) string {
	got := readAll(db, sub)
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	var lp [4]byte
	writeLP := func(b []byte) {
		binary.BigEndian.PutUint32(lp[:], uint32(len(b)))
		h.Write(lp[:])
		h.Write(b)
	}
	for _, k := range keys {
		writeLP([]byte(k))
		writeLP(got[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cloneModel(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = append([]byte{}, v...)
	}
	return out
}

// hexOrAbsent renders a value for a violation message: hex, or "<absent>" for a nil (absent) value.
func hexOrAbsent(v []byte) string {
	if v == nil {
		return "<absent>"
	}
	if len(v) == 0 {
		return "<empty>"
	}
	return hex.EncodeToString(v)
}

// keyHex renders a raw key relative to the subspace as the tuple index when possible, else hex.
func keyHex(sub subspace.Subspace, raw string) string {
	if t, err := sub.Unpack(fdb.Key(raw)); err == nil && len(t) == 1 {
		if i, ok := t[0].(int64); ok {
			return fmt.Sprintf("k%d", i)
		}
	}
	return hex.EncodeToString([]byte(raw))
}

// Profiles is the atomic-op sweep matrix: a balanced surface, a hot 2-key arena that stacks many
// ops per key per transaction, and an edge profile that hammers the nil-vs-empty branches.
func Profiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "atomic-ops", Cfg: hunt.Config{Workload: Workload{Keys: 6, Txns: 12, OpsPerTxn: 8, RYWChecks: true}}},
		{Name: "atomic-ops-hot", Cfg: hunt.Config{Workload: Workload{Keys: 2, Txns: 10, OpsPerTxn: 10, RYWChecks: true}}},
		{Name: "atomic-ops-edge", Cfg: hunt.Config{Workload: Workload{Keys: 3, Txns: 12, OpsPerTxn: 8, RYWChecks: true, EdgeBias: true}}},
	}
}
