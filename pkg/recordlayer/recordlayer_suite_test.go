package recordlayer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
	. "github.com/onsi/gomega"

	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var (
	sharedContainer    *foundationdbtc.Container
	sharedDB           *FDBDatabase
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

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
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

	clusterTmpFilePath = tmpFile.Name()
	fdb.MustAPIVersion(720)
	db, err := fdb.OpenDatabase(clusterTmpFilePath)
	Expect(err).NotTo(HaveOccurred())
	sharedDB = NewFDBDatabase(db)
})

var _ = SynchronizedAfterSuite(func() {
	// All processes: clean up temp cluster file
	if clusterTmpFilePath != "" {
		_ = os.Remove(clusterTmpFilePath)
	}
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

// Write Ginkgo's per-spec JUnit XML report to Bazel's undeclared test outputs
// directory. This gives the test-report tool individual spec granularity —
// Bazel's rules_go wrapper only sees the single TestRecordLayer bootstrap function.
var _ = ReportAfterSuite("ginkgo junit report", func(report Report) {
	dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if dir == "" {
		return
	}
	reporters.GenerateJUnitReport(report, dir+"/ginkgo-report.xml")
})
