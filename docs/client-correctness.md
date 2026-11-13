# Scheduled Go client correctness

`.github/workflows/client-correctness.yml` runs the Go client's upstream
FoundationDB BindingTester hourly (minute 17), or on `workflow_dispatch`.
This is **Go client coverage only**: it does not run Record Layer, SQL, Java
conformance, or the general repository test lanes.

## Contract and bounds

The single job runs on GitHub-hosted `ubuntu-24.04`, not the shared local heavy
runner. There is no matrix or fanout. Workflow concurrency disables cancellation
of an active run; GitHub may replace an older pending run with a newer pending
run. Cron delivery is best-effort, not an hourly execution guarantee.

The command is:

```sh
bazelisk run --config=ci //cmd/fdb-binding-stress -- \
  -scheduled -duration 20m -seed-start "$seed" -out "$evidence/results"
```

`seed` is freshly drawn from the positive 31-bit range (`1..2147483647`) on each
run. It is saved as `seed-start.txt`; random draws are not a uniqueness guarantee.
`evidence` is an absolute directory outside the checkout.

Scheduled mode first replays 14 committed regression cases borrowed from
`foundationdb-rs/scripts/run_bindingtester.sh`; these are not bugs discovered by
the Go client. These seeds run against our pinned API 730 surface rather than
Rust's API 740; the original API 610 setting is retained for the HCA regression.
The source snapshot is `foundationdb-rs` commit
`248b4de71b81327955f545d09b5cc91f72d4f898`. It then cycles through:

1. `scripted`;
2. `api`, compared with the Python binding;
3. `api`, concurrency 5;
4. `directory`, compared with the Python binding;
5. `directory_hca`, concurrency 5, 100 operations.

Exploration uses API version 730 and 1000 operations for the other modes by
default. The registered `pure_go` tester advertises upstream `ALL_TYPES`,
including versionstamps, and a maximum integer width of 2040 bits, matching the
pinned upstream tester's full type/integer generation surface.
Every invocation disables tenants; directory modes also disable directory
snapshot operations. Neither surface is covered by this lane.

The 20-minute wall budget is not permission to return a vacuous pass. The runner
must finish regression replay and at least one complete five-mode cycle; budget
exhaustion before that is a failure. It stops starting cases with less than a
full five-minute per-case timeout plus 30 seconds of cleanup headroom remaining.
Further cycles extend the seed population within the budget. The runner records mode, seed, options, results, and run
metadata; inspect that population rather than interpreting elapsed time as
coverage. A failure, missing verdict, dead FDB, or incomplete required population
must not be treated as a pass.

The workflow allows 40 minutes for the whole job, at most 7 minutes for dependency
setup and 30 minutes for the run step. An outer 28-minute timeout (plus a
30-second kill grace) bounds build/setup hangs and leaves room for input
verification and artifact upload. A timeout is a failed run, not a successful
20-minute sample. Hosted cold-cache build time and Docker startup can consume
these limits; a red setup/build step establishes no client correctness result.

## Dependencies

Docker must be running and usable by the invoking user. The workflow checks
`docker info` and pre-pulls `foundationdb/foundationdb:7.3.77`. It installs the
checksummed upstream **FoundationDB 7.3.77** amd64 clients `.deb`, which supplies
`libfdb_c.so`, and runs `ldconfig`. The Python reference binding needs that
library even though the client under test is pure Go.

Bazelisk 1.30.0 and just 1.48.1 are installed from checksummed upstream releases,
matching the pins in `infra/main.tf`; Bazelisk reads `.bazelversion`. Bazel
provides the Go SDK and the pinned Python FDB client archive. The runner unpacks
that archive itself. The workflow selects Python **3.11** with
`actions/setup-python@v5` rather than Ubuntu 24.04's default Python 3.12, matching
the upstream Rust BindingTester setup's compatibility choice. Use Python 3.11
for reproduction too; **do not `pip install foundationdb`** or substitute an unpinned host binding. If additional Python
package management is ever needed, use `uv`. `just` is available for repository
recipes, but this workflow runs only the BindingTester runner command above.

## Evidence and failure handling

Before running, the workflow hashes every Git-tracked and nonignored untracked
file, records HEAD and Git status, requires a nonempty input manifest and clean
checkout, and checks that HEAD equals `GITHUB_SHA`. It records the workflow
revision (`github.workflow_sha`) separately from the checkout revision. It refuses ambient `user.bazelrc`, home `.bazelrc`, and system `.bazelrc` overrides.
An EXIT trap repeats the manifest, HEAD, and status checks after success or
failure. Added, removed, changed inputs or checkout identity invalidate the run;
verification never replaces an original nonzero runner exit with success.
Ignored outputs are not source inputs. Do not add ignored source/configuration
dependencies or modify inputs while a correctness run is active.

All evidence stays under `$RUNNER_TEMP/client-correctness-evidence`, outside the
checkout. The artifact includes setup and runner logs, the random seed start,
workflow identity, before/after hashes and diffs, exit status, and the runner's
`results/` directory (report and per-case logs/traces). Artifact upload uses
`always()` and fails on missing files, with 14-day retention. A setup failure may
have only partial evidence; a hard runner termination or infrastructure loss can
prevent final hashes, complete reports, or upload. An artifact's presence alone
is not proof that the runner completed. The workflow independently checks the
report's revision, FDB version, nonempty population, successful per-case results,
and completed-cycle count before accepting the run.

Failures produce an **Actions failure summary** with triage directions. There is
no external notification service configured by this workflow; any GitHub email
or UI notifications depend on repository/user settings. Investigate the first
failure and preserve its evidence rather than rerunning it away.

## Reproduction

Use the same checkout SHA and pinned dependencies, with an absolute output path
outside the checkout. Preserve the original artifact before running anything
again. To reproduce the scheduled run, use its saved seed start:

```sh
seed=123456 # replace with seed-start.txt from the artifact
out="$(mktemp -d /tmp/client-correctness.XXXXXX)"
bazelisk run --config=ci //cmd/fdb-binding-stress -- \
  -scheduled -duration 20m -seed-start "$seed" -out "$out/results"
```

For a single failing generated case, copy **all** options from the runner report;
these examples illustrate the relevant flags, not substitutes for that record:

```sh
# Python differential API case.
bazelisk run --config=ci //cmd/fdb-binding-stress -- \
  -test-name api -concurrency 1 -compare-python \
  -seed-start 123456 -seeds 1 -ops 1000 -out "$out/api"

# Concurrent API case (not a Python differential).
bazelisk run --config=ci //cmd/fdb-binding-stress -- \
  -test-name api -concurrency 5 \
  -seed-start 123456 -seeds 1 -ops 1000 -out "$out/api-concurrent"
```

`-test-name` selects `scripted`, `api`, `directory`, or `directory_hca`;
`-concurrency` selects tester concurrency; `-compare-python` enables comparison
with the reference Python binding. `-seed-start` selects the first seed,
`-seeds` bounds the number of generated seeds, and `-ops` selects operations per
seed. `-api-version` defaults to 730; reproduce the borrowed HCA regression with
`-test-name directory_hca -concurrency 5 -ops 100 -api-version 610
-seed-start 3179331380 -seeds 1`. The runner accepts positive 32-bit seeds even
though the workflow draws new exploration starts from the positive 31-bit range.
Use the failing case's operation count, API version, concurrency, comparison
setting, and test name exactly. Directory differential cases use `-test-name directory
-compare-python`; HCA cases use `-test-name directory_hca -concurrency 5`.
Avoid a duration bound when isolating one seed. Replay committed regressions via
scheduled mode as well; a generated one-seed run does not establish that the
regression population passed.

## What a green run does not prove

This is a bounded, randomly seeded sample plus deterministic regression replay,
not exhaustive protocol verification, a full repository suite, a race test, or a
release gate. Python differential modes exercise only their supported sequential
comparison surface; concurrency modes are separate checks, not cross-client
schedule equivalence. Results depend on the recorded operation counts, seeds,
mode population, FDB version, and resource limits. A single hosted job avoids
sharing the local heavy runner, but cannot guarantee reproducible transaction
schedules or eliminate infrastructure failures.
