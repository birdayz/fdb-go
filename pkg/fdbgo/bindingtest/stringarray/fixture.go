//go:build bazelrunfiles && cgo

// Package stringarray allocates deliberately unaligned C pointer arrays for the
// upstream Go binding's decoder regression.
package stringarray

/*
#include <stdlib.h>
#include <string.h>

static void* make_string_array(size_t offset) {
    const char* strings[] = {"172.17.0.2", "", "[2001:db8::1]:4500"};
    unsigned char* base = malloc(offset + sizeof(strings));
    if (base != NULL) {
        memcpy(base + offset, strings, sizeof(strings));
    }
    return base;
}
*/
import "C"

import "unsafe"

// Allocate returns an array of three C string pointers at offset bytes past a
// malloc-aligned address. The caller must release its storage after decoding it.
func Allocate(offset uintptr) (unsafe.Pointer, func()) {
	base := C.make_string_array(C.size_t(offset))
	if base == nil {
		return nil, func() {}
	}
	return unsafe.Add(base, offset), func() { C.free(base) }
}
