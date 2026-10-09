package vectorindex

import (
	"context"
	"os"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var (
	sharedContainer *foundationdbtc.Container
	sharedDB        *recordlayer.FDBDatabase
	// sharedRawDB is the fdb.Database sharedDB wraps, its transactor.
	sharedRawDB        fdb.Database
	clusterTmpFilePath string
)

// specSubspace returns a unique subspace for the current spec, ensuring isolation
// across parallel specs. Uses the full spec description as the key.
func specSubspace() subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText()}.Pack())
}

var _ = SynchronizedBeforeSuite(func() []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
	Expect(err).NotTo(HaveOccurred())
	clusterFile, err := container.ClusterFile(ctx)
	Expect(err).NotTo(HaveOccurred())
	sharedContainer = container
	return []byte(clusterFile)
}, func(data []byte) {
	// fdb.OpenDatabase wants a cluster file path; ClusterFile returns its content.
	tmpFile, err := os.CreateTemp("", "fdb_cluster_*.txt")
	Expect(err).NotTo(HaveOccurred())
	_, err = tmpFile.Write(data)
	Expect(err).NotTo(HaveOccurred())
	Expect(tmpFile.Close()).To(Succeed())
	clusterTmpFilePath = tmpFile.Name()
	fdb.MustAPIVersion(730)
	db, err := fdb.OpenDatabase(clusterTmpFilePath)
	Expect(err).NotTo(HaveOccurred())
	sharedRawDB = db
	sharedDB = recordlayer.NewFDBDatabase(db)
})

var _ = SynchronizedAfterSuite(func() {
	if clusterTmpFilePath != "" {
		_ = os.Remove(clusterTmpFilePath)
	}
}, func() {
	if sharedContainer != nil {
		_ = sharedContainer.Terminate(context.Background())
	}
})

func TestVectorIndex(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Vector Index Suite")
}
