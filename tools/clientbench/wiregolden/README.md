# Wire goldens

`libfdbc.golden` is the normalized request transcript of the
[wire-golden scenarios](../internal/wgscenarios/scenarios.go) on libfdb_c 7.3.77.
`go.golden` is the Go client's transcript, and `go-divergences.diff` is
`diff -u libfdbc.golden go.golden`: how Go differs from libfdb_c today.

- **Fast lane** ([`golden_test.go`](golden_test.go), `just test`): runs the
  scenarios on the Go client against a test container, records its requests
  in process from the transport's send path (`transport.CaptureWireLog`), and
  fails unless they equal `go.golden` and `go-divergences.diff` is the diff of
  the two goldens. A client change that alters the Go requests fails here. It
  is a bounded integration test, not a hermetic one: like the other
  testcontainers tests it needs Docker, and it uses the host's `diff`. The
  goldens come from the lab cluster; the location lookups fall in setup on
  both clusters, so a different shard layout would show as an extra request
  in a scenario, failing the test rather than passing it silently.
- **Lab** ([`scripts/wiregolden.sh`](../scripts/wiregolden.sh)): captures both
  clients with tcpdump, renders both transcripts
  ([`cmd/wiregolden`](../cmd/wiregolden), normalization in
  [`internal/wiregolden`](../internal/wiregolden/wiregolden.go)), and fails if
  libfdb_c no longer matches its golden or Go no longer matches `go.golden`.
  It is the only way to regenerate the goldens.

```
tools/clientbench/scripts/wiregolden.sh OUTDIR CLUSTER_FILE PORTS          # check
tools/clientbench/scripts/wiregolden.sh -update OUTDIR CLUSTER_FILE PORTS  # rewrite
```

`-update` runs libfdb_c twice and rewrites the goldens only if both libfdb_c
transcripts are identical. After a Go change that is meant to alter its
requests, rerun `-update` and commit the three files with the change.

This is a lab regression check for the covered scenarios, not a proof that
the clients send identical requests in general. Run it against an idle
cluster: every request to `PORTS` is captured. Each scenario is one
transaction on one application thread. In the runs so far, reruns of either
client reproduced its transcript byte for byte; the check relies on that
observed stability, and a scenario that loses it fails the libfdb_c
comparison.

## Scenario boundaries

Before each scenario, `wgscen` reads the marker key `\x02wg/marker/<name>` in
its own transaction at the last committed version (`SetReadVersion`), so the
marker sends one `GetValueRequest` and no GRV. A section holds the requests
between its marker and the next one. A final `~end` marker closes the run. The
transcript fails unless the markers are exactly the scenarios `wgscen -list`
prints, followed by `~end`, and every scenario sent at least one request.
Setup traffic before the first marker (connection, database info, location
lookups, the seed transaction, libfdb_c's `GlobalConfigRefreshRequest`) is not
compared.

## Transcript format

One section per scenario (`== name`), one line per client request in the order
the requests became readable at the capture point: the message type and its
non-zero fields.

- Left out: reply tokens, spans and debug IDs.
- Versions (`Version`, `ReadSnapshot`, `MinKnownCommittedVersion`,
  `MaxVersion`) are `V1`, `V2`, … in order of appearance within the scenario;
  0, `invalidVersion` (-1), `latestVersion` (-2) and the maximum keep their
  values. This shows which requests share a version, not which GRV reply a
  version came from.
- Random identities are `<Rn:len>`, numbered in order of appearance within the
  scenario, so equal bytes keep one name: the 16 random bytes of a
  self-conflicting key (`\xff/SC/<R1:16>`) and `IdempotencyId` (presence and
  length are kept; an absent ID is left out, an empty one is `''`).
- In key fields (`Key`, `Begin`, `End`, `Param1`, and `Param2` of a ClearRange),
  the scenario prefix is `<p>`. Values are literal.
- A request type the decoder does not know fails the transcript inside a
  scenario; add its type to `internal/wiregolden` to compare it.

`SsLatestCommitVersions` is compared literally and `MaxVersion` through the
version naming. Both were constant (empty version vector, -1) on the local
three-process cluster used here; they can vary with cluster configuration and
state (`NativeAPI.actor.cpp:3638-3641, 7248`), so a cluster where they do needs
its own normalization first.

## Known divergences (`go-divergences.diff`)

| Scenario | libfdb_c | Go |
|---|---|---|
| `getkey_offset` | the storage range read has limit 2 | limit 256 |

It is a TODO.md item. Fixed: `clear_range_then_set` (a clear split around the
operations inside it) and `clear_key` (the compact single-key clear encoding).
Fixing a divergence changes the goldens, which are then regenerated with
`-update`.

## Adding a scenario

Append to `scenarios` in `internal/wgscenarios/scenarios.go` (one transaction, no
concurrency, keys under the prefix, no time or randomness in keys or values),
then run with `-update` and review both the new golden section and the Go
diff.
