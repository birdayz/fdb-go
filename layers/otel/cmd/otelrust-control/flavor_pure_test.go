//go:build !cgo || !libfdbc_backend

package main

import (
	"slices"
	"testing"
)

// This test binary is built as control-pure is: no build tag, no cgo, the native stub.
func TestCompiledFlavorIsPure(t *testing.T) {
	t.Parallel()
	if compiledFlavor.native || compiledFlavor.tags == nil || len(compiledFlavor.tags) != 0 || !slices.Equal(productionRuntime().flavor.tags, []string{}) {
		t.Fatalf("compiled flavor %+v", compiledFlavor)
	}
	if db, rule, err := openNativeBackend(t.Context(), "/libfdb_c.so", "/fdb.cluster"); err == nil || db != nil || rule != "" {
		t.Fatalf("the stub opened %v, %q, %v", db, rule, err)
	}
	if compiledFlavor.admits(&job{Mode: "ingest", ClientBackend: "native_c"}) == nil {
		t.Fatal("control-pure admitted a native ingest")
	}
	for _, mode := range []string{"corpus", "setup", "verify"} {
		if err := compiledFlavor.admits(&job{Mode: mode, ClientBackend: "native_c"}); err != nil {
			t.Fatalf("control-pure refused %s of a native arm: %v", mode, err)
		}
	}
}
