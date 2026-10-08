package embedded

import (
	"strings"
	"testing"
)

// Java's covering mapping copies each index key column into a field of the
// queried record (ScanWithFetchMatchCandidate.computeIndexEntryToLogicalRecord);
// the version key's __ROW_VERSION is no field of the record, so an index over
// a version has no covering scan and Java fetches through I1, in primary-key
// order within a col1 tie (versions-tests.yamsql).
func TestPlanHarness_VersionIndexHasNoCoveringScan(t *testing.T) {
	t.Parallel()
	const ddl = `create table t1(id bigint, col1 bigint, col2 bigint, primary key(id)) ` +
		`create index i1 as select col1 from t1 ` +
		`create index t1_version_index as select "__ROW_VERSION" from t1 ` +
		`create index grouped_version_index as select col1, "__ROW_VERSION" from t1 order by col1, "__ROW_VERSION" ` +
		`with options (store_row_versions=true)`
	for _, sql := range []string{
		`select col1, id, "__ROW_VERSION" from t1 order by col1`,
		`select col1, id from t1 order by col1, "__ROW_VERSION"`,
	} {
		plan, err := PlanQueryForTest(sql, ddl, nil)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if strings.Contains(plan, "VERSION_INDEX") && strings.Contains(plan, "COVERING") {
			t.Fatalf("%s planned a covering scan of a version index: %s", sql, plan)
		}
	}
}
