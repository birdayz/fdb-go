package ddl

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestGenerate_NilMetadataFailsLoud pins RFC-202 D4's metadata guard: the
// plan visitor silently degrades to a catalog-less text builder when its
// metadata is nil, and an index generated from that plan would be built from
// unresolved names. The generator must refuse, never degrade.
func TestGenerate_NilMetadataFailsLoud(t *testing.T) {
	t.Parallel()
	op := logical.NewScan("T1", "T1")
	_, err := Generate(op, nil, Options{})
	if err == nil {
		t.Fatal("Generate accepted nil metadata — the catalog-less plan fallback " +
			"is reachable from index DDL and the index would be built from " +
			"unresolved names (RFC-202 D4 mutation 13)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError {
		t.Fatalf("want internal-error code, got %v", err)
	}
}
