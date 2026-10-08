package testkit

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

// queryLabels are the labels the driver reports for sql's result columns,
// upper-cased for comparison.
func QueryLabels(t *testing.T, sql string, md *recordlayer.RecordMetaData) []string {
	t.Helper()
	labels, err := embedded.ResultColumnLabelsForQuery(sql, md)
	if err != nil {
		t.Fatalf("labels %q: %v", sql, err)
	}
	upper := make([]string, len(labels))
	for i, label := range labels {
		upper[i] = strings.ToUpper(label)
	}
	return upper
}
