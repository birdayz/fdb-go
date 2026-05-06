package catalog

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestBuildCatalogMetaData_RecordTypes confirms the three record types
// are present with record-type keys that exactly match Java's
// SystemTableRegistry constants. A mismatch here would break
// cross-language compatibility: Java writes tuples keyed by 0/1/2, and
// if Go used different numeric keys the record store would silently
// misclassify records on load.
func TestBuildCatalogMetaData_RecordTypes(t *testing.T) {
	t.Parallel()
	md, err := BuildCatalogMetaData()
	if err != nil {
		t.Fatalf("BuildCatalogMetaData: %v", err)
	}

	cases := []struct {
		name string
		key  int64
	}{
		{SchemasRecordName, SchemaRecordTypeKey},
		{DatabasesRecordName, DatabaseInfoRecordTypeKey},
		{TemplatesRecordName, SchemaTemplateRecordTypeKey},
	}
	for _, tc := range cases {
		rt := md.GetRecordType(tc.name)
		if rt == nil {
			t.Fatalf("record type %s missing from catalog metadata", tc.name)
		}
		if !rt.HasExplicitRecordTypeKey() {
			t.Errorf("%s: expected explicit record type key", tc.name)
		}
		if got := rt.GetRecordTypeKey(); got != tc.key {
			t.Errorf("%s: record type key = %v, want %d", tc.name, got, tc.key)
		}
	}
}

// TestBuildCatalogMetaData_PrimaryKeys covers the tuple shape of each
// PK. The prefix is the record-type key (enforced in the record-type
// test above); the trailing columns are what the SQL layer uses to
// look up rows.
func TestBuildCatalogMetaData_PrimaryKeys(t *testing.T) {
	t.Parallel()
	md, err := BuildCatalogMetaData()
	if err != nil {
		t.Fatal(err)
	}

	// ColumnSize = 1 (record type) + N trailing columns. Matches
	// Java's concat(recordType(), ...fields) shape.
	cases := []struct {
		name           string
		wantColumnSize int
	}{
		{SchemasRecordName, 3},   // typeKey + DATABASE_ID + SCHEMA_NAME
		{DatabasesRecordName, 2}, // typeKey + DATABASE_ID
		{TemplatesRecordName, 3}, // typeKey + TEMPLATE_NAME + TEMPLATE_VERSION
	}
	for _, tc := range cases {
		rt := md.GetRecordType(tc.name)
		pk := rt.PrimaryKey
		if got := pk.ColumnSize(); got != tc.wantColumnSize {
			t.Errorf("%s: PrimaryKey.ColumnSize() = %d, want %d", tc.name, got, tc.wantColumnSize)
		}
	}
}

// TestBuildCatalogMetaData_Indexes confirms all three indexes land
// attached to the right record type and with the right index type.
// Without these, cross-language aggregation queries would silently
// return empty or stale results.
func TestBuildCatalogMetaData_Indexes(t *testing.T) {
	t.Parallel()
	md, err := BuildCatalogMetaData()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		indexName string
		rtName    string
		wantType  string
	}{
		{IdxTemplatesCount, SchemasRecordName, recordlayer.IndexTypeCount},
		{IdxTemplatesValue, SchemasRecordName, recordlayer.IndexTypeValue},
		{IdxDatabasesCount, DatabasesRecordName, recordlayer.IndexTypeCount},
	}
	for _, tc := range cases {
		idx := md.GetIndex(tc.indexName)
		if idx == nil {
			t.Fatalf("index %s missing", tc.indexName)
		}
		if idx.Type != tc.wantType {
			t.Errorf("%s: index type = %q, want %q", tc.indexName, idx.Type, tc.wantType)
		}
		// Confirm the index is attached to the expected record type.
		idxs := md.GetIndexesForRecordType(tc.rtName)
		found := false
		for _, i := range idxs {
			if i.Name == tc.indexName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: not attached to record type %s", tc.indexName, tc.rtName)
		}
	}
}
