# conflictbuf-512-001 / v5-batchset50 — build-only result

Authorization: root request #348. **Build and standalone-harness unit validation only. No benchmark/measurement/profile executable was run.** Parent-module tests remain root's responsibility through just/Bazel; this agent ran none.

## Immutable source capture

Base revision `26abdbaac12be60ea8955027f6047e10474596f1`. Root source `${FDB_GO_WORKTREES}/fdb-fix-ryw-local` was read, not edited. Its root freeze manifest `/tmp/astra-conflictbuf-timeoutfix-frozen.sha256` contained 6814 file hashes. All **740** selected `pkg/fdbgo/**`, `go.mod`, `go.sum` files matched before and after capture; the actual selected file population matched the manifest exactly. The complete patch includes 10 untracked files. Extracting the source archive independently also verified all 740 hashes.

| Artifact | SHA-256 |
|---|---|
| Complete client patch against base, including untracked files | `b3c09057e81ac448c8b05881e10cd52a34067a27152fcd0a4c367e514e0861c3` |
| Frozen selected client/module source archive | `ee1fa82ab2fd167a257b95f440f4200662f4c9ff949f93da35343efaf3297801` |
| Root-supplied allocator-only delta over prior runtime | `862ebf40ae1f6c01e6abfeb4221cb46dc82b130d765f880da869b52e11bd6f2a` |
| Original v3 harness archive, unchanged | `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea` |
| Final immutable v5-batchset50 harness archive / harness identity | `738348814d86cc6ec828aa3590625275e9f1b74bed7ae92e06e359c480151cfc` |
| Final v5-batchset50 patch against v3 | `cd3f7744d006ec3714a4c8e7f85f411cf18917633fdce7dc4ab0812e16d2790d` |

Remote v3 candidate source: `/opt/fdbperf/sources/conflictbuf-512-001`. Created from `git archive` of the exact base, then the complete client patch applied. All 740 expected source files and the exact file population verified. Its original v3 harness was extracted unchanged and all 12 harness file hashes checked.

Remote v5 sources: `/opt/fdbperf/sources/grv-idle-v5batch-001` and `/opt/fdbperf/sources/conflictbuf-512-v5batch-001`. These preserve the previous frozen old client and new client respectively, with the same v5 patch. All 739 old-client/module hashes and 740 new-client/module hashes verified; all 13 v5 harness files matched the same manifest before and after building. The native v5 build uses the same v5 source tree as new Go, with CGO_ENABLED=1 selecting the unchanged official binding adapter. Neither old source directory nor any old binary was overwritten.

The finalized v5 source differs from the accepted draft only in README labeling: removed draft/unvalidated wording, directed validation status to this external build record, and retained the not-yet-authorized campaign wording. No functional change from the inspected draft. Both Go/CGo adapters are byte-identical to frozen v3.

## Binaries

All reside under `/opt/fdbperf/bin/` and are newly named immutable files:

| Binary | Harness | SHA-256 |
|---|---|---|
| `candidate-conflictbuf-512-001-go` | Original v3 | `47f7ea5872c0efcbf0435f3071e60c832915102df8c36c9e9208bb0916011cfb` |
| `batchset50-v5-grv-idle-001-go` | v5, old client | `c0b08946eb3f6e8ec448d6aecfc24722a9a523170f46d51fe9d80e9c6cef7fc2` |
| `batchset50-v5-conflictbuf-512-001-go` | v5, new client | `5214910d5c4084d5c95d8e919f1867b279b164968d54afbd6a65c52a6fb853c6` |
| `batchset50-v5-cgo` | v5, official native client | `8433071f23b26d8163ffed3555a2c32ce2fece22133dd281c679f929a144eee6` |

Go toolchain 1.26.9, build `go build -trimpath`, GOMAXPROCS=8, GOFLAGS=-mod=readonly. CGO_ENABLED=0 for pure Go and 1 for native. Exact go env, go version -m, readelf headers/program headers/dynamic sections and native ldd retained. Pure-Go binaries have no dynamic section. Native links `/lib/libfdb_c.so` resolving to `/usr/lib/libfdb_c.so`, SHA-256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`, unchanged. All eight pre-existing binaries checked unchanged after builds, as did server/library/v3 harness pins.

## Standalone harness validation (actual execution)

Every test command ran **only inside its `tools/clientbench` nested module**, using the explicitly approved exception: `CGO_ENABLED=<0|1> GOTOOLCHAIN=go1.26.9 go test -count=1 -v ./...`. No parent go test/go vet/Bazel target was invoked.

| Source/backend | Top-level tests | Result | Package duration |
|---|---:|---|---:|
| New client + original v3 / Go | 7 | all actually PASS | 0.003 s |
| Old client + v5 / Go | 13 | all actually PASS | 0.013 s |
| New client + v5 / Go | 13 | all actually PASS | 0.011 s |
| v5 / CGo | 13 | all actually PASS | 0.009 s |

The v5 tests include six new top-level batch controls: one write-transaction callback and exactly 50 unique keys/values; phase/worker/operation/slot identities; opt-in-only preparation; all final slots and round-robin boundaries; rejection of same-length corruption in each of the 50 slots; unprepared-shape rejection; config acceptance (the first shape/preparation controls cover several assertions). There are five boundary subtests plus the pre-existing subtests. These record harness shape only, not FDB semantics. Actual real-backend persistence validation still must occur during the subsequently authorized campaign.

## Preserved metadata collection failure

The initial build script completed all four unit commands and all four binary builds, then exited **127** at optional metadata command `file`: `file: command not found`. Its failed status, entire output and script are preserved unmodified. No source/test/compiler failure occurred. A separately named metadata-only completion used existing readelf/ldd, collected all four binary hashes, and repeated source/harness/old-binary checks; it completed with exit 0. No package installation, rebuild or extra test was needed.

## Batch promotion proof and release gate

See `PROMOTION-PROOF.md` for exact file:line chain. Ordinary Set records a write conflict using **2*key_length+1** bytes. Each batch key has the common prefix plus a 20-byte suffix. The existing accepted prefix bounds imply **3250..27650 conflict bytes per 50-Set transaction**, exceeding 512 but remaining below 32768. For arm prefix length <=235 bytes, the first conflict uses the small pool and a later Set necessarily promotes to the 32 KiB pool; at a longer prefix the first allocation is already large. The eventual schedule must assert prefix<=235 and record exact per-arm key lengths so this is specifically a promotion control. This is source proof, not an observed runtime promotion counter or performance result.

At build authorization root reported fast lane **100/100 green, 2 actual + 98 cached, 15.1 s**. A prior run actually passed all eight allocator unit tests but hit a tenant fixture timing failure subsequently fixed by explicit deadline expiration. Full 151-target task131 was **running**, not green, at this authorization. Parent correctness/review claims are root-reported and separate from the harness validation above.

**Await root review and explicit run release.** The proposed Read1000 and batchset50 timing campaigns remain unexecuted and unauthorized by this build-only request. No VM lifecycle action was taken.
