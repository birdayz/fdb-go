package factorycorpus_test

import (
	"sync"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// loadCorpus returns the committed corpus, loaded ONCE per test binary and
// shared read-only between the parallel gate tests.
//
// Six gates each loading the full corpus independently was fine at 2000
// scenarios and stopped being fine at 5000 under the race lane: the parallel
// loads peaked near 7 GB of instrumented heap (the kernel OOM-killed the
// binary at anon-rss 6.9 GB on a loaded box) and multiplied the runtime the
// same way. The gates only READ the loaded scenarios — the race detector
// itself now enforces that, since any test mutating the shared slice races
// every other gate.
var corpusOnce = sync.OnceValues(func() ([]*factorycorpus.Scenario, error) {
	return factorycorpus.LoadDir(factorycorpus.TestdataDir)
})

func loadCorpus(t *testing.T) []*factorycorpus.Scenario {
	t.Helper()
	scenarios, err := corpusOnce()
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return scenarios
}
