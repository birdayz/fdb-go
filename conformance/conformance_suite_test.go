package conformance_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var (
	sharedContainer *foundationdbtc.Container
	suiteCtx        context.Context
)

var _ = BeforeSuite(func() {
	suiteCtx = context.Background()
	var err error

	GinkgoWriter.Println("🚀 Starting shared FDB container for test suite...")

	// Start ONE container for the entire suite
	sharedContainer, err = foundationdbtc.Run(suiteCtx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	Expect(err).NotTo(HaveOccurred())

	err = sharedContainer.InitializeDatabase(suiteCtx)
	Expect(err).NotTo(HaveOccurred())

	// Ensure database is ready
	_, err = sharedContainer.GetFDBDatabase(suiteCtx)
	Expect(err).NotTo(HaveOccurred())

	GinkgoWriter.Println("✅ Shared FDB container ready")
})

var _ = AfterSuite(func() {
	if sharedContainer != nil {
		GinkgoWriter.Println("🧹 Terminating shared FDB container...")
		_ = sharedContainer.Terminate(suiteCtx)
	}
})

func TestConformance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Java/Go Conformance Suite")
}
