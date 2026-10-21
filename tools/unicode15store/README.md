# Persisted Unicode 15 compatibility probe

`//tools/unicode15store` is a standalone, pure-Go FoundationDB client. Each
operation commits and then reads committed data in fresh transactions. It uses
Demo.Customer metadata and tokenizer version 0. It deliberately does not share
Java collation key formats.

```sh
bazelisk build //tools/unicode15store
bazel-bin/tools/unicode15store/unicode15store_/unicode15store \
  -cluster-file=/path/to/fdb.cluster -prefix=isolated-run -operation=write
```

`-operation` accepts `write`, `inspect`, `update`, `rebuild`, and `delete`.
Reuse the prefix and cluster file across compiler-built processes. `write` does
not clear an existing prefix: use a fresh prefix for each experiment. `update`
changes text and grouping keys, removes obsolete terms, and replaces collated
primary keys. `delete` loads the persisted records and deletes their actual
primary keys. The program prints its actual `runtime.Version()` to stderr and
JSON to stdout.

The 19 stores are:

- Four TEXT stores: ungrouped/grouped × retained/omitted positions. The input
  includes repeated terms, 25 documents sharing terms (BunchedMap splitting),
  supplementary normalization, Unicode 16/17 witnesses, replacement characters,
  and the existing Go-v0 sigma/segmentation divergences.
- Fifteen opted-in Go-only collated stores: primary/secondary/tertiary × root,
  Swedish, Turkish, Chinese, and `en-u-kn-true`. Each stores a collated composite
  primary key, a collated value index, and collated record-count keys. Records
  include normalization equivalents, accents/case, implicit ideographs, and
  supplementary numeric-locale witnesses. The report includes exact primary
  record/index/count key-value bytes, primary scan order, primary-key loads and
  exact-prefix index range lookup populations.

Raw keys are hex-encoded relative to each store root, so independently allocated
prefixes compare exactly. TEXT reports additionally include decoded tokens,
primary keys, positions, and packed logical entry key/value bytes. Store headers,
transaction versions, and index-build range bookkeeping are not part of the
snapshot. Atomic count keys can remain with zero values after delete; record and
index subspaces must be empty.

## Regression and cross-process runs

The real-FDB test starts one bounded testcontainers FoundationDB 7.3.77 instance
and drives write → rebuild → update → rebuild → delete, plus direct deletion of
an untouched written fixture. It compares all states
against the checked-in pre-pin Go 1.26.10 goldens. Rebuild must preserve raw bytes.
It also checks nonempty populations, sorted raw keys, obsolete raw token-subspace
absence, omitted positions, and final index/record emptiness.

```sh
bazelisk test //tools/unicode15store/harness:harness_test
```

For the RFC-265 cross-compiler gate, retain two executable builds outside the
repository and run the same test with:

```sh
bazelisk test //tools/unicode15store/harness:harness_test \
  --test_arg=-unicode15-old-binary=/absolute/path/to/go1.26-binary \
  --test_arg=-unicode15-new-binary=/absolute/path/to/go1.27-binary
```

Both arguments are required together. The test executes two isolated same-store
sequences, A → B and B → A: write with A, inspect/update with B, rebuild with A,
delete with B, inspect empty with A; then write a fresh fixture with A and delete
it directly with B. Every committed state must match the immutable raw
goldens. Use the saved **pre-pin** Go 1.26 binary as A for the pre-pin-fixture gate.
The optional arguments do not skip a failed gate; they enable the additional
manual matrix. The ordinary test always runs the real-FDB regression.

## Reproducing the raw goldens

Before routing production callers, build and save `//tools/unicode15store` with
Go 1.26.10. Record the executable SHA-256, source identity, compiler stdout/stderr,
and build receipt outside git. The golden exporter always invokes this immutable
executable, not whatever implementation is currently compiled into the test:

```sh
bazelisk run //tools/unicode15store/capture -- \
  -writer=/absolute/path/to/prepin-go1.26-binary \
  -out=/absolute/path/to/exported-goldens
cp /absolute/path/to/exported-goldens/*.json tools/unicode15store/harness/testdata/
```

The capture tool is independent of the current record-layer implementation: it
starts one bounded testcontainers FDB, drives the old executable, verifies its
compiler identity and report population, and exports all five raw states. Its
writer-output validation has unit coverage, including wrong compiler, malformed
JSON, wrong schema, and empty/partial populations. The integration test also
supports `-unicode15-capture-binary` and `-unicode15-capture-dir` for the same
export; permit its external output directory with `--sandbox_writable_path`.
Capture does not rewrite source fixtures automatically. Review the exported
populations and provenance before replacing any checked-in golden. The writer
source and executable used for the initial capture are retained in the RFC-265
external evidence directory; binaries are not committed.

During RFC-265 implementation, prepend the owner's approved `nice -n 19 taskset
-c 0,1,6,7,11,12,13,18,19,23` mask and `bazelisk --batch` to focused commands.
Official fast/full validation lanes remain with the owner.
