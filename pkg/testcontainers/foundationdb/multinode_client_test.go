package foundationdb_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	. "github.com/onsi/gomega"
)

// TestMultiNodeCluster_GoClientCRUD creates a 3-container FDB cluster (each on
// a distinct Docker bridge IP, all sharing port 4500) and exercises the pure Go
// FDB client through multiple write+read transactions.
//
// This is a regression test for the connection pool same-port aliasing bug:
// before the fix, getOrDialConn reused a TCP connection for any address with
// a matching port number. In a multi-node cluster the coordinator (10.0.1.10:4500),
// GRV proxy (10.0.1.11:4500), and commit proxy (10.0.1.12:4500) all share port
// 4500. The aliasing caused proxy requests to hit the coordinator process, where
// the endpoint token didn't match, leading to silent frame drops and GRV timeouts.
//
// With the fix, each unique ip:port gets its own TCP connection, matching C++
// FlowTransport's one-Peer-per-NetworkAddress model.
func TestMultiNodeCluster_GoClientCRUD(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cluster, err := foundationdb.RunCluster(ctx, 3,
		foundationdb.WithRedundancyMode("double"),
		foundationdb.WithStorageEngine("memory"),
	)
	g.Expect(err).NotTo(HaveOccurred(), "RunCluster")
	defer cluster.Terminate(ctx)

	// Log process IPs for debugging.
	for i, r := range cluster.Replicas {
		ip, _ := r.ContainerIP(ctx)
		t.Logf("replica %d: %s", i, ip)
	}

	// Verify cluster is healthy (RunCluster already waits for all processes,
	// but confirm we can reach fdbcli).
	status, err := cluster.Coordinator.FDBCLIExec(ctx, "status minimal")
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf("cluster status: %s", status)

	// Connect with our pure Go FDB client.
	path, err := cluster.Coordinator.ClusterFilePath(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf("cluster file path: %s", path)

	gofdb.MustAPIVersion(730)
	db, err := gofdb.OpenDatabase(path)
	g.Expect(err).NotTo(HaveOccurred(), "OpenDatabase")
	defer db.Close()

	// Run 20 write+read transactions. This exercises both GRV proxies (for read
	// versions) and commit proxies (for commits) across multiple cluster nodes.
	// With the old aliasing bug, proxy requests would hit the wrong process and
	// time out after 5 seconds.
	for i := 0; i < 20; i++ {
		key := gofdb.Key(tuple.Tuple{"multinode_test", fmt.Sprintf("key_%03d", i)}.Pack())
		value := []byte(fmt.Sprintf("value_%03d", i))

		// Write.
		_, err = db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			tr.Set(key, value)
			return nil, nil
		})
		g.Expect(err).NotTo(HaveOccurred(), "write tx %d", i)

		// Read back.
		result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			return tr.Get(key).MustGet(), nil
		})
		g.Expect(err).NotTo(HaveOccurred(), "read tx %d", i)
		g.Expect(string(result.([]byte))).To(Equal(string(value)), "read back mismatch at %d", i)
	}

	// Range scan to verify all 20 keys exist.
	result, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		prefix := tuple.Tuple{"multinode_test"}.Pack()
		rng, err := gofdb.PrefixRange(prefix)
		if err != nil {
			return nil, err
		}
		kvs, err := tr.GetRange(rng, gofdb.RangeOptions{Limit: 100}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		return len(kvs), nil
	})
	g.Expect(err).NotTo(HaveOccurred(), "range scan")
	g.Expect(result.(int)).To(Equal(20), "expected 20 keys from range scan")

	t.Logf("multi-node cluster CRUD: 20 write+read transactions + range scan OK")
}
