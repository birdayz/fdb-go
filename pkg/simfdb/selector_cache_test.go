package simfdb

import (
	"bytes"
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// Prior reads reduce the crossed bound before the nested GetKey decision (RYW:611-626,914-941).
func TestSelectorRange_KnownReadReduction(t *testing.T) {
	t.Parallel()
	warmups := []struct {
		name  string
		known bool
	}{
		{"cold", false},
		{"point", true},
		{"absent-point", false},
		{"unrelated-point", false},
		{"point-after-m", false},
		{"range-forward", true},
		{"range-reverse", true},
		{"partial-forward", false},
		{"partial-reverse", true},
		{"getkey-forward", true},
		{"getkey-backward", true},
		{"dependent-atomic", true},
	}
	for _, reverse := range []bool{false, true} {
		for _, warmup := range warmups {
			for _, probe := range []string{"zz", "k"} {
				t.Run(fmt.Sprintf("reverse=%t/%s/probe=%s", reverse, warmup.name, probe), func(t *testing.T) {
					t.Parallel()
					db := New(nil)
					seedKeys(t, db, "b", "f", "h", "m")
					tx := db.newTxn()
					snapshot := tx.Snapshot()
					switch warmup.name {
					case "point", "dependent-atomic":
						if value, err := snapshot.Get(k("m")).Get(); err != nil || string(value) != "v" {
							t.Fatalf("warm point = %q, %v", value, err)
						}
						if warmup.name == "dependent-atomic" {
							tx.Add(k("m"), []byte{1})
						}
					case "absent-point", "point-after-m":
						key := "n"
						if warmup.name == "point-after-m" {
							key = "m\x00"
						}
						if value, err := snapshot.Get(k(key)).Get(); err != nil || value != nil {
							t.Fatalf("absent warm point = %q, %v", value, err)
						}
					case "unrelated-point":
						if _, err := snapshot.Get(k("b")).Get(); err != nil {
							t.Fatal(err)
						}
					case "getkey-forward", "getkey-backward":
						sel := fdb.FirstGreaterOrEqual(k("m"))
						if warmup.name == "getkey-backward" {
							sel = fdb.LastLessThan(k("n"))
						}
						if key, err := snapshot.GetKey(sel).Get(); err != nil || string(key) != "m" {
							t.Fatalf("warm GetKey = %q, %v", key, err)
						}
					case "range-forward", "range-reverse", "partial-forward", "partial-reverse":
						opts := fdb.RangeOptions{Reverse: warmup.name == "range-reverse" || warmup.name == "partial-reverse"}
						want := "h,m"
						if opts.Reverse {
							want = "m,h"
						}
						if warmup.name == "partial-forward" || warmup.name == "partial-reverse" {
							opts.Limit = 1
							want = "h"
							if opts.Reverse {
								want = "m"
							}
						}
						rows, err := snapshot.GetRange(fdb.KeyRange{Begin: k("h"), End: k("z")}, opts).GetSliceWithError()
						if err != nil || rowKeys(rows) != want {
							t.Fatalf("warm range = %s, %v; want %s", rowKeys(rows), err, want)
						}
					}
					if len(tx.readConflicts) != 0 {
						t.Fatalf("snapshot warmup recorded %v", conflictStrings(tx))
					}
					r := fdb.SelectorRange{Begin: fdb.KeySelector{Key: k("z"), Offset: -2}, End: fdb.KeySelector{Key: k("m"), Offset: 2}}
					wantRows := "f,h,m"
					if reverse {
						r = fdb.SelectorRange{Begin: fdb.KeySelector{Key: k("m"), Offset: 2}, End: fdb.KeySelector{Key: k("h"), Offset: 3}}
						wantRows = ""
					}
					rows, err := tx.GetRange(r, fdb.RangeOptions{Reverse: reverse}).GetSliceWithError()
					if err != nil || rowKeys(rows) != wantRows {
						t.Fatalf("selector rows = %s, %v; want %s", rowKeys(rows), err, wantRows)
					}
					wantConflict := probe == "k" || !warmup.known
					if got := concurrentWriteConflicts(t, db, tx, probe); got != wantConflict {
						t.Fatalf("conflicted=%t, want %t; ranges %v", got, wantConflict, conflictStrings(tx))
					}
				})
			}
		}
	}
}

// A matched CompareAndClear remains a KV operation slot even when kv() is null (RYWIterator.cpp:25-52,78-88).
// Reducing that slot avoids readThroughEnd on crossed bounds (ReadYourWrites.actor.cpp:437-442,611-618).
func TestSelectorRange_MatchedCompareAndClearOperationSlot(t *testing.T) {
	t.Parallel()
	for _, preparation := range []string{"set", "snapshot-get"} {
		for _, probe := range []string{"n", "z", "m"} {
			t.Run(preparation+"/probe="+probe, func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m", "n")
				tx := db.newTxn()
				wantBegin := "m\x00"
				if preparation == "set" {
					tx.Set(k("m"), []byte("v"))
				} else {
					if value, err := tx.Snapshot().Get(k("m")).Get(); err != nil || string(value) != "v" {
						t.Fatalf("snapshot Get(m) = %q, %v", value, err)
					}
					wantBegin = "m"
				}
				tx.CompareAndClear(k("m"), []byte("v"))
				r := fdb.SelectorRange{
					Begin: fdb.FirstGreaterOrEqual(k("z")),
					End:   fdb.KeySelector{Key: k("m"), Offset: 2},
				}
				rows, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil || len(rows) != 0 {
					t.Fatalf("crossed GetRange after matched CompareAndClear = %v, %v; want empty", rows, err)
				}
				if len(tx.readConflicts) != 1 || !bytes.Equal(tx.readConflicts[0].begin, []byte(wantBegin)) || string(tx.readConflicts[0].end) != "z" {
					t.Fatalf("read conflicts = %v, want [%q,%q)", conflictStrings(tx), wantBegin, "z")
				}
				wantConflict := probe == "n" || probe == "m" && preparation == "snapshot-get"
				if got := concurrentWriteConflicts(t, db, tx, probe); got != wantConflict {
					t.Fatalf("writer at %q: reader conflicted=%t, want %t", probe, got, wantConflict)
				}
			})
		}
	}
}

// Bounded native demand clears boundary flags when selector offsets cross (RYW:653-655,957-959).
func TestSelectorRange_BoundedOvershootConflicts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		r          fdb.SelectorRange
		reverse    bool
		begin, end string
		probes     []string
	}{
		{
			name: "forward",
			r: fdb.SelectorRange{
				Begin: fdb.KeySelector{Key: k("m"), Offset: 3},
				End:   fdb.FirstGreaterOrEqual(k("z")),
			},
			begin: "m", end: "z", probes: []string{"m", "n", "z", "zz"},
		},
		{
			name: "reverse",
			r: fdb.SelectorRange{
				Begin: fdb.KeySelector{Key: k("h"), Offset: 0},
				End:   fdb.KeySelector{Key: k("m"), Offset: -4},
			},
			reverse: true, begin: "h", end: "m", probes: []string{"a", "h", "k", "m"},
		},
	}
	for _, tc := range cases {
		for _, limit := range []int{0, 1} {
			for _, probe := range tc.probes {
				t.Run(fmt.Sprintf("%s/limit=%d/probe=%s", tc.name, limit, probe), func(t *testing.T) {
					t.Parallel()
					db := New(nil)
					seedKeys(t, db, "b", "f", "h", "m")
					tx := db.newTxn()
					rows, err := tx.GetRange(tc.r, fdb.RangeOptions{Limit: limit, Reverse: tc.reverse}).GetSliceWithError()
					if err != nil || len(rows) != 0 {
						t.Fatalf("bounded overshoot rows = %v, %v; want empty", rows, err)
					}
					if len(tx.readConflicts) != 1 || string(tx.readConflicts[0].begin) != tc.begin || string(tx.readConflicts[0].end) != tc.end {
						t.Fatalf("read conflicts = %v, want [%q,%q)", conflictStrings(tx), tc.begin, tc.end)
					}
					wantConflict := probe >= tc.begin && probe < tc.end
					if got := concurrentWriteConflicts(t, db, tx, probe); got != wantConflict {
						t.Fatalf("writer at %q: reader conflicted=%t, want %t", probe, got, wantConflict)
					}
				})
			}
		}
	}
}

func TestKnownReadKnowledgeLifecycle(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"Reset", "OnError", "Commit", "SetReadVersion", "RYWDisabled"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seedKeys(t, db, "b", "f", "h", "m")
			tx := db.newTxn()
			if boundary == "RYWDisabled" {
				if err := tx.Options().SetReadYourWritesDisable(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Snapshot().Get(k("m")).Get(); err != nil {
				t.Fatal(err)
			}
			if boundary != "RYWDisabled" && !tx.readKnown([]byte("m")) {
				t.Fatal("point read did not establish the lifecycle test's premise")
			}
			switch boundary {
			case "Reset":
				tx.Reset()
			case "OnError":
				if err := tx.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
					t.Fatal(err)
				}
			case "Commit":
				tx.Set(k("a"), []byte("v"))
				if err := tx.Commit().Get(); err != nil {
					t.Fatal(err)
				}
			case "SetReadVersion":
				version := tx.readVersion + 1
				tx.Reset()
				tx.SetReadVersion(version)
				if got, err := tx.GetReadVersion().Get(); err != nil || got != version {
					t.Fatalf("fresh pin = %d, %v; want %d, nil", got, err, version)
				}
			}
			if len(tx.knownReads) != 0 {
				t.Fatalf("retained prior-read knowledge: %v", tx.knownReads)
			}
		})
	}
}

func TestKnownReadBatchBounds(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, byteLimited := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/byteLimited=%t", reverse, byteLimited), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seed(db, "b", fatValue("b"), "f", fatValue("f"), "h", fatValue("h"), "m", fatValue("m"))
				tx := db.newTxn()
				opts := fdb.RangeOptions{Reverse: reverse, Limit: 1}
				if byteLimited {
					opts = fdb.RangeOptions{Reverse: reverse, Mode: fdb.StreamingModeIterator}
				}
				rr := tx.Snapshot().GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, opts).(*simRangeResult)
				if rr.first.err != nil || !rr.first.more || len(rr.first.batch) == 0 || len(rr.first.batch) >= 4 {
					t.Fatalf("warm first batch = %+v; want a nonempty partial fetch", rr.first)
				}
				last := rr.first.batch[len(rr.first.batch)-1].Key
				begin, end := []byte("a"), keyAfter(last)
				if reverse {
					begin, end = last, []byte("z")
				}
				if len(tx.knownReads) != 1 || !bytes.Equal(tx.knownReads[0].begin, begin) || !bytes.Equal(tx.knownReads[0].end, end) {
					t.Fatalf("known = %v, want [%q,%q) before consuming the iterator", tx.knownReads, begin, end)
				}
				if tx.readKnown(end) || !tx.readKnown(begin) {
					t.Fatal("known batch boundaries are not half-open")
				}
				if reverse && tx.readKnown([]byte("a")) || !reverse && tx.readKnown([]byte("y")) {
					t.Fatal("abandoned warmup learned its unfetched tail")
				}
			})
		}
	}
}

func TestKnownReadSegmentsPreserveGapsAndWrites(t *testing.T) {
	t.Parallel()
	for _, adjacent := range []bool{false, true} {
		t.Run(fmt.Sprintf("adjacent=%t", adjacent), func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seedKeys(t, db, "h", "m")
			tx := db.newTxn()
			tx.ensureReadVersion()
			tx.rememberRead([]byte("h"), []byte("h\x00"))
			tx.rememberRead([]byte("m"), []byte("n"))
			if adjacent {
				tx.rememberRead([]byte("h\x00"), []byte("m"))
			}
			sel := fdb.KeySelector{Key: k("h"), Offset: 3}
			reduced := reduceSelectorFromWrites(sel, tx.selectorWriteSegments(tx.buildView()))
			wantKey, wantOffset := "h\x00", 2
			if adjacent {
				wantKey, wantOffset = "n", 1
			}
			if string(reduced.Key.FDBKey()) != wantKey || reduced.Offset != wantOffset {
				t.Fatalf("reduced = %+v, want (%q,%d)", reduced, wantKey, wantOffset)
			}
			tx.Clear(k("m"))
			cleared := reduceSelectorFromWrites(fdb.KeySelector{Key: k("m"), Offset: 2}, tx.selectorWriteSegments(tx.buildView()))
			if cleared.Offset != 2 || string(cleared.Key.FDBKey()) != "n" {
				t.Fatalf("cached present row survived Clear: %+v", cleared)
			}
		})
	}
}
