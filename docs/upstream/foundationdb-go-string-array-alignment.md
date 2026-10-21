# Upstream issue draft: Go string-array futures assume aligned pointer slots

Draft for `apple/foundationdb`; not filed.

**Component:** Go binding `(*futureStringSlice).Get`,
`bindings/go/src/fdb/futures.go:438–456`, revision `37fcf1c8ce08`
(`v0.0.0-20250702211439-37fcf1c8ce08`), using libfdb_c 7.3.77.

## Observed failure

Calling `Transaction.LocalityGetAddressesForKey(key).Get()` under Go 1.26.10
with race instrumentation terminates the process:

```text
fatal error: checkptr: misaligned pointer conversion
runtime.checkptrAlignment
.../fdb.(*futureStringSlice).Get .../futures.go:453
```

The call was reading a real storage-server address in the Go port's
`TestDifferential_LocalityGetAddressesForKey`. This is a checkptr failure,
not a reported data race. Its enclosing target passed without instrumentation.
It reproduces on the Go port's master (`d211c3350`, Go 1.26.10, `-race`) with
the same fatal at `futures.go:453`. With the fix below, that differential
passes under `-race` at `ee05cf5b6`. The deterministic regression fails with
the same checkptr fatal against the original loop and passes with the fix,
with and without `-race`.

## Cause

At the binding revision above, line 453 reinterprets each C array slot as
`**C.char` and advances by a hard-coded eight bytes:

```go
ret[i] = C.GoString((*C.char)(*(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(strings)) + uintptr(i*8)))))
```

The C client does not guarantee alignment of this particular backing storage:

- In 7.3.77, `fdbclient/NativeAPI.actor.cpp:5745–5750` allocates the NUL-terminated
  address string in an arena, then appends its pointer to a `VectorRef` in the
  same arena. `VectorRef::reallocate` allocates the pointer array through arena
  placement new (`flow/include/flow/Arena.h:1151–1158`).
- `ArenaBlock::allocate` returns the current byte offset and advances by the
  requested byte count without alignment rounding (`flow/Arena.cpp:349–370`).
  Tiny blocks begin at a six-byte header (`flow/include/flow/Arena.h:175`).
  String length and block placement can therefore leave the pointer array
  unaligned even though the individual strings are valid.
- `fdb_future_get_string_array` exports the vector's backing address unchanged
  (`bindings/c/fdb_c.cpp:318–321`).

Go's pointer-containing typed conversion requires alignment
(`runtime/checkptr.go:15–20`). Successful uninstrumented execution on hardware
that permits unaligned loads does not make the conversion valid. Replacing the
arithmetic with `unsafe.Add` or an `unsafe.Slice` of pointers still requires
aligned pointer slots. The fixed eight-byte stride also assumes a 64-bit ABI.

## Suggested binding fix

Read each pointer representation with C `memcpy` into an aligned local pointer,
using `sizeof(pointer)` for the stride, then call `C.GoString` on that pointer.
Keep `runtime.KeepAlive(f.future)` until every string has been copied. This
avoids both Go's unaligned pointer-slot conversion and an unaligned typed load
in the helper. It does not change the C API, returned strings or future lifetime.

The Go port carries this boundary fix in
`patches/fdb-go-string-array-alignment.patch`, applied by its Bazel
`go_deps.module_override`. Its deterministic regression embeds the patched
binding's private decoder and supplies C-allocated arrays at every pointer
alignment residue, with three distinct values (including an empty string),
plus an empty-array case. The existing locality differential exercises the
real C future and checks equality with the pure-Go client.
