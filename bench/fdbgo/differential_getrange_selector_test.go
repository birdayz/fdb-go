//go:build bazelrunfiles

package bench

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// GetRange with key-SELECTOR bounds carrying NON-ZERO offsets / orEqual — RFC-010 C3
// (fresh differential axis). TestInterop_SelectorRange covers exactly one committed
// shape ([LastLessOrEqual, FirstGreaterThan), offset=1); TestDifferential_RangeRead
// uses plain prefix (key) ranges. Neither probes selector bounds with non-trivial
// OFFSETS, which both clients resolve inside the range read: the RYW walk and the
// storage server, with the selectors in the GetKeyValuesRequest (RFC-262 D10;
// ReadYourWrites.actor.cpp:592-813, NativeAPI.actor.cpp:4651-4679). This pins the
// results against libfdb_c; client:selector_range_conformance_test pins the
// read-conflict ranges.
//
// Determinism: both clients pin the SAME read version and read identical committed
// storage, so go==cgo is a clean assertion. Clamp: all bound selectors are chosen to
// resolve WITHIN the seeded prefix [k00..kNN]; offsets are small enough not to escape
// into the concurrently-mutated shared keyspace.

type srbSpec struct {
	key     string // suffix within the prefix, e.g. "k05"
	orEqual bool
	offset  int
}

func goSrbSel(pfx string, s srbSpec) gofdb.KeySelector {
	return gofdb.KeySelector{Key: gofdb.Key(pfx + s.key), OrEqual: s.orEqual, Offset: s.offset}
}

func cgoSrbSel(pfx string, s srbSpec) cgofdb.KeySelector {
	return cgofdb.KeySelector{Key: cgofdb.Key(pfx + s.key), OrEqual: s.orEqual, Offset: s.offset}
}

func TestDifferential_GetRangeSelectorBounds(t *testing.T) {
	t.Parallel()
	const prefix = "diff_srb_"
	const n = 20
	seedKeys(t, func(tx cgofdb.Transaction) {
		for i := 0; i < n; i++ {
			tx.Set(cgofdb.Key(fmt.Sprintf("%sk%02d", prefix, i)), []byte{byte(i)})
		}
	})

	// FGE(k)=(k,false,1) FGT(k)=(k,true,1) LLE(k)=(k,true,0) LLT(k)=(k,false,0);
	// add N to .offset to step N keys forward (or back, for the backward forms).
	cases := []struct {
		name       string
		begin, end srbSpec
		limit      int
		reverse    bool
	}{
		// Baseline (matches the interop shape) — orEqual, offset 1/0.
		{"lle03_to_fgt07", srbSpec{"k03", true, 0}, srbSpec{"k07", true, 1}, 0, false},
		// Forward offset on the BEGIN selector: FGT(k03)+3 steps to k07.
		{"fgt03plus3_to_fge15", srbSpec{"k03", true, 1 + 3}, srbSpec{"k15", false, 1}, 0, false},
		// Backward offset on the END selector: LLT(k15)-2 steps back to k12.
		{"fge02_to_llt15minus2", srbSpec{"k02", false, 1}, srbSpec{"k15", false, 0 - 2}, 0, false},
		// Both bounds backward selectors with offsets.
		{"llt05_to_lle12plus1", srbSpec{"k05", false, 0}, srbSpec{"k12", true, 0 + 1}, 0, false},
		// Forward offset that lands mid-range, with a limit.
		{"fge00plus4_to_fge18_limit5", srbSpec{"k00", false, 1 + 4}, srbSpec{"k18", false, 1}, 5, false},
		// Reverse + offset bounds.
		{"fge04_to_fge16_reverse", srbSpec{"k04", false, 1}, srbSpec{"k16", false, 1}, 0, true},
		{"fge04_to_fge16_rev_limit6", srbSpec{"k04", false, 1}, srbSpec{"k16", false, 1}, 6, true},
		// Inverted: begin resolves AFTER end → empty range (FDB returns nothing, no error).
		{"inverted_fge15_to_fge05", srbSpec{"k15", false, 1}, srbSpec{"k05", false, 1}, 0, false},
		// Adjacent: begin == end → empty.
		{"empty_fge08_to_fge08", srbSpec{"k08", false, 1}, srbSpec{"k08", false, 1}, 0, false},
		// Offset stepping from a non-present key (between k09 and k10 conceptually —
		// here exact keys, but FGT(k09)+1 = k11 via the present-key walk).
		{"fgt09plus1_to_fge14", srbSpec{"k09", true, 1 + 1}, srbSpec{"k14", false, 1}, 0, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const maxAttempts = 12
			for attempt := 0; ; attempt++ {
				if attempt >= maxAttempts {
					t.Fatalf("%s: retryable errors did not clear in %d attempts", tc.name, maxAttempts)
				}
				v := freshSharedVersion(t)
				goKVs, goErr := goSelRangeAt(v, gofdb.SelectorRange{Begin: goSrbSel(prefix, tc.begin), End: goSrbSel(prefix, tc.end)},
					gofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse})
				cKVs, cErr := cgoSelRangeAt(v, cgofdb.SelectorRange{Begin: cgoSrbSel(prefix, tc.begin), End: cgoSrbSel(prefix, tc.end)},
					cgofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse, Mode: cgofdb.StreamingModeWantAll})
				if (goErr != nil && isFDBRetryable(goErr)) || (cErr != nil && isFDBRetryable(cErr)) {
					continue
				}
				if (goErr == nil) != (cErr == nil) {
					t.Fatalf("%s: error mismatch: go=%v cgo=%v", tc.name, goErr, cErr)
				}
				if goErr != nil {
					// Both errored — the codes must still agree: a both-errored
					// case with DIFFERING non-retryable codes is a divergence, not a pass.
					if fdbErrorCode(goErr) != fdbErrorCode(cErr) {
						t.Fatalf("%s: both errored but codes differ: go=%d (%v) cgo=%d (%v)",
							tc.name, fdbErrorCode(goErr), goErr, fdbErrorCode(cErr), cErr)
					}
					return
				}
				gp, cp := normGo(goKVs), normC(cKVs)
				if len(gp) != len(cp) {
					t.Fatalf("%s: count differs: go=%d cgo=%d\n go=%v\ncgo=%v", tc.name, len(gp), len(cp), gp, cp)
				}
				for i := range gp {
					if !bytes.Equal(gp[i].k, cp[i].k) || !bytes.Equal(gp[i].v, cp[i].v) {
						t.Fatalf("%s: pair %d differs: go=(%x,%x) cgo=(%x,%x)", tc.name, i, gp[i].k, gp[i].v, cp[i].k, cp[i].v)
					}
				}
				return
			}
		})
	}
}

// pendMut is one pending mutation applied to an uncommitted txn.
type pendMut struct {
	clear bool // true = Clear(key); false = Set(key, val)
	key   string
	val   byte
}

// stripPfx removes the per-client prefix from each key so go/cgo results compare equal.
func stripPfx(kvs []kvPair, pfx string) []kvPair {
	out := make([]kvPair, len(kvs))
	for i, kv := range kvs {
		out[i] = kvPair{bytes.TrimPrefix(kv.k, []byte(pfx)), kv.v}
	}
	return out
}

// TestDifferential_GetRangeSelectorRYW pins GetRange with key-SELECTOR bounds over the
// READ-YOUR-WRITES view (pending writes merged with storage), serializable and snapshot.
// Both clients resolve the selectors in the walk over the write map and snapshot cache
// (getRangeValue / getRangeValueBack, ReadYourWrites.actor.cpp:592-813, 896-1123).
// Pending clears/sets at or near a resolved bound, offsets stepping across them, a begin
// key above the end key (the walk's nested getKey, :621-638, 925-940) and limits with
// reverse are where the two could diverge.
func TestDifferential_GetRangeSelectorRYW(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	goPfx := fmt.Sprintf("srbryw_%d_%s_go_", os.Getpid(), ns)
	cPfx := fmt.Sprintf("srbryw_%d_%s_c_", os.Getpid(), ns)
	clearPrefix(t, goPfx)
	clearPrefix(t, cPfx)

	const n = 20
	if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
		tx := txw.(gofdb.Transaction)
		for i := 0; i < n; i++ {
			tx.Set(gofdb.Key(fmt.Sprintf("%sk%02d", goPfx, i)), []byte{byte(i)})
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed go: %v", err)
	}
	mustCGo(t, func(tx cgofdb.Transaction) {
		for i := 0; i < n; i++ {
			tx.Set(cgofdb.Key(fmt.Sprintf("%sk%02d", cPfx, i)), []byte{byte(i)})
		}
	})

	cases := []rywSelectorCase{
		// Pending CLEAR of the key a backward begin selector resolves to: LLE(k07) over
		// the RYW view (k07 cleared) must resolve to k06, not k07.
		{"clear_begin_anchor", []pendMut{{true, "k07", 0}}, srbSpec{"k07", true, 0}, srbSpec{"k15", false, 1}, 0, false},
		// Pending CLEAR of the key an end selector resolves to.
		{"clear_end_anchor", []pendMut{{true, "k12", 0}}, srbSpec{"k03", false, 1}, srbSpec{"k12", true, 1}, 0, false},
		// Pending SET of a NEW key between seeded keys, with a forward offset stepping across it.
		{"set_newkey_offset_cross", []pendMut{{false, "k05a", 99}}, srbSpec{"k05", true, 1 + 2}, srbSpec{"k15", false, 1}, 0, false},
		// Pending clears thinning the range; offset must step over the holes.
		{"clear_holes_offset", []pendMut{{true, "k06", 0}, {true, "k07", 0}, {true, "k08", 0}}, srbSpec{"k04", false, 1 + 3}, srbSpec{"k16", false, 1}, 0, false},
		// Backward end offset over a pending set just below the anchor.
		{"set_below_end_back", []pendMut{{false, "k13a", 88}}, srbSpec{"k02", false, 1}, srbSpec{"k15", false, 0 - 1}, 0, false},
		// Reverse + pending clear inside the range.
		{"reverse_clear_mid", []pendMut{{true, "k10", 0}}, srbSpec{"k05", false, 1}, srbSpec{"k16", false, 1}, 0, true},
		// Pending set near the begin anchor + a limit.
		{"set_at_begin_anchor_limit", []pendMut{{false, "k09a", 77}}, srbSpec{"k09", true, 1}, srbSpec{"k14", false, 1}, 4, false},
		// BunchedMap's put read: lastLessOrEqual(k) .. firstGreaterThan(k)+1, k a new pending key.
		{"bunched_put_new_key", []pendMut{{false, "k07a", 55}}, srbSpec{"k07a", true, 0}, srbSpec{"k07a", true, 1 + 1}, 0, false},
		// The same read with the neighbours cleared: both selectors step over pending clears.
		{"bunched_put_cleared_neighbours", []pendMut{{true, "k07", 0}, {true, "k08", 0}, {false, "k07a", 55}}, srbSpec{"k07a", true, 0}, srbSpec{"k07a", true, 1 + 1}, 0, false},
		// Begin key above end key with an end offset that resolves past the begin: the walk's
		// nested getKey of the end selector before the loop.
		{"begin_above_end_key", []pendMut{{true, "k11", 0}}, srbSpec{"k12", false, 0 - 2}, srbSpec{"k10", true, 1 + 2}, 0, false},
		{"begin_above_end_key_reverse", []pendMut{{false, "k11a", 66}}, srbSpec{"k12", false, 0 - 2}, srbSpec{"k10", true, 1 + 2}, 0, true},
		// Reverse with offsets on both selectors, pending writes inside, and a limit cutting it.
		{"reverse_offsets_limit", []pendMut{{true, "k09", 0}, {false, "k10a", 44}}, srbSpec{"k04", true, 1 + 2}, srbSpec{"k15", false, 0 - 1}, 3, true},
		// Forward end offset beyond 1 stepping over pending clears, with a limit.
		{"end_offset_over_clears_limit", []pendMut{{true, "k13", 0}, {true, "k14", 0}}, srbSpec{"k10", false, 0}, srbSpec{"k12", true, 1 + 3}, 4, false},
	}

	for _, tc := range cases {
		for _, snapshot := range []bool{false, true} {
			tc, snapshot := tc, snapshot
			t.Run(fmt.Sprintf("%s/snapshot=%t", tc.name, snapshot), func(t *testing.T) {
				t.Parallel()
				checkRYWSelectorCase(t, tc, snapshot, goPfx, cPfx)
			})
		}
	}
}

type rywSelectorCase struct {
	name       string
	pending    []pendMut
	begin, end srbSpec
	limit      int
	reverse    bool
}

func checkRYWSelectorCase(t *testing.T, tc rywSelectorCase, snapshot bool, goPfx, cPfx string) {
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("%s: retryable errors did not clear in %d attempts", tc.name, maxAttempts)
		}
		v := freshSharedVersion(t)
		goTxn, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatalf("go CreateTransaction: %v", err)
		}
		cTxn, err := cgoClient.CreateTransaction()
		if err != nil {
			goTxn.Cancel()
			t.Fatalf("cgo CreateTransaction: %v", err)
		}
		goTxn.SetReadVersion(v)
		cTxn.SetReadVersion(v)
		for _, m := range tc.pending {
			if m.clear {
				goTxn.Clear(gofdb.Key(goPfx + m.key))
				cTxn.Clear(cgofdb.Key(cPfx + m.key))
			} else {
				goTxn.Set(gofdb.Key(goPfx+m.key), []byte{m.val})
				cTxn.Set(cgofdb.Key(cPfx+m.key), []byte{m.val})
			}
		}
		retry := func() bool {
			defer goTxn.Cancel()
			defer cTxn.Cancel()
			var goReader gofdb.ReadTransaction = goTxn
			var cReader cgofdb.ReadTransaction = cTxn
			if snapshot {
				goReader, cReader = goTxn.Snapshot(), cTxn.Snapshot()
			}
			goKVs, goErr := goReader.GetRange(gofdb.SelectorRange{Begin: goSrbSel(goPfx, tc.begin), End: goSrbSel(goPfx, tc.end)},
				gofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse}).GetSliceWithError()
			cKVs, cErr := cReader.GetRange(cgofdb.SelectorRange{Begin: cgoSrbSel(cPfx, tc.begin), End: cgoSrbSel(cPfx, tc.end)},
				cgofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			if isFDBRetryable(goErr) || isFDBRetryable(cErr) {
				return true
			}
			if (goErr == nil) != (cErr == nil) {
				t.Fatalf("%s: error mismatch: go=%v cgo=%v", tc.name, goErr, cErr)
			}
			if goErr != nil {
				// Both errored — codes must agree.
				if fdbErrorCode(goErr) != fdbErrorCode(cErr) {
					t.Fatalf("%s: both errored but codes differ: go=%d (%v) cgo=%d (%v)",
						tc.name, fdbErrorCode(goErr), goErr, fdbErrorCode(cErr), cErr)
				}
				return false
			}
			gp := stripPfx(normGo(goKVs), goPfx)
			cp := stripPfx(normC(cKVs), cPfx)
			if len(gp) != len(cp) {
				t.Fatalf("%s: RYW selector-range count differs: go=%d cgo=%d\n go=%v\ncgo=%v", tc.name, len(gp), len(cp), gp, cp)
			}
			for i := range gp {
				if !bytes.Equal(gp[i].k, cp[i].k) || !bytes.Equal(gp[i].v, cp[i].v) {
					t.Fatalf("%s: RYW pair %d differs: go=(%s,%x) cgo=(%s,%x)", tc.name, i, gp[i].k, gp[i].v, cp[i].k, cp[i].v)
				}
			}
			return false
		}()
		if retry {
			continue
		}
		return
	}
}

func goSelRangeAt(v int64, r gofdb.Range, opts gofdb.RangeOptions) ([]gofdb.KeyValue, error) {
	tr, err := goClient.CreateTransaction()
	if err != nil {
		return nil, err
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	return tr.GetRange(r, opts).GetSliceWithError()
}

func cgoSelRangeAt(v int64, r cgofdb.Range, opts cgofdb.RangeOptions) ([]cgofdb.KeyValue, error) {
	tr, err := cgoClient.CreateTransaction()
	if err != nil {
		return nil, err
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	return tr.GetRange(r, opts).GetSliceWithError()
}
