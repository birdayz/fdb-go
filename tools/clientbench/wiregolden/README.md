# Wire goldens

`libfdbc.golden` is the normalized request transcript of the
[`wgscen`](../cmd/wgscen/main.go) scenarios on libfdb_c 7.3.77.
`go-divergences.diff` is how the Go client's transcript differs from it today.
[`scripts/wiregolden.sh`](../scripts/wiregolden.sh) captures both clients, renders
both transcripts ([`cmd/wiregolden`](../cmd/wiregolden), normalization in
[`internal/wiregolden`](../internal/wiregolden/wiregolden.go)) and fails if
libfdb_c no longer matches the golden or the Go divergences changed.

```
tools/clientbench/scripts/wiregolden.sh OUTDIR CLUSTER_FILE PORTS          # check
tools/clientbench/scripts/wiregolden.sh -update OUTDIR CLUSTER_FILE PORTS  # rewrite
```

Run against an idle cluster: every request to `PORTS` is in the capture. A
scenario is one transaction run by one thread, so both clients send the same
requests in the same order each run; a libfdb_c rerun reproduces the golden
exactly.

## Transcript format

One section per scenario (`== name`), one line per client request, in the order
the requests became readable at the capture point: the message type and its
non-zero fields. Normalized: reply tokens, spans, debug and idempotency IDs are
left out; versions are `V1`, `V2`, … in order of appearance within the scenario
(0, -1 and the maximum keep their values); the scenario key prefix is `<p>`; a
self-conflicting range key's 16 random bytes are `<random>`. A request type the
transcript does not decode appears as `FileID(n)`. Setup traffic before the
first scenario (connection, database info, location lookups, libfdb_c's
`GlobalConfigRefreshRequest`) is not compared.

## Known divergences (`go-divergences.diff`)

| Scenario | libfdb_c | Go |
|---|---|---|
| `getkey_offset` | the storage range read has limit 2 | limit 256 |
| `clear_range_then_set` | the clear is split around the set: `[m, m1)`, `[m1\x00, n)` | one clear `[m, n)` |
| `clear_key` | a single-key clear is sent as `(k\x00, '')` | `(k, k\x00)` |

Each is a TODO.md item; fixing one changes `go-divergences.diff`, which is then
regenerated with `-update`.

## Adding a scenario

Append to `scenarios` in `cmd/wgscen/main.go` (one transaction, no
concurrency, keys under the prefix), then run with `-update` and review both
the new golden section and the Go diff.
