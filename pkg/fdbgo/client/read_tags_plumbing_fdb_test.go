package client

import (
	"context"
	"testing"
	"time"
)

// Every read request kind carries the transaction's read tags when sampled,
// an empty set included, and none when not (NativeAPI.actor.cpp:3681, 3806,
// 3970, 4303, 4684).
func TestReadTagSamplingRequestKinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rate float64
		tags []string
	}{
		{"sampled-empty", 1, nil},
		{"sampled-tags", 1, []string{"rt", "x"}},
		{"unsampled", 0, []string{"rt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			db, rec := openReadTagDB(t, ctx, sharedClusterFile, plainDial, tc.rate, nil)
			prefix := t.Name() + "/"
			seedPlumbingKeys(t, ctx, db, prefix, "a", "b", "c")
			begin, end := []byte(prefix), []byte(prefix+"\xff")
			tx := db.CreateTransaction()
			defer tx.Cancel()
			for _, tag := range tc.tags {
				if err := tx.SetAutoThrottleTag(tag); err != nil {
					t.Fatal(err)
				}
			}
			rv, err := tx.GetReadVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, 100, false, tx.tenantId, tx.currentSpan()); err != nil {
				t.Fatal(err)
			}

			rec.start()
			if _, err := tx.getValue(ctx, []byte(prefix+"a")); err != nil {
				t.Fatalf("getValue: %v", err)
			}
			if _, pending, err := tx.GetPipelined(ctx, []byte(prefix+"b")); err != nil || pending == nil {
				t.Fatalf("GetPipelined: %v %v", pending, err)
			} else if _, err := pending.Resolve(); err != nil {
				t.Fatalf("pipelined Resolve: %v", err)
			}
			if _, _, err := tx.getKeyImpl(ctx, rv, []byte(prefix+"a"), false, 1); err != nil {
				t.Fatalf("getKeyImpl: %v", err)
			}
			if _, _, _, err := tx.getRangeImpl(ctx, begin, end, newRangeLimits(0, ByteLimitUnlimited), rv, false); err != nil {
				t.Fatalf("getRangeImpl: %v", err)
			}
			// The data is not tuple-shaped, so storage may reject the mapper;
			// the request is what is under test.
			mapper := []byte("\x01m\x00")
			_, _, _ = tx.getMappedRangeImpl(ctx, begin, end, mapper, 0, ByteLimitUnlimited, rv, false)
			_, _ = tx.getMappedRangeSelectors(ctx, firstGreaterOrEqual(begin), firstGreaterOrEqual(end), mapper, newMappedRangeLimits(0, ByteLimitUnlimited), rv, false)
			watched := make(chan error, 1)
			go func() { watched <- tx.Watch(ctx, []byte(prefix+"w")) }()
			waitForRecorded(t, ctx, rec, "WatchValueRequest")
			// From another database: the recorder fails on a commit request.
			other, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithAPIVersion(730))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			setKey(t, ctx, other, []byte(prefix+"w"), []byte("changed"))
			if err := <-watched; err != nil {
				t.Fatalf("Watch: %v", err)
			}
			reqs := taggedRequests(t, rec.stop())

			want := taggedRequest{has: tc.rate == 1}
			if want.has {
				want.tags = string(encodeTagSet(tc.tags))
			}
			kinds := map[string]int{}
			for _, r := range reqs {
				kinds[r.kind]++
				if r.has != want.has || r.tags != want.tags {
					t.Errorf("%v, want HasTags=%v Tags=%q", r, want.has, want.tags)
				}
			}
			if len(kinds) != len(readTagKinds) || kinds["GetValueRequest"] < 3 {
				t.Fatalf("request kinds %v, want all five, with GetValue for the read, the pipelined read and the watch's read", kinds)
			}
		})
	}
}
