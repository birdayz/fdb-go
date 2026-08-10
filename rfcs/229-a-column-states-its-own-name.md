# RFC-229: a column states its own name

**Status:** DRAFT (rev 4 — three more of this RFC's own claims measured and refuted: the memo-identity premise, step 0, and the §2.3 renderer)
**Scope:** output-column naming across the projection, group-by and sort-key authorities.
**Retires:** part of the `contract` bucket. Rev 1 claimed 10 of 12 and that number was wrong — see §6.
**Relates to:** RFC-197 (the migration), RFC-226 (a projection states the row it produces), RFC-227 (the hidden sort column is named by the path it reads), RFC-228 (a leg column lookup declines on an ambiguous name).

## 0. What the bucket is actually waiting on

The `contract` bucket has been read as blocked on "per-slot leg provenance". It is not. Exactly **one** of its twelve entries says that, and its sibling states the opposite dependency direction. The other ten are waiting on one thing, and it is not a layout capability at all:

> **an output column's name is DERIVED, over and over, by every site that needs it.**

`ProjectionColumnName`, `AggregateKeyColumnName`, `aggregateGroupKeyOutputName` and `sortKeyFieldRef` each re-render a name from a `Value`. Their readers then re-derive the same name and look each other up by the result. Nothing stores it; the two sides agree by convention, and the convention is that both call the same renderer with the same argument on the same side of the bake.

That is the whole defect class. A name that is re-derived can be derived *differently* — and has been, three times now:

- **RFC-227**: the hidden sort column was named by its rendering rather than its path, so two `ORDER BY` keys under one struct root collapsed. Silently wrong row order.
- **`AggregateKeyColumnName`** still renders the flat struct root, so `GROUP BY n.sk, n.co` would collapse two grouping columns into one. Unreachable today only because nested-path `GROUP BY` is refused with 42703 — pinned as a tripwire, and NOT fixed here; see §6.
- **`ProjectionColumnName`** returns `fv.Field` for a `FieldValue` but `ToUpper(ExplainValue(v))` for anything else, and `ExplainValue` renders ordinals. So a computed projection over a *baked* field renders `(N#0 + 1)` where the same expression pre-bake renders `(N + 1)`. Writers and readers agree only if they derive on the same side of the bake. The asymmetry is real; **rev 1 named the wrong mask for it** — the short-circuit that hides it is `isPlainColumnRef` in the result-set layer, not an alias check in `OutputColumnName`. That correction matters because §7's test would otherwise have pinned a mask that does not exist and reported green.

Three instances, three different mechanisms, one cause. The cause is that the name is a function call rather than a fact.

## 1. Java

Java does not have this class, and the reason is structural rather than careful. A column carries its name:

```java
Column.of(Optional<String> name, Value value) → Field.of(...)
```

and every reader takes it back with a getter. No site re-derives a name from a `Value`, so there is no second derivation to disagree with the first.

Java also answers the question this RFC has to answer twice, because it treats the two populations differently:

- a **projected** column carries a name, because that name is user-visible output;
- a **group-by key** column carries `Column.unnamedOf`. Rev 1 read that as "no name at all"; it is not — `RecordConstructorValue.resolveColumns` normalizes through `normalizeFields`, which mints `"_" + i`, and EXPLAIN prints `AS _0`. The property that matters is that the name is POSITIONAL, so two slots cannot share it, not that it is absent.

## 2. The design

**A column states its own name at construction, and every reader binds by slot.**

Three parts, and the third is the one that makes it a fix rather than a refactor.

### 2.1 The name is stored on the column

A name slot on the projected column, carried through every copy, rebuild and rebase — the same preserve-on-copy contract `Resolved` already imposes. A column that survives a rebase with its ordinal intact and its name dropped is the bug in a new place, so the contract has to be the same contract, enforced the same way.

### 2.2 Each naming authority becomes a getter

**Five** authorities, not four. Rev 1 listed `ProjectionColumnName`, `AggregateKeyColumnName`, `aggregateGroupKeyOutputName` and `sortKeyFieldRef` and missed `cascades_generator.go`'s mirror, whose own comment admits to "three mirrors that must agree" — and which is the one that reaches the user, feeding `ColumnDef.Name` and from there `paginatingRows.Columns()`. An RFC about a name being derived in too many places that itself missed a place was making the point the hard way.

All five stop rendering and start reading. There is then exactly one minting point per authority, at construction, and one way to read it.

### 2.2.1 The stored name DOES participate in identity, and that is already true

**Rev 3 said the opposite and it was measurably false.** It claimed the question was "currently unanswered" and that a name entering identity would split alias-variant plans in the memo as a new hazard. Measured on master:

```
alias-only:        EqualsWithoutChildren=false  hashEqual=false
no-alias control:  EqualsWithoutChildren=true   hashEqual=true
```

`SELECT k AS a` and `SELECT k AS b` already do not intern. That is deliberate and documented: `ProjectionOutputIdentityKey` is folded into `EqualsWithoutChildren` and `HashCodeWithoutChildren` on the logical projection and into `structuralKey` on the physical one, the comment states *"Output names belong in memo identity"*, and the adjacent text explicitly forecloses the other direction — *"Do not answer this by folding aliasMinted into identity — that trades a wrong label for a duplicated memo group."*

It also matches Java, which is what rev 3's own closing sentence asked for while its headline asked for the reverse: `RecordConstructorValue.equalsWithoutChildren` compares fields. Go compares them too.

So there is nothing to decide and nothing to pin. **The alias stays in projection memo identity.** A stored name inherits that treatment; an implementation that removed it would be a query-engine behaviour change reversing two prior RFCs, which §2 neither proposes nor justifies.

The plan-hash half has no Go analogue to state: the query cache key is SQL text plus planner options, so there is no `Column.planHash` equivalent for an auto-generated name to be excluded from.

### 2.2.2 Duplicate stored names

A projection may legitimately produce two columns of one name — `SELECT a.k, b.k FROM …`. Java's answer is to drop to `Optional.empty()` on collision rather than store an ambiguous name. Go does the same: the collision produces no stored name, and readers fall to the positional identity they already carry. Storing the name on both would recreate, in data, exactly the ambiguity RFC-228 deleted the first-match lookup to avoid.

### 2.2.3 The EXPLAIN entries retire as a side effect

Once no output-naming authority calls `explainValueOrdinals`, its only remaining caller is display — and the debt entries that exist because a *renderer* was also a *namer* have nothing left to describe. They are not converted; they stop being true.

### 2.3 What gets stored is the RESOLVED PATH, never the flat root

This is the part that decides whether the RFC fixes anything.

A fused nested reference is ONE `FieldValue{Field:"N", Resolved:[N,SK]}`. `Field` is the struct root and is *not* the column's identity — `n.sk` and `n.co` share it. Measured:

```
ProjectionColumnName  n.sk -> "N"      n.co -> "N"      COLLAPSE
ColumnNameValue       n.sk -> "N.SK"   n.co -> "N.CO"   distinct
```

Storing `Field` at construction would freeze that collapse into data, where it is harder to see and impossible to catch by reading the renderer.

**But "store what `ColumnNameValue` renders" is too broad, and rev 3's wording was a trap.** Switching every column to it wholesale regresses the correlation-qualified case, which a prior fix documents as having previously served NULL. The template is RFC-227's, at `sortKeyExtraColumnName`: a **nested** reference — one with a multi-accessor `Resolved` — takes the path; everything else keeps its current rendering. That is the narrow change that closes the collapse without touching cases that are already right.

RFC-227 already made this change on the sort-key side. Doing 2.1 and 2.2 without 2.3 would ship a well-organized version of the defect.

One consequence to hold: `values.ColumnNameValue`, the renderer 2.3 mints from, is **itself** a site the RFC-197 detector flags. So the columns this RFC converts do not all leave the debt list — they move from "the name is re-derived at read time" to "the name was minted once from a flagged renderer". That is a real improvement and it is not a retirement, which is most of why §6's count is what it is.

## 3. The defect surface is narrower than §0 implies

Rev 1 said readers "re-derive the same name and look each other up by the result", generally. Rev 3 narrowed that to **three** sites and called them "the only load-bearing group-key name consumers in the engine". **That exhaustiveness claim is also false** — at least four more exist, and one is load-bearing on plan shape: `rule_push_requested_ordering_through_groupby.go` keys a last-wins map by `AccessorNamePathKey`, and its own comment names the cost of a collision as losing an index scan for an in-memory sort.

What survives is the direction, not the enumeration: a label that can be derived two ways is how `(N#0 + 1)` happens, and the consumers that match by that label are where it bites. An implementation must find them rather than work from a list this RFC has now been wrong about twice.

## 4. Sequencing, and the tripwire that enforces it

**Step 0 is STRUCK. Rev 3 specified it as "re-key the three maps of §3 by ordinal", and it is not implementable as worded — nor is it needed, because the substance already landed.** Measured:

- the first two maps hold the ordinal at their **write** site (the loop counter) and at **no read** site: reads are SELECT/HAVING/ORDER BY references carrying no output-relative ordinal, and the references that *do* carry one return early before touching the map. An ordinal-keyed map has no lookup argument;
- the third is **already ordinal-valued**, and is keyed on raw SQL source text rather than on a rendered `Value`, so it was never an instance of §0's defect class at all;
- what *was* implementable — an ordinal channel consulted beside the name map — is substantially done, including by the commit this revision is based on.

The accurate statement of the existing contract is **the ordinal channel overrides the name map**: the name gates *whether* a reference is an output column, structure decides *which*. That is what the code does. §2 therefore builds on it directly; there is no preparatory commit to land first, and rev 3's claim that step 0 is what makes §2 "a refactor over an already-correct base" was describing work that had already happened.


Nested-path `GROUP BY` is refused with 42703 today, which is the only reason the collapse is latent. `groupby_nested_key_collapse_fdb_test.go` pins that refusal and says in its failure message what must be converted first.

The order is therefore fixed and the tripwire enforces it: **`AggregateKeyColumnName` is converted to the resolved path before nested-path `GROUP BY` is implemented.** Implementing the feature first arms the collapse, and the symptom — missing groups — is one no existing test would catch. The tripwire fails loudly the moment the feature lands without the conversion, which is what makes this a sequencing statement rather than a hope.

## 5. What this does not retire

Two `contract` entries are outside this work entirely and retire on their own TODO items: `assertSuffixStep` (waiting on merged-layout struct typing, which makes its assert arm *live* rather than unblocking it) and `(FieldPath).ReAnchorRootInto` (waiting on per-slot leg provenance). Their closure conditions are genuinely different from each other and from the rest, and collapsing all three phrasings into one capability is how the bucket came to look like a single blocked item.

`ReAnchorRootInto`'s entry also carries a dead counterfactual — "RecordType.FieldIndex would have first-matched" — about a function RFC-228 deleted. The same dead claim is in production source at `values/values.go`. Both get corrected with this change: the decline is still real, but its justification is now that `FieldIndexUnique` is the only name resolver there is.

## 6. What rev 3 cut, and the count that was wrong

Rev 1 and rev 2 carried a §3 proposing that group-key columns go positional, Java-style, as a side effect of the naming refactor. **It is cut**, and the reason is not scope discipline — it was specified wrong and would have shipped a silent wrong answer on the most common `GROUP BY` in the language.

With keys named `_0`, `keyOrds` becomes `{"_0":0}` and `groupByOutputBaker`'s lookup of the SELECT-list spelling **misses**. The reference stays lazy and reads `"STATUS"` off a row the executor keyed `"_0"` — a NULL column, no error. That is `SELECT status, COUNT(*) FROM t GROUP BY status`. On the exact-aggregate path the label degrades to a literal `_0`, and the naming golden has no unaliased-group-key case, so the regression would have landed **unpinned**.

It is not only wrong answers. `rule_push_requested_ordering_through_groupby` matches the requested ordering against grouping keys by name; an output-row spelling of `_0` against an input-keyed map never matches, the rule pushes nothing, and that file's own comment names the consequence — `SELECT customer_id, COUNT(*) … GROUP BY customer_id ORDER BY customer_id` loses its index scan for an in-memory sort. Roughly 326 `plan_shape.golden` lines move, plus four yamsql EXPLAIN assertions. And `OrdinalDomainOfColumnNames` is length-prefixed over the names, so spelling every key `_0.._n` collapses structurally different aggregates of equal arity into one domain token, making the cross-layout `OrdinalIn` guard **vacuous** — with nothing failing to say so.

Two things survive the cut. Continuations are clean: they serialize the evaluated key tuple and index-parallel accumulator state, no column names, so Java wire compat was never at stake — rev 1 should have said that and didn't. And the elimination is not blocked on a capability that does not exist: the structural ordinal channel already works, which is what drained the aggregate arm from 1014 hits to 1. What keeps the name arm alive is the flat dotted `FieldValue` whose `Field` is literally `"A.ID"` — so this is the **dotted MINT** conversion plus CQ-55, exactly as the debt entry says, and it needs its own RFC and the query-engine gate.

**The retirement count.** Rev 1 claimed 10 of 12. That is false, and it was my number rather than a measured one. At least four entries survive the static gate after this change, and §2.3 adds a reason of its own: the stored name is minted from `values.ColumnNameValue`, which the detector flags. A site that mints once from a flagged renderer is better than a site that re-derives at every read, and it is still a flagged site. The honest claim is that this RFC removes a defect *class* and shrinks the bucket; the arithmetic belongs in the implementation PR, measured against the gate, not asserted here.

## 7. Tests

- The every-arm unit pin on the stored name: minted, copied, rebuilt, rebased — a column that loses its name across any of the four fails, because that is the contract §2.1 claims.
- A nested-path projection asserting the stored name is the PATH and not the root, which is the §2.3 claim stated as a test rather than as a comment.
- The pre-bake/post-bake rendering asymmetry from §0 pinned directly — against `isPlainColumnRef` in the result-set layer, which is the mask that actually hides it. Rev 1 named `OutputColumnName`'s alias check instead, so this test would have pinned a mask that does not exist and reported green.
- `groupby_nested_key_collapse_fdb_test.go` flips from asserting the 42703 refusal to asserting 4 groups for `GROUP BY n.sk, n.co` and 2 for each single-key query — when, and only when, nested-path `GROUP BY` lands.
- **A name-only difference DOES split the memo, and that must not change.** Rev 3 asked for the opposite and was wrong about current behaviour (§2.2.1). The pin therefore guards the existing contract: `SELECT k AS a` and `SELECT k AS b` compare unequal and hash unequal, with a no-alias control that compares equal — so an implementation that quietly drops the name out of identity fails here rather than shipping as a silent planner change.
- **The duplicate-name policy of §2.2.2**, driven from a test rather than left to the first collision in production: two projected columns of one name produce NO stored name on either, and readers fall through to the positional identity.
- **Step 0 in isolation**: the three re-keyed maps, with the golden and the row counts unmoved. A green here before the rest begins is what makes the later change a refactor rather than a refactor-plus-fix.
