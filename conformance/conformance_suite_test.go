package conformance_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
)

var (
	sharedContainer *foundationdbtc.Container
	sharedDB        gofdb.Database
	suiteCtx        context.Context
)

var _ = BeforeSuite(func() {
	suiteCtx = context.Background()
	setupCtx, setupCancel := context.WithTimeout(suiteCtx, 2*time.Minute)
	defer setupCancel()
	var err error

	GinkgoWriter.Println("🚀 Starting shared FDB container for test suite...")

	// Start ONE container per parallel node
	sharedContainer, err = foundationdbtc.Run(setupCtx, "",
		foundationdbtc.WithAPIVersion(720),
		foundationdbtc.WithDirectIP(),
	)
	Expect(err).NotTo(HaveOccurred())

	// Open ONE database connection for the entire suite.
	// Pure Go client bootstrap is expensive (~1-2s per connection).
	// Reusing avoids 422 × bootstrap = 633s+ overhead.
	sharedDB, err = openGoDatabase(setupCtx, sharedContainer)
	Expect(err).NotTo(HaveOccurred())

	GinkgoWriter.Println("✅ Shared FDB container + database ready")
})

var _ = AfterSuite(func() {
	// Shut down Java conformance servers before terminating FDB container,
	// so they can perform graceful shutdown while FDB is still available.
	// CloseJavaInvoker handles the global singleton; CloseAllJavaServers is the
	// backstop that force-kills (process-group SIGKILL + reap) ANY server still
	// registered — e.g. an A3 pool server whose scenario panicked before
	// Retire, or an in-flight pool spawn — so no JVM outlives the suite.
	CloseJavaInvoker()
	CloseAllJavaServers()

	if sharedContainer != nil {
		GinkgoWriter.Println("🧹 Terminating shared FDB container...")
		_ = sharedContainer.Terminate(suiteCtx)
	}
})

func TestConformance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Java/Go Conformance Suite")
}

// Write a tree-structured JSON report to Bazel's undeclared test outputs.
var _ = ReportAfterSuite("ginkgo tree report", func(report Report) {
	dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if dir == "" {
		return
	}
	writeGinkgoTreeReport(report, dir+"/ginkgo-report.json")
})

type ginkgoTreeSpec struct {
	Containers []string `json:"containers"`
	Name       string   `json:"name"`
	State      string   `json:"state"`
	DurationMs float64  `json:"duration_ms"`
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
			continue
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
