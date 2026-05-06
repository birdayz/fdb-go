package catalog

import "testing"

// TestSystemTableConstants pins Java-wire-compat values. Changing any
// of these makes existing FDB-backed catalog records unreadable.
func TestSystemTableConstants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		got      any
		wantInt  int64
		wantStr  string
		isString bool
	}{
		{"SchemaRecordTypeKey", SchemaRecordTypeKey, 0, "", false},
		{"DatabaseInfoRecordTypeKey", DatabaseInfoRecordTypeKey, 1, "", false},
		{"SchemaTemplateRecordTypeKey", SchemaTemplateRecordTypeKey, 2, "", false},
		{"SchemasTableName", SchemasTableName, 0, "SCHEMAS", true},
		{"DatabaseTableName", DatabaseTableName, 0, "DATABASES", true},
		{"SchemaTemplateTableName", SchemaTemplateTableName, 0, "TEMPLATES", true},
	}
	for _, tc := range cases {
		if tc.isString {
			if tc.got.(string) != tc.wantStr {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.wantStr)
			}
			continue
		}
		if tc.got.(int64) != tc.wantInt {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.wantInt)
		}
	}
}
