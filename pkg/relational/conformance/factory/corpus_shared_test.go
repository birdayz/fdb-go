package factory_test

import (
	"sync"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// loadCorpus returns the committed corpus, loaded ONCE per test binary and
// shared read-only between the parallel gates (determinism, orderedness).
//
// Each independent load holds the full 5000-scenario corpus as parsed value
// trees, and the race lane multiplies that heap severalfold — two parallel
// loads plus the per-file canonicality walk peaked past 4 GB of instrumented
// RSS, which is an OOM on a shared box before it is a timeout anywhere. The
// gates only READ the loaded scenarios; the race detector enforces that for
// every future edit.
var corpusOnce = sync.OnceValues(func() ([]*factorycorpus.Scenario, error) {
	return factorycorpus.LoadDir(corpusDir)
})

func loadCorpus(t *testing.T) []*factorycorpus.Scenario {
	t.Helper()
	scenarios, err := corpusOnce()
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return scenarios
}
