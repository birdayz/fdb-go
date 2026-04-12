package conformance_test

import (
	"context"
	"os"
	"testing"
	"time"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
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
	// Shut down Java conformance server before terminating FDB container,
	// so it can perform graceful shutdown while FDB is still available.
	CloseJavaInvoker()

	if sharedContainer != nil {
		GinkgoWriter.Println("🧹 Terminating shared FDB container...")
		_ = sharedContainer.Terminate(suiteCtx)
	}
})

func TestConformance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Java/Go Conformance Suite")
}

// Write Ginkgo's per-spec JUnit XML report to Bazel's undeclared test outputs.
var _ = ReportAfterSuite("ginkgo junit report", func(report Report) {
	dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if dir == "" {
		return
	}
	reporters.GenerateJUnitReport(report, dir+"/ginkgo-report.xml")
})
