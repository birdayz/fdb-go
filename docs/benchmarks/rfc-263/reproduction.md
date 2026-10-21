# RFC-263 reproduction and source identities

Raw logs, binaries, samples and closed indexes stay outside Git. Set `FDB_PERF_HOME`
to the operator's retained evidence root, `FDB_GO_WORKTREES` to the checkout parent,
`FDB_CPP_SOURCE` to the FoundationDB 7.3.77 checkout and `GO_SDK_ROOT` to the actual
Bazel Go 1.26.9 SDK. These are operator-supplied paths, not machine-specific defaults.
The scripts accept explicit flags or their documented `RFC263_*` environment variables.
A lock being available is not timing authorization: obtain a separate owner-approved
slot and absolute deadline before any build, test or measurement.

## Historical as-run versus portable source

The original source bytes and all closed raw indexes are preserved unchanged. The
portable scripts below remove personal path defaults and add explicit configuration;
they are **not** retroactively described as the source that produced old samples.
The collection/analyzer metrics and historical executable identities remain those in
the individual reports. New portable-source hashes identify only the committed replay
entrypoints. In particular, `rev5-primary-executed.py` is now a portable adaptation;
the byte-identical historical executed primary remains in the retained source archive.

| Script in `tools/benchcampaign/campaigns/rfc-263/` | Historical as-run SHA256 | Portable committed SHA256 |
|---|---|---|
| `rev5-primary.py` (Quiet-A helper) | `33159fc648a59a85805f55b7779433c328de6ed155e6c224a43c8cf96b656369` | `14146a3faf65c5844926a563b9f7c03b98bee7f39f4aaa0dbf49804f0a246c25` |
| `rev5-primary-executed.py` (original Phase 1 source) | `413034851ab5ca10e71a213291f7c0fba062842dd9103681aaae65231dfe6240` | `db60232a9f5cbe066f76d6a27afdaba0c04a41d83083f33de5af83c5dc0a688e` |
| `rev6-production-tail.py` | `9b9b728f525926dfd4240ce8b4cd4fa95845647e7c9324d8ec2df1763670c655` | `1bf7581971e612b27cdb3f91ad83b8057a3dc6a50a45990c73f87f6df33398e4` |
| `rev6-confirm.py` | `7a66c34cafa0bc5e0245fd2ad2ec65860cb0da0599af11ad0ef1643edd61bf32` | `d9a48707f2a90161a798d01fd8f4ecef8d8472a5ff3b2ac2a2edcb0b30c0a1a6` |
| `rev6-production-proofs.py` | `fbec3df8e5ef5e9807990f5682b913d74d872247c94265456fa9fecd20398e26` | `fed5a0e71a5c778e4901d6351ecac58e04ae93152bc20eadc4f247099b85eee0` |

The portable hash dependency chain is checked from actual bytes: the tail runner pins
`rev5-primary.py`, and the confirmation collector pins the tail helper. Historical
seals still pin their historical copies. The portable proof runner adds explicit
future-deadline guards and the two additional ordering mutants; its guard population
is five, versus four in the original completed production-proof epoch.

The publication preservation bundle contains 21 exact historical source copies. Its
`preservation-manifest.json` SHA256 is
`1425dcbec1a5497b194fef68223616fb23eb04d0cf0a5603cfda8fbd5ec0db54`,
and `preservation-files.sha256` is
`d7e02bc6564659ddb5c3075444a66dee9ba84baeb75ad0002d6eb9990916245f`.
Those hashes identify private retained artifacts without publishing personal paths.

## Analysis and decoder source

These source files are unchanged from the cited analyses:

| Script | SHA256 |
|---|---|
| `rev6-confirm-analyze.py` | `d3a64c062a02bb062ee3dc7e49863d070c43cb7c01febe53a535533304765543` |
| `rev6-confirm-crosscheck.py` | `5d4c3ffcbf8b40a5dcd9902439fda7a703562357ccd0c536b7727400155a0790` |
| `rev6-trace-extract.py` | `ee6068731326569f2bb4900c2c1d0ccb90221d440bd39c8817d051f57b722146` |
| `rev6-trace-analyze.py` | `51f3ba6a4ed2c80a5ede01481f462406850a6e3d990be3fe05cc415a5d3dce03` |
| `rev5-tail-analyze.py` | `ac8e0c72ef179d5c25ec1d11412ae21999d77278eebe2464ab2bb2130251346c` |
| `rev5-trace-extract.py` | `3d904dc74c00515ead10ea6d7408bd92831d637bdf1ed15fcc6d873f484ebf59` |
| `rev5-trace-analyze.py` | `0b8c5c160267e4bc54d909611258dcbdbec721e0be36e5d17156bbead01624f3` |

Run analysis only after collection has closed. Always choose a **new** output/evidence
copy; never overwrite a sealed directory. The confirmation analyzer and independent
crosscheck both read raw per-process CSVs, not each other's summaries:

```sh
P=tools/benchcampaign/campaigns/rfc-263
CONFIRM="${FDB_PERF_HOME}/validation/rfc263-production/confirmation/batch-20261020T1722Z"
# Set NEW_ANALYSIS and NEW_CROSSCHECK to distinct nonexistent external directories.
flock --close "${FDB_PERF_HOME}/validation/fast.lock" \
  taskset -c 0,1,6,7,11,12,13,18,19,23 nice -n 19 \
  env PYTHONDONTWRITEBYTECODE=1 uv run --no-project \
  --with numpy==2.3.4 --with scipy==1.16.3 python \
  "$P/rev6-confirm-analyze.py" --evidence "$CONFIRM" --output "$NEW_ANALYSIS"
flock --close "${FDB_PERF_HOME}/validation/fast.lock" \
  taskset -c 0,1,6,7,11,12,13,18,19,23 nice -n 19 \
  env PYTHONDONTWRITEBYTECODE=1 uv run --no-project \
  --with numpy==2.3.4 --with scipy==1.16.3 python \
  "$P/rev6-confirm-crosscheck.py" --evidence "$CONFIRM" --output-dir "$NEW_CROSSCHECK"
```

For full-state production trace replay, first copy the retained quiet-slot bundle to
`NEW_TRACE_EVIDENCE`. Before any quiet slot, build the decoder in the new copy, then
explicitly decode each of the three retained run directories with `--run`; no implicit
build is permitted during decoding. The analyzer expects those three records under
`decoder/extraction-inputs.json`:

```sh
uv run --no-project python "$P/rev6-trace-extract.py" \
  --sdk "$GO_SDK_ROOT" --evidence "$NEW_TRACE_EVIDENCE" \
  --output "$NEW_TRACE_EVIDENCE/decoder" --build-only
uv run --no-project python "$P/rev6-trace-extract.py" \
  --sdk "$GO_SDK_ROOT" --evidence "$NEW_TRACE_EVIDENCE" \
  --output "$NEW_TRACE_EVIDENCE/decoder" --expect-workers 8 \
  --run "$BASELINE_TRACE_RUN" --run "$R4_TRACE_RUN" --run "$PRODUCTION_TRACE_RUN"
uv run --no-project python "$P/rev6-trace-analyze.py" --evidence "$NEW_TRACE_EVIDENCE"
```

Wrap each command in the same owner lock, nice-19 and reserved-mask invocation shown
above. These are diagnostic replays, not new qualification timings. For historical
32-worker trace analysis use `rev5-trace-extract.py "$NEW_REV5_EVIDENCE" --sdk
"$GO_SDK_ROOT" --extract-only`, then `rev5-trace-analyze.py "$NEW_REV5_EVIDENCE"`;
`rev5-tail-analyze.py "$NEW_REV5_EVIDENCE"` is the separate unprofiled-tail analyzer.

## Portable campaign entrypoints

Whole-lane proof replay (no target list or test filter), with an explicit approved
validation deadline:

```sh
flock --close "${FDB_PERF_HOME}/validation/fast.lock" \
  taskset -c 0,1,6,7,11,12,13,18,19,23 nice -n 19 \
  env PYTHONDONTWRITEBYTECODE=1 uv run --no-project python \
  "$P/rev6-production-proofs.py" --evidence "$NEW_PROOF_EVIDENCE" \
  --deadline-utc "$VALIDATION_DEADLINE_UTC" --mode mutations \
  --optional-mutations eager frontier again cleanup stub-repair logical-order generation-barrier generation-turnover generation-turnover-no-defer
```

The older production screen's portable runner is **not** the new CPU plan. Its complete
operational inputs must be explicit:

```sh
uv run --no-project python "$P/rev6-production-tail.py" \
  --evidence "$NEW_TAIL_EVIDENCE" --production-sha "$PRODUCTION_SOURCE" \
  --production-binary "$PRODUCTION_BINARY" --phase1-evidence "$PHASE1_EVIDENCE" \
  --prior-tail-evidence "$PRIOR_TAIL_EVIDENCE" --cluster-file "$RFC263_CLUSTER_FILE" \
  --cluster-reference "$RFC263_CLUSTER_REFERENCE" --lock-file "$RFC263_FAST_LOCK" \
  --minutes 30 --deadline-utc "$TIMING_DEADLINE_UTC" --include-get-set
```

Use the same resource wrapper, but do not add an outer acquisition of the lock that
this runner acquires itself. Confirmation collection similarly requires explicit
`--quiet-a`, `--cluster-file`, `--cluster-reference` and `--lock-file` inputs, plus the
recorded provenance ACK/diff receipts at `prepare`. It freezes both epochs before
`run --epoch 1` or `run --epoch 2`; each run additionally requires its own explicit
`--authorization` and `--deadline-utc`. The completed p999 schedule and its original
identities are historical, not a template for silently substituting the new CPU arm.

The [CPU results](rev6-cpu-results.md) retain 256 A1 timing processes and 176 separate
A2 grv-w8 timing processes. A2 supersedes only the two A1 grv-w8 estimates, without
pooling; the other fourteen bounds are unchanged. Sixteen later diagnostic processes
are separate from timing. All sixteen final CPU upper 95% bounds are ≤1.02, and all
eight production queue-wait cells are <1% (maximum 0.008781790%). Final sampling HEAD
is `989b186a2`, tree `e3891d15e100a731a4cec7b48fc417067bceda0a`; later documentation
and rebased publication trees are not measured source. Replay strict source guards
from the archived as-run checkout and independently pinned manifests/seals. The
[original CPU plan](rev6-cpu-plan.md) and [A2 amendment](rev6-cpu-a2-grv-confirmation.md)
define the separate populations. Full/full-race, all five final-HEAD reviews, CI and
owner approval remain pending.
