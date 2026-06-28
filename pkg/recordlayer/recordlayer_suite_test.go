package recordlayer

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
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
		foundationdbtc.WithAPIVersion(730),
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
	// 730 (NOT 720): this package's FDBDatabaseFactory specs route through
	// fdbclient.Open, which under -tags libfdbc selects facade API 730
	// unconditionally. A 720 pin here would then fail those specs with
	// api_version_already_set (2201). 730 is also the 7.3.77 server's native
	// version and the testcontainer default. Keep this in sync with the container
	// WithAPIVersion above.
	fdb.MustAPIVersion(730)
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

// Write a tree-structured JSON report to Bazel's undeclared test outputs.
// Preserves Ginkgo's Describe/Context/It hierarchy for tree rendering in
// the test-report tool. JUnit XML flattens this hierarchy — we need it intact.
var _ = ReportAfterSuite("ginkgo tree report", func(report Report) {
	dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if dir == "" {
		return
	}
	writeGinkgoTreeReport(report, dir+"/ginkgo-report.json")
})

// ginkgoTreeSpec is a single spec with its container hierarchy preserved.
type ginkgoTreeSpec struct {
	// Containers is the Describe/Context path, e.g. ["SaveRecord", "with indexes"]
	Containers []string `json:"containers"`
	// Name is the leaf node text, e.g. "creates a new record"
	Name string `json:"name"`
	// State is "passed", "failed", "skipped", "pending", etc.
	State string `json:"state"`
	// DurationMs is the spec duration in milliseconds.
	DurationMs float64 `json:"duration_ms"`
}

func writeGinkgoTreeReport(report Report, path string) {
	var specs []ginkgoTreeSpec
	for _, spec := range report.SpecReports {
		if spec.LeafNodeType == types.NodeTypeBeforeSuite ||
			spec.LeafNodeType == types.NodeTypeAfterSuite ||
			spec.LeafNodeType == types.NodeTypeSynchronizedBeforeSuite ||
			spec.LeafNodeType == types.NodeTypeSynchronizedAfterSuite ||
			spec.LeafNodeType == types.NodeTypeReportAfterSuite ||
			spec.LeafNodeType == types.NodeTypeCleanupAfterSuite {
			continue // skip infrastructure nodes
		}
		specs = append(specs, ginkgoTreeSpec{
			Containers: spec.ContainerHierarchyTexts,
			Name:       spec.LeafNodeText,
			State:      spec.State.String(),
			DurationMs: spec.RunTime.Seconds() * 1000,
		})
	}
	data, err := json.Marshal(specs)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
