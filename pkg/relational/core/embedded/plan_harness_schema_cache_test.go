package embedded

import "testing"

// TestHarnessMetaDataIsBuiltOncePerDDL: the harness reuses a DDL's meta-data,
// never serves one DDL's meta-data for another, and does not remember a DDL
// that failed to build.
func TestHarnessMetaDataIsBuiltOncePerDDL(t *testing.T) {
	t.Parallel()
	ddl := func(table string) string {
		return "CREATE TABLE " + table + " (ID BIGINT, V BIGINT, PRIMARY KEY (ID))"
	}
	first, err := harnessMetaData(ddl("CACHE_A"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := harnessMetaData(ddl("CACHE_A"))
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("the same DDL rebuilt its meta-data")
	}
	other, err := harnessMetaData(ddl("CACHE_B"))
	if err != nil {
		t.Fatal(err)
	}
	if other == first || other.GetRecordType("CACHE_B") == nil || other.GetRecordType("CACHE_A") != nil {
		t.Error("a different DDL was served another DDL's meta-data")
	}
	broken := "CREATE TABLE CACHE_BROKEN (ID BIGINT, PRIMARY KEY (MISSING))"
	for range 2 {
		if _, err := harnessMetaData(broken); err == nil {
			t.Fatal("a DDL that cannot build produced meta-data")
		}
	}
	harnessSchemas.Lock()
	_, cached := harnessSchemas.byDDL[broken]
	harnessSchemas.Unlock()
	if cached {
		t.Error("a failed build was cached")
	}
}
