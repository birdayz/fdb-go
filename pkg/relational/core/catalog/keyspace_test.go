package catalog

import "testing"

// TestKeyspaceConstants pins the Java-compat string values. Changes
// here break wire compatibility with Java — if a test fails, the Java
// side has to change first.
func TestKeyspaceConstants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"SysConstant", SysConstant, "__SYS"},
		{"CatalogConstant", CatalogConstant, "CATALOG"},
		{"DBNameDir", DBNameDir, "dbName"},
		{"SchemaDir", SchemaDir, "schema"},
		{"DefaultSchemaDir", DefaultSchemaDir, "defaultSchema"},
		{"InterningLayer", InterningLayer, "__internedStrings"},
		{"InterningLayerValue", InterningLayerValue, "IL"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (Java wire-compat: RelationalKeyspaceProvider)", tc.name, tc.got, tc.want)
		}
	}
}
