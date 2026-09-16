---
name: query-engine
description: Work on the Cascades query planner/optimizer and SQL execution engine. Includes mandatory architectural review by Graefe (Cascades alignment) and Torvalds (code quality).
---

# Query Engine Work

You are working on the Cascades-based query planner, optimizer, and SQL execution engine. This is the most architecturally sensitive part of the codebase. Every change must be reviewed by two virtual reviewers before shipping.

## The two reviewers

### Goetz Graefe (Cascades paper author)
- Evaluates **Cascades framework alignment**: logical/physical separation, task stack architecture, correlation tracking, property-driven winner selection, phase boundaries
- His word is final on architectural decisions
- Key principle: "properties derived from the expression tree, not imperative flags"
- Key principle: "Java is the reference implementation — Go-only inventions are suspect"
- Will catch: rules in wrong phase, structural heuristics where algebraic checks belong, divergence from Java's match-then-implement pattern

### Linus Torvalds (code reviewer)
- Evaluates **code quality**: dead code, logic holes, incomplete conversions, papered-over regressions, test assertions downgraded to TODOs
- Blunt and specific — file:line references
- Will catch: dead code masking regressions, symptoms treated instead of root causes, dual mechanisms for the same concept, performance regressions hidden by bumping limits

## Workflow

### 1. Read Java first

Before writing ANY planner/optimizer code, read the corresponding Java source in `fdb-record-layer/`. Understand the algorithm, the class structure, the data flow. Then port.

```bash
find fdb-record-layer/ -name "*.java" | xargs grep -l "ClassName"
```

### 2. Implement with tests

One logical change at a time. Write the test BEFORE assuming the implementation is correct. Run `just test` after every change. Commit on green.

For planner changes, run determinism checks on affected tests:
```bash
for i in $(seq 1 10); do
  echo -n "Run $i: "
  bazelisk test //pkg/relational/sqldriver:sqldriver_test \
    --test_output=streamed --test_arg="--test.run=TestName$" \
    --test_arg="--test.v" --nocache_test_results 2>&1 | grep "PASS\|FAIL" | head -1
done
```

Non-deterministic test results mean the planner produces different plans across runs. This is ALWAYS a bug — investigate, don't paper over.

### 3. Review cycle (MANDATORY, milestone-level)

The review unit is a COMPLETED workstream/phase (or the whole RFC
implementation when it isn't phased) — NOT a commit. Implement the whole
milestone with green tests per commit, then launch BOTH reviewers once in
parallel. Do not run reviewer laps on intermediate commits (owner ruling
2026-07-18: "not per commit"). After folding findings, the final HEAD gets
one delta re-confirmation, not another full lap per fix commit.

Use **`gpt-6-astra` with `xhigh` reasoning** for both virtual reviewers and the
independent Codex review. Do not silently substitute another model. Run each
review in its own read-only session; launch the two commands below in parallel
through the tracked task runner (not detached shell jobs). Record the reviewed
HEAD and the actual verdict; an incomplete review is not an ACK.

```sh
codex -a never exec -m gpt-6-astra -c 'model_reasoning_effort="xhigh"' -s read-only \
  'Apply the virtual Goetz Graefe review lens. Review the completed milestone diff in /home/birdy/projects/fdb-record-layer-go. [Specify exact base/HEAD and describe what changed and why.] Read the full diff and corresponding Java source. Evaluate Cascades alignment. Do not edit files. Return ACK or NAK with file:line evidence and the reviewed SHA, under 300 words.'

codex -a never exec -m gpt-6-astra -c 'model_reasoning_effort="xhigh"' -s read-only \
  'Apply the virtual Linus Torvalds review lens. Review the completed milestone diff in /home/birdy/projects/fdb-record-layer-go. [Specify exact base/HEAD and describe what changed.] Read the full diff. Focus on dead code, logic holes, incomplete conversions, and papered-over regressions. Do not edit files. Return ACK or NAK with file:line evidence and the reviewed SHA, under 300 words.'
```

### 4. Address findings

- **Graefe NAK**: architectural issue. Think hard. Read Java again. The fix is usually "do what Java does" or "track the property algebraically, not structurally."
- **Torvalds NAK**: code quality issue. Usually concrete — delete dead code, fix the root cause instead of the symptom, complete the conversion.
- **Both ACK**: ship it.

Do NOT ship with a NAK from either reviewer. Iterate until both approve.

### 5. For full-PR reviews

When reviewing the entire PR (not just the latest commit), use `gh pr diff <number>`:

Use the same `gpt-6-astra` / `xhigh` read-only sessions above, with the PR number
and exact base/HEAD in each prompt:

```text
Run gh pr diff <number> to read the ENTIRE PR diff, not only the latest commit.
If GitHub refuses an oversized diff, read git diff <PR-base>...<PR-head> in full
instead. Report any incomplete scope rather than approving a sampled diff.
```

These catch systemic issues (dead code accumulation, MaxTasks creep, test assertion downgrades) that per-commit reviews miss.

## Lessons learned

These are hard-won patterns from RFC-005 and the unified planning phase work.

### Non-deterministic tests are alias bugs
When a test passes sometimes and fails sometimes with the same plan shape, the root cause is almost always **alias qualification mismatch**. The inner NLJ winner from a reference may use quantifier aliases (`q$N`) while predicates use table aliases (`R`, `P`). `mergeRows` produces unqualified keys, downstream predicates can't resolve qualified references. Trace the alias flow, don't add retry logic.

### REWRITING pruning destroys PLANNING alternatives
`AdvancePlannerStage` promotes exactly ONE winner from REWRITING as the PLANNING seed. Any logical alternative that only exists during REWRITING and isn't the winner is gone. If PLANNING needs it, either the rule must fire during PLANNING too, or the alternative must be re-derivable from the winner.

### Go-only rules are suspect
If a rule exists in Go but not in Java, it's probably wrong. `IndexIntersectionRule` was a Go-only logical rewrite that generated intersection alternatives combinatorially — Java uses a completely different mechanism (match-then-implement during PLANNING). Go-only rules that work in isolation often break when the phase architecture changes.

### Structural heuristics hide real bugs
`GetChildren() > 0` as a proxy for "inner is correlated" worked but hid the real bug (alias namespace mismatch in `PartitionBinarySelectRule`). When you find yourself writing a structural guard, ask: "what property am I actually checking, and why can't I check it directly?" If the answer is "because the infrastructure doesn't track it," fix the infrastructure.

### Physical wrappers must propagate correlation
`GetCorrelatedToWithoutChildren()` on physical wrappers must walk the plan's predicates (NLJ) or resultValue (FlatMap). Returning empty breaks `referenceIsCorrelatedTo()` and any rule that uses correlation sets for classification. Java's physical plans implement `getCorrelatedTo()` properly.

### Two alias namespaces cause silent bugs
Quantifier aliases (`q$N`) vs table aliases (`R`, `P`) is the #1 source of silent predicate misclassification. `GetCorrelatedToOfPredicate()` returns table aliases from QOV nodes. `GetAlias()` returns quantifier aliases. Any rule comparing them gets zero matches. The `rightAliasSet` workaround in `PartitionBinarySelectRule` is the current band-aid. Real fix: unify namespaces at quantifier creation (TODO 7.1).

## Key files

| File | What |
|------|------|
| `cascades/planner.go` | Task stack driver, Plan() entry point |
| `cascades/unified_tasks.go` | Task types: Explore, Transform, Optimize |
| `cascades/winner_lookup.go` | Per-ordering winner selection |
| `cascades/rule_implement_nested_loop_join.go` | NLJ/FlatMap join implementation |
| `cascades/rule_partition_binary_select.go` | Predicate partitioning for binary joins |
| `cascades/default_rules.go` | Rule registration (REWRITING vs PLANNING) |
| `cascades/physical_*_wrapper.go` | Physical plan wrappers (correlation propagation) |
| `cascades/planning_cost_model.go` | Cost comparison for plan selection |
| `embedded/cascades_generator.go` | SQL→plan, column derivation, execution |
| `executor/flat_map_cursor.go` | FlatMap execution (EXISTS/NOT EXISTS) |

## Current tech debt (TODO.md Phase 7)

| # | Item | Priority |
|---|------|----------|
| 7.1 | Unify alias namespaces (quantifier = table) | HIGH |
| 7.2 | Port matching infrastructure (MatchLeafRule etc.) | HIGH |
| 7.3 | Convert remaining predicateReferencesAlias sites | MEDIUM (blocked on 7.1) |
| 7.4 | FlatMap wrapper correlation propagation | LOW |
