# RFC-197: Column identity is an ordinal, not a name

Status: PROPOSED — implementation blocked on Graefe + Torvalds ACK.

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
IMPOSSIBLE, and 38 sites are grandfathered on its ratchet.

This RFC is about closing those 38.

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
and no new type to invent. What is missing is that 38 sites still ask the
name a question the path can answer.

Column identity in this engine is the pair **(CorrelationIdentifier, ordinal
path)** — not the ordinal alone, since ordinal 0 of two different quantifiers
are different columns, and not the name at any point. `correlatedInnerField`
returning `(string, CorrelationIdentifier)` is the wrong pair by one element,
and that is the whole bug class in one signature.

## Decision

Migrate all 38 to (correlation, ordinal path). Specifically:

1. **Resolve metadata names to ordinals at candidate construction, not at
   match time.** `matchCandidateIndex`'s `coveredColumns` /
   `blockedColumns` are sets of INDEX-DEFINITION names matched against a
   query value's leaf name at every match. Resolve the index's column names
   against the record descriptor ONCE when the candidate is built, and match
   `v.Resolved.Single().Ordinal` against an ordinal set. Same for
   `windowedIndexMatchCandidate`. This is the CRDB move applied to the
   metadata boundary: the index definition names its columns, that is its
   right, and the name dies at the boundary.

2. **Escapes return the pair, not the name.** Eight sites return
   `fv.Field` as a bare `string` and the caller keys a map by it —
   `correlatedInnerField`, `leafFieldName`, `bareColumnName`, and the rest.
   By the caller the type is gone and no gate can see the decision, which is
   precisely how the first version of the enforcement gate missed the
   function it was named after. These return a comparable key struct.

3. **Name-keyed sets in rules become ordinal-keyed.**
   `referenced_fields`, `rule_implement_distinct_final`,
   `rule_projection_merge`, `in_memory_sort`, `map_field_values`, `pullup`,
   `replace`, `simplifier_value`. `simplifier_value`'s
   `composeFieldOverConstructor` currently picks a constructor member by
   `field.Name == fv.Field` and leans on a duplicate-name guard for
   correctness — a guard that exists only because the key is wrong.

4. **Dotted-name probes lose their reason to exist.** Five sites ask
   `strings.Contains(fv.Field, ".")` to decide whether a reference is
   qualified. That is structure encoded in a string, and the string is the
   LEGACY FLAT representation (`Child == nil`). These are not fixed by
   swapping the predicate; they are fixed by removing the representation
   that makes the question necessary. This is the largest piece and the last
   one.

5. **The SQL translator layer keeps the name — and only it.** Roughly 15
   sites live in `cascades_translator`, `logical_predicate` and
   `cascades_generator`, matching a parsed identifier against a declared
   column list. That IS name resolution, the one place the rule permits a
   name. They move from the debt ratchet to `allowedFieldDecisions` ONLY
   with a per-site demonstration that the site's OUTPUT is a resolved value
   and the name does not survive it. A site that resolves a name and then
   hands the name onward is not on the boundary; it is a leak wearing the
   boundary's clothes.

## Rejected alternatives

**A global `ColumnID` like CRDB's.** The literal CRDB shape — one integer
naming a column across the whole query — is a better invariant than
(correlation, ordinal path), because it makes rebasing a no-op instead of a
walk. It is rejected because this is a PORT: Java's identity contract is the
resolved-accessor path, the index-matching and compensation subsystem is a
faithful port of that contract, and introducing a competing identity would
mean two identity notions in one memo for however long the migration takes.
Two authorities for one fact is the pathology this whole workstream is
about. If the (correlation, ordinal path) pair proves insufficient after the
38 are closed, that is the moment to revisit — with the call sites already
uniform.

**Leaving the ratchet and fixing sites as bugs surface.** This is what
produced the seven. The gate already exists; a ratchet that never reaches
zero is a permanent allowlist with extra ceremony.

**Deleting the gate once the sites are fixed.** No. The gate is what keeps
the count at zero, and it costs one AST walk.

## Verification

Per site: the debt entry is DELETED, not edited. The ratchet is
self-cleaning — an entry that stops matching fails the build — so a fixed
site cannot be silently left listed.

The migration is not proven by the gate going green; the gate would go green
if the decision merely moved somewhere it cannot see. Each converted site
needs a test on the DIMENSION that was unprobed: two columns sharing a leaf
name, reached through different quantifiers, where the name-keyed code
returns the wrong one. Where such a query is expressible in SQL, that test
is a yamsql scenario with an EXPLAIN assertion; where the shape is
plan-internal, an FDB integration test.

Absent that test the conversion is unfalsifiable, because every one of the
38 sites is green today with the defect latent — which is exactly the state
the original seven shipped in.
