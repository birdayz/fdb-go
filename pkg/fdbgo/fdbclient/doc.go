// Package fdbclient selects the FoundationDB client backend at BUILD time.
//
// Application code opens a database through fdbclient.Open and is otherwise
// backend-agnostic; a build tag — not a runtime flag — decides which client is
// compiled in. This is the same pattern the standard library uses to swap its
// cgo and pure-Go DNS resolvers (the netgo / netcgo tags) and the sqlite
// ecosystem uses to swap mattn/go-sqlite3 (cgo) for modernc.org/sqlite
// (pure-Go):
//
//	go build ./...                        // default: the from-scratch pure-Go client
//	CGO_ENABLED=1 go build -tags libfdbc  // Apple's decade-hardened libfdb_c client
//
// Exactly ONE backend is linked into the binary. A default build pulls in no
// cgo and never links libfdb_c; only -tags libfdbc imports pkg/fdbgo/libfdbc and
// the C library. The Backend constant reports which client this binary carries.
//
// Why build-time and not runtime: libfdb_c's network thread is initialized once
// per process and is unrecoverable, so the choice is physically static — a
// binary runs one client for its whole life. A build tag states that plainly and
// keeps the C dependency out of every build that does not ask for it.
//
// Both clients are wire-compatible (proven by the differential suite in
// pkg/fdbgo/libfdbc): they read and write byte-identical records, index entries,
// split records, and continuations, so the same data and the same cluster are
// shared across the two builds (and with Java/C apps).
package fdbclient
