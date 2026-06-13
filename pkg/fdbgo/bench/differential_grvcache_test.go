package bench

import (
	"bytes"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// TestDifferential_GRVCacheDefaultSeesCgoSeed pins RFC-104: a DEFAULT Go
// transaction (GRV cache off) observes a key a libfdb_c client just committed —
// external causality, exactly as a default libfdb_c transaction does.
//
// Before RFC-104 the Go client's GRV cache was ALWAYS-ON: a default Go
// transaction could be served a cached read version OLDER than the cgo commit,
// making the seed invisible (a demonstrated wrong answer that forced the
// accessed_unreadable differential to seed through the Go client — see
// differential_unreadable_test.go). With the cache opt-in/default-off, every
// default Go transaction issues a fresh proxy GRV, so it sees libfdb_c's
// committed data immediately.
func TestDifferential_GRVCacheDefaultSeesCgoSeed(t *testing.T) {
	t.Parallel()
	key := []byte("differential_grvcache_seed")
	want := []byte("from-cgo")

	// Warm the Go client's GRV cache with a real read first, so the regression
	// (serving a stale cached version) would be reachable on the old always-on
	// cache. Post-fix this is irrelevant — a default read never consults the cache.
	_ = goGet(t, []byte("differential_grvcache_warm"))

	// libfdb_c commits the key.
	mustCGo(t, func(tx cgofdb.Transaction) {
		tx.Set(cgofdb.Key(key), want)
	})

	// A fresh DEFAULT Go transaction must see it (fresh GRV per txn, no cache).
	got := goGet(t, key)
	if !bytes.Equal(got, want) {
		t.Fatalf("default Go read of a cgo-committed key = %q, want %q — a stale GRV cache hid it (RFC-104 regression)", got, want)
	}

	// And the converse: cgo sees a Go-committed key (symmetry / sanity).
	key2 := []byte("differential_grvcache_seed_go")
	want2 := []byte("from-go")
	if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		tx.Set(gofdb.Key(key2), want2)
		return nil, nil
	}); err != nil {
		t.Fatalf("go seed: %v", err)
	}
	if got := cgoGet(t, key2); !bytes.Equal(got, want2) {
		t.Fatalf("cgo read of a Go-committed key = %q, want %q", got, want2)
	}
}
