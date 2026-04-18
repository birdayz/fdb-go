package keyspace_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
)

func TestSchemaSubspace_EmptyPath(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub([]byte("test")))
	_, err := ks.SchemaSubspace("", "s1")
	if err == nil {
		t.Fatal("expected error for empty dbPath, got nil")
	}
}

func TestSchemaSubspace_EmptySchema(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub([]byte("test")))
	_, err := ks.SchemaSubspace("/db", "")
	if err == nil {
		t.Fatal("expected error for empty schemaName, got nil")
	}
}

func TestSchemaSubspace_Valid(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub([]byte("test")))
	ss, err := ks.SchemaSubspace("/mydb", "myschema")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ss == nil {
		t.Fatal("expected non-nil subspace")
	}
}

func TestCatalogSubspace_Distinct(t *testing.T) {
	t.Parallel()
	root := subspace.Sub([]byte("root"))
	ks := keyspace.New(root)
	catSS := ks.CatalogSubspace()
	schemaSS, _ := ks.SchemaSubspace("/db", "s1")
	if string(catSS.Bytes()) == string(schemaSS.Bytes()) {
		t.Error("catalog and schema subspaces must be distinct")
	}
}

func TestParseDBPath_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  []string
	}{
		{"/db", []string{"db"}},
		{"/domain/db", []string{"domain", "db"}},
	}
	for _, tc := range cases {
		parts, err := keyspace.ParseDBPath(tc.input)
		if err != nil {
			t.Errorf("ParseDBPath(%q): unexpected error %v", tc.input, err)
			continue
		}
		if len(parts) != len(tc.want) {
			t.Errorf("ParseDBPath(%q): got %v, want %v", tc.input, parts, tc.want)
		}
	}
}

func TestParseDBPath_Invalid(t *testing.T) {
	t.Parallel()
	cases := []string{"", "noslash", "/", "//double", "/a//b"}
	for _, c := range cases {
		if _, err := keyspace.ParseDBPath(c); err == nil {
			t.Errorf("ParseDBPath(%q): expected error, got nil", c)
		}
	}
}
