package client

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
)

// TestGetRange_MoreOnExactlyLimit pins the C++ `result.more = result.more || limits.isReached()`
// contract (ReadYourWrites.actor.cpp:799): when a row-limited GetRange returns EXACTLY `limit`
// rows, FDB forces more=true — the "limit was the stop reason" semantics — even when no further
// data exists. The bug: Go's RYW getRange derived `more` from residual-data presence, so the
// SLOW path (local write/clear in range, ryw.go) and the CACHED fast path (snapshotCache hit)
// returned more=FALSE at the exactly-limit==total boundary, diverging from libfdb_c and (via
// rangeConflictExtent, which keys off `more`) over-conflicting the read to the full [begin,end).
// The non-cached fast path was already correct (the wire getRangeImpl returns more=true on
// exactly-limit). Revert-proof: with the fix backed out, the slow + cached subtests fail.
func TestGetRange_MoreOnExactlyLimit(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"
	begin := []byte(prefix)
	end := append([]byte(prefix), 0xFF)

	// Seed exactly three committed keys a,b,c.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"a"), []byte("1"))
		tx.Set([]byte(prefix+"b"), []byte("2"))
		tx.Set([]byte(prefix+"c"), []byte("3"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// SLOW path: a pending write inside the range forces the iterative merge. The merged view is
	// still exactly 3 keys (a is overwritten), so limit=3 fills exactly → more must be true.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"a"), []byte("1x")) // in-range pending write → slow path
		kvs, more, err := tx.GetRange(ctx, begin, end, 3)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(len(kvs)).To(gomega.Equal(3))
		g.Expect(more).To(gomega.BeTrue(), "slow path: exactly-limit GetRange must report more=true (isReached)")
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// CACHED fast path: a full read populates the snapshotCache; the second limited read at
	// exactly-limit is served from cache → must also report more=true.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		_, _, err := tx.GetRange(ctx, begin, end, 0) // populate snapshotCache for [begin,end)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		kvs, more, err := tx.GetRange(ctx, begin, end, 3) // cached fast path, exactly-limit
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(len(kvs)).To(gomega.Equal(3))
		g.Expect(more).To(gomega.BeTrue(), "cached fast path: exactly-limit GetRange must report more=true (isReached)")
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// NON-cached fast path (positive control — already correct before the fix): no pending writes,
	// no cache → the wire path reports more=true on exactly-limit.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, begin, end, 3)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(len(kvs)).To(gomega.Equal(3))
		g.Expect(more).To(gomega.BeTrue(), "non-cached fast path: exactly-limit GetRange must report more=true")
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}
