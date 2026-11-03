package simfdb

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// rowKeys renders the rows' keys comma-separated.
func rowKeys(kvs []fdb.KeyValue) string {
	keys := make([]string, len(kvs))
	for i, kv := range kvs {
		keys[i] = string(kv.Key)
	}
	return strings.Join(keys, ",")
}

// concurrentWriteConflicts writes probes in another transaction, then commits tx and reports
// whether the commit failed with not_committed.
func concurrentWriteConflicts(t *testing.T, db *SimDB, tx *simTxn, probes ...string) bool {
	t.Helper()
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		for _, p := range probes {
			w.Set(k(p), []byte("concurrent"))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	tx.Set(k("sentinel"), []byte("1"))
	if err := tx.Commit().Get(); err != nil {
		if code := errCode(t, err); code != 1020 {
			t.Fatalf("commit error %d, want 1020 or success", code)
		}
		return true
	}
	return false
}

// TestSelectorRange_ContinuationAcrossBatches drains lastLessOrEqual(r02)..firstGreaterThan(r55)+1
// (rows r02..r56 of r00..r59) across several batches. Each continuation re-resolves the selectors
// with FirstGreaterThan(last) forward or FirstGreaterOrEqual(last) in reverse
// (bindings/go/src/fdb/range.go:229-233), and each batch records addConflictRange(GetRangeReq)
// (ReadYourWrites.actor.cpp:245-319): forward [previous batch's end, keyAfter(last row)), the
// first from r02; reverse [last row, previous batch's begin), the first up to keyAfter(r56).
func TestSelectorRange_ContinuationAcrossBatches(t *testing.T) {
	t.Parallel()
	var kv, want []string
	for i := 0; i < 60; i++ {
		key := fmt.Sprintf("r%02d", i)
		kv = append(kv, key, fatValue(key))
		if i >= 2 && i <= 56 {
			want = append(want, key)
		}
	}
	r := fdb.SelectorRange{Begin: fdb.LastLessOrEqual(k("r02")), End: fdb.KeySelector{Key: k("r55"), OrEqual: true, Offset: 2}}
	for _, reverse := range []bool{false, true} {
		for _, consumer := range []string{"GetSliceWithError", "Iterator"} {
			t.Run(fmt.Sprintf("reverse=%t/%s", reverse, consumer), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seed(db, kv...)
				tx := db.newTxn()
				// ITERATOR's first batch targets 4096 bytes: two ~2 KB rows.
				rr := tx.GetRange(r, fdb.RangeOptions{Reverse: reverse, Mode: fdb.StreamingModeIterator})
				var kvs []fdb.KeyValue
				if consumer == "GetSliceWithError" {
					var err error
					if kvs, err = rr.GetSliceWithError(); err != nil {
						t.Fatal(err)
					}
				} else {
					for it := rr.Iterator(); it.Advance(); {
						row, err := it.Get()
						if err != nil {
							t.Fatal(err)
						}
						kvs = append(kvs, row)
					}
				}
				wantRows := append([]string(nil), want...)
				if reverse {
					for i, j := 0, len(wantRows)-1; i < j; i, j = i+1, j-1 {
						wantRows[i], wantRows[j] = wantRows[j], wantRows[i]
					}
				}
				if got := rowKeys(kvs); got != strings.Join(wantRows, ",") {
					t.Fatalf("rows %s\nwant %s", got, strings.Join(wantRows, ","))
				}
				// GetSliceWithError reads the rest in one more, unlimited pass (fdb/range_result.go);
				// the iterator's byte targets grow per batch.
				conflicts, minBatches := tx.readConflicts, 3
				if consumer == "GetSliceWithError" {
					minBatches = 2
				}
				if len(conflicts) < minBatches {
					t.Fatalf("%d batches %v, want at least %d", len(conflicts), conflictStrings(tx), minBatches)
				}
				// Each batch's range starts (forward) or ends (reverse) where the previous one
				// stopped, and stops at a row it returned.
				rows := map[string]bool{}
				for _, key := range want {
					rows[key] = true
				}
				for i, c := range conflicts {
					var from, to []byte
					switch {
					case !reverse && i == 0:
						from = []byte("r02")
					case !reverse:
						from = conflicts[i-1].end
					case i == 0:
						to = keyAfter([]byte("r56"))
					default:
						to = conflicts[i-1].begin
					}
					if !reverse && (!bytes.Equal(c.begin, from) || !rows[strings.TrimSuffix(string(c.end), "\x00")] || !bytes.HasSuffix(c.end, []byte{0})) ||
						reverse && (!bytes.Equal(c.end, to) || !rows[string(c.begin)]) {
						t.Fatalf("batch %d conflict [%q,%q) breaks the per-batch spans %v", i, c.begin, c.end, conflictStrings(tx))
					}
				}
				first, last := conflicts[0], conflicts[len(conflicts)-1]
				if !reverse && string(last.end) != "r56\x00" || reverse && string(last.begin) != "r02" {
					t.Fatalf("batches end at [%q,%q), want the read's far end; spans %v", first.begin, last.end, conflictStrings(tx))
				}
			})
		}
	}
}

// TestSelectorRange_SnapshotRecordsNoConflict pins that a snapshot selector read, BunchedMap's
// shape among them, adds no read conflict (ReadYourWrites.actor.cpp:387; NativeAPI.actor.cpp:5841-5844),
// including the RYW-disabled readThrough getKey of a reverse end with offset > 1 (RYW:191-199).
func TestSelectorRange_SnapshotRecordsNoConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		r       fdb.SelectorRange
		reverse bool
		rows    string
	}{
		{"forward", fdb.SelectorRange{Begin: fdb.LastLessOrEqual(k("f")), End: fdb.KeySelector{Key: k("f"), OrEqual: true, Offset: 2}}, false, "f,h"},
		{"reverse", fdb.SelectorRange{Begin: fdb.FirstGreaterOrEqual(k("c")), End: fdb.KeySelector{Key: k("f"), Offset: 2}}, true, "f"},
	}
	for _, tc := range cases {
		for _, rywDisabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rywDisabled=%t", tc.name, rywDisabled), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				tx := db.newTxn()
				if rywDisabled {
					if err := tx.Options().SetReadYourWritesDisable(); err != nil {
						t.Fatal(err)
					}
				}
				kvs, err := tx.Snapshot().GetRange(tc.r, fdb.RangeOptions{Reverse: tc.reverse}).GetSliceWithError()
				if err != nil {
					t.Fatal(err)
				}
				if got := rowKeys(kvs); got != tc.rows {
					t.Fatalf("rows %q, want %q", got, tc.rows)
				}
				if len(tx.readConflicts) != 0 {
					t.Fatalf("snapshot selector read recorded %v", conflictStrings(tx))
				}
				if concurrentWriteConflicts(t, db, tx, "c", "f", "g", "h") {
					t.Fatal("a write inside a snapshot selector read's span made the commit conflict")
				}
			})
		}
	}
}

// TestSelectorRange_ReadToBeginAndThroughEnd pins the widening when a selector resolves before the
// first key (readToBegin: the conflict reaches the empty key) or past the last (readThroughEnd:
// it reaches the end marker): RYW:458-470, 799-811, 1111-1120; RYW disabled NAPI:4482-4483 via
// readToBegin. Storage holds b, f, h, m; "a" lies below every key and "z" above.
func TestSelectorRange_ReadToBeginAndThroughEnd(t *testing.T) {
	t.Parallel()
	toBegin := fdb.SelectorRange{Begin: fdb.LastLessThan(k("b")), End: fdb.FirstGreaterOrEqual(k("f"))}
	throughEnd := fdb.SelectorRange{Begin: fdb.FirstGreaterOrEqual(k("h")), End: fdb.KeySelector{Key: k("m"), Offset: 3}}
	cases := []struct {
		name        string
		r           fdb.SelectorRange
		reverse     bool
		rywDisabled bool
		rows        string
		conflicts   map[string]bool
	}{
		{"to-begin/forward/ryw", toBegin, false, false, "b", map[string]bool{"a": true, "g": false}},
		{"to-begin/reverse/ryw", toBegin, true, false, "b", map[string]bool{"a": true, "g": false}},
		{"to-begin/forward/ryw-disabled", toBegin, false, true, "b", map[string]bool{"a": true, "g": false}},
		{"to-begin/reverse/ryw-disabled", toBegin, true, true, "b", map[string]bool{"a": true, "g": false}},
		{"through-end/forward/ryw", throughEnd, false, false, "h,m", map[string]bool{"z": true, "g": false}},
		{"through-end/reverse/ryw", throughEnd, true, false, "m,h", map[string]bool{"z": true, "g": false}},
	}
	for _, tc := range cases {
		for probe, want := range tc.conflicts {
			t.Run(fmt.Sprintf("%s/probe=%s", tc.name, probe), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				tx := db.newTxn()
				if tc.rywDisabled {
					if err := tx.Options().SetReadYourWritesDisable(); err != nil {
						t.Fatal(err)
					}
				}
				kvs, err := tx.GetRange(tc.r, fdb.RangeOptions{Reverse: tc.reverse}).GetSliceWithError()
				if err != nil {
					t.Fatal(err)
				}
				if got := rowKeys(kvs); got != tc.rows {
					t.Fatalf("rows %q, want %q", got, tc.rows)
				}
				if got := concurrentWriteConflicts(t, db, tx, probe); got != want {
					t.Fatalf("write at %q: conflicted=%t, want %t (recorded %v)", probe, got, want, conflictStrings(tx))
				}
			})
		}
	}
}

// Crossed anchors trigger a nested getKey in the RYW prologue (:621-638, 925-942).
// Resolving exactly one past the last row returns maxKey and sets readThroughEnd.
func TestSelectorRange_CrossedBoundsReadThroughEnd(t *testing.T) {
	t.Parallel()
	forward := fdb.SelectorRange{Begin: fdb.KeySelector{Key: k("z"), Offset: -2}, End: fdb.KeySelector{Key: k("m"), Offset: 2}}
	reverse := fdb.SelectorRange{Begin: fdb.KeySelector{Key: k("m"), Offset: 2}, End: fdb.KeySelector{Key: k("h"), Offset: 3}}
	clearSetM := func(tx *simTxn) {
		tx.Clear(k("m"))
		tx.Set(k("m"), []byte("v"))
	}
	cases := []struct {
		name         string
		r            fdb.SelectorRange
		options      fdb.RangeOptions
		rows         string
		rywTail      bool
		disabledTail bool
		prepare      func(*simTxn)
	}{
		{"forward", forward, fdb.RangeOptions{}, "f,h,m", true, false, nil},
		{"reverse", reverse, fdb.RangeOptions{Reverse: true}, "", true, true, nil},
		{"forward-limited", forward, fdb.RangeOptions{Limit: 1}, "f", false, false, nil},
		{"uncrossed", fdb.SelectorRange{Begin: fdb.FirstGreaterOrEqual(k("h")), End: fdb.KeySelector{Key: k("m"), Offset: 2}}, fdb.RangeOptions{}, "h,m", false, false, nil},
		{"forward/local-m", forward, fdb.RangeOptions{}, "f,h,m", false, false, func(tx *simTxn) { tx.Set(k("m"), []byte("v")) }},
		{"reverse/local-m", reverse, fdb.RangeOptions{Reverse: true}, "", false, true, func(tx *simTxn) { tx.Set(k("m"), []byte("v")) }},
		{"forward/unrelated-set", forward, fdb.RangeOptions{}, "f,h,m", true, false, func(tx *simTxn) { tx.Set(k("b"), []byte("v")) }},
		{"reverse/unrelated-set", reverse, fdb.RangeOptions{Reverse: true}, "", true, true, func(tx *simTxn) { tx.Set(k("b"), []byte("v")) }},
		{"forward/dependent-m", forward, fdb.RangeOptions{}, "f,h,m", true, false, func(tx *simTxn) { tx.Add(k("m"), []byte{1}) }},
		{"reverse/dependent-m", reverse, fdb.RangeOptions{Reverse: true}, "", true, true, func(tx *simTxn) { tx.Add(k("m"), []byte{1}) }},
		{"forward/clear-set-m", forward, fdb.RangeOptions{}, "f,h,m", false, false, clearSetM},
		{"reverse/clear-set-m", reverse, fdb.RangeOptions{Reverse: true}, "", false, true, clearSetM},
	}
	for _, tc := range cases {
		for _, rywDisabled := range []bool{false, true} {
			for _, snapshot := range []bool{false, true} {
				for _, probe := range []string{"zz", "a"} {
					t.Run(fmt.Sprintf("%s/rywDisabled=%t/snapshot=%t/probe=%s", tc.name, rywDisabled, snapshot, probe), func(t *testing.T) {
						t.Parallel()
						db := New(nil)
						seedKeys(t, db, "b", "f", "h", "m")
						tx := db.newTxn()
						if rywDisabled {
							if err := tx.Options().SetReadYourWritesDisable(); err != nil {
								t.Fatal(err)
							}
						}
						if tc.prepare != nil {
							tc.prepare(tx)
						}
						var reader fdb.ReadTransaction = tx
						if snapshot {
							reader = tx.Snapshot()
						}
						kvs, err := reader.GetRange(tc.r, tc.options).GetSliceWithError()
						if err != nil {
							t.Fatal(err)
						}
						if got := rowKeys(kvs); got != tc.rows {
							t.Fatalf("rows %q, want %q", got, tc.rows)
						}
						if snapshot && len(tx.readConflicts) != 0 {
							t.Fatalf("snapshot recorded %v", conflictStrings(tx))
						}
						tail := tc.rywTail
						if rywDisabled {
							// NativeAPI lacks the RYW prologue; only reverse readThrough's
							// getKey of the end selector protects the user-keyspace tail.
							tail = tc.disabledTail
						}
						want := probe == "zz" && tail && !snapshot
						if got := concurrentWriteConflicts(t, db, tx, probe); got != want {
							t.Fatalf("write at %q: conflicted=%t, want %t (recorded %v)", probe, got, want, conflictStrings(tx))
						}
					})
				}
			}
		}
	}
}

func TestSelectorRange_LocalWriteReduction(t *testing.T) {
	t.Parallel()
	setM := func(tx *simTxn) { tx.Set(k("m"), []byte("v")) }
	clearM := func(tx *simTxn) { tx.Clear(k("m")) }
	cases := []struct {
		name       string
		prepare    func(*simTxn)
		anchor     string
		offset     int
		wantKey    string
		wantOffset int
	}{
		{"storage-unknown", nil, "m", 2, "m", 2},
		{"local-forward", setM, "m", 2, "m\x00", 1},
		{"local-backward", setM, "m\x00", 0, "m", 1},
		{"local-trivial", setM, "m", 1, "m", 1},
		{"trivial-at-max", setM, "\xff", 1, "\xff", 1},
		{"backward-boundary", setM, "m", 0, "m", 0},
		{"clear-forward", clearM, "m", 2, "m\x00", 2},
		{"clear-backward", clearM, "m\x00", 0, "m", 0},
		{"clear-trivial", clearM, "m", 1, "m\x00", 1},
		{"set-clear", func(tx *simTxn) {
			setM(tx)
			clearM(tx)
		}, "m", 2, "m\x00", 2},
		{"clear-set", func(tx *simTxn) {
			clearM(tx)
			setM(tx)
		}, "m", 2, "m\x00", 1},
		{"dependent-atomic", func(tx *simTxn) {
			tx.Add(k("m"), []byte{1})
		}, "m", 2, "m", 2},
		{"atomic-over-set", func(tx *simTxn) {
			setM(tx)
			tx.Add(k("m"), []byte{1})
		}, "m", 2, "m\x00", 1},
		{"atomic-over-clear", func(tx *simTxn) {
			clearM(tx)
			tx.Add(k("m"), []byte{1})
		}, "m", 2, "m\x00", 1},
		{"known-absent-operation", func(tx *simTxn) {
			setM(tx)
			tx.CompareAndClear(k("m"), []byte("v"))
		}, "m", 2, "m\x00", 2},
		{"unreadable-key", func(tx *simTxn) {
			tx.SetVersionstampedValue(k("m"), versionstampOperand())
			setM(tx)
		}, "m", 2, "m", 2},
		{"unreadable-range", func(tx *simTxn) {
			tx.ClearRange(fdb.KeyRange{Begin: k(""), End: endKeyMarker})
			tx.addUnreadableRange([]byte("m"), []byte("n"))
		}, "l", 2, "m", 2},
		{"known-left-end", func(tx *simTxn) {
			tx.ClearRange(fdb.KeyRange{Begin: k(""), End: k("m")})
		}, "b", 0, "", 1},
		{"known-right-end", func(tx *simTxn) {
			tx.ClearRange(fdb.KeyRange{Begin: k("m"), End: endKeyMarker})
		}, "m", 2, "\xff", 1},
		{"backward-from-max", func(tx *simTxn) {
			tx.ClearRange(fdb.KeyRange{Begin: k("m"), End: endKeyMarker})
		}, "\xff", 0, "m", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seedKeys(t, db, "b", "f", "h", "m")
			tx := db.newTxn()
			if tc.prepare != nil {
				tc.prepare(tx)
			}
			tx.ensureReadVersion()
			segments := tx.selectorWriteSegments(tx.buildView())
			got := reduceSelectorFromWrites(fdb.KeySelector{Key: k(tc.anchor), Offset: tc.offset}, segments)
			if string(got.Key.FDBKey()) != tc.wantKey || got.Offset != tc.wantOffset || got.OrEqual {
				t.Fatalf("reduced selector = %q/%d/orEqual=%t, want %q/%d/orEqual=false", got.Key.FDBKey(), got.Offset, got.OrEqual, tc.wantKey, tc.wantOffset)
			}
		})
	}
}

// Inverted nontrivial selectors return before conflict registration (RYW:1758-1760,
// NAPI:5830-5832), even though their anchors enclose concurrently writable keys.
func TestSelectorRange_InvertedRecordsNoConflict(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, rywDisabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/rywDisabled=%t", reverse, rywDisabled), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				tx := db.newTxn()
				if rywDisabled {
					if err := tx.Options().SetReadYourWritesDisable(); err != nil {
						t.Fatal(err)
					}
				}
				r := fdb.SelectorRange{Begin: fdb.LastLessThan(k("m")), End: fdb.LastLessThan(k("b"))}
				rows, err := tx.GetRange(r, fdb.RangeOptions{Reverse: reverse}).GetSliceWithError()
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 0 {
					t.Fatalf("inverted selector range returned %q, want no rows", rowKeys(rows))
				}
				if len(tx.readConflicts) != 0 {
					t.Fatalf("inverted selector range recorded %v", conflictStrings(tx))
				}
				if concurrentWriteConflicts(t, db, tx, "f") {
					t.Fatal("inverted selector read conflicted with a write between its anchors")
				}
			})
		}
	}
}

// TestSelectorRange_CancelAndLimitChecks pins the selector route's checks in RYW getRange's order,
// cancelled (1025) then a limit below -1 (2012) (ReadYourWrites.actor.cpp:1736-1750), on both
// consumers, and a Cancel between batches failing the next one.
func TestSelectorRange_CancelAndLimitChecks(t *testing.T) {
	t.Parallel()
	r := fdb.SelectorRange{Begin: fdb.LastLessOrEqual(k("f")), End: fdb.KeySelector{Key: k("f"), OrEqual: true, Offset: 2}}
	for _, tc := range []struct {
		name   string
		cancel bool
		limit  int
		want   int
	}{
		{"limit -2", false, -2, 2012},
		{"cancelled", true, 0, 1025},
		{"cancelled with limit -2", true, -2, 1025},
	} {
		for _, snapshot := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/snapshot=%t", tc.name, snapshot), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				tx := db.newTxn()
				if tc.cancel {
					tx.Cancel()
				}
				var rt fdb.ReadTransaction = tx
				if snapshot {
					rt = tx.Snapshot()
				}
				kvs, err := rt.GetRange(r, fdb.RangeOptions{Limit: tc.limit}).GetSliceWithError()
				if code := errCode(t, err); code != tc.want {
					t.Fatalf("GetSliceWithError: %d rows, code %d, want %d", len(kvs), code, tc.want)
				}
				it := rt.GetRange(r, fdb.RangeOptions{Limit: tc.limit}).Iterator()
				if it.Advance() {
					t.Fatal("iterator advanced")
				}
				if _, err := it.Get(); errCode(t, err) != tc.want {
					t.Fatalf("iterator: code %d, want %d", errCode(t, err), tc.want)
				}
			})
		}
	}
	// With c and d cleared locally the end, lastLessOrEqual(d), resolves to b: the next batch's span
	// is empty and reads no storage, so only the read's own cancelled check can fail it.
	for _, clearRest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel between batches/clearRest=%t", clearRest), func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seed(db, "a", fatValue("1"), "b", fatValue("2"), "c", fatValue("3"), "d", fatValue("4"))
			tx := db.newTxn()
			it := tx.GetRange(fdb.SelectorRange{Begin: fdb.LastLessOrEqual(k("a")), End: fdb.LastLessOrEqual(k("d"))},
				fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
			for i := 0; i < 2; i++ { // batch 1: two ~2 KB rows
				if !it.Advance() {
					t.Fatalf("batch 1 ended after %d rows", i)
				}
			}
			if clearRest {
				tx.ClearRange(fdb.KeyRange{Begin: k("c"), End: k("z")})
			}
			tx.Cancel()
			if it.Advance() {
				t.Fatal("iterator read a batch after Cancel")
			}
			if _, err := it.Get(); errCode(t, err) != 1025 {
				t.Fatalf("next batch after Cancel: code %d, want 1025", errCode(t, err))
			}
		})
	}
}
