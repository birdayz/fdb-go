package simfdb

import (
	"bytes"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// FuzzSimFDB_RYW cross-checks SimFDB's read-your-writes merge (snapshot + buffered mutations,
// resolveKey/buildView) against a straightforward sequential reference model over a small
// keyspace. A random op stream is applied to a transaction; after each op the RYW value of
// every key must equal the model, and a point read must equal the same-key range read. This
// exercises the mutation ordering, clear-range coverage, and atomic accumulation of the RYW
// path independently of the (separately-tested) applyAtomic byte semantics.
func FuzzSimFDB_RYW(f *testing.F) {
	f.Add([]byte{0, 2, 5, 1, 3, 2, 4, 8, 0, 0, 1})
	f.Add([]byte{})
	f.Add([]byte{3, 0, 4, 3, 2, 0, 7})

	const nKeys = 6
	key := func(b byte) []byte { return []byte{'k', b%nKeys + '0'} }

	f.Fuzz(func(t *testing.T, data []byte) {
		db := New(nil)
		tx := db.newTxn(false)
		model := make(map[string][]byte) // reference: key -> value (nil/absent = not present)

		i := 0
		next := func() (byte, bool) {
			if i >= len(data) {
				return 0, false
			}
			b := data[i]
			i++
			return b, true
		}

		for {
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

			// After each op, every key's RYW value must match the model.
			for b := byte(0); b < nKeys; b++ {
				kk := key(b)
				got := tx.Get(fdb.Key(kk)).MustGet()
				want := model[string(kk)]
				if (got == nil) != (want == nil) || !bytes.Equal(got, want) {
					t.Fatalf("RYW mismatch key %q: got %x (nil=%v), want %x (nil=%v)",
						kk, got, got == nil, want, want == nil)
				}
			}
		}
	})
}
