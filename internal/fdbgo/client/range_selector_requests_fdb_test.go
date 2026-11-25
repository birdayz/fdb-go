package client

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A selector range read costs one storage request: the selectors travel in the
// GetKeyValuesRequest and the storage server resolves them in the same read
// (NativeAPI.actor.cpp:4651-4679; storageserver.actor.cpp:4602-4611), under
// RYW after the walk has resolved what the write map and cache know
// (ReadYourWrites.actor.cpp:705-759). No GetKeyRequest is sent, and the read is
// not preceded by one per selector. The shape is BunchedMap's put read:
// lastLessOrEqual(k) to firstGreaterThan(k)+1.
func TestSelectorRangeReadSendsOneStorageRequest(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name                  string
		snapshot, rywDisabled bool
	}{{"ryw", false, false}, {"ryw-snapshot", true, false}, {"ryw-disabled", false, true}, {"ryw-disabled-snapshot", true, true}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			db, rec := openRecordedDB(t, ctx)
			prefix := t.Name() + "/"
			seedPlumbingKeys(t, ctx, db, prefix, "a", "c", "e", "g")

			tx := db.CreateTransaction()
			defer tx.Cancel()
			if mode.rywDisabled {
				tx.SetReadYourWritesDisable()
			}
			if _, err := tx.GetReadVersion(ctx); err != nil {
				t.Fatal(err)
			}
			// Warm the location cache so only storage requests are recorded.
			if _, err := tx.db.locCache.locateRange(tx.db, ctx, []byte(prefix), []byte(prefix+"\xff"), 100, false, tx.tenantId, tx.currentSpan()); err != nil {
				t.Fatal(err)
			}
			begin := KeySelector{Key: []byte(prefix + "d"), OrEqual: true, Offset: 0}
			end := KeySelector{Key: []byte(prefix + "d"), OrEqual: true, Offset: 2}
			read := tx.GetRangeSelectors
			if mode.snapshot {
				read = tx.Snapshot().GetRangeSelectors
			}
			rec.start()
			kvs, more, err := read(ctx, begin, end, 0, 0, false)
			recorded := rec.stop()
			if err != nil {
				t.Fatal(err)
			}
			if len(recorded.problems) > 0 {
				t.Fatalf("recorder problems:\n%s", strings.Join(recorded.problems, "\n"))
			}
			if got := keysOf(kvs); got != prefix+"c,"+prefix+"e" || more {
				t.Fatalf("rows %s more=%t, want %sc,%se", got, more, prefix, prefix)
			}
			kinds := map[string]int{}
			var storage []string
			for _, r := range recorded.requests {
				kind, _, _ := strings.Cut(r, " ")
				if storageRequest[kind] {
					kinds[kind]++
					storage = append(storage, r)
				}
			}
			if kinds["GetKeyValuesRequest"] != 1 || len(storage) != 1 {
				t.Fatalf("storage requests %v, want one GetKeyValuesRequest:\n%s", kinds, strings.Join(storage, "\n"))
			}
			// The selectors reach storage unresolved, after removeOrEqual.
			key := []byte(prefix + "d\x00")
			for _, field := range []string{
				fmt.Sprintf("Begin={Key:%v OrEqual:false Offset:0}", key),
				fmt.Sprintf("End={Key:%v OrEqual:false Offset:2}", key),
			} {
				if !strings.Contains(storage[0], field) {
					t.Errorf("request lacks %s:\n%s", field, storage[0])
				}
			}
		})
	}
}
