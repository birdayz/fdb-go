package fdbmetrics_test

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdbmetrics"
)

// The live integration is one line next to any *client.Database:
//
//	http.Handle("/metrics", fdbmetrics.Handler(db))
//
// WriteText renders a single snapshot wherever an io.Writer goes.
func ExampleWriteText() {
	var buf bytes.Buffer
	fdbmetrics.WriteText(&buf, client.ClientMetricsSnapshot{TransactionsNotCommitted: 2})
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "fdb_client_transactions_not_committed_total") {
			fmt.Println(line)
		}
	}
	// Output: fdb_client_transactions_not_committed_total 2
}
