package sqldriver_test

// StoredQueryWarmUp is a public accessor for start-up metrics, so a caller may
// poll it while another goroutine's first Connect is still initialising the
// connector. The warm-up outcome is written inside that initialisation, so the
// read must synchronise with it. Run under the race lane (-race), the
// unsynchronised version reports a data race here.

import (
	"context"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestStoredQueryWarmUpConcurrentWithFirstConnect(t *testing.T) {
	t.Parallel()
	cf := testkit.ClusterFile()
	if cf == "" {
		t.Skip("FDB not available (no Docker)")
	}
	c, err := (&sqldriver.Driver{}).OpenConnector("fdbsql:///FRL/WARMUP_RACE?cluster_file=" + cf)
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	connector := c.(*sqldriver.Connector)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_ = connector.StoredQueryWarmUp()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	conn, err := connector.Connect(context.Background())
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_ = conn.Close()
	// After the first Connect returns, the warm-up outcome is published.
	_ = connector.StoredQueryWarmUp()
}
