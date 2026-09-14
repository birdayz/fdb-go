---
name: query-grind
description: "Grind query-engine correctness coverage using the existing yamsql, factory/factorycorpus, rowdiff, sqlhunt, metamorphic and DST tooling. Use for hardcore test campaigns, gap-directed LLM/subagent generation, semantic boundary combinations, mutation-verified assertions, and continuing TODO.md section 12 QSC work. Invoking this skill starts the research → generate → challenge → execute → minimize → fix → pin loop, not another planning-only pass."
---

# Query correctness grinding loop

## Mandate

Close named semantic coverage gaps and find real bugs. **Use the existing tooling; do
not build a parallel testing framework.** More files, more SQL, more seeds and a green
suite are not proof that the relevant combinations ran or that the oracle can detect
wrong results. Each admitted case needs a contract, an independent oracle, an execution
witness where a route matters, and a durable replay.

The work queue is [TODO.md §12](../../../TODO.md), QSC-01 through QSC-11. Read the
current entries, appended implementation blocks and linked RFCs on every resumption.
They contain the current scope, completed slices and remaining gaps; this skill does
not freeze their changing counts. Do not mark a broad QSC item done because one bounded
slice is green.

**Work on the current branch unless the user requests otherwise.** Do not create a
branch per batch, switch branches, push, open a PR or merge merely because another
workflow normally does so. Make logical commits after verification. Preserve existing
user edits; stage explicit paths and inspect the staged diff. Follow the normal hooks,
never bypass them. If unrelated user edits prevent the hook from checking the intended
index, preserve them and use a clean comparison worktree for verification/commit rather
than hiding or reverting them; verify parent and tree identity before adopting it.

## Start now, not with another proposal

1. Run `git status --short`, identify the current branch/HEAD, check disk space, and
   locate active jobs before starting expensive tests.
2. Read §12 and its newest implementation block. Resume any unresolved finding first.
   Otherwise select the next unvalidated interaction in the current semantic family.
3. Read the actual generator, comparator, runner and corpus schema you will use. Know
   which distinctions they erase and which paths they execute. Use the command's current
   flags/source, not remembered flags. A harness limitation is not engine coverage.
4. Read Java for the shared contract before changing code. For a Go-only extension,
   establish an explicit independent contract; never present Go output as its own oracle.
5. Write one self-contained campaign block at the END of the shared tracking section:
   target QSC IDs, finite axes, missing cells, oracle basis, owning existing harness,
   intended witnesses and exact next action. Keep changing campaign state out of this skill.
6. **Start the first concrete batch in the same invocation.** Do not stop after writing
   a plan or asking whether to proceed. Research/candidate generation is useful work;
   an implementation still needs the applicable design gate before code changes.

## Pick a semantic slice, not an arbitrary test count

Follow QSC-07's ordering: numeric/type transport → NULL/Boolean/empty inputs → relational
composition → names/nested values → remaining scalar domains and DML/index effects.
Within a family, cross producers with consumers instead of testing isolated functions:

- Producers: decimal/exponent literals, actual driver args, stored fields, casts,
  scalar expressions, aggregates, scalar subqueries and derived/unnested fields.
- Consumers: projection/metadata, arithmetic/casts, WHERE/HAVING, join predicates,
  composite GROUP BY/DISTINCT, sorting/ties, LIMIT/OFFSET admission, DML/index writes.
- Boundaries: integer versus whole DOUBLE, FLOAT precision, ±0, NULL, empty and
  duplicate-heavy data, overflow, nonfinite admission, conversion limits.
- Composition: float keys in NON-final positions, correlated AND uncorrelated forms,
  outer-join null extension, alternative scans/joins, covering/fetch, paging and
  successive sign/type changes on the same connection where that route is in scope.

List the finite required population independently of the generator. Pairwise exploration
is not full Cartesian coverage. Leave outside-envelope domains and unavailable input
routes visible. Driver parameter text substitution is not executor `WithParams` binding;
SimFDB is not real wire-transport fidelity. Scope all counts.

## Existing homes — choose, extend, and compose them

| Existing home | Use |
|---|---|
| `pkg/relational/conformance/yamsql` | Explicit SQL expectations, typed args, exact carrier/bit assertions, metadata/errors and durable real-FDB regressions |
| `pkg/relational/conformance/factory`, `factorycorpus`, `cmd/factory-run` | Structured candidates, alternative-plan/partition checks, provenance, corpus admission and promotion |
| `pkg/relational/conformance/rowdiff` | Small independent expected-result models and real-FDB query/projection/paging comparisons |
| `pkg/simfdb/hunt/sqlhunt` | Fast deterministic stateful SQL exploration against focused models |
| `pkg/simfdb/hunt/metamorphic`, `cmd/dst-generate` | Hand-written and LLM-authored equivalence families replayed over SimFDB |
| `pkg/relational/conformance/explaindiff`, `plandiff` | Observed plan contracts/diversity and shared-surface cross-engine evidence |
| `pkg/relational/sqldriver`, Cascades value/rule tests | Targeted API, storage, evaluator and planner regression pins |
| Existing Bazel targets/nightlies | Corpus replay, fuzz, race, coverage and mutation checks |

Compose [dst](../dst/SKILL.md) for reproduction/fuzz/simulation mechanics and
[query-engine](../query-engine/SKILL.md) for design and implementation review. A client
bug additionally uses the client engineering/review skill and C++ as its specification.

### Tool commands: discover the actual route

These are entry points, not proof that every semantic axis is implemented:

```sh
# Read the candidate formats, models, constraints and supported CLI flags.
rg -n 'flag\.|type Scenario|type Group|func Check|func LoadDir' \
  cmd/factory-run cmd/dst-generate pkg/simfdb/hunt/metamorphic
rg -n '^func (Test|Fuzz)' pkg/relational/conformance/yamsql \
  pkg/relational/conformance/rowdiff pkg/simfdb/hunt/sqlhunt

# Metamorphic replay: supply an existing-format directory, not generated shell code.
bazelisk run //cmd/dst-generate -- -dir /absolute/path/to/candidate-json

# Strict SQL/model/corpus replay through existing targets.
bazelisk test //pkg/relational/conformance/yamsql:yamsql_test \
  --nocache_test_results --test_output=all
bazelisk test //pkg/relational/conformance/rowdiff:rowdiff_test \
  --nocache_test_results --test_output=all
```

`factory-run` writes candidates and census/manifests; inspect its `-out`, `-findings`,
`-manifest`, `-pr-body`, `-date`, seed and Java options before running a batch. Use an
explicit work area for unadmitted candidates. Do not rewrite a census baseline merely
to make it green. Without an independently checked oracle, metamorphic agreement is
exploration evidence, not a correct-answer certificate.

`dst-generate` reports setup/query errors separately from inequivalence; **exit zero
alone is not admission**. Inspect non-empty scenario/group counts and every error class.
A supported query that errors is a finding; an invalid equivalence is an oracle finding.
The output's "unsupported SQL" label does not adjudicate either for you.

## Delegate focused authoring; keep integration serialized

Use available subagent tooling. Parallelize read-only research and isolated candidate
authoring, never shared manifest/generator edits or multiple large implementations.
Have one integration owner and a separate challenger. If subagents are unavailable,
perform the same author/challenger checks locally and say so; do not invent agent runs.

An author batch receives:

- Named missing cells and the exact finite axes, not "write lots of SQL".
- Grammar/catalog references, existing fixture format, already-covered examples and
  known oracle/transport limitations.
- Expected dataset dimensions, route witnesses and allowed authority for each claim.
- An isolated output location. Output only structured scenario inputs and reasoning;
  never generated shell commands, production edits or manufactured EXPLAIN text.

Require each candidate to carry: cell IDs, DDL/data, typed bindings, query or statement
sequence, claimed result/relation, authority and preconditions, intended route, and
provenance (source/prompt/model revisions). **Persist the exact generated inputs**;
an LLM seed is not a replay artifact. Proposed answers are untrusted until checked.

The challenger attacks: NULL/three-valued logic, duplicates, empty inputs, ordering ties,
integer overflow, floating reassociation, error/evaluation order, correlated versus
uncorrelated behavior, and common-mode derivations. Two agents agreeing is not an oracle.
Use hand derivation, a small independent model, or pinned Java behavior. Never call the
production evaluator/cast/comparator to calculate its own expected answers.

## Execute → investigate → fix → retain

For each candidate batch:

1. Strict-load inputs and validate contracts/preconditions. Classify malformed candidates,
   missing oracles and unsupported routes; never erase them into an apparently complete run.
2. Run existing fast model/metamorphic checks, then the real-FDB path needed for the
   contract. Inspect complete outputs, explicit statement outcomes and route witnesses.
3. Assert the distinctions the contract requires: bits/carriers and SQL metadata are
   separate; unordered results preserve multiplicities; signed-zero comparisons need
   the right position and policy. Mathematical numeric equality is still appropriate
   where specified; do not impose bitwise equality globally.
4. **On any mismatch or flake, stop that queue and DFS it to completion.** Preserve the
   input, reproduce, establish the Java/extension contract, distinguish engine/oracle/
   infrastructure failure, minimize structurally, fix, and retain a regression. Do not
   relabel a valid failing query "bad generation" or update expectations to match Go.
5. Keep the exact failing dimension when reducing: composite key position, NULL placement,
   ordering, binding type, correlation, plan route or continuation boundary. Amplify nearby
   combinations after the fix. Every load-bearing probe, including a negative one, stays
   as a test rather than being deleted after it informed a decision.
6. Promote independently justified inputs into the existing corpus/tests. Preserve semantic
   identity where the current shape-only dedup drops literal/type/sign distinctions.
   Extend that existing mechanism when necessary; do not start a second corpus framework.
7. Mutation-check the detector: prove the edit applied exactly, compile, observe the
   expected test and failure reason, restore, verify hashes, rerun green. Alias swaps,
   zero-sign/carrier loss, missing args, dropped duplicates and weakened denominators are
   useful semantic mutants. A build failure is not a killed semantic mutant.
8. Reconcile the executable denominator and report required/applicable/exercised/validated
   cells separately. Missing outcomes, an empty/partial run, stale digests and false route
   labels cannot earn credit. Reports and gates must share the same calculation.
9. Record the result in the campaign's durable TODO/RFC block. Feed missing witnesses,
   rejected relations, surviving mutants and neighboring failures into the next batch.

## Verification and review are part of the loop

- Use finite, explicit command timeouts and full log files. Wait for jobs before declaring
  results; do not end with an unattended "monitor". Do not oversubscribe Docker-heavy runs
  or run binding stress concurrently with `just test`.
- Confirm the requested tests actually ran (names and RUN lines), not merely the target.
  Use uncached affected Bazel runs. A new test file requires `just gazelle`,
  `bazelisk mod tidy`, and an observed sandboxed execution. Use `gofumpt`.
- Fuzz newly extended codecs/comparators/generators, and report whether the build actually
  provides coverage guidance. Retain minimized failures; seed counts alone aren't coverage.
- For planner/executor changes, follow the existing stress/performance and Java-reference
  requirements. A harness-only change is not a reason to invent a planner speed claim.
- Follow RFC/design → ACK → implementation → milestone review. Graefe/Torvalds review the
  completed slice, with Codex at that same granularity and a final delta confirmation after
  findings. No per-commit review treadmill. Existing approved designs cover work only within
  their actual scope; new harness/engine mechanisms need their design gate.
- Run `just test`, commit a logical verified change, continue on the same branch. No push
  without permission. Keep user edits out of the index/commit and preserve their bytes.
- Record exact commands, source revisions/hashes, non-empty populations and observed
  outcomes. Fix superseded claims everywhere they occur. Hash inventories use explicit
  locale-independent ordering (`LC_ALL=C sort`); never confuse a sorting difference with
  changed source bytes.

## Continue until externally stopped

One green batch is the input to the next, not a stopping point. Do not substitute
repeated planning, review churn, test-count padding or arbitrary seed volume for closing
missing combinations. Finish the active finding/slice, reconcile evidence, then take the
next gap in the same family. Widen according to QSC-07 only when the current bounded slice
has its oracle, witnesses, mutations and replay pins.

Stop for user intervention or a genuine external dependency/owner decision you cannot
resolve. State that explicitly with the reproducer and durable location; do not silently
file it and move laterally. Any necessary handoff names the exact open finding, current
branch/HEAD, artifacts, completed verification and next command. Never promise autonomous
work after the session ends.
