package fdb

import (
	"fmt"
	"runtime/debug"

	"fdb.dev/pkg/fdbgo/internal/diag"
)

// recoverFuturePanic is the RFC-110 backstop for the facade-future goroutines
// (runFuture, behind newFutureByteSlice/Nil/Key/Int64/KeyArray/StringSlice); fn() runs client
// read/encode on a DETACHED goroutine, so a panic there would abort the whole
// host — and panicToError (transaction.go) does NOT cover it: that recover runs
// on the CALLER's goroutine after the future already resolved, a different
// stack. Deferred before the result is published (so it runs first, LIFO), this converts
// the panic into the future's error — the caller's Get()/MustGet() sees a normal
// error, the process survives. Matches libfdb_c, where a value future is
// resolved with an error by the network thread rather than crashing it.
func recoverFuturePanic(setErr func(error)) {
	r := recover()
	if r == nil {
		return
	}
	err := fmt.Errorf("fdbgo: panic resolving future: %v", r)
	diag.Recovered("fdbgo: recovered panic in client future goroutine",
		"goroutine", "future",
		"err", err.Error(),
		"stack", string(debug.Stack()),
	)
	setErr(err)
}
