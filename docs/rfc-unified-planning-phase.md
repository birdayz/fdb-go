# RFC: Unified Task-Stack Planning — BatchA→PLANNING Migration

**Status:** Proposed
**Date:** 2026-05-26
**Supersedes:** rfc-cascades-plan-extraction.md (extraction blocker analysis)
**Reference:** Java `CascadesPlanner.java`, `Reference.java`, `PlanningRuleSet.java`, `PlannerPhase.java`, `PlannerStage.java` (tag 4.11.1.0)

## Expert Panel

Five independent reviewers assessed this RFC unanimously:

- **FDB C++ systems expert:** "Approach A. No contest. Seven failed attempts is not bad luck — it's the architecture telling you it's wrong."
- **FDB Relational expert:** "Java's PREORDER rules interleave with EXPLORATION and IMPLEMENTATION rules on the same task stack, and constraint pushes trigger re-exploration of children mid-phase. The three passes can't capture this."
- **Linus Torvalds:** "The split architecture is fundamentally broken. Cascades is a task-stack algorithm. The task stack IS the architecture."
- **Asmongold:** "If you try to open a door seven times and it doesn't open, maybe it's a wall."
- **Rob Pike:** "You have one optimization algorithm with two phases, and you implemented them using two completely different architectures. That's not Go pragmatism, that's two half-implementations duct-taped together."

## Problem Statement

Go fires BatchA physical implementation rules (PrimaryScanRule, ImplementFilterRule, ImplementIndexScanRule, etc.) during the EXPLORE phase. Physical wrappers land in `members` (exploratory set). The EXPLORE-phase `bestMember` is always physical, so extraction picks it and skips PLANNING-phase `finalMembers`. PLANNING produces better plans (InJoin, multi-predicate index scan, covering index) but they're never selected.

**Performance impact:** 6 queries are 30–300x slower than they should be. On real data this means hitting FDB's 5-second transaction limit on queries that should take <10ms.

**Seven approaches have failed** (adapter, two-phase explore, FinalMembers preference, InsertFinal, advancePlannerStage v1, v2a/b/c, InsertFinal v2). Each broke 5–9 tests in CTE/JOIN/UNION/derived-table queries. Root cause: the explicit bottom-up PLANNING phase can't model the interleaving that Java's task-stack provides.

**Four workaround hacks** paper over the gap:
1. `promoteInJoinWinners` — manually promotes InJoin/InUnion plans post-PLANNING
2. `promoteByDataAccessCost` — manually compares data access cardinality
3. `reoptimizeAll` — reruns cost comparison on all References after PLANNING
4. `FinalizeExpressionsRule` — copies EXPLORE physical wrappers into FinalMembers (creates stale copies)

## Root Cause

### Java's Architecture (one mechanism, two phases)

Java uses **one unified task-stack** for both REWRITING and PLANNING phases. The flow:

```
pushInitialTasks():
    push InitiatePlannerPhase(REWRITING)

InitiatePlannerPhase(REWRITING).execute():
    push InitiatePlannerPhase(PLANNING)   // LIFO: fires AFTER rewriting
    push OptimizeGroup(REWRITING, root)
    push ExploreGroup(REWRITING, root)

// REWRITING runs to convergence...
// OptimizeGroup(REWRITING) prunes finals to single best logical form...

InitiatePlannerPhase(PLANNING).execute():
    push OptimizeGroup(PLANNING, root)
    push ExploreGroup(PLANNING, root)

ExploreGroup(PLANNING, ref).execute():
    if ref.stage != PLANNED:
        ref.advancePlannerStage(PLANNED)   // LAZY, per-Reference
    // Re-explore from promoted seed with PlanningRuleSet
    for each final expression:
        exploreExpressionAndOptimizeInputs(PLANNING, ref, expr)
    for each exploratory expression:
        exploreExpression(PLANNING, ref, expr)

OptimizeGroup(PLANNING, ref).execute():
    // Pick best from getFinalExpressions() using PlanningCostModel
    // Prune losers, keep single winner
```

Key property: `advancePlannerStage` is **lazy** — it fires in `ExploreGroup.execute()` when the Reference's stage doesn't match the target. This means:

```java
void advancePlannerStageUnchecked(PlannerStage newStage) {
    this.plannerStage = newStage;
    constraintsMap.advancePlannerStage();          // Reset constraint watermarks
    this.propertiesMap = newStage.createPropertiesMap();  // Switch to PlanPropertiesMap
    exploratoryMembers.clear();                    // Drop ALL EXPLORE artifacts
    exploratoryMembers.addAll(finalMembers);       // Promote REWRITING winner as seed
    finalMembers.clear();                          // Clean slate for PLANNING finals
}
```

After advancing, the task-stack re-explores from the promoted seed. PlanningRuleSet fires through the **same** ExploreExpression → TransformExpression mechanism:

- **EXPLORATION_RULES** (6): NormalizePredicates, InComparisonToExplode, SplitSelectExtractIndependentQuantifiers, PullUpNullOnEmpty, PartitionSelect, PartitionBinarySelect
- **MATCHING_RULES** (2): MatchLeafRule, MatchIntermediateRule
- **PREORDER_RULES** (17): PushRequestedOrderingThrough*, PushReferencedFieldsThrough*
- **IMPLEMENTATION_RULES** (37): ALL physical implementation including InJoin, Sort elimination, etc.

The critical interleaving property: PREORDER rules push constraints to child References, which triggers re-exploration of those children, which fires IMPLEMENTATION rules on the children, which may produce new physical plans that change the parent's optimal plan. This is a fixpoint **within** the task-stack, not three sequential passes.

### Go's Architecture (two mechanisms, two phases)

Go split the planner into two different control-flow mechanisms:

```go
func (p *Planner) Plan(rootRef *Reference) (RelationalExpression, int, error) {
    // EXPLORE: task-stack (ExploreReferenceTask → TransformReferenceTask → SaturationCheckTask)
    // Fires ALL rules including BatchA physical implementation rules
    tasks, conv := p.Explore(rootRef)

    AdjustMatches(rootRef)

    // PLANNING: explicit 3-pass bottom-up (NOT task-stack)
    p.runPlanningPhase(rootRef)
    //   Pass 1: propagateConstraints (top-down)
    //   Pass 2: generateDataAccessWithConstraints (bottom-up)
    //   Pass 3: implementBottomUp (bottom-up fixpoint)

    // Workarounds for extraction not seeing PLANNING results:
    p.reoptimizeAll(rootRef)
    p.promoteInJoinWinners(rootRef)
    promoteByDataAccessCost(rootRef, p.stats)

    plan, err := ExtractBestPlanFromSelector(rootRef, p, p.stats)
    return plan, tasks, err
}
```

The split creates a structural mismatch:
1. BatchA fires during EXPLORE → physical wrappers go into `members`
2. Extraction sees physical `Winner(NoProperties)` from EXPLORE → skips `FinalMembers`
3. PLANNING produces better plans in `FinalMembers` → never selected
4. The three explicit passes can't model constraint-push-triggers-re-exploration

### Why the 7 approaches failed

Every approach tried to fix one of the four touch points without fixing all of them atomically:

| # | Approach | What it fixed | What it broke |
|---|----------|---------------|---------------|
| 1 | BatchA as ImplementationRules | FinalMembers populated | Wrong ordering in FinalMembers (first-in-order, not cost-best) |
| 2 | Two-phase Explore | BatchA in second pass | Member insertion ordering changes break ties |
| 3 | FinalMembers preference | Extraction consults finals | Stale-child plans from FinalizeExpressionsRule |
| 4 | InsertFinal | PLANNING yields to finals | ImplementSimpleSelectRule yields bare Fetch(nil) with 0 cost |
| 5 | advancePlannerStage v1 | Clears EXPLORE artifacts | PLANNING's explicit passes rely on those artifacts |
| 6 | advancePlannerStage v2 | Various partial clears | CTE/JOIN/UNION execution breaks |
| 7 | InsertFinal v2 | Phase-aware insertion | 1-quantifier SelectExpression persists from EXPLORE |

The common thread: **the explicit bottom-up PLANNING phase depends on EXPLORE-phase artifacts** (physical wrappers in members, PartialMatches, ToPlanPartitions). Clearing those breaks PLANNING. Keeping them blocks extraction. The split is irreconcilable.

## Proposed Solution: Unified Task-Stack

Port Java's unified task-stack architecture. Both REWRITING and PLANNING use the same ExploreGroup → ExploreExpression → TransformExpression → OptimizeGroup task flow.

### Design

#### 1. PlannerPhase enum

```go
type PlannerPhase int

const (
    PhaseRewriting PlannerPhase = iota
    PhasePlanning
)
```

Each phase carries:
- A rule set (RewritingRuleSet or PlanningRuleSet)
- A cost model (RewritingCostModelLess or PlanningCostModelLess)
- A target PlannerStage (Canonical or Planned)

#### 2. PlannerStage on Reference

```go
type PlannerStage int

const (
    StageInitial PlannerStage = iota
    StageCanonical
    StagePlanned
)
```

Every Reference tracks its current stage. `advancePlannerStage` transitions it:

```go
func (r *Reference) AdvancePlannerStage(newStage PlannerStage) {
    r.stage = newStage
    r.members = append(r.members[:0], r.finalMembers...)  // Promote finals as seed
    r.finalMembers = r.finalMembers[:0]                    // Clean slate
    r.planProperties = nil                                 // Reset properties
    r.winners = nil                                        // Clear REWRITING winners
    // PartialMatchMap preserved — data access rules consume it in PLANNING
}
```

#### 3. Phase-aware tasks

Every task carries its `PlannerPhase` and fires the phase-appropriate rule set:

```go
type ExploreReferenceTask struct {
    Ref   *expressions.Reference
    Phase PlannerPhase
}

func (t *ExploreReferenceTask) Run(p *Planner) {
    targetStage := t.Phase.TargetStage()
    refStage := t.Ref.Stage()

    if targetStage != refStage {
        if targetStage.Precedes(refStage) {
            return  // Reference is further along; don't re-explore
        }
        // Reference needs advancement
        t.Ref.AdvancePlannerStage(targetStage)
    }

    // Fire phase-appropriate rules via the same task infrastructure
    rules := p.rulesForPhase(t.Phase)
    // ... push SaturationCheck, TransformReference, ExploreExpression ...
}
```

#### 4. InitiatePlannerPhase task

New task type that transitions between phases:

```go
type InitiatePlannerPhaseTask struct {
    Phase PlannerPhase
}

func (t *InitiatePlannerPhaseTask) Run(p *Planner) {
    if next, ok := t.Phase.Next(); ok {
        p.push(&InitiatePlannerPhaseTask{Phase: next})
    }
    p.push(&OptimizeReferenceTask{Ref: p.rootRef, Phase: t.Phase})
    p.push(&ExploreReferenceTask{Ref: p.rootRef, Phase: t.Phase})
}
```

#### 5. Phase-aware OptimizeGroup

During PLANNING, `OptimizeGroup` picks best from **FinalMembers** only (physical plans), matching Java's behavior of leaving exploratory members untouched:

```go
func (t *OptimizeReferenceTask) Run(p *Planner) {
    if t.Phase == PhasePlanning {
        // Only consider final members (physical plans)
        // Prune losers, keep single best
        best := bestFrom(t.Ref.FinalMembers(), p.costModelFor(t.Phase))
        if best != nil {
            t.Ref.PruneWith(best)  // Keep only best final
        }
    } else {
        // REWRITING: pick best from all members as before
        p.OptimizeGroup(t.Ref, expressions.NoProperties)
    }
}
```

#### 6. Plan() becomes trivial

```go
func (p *Planner) Plan(rootRef *expressions.Reference) (RelationalExpression, int, error) {
    p.rootRef = rootRef
    p.memo = NewMemo(rootRef)

    // One task-stack drives both phases
    p.push(&InitiatePlannerPhaseTask{Phase: PhaseRewriting})

    for len(p.stack) > 0 {
        if p.tasksRun >= p.MaxTasks {
            return nil, p.tasksRun, ErrPlannerCapHit
        }
        task := p.pop()
        task.Run(p)
        p.tasksRun++
    }

    // Extract — no workarounds needed
    plan, err := properties.ExtractBestPlan(rootRef)
    return plan, p.tasksRun, err
}
```

#### 7. Rule categorization (matches Java's PlanningRuleSet)

| Category | Phase | Go equivalent |
|----------|-------|---------------|
| RewritingRuleSet | REWRITING | `DefaultExpressionRules()` + `RewritingRules()` + `MatchingRules()` |
| PlanningRuleSet.EXPLORATION | PLANNING | `PlanningExplorationRules()` (6 rules, already ported) |
| PlanningRuleSet.MATCHING | PLANNING | `MatchingRules()` (MatchLeafRule, MatchIntermediateRule) |
| PlanningRuleSet.PREORDER | PLANNING | Constraint-push + field-push rules (already in `DefaultImplementationRules`) |
| PlanningRuleSet.IMPLEMENTATION | PLANNING | `BatchAExpressionRules()` + remaining implementation rules |

### What gets deleted

1. **`promoteInJoinWinners`** — task-stack PLANNING produces InJoin plans and OptimizeGroup selects them naturally
2. **`promoteByDataAccessCost`** — data access cost is reflected in PlanningCostModel comparison
3. **`reoptimizeAll`** — OptimizeGroup fires per-Reference during PLANNING
4. **`FinalizeExpressionsRule`** — no longer needed; PLANNING produces fresh physical plans from promoted seed
5. **Explicit `runPlanningPhase`** — replaced by task-stack PLANNING
6. **Explicit `propagateConstraints`** — replaced by PREORDER rules firing in ExploreExpression
7. **Explicit `implementBottomUp`** — replaced by IMPLEMENTATION rules firing in TransformExpression
8. **Explicit `generateDataAccessWithConstraints`** — replaced by MATCH_PARTITION rules firing in ExploreExpression (data access rules consume PartialMatches during PLANNING exploration)

### Extraction simplification

After PLANNING's OptimizeGroup prunes each Reference to a single final member, extraction becomes trivial: `ref.Get()` returns the only member. The complex `ExtractBestPlanFromSelector` with its selector/cost-fallback/sort-elimination paths simplifies to walking the pruned DAG.

Java's `getOnlyElementAsPlan()` (Reference.java:237) verifies: `exploratoryMembers.isEmpty()` then returns `finalMembers.getOnlyElement()`. After proper PLANNING, every Reference has been pruned to exactly one final physical plan.

### Invariants

1. **`advancePlannerStage` requires exactly one final member.** Java verifies `finalMembers.size() == 1` (Reference.java:210). REWRITING's OptimizeGroup must prune to exactly one winner before PLANNING begins.

2. **PartialMatches survive stage advancement.** They're stored on the Reference, not in the properties map. Data access rules consume them during PLANNING exploration.

3. **Shared References (CTE, multi-consumer).** `ExploreGroup` has a stage guard: if `targetStage.precedes(refStage)`, skip. A CTE Reference already at PLANNED stage won't be re-explored when a second consumer reaches it.

4. **Continuation tokens unaffected.** Plan selection changes which plan gets built, but the continuation format (cursor state serialization) is plan-type-specific. Same plan type → same continuation format.

5. **Cost model is phase-specific.** REWRITING uses `RewritingCostModelLess` (few criteria, favors canonical forms). PLANNING uses `PlanningCostModelLess` (17 criteria including EstimateCost). Each `OptimizeGroup` uses the phase-appropriate model.

## Execution Plan

### Phase 1: Infrastructure (no behavioral change)

1. Add `PlannerStage` to Reference (field + getter/setter)
2. Add `PlannerPhase` to all Task types (field)
3. Add `rulesForPhase(PlannerPhase) []ExpressionRule` to Planner
4. Add `costModelFor(PlannerPhase)` to Planner
5. Add `InitiatePlannerPhaseTask`
6. **All tests still pass** — REWRITING fires same rules, PLANNING task is pushed but executes with empty rule set (behavioral no-op)

### Phase 2: advancePlannerStage activation

1. Wire `advancePlannerStage` into `ExploreReferenceTask.Run` (stage comparison guard)
2. REWRITING's `OptimizeGroup` now prunes to single final member
3. PLANNING's `ExploreGroup` promotes that member and re-explores with PlanningExplorationRules
4. **Tests may break** — this is the critical point where EXPLORE artifacts are cleared

### Phase 3: Move BatchA to PLANNING

1. Remove BatchA from EXPLORE-phase rule set
2. Add BatchA to PlanningRuleSet's IMPLEMENTATION category
3. PLANNING fires exploration + matching + implementation rules from promoted seed
4. **This is the atomic change** — extraction now sees PLANNING-phase physical plans only

### Phase 4: Delete workarounds

1. Delete `promoteInJoinWinners`, `promoteByDataAccessCost`, `reoptimizeAll`
2. Delete `FinalizeExpressionsRule`
3. Delete explicit `runPlanningPhase`, `propagateConstraints`, `implementBottomUp`, `generateDataAccessWithConstraints`
4. Simplify extraction (no selector path needed)
5. Run stress test 1M comparison

### Risk Assessment

| Risk | Mitigation |
|------|------------|
| Large refactor (~2000-3000 LOC changed) | Phased execution; Phase 1 is behavioral no-op |
| CTE/recursive-CTE edge cases | Stage guard in ExploreGroup handles shared References |
| PREORDER rule interleaving | Task-stack handles this naturally (Java's proven architecture) |
| Test suite breadth | 46 targets, 270 yamsql, 508 cross-engine, 63 planner harness |
| Data access generation timing | PartialMatches survive advancePlannerStage; data access rules fire during PLANNING exploration |

## Expected Outcomes

1. **6 queries fixed:** InJoin(IndexScan), multi-predicate PK scan, covering indexes all selected by cost model
2. **Architecture aligned with Java:** one mechanism, two phases, same task flow
3. **Per-column selectivity activatable:** no longer blocked by EXPLORE/PLANNING phase mismatch
4. **Workaround hacks deleted:** 4 compensatory passes removed, ~300 LOC deleted
5. **Future Java changes become ports, not translations**
