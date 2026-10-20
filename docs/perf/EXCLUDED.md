# Not imported

What the controller's performance working directories held that is not in this
repository, and why. Paths use the placeholders in [README.md](README.md).

## Raw data and archives

Raw samples, logs, telemetry, pprof profiles and sealed archives stay outside Git.
Write-ups cite them by sha256:

- `fdb-perf-20261009` round archives (39, 1.7 GB):
  [RAW-ARCHIVES.sha256](../benchmarks/fdb-perf-20261009/RAW-ARCHIVES.sha256). The
  `*-extracted/` and `work-*/` copies, `*verification.log`, `pre-timing-backup/`,
  `source-handoffs/` and `baseline-complete-v3/artifacts/binaries/` are the same
  data unpacked or staged.
- RFC-263 source archives and arm binaries:
  [RAW-ARCHIVES.sha256](../benchmarks/rfc-263/RAW-ARCHIVES.sha256). The gctrace
  dumps (`gctrace-analysis.json`, `memory4-gctrace-analysis.json`, 0.7 MB) and
  pprof outputs stay with them.
- `hcloud-rebench-20261014`: `results/`, `results-reconstructed/`, `analysis/`,
  `provenance/`, sealed `plans/` (derived from the imported protocols by the
  sealers), `dashboard/index.html` (rendered by `render_dashboard.py`) and the
  source/overlay archives:

  ```
  78dd35395f3f7ea16da9b0e2f68fd9c02658fb43a95b91380ad80c3f9a66d235  sources/before.tar.gz
  df5794e6ea7b541d5367e11bc4579c755d482d87dca59bd048acb6121e40730a  sources/final.tar.gz
  bba97d9491ffe0e6a02e2ca3ec8669a725d2aa38b4f2a75052e7af92541dd1d0  sources/legacy.tar.gz
  61bf347979c0779e0a3220c3720ac342af53b48bc1f4bea94e55ff8eb8ec50ca  sources/pr2.tar.gz
  c53cb807433cb3b917a18f2ecac5e3c5cc083f779a70f364a20ea9a8d3c70fc5  overlays/client-scripts-e944411c52a6fef0347f0fbf791f1484cbcb1c75.tar.gz
  7083b85e642ba584eb45c94d2026c99e1eb4e67635473906f67a67278150cadf  overlays/common-e944411c52a6fef0347f0fbf791f1484cbcb1c75.tar.gz
  5cf6ddcbcb6fccec4ffc18a7a2405dbaa61d6789a6ecf585eaba322bf457d9e9  overlays/final-only-e944411c52a6fef0347f0fbf791f1484cbcb1c75.tar.gz
  d9b89cdc8f3532c16f909b98a6fe36814b7092c9f95be64187ad31a72b458b82  overlays/remote-e944411c52a6fef0347f0fbf791f1484cbcb1c75.tar.gz
  ```
- `hcloud-multinode-vpc-20261014`: `results/`, `analysis*/`, `reconcile/`,
  `telemetry/`, `provenance*/`, `diagnostic/` outputs, sealed `plans/`, and the
  reused binaries (`binaries/rebench-20261014/bin.tar`,
  `7e3855c9f1ad5e9eeeb0b04650f2985ad4463b1560515248384f094e8e03cf4c`).
- `fdbmn-20261020` (running): `results-raw/`, `telemetry/`, `reconcile/`,
  `collected/`, `diagnostic-*/`, `phase-controller/`, `provenance/`, `sealed/`,
  `deploy/`, `frozen/`, `build-check/{logs,trees,bin,cache,…}` and all `*.log`.
- `cpu-work/` (RFC-259 screens): raw runs and binaries; the committed results
  are in [docs/benchmarks/rfc-259](../benchmarks/rfc-259/README.md).
- `/tmp/govsc-ae14`: Go vs CGo short screen, raw runs only, no write-up
  (binaries `go.bin` `da7c3afc7db4171e702a06ea7038bafc7abc27a4fd344133a428981e76a7fcf6`,
  `cgo.bin` `24729ecd7b1897589de6b658c5763e8649911e73dc2ccdd5cf72d7ddd5707a65`).
- `validation/`, `eager/`, `eager_*.log`, `prefixlen_*.log`: validation-lane logs.

## Secrets, infrastructure state and identities

- Every `private/` directory (SSH keys, known_hosts, cloud-init with host keys).
- `state.json`, `campaign.json`, `campaign-owned-primary-ips.json`,
  `work/provision*/`, `work/teardown/`, `inventory/`: cloud resource IDs, public
  addresses and the project's other resources.
- `fdbmn-20261020/commands/{provision-dry-run*,teardown-*}.sh` and
  `execution/provision-*.sh`, and `provision-*dry-run.txt`: they carry the names and
  IDs of every unrelated resource the campaign protects; without them the commands
  lose their meaning. The generic tools are in-tree and read the protected list
  from a private file (`--protect-file`): `tools/benchcampaign/multinode/`
  `inventory.py` (snapshot and verify), `provision.py`, `teardown_guard.py` and
  `primary_ip_check.py`.
- `fdb-perf-20261009` `deletion/` and `work-fullstack-r17/rebuild/`: cloud API
  calls with server and action IDs.
- `infra-pool2/`: an older CI pool's OpenTofu state, lock and cloud-init.

## Third-party binaries

Not vendored; each is pinned where the repository uses it.

| Binary | Version, sha256 | Pinned in |
|---|---|---|
| Hugo | 0.162.0 extended; local `hugo` `4c0764e431438114a8d36d02e68af12a9d8cff0b00053ef87177bddfd51be0e7`, `hugo.tgz` `b118d52077bcb301c8d8031f116c04d9dd848e4a5cab572ca10905ea94791216` | `.github/workflows/website.yml` (`HUGO_VERSION`) |
| gofumpt | `049805709714cb4f9f7f766fb06305db21ae10ab0aad086457d0faad05bf42b4` | Bazel (`just fmt`), same version as nogo |
| OpenTofu | 1.10.6, `tofu_1.10.6_linux_amd64.zip` `15b7bed76420b50da3e121769c43341df8cd57d751ca14e6dbe9c850124c6dac` (from the release's SHA256SUMS) | not pinned in-tree |

`fdbsrc/` is the FoundationDB 7.3.77 source (fetch the upstream tag).

## Superseded or out of scope

- `hcloud-rebench-20261014/*.py|*.sh` and `hcloud-multinode-vpc-20261014/tools/`:
  older copies of `tools/benchcampaign/{singlenode,multinode}`; the in-tree
  versions add flags for keys, hosts and roots. `patches/test_multinode.py.v3-provenance.patch`
  is already applied in-tree.
- `fdb-perf-20261009` duplicate copies of `perf-lead-patches/*.patch` under
  `work-*/`, `frozen-v7-scripts/` (copies of `tools/clientbench/scripts`), and
  the source trees inside the archives.
- `/tmp/gcalign.py`: superseded by `tools/clientbench/scripts/gcalign.py`.
- `/tmp/rfc263-*/rfc263-revision4.md`, `rev4-section-draft.md`: the RFC text,
  which lives on the RFC-263 branch. Prototype `.go` files and other prototype
  patches: in the source archives above.
- `cpu-work/*.patch` and `*-body.md`: prototype patches and PR bodies; the PRs
  and the committed `rfc-259/prototype.patch` carry them. Its wrapper scripts are in
  `tools/benchcampaign/campaigns/rfc-259/`.
- `/tmp/rfc263-rev5.*`: revision 5 is added to `docs/benchmarks/rfc-263` from the
  RFC-263 branch.
- `backup/`, `backups/`, `worktrees/`, `workspace-checkpoints/`: superseded
  work-in-progress patches and checkouts.
- `model-migration/`, `hn-*-state.md`, `notes/hn-sql-ws3-queue.md`,
  `WORKSTREAM_RULES.md`, `decisions.log`: agent handoff and process notes.
- `site-preview/`, `hugo-work/`: generated website; `rewrite_summaries.py` was a
  one-off edit already applied to `scripts/sync-website-benchmarks.py`.
- `/tmp/measure_tui.py`, `/tmp/pick.py`: tools for another project.
  `/tmp/install-earlyoom.sh`: workstation setup.
- `git-fixture-local.bazelrc`, `gofumpt-path`, `fast.lock`: local machine state.
