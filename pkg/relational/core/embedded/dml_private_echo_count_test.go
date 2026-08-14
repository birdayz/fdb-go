package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
)

// UPDATE/INSERT/DELETE expose only RowsAffected. Their physical rows are
// private executor carriers, so counting one must not invoke the SELECT result
// adapter against public target-table columns. A nil ResultSet makes that
// boundary mutation-sensitive: removing the DML branch dereferences it while
// trying to read column ID.
func TestMaterializePageRowDMLCountsWithoutReadingThePrivateEcho(t *testing.T) {
	t.Parallel()

	row, err := materializePageRow(nil, []executor.ColumnDef{{Name: "ID"}}, true)
	if err != nil {
		t.Fatalf("materialize DML echo: %v", err)
	}
	if row == nil || len(row) != 0 {
		t.Fatalf("DML count token = %#v, want a non-nil zero-column row", row)
	}
}
