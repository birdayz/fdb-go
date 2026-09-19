# RFC-257 upgrade audit artifacts

Companion to [RFC-257](../257-java-4.14.2.0-upgrade.md). This directory is the
durable source-audit record, not a runtime parity certificate. All five reports
and their path ledgers are integrated; `coverage.json` records the verified disjoint
union and history-only accounting. No production implementation or reviewer ACK
is implied.

## Frozen populations

- Go baseline: `e48f5b4965543cd4d99b5578356059e12d969c7c`.
- Java base: 4.12.11.0, `257aa83cae7f90e18ea6595fdf2cf841ca72e802`.
- Java target: 4.14.2.0, `fdacd162a9c8acfadc49082b89185c823ab8ae4a`.
- `manifest.json`: 1,189 unique net-changed destination paths, assigned to five
  disjoint research groups. Renames are recorded by destination here.
- `commit-manifest.json` / `commits.txt`: 228 in-range commits, subjects and
  per-commit touched paths. The union has 1,273 paths, not 1,189.
- `history-only.json`: the 84 paths outside the net destination population.
- `renames.json`: rename-source/destination correspondence.

The source reports are preserved verbatim, including their explicit limits.
Absolute workspace links and cited line numbers refer to the frozen revisions,
not to later implementation line numbers. A report's `Wn` labels are local to
that report. `*.prompt` records the actual research assignment and model scope.
`*-ledger.json` maps each assigned path to its expanded report-ledger line and
pins the report SHA-256; this checks accounting, not the truth of the classification.

| Report | Assigned net paths | Accounting |
|---|---:|---|
| [build-docs](build-docs.md) | 126 | 126 exact assigned paths found in ledger |
| [extensions-vector](extensions-vector.md) | 170 | 170 after expanding its declared E/ET/EF/L/LT/IT prefixes |
| [record-storage](record-storage.md) | 207 | 207 after expanding its declared C/J/F/I/P/T/TF/TI/TP/X/XP prefixes |
| [cascades](cascades.md) | 282 | 282 after expanding its declared M/C/R/V/P/T/F prefixes |
| [relational](relational.md) | 404 | 404 after expanding literal heading prefixes and the 79 metric pairs |

All five researchers were launched read-only with `gpt-6-astra` and `xhigh`
reasoning. They were told to read the entire assigned diff, inspect actual Go
mechanisms and separate new deltas from pre-existing gaps. They ran no builds or
tests. Reports are research, not virtual Graefe/Torvalds/Codex ACKs. Transient
bubblewrap/autofs command failures occurred during research; a successful later
read or an explicit unread-scope disclosure is required, not an inferred read.
The binary Gradle wrapper was identified but not disassembled. All 79 assigned
binary metrics were decoded and compared; DOT strings were compared, not rendered
for visual graph inspection. RFC-257's cross-report reconciliation records the
controller's decisions where report shorthand needs qualification.

## History-only reconciliation

The net-diff reports alone would miss these categories:

- **72 rename sources:** each maps to an audited destination; source paths are not
  additional live target files. The rename ledger prevents treating movement as
  removal of the represented behavior.
- **Seven net-zero paths:** identical base/target Git blob IDs are recorded. The
  cursor-serialization changes in #4436 (`f1c848ef8`) were reverted by #4482
  (`f4b15ab3a`). Do not port an intermediate cursor format absent from the target.
- **Five intermediate-only paths:** two extension test helpers moved into fixtures
  in #4418 (`b9ceed781`); the pending-write drainer/factory introduced in #4293
  (`6a685875a`) were replaced by #4350 (`244241bd1`); the stored-query test was
  renamed by #4291 (`02af41b57`). Their final replacements belong to the source
  reports. The queue replacement explicitly changes experimental queued-write
  format, requiring disable/rebuild of intermediate queued indexes. It does not
  make ordinary WRITE_ONLY indexes incompatible.

To reproduce inventory, run against the Java checkout at the named SHAs:

```sh
git -C fdb-record-layer rev-list --count \
  257aa83cae7f90e18ea6595fdf2cf841ca72e802..fdacd162a9c8acfadc49082b89185c823ab8ae4a
git -C fdb-record-layer diff --name-status \
  257aa83cae7f90e18ea6595fdf2cf841ca72e802 fdacd162a9c8acfadc49082b89185c823ab8ae4a
```

The `.paths` files contain exact arguments for the scoped full diffs. Passing
them as an argument array avoids whitespace/glob reinterpretation. Inventory
completeness is distinct from individually reviewing each intermediate commit;
the reports read complete net diffs plus necessary history, and this supplement
accounts for paths erased by netting/reverts.

## Build and schema evidence

`maven.json` records availability of the three target Maven POMs.
`proto-sync.json` records SHA-256 for **all 16 canonical production protos**;
each local copy was compared byte-for-byte with the target source. Fixture protos
are not production-schema inputs.

`classpath.json` records the resolved Maven coordinates and the actual Bazel
`JavaRuntimeClasspathInfo` dependency jars. At the prepared MODULE hash it has
**28 Maven entries and 33 runtime dependency jars**, excluding the launcher jar.
The launcher adds its own jar before those dependencies. The runtime places
Bazel protobuf `libcore.jar`/`liblite_runtime_only.jar` before Maven protobuf-java
4.29.3; util remains 4.29.3. Record Layer's published POM requests 3.25.9. This
is a real mixed resolved classpath, not proof that gRPC requires protobuf 4.
The three target application artifacts, extensions and annotations resolve to
4.14.2.0; FDB Java remains 7.1.26. ICU 72.1 in this oracle is ANTLR's dependency,
not proof that the separate target ICU index module (78.3) was exercised.
The oracle does not enable the incubator vector module; its runs do not establish
coverage of Java's SIMD backend.

Reproduce the runtime list with the provider's actual qualified key:

```sh
bazelisk cquery //conformance:conformance_server --output=starlark \
  --starlark:expr='"\n".join([f.path for f in providers(target)["@@rules_java+//java/private:java_common.bzl%JavaRuntimeClasspathInfo"].runtime_classpath.to_list()])'
```

An initial `JavaInfo` key lookup produced an evaluation error despite cquery exit
zero; its qualified version returned only the binary's own jar. Neither is the
runtime dependency population. The retained successful measurement uses the
runtime provider and explicitly checks production proto/runtime/application jars.

## Runtime evidence and retained tests

`initial-full-suite.json` records the frozen initial run's 92 target verdicts,
uncached flags, log hashes and reconciled Go outcomes. **It is red:** 87 targets
passed and five failed. The broad conformance target separately executed 1,325
of 1,444 Ginkgo specs (1,321 passed, four failed, 119 skipped). The five restricted
opt-in Go hunts remain unapproved, not passing coverage.

The original raw Go outcome grep double-counted allocation subprocess output
embedded in test logs. `go tool test2json` ignores those indented log payloads;
RUN and terminal-event multisets were checked for every target. The correct
population is 40,374 RUN = 40,354 PASS + 15 FAIL + five SKIP. A duplicate test name
across simfdb's internal/external packages has two RUNs and two outcomes; retaining
both is correct. Parent and subtest failures are included in these counts.

The executable regressions remain in their owning existing test targets:

- `values/value_array_constructor_test.go`:
  `TestArrayConstructorValue_CheckedRebuildNestedNumericCarriers`.
- `values/values_test.go`: `TestPromoteValue_EvaluateRecordNumericCarriers`.
- `sqldriver/wrapper_hidden_child_fdb_test.go`:
  `TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes`.
- `conformance/record_constructor_java_probe_test.go`: exact four-case target-Java
  success rows, no mixed-width failure acceptance.
- `recordlayer/metadata_proto_fidelity_test.go`: actual stored-query content and
  cloning, not merely a populated-fixture guard. The updated pre-fix unit run has
  seven RUN events, one PASS and six FAIL (parent/subtest outcomes included).
- `conformance/metadata_store_conformance_test.go`: Java writes actual stored-query
  metadata, Go loads/rebuilds/saves the model, Java consumes it. The pre-fix selected
  spec fails at the Go-model boundary after the Java-written contents were verified.
- `conformance/vector_index_conformance_test.go`: Java access-info encoding,
  exact Java nearest results, and Go self-distance. Five prior repetitions fail;
  the strengthened case measures the coordinate-provenance defect directly.

Full local command outputs, BEP, frozen hashes and initial failed attempts remain
under `/var/tmp/query-grind-cast/java-upgrade/`, notably `before/`, `after/`,
`full/`, `metadata-preservation/`, `rabitq-repro/` and `rabitq-entry/`. Those local
logs supplement, not replace, the durable conclusions and retained tests. No
failure expectation, tolerance or golden is weakened by this record.

## WS-A first design review and additional evidence

`design-v1/` retains the exact prompts and verdicts for virtual Git tree
`f6efe9b8fd99522d2b95568a99eaac9ae7acb72b`: Graefe/storage ACK,
Torvalds/independent NAK. All used `gpt-6-astra`/`xhigh`/read-only. The NAKs require
coercion preparation/mutation ownership, an explicit legacy Go HNSW migration,
and candidate-to-node/inline-edge encoding plus cache invariants. RFC-257 records
the revised decisions. These ACKs do not cover the revised tree or implementation.

`legacy-entry-producer.patch` is the retained regression applied to the exact Go
parent's test file, leaving production code untouched. It inserted PKs 1 and 2,
established a centroid, deleted entry PK 1, committed, and cold-reopened in another
transaction. The selected spec failed because entry PK 2 was stored raw instead
of transformed. The two persisted prefix-relative KVs and source/log hashes are
in [`hnsw_legacy_go_entry.json`](../../pkg/recordlayer/testdata/hnsw_legacy_go_entry.json).
A prior same-transaction attempt printed `fdb.Key` through its string formatter;
those escaped-text keys were discarded. The retained fixture uses explicit byte
slices and committed cold-reopen output. This fixture proves the old writer defect,
not upgraded-reader compatibility. A Java-compatible tuple lacks writer provenance;
RFC-257 requires disable/rebuild for legacy Go RaBitQ writer histories, not a
heuristic decoder that guesses coordinate systems.

`stress-baseline.json` records two sequential uncached 1M baseline samples at
`e48f5b4965543cd4d99b5578356059e12d969c7c`, the exact parent/merge-base when measured.
Both have 24 RUN/PASS events and matching row counts over 22 timed query entries;
full-scan COUNT is separately pinned at 1,000,000. Tracked-file hashes were verified
after both runs; load, filesystem, BEP results and log hashes are retained. No
current-tree measurement or performance ratio is claimed.

## WS-A second design review

`design-v2/` retains all four prompts and verdicts for virtual tree
`822e4f49bb9f03d26659a7520835953c60998709`: Torvalds/storage ACK,
Graefe/independent NAK. The remaining findings require a collect/register/seal/bind
publication phase (one repository can still contain multiple descriptor identities)
and move exact RaBitQ encoder/norm fidelity into WS-A before canonical rebuilt-byte
acceptance. The revised RFC records both decisions. No production implementation
or current-tree approval follows from the earlier ACKs. The owner has explicitly
requested completion through an approved PR; publication is authorized, gates intact.

## WS-A implementation evidence after design acceptance

`design-v3/` retains all four design ACKs for virtual tree
`84844bdb679dacefde59a61efe67969f7bc9be11`; these do not approve implementation.
The working tree now includes RaBitQ arithmetic/provenance and mandatory rebuild
proofs, stored-query field-16 preservation, prepared recursive promotion and
transactional collect/register/seal/bind descriptors. Current bounded results and
remaining gates are recorded in RFC-257's **Execution ledger**, including its
**WS-A prepared promotion and descriptor publication** subsection. The initial
red run above remains historical; a subsequent full run also failed (84/92
passed), with repairs tracked rather than its result relabeled green. No full
upgrade pass, current performance comparison or implementation ACK is implied.
