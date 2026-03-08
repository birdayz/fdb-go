package recordlayer

import (
	"context"
	"os"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var (
	sharedContainer *foundationdbtc.Container
	sharedDB        *FDBDatabase
)

// specSubspace returns a unique subspace for the current spec, ensuring isolation
// across parallel specs. Uses the full spec description as the key.
func specSubspace() subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText()}.Pack())
}

var _ = SynchronizedBeforeSuite(func() []byte {
	ctx := context.Background()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	Expect(err).NotTo(HaveOccurred())

	err = container.InitializeDatabase(ctx)
	Expect(err).NotTo(HaveOccurred())

	clusterFile, err := container.ClusterFile(ctx)
	Expect(err).NotTo(HaveOccurred())

	// Only visible to process #1
	sharedContainer = container

	return []byte(clusterFile)
}, func(data []byte) {
	// All processes: connect to the shared FDB container.
	// ClusterFile() returns the cluster content (e.g., "docker:docker@host:port"),
	// but fdb.OpenDatabase() expects a file path. Write to a temp file.
	clusterContent := string(data)
	tmpFile, err := os.CreateTemp("", "fdb_cluster_*.txt")
	Expect(err).NotTo(HaveOccurred())
	_, err = tmpFile.WriteString(clusterContent)
	Expect(err).NotTo(HaveOccurred())
	err = tmpFile.Close()
	Expect(err).NotTo(HaveOccurred())

	fdb.MustAPIVersion(720)
	db, err := fdb.OpenDatabase(tmpFile.Name())
	Expect(err).NotTo(HaveOccurred())
	sharedDB = NewFDBDatabase(db)
})

var _ = SynchronizedAfterSuite(func() {
	// All processes: nothing to clean up
}, func() {
	// Process #1 only: terminate container
	if sharedContainer != nil {
		_ = sharedContainer.Terminate(context.Background())
	}
})

func TestRecordLayer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Record Layer Suite")
}
