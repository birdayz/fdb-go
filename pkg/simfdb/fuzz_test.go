package simfdb

import (
	"bytes"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// FuzzSimFDB_RYW cross-checks SimFDB's read-your-writes merge (snapshot + buffered mutations,
// resolveKey / buildViewRange) against a straightforward sequential reference model over a
// small keyspace. A random op stream is applied to a transaction; after each op EVERY read
// surface must agree with the model and with each other:
//
//   - the point read of each key (resolveKey walks the buffer over the stored row);
//   - the same key read as a single-key RANGE (a windowed store scan merged with the buffer —
//     a completely different code path, so point-vs-range disagreement is a defect either way);
//   - the whole span through GetSlice (one fetch for the full budget) and through the Iterator
//     (paged, re-resolving against the buffer on every page);
//   - the whole span REVERSED, which must be the forward answer read backwards.
//
// The cross-surface half of that was documented here long before it existed: the body checked
// point reads only and never called GetRange at all. It matters more now that a range read is
// lazy — the iterator resolves each page against the buffer as it is at fetch time, so the
// paged and single-fetch surfaces genuinely execute different merges over the same state.
//
// The "merged with the buffer" half was documented ahead of itself in the same way: the target
// opened on an EMPTY store, so there was no stored side to merge and every read was buffer-or-
// absent. Committed baseline rows (below) are what make the merge real.
//
// Together this exercises mutation ordering, clear-range coverage, and atomic accumulation of
// the RYW path independently of the (separately-tested) applyAtomic byte semantics.
func FuzzSimFDB_RYW(f *testing.F) {
	f.Add([]byte{0, 2, 5, 1, 3, 2, 4, 8, 0, 0, 1})
	f.Add([]byte{})
	f.Add([]byte{3, 0, 4, 3, 2, 0, 7})
	// Longer than maxOps, so the seed corpus covers the truncation path itself. (An
	// uncapped body reached ~1250 ops on an input like this and the fuzzer killed the
	// worker for taking 5.7s; the cap is why that is now 0.01s.)
	f.Add(bytes.Repeat([]byte{2, 3, 5}, 100))

	const nKeys = 6
	key := func(b byte) []byte { return []byte{'k', b%nKeys + '0'} }

	// COMMITTED baseline rows, laid down before the transaction opens. Every read below is
	// therefore a genuine merge of a windowed store scan with the write buffer, which is what the
	// target claims to exercise: with an empty store the "store side" is constant, every read
	// resolves to buffer-or-absent, and the whole store/buffer axis is untested while looking
	// covered. Only a SUBSET of the keyspace is seeded, so both directions stay reachable — a
	// key the buffer shadows and a key only storage can answer.
	//
	// The values are two bytes wide while every value the op stream writes is one byte, so a
	// merge that returns the wrong side is visible in the value itself rather than only in a
	// presence check. It also puts AppendIfFits over a STORED value on the path (the model
	// appends to the baseline), and Clear/ClearRange over one.
	baseline := map[string][]byte{
		"k0": []byte("S0"),
		"k2": []byte("S2"),
		"k4": []byte("S4"),
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		db := New(nil)
		seed(db, "k0", "S0", "k2", "S2", "k4", "S4")
		tx := db.newTxn()
		model := make(map[string][]byte) // reference: key -> value (nil/absent = not present)
		for k, v := range baseline {
			model[k] = append([]byte(nil), v...)
		}

		// maxOps bounds the op stream so one input stays well inside the fuzzer's per-input
		// budget. The oracle checks fifteen reads after EVERY op, and each read rebuilds the
		// write map from the buffer (buildWriteMap is deliberately recomputed per conflict
		// decision — see ryw_conflict.go), so the body is quadratic in the op count. An
		// unbounded stream reached ~1250 ops and 5.7s for a single input, which the fuzzer
		// reports as a hung worker rather than as the timing artefact it is.
		//
		// The bound costs nothing the target was buying: the interesting axis is the op
		// SEQUENCE (mutation ordering, clear coverage, atomic accumulation over a key), and
		// every ordering worth reaching is reachable inside 64 ops over 6 keys. Length alone
		// only re-walks states already visited.
		const maxOps = 64

		i := 0
		next := func() (byte, bool) {
			if i >= len(data) {
				return 0, false
			}
			b := data[i]
			i++
			return b, true
		}

		for ops := 0; ops < maxOps; ops++ {
			opb, ok := next()
			if !ok {
				break
			}
			kb, ok := next()
			if !ok {
				break
			}
			k := key(kb)
			switch opb % 4 {
			case 0: // Set
				vb, _ := next()
				val := []byte{vb}
				tx.Set(fdb.Key(k), val)
				model[string(k)] = val
			case 1: // Clear
				tx.Clear(fdb.Key(k))
				delete(model, string(k))
			case 2: // ClearRange [k, k+2)
				end := key(kb + 2)
				lo, hi := k, end
				if bytes.Compare(lo, hi) > 0 {
					lo, hi = hi, lo
				}
				tx.ClearRange(fdb.KeyRange{Begin: fdb.Key(lo), End: fdb.Key(hi)})
				for mk := range model {
					if bytes.Compare([]byte(mk), lo) >= 0 && bytes.Compare([]byte(mk), hi) < 0 {
						delete(model, mk)
					}
				}
			case 3: // AppendIfFits (deterministic, no width math) — append one byte
				vb, _ := next()
				tx.AppendIfFits(fdb.Key(k), []byte{vb})
				cur := model[string(k)]
				model[string(k)] = append(append([]byte(nil), cur...), vb)
			}

			// After each op, every key's RYW value must match the model — and the same key read
			// as a single-key RANGE must agree with the point read. The two go through
			// different code (resolveKey walks the buffer; the range read merges a windowed
			// store scan with it), so a divergence between them is a real defect either way.
			for b := byte(0); b < nKeys; b++ {
				kk := key(b)
				got := tx.Get(fdb.Key(kk)).MustGet()
				want := model[string(kk)]
				if (got == nil) != (want == nil) || !bytes.Equal(got, want) {
					t.Fatalf("RYW mismatch key %q: got %x (nil=%v), want %x (nil=%v)",
						kk, got, got == nil, want, want == nil)
				}
				single := tx.GetRange(fdb.KeyRange{
					Begin: fdb.Key(kk), End: fdb.Key(keyAfter(kk)),
				}, fdb.RangeOptions{}).GetSliceOrPanic()
				switch {
				case want == nil && len(single) != 0:
					t.Fatalf("single-key range over %q returned %d rows, point read says absent",
						kk, len(single))
				case want != nil && (len(single) != 1 || !bytes.Equal(single[0].Value, want)):
					t.Fatalf("single-key range over %q = %v, point read = %x", kk, single, want)
				}
			}

			// The whole-span range read must agree with the model too, on BOTH consumption
			// surfaces and in both directions. GetSlice takes the budget in one fetch while the
			// iterator pages, and each page re-resolves against the write buffer, so the two
			// exercise genuinely different merge paths over the same state.
			var wantRows []string
			for b := byte(0); b < nKeys; b++ {
				kk := key(b)
				if v := model[string(kk)]; v != nil {
					wantRows = append(wantRows, string(kk)+"="+string(v))
				}
			}
			span := fdb.KeyRange{Begin: fdb.Key("k"), End: fdb.Key("l")}
			checkRows := func(what string, rows []string) {
				if len(rows) != len(wantRows) {
					t.Fatalf("%s = %v, model = %v", what, rows, wantRows)
				}
				for i := range rows {
					if rows[i] != wantRows[i] {
						t.Fatalf("%s = %v, model = %v", what, rows, wantRows)
					}
				}
			}
			var sliceRows []string
			for _, kv := range tx.GetRange(span, fdb.RangeOptions{}).GetSliceOrPanic() {
				sliceRows = append(sliceRows, string(kv.Key)+"="+string(kv.Value))
			}
			checkRows("GetSlice", sliceRows)

			var iterRows []string
			it := tx.GetRange(span, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
			for it.Advance() {
				kv := it.MustGet()
				iterRows = append(iterRows, string(kv.Key)+"="+string(kv.Value))
			}
			checkRows("Iterator", iterRows)

			var revRows []string
			revIt := tx.GetRange(span, fdb.RangeOptions{
				Mode: fdb.StreamingModeIterator, Reverse: true,
			}).Iterator()
			for revIt.Advance() {
				kv := revIt.MustGet()
				revRows = append(revRows, string(kv.Key)+"="+string(kv.Value))
			}
			for i, j := 0, len(revRows)-1; i < j; i, j = i+1, j-1 {
				revRows[i], revRows[j] = revRows[j], revRows[i]
			}
			checkRows("reverse Iterator (re-reversed)", revRows)
		}
	})
}
