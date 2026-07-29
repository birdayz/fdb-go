# RFC-197: Column identity is an ordinal, not a name

Status: PROPOSED, revision 2 — implementation blocked on joint-review ACK.
Revision 1 was NAK'd twice; this revision folds both reviews. The material
changes: identity gained a third element (the ordinal DOMAIN), the
dotted-probe item was re-rooted off a falsified premise, the migration order
gained a step 0, the allowlist mechanism the plan depends on was rebuilt
per-site (landed, `pkg/docscheck`), and the bucket arithmetic became a
partition pinned by a test instead of prose.

## The problem

Seven wrong proofs in this planner came from one shape: a column's leaf
DISPLAY name used as its identity. `PushValueThroughFetch`,
`correlatedInnerField`, `correlatedFieldOf`, `fieldValueAliasAndCol`,
`buriedLegOrdinalLayout`, `rebaseOuterLegValue`, and the unique-key proof.
Each was found by a different route and none by the test suite. Two columns
with the same leaf name get treated as one, or one column reached by two
paths gets treated as two.

Fixing the seven and stopping guarantees an eighth, so `pkg/docscheck`'s
`TestFieldNameNeverDecides` now fails the build when `.Field` reaches a
decision. That gate makes violations LOUD. It does not make them
IMPOSSIBLE, and 68 sites are grandfathered on its ratchet. (38 when this
revision was first drafted; two rounds of detector widening — same-package
escapes, laundering via concatenation/slicing/nested helpers, then
intra-function taint through local variables — surfaced 30 more, including
the LAST TWO of the original seven bugs, which had been absent from the
inventory the whole time. A count that grows when the instrument improves
is the instrument working.)

This RFC is about closing those 68.

## The rule, and where it comes from

CockroachDB settles this at name-resolution time: `opt.ColumnID` is assigned
once when an identifier is resolved, the optimizer uses only the id, and
`ColumnMeta.Alias` is documented as display-only
(`cockroach/pkg/sql/opt/column_meta.go:17,192-196`). Nothing downstream can
express a name comparison because nothing downstream holds a name.

Java reaches the same place by a different road: `FieldValue.ResolvedAccessor`
is built at construction and its `equals`/`hashCode` are ordinal-only
(`FieldValue.java:~630`), deliberately excluding the name.

**The name is legitimate EXACTLY ONCE, at name resolution. Everywhere
downstream it is display.**

Go already has the machinery. `FieldValue.Resolved` is a `*FieldPath` of
`ResolvedAccessor`s with ordinal-only identity — the port of Java's contract,
already load-bearing and already tested. There is no missing capability here
and no new type to invent. What is missing is that 68 sites still ask the
name a question the path can answer.

## What identity actually is: a triple

Column identity in this engine is **(correlation, domain, ordinal path)**.

- **Correlation**, because ordinal 0 of two different quantifiers are
  different columns. `correlatedInnerField` returning
  `(string, CorrelationIdentifier)` is the pair wrong by one element — the
  whole original bug class in one signature.
- **Domain**, because the same ordinal under the same correlation can index
  two different layouts. `values.go` documents two bake kinds:
  MACHINERY-OWNED (`FieldPath.FrontierPinned` — the ordinal is final for the
  executor's assembled row / leg window) and SOURCE-RELATIVE (the ordinal
  indexes the reference's OWN source's declared column order). Same
  correlation, same ordinal, different domain, different column. Revision 1
  omitted this element entirely, and an ordinal comparison that ignores it is
  the SAME bug with better-looking types: an ordinal conflation that reads as
  authoritative is strictly more dangerous than a name conflation that reads
  as suspect.
- **Ordinal path**, per-element, `FieldPath.Equals` — already ordinal-only,
  name and pin-state excluded.

A comparison across domains is a type error and must FAIL CLOSED, never
coerce. This is why the migration has a step 0.

## Decision

The 68 sites partition into seven buckets. The partition is not prose: every
`knownFieldDecisionDebt` entry carries exactly one bucket tag as a mandatory
prefix on its reason string, pinned by `TestFieldDebtBucketsArePartition`.
Counts at this revision: boundary 2, escape 11, contract 11, dotted 15,
name-keyed 15, translator 13, harness 1. Revision 1's buckets double-counted
sites (an item claimed as both "escape" and "translator" disappears under
whichever label is softest); single ownership makes the arithmetic real —
and it already worked once: applying the bucket criteria moved
`logical_predicate.go:6188` out of translator, because both of its operands
are a Value's `.Field` (a value-identity matcher), the same shape as 4151.

**Step 0 — a fail-closed domain accessor, with the domain as a PARAMETER.**
Before any ordinal matching, the values package grows one accessor, working
signature `OrdinalIn(frontier) (int, bool)`. The caller states which layout
its ordinal set indexes; the accessor answers only when the value's ordinal
provably indexes THAT layout — for a frontier-pinned path by verifying the
pin's frontier is the given one, for an unpinned single-accessor path by
verifying the value's own source is the given frontier — and fails closed on
everything else: multi-accessor, lazy, or any domain mismatch.

The token is a GO EXTENSION, and the implementation review located exactly
why it exists: Java never needs one. Java's `FieldValue.childValue` is
non-null and typed, so the domain is always derivable as
`childValue.getResultType()` — the frontier identity rides the child. Go
mints CHILDLESS baked FieldValues, a shape Java cannot express, and only
those need the domain STORED. So the working rule is: **derive the domain
when the child is typed; store the token only when it is not** —
`NewFieldValueOfOrdinal` already derives this way and is the proof the rule
works. The rejected alternative (stop minting childless bakes entirely,
making derivation universal) is the deeper Java alignment and remains the
end-state candidate once the dotted bucket removes the qualified-name
channel that produces most childless shapes; it is not attempted in step 0
because it would couple the foundation to the riskiest bucket.

The accessor requires state that does not yet exist, and step 0 is honest
about being a REPRESENTATION change, not a query: `FieldPath` today stores
only a `FrontierPinned` boolean, and a childless source-relative value
retains no reference to its source at all — so "verify the value's source is
the given frontier" is unanswerable from the current struct. Step 0 threads
a DOMAIN TOKEN through construction and every copy/rebuild/rebase site
(the same preserve-on-copy contract `Resolved` already imposes), excluded
from equality and hashing exactly as `FrontierPinned` is — an
evaluation-contract marker, not a value distinction. Without the token,
`OrdinalIn(frontier)` would be a signature wearing a proof it cannot check.

The accessor also FAILS CLOSED on a negative ordinal, and this clause is
measured, not defensive. Java's ordinal-only accessor equality is safe only
because construction asserts `ordinal >= 0` (`FieldValue.java:651`); Go mints
`Ordinal: -1` NAME-ONLY accessors at four producer sites
(`cascades_translator.go:1910-1912`, `unnest_seed.go:177`,
`unnest_gather.go:189`, `index_expansion.go:491`). Two `-1` accessors are
ordinal-equal by construction, so for them the name is the ONLY identity — a
probe demonstrated that deleting the "redundant" name check in
`fieldValueMatchesAggregateGroupKey` lets `X(-1)` match `Y(-1)`: a
wrong-column bind manufactured by exactly the naive ordinal-only conversion
this RFC prescribes for the name-keyed bucket. The probe also established the
other direction: that name check is UNREACHABLE as a decision-changer today
(every call site ORs it with `SemanticEqualsUnderAliasMap`, and the whole
sqldriver suite stays green with the matcher hard-wired to false), both
pinned in `aggregate_group_key_accessor_name_test.go`. The consequence for
this RFC: the `-1` mints are part of the debt — a name-only accessor is the
name-as-identity pattern wearing an ordinal's type — and each name-keyed
conversion is safe only against accessors the domain accessor accepts.
Migrating a site that can see a `-1` accessor without first eliminating (or
fail-closing on) the mint is how this migration would ship the eighth bug it
exists to prevent.

A first draft of step 0 (`SourceRelativeOrdinal()`, no parameter, false for
all pinned forms) was wrong in a way worth recording, because it is the
failure mode this whole RFC warns against wearing step 0's own clothes.
`match_candidate_index.go:810-825` deliberately admits BOTH the
source-relative and the frontier-pinned single-accessor forms — sound there
because that site's frontier provably IS the record descriptor (the comment
at 793-813 is that proof). A parameterless accessor that rejects pinned
forms would have left no name to fall back to, so a pinned covered column
would take the `return nil, false` path: the predicate silently stops
pushing below the fetch — a quiet plan regression at the RFC's own
motivating site. And a parameterless accessor that ACCEPTED unpinned forms
would be no better: "source-relative" means relative to the reference's OWN
source, so two unpinned values from different sources return ordinals that
are not comparable, and the caller is back to knowing its domain by
comment. The domain must be an argument, so the per-site proof becomes a
predicate the accessor checks — not a comment the next caller doesn't
read.

**1. boundary (2) — resolve metadata names to ordinals at candidate
construction, not at match time.** `coveredColumns` / `blockedColumns` in
`match_candidate_index.go` and `windowed_index_match_candidate.go` are sets
of INDEX-DEFINITION names matched against a query value's leaf name on every
match. Resolve the index's column names against the record descriptor ONCE
when the candidate is built; match domain-checked ordinals (step 0) against
an ordinal set. The index definition names its columns — that is its right —
and the name dies at that boundary.

Two preconditions this item must respect, not assume away:

- *There is not always one descriptor.* A multi-record-type index has no
  single row layout — the index row type degrades to unknown — so "the
  candidate's ordinal set" is unsound as a single set. The resolution is
  per record type: one ordinal set per (index, record type) pair, built
  from that type's descriptor, and the match consults the set for the type
  being matched. Where the type is not statically known, the site FAILS
  CLOSED (declines the push/claim) rather than falling back to the name —
  a declined optimization is recoverable, a wrong ordinal is not.
- *Not every value arriving at these sites is baked.* Match-candidate
  columns and some rule inputs are deliberately LAZY (`Resolved == nil` —
  plan-time name carriers, per the values.go contract). A lazy value has no
  ordinal to match; the site either bakes upstream (preferred where the
  producer knows its source) or fails closed. It never falls back to
  comparing the name it happens to still carry.

**2. escape (11) — escapes return an identity key, not the name.** Sites like
`correlatedInnerField`, `leafFieldName`, `bareColumnName` return `fv.Field`
as a bare `string`; the caller keys maps by it, and by then no gate can see
the decision. These return a key struct instead. **The key struct must not
carry the name at all** — not as a field, not "for diagnostics". A struct
literal smuggling `fv.Field` through a key type is invisible to the gate
(the composite-literal check fires on keys, not values, and a returned
struct is not a returned selector), so the defense is structural: the key
type has no string field to put a name in. And because a rule without
enforcement is how this codebase got its first seven bugs, the structural
claim is itself pinned when this item lands: a reflection test over the key
type(s) asserting no field of string kind. Diagnostics render from the
FieldValue they already have.

**3. name-keyed (15) — name-keyed sets and matchers become identity-keyed.**
`referenced_fields`, `rule_implement_distinct_final`,
`rule_projection_merge`, `in_memory_sort`, `map_field_values`, `pullup`,
`replace`, `simplifier_value`, and `logical_predicate.go:4151`.
`composeFieldOverConstructor` picks a constructor member by
`field.Name == fv.Field` and is correct only because of a duplicate-name
guard — a guard that exists because the key is wrong; the migration deletes
both and pins the duplicate-name shape as a regression test.
`logical_predicate.go:4151` is reclassified from revision 1's "translator"
bucket: it ANDs a name check onto ordinal equality between two RESOLVED
values. That is a value-identity matcher, not resolution — and the name
check can REFUSE an ordinal-equal match, which is a suspected live defect
(an aliased reference to the same resolved column). Implementation probes
that shape FIRST and pins whichever answer falls out.

**4. translator (13) — the boundary keeps the name, under a mechanical test.**
These sites match a PARSED identifier or a DECLARED column list — name
resolution, the one place the rule permits a name. They move from the debt
ratchet to the allowlist only under a demonstration with two testable legs:
(a) the compared name originates from parser output or schema declaration,
never from another Value's `.Field`; (b) the site's OUTPUT carries
`Resolved != nil` (born-baked), so the name does not survive the site.
Honestly stated: (a) and (b) are checked at review time per site, with the
demonstration recorded in the allowlist entry's reason — the shape test
enforces the entry's FORM, not its truth. The (b) leg additionally gets a
unit test per allowlisted site (construct the site's output, assert
Resolved) so the demonstration outlives the review. A site failing (a) is
name-keyed and migrates — that is how 4151 and 6188 moved buckets. The mechanism this depends on now exists and is itself tested:
`allowedFieldDecisions` is per-site (`file.go:LINE` + decision count +
reason), starts EMPTY, is shape-checked by `TestFieldDecisionAllowlistIsPerSite`
(a file-wide entry fails the build), and is self-cleaning in both directions
— an entry that stops matching, or matches a different number of decisions
than it claims, fails. Revision 1's file-prefix allowlist would have let one
exempted line exempt a 6000-line translator forever; measurement also showed
its three file-wide entries covered ZERO sites, so it was deleted rather
than narrowed.

**5. contract (11) — one coordinated naming-contract change, scoped as its own
phase.** `AggregateKeyColumnName` (`group_by.go:118`) is THE naming
authority binding planner, executor, and translator for group-key output
columns; `logical_predicate.go:6093` and `cascades_translator.go:4748` are
the same contract family (aggregate output and RFC-141 hidden sort columns);
the widened detector added `values.ProjectionColumnName` (`values.go:1274`,
the projection output-naming authority) and the JDBC result-set label match
(`cascades_generator.go:3301` — the label is the contract with the DRIVER
consumer). These are not 11 independent sites; they are one contract whose
currency is a name, and they close only by making that currency an ordinal
slot where the consumer is internal — the JDBC label, whose consumer is the
user's result set, may prove to be a true boundary and end on the allowlist
instead, decided by item 4's test, not by fiat here. Scoping honestly: this
RFC migrates 57 sites and changes one cross-component contract covering the
other 11 (the aggregate-result naming switch in group_by.go joined its
sibling authority when taint tracking exposed its six arms). No wire impact — the names never leave the process — but it is the
one piece where "migrate the site" understates the work.

**6. dotted (15) — kill the qualified-name channel, then the probes die.**
Revision 1 claimed these probes exist because of the legacy flat
`Child == nil` representation. That premise is FALSE and was falsified by
reading the sites: `rule_implement_nested_loop_join.go:2337`,
`ordinal_seed.go:761` and `left_outer_existential.go:112` probe for a dot on
values whose `Child` is a live `QuantifiedObjectValue`; only
`box_conjunct.go:149` gates on `Child == nil`. The real debt is the
**qualified-name channel**: producers that pack structure into the string —
`clustered_outer_scalar.go:612` literally builds
`FieldValue{Field: alias + "." + col}`, and the merged-QOV machinery carries
`leg.col` the same way. The fix is at the producers: qualification becomes
structural (the leg's correlation/ordinal on the value), the channel dies,
and the five probes become type checks or become unreachable. Largest piece,
highest blast radius, LAST — after every other bucket has shrunk the
surface.

The gate deliberately does not flag construction — building a FieldValue
FROM a name is what the values package is for — which means it will go
green on this item while proving nothing: the producers it targets are
constructors. So this item carries its own gate, landed WITH the producer
fix. And that gate CANNOT be lexical: a quoted identifier like `"A.B"` is
ONE legitimate column name containing a dot, regression-covered, and
indistinguishable by string inspection from the channel's `alias + "." +
col`. The gate is provenance-based instead — qualification becomes a tagged
structural representation on the value, producers set the tag, and the
assertion is "no untagged Field ever contains a dot introduced by
CONCATENATION", checked where the concatenation used to happen (the
producer sites) plus a constructor-boundary debug assertion on the tag.
When that gate is green AND the fifteen probe sites are deleted, the channel
is provably closed; either alone proves nothing — and a quoted `"A.B"`
column keeps working, pinned by keeping its existing regression coverage
green through the change.

*Measured while implementing the first five sites.* The bucket is not one
channel with fifteen readers; it is four, and they close in different orders.

- **The unnest collection producer is gone, with its reader.** The lowering
  built a name-model `SEG0.COL` collection and then discarded it on every path
  that returns a plan (asserted; never fired across query, embedded, yamsql,
  cascades, sqldriver-FDB, explaindiff, plandiff). Its reader — a correlation
  set keyed by the sliced prefix — returned non-empty exactly once across those
  suites, in its own hand-built pin. Both deleted; the dependency they carried
  is the baked collection's own correlation to its owner.
- **Attribution needs a correlation, and that alone closed four sites.** The
  clustered-outer bake and outer-ref classifier each had two arms: ask the
  QuantifiedObjectValue child, or slice the display name. Deleting the slice
  arm cost nothing measurable and removed the invented-alias failure. Worth
  recording is WHAT the slice arm actually meets in production: not a qualified
  reference but a rendered aggregate output name, `SUM(AMOUNT+E.REF)`, whose
  first dot sits inside the operand — the manufactured qualifier is
  `SUM(AMOUNT+E`. So one feed of this bucket is not a qualification producer at
  all; it is the CONTRACT bucket's aggregate output-naming authority, and any
  reader that must *succeed* on such a value (rather than decline it) is gated
  on item 5, not on a producer conversion here.
- **The merged-row channel is executor-gated, and this is the STOP.**
  `ordinal_seed.legRef`'s dot-probe is load-bearing, measured: without it the
  probe fires on `FieldValue{Field:"A.ID", Child:QOV(S)}` and reports the
  MERGED correlation S as if it were leg A — the conflation itself. That value
  is minted by `rule_implement_nested_loop_join.go:2366` and
  `cascades_translator.go:3612`, which rewrite `QOV(leg).COL` into
  `QOV(merged)."LEG.COL"` because the FlatMap inner's binder resolves the
  merged row by that key. The structural form already resolves elsewhere —
  `concatLegPositionals` stores leg WINDOWS (`RecordType.Legs`) rather than
  dotted column names, and `spansFromMergedLegs` binds `QOV(leg).col`
  leg-locally over the merged row for the join-predicate path. So the
  conversion is teaching the FlatMap inner binder the same trick, which is
  executor work under its own review gate, not a translator edit. The five
  readers of this channel (`ordinal_seed.go:761`,
  `rule_implement_nested_loop_join.go:2332`, `left_outer_existential.go:112`,
  `box_conjunct.go:149`, `accessor_name_path.go:61`) stay until it lands —
  producer-first, as this item requires.
- **Three translator sites may not belong to this bucket.** The dotted
  qualifier compares in `bakeDottedRefsToLegQOV` and `bakeFlatRefsAgainstColumns`
  all guard on `Child != nil → bail`, so they see only lazy carriers minted from
  PARSED text (`p.Projections[i]`, `SortKey.Expr`, `GroupKey.Display`), and each
  emits a born-baked value — item 4's two legs, on the qualifier segment of one
  parsed identifier whose leaf segment is already tagged translator. If that
  reading holds they are name RESOLUTION, and the real defect is upstream and
  smaller than a bucket: the parser HAS the segments (`SortKey.Bare/Qualifier`,
  `GroupKey.Bare/Qualifier` are populated and then discarded;
  `LogicalProject.Projections []string` never had them), joins them, and the
  resolver splits them back. Deciding this is a bucket move, so it is left for
  the site-by-site pass rather than taken unilaterally here — but taking them as
  "producers to convert" would be converting the wrong thing.

**harness (1)** — `rowdiff/ordering.go:241` compares plan sort keys against
SQL `ORDER BY` text in the conformance oracle. Engine identity rules do not
apply to the oracle, but the entry stays on the ratchet until the harness is
audited separately rather than being waved through with the engine work.

## Order

0. Domain accessor (fail-closed) — nothing else may land first.
1. boundary (2): metadata names die at candidate construction.
2. escape (11): key structs; kills the caller-side blindness the gate cannot
   reach.
3. name-keyed (15): including the 4151 probe-first defect check; 6188 is
   the same two-Values shape and travels with it.
4. translator (13): boundary demonstrations; allowlist grows only here.
5. contract (11): the coordinated naming-contract change.
6. dotted (15): producer-side channel removal.

## Rejected alternatives

**A global `ColumnID` like CRDB's.** One integer naming a column across the
whole query is a better invariant than the triple — rebasing becomes a no-op
instead of a walk. Rejected because this is a PORT: Java's identity contract
is the resolved-accessor path, the index-matching/compensation subsystem is
a faithful port of that contract, and a competing identity notion would put
two authorities on one fact for the length of the migration — the exact
pathology this workstream exists to end. Revisit only if the triple proves
insufficient after the 68 are closed, with the call sites already uniform.

**Swapping the dotted-probe predicate for a structural one, in place.**
Treats the reader; the producer keeps writing structure into strings and
every new consumer re-grows a probe. Root cause is the channel.

**Leaving the ratchet and fixing sites as bugs surface.** This is what
produced the seven.

**Deleting the gate once the sites are fixed.** No — the gate is what holds
the count at zero, and it costs one AST walk.

## Verification

Per site: the debt entry is DELETED, not edited; the ratchet is
self-cleaning in both directions (missing AND count-mismatched entries
fail), so a fixed site cannot stay listed and a half-fixed line cannot hide.
The bucket partition is pinned by test, so sites cannot migrate between
buckets silently.

The migration is not proven by the gate going green — the gate would go
green if a decision merely moved somewhere the walk cannot see (a key struct
carrying the name is exactly such a place, which is why item 2 bans it
structurally). Each converted site needs a test on the DIMENSION that was
unprobed: two columns sharing a leaf name, reached through different
quantifiers — or the same column at the same ordinal in two DOMAINS, the
new failure mode this revision admits into the model. Where the shape is
expressible in SQL, a yamsql scenario with an EXPLAIN assertion; where it is
plan-internal, an FDB integration test.

Absent that test a conversion is unfalsifiable, because every one of the 68
sites is green today with the defect latent — which is exactly the state the
original seven shipped in.
