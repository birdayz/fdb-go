package client

import (
	"bytes"
	"testing"

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
			c.writes = map[string]rywEntry{"c": {absent: true, dependent: true}}
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
			err := tx.addMappedRangeConflictMustUnmodified([]byte(tc.begin), []byte(tc.end))
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
