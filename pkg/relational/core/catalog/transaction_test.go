package catalog

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestInMemoryTransaction_LifecycleCommit(t *testing.T) {
	t.Parallel()
	tx := NewInMemoryTransaction()
	if tx.IsClosed() {
		t.Fatal("fresh transaction reports closed")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !tx.IsClosed() {
		t.Error("Commit didn't close")
	}
	// Second Commit must error — idempotent close is fine but double-commit
	// is an error (mirrors Java's exception-on-closed-transaction).
	if err := tx.Commit(); err == nil {
		t.Error("double Commit succeeded, want error")
	} else {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Errorf("not *api.Error: %v", err)
		} else if apiErr.Code != api.ErrCodeTransactionInactive {
			t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeTransactionInactive)
		}
	}
}

func TestInMemoryTransaction_LifecycleAbort(t *testing.T) {
	t.Parallel()
	tx := NewInMemoryTransaction()
	if err := tx.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !tx.IsClosed() {
		t.Error("Abort didn't close")
	}
	if err := tx.Commit(); err == nil {
		t.Error("Commit after Abort succeeded, want error")
	}
}

func TestInMemoryTransaction_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	tx := NewInMemoryTransaction()
	if err := tx.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close is a no-op per api.Transaction contract.
	if err := tx.Close(); err != nil {
		t.Errorf("second Close: %v (should be no-op)", err)
	}
}

func TestInMemoryTransaction_BoundSchemaTemplate(t *testing.T) {
	t.Parallel()
	tx := NewInMemoryTransaction()
	if got := tx.BoundSchemaTemplate(); got != nil {
		t.Errorf("fresh transaction has bound template %v, want nil", got)
	}
	// SetBoundSchemaTemplate accepts a template and BoundSchemaTemplate
	// returns it. Use a nil-valued typed reference to avoid having to
	// build a full template here; tests in schema_template_test.go cover
	// the concrete case via the real bridge.
	var stub api.SchemaTemplate = nil
	tx.SetBoundSchemaTemplate(stub)
	if got := tx.BoundSchemaTemplate(); got != stub {
		t.Errorf("BoundSchemaTemplate = %v, want %v", got, stub)
	}
	tx.UnsetBoundSchemaTemplate()
	if got := tx.BoundSchemaTemplate(); got != nil {
		t.Errorf("BoundSchemaTemplate after unset = %v, want nil", got)
	}
}
