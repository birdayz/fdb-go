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

### 3. Review cycle (MANDATORY)

After implementation passes all tests, launch BOTH reviewers in parallel:

```
Agent(description: "Graefe Cascades review", prompt: "You are Goetz Graefe, author of the Cascades optimization framework paper. Review the diff in /home/birdy/projects/fdb-record-layer-go. Run `git diff HEAD` (or `git diff HEAD~N` for specific commits). [describe what changed and why]. Evaluate Cascades alignment. Under 300 words.", run_in_background: true)

Agent(description: "Torvalds code review", prompt: "You are Linus Torvalds. Review the diff in /home/birdy/projects/fdb-record-layer-go. Run `git diff HEAD`. [describe what changed]. Focus on dead code, logic holes, incomplete conversions, papered-over regressions. Under 300 words.", run_in_background: true)
```

### 4. Address findings

- **Graefe NAK**: architectural issue. Think hard. Read Java again. The fix is usually "do what Java does" or "track the property algebraically, not structurally."
- **Torvalds NAK**: code quality issue. Usually concrete — delete dead code, fix the root cause instead of the symptom, complete the conversion.
- **Both ACK**: ship it.

Do NOT ship with a NAK from either reviewer. Iterate until both approve.

### 5. For full-PR reviews

When reviewing the entire PR (not just the latest commit), use `gh pr diff <number>`:

```
Agent(prompt: "...Run `gh pr diff 200` to read the ENTIRE PR diff. READ THE ENTIRE DIFF...", run_in_background: true)
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
