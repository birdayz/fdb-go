# RFC-229: a column states its own name

**Status:** DRAFT (rev 2 — folds the Cascades lap: a fifth namer, identity participation, duplicate-name policy, and a separable step 0)
**Scope:** output-column naming across the projection, group-by and sort-key authorities.
**Retires:** 10 of the 12 `contract` entries on the RFC-197 debt list, plus the read-side entries that depend on them.
**Relates to:** RFC-197 (the migration), RFC-226 (a projection states the row it produces), RFC-227 (the hidden sort column is named by the path it reads), RFC-228 (a leg column lookup declines on an ambiguous name).

## 0. What the bucket is actually waiting on

The `contract` bucket has been read as blocked on "per-slot leg provenance". It is not. Exactly **one** of its twelve entries says that, and its sibling states the opposite dependency direction. The other ten are waiting on one thing, and it is not a layout capability at all:

> **an output column's name is DERIVED, over and over, by every site that needs it.**

`ProjectionColumnName`, `AggregateKeyColumnName`, `aggregateGroupKeyOutputName` and `sortKeyFieldRef` each re-render a name from a `Value`. Their readers then re-derive the same name and look each other up by the result. Nothing stores it; the two sides agree by convention, and the convention is that both call the same renderer with the same argument on the same side of the bake.

That is the whole defect class. A name that is re-derived can be derived *differently* — and has been, three times now:

- **RFC-227**: the hidden sort column was named by its rendering rather than its path, so two `ORDER BY` keys under one struct root collapsed. Silently wrong row order.
- **This RFC's §3**: `AggregateKeyColumnName` still renders the flat struct root, so `GROUP BY n.sk, n.co` would collapse two grouping columns into one. Unreachable today only because nested-path `GROUP BY` is refused with 42703 — pinned as a tripwire, not fixed, because the fix is this RFC.
- **`ProjectionColumnName`** returns `fv.Field` for a `FieldValue` but `ToUpper(ExplainValue(v))` for anything else, and `ExplainValue` renders ordinals. So a computed projection over a *baked* field renders `(N#0 + 1)` where the same expression pre-bake renders `(N + 1)`. Writers and readers agree only if they derive on the same side of the bake. Today that is masked because the first namer stores an alias and `OutputColumnName` short-circuits on it — but that mask *is* the convention being replaced, and it is load-bearing.

Three instances, three different mechanisms, one cause. The cause is that the name is a function call rather than a fact.

## 1. Java

Java does not have this class, and the reason is structural rather than careful. A column carries its name:

```java
Column.of(Optional<String> name, Value value) → Field.of(...)
```

and every reader takes it back with a getter. No site re-derives a name from a `Value`, so there is no second derivation to disagree with the first.

Java also answers the question this RFC has to answer twice, because it treats the two populations differently:

- a **projected** column carries a name, because that name is user-visible output;
- a **group-by key** column carries `Column.unnamedOf` — no name at all, positional `_0`.

## 2. The design

**A column states its own name at construction, and every reader binds by slot.**

Three parts, and the third is the one that makes it a fix rather than a refactor.

### 2.1 The name is stored on the column

A name slot on the projected column, carried through every copy, rebuild and rebase — the same preserve-on-copy contract `Resolved` already imposes. A column that survives a rebase with its ordinal intact and its name dropped is the bug in a new place, so the contract has to be the same contract, enforced the same way.

### 2.2 Each naming authority becomes a getter

**Five** authorities, not four. Rev 1 listed `ProjectionColumnName`, `AggregateKeyColumnName`, `aggregateGroupKeyOutputName` and `sortKeyFieldRef` and missed `cascades_generator.go`'s mirror, whose own comment admits to "three mirrors that must agree" — and which is the one that reaches the user, feeding `ColumnDef.Name` and from there `paginatingRows.Columns()`. An RFC about a name being derived in too many places that itself missed a place was making the point the hard way.

All five stop rendering and start reading. There is then exactly one minting point per authority, at construction, and one way to read it.

### 2.2.1 Whether the stored name participates in identity

It does not, and this has to be stated rather than left to whatever the first implementation does.

Java splits it: `Column.planHash` deliberately excludes auto-generated names, while `RecordConstructorValue.equalsWithoutChildren` compares fields. Go has `ProjectionOutputIdentityKey` for one half and nothing for group-by, so the question is currently unanswered rather than answered differently.

The hazard is concrete. A stored name that silently enters `EqualsWithoutChildren` or `SemanticHashCode` splits alias-variant plans in the memo: `SELECT k AS a` and `SELECT k AS b` produce structurally identical plans that stop interning, and the cost is paid as duplicated search, invisibly. §6 pins both directions.

### 2.2.2 Duplicate stored names

A projection may legitimately produce two columns of one name — `SELECT a.k, b.k FROM …`. Java's answer is to drop to `Optional.empty()` on collision rather than store an ambiguous name. Go does the same: the collision produces no stored name, and readers fall to the positional identity they already carry. Storing the name on both would recreate, in data, exactly the ambiguity RFC-228 deleted the first-match lookup to avoid.

### 2.2.3 The EXPLAIN entries retire as a side effect

Once no output-naming authority calls `explainValueOrdinals`, its only remaining caller is display — and the debt entries that exist because a *renderer* was also a *namer* have nothing left to describe. They are not converted; they stop being true.

### 2.3 What gets stored is the RESOLVED PATH, never the flat root

This is the part that decides whether the RFC fixes anything.

A fused nested reference is ONE `FieldValue{Field:"N", Resolved:[N,SK]}`. `Field` is the struct root and is *not* the column's identity — `n.sk` and `n.co` share it. Storing `Field` at construction would freeze the collapse into data, where it is harder to see and impossible to catch by reading the renderer. What gets stored is what `values.ColumnNameValue` renders: the path.

RFC-227 already made this change on the sort-key side. §3 is the same change on the group-key side. Doing 2.1 and 2.2 without 2.3 would ship a well-organized version of the defect.

## 3. Two policies, one mechanism

The ten entries split into two populations that want opposite things, and this is the trap in treating them as one job.

| | population | what the name is | policy |
|---|---|---|---|
| KEEP | projection columns, EXPLAIN output, sort-key labels | user-visible output text | store it, per §2.3 |
| ELIMINATE | group-by key columns | an internal convention Go invented | drop it — Java's `Column.unnamedOf`, positional |

Group-key output names are not a feature. Java's group keys are unnamed; Go's naming of them is the convention that produced the collapse in the first place. Storing a name there would port a Go-only convention *as data*, which is strictly worse than the status quo: today the convention is visible in a renderer that can be read and questioned, and afterwards it would be a field nobody has a reason to look at.

So group keys go positional, and the collapse becomes unrepresentable rather than merely fixed. `GROUP BY n.sk, n.co` produces two key columns because it produces two *slots*, regardless of what either would have been called.

### 3.1 "Unnamed" does not mean nameless downstream

Rev 1 said "no name at all" and stopped there, which is not what Java does and would have broken the result set. Java's group-by columns are constructed `unnamedOf`, and then `RecordConstructorValue.resolveColumns` normalizes through `Type.Record.normalizeFields`, which mints `"_" + i`. EXPLAIN prints `AS _0`. The name is *positional and derived from the slot*, which is the property that matters — it cannot collapse, because two slots cannot share an index — rather than absent.

### 3.2 Where the result-set name lands

This is the gap rev 1 left, and it is the one that would have shipped a regression. Of the four surfaces that could consume a group-key name, three are already clean: EXPLAIN goes through `ExplainValue` rather than the key namer, continuations pack key *data*, and the plan hash folds Values only and explicitly excludes the operand name. The fourth is not: **SQL result-set metadata does consume a group-key-derived name**, through the `cascades_generator.go` mirror of §2.2.

Java puts that name at the relational **expression** layer, not on the column — `Expressions`/`LogicalOperator` name the output of the operator, while the column underneath stays positional. Go does the same: the result-set column name is stated by the relational output expression, and the group-by column below it stays `_i`. That keeps the user-visible name exactly where a user-visible name belongs and keeps it out of the structure the planner reasons about.

## 3.3 The defect surface is narrower than §0 implies

Rev 1 said readers "re-derive the same name and look each other up by the result", generally. Measured, that is true at **three** sites: two last-wins string maps in the translator and one in `logical_predicate.go`. The other readers already carry an authoritative ordinal and use the name only as a label.

That does not weaken the fix — a label that can be derived two ways is still how `(N#0 + 1)` happens — but it changes what to build first and what the tests must target. Those three maps are the only load-bearing group-key name consumers in the engine.

## 4. Sequencing, and the tripwire that enforces it

**Step 0, and it lands as its own commit before anything in §2 or §3:** re-key the three maps of §3.3 by ordinal. It is separable, it is the whole load-bearing surface, and it can be verified on its own — which means the large change that follows is a refactor over an already-correct base rather than a refactor that is also a fix. If step 0 moves a single plan or a single row, that is a defect found early and cheaply instead of inside a change touching five authorities.


Nested-path `GROUP BY` is refused with 42703 today, which is the only reason the collapse is latent. `groupby_nested_key_collapse_fdb_test.go` pins that refusal and says in its failure message what must be converted first.

The order is therefore fixed and the tripwire enforces it: **§2.3 and §3 land before nested-path `GROUP BY` is implemented.** Implementing the feature first arms the collapse, and the symptom — missing groups — is one no existing test would catch. The tripwire fails loudly the moment the feature lands without the conversion, which is what makes this a sequencing statement rather than a hope.

## 5. What this does not retire

Two `contract` entries are outside this work entirely and retire on their own TODO items: `assertSuffixStep` (waiting on merged-layout struct typing, which makes its assert arm *live* rather than unblocking it) and `(FieldPath).ReAnchorRootInto` (waiting on per-slot leg provenance). Their closure conditions are genuinely different from each other and from these ten, and collapsing all three phrasings into one capability is how the bucket came to look like a single blocked item.

`ReAnchorRootInto`'s entry also carries a dead counterfactual — "RecordType.FieldIndex would have first-matched" — about a function RFC-228 deleted. The same dead claim is in production source at `values/values.go`. Both get corrected with this change: the decline is still real, but its justification is now that `FieldIndexUnique` is the only name resolver there is.

## 6. Tests

- The every-arm unit pin on the stored name: minted, copied, rebuilt, rebased — a column that loses its name across any of the four fails, because that is the contract §2.1 claims.
- A nested-path projection asserting the stored name is the PATH and not the root, which is the §2.3 claim stated as a test rather than as a comment.
- The pre-bake/post-bake rendering asymmetry from §0 pinned directly, so the mask that currently hides it (`OutputColumnName` short-circuiting on an alias) stops being load-bearing without anyone noticing.
- Group keys: an assertion that the key columns are positional and carry no name, so a future change that reintroduces group-key naming fails rather than passing quietly.
- `groupby_nested_key_collapse_fdb_test.go` flips from asserting the 42703 refusal to asserting 4 groups for `GROUP BY n.sk, n.co` and 2 for each single-key query — when, and only when, nested-path `GROUP BY` lands.
- **A name-only difference must not split the memo.** `SELECT k AS a` and `SELECT k AS b` produce structurally identical plans; if the stored name reaches `EqualsWithoutChildren` or `SemanticHashCode` they stop interning and the cost is duplicated search that no assertion currently notices. Pin both directions — the name is compared where Java compares it, and excluded from the plan hash where Java excludes it.
- **The duplicate-name policy of §2.2.2**, driven from a test rather than left to the first collision in production: two projected columns of one name produce NO stored name on either, and readers fall through to the positional identity.
- **Step 0 in isolation**: the three re-keyed maps, with the golden and the row counts unmoved. A green here before the rest begins is what makes the later change a refactor rather than a refactor-plus-fix.
