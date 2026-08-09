# RFC-224 — The one-final invariant asserts Java's mechanism, not Go's property

Status: proposed (implementation gated on Graefe + Torvalds ACK)
Java reference: fdb-record-layer 4.12.11.0 (pinned `MODULE.bazel:117`)

## 1. The defect

`TestOneFinalPlanPerReference` (`pkg/relational/core/embedded/one_final_member_test.go`)
is green across 20 plan shapes and its own comment claims "zero violations across all
2407 yamsql corpus queries and the whole Go test suite". It is asserting a property Go
does not have, and it could not see a violation if one occurred. Both halves are
measured.

**(a) The walk is blind.** `VerifyOneFinalPlanPerReference`
(`cascades/final_member_invariant.go:31-69`) descends only through FINAL members'
quantifiers, so a reference holding an EMPTY final set is simultaneously counted as a
non-violation and used as a walk terminator. Across the 20 subtests: 43 reference
visits, 21 of them dead ends, and for 18 of the 20 queries the root is the only
non-empty-final reference the walk ever reaches. On the `join` shape the verifier
visits 2 references where plan extraction visits 5, and the memo holds three groups
with multiple physical finals and no winner.

**(b) The property is Java's mechanism, not a Go invariant.** Java's
`Reference.pruneWith` clears the member set and inserts one, so a group ends with
exactly one final and `Iterables.getOnlyElement(getFinalExpressions())` is total. Go's
`OptimizeGroupTask` prunes to a KEEP SET (`unified_tasks.go:662-699`) whose own comment
names the reason:

> "Winner-per-(group, properties) retention (Graefe 1995 §2): pruning to the single
> overall cost winner would destroy a costlier-but-ORDERED final that a pushed
> RequestedOrderingConstraint asked this group for … so the group must retain, per
> requested ordering, the cheapest final satisfying it."

The keep set is `{bestFinal} ∪ {cheapest final satisfying each requested ordering}`. A
group holding several physical finals is therefore CORRECT Cascades here — a group
retaining one winner per required physical property is the textbook shape — and the
invariant contradicts the planner rather than describing it.

So making the walk honest without settling (b) would convert a vacuous green into a red
that is also wrong. That is why this is an RFC and not a patch.

## 2. Root cause: two mechanisms for one guarantee

The guarantee both engines need is that **dereferencing a child reference during plan
extraction is unambiguous**. Java gets it by arranging that only one candidate exists.
Go gets it a different way, and already does:

- `OptimizeGroupTask` stamps a designated winner — `t.Ref.SetWinner(bestFinal)`
  (`unified_tasks.go:700`).
- Extraction dereferences THAT, not the member set — `w := ref.Winner()`
  (`plan_extraction.go:269`), taken when `w != nil && isPhysicalPlan(w)`.

Go's mechanism is strictly more expressive: it keeps the ordered alternatives a parent's
`bestSatisfyingMember` lookup needs, which Java does not need because Java bakes concrete
child plans at rule time (`memoizePlan`) while Go wrappers range over child References
resolved at lookup/extraction.

The invariant was written against Java's mechanism and then treated as a precondition on
Go. RFC-183 P5's actual requirement — that the nil-inner shell state be unrepresentable —
is met by the winner stamp, not by member-set cardinality.

## 3. Decision

**Replace the assertion "at most one physical final per Reference" with "every reference
plan extraction dereferences has a designated physical winner", and walk the graph
extraction actually walks.**

Concretely:

1. **`VerifyOneFinalPlanPerReference` is replaced by `VerifyExtractionIsUnambiguous`**,
   which walks the reference graph the way `plan_extraction.go` does — through the
   winner when one is stamped, through the selector's best member otherwise — and
   reports every reference reached where NEITHER resolves to a physical plan. That set
   is exactly the ambiguity P5 cares about, and unlike member cardinality it is a
   property Go intends to hold.

2. **The walk reports its own reach.** It returns visited and dead-end counts, and the
   test asserts dead ends are zero. This is the specific defect that made the current
   green vacuous, so it becomes a checked quantity rather than a property of the
   implementation nobody measured. A gate that cannot distinguish "no violations" from
   "never looked" fails OPEN, which is the direction that ships bugs.

3. **Multi-final groups stop being violations.** They are the keep set doing its job.
   What replaces the cardinality check is a *coherence* check: for every reference with
   more than one physical final, the stamped winner must be a member of that group's
   final set, and every retained non-winner must satisfy some requested **property**
   that was pushed to it. A retained final satisfying no requested property is dead
   weight the prune was supposed to drop, and today nothing would notice it.

   **"Property", not "ordering", and the distinction is load-bearing** — on grounds
   independent of any other RFC. An earlier draft wrote this clause as "some requested
   ORDERING". That hard-codes one property class into an invariant whose whole subject
   is property-keyed retention: the keep set is defined per required physical property,
   so a coherence rule that can only see orderings would report any future
   property-retained member as dead weight and pressure someone into pruning it. Any
   new required property joins this clause rather than being special-cased beside it.

   This wording was first proposed to protect a specific retained member that RFC-220
   was thought to need. That reasoning is **withdrawn** — the diagnosis it rested on
   was refuted by measurement — and the clause is stated here without it, because it
   was never the reason the clause is right.

4. **RFC-183 P5 is unblocked, and that is a finding, not a side effect.** The current
   test's failure message says "P5 is blocked and any plan-holds-a-quantifier work must
   stop". That instruction rests on the wrong property. P5's precondition is the winner
   stamp's totality over the extraction graph, which item 1 is precisely what asserts.

### 3.1 Why not the alternatives

**(A) Repoint the walk, keep the cardinality assertion.** This is the change the current
comment invites, and it turns `join` red on a group with four physical finals — all four
legitimately retained per requested ordering. It would report the keep set as a bug and
pressure someone into "fixing" the planner by pruning harder, which is the exact
regression the Graefe 1995 §2 comment was written to prevent.

**(B) Delete the test.** It is the only executable statement about extraction
unambiguity in the tree, and the property it gropes at is real. Deleting it trades a
wrong assertion for no assertion.

**(C) Leave it, document the limits.** What this change already does in the interim — the
doc comments now say the green proves nothing. That is honest but it is not a gate, and a
documented-as-useless test is a test that will be cited as evidence anyway.

## 4. Coverage

Each pin mutation-checked, one mutation per independent failure direction:

1. **The dead-end count is asserted zero.** Mutation: restore the finals-only descent;
   the count goes to 21 across the 20 shapes and the test fails naming the terminator.
   This is the direction that made the old green vacuous.
2. **A reference with no winner and no physical best member is reported.** Mutation:
   clear a winner stamp on one group; the walk names that reference.
3. **A multi-final group is NOT a violation.** The `join` shape holds four physical
   finals and must pass — a test that goes red here has reintroduced (A).
4. **A retained final satisfying no requested ordering IS a violation** (item 3's
   coherence half). Mutation: add a final to the keep set unconditionally; the test
   names it.
5. **Extraction agreement.** For each of the 20 shapes, the set of references the
   verifier visits equals the set plan extraction visits. This is what makes the walk's
   claim to follow extraction checkable rather than asserted in a comment — on the
   current `join` case those sets are 2 and 5.

The new file is confirmed present in its Bazel target's `srcs` via `just gazelle` plus a
grep of the target output for a line only that test emits.

## 5. Blast radius

`VerifyOneFinalPlanPerReference`, `SetVerifyOneFinal`, `OneFinalViolations` and
`planner.verifyOneFinal` are the whole surface; `planAndVerifyOneFinal`
(`plan_harness_support_test.go`) is the only caller and is test-only. One planner comment
cites the test by name — `plans/plan_expression.go:172`, the sole hit of
`grep -rn TestOneFinalPlanPerReference pkg/` outside the test's own file — and must be
rewritten to cite the winner stamp instead. `cascades/physical_wrapper.go:331` already
states outright that Go does not have the one-final property; it becomes consistent with
the code rather than contradicting a green test.

No plan shape changes, no golden moves, no wire surface: this is an assertion change plus
a walk. `planner.go`'s post-drain comment ("each Reference's FinalMembers has been pruned
to exactly one physical plan by OptimizeGroup") is false today and is corrected.

## 6. Gate

RFC → Graefe + Torvalds ACK **before** implementation → implement → one joint review lap
+ a single codex run → PR → @claude LGTM → delta re-confirmation on the final head →
merge. Never merge on a NAK.

Graefe's reading, recorded because it is the load-bearing judgement here: *"a group
retaining one winner per required physical property is textbook Cascades; Java's single
final is an artifact of `pruneWith`. The invariant is wrong, not the planner."*
