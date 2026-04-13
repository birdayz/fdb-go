package client

import (
	"context"
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
