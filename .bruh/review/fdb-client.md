---
lens: fdb-client
applies: ["pkg/fdbgo/**"]
references:
  - "FoundationDB C++ source at tag 7.3.77, the specification (clone if missing: git clone --depth 1 --branch 7.3.77 https://github.com/apple/foundationdb /tmp/fdbsrc). Key files: fdbclient/NativeAPI.actor.cpp (commit, GRV, read retry loops), fdbclient/ReadYourWrites.actor.cpp (RYW overlay, timeouts, onError, conflict ranges), fdbrpc/include/fdbrpc/LoadBalance.actor.h, fdbclient/include/fdbclient/Atomic.h, fdbclient/ClientKnobs.cpp, flow/error_definitions.h, fdbserver/storageserver.actor.cpp"
  - "DIVERGENCES.md and pkg/fdbgo/fdb/API_PARITY.md: declared, accepted differences"
---
`pkg/fdbgo` is a pure-Go FoundationDB client that must behave exactly like libfdb_c 7.3.77:
Go, C and Java applications share one cluster. A behavioural difference from the C++ client
is a defect unless the C++ source shows the C++ client does the same, or the difference is
declared in DIVERGENCES.md. Cite the C++ `file:line` for every verdict about client
behaviour; read the function, do not rely on memory.

### fdb-client/semantics — Transaction semantics match libfdb_c
Severity: high
Check: the changed path does what the corresponding C++ path does: GRV and read-version
reuse, retry loops and their limits, onError classification and backoff, timeout and
cancellation (what is a recorded cause, when it is delivered, what wins when several apply),
reset and option persistence across retries, commit and commit_unknown_result handling, RYW
reads over pending writes, key selector resolution, watches. Ordering matters: which check
runs first decides which error the caller sees.
Verify: find the C++ function for the path (grep /tmp/fdbsrc), trace the same input there,
and compare outcomes and error codes.
Not a finding: a Go-only extension (context cancellation, a Go API shape) that libfdb_c
cannot express, when it does not change behaviour libfdb_c callers rely on.

### fdb-client/wire — Bytes on the wire and in the database match
Severity: blocker
Check: request fields, flags, limits, versions, conflict ranges, mutations and their
serialization; error codes and the channel they arrive on (an ErrorOr root versus an inline
LoadBalancedReply error); key and value encodings. An extra, missing or reordered request,
a different conflict range or a different mutation stream is a defect even when the data
read back is the same.
Verify: compare with the C++ request construction and serialization; where the repository
has wire goldens (tools/clientbench/wiregolden), say which scenario covers it or that none does.
Not a finding: differences in reply-promise IDs, span IDs, batching and timing that C++
itself does not fix.

### fdb-client/error-codes — Error codes and retryability
Severity: high
Check: every error code a changed path returns, maps, absorbs or retries: the code number
(flow/error_definitions.h), whether onError retries it, whether the fdb.IsRetryable
predicate agrees, and whether read-path-only errors (1006 all_alternatives_failed,
wrong_shard_server) are absorbed instead of surfacing.
Verify: cite the C++ site that raises or classifies the code.
Not a finding: Go wrapping of an error that preserves its code for errors.As.

### fdb-client/concurrency — Futures, goroutines and the single network thread
Severity: high
Check: C++ runs the client on one network thread; Go does not. State C++ touches only on
that thread (options, conflict ranges, the write map, incarnation and lease state) needs
explicit synchronization in Go. Check goroutines started per operation are owned and
stopped on reset, cancel and close; that a reply or timer callback cannot act on a
successor transaction; that locks are not held across callbacks or blocking waits.
Verify: name the two goroutines and the shared state, and the interleaving.
Not a finding: state documented as owned by one goroutine.

### fdb-client/tests — Client tests are deterministic and prove the claim
Severity: medium
Check: real FoundationDB via testcontainers, not mocks; races pinned with deterministic
fault injection (dropped replies, gated replies, manual timeout queues) rather than sleeps
or tight timeouts; fault tests inject the literal code the server sends, not the code under
test's own constant; a divergence fix has a red-to-green regression, preferably a libfdb_c
differential (pkg/fdbgo/bench, pkg/fdbgo/libfdbc) or wire golden.
Verify: show how the test would pass without the fix, or flake.
Not a finding: pure unit tests of a helper with no timing.
