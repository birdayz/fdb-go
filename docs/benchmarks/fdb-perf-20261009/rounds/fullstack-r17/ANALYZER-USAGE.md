# R17 offline analyzer

Files to pin before acquisition:

- `analyze-r17.py`
- `r17_statistics.py`
- `selfcheck-r17.py`
- Frozen `frozen-v7-scripts/{analyze.py,run.py,r3_plan.py}` (extracted unchanged from the SHA256-pinned V7 harness archive).

The analyzer requires its own SHA256 and the statistics module SHA256 to occur in the common `execution-manifest.json.input_sha256` map. All 80 segment plans must use that exact same map. The executable/library/source identities, original registration hash, and owner-approved v2 freshness-addendum hash are hard-coded; the addendum cannot permit a different implementation.

## Usage after all acquisition completes

```sh
python3 -B /path/to/work-fullstack-r17/analyze-r17.py \
  /path/to/r17-acquisition-root \
  /path/to/new-r17-analysis-output \
  --v7-scripts /path/to/work-fullstack-r17/frozen-v7-scripts
```

The output directory must not exist and must be outside the input tree. The default V7 script directory is next to the analyzer. Registration files default to:

- `INPUT_ROOT/R17-preregistration-v1.md`
- `INPUT_ROOT/R17-preregistration-addendum-v2.md`

Optional `--preregistration PATH` and `--addendum PATH` can relocate those two files but cannot change their frozen hashes.

Optional `--raw-archive PATH --raw-archive-sha256 EXPECTED_SHA256` records and verifies a pre-existing sealed archive's bytes. It does not extract that archive or claim that hashing the tar alone verifies every extracted member. `input-files.csv` independently inventories every input file. If analysis precedes sealing, the archive field is explicitly null; the acquisition owner binds the output/inventory into the final archive and performs transfer/member verification.

## Validation and outputs

No performance estimate is produced before all 80 segments/640 runs pass. The analyzer rejects missing/extra segments or run summaries, aborted acquisition, nonmatching pre-pinned schedules, script/binary/source/runtime identity drift, malformed raw samples, nonzero callback retries/errors, failed exact byte/persistence checks, incomplete lifecycle fixpoints, and nonserial process ordering. Frozen V7 `validate_raw_output`, `validate_identity`, `validate_lifecycle`, scheduler, schedule normalization, and quantile functions are imported directly. Their acquisition/analyzer `main` functions are never invoked, and raw inputs are not modified.

Pre-run quiescence/busy/steal gates are recomputed using the frozen V7 arithmetic and fixed plan thresholds. During-run telemetry is retained and reported descriptively; **there is no newly introduced during-run steal/affinity/process threshold that rejects a run**. Discrete telemetry observations cannot prove absence of every transient concurrent process.

Main outputs:

- `status.json` — complete or failed; a failure forbids confirmatory claims.
- `analysis.json`, `TABLES.md` — all five endpoint decisions plus descriptive secondary tables.
- `primary-decisions.csv`, `bootstrap.json` — both predeclared 99% intervals, fixed bootstrap seeds/lengths, draw/distribution digests.
- `primary-per-block.csv` — 160 rows with the five NEW/CGo values, ratios, and paired logs.
- `per-block.csv` — all 320 workload/block pairs, randomized arm order and secondary ratios; no absolute-E ratios.
- `per-run.csv` — all 640 processes, raw A–E integer counters, CPU intervals and components, lifecycle/GC/finalizer records, command/configuration, quiescence/telemetry metadata, and per-run file hashes.
- `absolute.csv`, `secondary-descriptive.csv` — explicit CPU/memory scopes. Absolute E/N is unpaired only. Go allocation counters exclude native C++ heap.
- `segments.csv`, `validation.json`, `provenance.json`, `input-files.csv` — schedules, validation and complete raw-file provenance.

No R16 observations enter inference. Claims are conditional on the registered protocol and **reimaged existing instance with a fresh cluster**, not a new independent VM/host allocation. Neither confidence interval failure nor a point estimate below one establishes equivalence. Benchmark success does not establish repository correctness, review/CI approval, or merge permission.

## Artifact-only selfchecks

```sh
python3 -B /path/to/work-fullstack-r17/selfcheck-r17.py
```

Uses synthetic 160-by-5 paired logs only. Checks direct expanded-index reference agreement, circular wrapping, common draws across columns, determinism, shape/nonfinite rejection, exact nearest-rank indices 499/99499, both strict-upper-less-than-one gates (including equality), and full 100,000-resample execution for each registered length/seed. No repository tests, builds, VM access, network, or real R17 observations are involved.
