package functions

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

func TestResolveQualifiedTableName_Unqualified(t *testing.T) {
	t.Parallel()
	got, err := ResolveQualifiedTableName("ORDERS", "MYSCHEMA")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ORDERS" {
		t.Fatalf("got %q, want %q", got, "ORDERS")
	}
}

func TestResolveQualifiedTableName_MatchingQualifier(t *testing.T) {
	t.Parallel()
	got, err := ResolveQualifiedTableName("MYSCHEMA.ORDERS", "MYSCHEMA")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ORDERS" {
		t.Fatalf("got %q, want %q", got, "ORDERS")
	}
}

func TestResolveQualifiedTableName_CaseInsensitiveQualifier(t *testing.T) {
	t.Parallel()
	got, err := ResolveQualifiedTableName("myschema.ORDERS", "MYSCHEMA")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ORDERS" {
		t.Fatalf("got %q, want %q", got, "ORDERS")
	}
}

func TestResolveQualifiedTableName_CaseInsensitiveSchemaLower(t *testing.T) {
	t.Parallel()
	got, err := ResolveQualifiedTableName("MYSCHEMA.ORDERS", "myschema")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ORDERS" {
		t.Fatalf("got %q, want %q", got, "ORDERS")
	}
}

func TestResolveQualifiedTableName_WrongQualifier(t *testing.T) {
	t.Parallel()
	_, err := ResolveQualifiedTableName("OTHERSCHEMA.ORDERS", "MYSCHEMA")
	if err == nil {
		t.Fatal("expected error for wrong qualifier")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T: %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUndefinedDatabase {
		t.Fatalf("expected SQLSTATE %s, got %s", api.ErrCodeUndefinedDatabase, apiErr.Code)
	}
}

func TestResolveQualifiedTableName_MultiPartQualifier(t *testing.T) {
	t.Parallel()
	_, err := ResolveQualifiedTableName("A.B.C", "MYSCHEMA")
	if err == nil {
		t.Fatal("expected error for multi-part qualifier")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T: %v", err, err)
	}
	if apiErr.Code != api.ErrCodeInternalError {
		t.Fatalf("expected SQLSTATE %s, got %s", api.ErrCodeInternalError, apiErr.Code)
	}
}

func TestResolveQualifiedTableName_EmptyString(t *testing.T) {
	t.Parallel()
	got, err := ResolveQualifiedTableName("", "MYSCHEMA")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestResolveQualifiedTableName_EmptySchema(t *testing.T) {
	t.Parallel()
	// Qualifier present, schema empty → mismatch.
	_, err := ResolveQualifiedTableName("FOO.BAR", "")
	if err == nil {
		t.Fatal("expected error for qualifier with empty schema")
	}
}

func TestResolveQualifiedTableName_PreservesTableCase(t *testing.T) {
	t.Parallel()
	got, err := ResolveQualifiedTableName("MYSCHEMA.MyTable", "myschema")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "MyTable" {
		t.Fatalf("got %q, want %q (table case preserved)", got, "MyTable")
	}
}
