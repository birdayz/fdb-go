//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
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
	opts := append(runOptions(foundationdbtc.Run), foundationdbtc.WithAPIVersion(720), foundationdbtc.WithDirectIP())
	knobs := serverKnobs()
	GinkgoWriter.Printf("FDB server knobs from CONFORMANCE_FDB_KNOBS: %q\n", knobs)
	for _, k := range knobs {
		opts = append(opts, foundationdbtc.WithKnob(k[0], k[1]))
	}
	sharedContainer, err = foundationdbtc.Run(setupCtx, "", opts...)
	Expect(err).NotTo(HaveOccurred())
	for _, k := range knobs {
		expectServerKnob(setupCtx, sharedContainer, k[0], k[1])
	}

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
		Expect(foundationdbtc.Terminate(suiteCtx, sharedContainer)).To(Succeed())
	}
})

// runOptions returns an empty slice of run's option type, so options can be appended without naming it.
func runOptions[E any](_ func(context.Context, string, ...E) (*foundationdbtc.Container, error)) []E {
	return nil
}

// serverKnobs parses CONFORMANCE_FDB_KNOBS ("name=value,..."), set by a target's env.
func serverKnobs() [][2]string {
	var knobs [][2]string
	for _, kv := range strings.Split(os.Getenv("CONFORMANCE_FDB_KNOBS"), ",") {
		if kv == "" {
			continue
		}
		name, value, ok := strings.Cut(kv, "=")
		Expect(ok).To(BeTrue(), "CONFORMANCE_FDB_KNOBS entry %q is not name=value", kv)
		knobs = append(knobs, [2]string{name, value})
	}
	return knobs
}

// expectServerKnob waits until an fdbserver runs with the knob (fdb.bash passes it as "--knob_name value");
// recovery may restart the process.
func expectServerKnob(ctx context.Context, c *foundationdbtc.Container, name, value string) {
	want := `(^|\s)--knob_` + regexp.QuoteMeta(name) + `[= ]` + regexp.QuoteMeta(value) + `(\s|$)`
	Eventually(func() string {
		_, out, err := c.Exec(ctx, []string{
			"/bin/bash", "-c",
			`pgrep fdbserver | while read pid; do tr '\0' ' ' < /proc/$pid/cmdline; echo; done`,
		})
		if err != nil {
			return err.Error()
		}
		b, _ := io.ReadAll(out)
		return string(b)
	}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(MatchRegexp(want))
}

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
