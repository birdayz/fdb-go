// Package diag is the shared diagnostics sink for recovered panics in the
// pure-Go FDB client (the client and fdb-facade layers; transport keeps its own
// seriousLog). It is the analog of libfdb_c's Net2::run catch-all, which logs
// SevError "TaskError" and keeps the network thread alive (g_crashOnError is
// false in the client) — see RFC-110. Routing through slog.Default() at ERROR
// level makes these diagnostics pluggable via the standard Go mechanism
// (slog.SetDefault) with no fdbgo-specific logging API.
package diag

import "log/slog"

// Recovered reports a recovered panic in a long-lived/background client
// goroutine at ERROR level, passing structured attributes (not a pre-formatted
// string) so a JSON/structured handler gets queryable fields. It is a var so
// tests can capture it. Callers are responsible for rate-limiting (a recovered
// panic in a deterministically-broken loop would otherwise log every iteration);
// the storm signal is the recoveredPanics counter, not the log volume.
var Recovered = func(msg string, attrs ...any) {
	slog.Default().Error(msg, attrs...)
}
