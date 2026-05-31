# Review Queue

Use this file to review recent PRs one by one. Check off each PR only after the code, tests, docs, and prior review discussion have been inspected at the merged/head commit.

## Review Instructions

1. Gather context:
   - Read the PR title, body, linked RFCs/issues, changed files, and prior comments.
   - Confirm the exact commit being reviewed.
   - If the PR has earlier findings, verify the final commit rather than trusting the discussion.

2. Inspect the implementation:
   - Read the changed code and the surrounding existing code.
   - Follow data/control flow across module boundaries touched by the change.
   - Check invariants that are easy to state but easy to violate: identity, aliasing, equality/hash consistency, determinism, ordering, error handling, cancellation, and resource ownership.
   - Look for edge cases that are not in the happy-path tests: collisions, duplicate keys, nil/empty inputs, retries, partial failures, and mixed old/new representations.

3. Inspect tests:
   - Verify the tests actually fail on the old bug and assert values, not only counts or non-errors.
   - Look for missing regression tests around the tightest invariant.
   - Prefer small focused unit tests for pure logic and end-to-end tests for planner/executor behavior.

4. Run targeted verification:
   - Run the smallest meaningful test command first.
   - Broaden testing if the change touches shared behavior.
   - Record any command that was not run and why.

5. Write the review:
   - Lead with findings, ordered by severity.
   - Use concrete file/line references.
   - Explain why the issue can happen, not just what looks suspicious.
   - Include a minimal fix direction and the test that should cover it.
   - If there are no findings, say that clearly and mention residual test gaps.

6. Post to GitHub:
   - Use `REQUEST_CHANGES` for blocking findings when GitHub allows it.
   - If GitHub rejects request-changes on your own PR, post the same content as a regular review comment.
   - After posting, copy the review link or review id into the PR section below.

## Current Review Target

- [x] #217 feat(cascades): generic N-way join execution (RFC-043, PR-D)
  - Review posted: `review_id=4396011400`
  - Finding: `mergeQuantifierAlias` was stable but not injective for alias names containing underscores.
  - Verification:
    - `go test ./pkg/recordlayer/query/plan/cascades/values -run TestJoinMergeAllValue`
    - `go test ./pkg/relational/sqldriver -run TestFDB_MultiwayJoinOrder_Nway`

## Backlog

### → Resolved in PR #219 (RFC-044, `rfcs/044-codex-review-followups.md`)

The four backlog findings below (#213–#216 Finding 1) were triaged and addressed:

- **#216** `composeFieldOverJoinMerge` blind inner pick — **REAL (Go-only extension bug).** FDB probe proved it unreachable (`SelectMergeRule` re-flows the merge under the inner alias, so only inner-side bare fields reach the rule). Fixed: replaced the paper-over comment with the structural invariant + qualified-field fail-safe. Tests: `TestSimplifyValue_FieldOverJoinMerge_QualifiedOuterPreserved`, `TestFDB_JoinMerge_OuterColumn_NotDropped`. Root fix filed as TODO 7.6.
- **#215** `ChildrenAsSet` permutes LEFT OUTER joins — **REAL latent (Go-only extension bug).** Fixed: `ChildrenAsSet()` narrowed to `JoinInner`/`JoinCross`. Test: `TestMemoEqual_OuterJoinNotChildrenAsSet`.
- **#214** `IndexEntryObjectValue` ignores `Source` — **REAL (Java-parity divergence).** Codex's "equality collapses" framing was half-right; the clean fix folds `Source` into **both** equality and hash. Test: `TestSemanticEquals_IndexEntryObject_Source`.
- **#213** `integrateOne` single equivalent ref — **FALSE POSITIVE.** Self-contradictory under memo interning + RFC-037 merge invariants; Java has no group merge. No change; reachable cyclic case already pinned by `TestMemoMerge_SkipsCyclicMerge`.

Graefe + Torvalds ACK (RFC + implementation); each regression verified to fail on pre-fix code.

## #216 feat(cascades): FROM-order-independent 3-way join ordering (RFC-042)

- [x] Review complete
- Author: birdayz
- Status: merged 3 hours ago
- Task status: 4 tasks done
- Metadata count: 4
- Review link/id: `review_id=4396033214` (`REQUEST_CHANGES` rejected on own PR; posted as COMMENT)

### Findings

- Finding 1: `composeFieldOverJoinMerge` rewrites every `field(join_merge{outer,inner}, f)` to `field(QOV(inner), f)`, which is not semantics-preserving for fields that exist only on the outer side. A temporary negative unit test showed `OUTER_ONLY` evaluating to `42` before simplification and `<nil>` after simplification.
- Finding 2: None.
- Test gaps: Add a regression test for `FieldValue` over `JoinMergeResultValue` where the field is present only on the outer side. The existing test covers only the inner-side case.
- Latest master check: Still applies on fetched `origin/master` at `6809439065a6095f389b846038fe36cf5d492161` (`refs/heads/master`, PR #216 merge commit). A temporary regression test expecting the outer-only field to survive simplification failed with `after simplify = <nil>, want 42`.
- Verification:
  - `git fetch origin master`
  - `git ls-remote origin refs/heads/master`
  - `go test ./pkg/recordlayer/query/plan/cascades/values -run TestSimplifyValue_FieldOverJoinMerge_OuterOnlyStillPreserved -count=1` (temporary test; failed as expected on latest master)
  - `go test ./pkg/recordlayer/query/plan/cascades/values -run TestSimplifyValue_FieldOverJoinMerge`
  - `go test ./pkg/recordlayer/query/executor -run TestExtractEquijoinColumns_QOVChild`
  - `go test ./pkg/recordlayer/query/plan/cascades -run TestLowerAliasesConnected`
  - `go test ./pkg/relational/sqldriver -run 'TestFDB_MultiwayJoinOrder_Probe|TestFDB_MultiwayJoinOrder_Limit|TestFDB_MultiwayJoinIndexProbe|TestFDB_TwoTableJoin_OrderInvariant'`

## #215 feat(cascades): activate alias-aware memo interning (RFC-038 PR-A / RFC-039)

- [x] Review complete
- Author: birdayz
- Status: merged 3 hours ago
- Task status: 4 tasks done
- Metadata count: 6
- Review link/id: `review_id=4396045604` (`REQUEST_CHANGES` rejected on own PR; posted as COMMENT)

### Findings

- Finding 1: `MemoEqual` now permutes every expression whose children report `ChildrenAsSet()`, and `SelectExpression.ChildrenAsSet()` is true even for `JoinLeftOuter`. Because `memoizeNonLeaf` now trusts `MemoEqual`, `MemoizeExpression(A LEFT OUTER JOIN B)` and `MemoizeExpression(B LEFT OUTER JOIN A)` intern to the same canonical `Reference`, which is not semantics-preserving for left joins.
- Finding 2: None.
- Test gaps: Add a regression asserting swapped `JoinLeftOuter` `SelectExpression`s do not compare `MemoEqual` and do not memoize to the same reference. The existing `TestE2E_JoinCommutativitySkippedForLeftJoin` only proves the planner does not deliberately explore the swapped implementation direction.
- Latest master check: Still applies on fetched `origin/master` at `6809439065a6095f389b846038fe36cf5d492161` (`refs/heads/master`, PR #216 merge commit). The temporary swapped-left-join memo regression failed there with `memo interned swapped LEFT OUTER joins into the same Reference`.
- Verification:
  - `git fetch origin pull/215/head:refs/remotes/origin/pr/215`
  - `git fetch origin master`
  - `go test ./pkg/recordlayer/query/plan/cascades/expressions -run 'TestMemoEqual|TestAliasMap' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -run 'TestMemoActivation|TestMemoMerge|TestMemoize' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -run TestE2E_JoinCommutativitySkippedForLeftJoin -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -run TestMemoizeExpression_LeftOuterJoinOrderIsSignificant_Tmp -count=1` (temporary test on PR head; failed as expected)
  - `go test ./pkg/recordlayer/query/plan/cascades -run TestMemoizeExpression_LeftOuterJoinOrderIsSignificant_Tmp -count=1` (same temporary test on latest `origin/master`; failed as expected)

## #214 feat(cascades): alias-aware equality + alias-invariant hashing foundation (RFC-040)

- [x] Review complete
- Author: birdayz
- Status: merged yesterday
- Task status: 5 tasks done
- Metadata count: 7
- Review link/id: `issuecomment-4584986623` (`REQUEST_CHANGES` unavailable after merge; posted as COMMENT)

### Findings

- Finding 1: `IndexEntryObjectValue` equality still ignores `Source`. `SemanticEqualsUnderAliasMap` falls through to `EqualsWithoutChildren`, whose `IndexEntryObjectValue` case compares only `OrdinalPath`; `SemanticHashCode` also folds only `OrdinalPath`. That collapses `KEY[0]` and `VALUE[0]` even though `Evaluate` reads `PrimaryKey()` for `TupleSourceKey` and `IndexValues()` for `TupleSourceValue`.
- Finding 2: None.
- Test gaps: Add a regression beside `TestSemanticEquals_IndexEntryObject_OrdinalPath` that constructs KEY and VALUE variants with the same alias and ordinal path and asserts they are not semantically equal. The existing regression varies only `OrdinalPath`.
- Latest master check: Still applies on fetched `origin/master` at `6809439065a6095f389b846038fe36cf5d492161` (`refs/heads/master`, PR #216 merge commit). A temporary regression test expecting KEY and VALUE index-entry objects with the same ordinal path to compare unequal failed with `KEY and VALUE index-entry objects with same ordinal path compare equal`.
- Verification:
  - `git fetch origin pull/214/head:refs/remotes/origin/pr/214`
  - `git fetch origin master`
  - `git ls-remote origin refs/pull/214/head refs/heads/master`
  - `go test ./pkg/recordlayer/query/plan/cascades/values -run TestIndexEntryObjectValueSourceIsSemanticDiscriminatorTmp -count=1` (temporary test on PR head; failed as expected)
  - `go test ./pkg/recordlayer/query/plan/cascades/values -run 'TestSemanticEquals_IndexEntryObject_OrdinalPath|TestIndexEntryObjectValue' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades/predicates -run 'TestSemanticEquals|TestPredicateEquals|TestStructurallyEqual' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -run 'TestValueSemanticHashCode|TestPredicateSemanticHashCode|FuzzValueSemanticHashConsistency|FuzzPredicateSemanticHashConsistency' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades/expressions -run 'TestRelationalAliasCompleteness|TestAliasMap' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades/values -run TestIndexEntryObjectValueSourceIsSemanticDiscriminatorTmp -count=1` (temporary test on latest `origin/master`; failed as expected)

## #213 feat(cascades): cross-Reference equivalence-class merging - full Graefe Memo (RFC-037)

- [x] Review complete
- Author: birdayz
- Status: merged yesterday
- Task status: 5 tasks done
- Metadata count: 8
- Review link/id: https://github.com/birdayz/fdb-record-layer-go/pull/213#issuecomment-4585006345

### Findings

- Finding 1: `Memo.integrateOne` asks `findEquivalentRef` for only one equivalent group. If that first group is not mergeable, for example the existing `Distinct(Distinct(scan))` descendant case that would create a cycle, the code falls through to `indexExpr` instead of continuing to later equivalent sibling groups. A temporary regression with `outer = Distinct(Distinct(scan))`, a sibling `Distinct(scan)`, and a yielded `Distinct(scan)` into `outer` failed with `MergeCount() == 0`; expected the cyclic inner candidate to be skipped and the sibling to merge.
- Finding 2: None.
- Test gaps: Missing coverage for candidate-order cases where the first equivalent ref is intentionally non-mergeable but a later sibling ref is mergeable.
- Verification:
  - `git ls-remote origin refs/heads/master refs/pull/213/head` -> master `6809439065a6095f389b846038fe36cf5d492161`, PR head `bf8c5a17e567413598d9d8c8e5ab49c614198ffb`
  - `go test ./pkg/recordlayer/query/plan/cascades -run 'TestMemoMerge|TestMemoize|TestMemo' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades/expressions -run 'TestReference|TestSemanticEquals|TestAliasMap' -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -run TestPlanner -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades/expressions -count=1`
  - `go test ./pkg/recordlayer/query/plan/cascades -run TestMemoMerge_ContinuesPastCyclicCandidate -count=1` (temporary test on PR head; failed as expected)
  - Static check against latest `origin/master` at `6809439065a6095f389b846038fe36cf5d492161`: same single-candidate `integrateOne`/`findEquivalentRef` flow remains present.

## #212 docs: clarify wire-compat-vs-query-reach directive

- [ ] Review complete
- Author: birdayz
- Status: merged yesterday
- Task status: 1 task done
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #211 feat: FULL OUTER JOIN as a Go-only query extension (RFC-036)

- [ ] Review complete
- Author: birdayz
- Status: merged yesterday
- Task status: 2 tasks done
- Metadata count: 5
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #210 feat: DML executes through Cascades (P0.4, RFC-035)

- [ ] Review complete
- Author: birdayz
- Status: merged yesterday
- Task status: 4 tasks done
- Metadata count: 11
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #209 feat: planning-metrics hook for operational debuggability (P2.2)

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 4 tasks done
- Metadata count: 5
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #208 perf: O(1) plan cache LRU via container/list (P2.1)

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 5 tasks done
- Metadata count: 6
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #207 fix: QOV-based FieldValue migration - delete stripAlias* (P1.2)

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 4 tasks done
- Metadata count: 4
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #206 fix: statistics pipeline - read-only transactions + drop intermingled fallback (P1.1)

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 5 tasks done
- Metadata count: 5
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #205 feat: context cancellation in executor cursors (P0.3)

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 6 tasks done
- Metadata count: 4
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #204 fix: plan cache keys on normalized SQL string (P0.2)

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 5 tasks done
- Metadata count: 5
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #203 feat: cap unbounded materialization in executor paths

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 3 of 4 tasks
- Metadata count: 6
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:

## #202 feat: widen correlated scalar subquery shapes

- [ ] Review complete
- Author: birdayz
- Status: merged 2 days ago
- Task status: 6 tasks done
- Review link/id:

### Findings

- Finding 1:
- Finding 2:
- Test gaps:
