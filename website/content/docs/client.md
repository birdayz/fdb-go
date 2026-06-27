---
title: The Client
weight: 2
---

`fdb.dev/pkg/fdbgo` is a from-scratch implementation of the FoundationDB client — it speaks the
FoundationDB **7.3** wire protocol directly (validated against `libfdb_c` 7.3.77). It is the default
backend: a normal `go build` pulls in **no cgo and no C library**, producing a static binary.

## Two backends, one API

Application code opens through a backend-agnostic entry point:

```go
import "fdb.dev/pkg/fdbgo/fdbclient"

db, err := fdbclient.Open("/etc/foundationdb/fdb.cluster")
```

A **build tag** selects the implementation — the choice is static per binary, there is no runtime
flag:

```sh
go build ./...                        # default: the pure-Go client (no cgo)
CGO_ENABLED=1 go build -tags libfdbc  # Apple's libfdb_c client
```

`fdbclient.Backend` reports which one a binary carries (`"pure-go"` / `"libfdb_c"`). Both read and
write **byte-identical** records, index entries, and continuations against the same cluster —
verified by a cross-backend differential suite — so you can switch the tag and keep sharing data.

## What it implements

Read-your-writes, transaction retry with `commit_unknown_result` handling, key selectors, GRV
batching, range reads with streaming modes, and the wire encoding. Correctness is checked against
`libfdb_c` by a binding-stress tester (randomized op sequences, replayable by seed).

## Limits (inherited from FDB)

5 s transaction time limit, 10 MB transaction size, 100 KB value size, ~10 KB key size. The Record
Layer handles large values via split records; cursors handle long scans via continuations.

{{< callout type="warning" >}}
  The FDB wire protocol is not a stable third-party contract — it changes between releases. The
  client is pinned to **7.3**; FDB 8.0 support is future work.
{{< /callout >}}
