package fdbmetrics_test

import (
	"bytes"
	"fmt"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdbmetrics"
)

// The live integration is one line next to any fdb.Database:
//
//	http.Handle("/metrics", fdbmetrics.Handler(db))
//
// WriteText renders a single snapshot wherever an io.Writer goes.
func ExampleWriteText() {
	var buf bytes.Buffer
	_ = fdbmetrics.WriteText(&buf, fdb.MetricsSnapshot{TransactionsNotCommitted: 2})
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "fdb_client_transactions_not_committed_total") {
			fmt.Println(line)
		}
	}
	// Output: fdb_client_transactions_not_committed_total 2
}
