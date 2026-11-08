package sqlbackend

import (
	"strings"
	"testing"
)

// Open refuses --bulk-index-conflicts on the write paths that do not run the
// SpanWriter, before it reaches the cluster: dynamic rows and SQL INSERT.
func TestOpenRefusesBulkConflictsOffSpanWriter(t *testing.T) {
	t.Parallel()
	cfg := testConfig("sqlb_bulk_refusal")
	cfg.BulkIndexConflicts = true
	for name, c := range map[string]struct {
		opts Options
		want string
	}{
		"dynamic rows":    {Options{Rows: RowsDynamic}, "--bulk-index-conflicts requires direct ingest of compiled rows"},
		"SQL INSERT":      {Options{Ingest: IngestInsert}, "--bulk-index-conflicts requires direct ingest of compiled rows"},
		"SpanWriter (ok)": {Options{}, "explicit owned FDB cluster file required"},
	} {
		if _, err := Open(t.Context(), cfg, "", c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Open = %v, want %q", name, err, c.want)
		}
	}
}
