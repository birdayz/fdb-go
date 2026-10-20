package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func TestMappedRangeConflictMustUnmodified(t *testing.T) {
	t.Parallel()
	set := func(c *rywCache, key string) { c.set([]byte(key), nil) }
	clear := func(c *rywCache, begin, end string) { c.clearRange([]byte(begin), []byte(end)) }
	for _, tc := range []struct {
		name, begin, end, prefixEnd string
		modify                      func(*rywCache)
		wantError                   bool
	}{
		{"empty", "b", "b", "b", func(c *rywCache) { set(c, "b") }, false},
		{"inverted", "d", "b", "d", func(c *rywCache) { set(c, "c") }, false},
		{"unmodified", "b", "e", "e", func(*rywCache) {}, false},
		{"outside_writes", "b", "e", "e", func(c *rywCache) { set(c, "a"); set(c, "e") }, false},
		{"outside_clears", "b", "e", "e", func(c *rywCache) { clear(c, "a", "b"); clear(c, "e", "f") }, false},
		{"write_at_begin", "b", "e", "b", func(c *rywCache) { set(c, "b") }, true},
		{"set", "b", "e", "c", func(c *rywCache) { set(c, "c") }, true},
		{"absent_operation", "b", "e", "c", func(c *rywCache) {
			c.liveLocked().writes = map[string]rywEntry{"c": {absent: true, dependent: true}}
		}, true},
		{"clear", "b", "e", "c", func(c *rywCache) { clear(c, "c", "d") }, true},
		{"overlapping_clear", "b", "e", "b", func(c *rywCache) { clear(c, "a", "c") }, true},
		{"set_over_clear", "b", "e", "c", func(c *rywCache) { clear(c, "c", "d"); set(c, "c") }, true},
		{"write_before_clear", "b", "f", "c", func(c *rywCache) { set(c, "c"); clear(c, "d", "e") }, true},
		{"clear_before_write", "b", "f", "c", func(c *rywCache) { clear(c, "c", "d"); set(c, "e") }, true},
		{"dependent_atomic", "b", "e", "c", func(c *rywCache) { c.atomic(MutAddValue, []byte("c"), []byte{1}) }, true},
		{"unreadable_operation", "b", "e", "c", func(c *rywCache) { c.atomic(MutSetVersionstampedValue, []byte("c"), nil) }, true},
		{"unmodified_unreadable_gap", "b", "e", "e", func(c *rywCache) { c.addUnreadableRange([]byte("c"), []byte("d")) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			tc.modify(&tx.ryw)
			err := tx.addMappedRangeConflictMustUnmodified(context.Background(), []byte(tc.begin), []byte(tc.end))
			if tc.wantError {
				if fe, ok := err.(*wire.FDBError); !ok || fe.Code != 2039 {
					t.Fatalf("error=%v, want 2039", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tc.begin >= tc.prefixEnd {
				if len(tx.readConflicts) != 0 {
					t.Fatalf("unexpected conflict ranges: %+v", tx.readConflicts)
				}
				return
			}
			if len(tx.readConflicts) != 1 || !bytes.Equal(tx.readConflicts[0].Begin, []byte(tc.begin)) || !bytes.Equal(tx.readConflicts[0].End, []byte(tc.prefixEnd)) {
				t.Fatalf("conflict ranges=%+v, want [%q,%q)", tx.readConflicts, tc.begin, tc.prefixEnd)
			}
		})
	}
}

func TestMappedRangeConflictPublicationRetainsIncarnation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		pauseAt  int
		modified bool
	}{
		{name: "primary", pauseAt: 1},
		{name: "point", pauseAt: 2},
		{name: "range", pauseAt: 3},
		{name: "modified-prefix", pauseAt: 3, modified: true},
	} {
		for _, reset := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reset=%t", tc.name, reset), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				tx := &Transaction{}
				rows := []MappedKeyValue{
					{Key: []byte("b"), Kind: MappedResultGetValue, GetValue: MappedGetValue{Key: []byte("j")}},
					{Key: []byte("c"), Kind: MappedResultGetRange, GetRange: MappedGetRange{
						Begin: firstGreaterOrEqual([]byte("m")), End: firstGreaterOrEqual([]byte("q")),
						Rows: []KeyValue{{Key: []byte("o")}}, More: true,
					}},
				}
				want := [][2]string{{"a", "e"}, {"j", "j\x00"}, {"m", "o\x00"}}
				if tc.modified {
					tx.ryw.set([]byte("n"), nil)
					want[2][1] = "n"
				}
				entered, release := make(chan struct{}), make(chan struct{})
				resume := sync.OnceFunc(func() { close(release) })
				defer resume()
				publications := 0
				tx.beforeReadConflict = func() {
					publications++
					if publications == tc.pauseAt {
						close(entered)
						<-release
					}
				}
				parent, leave := tx.opContext(context.Background())
				defer leave()
				done := make(chan error, 1)
				go func() {
					borrowed, releaseBorrowed := tx.opContext(parent)
					defer releaseBorrowed()
					done <- tx.addMappedRangeConflicts(borrowed, []byte("a"), []byte("e"), false, false, rows)
				}()
				select {
				case <-entered:
				case err := <-done:
					t.Fatalf("mapped read completed before publication %d: %v", tc.pauseAt, err)
				case <-ctx.Done():
					t.Fatal("mapped read never reached conflict publication")
				}
				tx.conflictMu.Lock()
				prior := len(tx.readConflicts)
				tx.conflictMu.Unlock()
				if prior != tc.pauseAt-1 {
					t.Fatalf("published prefix has %d ranges, want %d", prior, tc.pauseAt-1)
				}
				leave() // the async read borrowed this now-retired frame's lease
				if reset {
					resetDone := make(chan struct{})
					go func() { tx.Reset(); close(resetDone) }()
					select {
					case <-resetDone:
					case <-ctx.Done():
						t.Fatal("Reset waited on a borrowed completed read")
					}
				}
				resume()
				var err error
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal("mapped read did not finish after publication resumed")
				}
				if reset {
					if code, _ := fdbErrorCode(err); code != ErrTransactionCancelled {
						t.Errorf("retired mapped read = %v, want transaction_cancelled (1025)", err)
					}
					want = nil
					if publications != tc.pauseAt {
						t.Errorf("retired mapped read attempted %d publications, want %d", publications, tc.pauseAt)
					}
				} else if tc.modified {
					if code, _ := fdbErrorCode(err); code != ErrGetMappedRangeReadsYourWrites {
						t.Errorf("modified mapped read = %v, want 2039", err)
					}
				} else if err != nil {
					t.Errorf("live mapped read = %v", err)
				}
				tx.conflictMu.Lock()
				defer tx.conflictMu.Unlock()
				if len(tx.readConflicts) != len(want) {
					t.Fatalf("conflicts = %+v, want %v", tx.readConflicts, want)
				}
				for i, r := range tx.readConflicts {
					if string(r.Begin) != want[i][0] || string(r.End) != want[i][1] {
						t.Errorf("conflict %d = [%q,%q), want %v", i, r.Begin, r.End, want[i])
					}
				}
			})
		}
	}
}

func TestMappedRangeConflictBatchOrder(t *testing.T) {
	t.Parallel()
	for _, failAt := range []string{"none", "primary", "secondary"} {
		t.Run(failAt, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			rows := []MappedKeyValue{
				{Key: []byte("b"), Kind: MappedResultGetValue, GetValue: MappedGetValue{Key: []byte("j")}}, // point miss still conflicts
				{Key: []byte("c"), Kind: MappedResultGetRange, GetRange: MappedGetRange{
					Begin: KeySelector{Key: []byte("m"), Offset: 1}, End: KeySelector{Key: []byte("q"), Offset: 1},
					Rows: []KeyValue{{Key: []byte("o")}}, More: true,
				}},
				{Key: []byte("d"), Kind: MappedResultGetValue, GetValue: MappedGetValue{Key: []byte("z"), Present: true}},
			}
			want := [][2]string{{"a", "e"}, {"j", "j\x00"}, {"m", "o\x00"}, {"z", "z\x00"}}
			switch failAt {
			case "primary":
				tx.ryw.set([]byte("c"), nil)
				want = [][2]string{{"a", "c"}}
			case "secondary":
				tx.ryw.set([]byte("n"), nil)
				want = [][2]string{{"a", "e"}, {"j", "j\x00"}, {"m", "n"}}
			}
			err := tx.addMappedRangeConflicts(context.Background(), []byte("a"), []byte("e"), false, false, rows)
			if failAt == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if native, ok := err.(*wire.FDBError); !ok || native.Code != 2039 {
				t.Fatalf("error = %v, want 2039", err)
			}
			if len(tx.readConflicts) != len(want) {
				t.Fatalf("conflicts = %v, want %v", tx.readConflicts, want)
			}
			// Conflict storage must own keys even after the source batch is gone.
			for i := range rows {
				if len(rows[i].GetValue.Key) != 0 {
					rows[i].GetValue.Key[0] = 'x'
				}
				if len(rows[i].GetRange.Begin.Key) != 0 {
					rows[i].GetRange.Begin.Key[0] = 'x'
				}
				if len(rows[i].GetRange.End.Key) != 0 {
					rows[i].GetRange.End.Key[0] = 'x'
				}
				for j := range rows[i].GetRange.Rows {
					rows[i].GetRange.Rows[j].Key[0] = 'x'
				}
			}
			for i, r := range tx.readConflicts {
				if string(r.Begin) != want[i][0] || string(r.End) != want[i][1] {
					t.Fatalf("conflict %d = [%q,%q), want %v", i, r.Begin, r.End, want[i])
				}
			}
		})
	}
}

// This isolated Bazel target has no concurrent workers: parallel tests wait
// until this sequential allocation counter finishes. Buffer growth is warmed.
func TestMappedRangeConflictBatchAllocations(t *testing.T) {
	const count = 256
	rows := make([]MappedKeyValue, count)
	for i := range rows {
		rows[i] = MappedKeyValue{
			Key: []byte{0x10, byte(i)}, Kind: MappedResultGetRange,
			GetRange: MappedGetRange{Begin: KeySelector{Key: []byte{0x30, byte(i)}}, End: KeySelector{Key: []byte{0x31, byte(i)}}},
		}
	}
	tx := &Transaction{readConflicts: make([]KeyRange, 0, count+1), conflictBuf: make([]byte, 0, 8192)}
	begin, end := []byte{0x00}, []byte{0x20}
	allocations := testing.AllocsPerRun(100, func() {
		clear(tx.readConflicts)
		tx.readConflicts, tx.conflictBuf = tx.readConflicts[:0], tx.conflictBuf[:0]
		if err := tx.addMappedRangeConflicts(context.Background(), begin, end, false, false, rows); err != nil {
			t.Fatal(err)
		}
	})
	if len(tx.readConflicts) != count+1 || !bytes.Equal(tx.readConflicts[0].Begin, begin) || !bytes.Equal(tx.readConflicts[0].End, end) {
		t.Fatal("allocation sample did not insert every primary/secondary conflict")
	}
	// The current helper clones one endpoint per range. Collecting those ranges
	// in a temporary growing slice would add batch allocations beyond these.
	if allocations != count+1 {
		t.Fatalf("mapped conflict allocations = %g, want %d endpoint copies only", allocations, count+1)
	}
	t.Logf("mapped conflict allocations: rows=%d allocs=%.0f endpoint copies only", count, allocations)
}
