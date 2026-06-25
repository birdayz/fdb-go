//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/libfdbc"
)

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
