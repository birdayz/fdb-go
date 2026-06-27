package bench

import (
	"sort"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Differential sentinels for the RFC prod-readiness "unprobed axes" (P2.7): the
// RESULT VALUES of LocalityGetAddressesForKey / GetEstimatedRangeSizeBytes /
// GetRangeSplitPoints — previously only their error codes were pinned. Each is
// SERVER-computed (both clients merely encode the request and decode the reply),
// so on a quiescent shared cluster the pure-Go client and libfdb_c must return
// identical values; a divergence is a request-encode or reply-decode bug.
//
// These call methods that live on the concrete Transaction (not the
// WritableTransaction/ReadTransaction interfaces), so they use CreateTransaction
// directly rather than the Transact closure.
//
// The other two axes the RFC lists — commit_unknown_result (1021) idempotency and
// cross-shard range-merge — are NOT covered here: forcing 1021 needs wire fault
// injection the cgo binding can't do in this harness, and a cross-shard merge needs
// a multi-shard cluster the single-node test container doesn't provide. Those stay
// client-only / go-only coverage.

func TestDifferential_LocalityGetAddressesForKey(t *testing.T) {
	t.Parallel()
	key := []byte(t.Name() + "_key")

	if _, err := goClient.Transact(func(tx gofdb.WritableTransaction) (any, error) {
		tx.Set(gofdb.Key(key), []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	goTr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	goAddrs, err := goTr.LocalityGetAddressesForKey(gofdb.Key(key)).Get()
	if err != nil {
		t.Fatalf("go LocalityGetAddressesForKey: %v", err)
	}
	cTr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	cAddrs, err := cTr.LocalityGetAddressesForKey(cgofdb.Key(key)).Get()
	if err != nil {
		t.Fatalf("cgo LocalityGetAddressesForKey: %v", err)
	}

	goAddrs = append([]string(nil), goAddrs...)
	cAddrs = append([]string(nil), cAddrs...)
	sort.Strings(goAddrs)
	sort.Strings(cAddrs)
	if len(goAddrs) == 0 {
		t.Fatal("go returned no storage-server addresses")
	}
	if len(goAddrs) != len(cAddrs) {
		t.Fatalf("address count differs: go=%v cgo=%v", goAddrs, cAddrs)
	}
	for i := range goAddrs {
		if goAddrs[i] != cAddrs[i] {
			t.Fatalf("address[%d] differs: go=%q cgo=%q", i, goAddrs[i], cAddrs[i])
		}
	}
}

func TestDifferential_GetEstimatedRangeSizeBytes(t *testing.T) {
	t.Parallel()
	prefix := t.Name() + "_"
	begin := gofdb.Key(prefix)
	end := gofdb.Key(prefix + "\xff")
	cbegin := cgofdb.Key(prefix)
	cend := cgofdb.Key(prefix + "\xff")

	if _, err := goClient.Transact(func(tx gofdb.WritableTransaction) (any, error) {
		for i := 0; i < 50; i++ {
			tx.Set(gofdb.Key(prefix+string(rune('a'+i%26))+string(rune('0'+i/26))), make([]byte, 200))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	goTr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	goSize, err := goTr.GetEstimatedRangeSizeBytes(gofdb.KeyRange{Begin: begin, End: end}).Get()
	if err != nil {
		t.Fatalf("go GetEstimatedRangeSizeBytes: %v", err)
	}
	cTr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	cSize, err := cTr.GetEstimatedRangeSizeBytes(cgofdb.KeyRange{Begin: cbegin, End: cend}).Get()
	if err != nil {
		t.Fatalf("cgo GetEstimatedRangeSizeBytes: %v", err)
	}
	if goSize != cSize {
		t.Fatalf("estimated size differs: go=%d cgo=%d", goSize, cSize)
	}
}

func TestDifferential_GetRangeSplitPoints(t *testing.T) {
	t.Parallel()
	prefix := t.Name() + "_"
	begin := gofdb.Key(prefix)
	end := gofdb.Key(prefix + "\xff")
	cbegin := cgofdb.Key(prefix)
	cend := cgofdb.Key(prefix + "\xff")

	if _, err := goClient.Transact(func(tx gofdb.WritableTransaction) (any, error) {
		for i := 0; i < 20; i++ {
			tx.Set(gofdb.Key(prefix+string(rune('a'+i))), make([]byte, 100))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const chunk = 1 << 20 // 1 MiB — larger than the data, so a small/empty split set
	goTr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	goPts, err := goTr.GetRangeSplitPoints(gofdb.KeyRange{Begin: begin, End: end}, chunk).Get()
	if err != nil {
		t.Fatalf("go GetRangeSplitPoints: %v", err)
	}
	cTr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	cPts, err := cTr.GetRangeSplitPoints(cgofdb.KeyRange{Begin: cbegin, End: cend}, chunk).Get()
	if err != nil {
		t.Fatalf("cgo GetRangeSplitPoints: %v", err)
	}
	if len(goPts) != len(cPts) {
		t.Fatalf("split-point count differs: go=%d cgo=%d", len(goPts), len(cPts))
	}
	for i := range goPts {
		if string(goPts[i]) != string(cPts[i]) {
			t.Fatalf("split-point[%d] differs: go=%x cgo=%x", i, goPts[i], cPts[i])
		}
	}
}
