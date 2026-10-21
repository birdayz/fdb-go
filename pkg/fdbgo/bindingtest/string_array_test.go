//go:build bazelrunfiles && cgo

package fdb

import (
	"fmt"
	"slices"
	"testing"
	"unsafe"

	"fdb.dev/pkg/fdbgo/bindingtest/stringarray"
)

func TestStringSliceFromCUnalignedArray(t *testing.T) {
	t.Parallel()
	alignment := unsafe.Alignof(unsafe.Pointer(nil))
	for offset := uintptr(0); offset < alignment; offset++ {
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			t.Parallel()
			array, release := stringarray.Allocate(offset)
			defer release()
			if array == nil {
				t.Fatal("C pointer-array allocation failed")
			}
			if residue := uintptr(array) % alignment; residue != offset {
				t.Fatalf("pointer-array alignment residue = %d, want %d", residue, offset)
			}
			want := []string{"172.17.0.2", "", "[2001:db8::1]:4500"}
			if got := stringSliceFromC(array, len(want)); !slices.Equal(got, want) {
				t.Fatalf("decoded strings = %q, want %q", got, want)
			}
		})
	}
}

func TestStringSliceFromCEmptyArray(t *testing.T) {
	t.Parallel()
	if got := stringSliceFromC(nil, 0); got == nil || len(got) != 0 {
		t.Fatalf("empty C array decoded as %#v, want non-nil empty slice", got)
	}
}
