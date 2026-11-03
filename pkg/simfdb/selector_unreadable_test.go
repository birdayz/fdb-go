package simfdb

import (
	"fmt"
	"strings"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// Selector resolution can demand unreadable data outside the final row extent.
// C++ checks the filled limit and far-bound lookahead before the next iterator type access.
func TestSelectorRange_UnreadableDemand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		begin, end fdb.KeySelector
		unreadable string
		reverse    bool
		limit      int
		want       string
		code       int
		gap        bool
	}{
		{"forward-two-lookahead", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("m"), Offset: -1}, "h", false, 0, "", 1036, false},
		{"forward-two-lookahead-limited", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("m"), Offset: -1}, "h", false, 1, "", 1036, false},
		{"forward-one-lookahead", fdb.FirstGreaterOrEqual(k("b")), fdb.LastLessThan(k("m")), "h", false, 0, "", 1036, false},
		{"forward-one-lookahead-limited", fdb.FirstGreaterOrEqual(k("b")), fdb.LastLessThan(k("m")), "h", false, 1, "b", 0, false},
		{"reverse-two-lookahead", fdb.KeySelector{Key: k("b"), Offset: 4}, fdb.FirstGreaterThan(k("m")), "f", true, 0, "", 1036, false},
		{"reverse-two-lookahead-limited", fdb.KeySelector{Key: k("b"), Offset: 4}, fdb.FirstGreaterThan(k("m")), "f", true, 1, "", 1036, false},
		{"reverse-one-lookahead", fdb.KeySelector{Key: k("b"), Offset: 3}, fdb.FirstGreaterThan(k("m")), "f", true, 0, "", 1036, false},
		{"reverse-one-lookahead-limited", fdb.KeySelector{Key: k("b"), Offset: 3}, fdb.FirstGreaterThan(k("m")), "f", true, 1, "m", 0, false},
		{"cold-crossed-empty", fdb.KeySelector{Key: k("z"), Offset: -1}, fdb.LastLessThan(k("m")), "h", false, 0, "", 1036, false},
		{"cold-crossed-empty-limited", fdb.KeySelector{Key: k("z"), Offset: -1}, fdb.LastLessThan(k("m")), "h", false, 1, "", 1036, false},
		{"excluded-positive-end", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("h"), Offset: 2}, "m", false, 0, "b,f,h", 0, false},
		{"far-positive-end", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("h"), Offset: 2}, "h", false, 0, "", 1036, false},
		{"far-positive-end-limited", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("h"), Offset: 2}, "h", false, 1, "b", 0, false},
		{"or-equal-excludes-anchor", fdb.FirstGreaterThan(k("h")), fdb.KeySelector{Key: k("m"), Offset: 2}, "h", false, 0, "m", 0, false},
		{"rowless-gap-forward", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("m"), Offset: -1}, "g", false, 0, "", 1036, true},
		{"rowless-gap-forward-limited", fdb.FirstGreaterOrEqual(k("b")), fdb.KeySelector{Key: k("m"), Offset: -1}, "g", false, 1, "b", 0, true},
		{"rowless-gap-reverse", fdb.KeySelector{Key: k("b"), Offset: 4}, fdb.FirstGreaterThan(k("m")), "g", true, 0, "", 1036, true},
		{"rowless-gap-reverse-limited", fdb.KeySelector{Key: k("b"), Offset: 4}, fdb.FirstGreaterThan(k("m")), "g", true, 1, "m", 0, true},
	}
	for _, tc := range cases {
		for _, snapshot := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/snapshot=%t", tc.name, snapshot), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				tx := db.newTxn()
				if tc.gap {
					tx.addUnreadableRange([]byte("g"), []byte("g\x00"))
				} else {
					tx.SetVersionstampedValue(k(tc.unreadable), versionstampOperand())
				}
				var reader fdb.ReadTransaction = tx
				if snapshot {
					reader = tx.Snapshot()
				}
				rows, err := reader.GetRange(fdb.SelectorRange{Begin: tc.begin, End: tc.end}, fdb.RangeOptions{Reverse: tc.reverse, Limit: tc.limit}).GetSliceWithError()
				if tc.code != 0 {
					if err == nil || errCode(t, err) != tc.code || len(rows) != 0 {
						t.Fatalf("rows=%s, error=%v; want no rows and %d", rowKeys(rows), err, tc.code)
					}
				} else if err != nil || rowKeys(rows) != tc.want {
					t.Fatalf("rows=%s, error=%v; want %s", rowKeys(rows), err, tc.want)
				}
				if (snapshot || tc.code != 0) && len(tx.readConflicts) != 0 {
					t.Fatalf("unissued/snapshot read recorded %v", conflictStrings(tx))
				}
			})
		}
	}
}

func TestSelectorRange_UnreadableDemandControls(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, control := range []string{"bypass", "ryw-disabled", "clear-set", "sticky-set", "clear"} {
			t.Run(fmt.Sprintf("reverse=%t/%s", reverse, control), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				tx := db.newTxn()
				if control == "bypass" {
					if err := tx.Options().SetBypassUnreadable(); err != nil {
						t.Fatal(err)
					}
				}
				if control == "ryw-disabled" {
					if err := tx.Options().SetReadYourWritesDisable(); err != nil {
						t.Fatal(err)
					}
				}
				unreadable, want := "h", "b"
				r := fdb.SelectorRange{Begin: fdb.FirstGreaterOrEqual(k("b")), End: fdb.KeySelector{Key: k("m"), Offset: -1}}
				if reverse {
					unreadable, want = "f", "m"
					r = fdb.SelectorRange{Begin: fdb.KeySelector{Key: k("b"), Offset: 4}, End: fdb.FirstGreaterThan(k("m"))}
				}
				tx.SetVersionstampedValue(k(unreadable), versionstampOperand())
				if control == "clear" || control == "clear-set" {
					tx.Clear(k(unreadable))
				}
				if control == "clear-set" || control == "sticky-set" {
					tx.Set(k(unreadable), []byte("v"))
				}
				if control == "clear" {
					want = ""
				}
				rows, err := tx.GetRange(r, fdb.RangeOptions{Reverse: reverse}).GetSliceWithError()
				if control == "sticky-set" {
					if err == nil || errCode(t, err) != 1036 {
						t.Fatalf("sticky unreadable read = %s, %v; want 1036", rowKeys(rows), err)
					}
				} else if err != nil || rowKeys(rows) != want {
					t.Fatalf("rows=%s, error=%v; want %s", rowKeys(rows), err, want)
				}
			})
		}
	}
}

func TestSelectorRange_UnreadableByteLimitedBatch(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			large := strings.Repeat("v", 8192)
			seed(db, "b", large, "f", large, "h", large, "m", large)
			tx := db.newTxn()
			unreadable, want := "h", "b"
			r := fdb.SelectorRange{Begin: fdb.FirstGreaterOrEqual(k("b")), End: fdb.LastLessThan(k("m"))}
			if reverse {
				unreadable, want = "f", "m"
				r = fdb.SelectorRange{Begin: fdb.KeySelector{Key: k("b"), Offset: 3}, End: fdb.FirstGreaterThan(k("m"))}
			}
			tx.SetVersionstampedValue(k(unreadable), versionstampOperand())
			rr := tx.GetRange(r, fdb.RangeOptions{Reverse: reverse, Mode: fdb.StreamingModeIterator}).(*simRangeResult)
			if rr.first.err != nil || !rr.first.more || rowKeys(rr.first.batch) != want {
				t.Fatalf("first byte-limited batch = %+v, want %s with more", rr.first, want)
			}
			if rows, err := rr.GetSliceWithError(); err == nil || errCode(t, err) != 1036 || len(rows) != 0 {
				t.Fatalf("continued byte-limited read = %s, %v; want 1036", rowKeys(rows), err)
			}
		})
	}
}
