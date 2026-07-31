# Vendored yamsql acceptance corpus (Apple fdb-record-layer)

This tree is a **verbatim** copy of the `.yamsql` acceptance-test corpus from
Apple's [fdb-record-layer](https://github.com/FoundationDB/fdb-record-layer),
Copyright Apple Inc. and the FoundationDB project authors, licensed under the
Apache License, Version 2.0.

The pinned upstream tag is recorded in [`VERSION`](VERSION) and matches the tag
the Go port is written against (see `CLAUDE.md`).

## Provenance

| | |
|---|---|
| Upstream path | `yaml-tests/src/test/resources/` |
| Local path | `yaml-tests/src/test/resources/` (mirrored exactly) |
| Files | `*.yamsql` only |

The corpus is the input to `pkg/relational/conformance/javayamsql`, which parses
every file and asserts the directive/tag surface has not drifted.

## Never edit these files

They are upstream data, not our source. Editing them destroys the only property
that makes them useful — that a Go/Java behavioural difference is attributable to
the engine and not to a locally-tweaked expectation. Corpus entries the Go engine
cannot yet satisfy are recorded in `manifest.go` in the Go package, never by
changing the `.yamsql`.

## What is deliberately excluded

`*.metrics.binpb` and `*.metrics.yaml` are **not** vendored. They are counters of
Java planner-internal task/event totals, asserted only by Java's `explain`
config. They describe the Java planner's search, carry no cross-engine meaning,
and would rot on every upstream planner change without ever failing usefully
here.

Also excluded, as they are not corpus data: `log4j2-test.properties`,
`serialization-keys.p12`, `valid_identifiers_metadata.json`,
`import-schema-template/with_included_dependencies_metadata.json`.

## Re-sync procedure

Clone the Java repo at the desired tag to the repo root (it is gitignored there),
then rsync `.yamsql` files only:

```sh
git -C fdb-record-layer checkout <tag>

rsync -a --delete --prune-empty-dirs \
  --include='*/' --include='*.yamsql' --exclude='*' \
  fdb-record-layer/yaml-tests/src/test/resources/ \
  third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/

echo '<tag>' > third_party/apple/fdb-record-layer/VERSION
```

Then run the parse gate:

```sh
bazelisk test //pkg/relational/conformance/javayamsql:javayamsql_test
```

`TestCorpusParses` pins the measured census (file/block/query/config/tag totals).
A tag bump that changes the corpus will fail it with a diff of the counts, and a
tag bump that introduces a new directive or YAML tag will fail it by name. Both
are intended: update the pinned baseline in the same commit as the re-sync, so
the format delta is reviewable.
