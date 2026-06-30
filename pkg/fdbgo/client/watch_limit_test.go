package client

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestDatabase_OutstandingWatchLimit pins the outstanding-watch cap (C++ increaseWatchCounter →
// too_many_watches 1032, NativeAPI.actor.cpp:2175-2179/:5694). Previously SetMaxWatches was a no-op
// and no counter existed, so a Go app could register unlimited pending watches and never see 1032.
// White-box on the Database counter (no FDB container needed). Revert-proof: a no-op SetMaxWatches /
// missing counter never returns 1032.
func TestDatabase_OutstandingWatchLimit(t *testing.T) {
	t.Parallel()

	t.Run("cap_enforced", func(t *testing.T) {
		t.Parallel()
		db := &database{}
		db.maxWatches.Store(1)
		if err := db.tryAcquireWatch(); err != nil {
			t.Fatalf("first acquire (under the cap) must succeed, got %v", err)
		}
		err := db.tryAcquireWatch()
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrTooManyWatches {
			t.Fatalf("acquiring over the cap must be too_many_watches (1032), got %v", err)
		}
		db.releaseWatch() // free the first slot
		if err := db.tryAcquireWatch(); err != nil {
			t.Fatalf("after release a slot is free again, acquire must succeed, got %v", err)
		}
	})

	t.Run("unlimited_when_zero", func(t *testing.T) {
		t.Parallel()
		db := &database{} // maxWatches zero value = 0 = unlimited
		for i := 0; i < 100; i++ {
			if err := db.tryAcquireWatch(); err != nil {
				t.Fatalf("max=0 must be unlimited, got %v at %d", err, i)
			}
		}
	})
}
