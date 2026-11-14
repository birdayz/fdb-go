//go:build cgo && libfdbc_backend

package main

import (
	"slices"
	"testing"
)

// This test binary is built as control-native is: libfdbc_backend with cgo, the real
// opener over the pinned client, admitting only go_native ingest.
func TestCompiledFlavorIsNative(t *testing.T) {
	t.Parallel()
	if !compiledFlavor.native || !slices.Equal(compiledFlavor.tags, []string{"libfdbc_backend"}) || !slices.Equal(productionRuntime().flavor.tags, compiledFlavor.tags) {
		t.Fatalf("compiled flavor %+v", compiledFlavor)
	}
	if native == nil || native.pin != clientSHA256 || native.open == nil {
		t.Fatal("the native opener is not compiled in")
	}
	if err := compiledFlavor.admits(&job{Mode: "ingest", ClientBackend: "native_c"}); err != nil {
		t.Fatalf("control-native refused a native ingest: %v", err)
	}
	for _, j := range []*job{
		{Mode: "ingest", ClientBackend: "pure_go"},
		{Mode: "corpus"},
		{Mode: "setup", ClientBackend: "native_c"},
		{Mode: "verify", ClientBackend: "native_c"},
	} {
		if compiledFlavor.admits(j) == nil {
			t.Fatalf("control-native admitted %s on %s", j.Mode, j.ClientBackend)
		}
	}
}
