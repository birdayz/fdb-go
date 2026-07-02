//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
)

// TestLibFDBC_LocalityNegativeLimit pins that the libfdb_c backend
// must match the pure-Go backend's limit validation EXACTLY, not panic and not
// silently diverge. Apple's binding uses a `limit != 0` form that drives
// make([]Key, size) negative and PANICS on any negative limit. Pure-Go (via
// RangeOptions) treats -1/0 as unlimited and rejects limit < -1 as
// range_limits_invalid (2012). The backend reproduces that exactly: -1 → unlimited
// (no panic), < -1 → 2012 (no panic, no build-tag divergence). Differential against
// pure-Go so the < -1 case is proven equal, not just non-panicking.
func TestLibFDBC_LocalityNegativeLimit(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)

	be, err := libfdbc.Open(clusterFile) // also sets the pure-Go facade API version to 730
	if err != nil {
		t.Fatalf("open libfdb_c backend: %v", err)
	}
	defer be.Close()
	goRaw, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open pure-Go database: %v", err)
	}
	defer goRaw.Close()
	rng := fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}

	// limit=-1 (ROW_LIMIT_UNLIMITED): unlimited, no panic, no error — on both.
	if _, err := be.LocalityGetBoundaryKeys(rng, -1, 0); err != nil {
		t.Fatalf("libfdb_c limit=-1 should be unlimited, got: %v", err)
	}
	if _, err := goRaw.LocalityGetBoundaryKeys(rng, -1, 0); err != nil {
		t.Fatalf("pure-Go limit=-1 should be unlimited, got: %v", err)
	}

	// limit < -1: range_limits_invalid (2012) on BOTH backends (no panic, no
	// silent success — the pre-fix build-tag divergence).
	cgoErr := mustErrCode(t, func() error { _, e := be.LocalityGetBoundaryKeys(rng, -5, 0); return e })
	goErr := mustErrCode(t, func() error { _, e := goRaw.LocalityGetBoundaryKeys(rng, -5, 0); return e })
	if cgoErr != 2012 || goErr != 2012 {
		t.Fatalf("limit<-1 should be range_limits_invalid (2012) on both; libfdb_c=%d pure-Go=%d", cgoErr, goErr)
	}
}

// mustErrCode runs fn, fails the test if it returns nil (or panics), and returns
// the fdb.Error code otherwise.
func mustErrCode(t *testing.T, fn func() error) int {
	t.Helper()
	err := fn()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var fe fdb.Error
	if !errors.As(err, &fe) {
		t.Fatalf("expected an fdb.Error, got %T: %v", err, err)
	}
	return fe.Code
}

// TestLibFDBC_LocalityGetBoundaryKeys proves the libfdb_c backend exposes the FDB
// locality API (shard boundary keys) — the capability the online MUTUAL indexer
// uses to partition the keyspace for concurrent building. Before
// BackendDatabase.LocalityGetBoundaryKeys existed, mutual indexing on a non-pure-Go
// backend silently degraded to a single fragment (it called the concrete pure-Go
// db, guarded by IsValid()).
//
// It's a differential: both clients read the SAME \xff/keyServers system range on
// the SAME cluster, so the boundary keys MUST be byte-identical. (On a single-node
// testcontainer both return an empty set — no shard splits — which is still a valid
// agreement and proves the libfdb_c call succeeds rather than erroring.)
func TestLibFDBC_LocalityGetBoundaryKeys(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)

	// Open libfdb_c FIRST so it sets the shared pure-Go facade API version to 730
	// (see TestLibFDBC_RecordLayerDifferential).
	cgoBackend, err := libfdbc.Open(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c backend: %v", err)
	}
	defer cgoBackend.Close()

	goRaw, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open pure-Go database: %v", err)
	}
	defer goRaw.Close()

	rng := fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}

	cgoKeys, err := cgoBackend.LocalityGetBoundaryKeys(rng, 0, 0)
	if err != nil {
		t.Fatalf("libfdb_c LocalityGetBoundaryKeys: %v", err)
	}
	goKeys, err := goRaw.LocalityGetBoundaryKeys(rng, 0, 0)
	if err != nil {
		t.Fatalf("pure-Go LocalityGetBoundaryKeys: %v", err)
	}

	// Byte-identical boundaries on the same cluster (the wire/locality compat).
	if len(cgoKeys) != len(goKeys) {
		t.Fatalf("boundary count differs: libfdb_c=%d pure-Go=%d", len(cgoKeys), len(goKeys))
	}
	for i := range cgoKeys {
		if !bytes.Equal(cgoKeys[i], goKeys[i]) {
			t.Fatalf("boundary[%d] differs: libfdb_c=%x pure-Go=%x", i, cgoKeys[i], goKeys[i])
		}
	}
}
