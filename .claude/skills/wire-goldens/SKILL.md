---
name: wire-goldens
description: Compare the requests the pure-Go FDB client and libfdb_c send for the same small deterministic scenario, using libfdb_c transcripts as checked-in goldens. Use to check a client change for wire divergence in the covered scenarios (extra or missing GRVs, resent commits, different conflict ranges, mutations, limits, flags), to reproduce a suspected divergence in minutes by adding a scenario, to pin a fix, or for wire experiments ("what does libfdb_c send for X?"). Complements hunt-divergences, which compares data-plane results.
---

# Wire goldens

The data-plane differential (`hunt-divergences`) checks that both clients *store and
read* the same bytes. Wire goldens check, for a set of small scenarios, that both
clients *send the same requests*: the same decoded request types in the same order, with
the same fields after normalization. Each scenario is one transaction on one application
thread against an idle cluster. In the runs so far, reruns of either client reproduced
its transcript byte for byte, so a difference in the Go transcript has pointed to
behavior, not timing. The check relies on that observed stability; it is a lab
regression check for the covered scenarios, not a proof of identical requests in
general.

Use it when a change could alter what the client sends (commit path, GRV path, RYW write
map, read path, options): a review argument like "this sends one GRV" becomes a diff for
the covered scenarios.

## Pieces

| Path | What |
|---|---|
| `tools/clientbench/internal/wgscenarios` | the scenarios |
| `tools/clientbench/cmd/wgscen` | runs them on one client per binary (`wgscen_go`, `wgscen_cgo` with the `libfdbc` tag over `pkg/fdbgo/libfdbc`); `-list` prints their names |
| `tools/clientbench/wiregolden/golden_test.go` | the fast-lane check: the Go client's requests, recorded in process (`transport.SetWireLog`), must equal `go.golden` |
| `tools/clientbench/internal/wiregolden` | pcap → decoded requests (the request types in its `requests` map, via `pkg/fdbgo/wire/types`) → normalized transcript, with boundary checks |
| `tools/clientbench/cmd/wiregolden` | the transcript CLI (`-expect` takes the `-list` output) |
| `tools/clientbench/scripts/wiregolden.sh` | build, capture both clients (`wirecapture.sh`, tcpdump in docker), render, compare |
| `tools/clientbench/wiregolden/libfdbc.golden` | libfdb_c's transcript, the spec |
| `tools/clientbench/wiregolden/go.golden` | the Go client's transcript |
| `tools/clientbench/wiregolden/go-divergences.diff` | `diff -u libfdbc.golden go.golden`: the known differences, each a TODO.md item |

Format, normalization and boundaries: `tools/clientbench/wiregolden/README.md`.

## Fast lane

`just test` runs `//tools/clientbench/wiregolden:wiregolden_test`: the scenarios on
the Go client against a test container, compared with `go.golden`. A change that
alters the Go client's requests fails it (`WIREGOLDEN_GO`, with the diff). If the
change is intended (a divergence fix), regenerate the goldens with the lab run below
and commit them with the change. The fast lane never regenerates anything and needs
no libfdb_c.

## Lab run (both clients, regenerates the goldens)

Needs docker, an idle cluster (only the scenarios' traffic may reach its ports), and
libfdb_c 7.3.77 (`LIBFDB_C_DIR`, default `~/.local/share/foundationdb/7.3.77/lib`).
Locally, a tmpfs cluster on ports 4731–4733 gives stable runs:

```bash
tools/clientbench/scripts/wiregolden.sh OUTDIR CLUSTER_FILE 4731,4732,4733
```

- **Pass:** libfdb_c matches the golden and Go differs only by `go-divergences.diff`.
- **libfdb_c differs from the golden:** a scenario lost determinism (concurrency, time,
  a key outside the prefix, other traffic on the ports) or libfdb_c changed. Fix the
  scenario; never update the golden to make an unstable scenario pass.
- **Go divergences changed:** read `OUTDIR/go.diff`. A new divergence is a Go bug until
  the C++ source says otherwise; a removed one means a fix landed.
- **The transcript fails:** an incomplete capture (drops, gaps, cut-off or undecodable
  frames), missing or reordered markers, a scenario with no request, or a request type
  the decoder does not know inside a scenario. Fix the cause; for an unknown type, add
  it to `internal/wiregolden`.

Outputs per run: `OUTDIR/{cgo,go}/capture.pcap`, `OUTDIR/{cgo,go}.transcript`,
`OUTDIR/go.diff`, `OUTDIR/scenarios.txt`.

## Workflows

**Check a client change.** The fast lane does it. For a change meant to alter the
requests, run the lab check before and after; a change in `go.diff` you did not intend
is a regression.

**Investigate "what does libfdb_c send for X".** Add a scenario, run, and read its
section in `OUTDIR/cgo.transcript`. Then find the C++ code that produces it (the 7.3.77
checkout; `fdb-client-engineer` §1). The transcript tells you where to look; the source
is still the spec.

**Fix a divergence.**
1. Root-cause it in the C++ source; cite `file:line` (the TODO.md entry has the first
   citations).
2. Fix Go and add the focused regression the fix needs (`hunt-divergences` step 6).
3. Rerun: the scenario's lines leave `go.diff`. Regenerate with `-update` (two libfdb_c
   runs must agree), check that only the intended lines changed, and commit
   `go-divergences.diff` with the fix. The diff shrinking is the red→green evidence.

**Add a scenario.** Append to `scenarios` in `internal/wgscenarios/scenarios.go`: one transaction, no
goroutines, keys under the `\x02wg/d/` prefix, no time or randomness in keys or values.
Seed data goes in the setup transaction, which is not compared. Run with `-update`, then
review the new golden section line by line and the Go diff before committing.

## What is compared

Compared: the decoded request types, their order within a scenario, and their fields:
keys, values, mutations and their types, conflict ranges, selectors, limits, flags,
transaction counts, tenant, read options, idempotency-ID presence, length and reuse.

Normalized: versions (named per scenario, so which requests share a version is still
compared, but not which GRV reply it came from); random identities (self-conflict bytes,
idempotency IDs), named per scenario so equality is kept; the key prefix in key fields
only. Left out: reply tokens, spans, debug IDs; replies; setup traffic before the first
marker, including libfdb_c's startup `GlobalConfigRefreshRequest`.

## Limits

- Concurrency, batching, background requests, timing and retries under faults are not
  covered. Use wire captures (`wirecapture.sh` + `wiretrace`), the benchmark campaigns
  and fault injection for them.
- Request order is capture order. Requests a client sends together could become readable
  in another order; it has not happened in these scenarios, and the libfdb_c rerun in
  `-update` would show it.
- `SsLatestCommitVersions` and `MaxVersion` were constant on the local cluster; a
  configuration where they vary needs normalization first (README).
- The libfdb_c side needs tcpdump (docker) and libfdb_c, so it is a lab run; the
  fast lane checks the Go side against the committed goldens.

## Hard rules

- libfdb_c is the spec: the golden is regenerated only from libfdb_c runs, and only with
  the reason in the commit message.
- Never fold a new Go difference into `go-divergences.diff` without a TODO.md entry and
  a C++ citation explaining it.
- Keep every scenario deterministic; a libfdb_c rerun must reproduce the golden.
