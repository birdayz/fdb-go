package catalog

import (
	"bytes"
	"testing"
)

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

// TestDefaultCatalogSubspaceBytes pins the exact wire prefix of the
// catalog. Java's RelationalKeyspaceProvider declares:
//
//	KeySpaceDirectory(SYS,     KeyType.NULL)
//	  KeySpaceDirectory(SYS,   KeyType.NULL)
//	    KeySpaceDirectory(CATALOG, KeyType.LONG, 0L)
//
// So the catalog's on-disk subspace prefix is tuple (NULL, NULL, 0L) which
// packs to `0x00 0x00 0x14` in the FDB tuple layer (0x00 = NULL marker,
// 0x14 = integer-zero marker). Any deviation means a Go-written catalog
// cannot be read by Java and vice versa.
func TestDefaultCatalogSubspaceBytes(t *testing.T) {
	t.Parallel()
	got := DefaultCatalogSubspace().Bytes()
	want := []byte{0x00, 0x00, 0x14}
	if !bytes.Equal(got, want) {
		t.Errorf("DefaultCatalogSubspace() = %x, want %x — breaks Java cross-language compat", got, want)
	}
}
