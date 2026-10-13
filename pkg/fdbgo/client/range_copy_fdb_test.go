package client

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestRangeCopiesPublicOwnershipAndConflicts(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, writes := range []bool{false, true} {
				t.Run(fmt.Sprintf("snapshot=%t/reverse=%t/writes=%t", snapshot, reverse, writes), func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					db := rangeIssueDB(t, ctx)
					defer db.Close()
					prefix := []byte(t.Name())
					rangeIssueSeed(t, ctx, db, prefix, 6)
					key := func(i int) []byte { return rangeIssueKey(prefix, i) }
					tx := db.CreateTransaction()
					defer tx.Cancel()
					var want []KeyValue
					for i := 0; i < 6; i++ {
						if writes && i == 3 {
							continue
						}
						value := []byte{byte(i)}
						if writes && i == 2 {
							value = []byte("local")
							tx.Set(key(i), value)
						}
						want = append(want, KeyValue{Key: key(i), Value: value})
					}
					if writes {
						tx.Clear(key(3))
					}
					if reverse {
						for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
							want[i], want[j] = want[j], want[i]
						}
					}
					end := append(bytes.Clone(prefix), 0xff)
					start := func() *PendingRange {
						if snapshot {
							return tx.Snapshot().StartGetRange(ctx, prefix, end, 0, ByteLimitUnlimited, reverse)
						}
						return tx.StartGetRange(ctx, prefix, end, 0, ByteLimitUnlimited, reverse)
					}
					pending := start()
					check := func(p *PendingRange, want []KeyValue) {
						t.Helper()
						for replay := 0; replay < 2; replay++ {
							rows, more, err := p.Get()
							if err != nil || more || !reflect.DeepEqual(rows, want) {
								t.Fatalf("public rows = %v/%t/%v, want %v", rows, more, err, want)
							}
							for i := range rows {
								rows[i].Key[0], rows[i].Value[0] = '!', '!'
								rows[i] = KeyValue{Key: []byte("replaced")}
							}
						}
					}
					check(pending, want)
					check(start(), want) // A cache-backed new operation must own its public bytes too.
					tx.Set(key(1), []byte("later"))
					check(pending, want) // Completed future retains its issue-time value after later writes.
					later := append([]KeyValue(nil), want...)
					for i := range later {
						if bytes.Equal(later[i].Key, key(1)) {
							later[i].Value = []byte("later")
						}
					}
					check(start(), later)
					if _, err := db.Transact(ctx, func(w *Transaction) (any, error) { w.Set(key(4), []byte("external")); return nil, nil }); err != nil {
						t.Fatal(err)
					}
					tx.Set(key(99), []byte("marker"))
					err := tx.Commit(ctx)
					if snapshot && err != nil {
						t.Fatalf("snapshot conflict changed: %v", err)
					}
					if !snapshot && fdbCode(err) != 1020 {
						t.Fatalf("ordinary read conflict = %v, want 1020", err)
					}
				})
			}
		}
	}
}
