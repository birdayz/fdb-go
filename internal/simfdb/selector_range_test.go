package simfdb

import (
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestSelectorRange_ConflictPerRoute pins which concurrent writes a selector range read
// conflicts with, by route: RYW (addConflictRange(GetRangeReq), ReadYourWrites.actor.cpp:245-319)
// and RYW disabled (readThrough, RYW:187-215, then getRangeFinished, NativeAPI.actor.cpp:4558-4586).
// Every expectation is derived by hand from the C++ for storage b, f, h, m:
//
// forward lastLessThan(g) .. firstGreaterOrEqual(h)+1 returns f, h on both routes.
//   - RYW: g < h gives [g, h); begin.offset <= 0 lowers it to the first row f; h <= the last
//     row raises the end to keyAfter(h): [f, h\x00).
//   - RYW disabled: begin.offset is 0, so the begin is the first row f; end.offset 2 > 1 with
//     rows makes the end keyAfter(last) = h\x00: [f, h\x00).
//
// reverse firstGreaterOrEqual(c) .. firstGreaterOrEqual(f)+1 returns f on both routes.
//   - RYW: c < f gives [c, f) (more is false); end.offset > 0 and f <= the first row raises
//     the end to f\x00: [c, f\x00).
//   - RYW disabled: readThrough resolves the end (offset 2) with getKey to h, adding getKey's
//     [f, h\x00), and reads [FGE(c), FGE(h)) with getRangeFinished's [c, h): [c, h\x00).
func TestSelectorRange_ConflictPerRoute(t *testing.T) {
	t.Parallel()
	forward := fdb.SelectorRange{
		Begin: fdb.LastLessThan(k("g")),
		End:   fdb.KeySelector{Key: k("h"), Offset: 2},
	}
	reverse := fdb.SelectorRange{
		Begin: fdb.FirstGreaterOrEqual(k("c")),
		End:   fdb.KeySelector{Key: k("f"), Offset: 2},
	}
	cases := []struct {
		name        string
		r           fdb.SelectorRange
		reverse     bool
		rywDisabled bool
		rows        string
		conflicts   map[string]bool // probe key -> conflicts
	}{
		{"forward/ryw", forward, false, false, "f,h", map[string]bool{"e": false, "f": true, "g": true, "h": true, "k": false}},
		{"forward/ryw-disabled", forward, false, true, "f,h", map[string]bool{"e": false, "f": true, "g": true, "h": true, "k": false}},
		{"reverse/ryw", reverse, true, false, "f", map[string]bool{"b": false, "c": true, "f": true, "g": false, "h": false}},
		{"reverse/ryw-disabled", reverse, true, true, "f", map[string]bool{"b": false, "c": true, "f": true, "g": true, "h": true, "k": false}},
	}
	for _, tc := range cases {
		for probe, want := range tc.conflicts {
			t.Run(fmt.Sprintf("%s/probe=%s", tc.name, probe), func(t *testing.T) {
				t.Parallel()
				db := New(nil)
				seedKeys(t, db, "b", "f", "h", "m")
				handle, err := db.CreateWritableTransaction()
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				tx := handle.(*simTxn)
				if tc.rywDisabled {
					if err := tx.Options().SetReadYourWritesDisable(); err != nil {
						t.Fatal(err)
					}
				}
				kvs, err := tx.GetRange(tc.r, fdb.RangeOptions{Reverse: tc.reverse}).GetSliceWithError()
				if err != nil {
					t.Fatalf("range read: %v", err)
				}
				rows := ""
				for i, kv := range kvs {
					if i > 0 {
						rows += ","
					}
					rows += string(kv.Key)
				}
				if rows != tc.rows {
					t.Fatalf("rows %q, want %q", rows, tc.rows)
				}
				if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
					w.Set(k(probe), []byte("concurrent"))
					return nil, nil
				}); err != nil {
					t.Fatalf("concurrent write: %v", err)
				}
				tx.Set(k("sentinel"), []byte("1"))
				got := false
				if err := tx.Commit().Get(); err != nil {
					if code := errCode(t, err); code != 1020 {
						t.Fatalf("commit error %d, want 1020 or success", code)
					}
					got = true
				}
				if got != want {
					t.Fatalf("write at %q: conflicted=%t, want %t (recorded %v)", probe, got, want, conflictStrings(tx))
				}
			})
		}
	}
}
