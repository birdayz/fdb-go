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

`-update` runs libfdb_c twice and rewrites the golden only if both transcripts
are identical.

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
| `clear_range_then_set` | the clear is split around the set: `[m, m1)`, `[m1\x00, n)` | one clear `[m, n)` |
| `clear_key` | a single-key clear is sent as `(k\x00, '')` | `(k, k\x00)` |

Each is a TODO.md item; fixing one changes `go-divergences.diff`, which is then
regenerated with `-update`.

## Adding a scenario

Append to `scenarios` in `cmd/wgscen/main.go` (one transaction, no
concurrency, keys under the prefix, no time or randomness in keys or values),
then run with `-update` and review both the new golden section and the Go
diff.
