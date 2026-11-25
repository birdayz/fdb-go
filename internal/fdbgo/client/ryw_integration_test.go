package client

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
)

// TestRYW_SetClearGetRange_Integration tests the exact scenario that failed
// in the multi-shard test: Set 3 keys, Clear 1, GetRange should return 2.
func TestRYW_SetClearGetRange_Integration(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"a"), []byte("val-a"))
		tx.Set([]byte(prefix+"b"), []byte("val-b"))
		tx.Set([]byte(prefix+"c"), []byte("val-c"))

		tx.Clear([]byte(prefix + "b"))

		// Single key reads.
		val, err := tx.Get(ctx, []byte(prefix+"a"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(val)).To(gomega.Equal("val-a"))

		val, err = tx.Get(ctx, []byte(prefix+"b"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		t.Logf("Get(b) after clear: val=%v (nil=%v, len=%d)", val, val == nil, len(val))

		// Range read.
		kvs, more, err := tx.GetRange(ctx, []byte(prefix), append([]byte(prefix), 0xFF), 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		t.Logf("GetRange: %d keys, more=%v", len(kvs), more)
		for _, kv := range kvs {
			t.Logf("  key=%s val=%s", kv.Key, kv.Value)
		}
		g.Expect(len(kvs)).To(gomega.Equal(2), "expected a and c")

		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

// TestSnapshotCache_GetRange_Integration verifies that the SnapshotCache
// produces correct results against a real FDB cluster. Reads the same range
// twice within one transaction — second read must hit the cache without
// network I/O and return identical results.
func TestSnapshotCache_GetRange_Integration(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed data.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 20; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), []byte(fmt.Sprintf("val-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Read the same range twice in one transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)

		kvs1, more1, err := tx.GetRange(ctx, begin, end, 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(more1).To(gomega.BeFalse())
		g.Expect(kvs1).To(gomega.HaveLen(20))

		// Second read: should hit SnapshotCache.
		kvs2, more2, err := tx.GetRange(ctx, begin, end, 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(more2).To(gomega.BeFalse())
		g.Expect(kvs2).To(gomega.HaveLen(20))

		// Values must match.
		for i := range kvs1 {
			g.Expect(kvs2[i].Key).To(gomega.Equal(kvs1[i].Key))
			g.Expect(kvs2[i].Value).To(gomega.Equal(kvs1[i].Value))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

// TestSnapshotCache_GetAfterRange_Integration verifies that a single-key Get
// after a range scan is served from the SnapshotCache.
func TestSnapshotCache_GetAfterRange_Integration(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed data.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), []byte(fmt.Sprintf("val-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)

		// Range scan populates cache.
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(kvs).To(gomega.HaveLen(10))

		// Single key Get — should come from cache.
		val, err := tx.Get(ctx, []byte(fmt.Sprintf("%s%04d", prefix, 5)))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(val)).To(gomega.Equal("val-5"))

		// Non-existent key in cached range — should return nil.
		val, err = tx.Get(ctx, []byte(prefix+"9999"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(val).To(gomega.BeNil())

		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

// TestSnapshotCache_WritesThenRange_Integration verifies that writes + range
// scan works correctly with SnapshotCache: server data is cached, local writes
// are merged in on top.
func TestSnapshotCache_WritesThenRange_Integration(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 5 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 5; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), []byte(fmt.Sprintf("val-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)

		// First range scan — populates cache.
		kvs1, _, err := tx.GetRange(ctx, begin, end, 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(kvs1).To(gomega.HaveLen(5))

		// Add a local write.
		tx.Set([]byte(prefix+"0002_5"), []byte("inserted"))

		// Clear one key.
		tx.Clear([]byte(fmt.Sprintf("%s%04d", prefix, 1)))

		// Second range scan — should use cache for server data, merge writes/clears.
		kvs2, _, err := tx.GetRange(ctx, begin, end, 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(kvs2).To(gomega.HaveLen(5)) // 5 - 1 (clear) + 1 (insert) = 5

		// Verify the inserted key is present.
		found := false
		for _, kv := range kvs2 {
			if string(kv.Key) == prefix+"0002_5" {
				found = true
				g.Expect(string(kv.Value)).To(gomega.Equal("inserted"))
			}
		}
		g.Expect(found).To(gomega.BeTrue(), "inserted key should be in range scan")

		// Verify cleared key is absent.
		for _, kv := range kvs2 {
			g.Expect(string(kv.Key)).ToNot(gomega.Equal(fmt.Sprintf("%s%04d", prefix, 1)),
				"cleared key should not be in range scan")
		}

		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}
