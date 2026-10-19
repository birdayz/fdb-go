---
name: wire-goldens
description: Compare the exact requests the pure-Go FDB client and libfdb_c send for the same deterministic scenario, using libfdb_c transcripts as checked-in goldens. Use to check a client change for wire divergence (extra or missing GRVs, resent commits, different conflict ranges, mutations, limits, flags), to reproduce a suspected divergence in minutes, to pin a fix, or for wire experiments ("what does libfdb_c send for X?"). Complements hunt-divergences, which compares data-plane results.
---

# Wire goldens

The data-plane differential (`hunt-divergences`) proves both clients *store and read*
the same bytes. Wire goldens prove they *send the same requests*: the same request types
in the same order, with the same fields. A scenario is one transaction run by one thread
on an idle cluster, so it is deterministic: a libfdb_c rerun reproduces its golden byte
for byte, and any difference in the Go transcript is real behavior, not timing. This is
the DST idea applied to the client's wire protocol.

Use it whenever a change could alter what the client sends: commit path, GRV path, RYW
write map, read path, options, retries. A review argument like "this sends one GRV" is
then a diff, not a reading of the code.

## Pieces

| Path | What |
|---|---|
| `tools/clientbench/cmd/wgscen/main.go` | the scenarios; one binary per client (`wgscen_go`, `wgscen_cgo` with the `libfdbc` tag over `pkg/fdbgo/libfdbc`) |
| `tools/clientbench/internal/wiregolden` | pcap → decoded requests (all request types via `pkg/fdbgo/wire/types`) → normalized transcript |
| `tools/clientbench/cmd/wiregolden` | the transcript CLI |
| `tools/clientbench/scripts/wiregolden.sh` | build, capture both clients (`wirecapture.sh`, tcpdump in docker), render, compare |
| `tools/clientbench/wiregolden/libfdbc.golden` | libfdb_c's transcript, the spec |
| `tools/clientbench/wiregolden/go-divergences.diff` | the Go client's known differences, each a TODO.md item |

Format and normalization rules: `tools/clientbench/wiregolden/README.md`.

## Run it

Needs docker, an idle cluster (only the scenario's traffic may reach its ports), and
libfdb_c 7.3.77 (`LIBFDB_C_DIR`, default `~/.local/share/foundationdb/7.3.77/lib`).
Locally, a tmpfs cluster on ports 4731–4733 gives stable runs:

```bash
tools/clientbench/scripts/wiregolden.sh OUTDIR CLUSTER_FILE 4731,4732,4733
```

- **Pass:** libfdb_c matches the golden and Go differs only by `go-divergences.diff`.
- **libfdb_c differs from the golden:** a scenario is not deterministic (concurrency,
  time, a key outside the prefix, other traffic on the ports) or libfdb_c changed. Fix
  the scenario; never update the golden to make a flaky scenario pass.
- **Go divergences changed:** read `OUTDIR/go.diff`. A new divergence is a Go bug until
  the C++ source says otherwise; a removed one means a fix landed.

Outputs per run: `OUTDIR/{cgo,go}/capture.pcap`, `OUTDIR/{cgo,go}.transcript`,
`OUTDIR/go.diff`.

## Workflows

**Check a client change.** Run before and after. A change in `go.diff` that you did
not intend is a regression.

**Investigate "what does libfdb_c send for X".** Add a scenario, run, read the libfdb_c
section of `OUTDIR/cgo.transcript`. Then find the C++ code that produces it
(`/tmp/fdbsrc` or the 7.3.77 checkout; `fdb-client-engineer` §1). The transcript tells
you where to look; the source is still the spec.

**Fix a divergence.**
1. Root-cause it in the C++ source; cite `file:line` (the TODO.md entry has the first
   citations).
2. Fix Go; add the focused regression (`hunt-divergences` step 6) the fix needs anyway.
3. Rerun: the scenario's lines leave `go.diff`. Regenerate with `-update` (it rewrites
   both files from this run), check that only the intended lines changed, and commit
   `go-divergences.diff` with the fix. The diff shrinking is the red→green proof.

**Add a scenario.** Append to `scenarios` in `wgscen/main.go`: one transaction, no
goroutines, keys under the `\x02wg/d/` prefix, deterministic values (no time, no random).
Run with `-update`, then review the new golden section line by line and the Go diff
before committing. Seed data goes in the setup transaction, which is not compared.

## Normalization: what is and is not compared

Compared: request types, their order within a scenario, every field except the ones
below (keys, values, mutations and their types, conflict ranges, selectors, limits,
flags, transaction counts, tenant, read options).

Not compared: reply tokens, span/debug/idempotency IDs (per run); version values (they
are numbered per scenario, so *which* version a request uses is still compared); the 16
random bytes of a self-conflicting key; replies (the server is the same for both); setup
traffic before the first scenario, including libfdb_c's startup
`GlobalConfigRefreshRequest`. A request type the decoder does not know appears as
`FileID(n)`, so unexpected traffic inside a scenario still shows up; add its type to
`internal/wiregolden` to see its fields.

## Limits

- Concurrency, batching, timing and retries under faults are not covered: those are
  nondeterministic by nature. Use wire captures (`wirecapture.sh` + `wiretrace`) and the
  benchmark campaigns for them, and fault injection for retries.
- It needs docker and a live cluster, so it is a lab check, not a Bazel test target.
  Run it at client-change and PR boundaries.
- Request order is capture order. Two requests the client sends together could in
  principle become readable in a different order; with one thread per scenario this has
  not happened (reruns are byte-identical).

## Hard rules

- libfdb_c is the spec: the golden is regenerated only from a libfdb_c run, and only
  with the reason in the commit message.
- Never fold a new Go difference into `go-divergences.diff` without a TODO.md entry and
  a C++ citation explaining it.
- Keep every scenario deterministic; a libfdb_c rerun must reproduce the golden exactly.
