package client

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func openRecordedDB(t *testing.T, ctx context.Context) (*Database, *sendRecorder) {
	t.Helper()
	rec := &sendRecorder{}
	// Never sample read tags, so recorded requests compare without chance fields.
	db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithDialFunc(rec.wrap(plainDial)), WithAPIVersion(730), withReadTagSampling(0, nil))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, rec
}

func seedPlumbingKeys(t *testing.T, ctx context.Context, db *Database, prefix string, keys ...string) {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, k := range keys {
			tx.Set([]byte(prefix+k), []byte(strings.Repeat("v", 100)))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// Every storage request a read sends carries the version its operation
// captured, not a later read of the transaction's version (C++ getRange and
// getKey take trState->readVersion() once, NativeAPI.actor.cpp:4617-4618).
// Each entry point is handed a version older than the transaction's own, so a
// sender that re-read the transaction would send the newer one.
func TestStorageRequestsCarryOperationVersion(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, recorder := openRecordedDB(t, ctx)
	prefix := t.Name() + "/"
	seedPlumbingKeys(t, ctx, db, prefix, "a", "b", "c")
	begin, end := []byte(prefix), []byte(prefix+"\xff")

	tx := db.CreateTransaction()
	defer tx.Cancel()
	own, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	older := own - 1
	mapper := []byte("\x01m\x00")

	recorder.start()
	if _, _, _, err := tx.getRangeImpl(ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), older, false); err != nil {
		t.Fatalf("getRangeImpl: %v", err)
	}
	if _, _, err := tx.getKeyImpl(ctx, older, []byte(prefix+"a"), false, 2); err != nil {
		t.Fatalf("getKeyImpl: %v", err)
	}
	// The data is not tuple-shaped, so storage may reject the mapper; the
	// request is what is under test.
	_, _, _ = tx.getMappedRangeImpl(ctx, begin, end, mapper, 0, ByteLimitUnlimited, older, false)
	_, _ = tx.getMappedRangeSelectors(ctx, firstGreaterOrEqual(begin), firstGreaterOrEqual(end), mapper, newMappedRangeLimits(0, ByteLimitUnlimited), older, false)
	rec := recorder.stop()
	if len(rec.problems) > 0 {
		t.Fatalf("recorder problems:\n%s", strings.Join(rec.problems, "\n"))
	}

	// The recorder numbers versions in order of appearance; every storage
	// request must name the same one, and it must be the older version.
	if name := rec.versions[older]; name != "V1" || len(rec.versions) != 1 {
		t.Errorf("versions sent %v, want only the operation's %d (the transaction's is %d)", rec.versions, older, own)
	}
	kinds := map[string]int{}
	for _, r := range rec.requests {
		kind, _, _ := strings.Cut(r, " ")
		if kind == "GetKeyServerLocationsRequest" {
			continue
		}
		kinds[kind]++
		if !strings.Contains(r, "Version=V1") {
			t.Errorf("request not at the operation's version:\n%s", r)
		}
	}
	if kinds["GetKeyValuesRequest"] != 1 || kinds["GetKeyRequest"] != 1 || kinds["GetMappedKeyValuesRequest"] != 2 || len(kinds) != 3 {
		t.Fatalf("storage requests %v, want 1 GetKeyValues, 1 GetKey, 2 GetMappedKeyValues:\n%s", kinds, strings.Join(rec.requests, "\n"))
	}
}

// A byte-limited exact range that has not returned minRows rows keeps reading
// instead of stopping at the first non-empty reply, and its next request asks
// storage for exactly the rows still missing, without a byte limit
// (NativeAPI.actor.cpp:4415, 4236-4239). RYW requests carry minRows > 1
// (setRequestLimits); this pins the exact-range path they fall back to.
func TestExactRangeMinRowsOnStorage(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			db, rec := openRecordedDB(t, ctx)
			prefix := t.Name() + "/"
			seedPlumbingKeys(t, ctx, db, prefix, "a", "b", "c", "d", "e")
			begin, end := []byte(prefix), []byte(prefix+"\xff")

			tx := db.CreateTransaction()
			defer tx.Cancel()
			rv, err := tx.GetReadVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Warm the location cache so only storage requests are recorded.
			if _, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, 100, reverse, tx.tenantId, tx.currentSpan()); err != nil {
				t.Fatal(err)
			}
			rec.start()
			kvs, more, _, err := tx.getRangeImpl(ctx, begin, end, rangeLimits{rows: -1, minRows: 3, bytes: 1}, rv, reverse)
			recorded := rec.stop()
			if err != nil {
				t.Fatal(err)
			}
			if len(recorded.problems) > 0 {
				t.Fatalf("recorder problems:\n%s", strings.Join(recorded.problems, "\n"))
			}
			reqs := recorded.requests
			first, second := prefix+"a", prefix+"a\x00"
			wantLimit := int32(2)
			if reverse {
				first, second = prefix+"e", prefix+"e"
				wantLimit = -2
			}
			if len(kvs) != 3 || !more || string(kvs[0].Key) != first {
				var got []byte
				if len(kvs) > 0 {
					got = kvs[0].Key
				}
				t.Fatalf("got %d rows (first %q), more=%t; want 3 rows from %q and more", len(kvs), got, more, first)
			}
			if len(reqs) != 2 {
				t.Fatalf("got %d requests, want 2:\n%s", len(reqs), strings.Join(reqs, "\n"))
			}
			if !strings.Contains(reqs[0], " LimitBytes=1 ") || !strings.Contains(reqs[0], fmt.Sprintf(" Limit=%d ", 80000*(1-2*boolInt(reverse)))) {
				t.Errorf("first request is not the caller's byte limit under the row ceiling:\n%s", reqs[0])
			}
			if !strings.Contains(reqs[1], fmt.Sprintf(" Limit=%d LimitBytes=80000 ", wantLimit)) {
				t.Errorf("second request does not ask for the %d missing rows without a byte limit:\n%s", wantLimit, reqs[1])
			}
			field := fmt.Sprintf("Begin={Key:%v OrEqual:false Offset:1}", []byte(second))
			if reverse {
				field = fmt.Sprintf("End={Key:%v OrEqual:false Offset:1}", []byte(second))
			}
			if !strings.Contains(reqs[1], field) {
				t.Errorf("second request does not continue after the first reply (%s):\n%s", field, reqs[1])
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
