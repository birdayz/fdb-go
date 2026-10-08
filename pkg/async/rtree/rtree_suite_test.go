package rtree

import (
	"context"
	"os"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var (
	sharedContainer    *foundationdbtc.Container
	sharedDB           testDatabase
	clusterTmpFilePath string
)

// testDatabase and testContext give the specs the record layer's
// Run/Transaction shape over a bare fdb.Database.
type testDatabase struct{ db fdb.Database }

type testContext struct{ tr fdb.WritableTransaction }

func (c *testContext) Transaction() fdb.WritableTransaction { return c.tr }

func (d testDatabase) Run(ctx context.Context, fn func(*testContext) (any, error)) (any, error) {
	return d.db.TransactCtx(ctx, func(tr fdb.WritableTransaction) (any, error) {
		return fn(&testContext{tr: tr})
	})
}

// specSubspace returns a unique subspace for the current spec.
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
	tmpFile, err := os.CreateTemp("", "fdb_cluster_*.txt")
	Expect(err).NotTo(HaveOccurred())
	_, err = tmpFile.Write(data)
	Expect(err).NotTo(HaveOccurred())
	Expect(tmpFile.Close()).To(Succeed())
	clusterTmpFilePath = tmpFile.Name()
	fdb.MustAPIVersion(730)
	db, err := fdb.OpenDatabase(clusterTmpFilePath)
	Expect(err).NotTo(HaveOccurred())
	sharedDB = testDatabase{db: db}
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

func TestRTree(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "R-Tree Suite")
}
