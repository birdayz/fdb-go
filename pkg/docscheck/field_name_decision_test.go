package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// FieldValue.Field is a DISPLAY name. It must never decide anything.
//
// Seven separate hand-rolled proofs of a semantic property by leaf-name
// comparison went wrong in this codebase, each found by a different route and
// none by the test suite: PushValueThroughFetch, correlatedInnerField,
// correlatedFieldOf, fieldValueAliasAndCol, buriedLegOrdinalLayout,
// rebaseOuterLegValue, and the unique-key proof. They share one shape — two
// columns with the same leaf name are treated as the same column, or the same
// column reached by two paths is treated as two.
//
// The correct inputs already exist. `FieldValue.Resolved` is the
// construction-time resolved accessor (Java's ResolvedAccessor), and
// `SemanticEqualsUnderAliasMap` compares values under a correlation mapping.
// CockroachDB settles this at name-resolution time by assigning a column id the
// optimizer then uses exclusively; `ColumnMeta.Alias` is documented as
// display-only.
//
// Fixing the seven and stopping there guarantees an eighth, so this is the
// build-time gate instead. It fails when `.Field` reaches a DECISION —
// equality, a switch tag, a map key, or a string-comparison helper — anywhere
// outside the allowlist below.
//
// Adding an entry is deliberately annoying: it needs a one-line justification,
// and the standing question is always "why can Resolved not answer this?"
//
// The exemption is per SITE (file:line, with a count), never per file. A
// whole-file exemption is not an allowlist, it is a hole with a comment on it:
// it covers every decision the file grows later, for free and silently. The
// earlier version of this list held three FILES; measurement showed they
// exempted nothing at all, so the loophole was pure downside — it would have
// been discovered the first time someone needed one line of a 6000-line
// translator exempted and got the other 5999 with it.
//
// # Known blind spots
//
// What the walk CANNOT see belongs here, in the gate's own documentation,
// because a green ratchet is a claim about the whole tree and its exceptions
// have to outlive whatever made them visible. Each of these was found while
// auditing a debt entry, and each was first written down INSIDE that entry's
// reason string — which is the wrong home: those entries retire (CQ-52 retires
// both of the ones below), and the blind spot would retire with the prose
// describing it while remaining just as real.
//
//   - A name compared as a PLAIN STRING PARAMETER, after the `.Field` read has
//     already happened at the caller. `legBake` (cascades_translator.go, called
//     from the multi-ForEach leg baker at :5736) takes the leaf segment as a
//     `string` and matches it against a leg's declared columns; the walk sees a
//     string parameter, not a display name, so only the QUALIFIER half of that
//     identifier is on the ratchet and the LEAF half is invisible. Closing this
//     needs types — the parameter's origin is a `.Field` read one frame up —
//     which is why it is recorded rather than fixed here.
//   - `Typ.FieldIndex(name)` and friends: resolving a name against a record
//     type is a lookup, not a comparison or a map key, so no sink tier reports
//     it. The gathered-EXISTS wrap (exists_gathered_cluster_wrap.go:131) is
//     recorded for its qualifier and silent for its leaf for exactly this
//     reason. This one is arguably the largest remaining hole: a type lookup by
//     name is precisely the conflation the gate is named for, and widening to
//     it is its own pass because the same call is the CORRECT way to resolve a
//     metadata name once, at a boundary.
//
// Both are non-detections, not exemptions: nothing suppresses them, the walk
// simply never reaches them. Neither is counted in any bucket total, so the
// arithmetic those totals feed is a floor rather than a census.
//
// The FieldIndex hole's known instances are enumerated in
// fieldIndexBlindSpotDebt below and CHECKED, rather than described here in
// prose. A blind spot recorded only in a comment is rediscovered rather than
// tracked: the prose cannot notice the call moving, being deleted, or a new one
// arriving beside it, so it decays into a claim about a tree that has moved on.

// fieldIndexBlindSpotDebt enumerates the `FieldIndex(name)`-shaped lookups the
// walk above CANNOT see, so the hole has an inventory instead of a paragraph.
//
// It is deliberately NOT knownFieldDecisionDebt: entries there must match a
// decision the detector reports, and by construction none of these do. Merging
// them would make every entry here permanently stale.
//
// TestFieldIndexBlindSpotSitesAreCurrent checks each recorded line still holds
// the call it names, so drift fails the way the main ratchet does. What it
// cannot do is prove the list is COMPLETE — that needs the detector widening
// this list exists to justify.
var fieldIndexBlindSpotDebt = map[string]string{
	// The header bullet cited :131 for this file. That line is the QUALIFIER
	// comparison, which the detector does see and which is already on the main
	// ratchet — the invisible lookups are the two below. Prose naming the wrong
	// line while claiming to name a blind spot is the decay this list replaces.
	"pkg/relational/core/query/exists_gathered_cluster_wrap.go:116": "translator: BARE column name resolved against a leg window's row type, gathered-EXISTS wrap. No qualifier to disambiguate, so two same-named columns in different windows are one column to this lookup. Retired upstream by CQ-52.",
	"pkg/relational/core/query/exists_gathered_cluster_wrap.go:135": "translator: the LEAF half of a parsed dotted identifier, resolved against the selected window's row type. Its qualifier half is on the main ratchet at :131; this half is what the ratchet cannot see. Retired upstream by CQ-52.",

	// DIAGNOSTICS ONLY, and labelled so rather than quietly filed with the
	// engine sites. This lookup decides nothing a plan can observe: it splits a
	// census witness between COLUMN-ABSENT and LAYOUT-AVAILABLE. It is recorded
	// because it is the exact move RFC-197 forbids — a column identified by its
	// display name against a row type — and the leg-local bake that was DELETED
	// from this branch made precisely this call for real, to mint an ordinal. A
	// diagnostic that survived its deleted sibling is how the move gets
	// reintroduced: someone finds it, reads it as sanctioned, and promotes it.
	"pkg/recordlayer/query/plan/cascades/leg_local_bake_census.go:474": "name-keyed: DIAGNOSTICS ONLY — classifies a census witness, never reaches a plan. The identically-shaped call that DID reach a plan (the leg-local bake) was deleted; this one is retained because the two residues it separates have different fixes. Retires with the census.",
}

type fieldDecisionSite struct {
	site string // "path/to/file.go:LINE"
	n    int    // decisions this line hosts
	why  string
}

// allowedFieldDecisions are the sites where comparing the display name is
// genuinely correct. Each needs a reason that survives the question above.
// It is EMPTY, and that is a measured result rather than an aspiration. The
// previous version exempted three whole files — values.go (which declares
// FieldValue), key_expression_proto.go and index_expansion.go — on the reasoning
// that the name is the identity at the metadata layer. Emptying the list changed
// nothing: none of the three contains a single decision the walk reports. Three
// file-wide holes were standing open to cover zero sites.
//
// So the list stays empty until a site earns a line, and the line is a SITE.
var allowedFieldDecisions = []fieldDecisionSite{}

func fieldDecisionAllowed(sites []fieldDecisionSite, site string) (fieldDecisionSite, bool) {
	for _, a := range sites {
		if a.site == site {
			return a, true
		}
	}
	return fieldDecisionSite{}, false
}

// knownFieldDecisionDebt is the surface that EXISTED when this gate was added:
// sites that should consult Resolved and do not yet. It is a RATCHET, not an
// exemption — the test fails if a new site appears, and it also fails if an
// entry here stops matching, so fixing one forces deleting its line rather than
// letting the list rot into a permanent allowlist.
//
// Deliberately NOT merged with allowedFieldDecisions above. That list says "the
// name is the identity at this layer" and is expected to stay. This one says
// "this is wrong and not yet migrated" and is expected to reach zero. Collapsing
// them would erase exactly the distinction that matters.
//
// Recorded as file:line at the moment of writing. Line drift makes an entry
// stale, which fails loudly — annoying by design, since a stale entry means
// nobody checked whether the site still needs it.
//
// Every reason carries a BUCKET TAG as its first token (RFC-197): the one
// migration bucket that owns the site. The seven buckets are a PARTITION —
// each site belongs to exactly one, and `TestFieldDebtBucketsArePartition`
// enforces the tag. An earlier version used informal categories that overlapped,
// so a site could be counted under two of them and the per-bucket totals added
// up to more than the list; migration arithmetic built on that is fiction.
//
//   - boundary:   the name crosses into a layer that stores names (index
//     definitions, covered-column sets). Fix is to resolve to ordinals at the
//     boundary, once, rather than at every crossing.
//   - escape:     the name leaves as a bare string, so the caller decides with
//     no type left to consult. The `correlatedInnerField` shape.
//   - contract:   the name IS the agreed output-naming contract with a consumer
//     (executor group keys, hidden sort-key fields). Moves only when the
//     contract itself becomes an ordinal slot.
//   - dotted:     `strings.Contains(fv.Field, ".")` asking whether a reference
//     is qualified. Structure encoded in a string; the flat "ALIAS.col"
//     representation is the actual debt and these sites are its readers.
//   - name-keyed: a set/map inside the engine keyed by leaf name, conflating
//     two same-named columns. The original seven bugs.
//   - translator: name resolution in the SQL translator, where a parsed
//     identifier legitimately arrives as text. Each still owes a demonstration
//     that its OUTPUT is resolved.
//   - harness:    test/oracle-side code, not the engine. Engine identity rules
//     do not apply, but the entry stays until the harness is audited.
//
// fieldDebt records HOW MANY decisions a line hosts, not merely that it hosts
// one. A single source line can host several: logical_predicate.go:4151 packs
// three `.Field` comparisons into one condition and carried a count of 3, until
// two of them -- tests against the EMPTY string -- were proven not to be identity
// decisions at all and the count dropped to 1. A boolean "this line is known"
// accepts any subset, so two of three could be deleted or swapped for different
// violations with the ratchet still green, and a reclassification like that one
// would pass unnoticed. The count is what makes it a ratchet.
type fieldDebt struct {
	n   int
	why string
}

var knownFieldDecisionDebt = map[string]fieldDebt{
	// boundary (0) — MIGRATED (RFC-197 item 1). Both covered-column sites now
	// resolve the index definition's column names against each record type's
	// descriptor when the translate function is built, and match a
	// domain-checked ordinal (values.FieldValue.OrdinalIn) at push time. The
	// bucket is empty rather than deleted: the partition test counts buckets,
	// and a bucket that reaches zero is the shape every other bucket is aiming
	// for.

	// escape (0) -- MIGRATED (RFC-197 item 2). fieldValueAliasAndCol
	// and bareColumnName are gone: the join fast path asks a value for its
	// CORRELATION (structurally, from its QuantifiedObjectValue child) and matches
	// the inner key column by IDENTITY against the metadata name resolved once
	// into the inner leg's row layout. The three entries this removed were one
	// helper's three arms; the two bareColumnName entries went with the lazy
	// construction arm that produced them, which built an operand the executor
	// cannot evaluate.
	//
	// gatheredExplodeElement MIGRATED too, and its escape was hiding a live
	// wrong-type defect: it handed the unnested array column's leaf name to a
	// caller holding EVERY leg of the join, which re-resolved it first-match, so
	// a struct element in one leg reported the other leg's scalar kind whenever
	// the planner put that leg outer. It now resolves the element type once, at
	// the site, by ordinal in the leg the Explode reads.
	//
	// aggregateOperandColumn is gone with it: the SUM/AVG operand's static
	// integer width — the int32-vs-int64 overflow decision, an arithmetic
	// result no plan golden can see — now indexes the input's typed column list
	// by ordinal, with the two separately-derived layouts required to agree
	// before the ordinal is used. Converting it was gated on measurement rather
	// than argument: name and ordinal answered identically on all 8358
	// aggregate operands the relational suite produces.

	// contract (15)
	//
	// The four `contract:` entries below with a `normalizeAggOutputName` note
	// were INVISIBLE when this bucket was sized, and their absence is the
	// bucket's whole shape: it listed eleven PRODUCERS of the group-by output
	// name and not one CONSUMER, because every consumer launders the name
	// through that one helper (now on nameLaunderers, with the measurement in
	// its doc comment). A migration plan written against producers alone cannot
	// close this bucket — a display name has nowhere to come from except
	// `.Field`, so converting a producer is not a thing that exists. The READ
	// side is what becomes an ordinal slot.
	//
	// Measured while they were surfaced, so nobody re-derives it: the EXECUTOR
	// is not a party to this contract. Replacing every name
	// aggregateCursor.finalizeGroup emits with `$PROBEn` leaves all 20 targets
	// of //pkg/relational/... green; only six executor UNIT tests, which assert
	// on the emitted map keys, go red. The binding is already ordinal at
	// runtime. What is left is entirely plan-time, and it is these four lines.
	"pkg/recordlayer/query/plan/cascades/values/values.go:1733": {1, "contract: explainValueOrdinals returns the rendered DISPLAY text of a value, and for a FieldValue that text is its leaf name — the rendering ProjectionColumnName (:1492) and every output-naming authority above it delegate to. Same producer family; surfaced once ReplaceAll joined nameLaunderers, since the '#'-doubling escape is what carried the name past the walk"},
	"pkg/recordlayer/query/plan/cascades/values/values.go:1731": {1, "contract: same renderer, the un-suffixed (ColumnNameValue) arm — which DROPS the '#ordinal' discriminator, so two baked reads of duplicate-named slots render identically. That is the collision this bucket is about, in the authority itself"},

	"pkg/relational/core/query/cascades_translator.go:983":  {1, "contract: groupByOutputBaker matches a post-aggregate reference's rendered name against the AGGREGATE output-name map to pick its slot — the READ side of AggregateResultColumnName (group_by.go:147-157). Laundered through normalizeAggOutputName, which is why it was absent from this bucket while all six of its producer arms were on it"},
	"pkg/relational/core/query/cascades_translator.go:992":  {1, "contract: same binder, the GROUP-KEY output-name map — the READ side of AggregateKeyColumnName (group_by.go:118). Java binds here by ordinal instead: the SELECT list is pulled up through the group-by result row by loop index (CompensateRecordConstructorRule.java:92) over columns built with Column.unnamedOf (GroupByExpression.java:754,758)"},
	"pkg/relational/core/query/cascades_translator.go:1022": {1, "contract: same binder, the final group-key lookup that actually emits the baked ordinal. Its map is last-wins on a duplicated output name (groupByOutputOrdinals `keys[full] = ord`), so two group keys sharing a leaf collapse to one slot; today they are separated only because the name channel happens to carry the qualifier, which is the dotted bucket's debt propping up this one"},
	//
	// AggregateResultColumnName renders one canonical output name per aggregate
	// function, so the SAME escape appears once per switch arm. Six lines, six
	// entries: the ratchet is per SITE, and collapsing them to one would let five
	// of the six change shape unnoticed.
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:147": {1, "contract: AggregateResultColumnName bakes the operand's DISPLAY name into the canonical aggregate output-column name the SELECT list references; same naming-authority family as AggregateKeyColumnName at :118, COUNT arm"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:149": {1, "contract: same authority, SUM arm"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:151": {1, "contract: same authority, MIN arm"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:153": {1, "contract: same authority, MAX arm"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:155": {1, "contract: same authority, AVG arm"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:157": {1, "contract: same authority, default arm"},

	"pkg/recordlayer/query/plan/cascades/values/values.go:1531":       {1, "contract: ProjectionColumnName IS the projection output-column naming contract -- the key the executor writes a projected slot under and every re-reader reads it by; the naming authority the other contract sites delegate to, and invisible until the gate could see unqualified *FieldValue inside the values package"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:118": {1, "contract: AggregateKeyColumnName is THE group-key naming contract with the executor; moves only when the contract becomes an ordinal slot"},
	"pkg/relational/core/embedded/logical_predicate.go:6191":          {1, "contract: aggregate group-key output name, same contract family"},
	"pkg/relational/core/query/cascades_translator.go:4741":           {1, "contract: sort-key hidden-field naming (RFC-141), same output-naming contract family"},

	// dotted (7)
	//
	// cascades_translator.go:988 arrived here with the launderer widening, not
	// from another bucket. It asks whether a group-by output name is QUALIFIED
	// by comparing it to its own qualifier-stripped form — the identical shape
	// to cascades_generator.go:4122 below, and it is filed with it rather than
	// with the binder it sits inside, because the debt is the flat
	// representation, not the lookup around it.
	"pkg/relational/core/query/cascades_translator.go:993": {1, "dotted: groupByOutputBaker asks whether a reference is qualified by comparing its name to stripColumnQualifier of itself, then re-looks-up the leaf; same shape as cascades_generator.go:4147"},
	//
	// value_correlation.go:57 MIGRATED (RFC-197 item 6). It keyed a correlation
	// set by the QUALIFIER sliced off a flat 'ALIAS.col' collection name — a
	// quantifier's identity decided by text. Its producer went with it: the
	// lateral-unnest lowering's name-model collection could not escape
	// translateUnnestJoin. That is a STATIC argument, not a probe — the
	// function's only success return is guarded by `resultValue != nil`, and
	// the sole assignment that leaves resultValue non-nil is the one that also
	// overwrites innerQ with the ORDINAL-baked Explode; every other path sets
	// it back to nil and raises 0AF00. So the name-model quantifier was
	// unreachable by construction, and deleting it changes no output (measured
	// too: the query and embedded suites are byte-identical with the deleted
	// lines restored). The dependency the recovery reconstructed is now the
	// baked collection's own correlation to its owner, pinned at both
	// consumers by TestGatheredExplodeOwnerEdgeReachesPartitionOrder and
	// TestGatheredExplodeOwnerEdgeReachesMatchEnumerator, whose name-model arms
	// go red if the slice is ever restored.
	"pkg/relational/core/embedded/cascades_generator.go:4147": {1, "dotted: asks whether a group-key name is QUALIFIED by comparing it to its own bare form, then labels the output column from the answer; the flat representation is the debt, not the comparison"},
	//
	// clustered_outer_scalar.go:189/402/405/406 MIGRATED (RFC-197 item 6). The
	// pull-up bake and the outer-ref classifier attribute a reference to a leg by
	// its CORRELATION; the childless arm that sliced a qualifier out of the name
	// is gone, so the ref set is written under a quantifier's own identifier
	// rather than under text. Measured: the only childless dotted value either
	// site meets across the FDB suite is a rendered aggregate output name
	// (`SUM(AMOUNT+E.REF)`), out of which the first-dot slice manufactured the
	// leg alias `SUM(AMOUNT+E` — the genuine reference that name embeds is
	// walked separately as the aggregate's operand, carrying its correlation.
	// Pinned by TestClusterBake_DoesNotAttributeAChildlessDottedName and
	// TestClusterOuterRefs_DoesNotAttributeAChildlessDottedName.
	//
	// Four more sites left this bucket by RECLASSIFICATION, not by a fix
	// (cascades_translator.go:5688/:5736/:5860 and
	// exists_gathered_cluster_wrap.go:131 — now under translator below). RFC-197
	// item 6 flagged them as possibly misfiled and left the call to the
	// site-by-site pass; the pass read them and they are name RESOLUTION. Each
	// guards `Child != nil → bail` BEFORE the dot slice, so the only value it can
	// reach is a lazy carrier minted from parsed text, and each emits a born-baked
	// value on a match. That is item 4's demonstration, applied to the qualifier
	// segment of an identifier whose LEAF segment the list already files under
	// translator. Recorded because a bucket move that looks like a fix is the one
	// way this list can lie: nothing was migrated, the sites still read a name.
	//
	// What remains here is genuinely dotted: readers of the merged-row `leg.col`
	// channel, whose producers are executor-side (CQ-53), plus the group-key
	// qualification probe.
	"pkg/recordlayer/query/plan/cascades/left_outer_existential.go:132":           {1, "dotted: leg-relative vs qualified ref probed via '.' in the name"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:2694": {1, "dotted: declines re-qualifying an already-dotted ref; Child is a live QOV, so this is the qualified-name channel, not the legacy flat shape"},
	"pkg/recordlayer/query/plan/cascades/values/accessor_name_path.go:61":         {1, "dotted: accessor path derived by splitting the name on dots"},
	"pkg/relational/core/query/box_conjunct.go:149":                               {1, "dotted: frontier read attributed by '.' probe; the only dotted site actually gated on Child == nil"},
	"pkg/relational/core/query/ordinal_seed.go:794":                               {1, "dotted: leg-ref detection via '.' probe on the merged-QOV leg.col channel"},

	// name-keyed (3)
	"pkg/recordlayer/query/plan/cascades/referenced_fields.go:125":       {1, "name-keyed: referenced-field set keyed by leaf name. Java's member is a Set<FieldValue> (ReferencedFieldsConstraint.java:41), keyed by semanticEquals/semanticHashCode, so the port is unambiguous -- and it was BUILT and MEASURED, then reverted: keying by value makes the set grow where two quantifiers share a leaf name, and this constraint's every growth re-fires the push rules. 4-table chain tasksRun 10255 -> 12901, 3-spoke ordinal star 9481 -> 12644 (both budget baselines are +-2% pins), and the hub+5 star stops planning entirely -- ErrPlannerCapHit becomes a rule-cycle round-cap divergence at 87642 tasks. plan_shape.golden does not move. The conversion is correct and the coupling between constraint growth and re-exploration is what has to change first; that is planner machinery with its own review gate, not this bucket"},
	"pkg/recordlayer/query/plan/cascades/rule_projection_merge.go:113":   {1, "name-keyed: projection merge composes an outer LAZY read by unique output-name match against the inner projection's slot names. Probed: the arm is HEAVILY LIVE -- a panic at its match point reds dozens of FDB tests (derived tables, CTEs, RFC-128 limits, cross-table predicates), so it cannot be failed closed where it stands. The outer read has no ordinal to select a slot with, and the conversion is therefore upstream: the resolver must bake a projection-output reference to its output ordinal, the way it already bakes a source-column reference (expr.go:296-297). That is translator/resolver work, not a rewrite of this site"},
	"pkg/recordlayer/query/plan/cascades/values/map_field_values.go:354": {1, "name-keyed: EqualsWithoutChildren's LAZY-vs-LAZY arm. Probed: it returns TRUE constantly in production (a panic on the true branch reds the sqldriver FDB suite and the explaindiff corpus immediately), so it is load-bearing for memo dedup of lazy carriers. It is also the one site in this bucket with NO Java counterpart to port: Java's FieldValue is resolved at construction (FieldValue.java:273-299), a lazy name carrier is a shape it cannot express, and for such a carrier the pending name IS the whole identity. Failing it closed makes two lazy references to one column unequal, which un-interns memo members rather than fixing a conflation. This closes when lazy FieldValues stop being minted, not before"},

	// translator (15)
	//
	// Two entries left by DELETION, not by migration: cascades_translator.go's
	// :6078 and :6086 (the BareRef / rendered-item name-match loops that resolved
	// an ORDER BY key against a post-aggregate projection's output spellings —
	// and RFC-197 item 4's named "first candidate for the per-site allowlist").
	// They were UNREACHABLE, and had been since the day they were written. Both
	// builders defer the SELECT-list projection PAST the sort (postSortStripProj),
	// so `translateSort`'s `s.Input` is never a *logical.LogicalProject and the
	// enclosing block never ran. Measured with a LOG probe over
	// `go test ./pkg/relational/...` (all packages green): the guard fired ZERO
	// times, and `s.Input`'s dynamic type over explaindiff+sqldriver was Filter
	// 4547 / Scan 1915 / Aggregate 1568 / Join 390 / CTE 160 / Union 92 — no
	// Project. Removed rather than allowlisted, because an allowlist entry is
	// PRECEDENT and "the name is genuinely the identity here" cannot be
	// demonstrated for code no query reaches; and removed rather than migrated,
	// because there is no ordinal to convert to when the domain it would index
	// cannot exist. The layering that makes it so is pinned by the
	// TestSortNeverSitsOverAProjection family (core/embedded) across all SEVEN
	// NewSort sites in the four builder contexts -- buildSelectShell,
	// visitOrderBy, the two union builders and the three correlated-scalar arms
	// -- and REFUSED at the consumer: translateSort returns 0AF00 on a
	// LogicalProject input. Deleting the arm without the guard left the shape
	// translating SILENTLY into a leaf-name match in the wrong ordinal domain,
	// which is a wrong sort order.
	//
	// The four :5688/:5736/:5860/exists:131 entries arrived from the dotted
	// bucket (see the note there). They are the QUALIFIER halves of identifiers
	// whose leaf halves are already here; splitting one parsed identifier across
	// two buckets was the misfiling, and the upstream fix that retires all four
	// at once is CQ-52 — the parser already produces the segments and joins them
	// only for the resolver to split them back.
	"pkg/relational/core/embedded/cascades_generator.go:3182":       {1, "translator: parsed column ref matched against declared inner columns"},
	"pkg/relational/core/query/cascades_translator.go:5768":         {1, "translator: QUALIFIER segment of a parsed identifier, single-ForEach flat baker -- guarded by `Child != nil || Resolved != nil → bail` at 5763, so the only value reaching the slice is a lazy carrier minted from parsed text, and a match emits NewFieldValueWithResolvedOrdinalInDomain (born-baked). The LEAF segment of the same identifier is the entry at 5774; retired upstream by CQ-52"},
	"pkg/relational/core/query/cascades_translator.go:5774":         {1, "translator: single-ForEach flat baker scans the layout's declared column list for the parsed leaf and emits NewFieldValueWithResolvedOrdinal; same resolve-then-bake shape as 5936, reached through a local leaf"},
	"pkg/relational/core/query/cascades_translator.go:5816":         {1, "translator: QUALIFIER segment of a parsed identifier, multi-ForEach leg baker (bakeDottedRefsToLegQOV) -- same `Child != nil || Resolved != nil → bail` guard at 5809, so it sees only lazy carriers from parsed text, and the leg it selects bakes through legBake into NewCorrelatedFieldValueWithResolvedOrdinalInDomain. Its leaf segment is invisible to this gate -- see the plain-string-parameter blind spot in the header above, which is why only the qualifier half was ever recorded; retired upstream by CQ-52"},
	"pkg/relational/core/query/cascades_translator.go:5946":         {1, "translator: QUALIFIER segment of a parsed identifier, bakeFlatRefsAgainstColumns leg-window arm -- same bail guard at 5930, born-baked on match via NewFieldValueWithResolvedOrdinalInDomain. The LEAF segment of the same identifier is the entry at 5954; retired upstream by CQ-52"},
	"pkg/relational/core/query/cascades_translator.go:5954":         {1, "translator: multi-leg baker, column membership within the matched leg window"},
	"pkg/relational/core/query/exists_gathered_cluster_wrap.go:131": {1, "translator: QUALIFIER segment of a parsed identifier, gathered-EXISTS wrap -- the QOV-shaped and composed-read arms are taken FIRST (the `Child != nil → bail` at 126 sits directly above), so the dotted arm sees only a lazy carrier from parsed text, and a match emits NewFieldValueOfOrdinal against the box QOV. Its leaf segment is invisible to this gate -- see the Typ.FieldIndex blind spot in the header above; retired upstream by CQ-52"},
	"pkg/relational/core/embedded/cascades_generator.go:3202":       {1, "translator: same inner-column lookup as the sibling entry above, leg-qualified arm -- the map key is a CONCATENATION, which is why that sibling was recorded and this one was not"},
	"pkg/relational/core/embedded/cascades_generator.go:3196":       {1, "translator: inner-column lookup by parsed name (laundered map key)"},
	"pkg/relational/core/embedded/logical_predicate.go:6767":        {1, "translator: join-side name set during translation (laundered map key)"},
	"pkg/relational/core/query/cascades_translator.go:2060":         {1, "translator: unnest element alias resolution, flat arm; the sibling arm consults the ordinal"},
	"pkg/relational/core/query/cascades_translator.go:2074":         {1, "translator: unnest element/ordinality selection by declared alias, qualified arm (laundered switch tag)"},
	"pkg/relational/core/query/cascades_translator.go:3855":         {1, "translator: element slot lookup during translation (laundered map key)"},
	"pkg/relational/core/query/cascades_translator.go:5048":         {1, "translator: pullUpSortKeyValue resolves a bare ORDER BY key against the FOLDED projection's output fields -- guarded by `Child == nil && Resolved == nil` at 5046, so the key side is a lazy carrier from parsed text, and a match emits NewFieldValueWithResolvedOrdinal. Gated on cascades_translator.go:4731, NOT on values.go:1510: this site's `fields` are named from p.Projections/p.Aliases (parser text) or a positional `_i`, and the ONLY .Field-derived names in them are the hidden sort columns collectExtraSortColumns appends, each named by sortKeyFieldRef's strings.ToUpper(fv.Field) at 4741 -- a contract-bucket entry. Converting ProjectionColumnName leaves this one red"},
	"pkg/relational/core/query/cascades_translator.go:5936":         {1, "translator: column list membership during resolution"},

	// harness (1)
	"pkg/relational/conformance/rowdiff/ordering.go:241": {1, "harness: conformance oracle compares plan sort keys to SQL ORDER BY text; engine identity rules do not apply, but the entry stays until the harness is separately audited"},
}

// fieldDebtBucketTag is the mandatory prefix on every knownFieldDecisionDebt
// reason. The seven buckets are the migration partition (RFC-197): a site has
// exactly ONE owning bucket, so the per-bucket counts sum to the list.
var fieldDebtBucketTag = regexp.MustCompile(`^(boundary|escape|contract|dotted|name-keyed|translator|harness): `)

// bucketTagOf returns the single owning bucket named at the head of a debt
// reason, and whether the reason carries one at all.
//
// Split out of the test, together with bucketCounts and invalidAllowlistEntries
// below, so the VALIDATION can be exercised against fixtures the way
// scanFieldDecisions already is. Hand-mutating the regexp once and eyeballing the
// failure proves the same thing exactly once, and then the proof is deleted along
// with the mutation; the fixtures in field_name_decision_detector_test.go are that
// proof in committed form.
func bucketTagOf(why string) (string, bool) {
	m := fieldDebtBucketTag.FindStringSubmatch(why)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// bucketCounts sums the per-bucket decision counts over a debt list and reports
// every entry whose reason names no bucket. An untagged site is in no bucket, so
// it is missing from the totals entirely.
func bucketCounts(m map[string]fieldDebt) (counts map[string]int, untagged []string) {
	counts = map[string]int{}
	for site, d := range m {
		bucket, ok := bucketTagOf(d.why)
		if !ok {
			untagged = append(untagged, fmt.Sprintf("%s\n      reason: %q", site, d.why))
			continue
		}
		counts[bucket] += d.n
	}
	sort.Strings(untagged)
	return counts, untagged
}

// bucketHeaderPattern matches a group-header comment: `// <bucket> (N)` at the
// start of the comment's own text. WHERE the comment sits is decided
// structurally rather than by indentation — see bucketHeaderCounts.
var bucketHeaderPattern = regexp.MustCompile(`^// (boundary|escape|contract|dotted|name-keyed|translator|harness) \((\d+)\)`)

// debtLiteralSpan returns the byte span of the knownFieldDecisionDebt composite
// literal's braces. Everything outside it — the doc comment above the var,
// prose in other declarations, comments in function bodies — is not a header,
// whatever it is indented by.
func debtLiteralSpan(f *ast.File) (lo, hi token.Pos) {
	for _, decl := range f.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, isVS := spec.(*ast.ValueSpec)
			if !isVS {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "knownFieldDecisionDebt" || i >= len(vs.Values) {
					continue
				}
				if lit, isLit := vs.Values[i].(*ast.CompositeLit); isLit {
					return lit.Lbrace, lit.Rbrace
				}
			}
		}
	}
	return token.NoPos, token.NoPos
}

// bucketHeaderCounts reads the per-bucket totals a debt list ADVERTISES in its
// own group-header comments, and reports anything that makes those totals
// unreadable.
//
// The headers are how anyone reads this list — nobody counts 38 map entries by
// hand — and until this existed they were unchecked prose sitting on top of the
// data they described. A retag that moved four sites between two buckets would
// leave both headers stale and every test green, which is precisely the failure
// this file exists to prevent one level down: a claim about identity that the
// code does not have. Migration arithmetic is quoted OUT of these numbers into
// RFC-197 and road-to-prod.md, so a stale header is not cosmetic — it is a plan
// sized from fiction, the same defect the partition tag was introduced to kill.
//
// Two things decide which comments count, and both were wrong before:
//
//   - SCOPE is the composite literal's own span, taken from the parsed AST.
//     The earlier version anchored on `^\t`, which reads as "a line starting
//     with one tab, ANYWHERE in the file" — and one tab is also the indent of
//     a comment in a function body. The file already parses itself for the
//     decision walk, so nothing was saved by not parsing here.
//   - FIRST HEADER WINS, and a second one for the same bucket is reported.
//     The earlier version let the LAST match overwrite, so a stale header
//     could be silently corrected by any later line that happened to look
//     like one — the gate then agreed with a number the list does not
//     advertise. Duplicates inside the literal are an error rather than a
//     tiebreak: two headers for one bucket means the list has two answers.
func bucketHeaderCounts(src []byte) (map[string]int, []string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "debt.go", src, parser.ParseComments)
	if err != nil {
		return nil, []string{fmt.Sprintf("source does not parse, so no header is locatable: %v", err)}
	}
	lo, hi := debtLiteralSpan(f)
	if lo == token.NoPos {
		return nil, []string{"knownFieldDecisionDebt composite literal not found — " +
			"the headers cannot be scoped to it, and an unscoped read counts prose"}
	}

	got := map[string]int{}
	firstLine := map[string]int{}
	var problems []string
	for _, group := range f.Comments {
		for _, c := range group.List {
			if c.Pos() < lo || c.End() > hi {
				continue
			}
			m := bucketHeaderPattern.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}
			line := fset.Position(c.Pos()).Line
			if first, dup := firstLine[m[1]]; dup {
				problems = append(problems, fmt.Sprintf(
					"bucket %q is headed twice (lines %d and %d) — the list advertises two "+
						"totals for one bucket", m[1], first, line))
				continue // first wins
			}
			firstLine[m[1]] = line
			n, err := strconv.Atoi(m[2])
			if err != nil {
				continue // unreachable: the group is \d+
			}
			got[m[1]] = n
		}
	}
	sort.Strings(problems)
	return got, problems
}

// bucketHeaderMismatches reports every bucket where the advertised header and
// the live tally disagree, in either direction. Both directions matter: a
// header that overstates hides a migrated site, and one that understates hides
// a site that arrived.
func bucketHeaderMismatches(header, live map[string]int) []string {
	var bad []string
	for _, b := range []string{"boundary", "escape", "contract", "dotted", "name-keyed", "translator", "harness"} {
		h, declared := header[b]
		if !declared {
			bad = append(bad, fmt.Sprintf("bucket %q has no `// %s (N)` group header — "+
				"every bucket advertises its count, including the ones at zero", b, b))
			continue
		}
		if h != live[b] {
			bad = append(bad, fmt.Sprintf("bucket %q: header says %d, entries tally %d", b, h, live[b]))
		}
	}
	return bad
}

// invalidAllowlistEntries returns one message per allowlist entry that is not a
// per-SITE exemption carrying a count and a reason.
func invalidAllowlistEntries(sites []fieldDecisionSite) []string {
	var bad []string
	for _, a := range sites {
		file, line, ok := strings.Cut(a.site, ":")
		if !ok || !strings.HasSuffix(file, ".go") || line == "" || strings.Trim(line, "0123456789") != "" {
			bad = append(bad, fmt.Sprintf("allowlist entry %q must be file.go:LINE — a whole-file "+
				"exemption covers every decision the file grows later, silently and for free", a.site))
		}
		if a.n < 1 {
			bad = append(bad, fmt.Sprintf("allowlist entry %q must state how many decisions the "+
				"line hosts", a.site))
		}
		if strings.TrimSpace(a.why) == "" {
			bad = append(bad, fmt.Sprintf("allowlist entry %q needs a reason answering: why can "+
				"Resolved not answer this?", a.site))
		}
	}
	return bad
}

// tallyFieldDecisions scans one parsed file and folds every reported decision
// into the allowlist tally, the debt tally, or the returned offense list.
//
// The lists it consults are PARAMETERS rather than the package globals, so the
// accumulation can be driven over synthetic source. It is three lines, and two
// of them are the increments that turn "this line is known" into a count — the
// entire difference between a ratchet and a suppression list. Reachable only
// through the tree walk, they are also unfalsifiable there: every recorded
// site carries n == 1, so replacing `seen[key]++` with `seen[key] = 1` leaves
// every tally byte-identical and the suite green. Nothing about the real tree
// can distinguish the two, which is precisely why the fixture has to supply a
// line that hosts more than one.
func tallyFieldDecisions(
	rel string,
	fset *token.FileSet,
	f *ast.File,
	allowed []fieldDecisionSite,
	debt map[string]fieldDebt,
	seenAllowed, seenDebt map[string]int,
) []string {
	var offenses []string
	scanFieldDecisions(f, func(pos token.Pos, form string) {
		key := fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line)
		if _, ok := fieldDecisionAllowed(allowed, key); ok {
			seenAllowed[key]++
			return
		}
		if _, known := debt[key]; known {
			seenDebt[key]++
			return
		}
		offenses = append(offenses, fmt.Sprintf("%s: %s uses FieldValue.Field", key, form))
	})
	return offenses
}

// debtMismatches compares the recorded debt against what the walk actually
// found, returning one message per entry that no longer matches.
//
// Self-cleaning: a debt entry that no longer matches means the site moved or was
// fixed. Either way the line must go, or the list silently becomes a permanent
// allowlist pointing at code that has changed underneath it.
//
// The COUNT is checked, not just presence. A line hosting three decisions under
// a boolean "seen" would accept one, two or three of them — delete two and swap
// the third for a different violation and the ratchet stays green, which is a
// suppression wearing a ratchet's clothes.
//
// Extracted from the tree walk for the same reason bucketTagOf and
// invalidAllowlistEntries were: the count arithmetic is the whole claim of the
// ratchet, and it was reachable ONLY through a 828-file tree walk in which every
// entry happens to carry n == 1. Under that input `seen[key]++` and
// `seen[key] = 1` are indistinguishable, so the mechanism that makes this a
// ratchet rather than a presence check was never exercised by anything. The
// fixtures in field_name_decision_detector_test.go drive it over both directions
// of disagreement directly.
func debtMismatches(want map[string]fieldDebt, seen map[string]int) []string {
	var stale []string
	for key, w := range want {
		switch got := seen[key]; {
		case got == 0:
			stale = append(stale, key+" (no decision found)")
		case got != w.n:
			stale = append(stale, fmt.Sprintf("%s (hosts %d decisions, entry says %d)", key, got, w.n))
		}
	}
	sort.Strings(stale)
	return stale
}

// allowlistMismatches applies the SAME discipline to the allowlist. An exemption
// that stops matching is an exemption nobody re-justified, and it is more
// dangerous than a stale debt entry: debt is expected to be fixed, an exemption
// claims it never needed fixing.
//
// The allowlist is empty, so in the tree walk this loop runs zero times — it is
// dead code that reads as enforcement. Its fixtures are what make the claim real.
func allowlistMismatches(sites []fieldDecisionSite, seen map[string]int) []string {
	var stale []string
	for _, a := range sites {
		switch got := seen[a.site]; {
		case got == 0:
			stale = append(stale, a.site+" (allowlisted, but no decision found)")
		case got != a.n:
			stale = append(stale, fmt.Sprintf("%s (allowlisted for %d decisions, hosts %d)", a.site, a.n, got))
		}
	}
	sort.Strings(stale)
	return stale
}

// isFieldSelector reports whether e reads `.Field` off something.
func isFieldSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel != nil && sel.Sel.Name == "Field"
}

// nameTaint is the set of LOCAL variables a function assigns the leaf name to.
// Every predicate below treats such a variable exactly as it treats `.Field`
// itself.
//
// Both tiers of the sink test inspect only the SINK expression, so a single
// local assignment hid the decision completely — and it hid two of the seven
// bugs the gate exists for. `buriedLegOrdinalLayout` writes
// `key := strings.ToUpper(qov…) + "." + strings.ToUpper(fv.Field)` and then
// `layout[key]`; `fieldValueAliasAndCol` writes `upper := strings.ToUpper(fv.Field)`
// and then `return upper[:dot], upper[dot+1:]`. By the sink the AST node is an
// Ident, and every check downstream is blind — the same blindness that let the
// RETURN escape defeat the gate's first version, one step earlier.
//
// Keyed by the parser's *ast.Object — the DECLARATION — and never by spelling,
// which is a measured correction rather than tidiness. Keying by name reports
// rule_implement_nested_loop_join.go:2269-2270, where a second, unrelated
// `key := leg.Name + "." + strings.ToUpper(fields[…].Name)` in a sibling block
// of the same function is keyed into a map. That key is built from a record
// constructor's column names and never touches a FieldValue, so the report is a
// lie in a list whose whole value is that every line on it is true. go/parser
// resolves block scopes, so the two `key`s are two objects and the question
// does not arise.
//
// The cost of the correction, stated because it is real: cascades_translator.go:5757
// stops being reported. Its `leaf` is a PARAMETER of one closure that happens to
// share a spelling with a name-derived local in a SIBLING closure, and the call
// site does pass it `fv.Field[dot+1:]` — so it is a true site, found for a false
// reason. Taint across a call boundary is out of scope by design (that is what
// the RETURN escape check covers, from the side where the type is visible), and
// a gate that keeps a site by coincidence has not earned it.
type nameTaint map[*ast.Object]bool

// has reports whether e is a local variable holding the name. Objects are the
// parser's resolution of a declaration; an unresolved identifier (package-level
// or dot-imported) has none and is never tainted, which keeps the taint strictly
// intra-function.
func (t nameTaint) has(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Obj != nil && t[id.Obj]
}

// nameDerivedIdents collects the identifiers fn assigns an expression that
// reads the leaf name — `:=`, `=`, or a `var` with an initializer.
//
// Deliberately flow-INSENSITIVE and scoped to the whole FuncDecl, nested
// closures included: an identifier assigned the name anywhere in the function
// counts everywhere in it. That over-approximates — a closure's `key` and its
// parent's unrelated `key` are one name here — and the over-approximation was
// measured on the tree rather than assumed. Lexical scoping (a closure sees the
// parent's set plus its own, and its own does not leak back out) reports the
// IDENTICAL set of sites over all 828 files, so the precise variant buys
// nothing today and costs a parent-visible taint whenever a closure assigns to
// a captured variable — `fn := func() { name = fv.Field }` followed by
// `m[name]` in the parent is a real shape it would go blind to.
//
// Deliberately intra-function. Taint across a call boundary is what the RETURN
// escape check already covers, from the side where the type is still visible;
// doing it again by name would need a call graph and would report the callee
// twice.
//
// The taint set is threaded back into the predicates as it is built, so
// `a := fv.Field; b := a; m[b]` reports: source order makes the transitive step
// free, and stopping at one step would be an arbitrary depth limit on the same
// laundering the wrapper-whitelist lesson already settled.
//
// The derivation predicate is escapesFieldName — the SHAPE-matched one — and
// not the deep-containment tier, and that is a measured narrowing rather than
// caution. Deep containment as the taint rule adds 53 sites on top of the 22
// this one finds, and they are not columns at all: it taints whatever an
// expression MENTIONING the name produces, so `dot := strings.IndexByte(upper, '.')`
// makes an int offset a display name and `if dot >= 0` an identity decision.
// The locals it reports are `dot` seven times, `curIdx` six, loop indices `i`,
// `j`, `n`, and string SLICES built beside the name. There is no Resolved
// accessor to consult for an int offset, so those entries are unfixable by
// construction, and a debt list padded with unfixable entries is one nobody
// reads — which costs the 22 real ones their audience.
//
// escapesFieldName is the right predicate because it answers exactly the taint
// question: does this expression yield a STRING that is still the name, with at
// most a decoration on it. Its four shapes are the four ways a local acquires
// the name, and it is safe without type information for the same reason the
// shallow sink tier is — `strings.ToUpper(x)` only type-checks if x is a string.
func nameDerivedIdents(fn ast.Node) nameTaint {
	t := nameTaint{}
	taint := func(lhs ast.Expr, rhs ast.Expr) {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name == "_" || id.Obj == nil || !escapesFieldName(rhs, t) {
			return
		}
		t[id.Obj] = true
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			// A multi-value RHS (`a, b := f()`) cannot attribute the name to
			// one of the results without types, so it stays silent.
			if (x.Tok != token.DEFINE && x.Tok != token.ASSIGN) || len(x.Lhs) != len(x.Rhs) {
				return true
			}
			for i, lhs := range x.Lhs {
				taint(lhs, x.Rhs[i])
			}
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if i < len(x.Values) {
					taint(name, x.Values[i])
				}
			}
		}
		return true
	})
	return t
}

// readsFieldName reports whether e delivers the leaf name through wrapping that
// PROVES it is still the name — `.Field` itself, a local identifier assigned
// from it, in parentheses, or under a launderer. This is the shape-matched tier
// of the sink test, and it needs no type information to be safe:
// `strings.ToUpper(x)` is only well-typed if x is a string, so a name read
// under it is still a name.
//
// Requiring `.Field` to be the IMMEDIATE child of a sink is how
// `coveredColumns[strings.ToUpper(v.Field)]` and `switch strings.ToUpper(fv.Field)`
// stayed invisible: the sink's child is a CallExpr, and one level of indirection
// was enough to hide the decision. Uppercasing a name does not turn it into a
// resolved column, so the wrapper is peeled and the sink is judged on what
// actually reaches it.
func readsFieldName(e ast.Expr, taint nameTaint) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return taint.has(x)
	case *ast.SelectorExpr:
		return isFieldSelector(x)
	case *ast.ParenExpr:
		return readsFieldName(x.X, taint)
	case *ast.CallExpr:
		if !nameLaunderers[callFuncName(x.Fun)] {
			return false
		}
		for _, arg := range x.Args {
			if readsFieldName(arg, taint) {
				return true
			}
		}
	}
	return false
}

// containsFieldNameRead reports whether e transitively contains a `.Field`
// read anywhere inside it. This is the DEEP tier of the sink test.
//
// Peeling only a whitelist of wrappers is how two laundering shapes stayed
// invisible after the first widening. `innerByName[legPrefix+strings.ToUpper(fv.Field)]`
// hides the name under a string CONCATENATION and
// `layouts[strings.ToUpper(fv.Field[:dot])]` hides it under a SLICE — neither is
// a call, so no whitelist of call names could ever reach them, and enumerating
// wrappers one shape at a time is a losing game against arbitrary expressions.
// A sink is judged on whether the name reaches it AT ALL: concatenating,
// slicing or upper-casing a display name does not turn it into a resolved
// column, and the map lookup that results conflates two same-named columns
// exactly as much as the bare name would.
//
// Deliberately NOT used for RETURN escapes. A returned
// `values.NewFieldValueWithResolvedOrdinal(fv.Field, …)` transitively contains a
// name read and is the CORRECT code — construction, not escape. Returns keep a
// shape whitelist; see escapesFieldName.
//
// The walk is bounded at a FuncLit: a closure appearing inside a sink
// expression is its own scope with its own returns, and its body is visited by
// the outer ast.Inspect on its own terms.
func containsFieldNameRead(e ast.Expr, taint nameTaint) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectorExpr:
			// Only the RECEIVER side is searched. A selector's Sel is a bare
			// Ident, so descending into it would let a tainted local sharing a
			// method's name match a call that never touches the name.
			if isFieldSelector(x) || containsFieldNameRead(x.X, taint) {
				found = true
			}
			return false
		case *ast.Ident:
			if taint.has(x) {
				found = true
			}
		}
		return !found
	})
	return found
}

// escapesFieldName reports whether a RETURNED expression hands the leaf name
// out as a bare string.
//
// Unlike a sink, a return cannot use deep containment: passing the name INTO a
// FieldValue constructor is what the values package exists to do, and the
// resolved-ordinal constructor that FIXED one of the seven bugs would be the
// loudest offender. So the top level is matched by SHAPE — the wrappers that
// hand back a string derived from the name and nothing else:
//
//   - `fv.Field` itself, and it in parentheses;
//   - a local identifier assigned from the name — `upper := strings.ToUpper(fv.Field)`
//     followed by `return upper`, which is `fieldValueAliasAndCol`;
//   - `strings.ToUpper(fv.Field)` — a launderer;
//   - `legPrefix + fv.Field` — concatenation, which escapes the name with a
//     decoration on it, still keyed and compared as a name downstream;
//   - `fv.Field[:dot]` — a slice, i.e. the qualifier or the leaf on its own,
//     over the name or over such a local.
//
// BELOW a launderer the rule relaxes to deep containment, which is what makes
// `strings.ToUpper(stripColumnQualifier(fv.Field))` visible. A launderer's
// argument is already a string, so anything under it is a string-to-string
// derivation of the name — there is no constructor to confuse it with, and
// requiring the inner callee to be whitelisted too would just restart the
// enumeration game one level down.
func escapesFieldName(e ast.Expr, taint nameTaint) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return taint.has(x)
	case *ast.SelectorExpr:
		return isFieldSelector(x)
	case *ast.ParenExpr:
		return escapesFieldName(x.X, taint)
	case *ast.SliceExpr:
		return escapesFieldName(x.X, taint)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return escapesFieldName(x.X, taint) || escapesFieldName(x.Y, taint)
		}
	case *ast.CallExpr:
		if !nameLaunderers[callFuncName(x.Fun)] {
			return false
		}
		for _, arg := range x.Args {
			if containsFieldNameRead(arg, taint) {
				return true
			}
		}
	}
	return false
}

// stringCompareHelpers are calls whose result is a name equality/ordering
// decision. Matched on the function's own identifier, so BOTH `strings.EqualFold(…)`
// (a SelectorExpr) and a bare or generic call like `slices.Contains(names, …)`
// are covered — an earlier version only matched method-selector calls and
// therefore missed every package-level generic helper.
var stringCompareHelpers = map[string]bool{
	"EqualFold": true, "Compare": true, "HasPrefix": true, "HasSuffix": true,
	"Contains": true, "Index": true, "SearchStrings": true, "ContainsFunc": true,
	"IndexFunc": true, "Equal": true,
}

// callFuncName returns the identifier a call expression invokes, for either
// `pkg.Fn(…)` / `x.Method(…)` (SelectorExpr) or a bare `Fn(…)` (Ident).
func callFuncName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if f.Sel != nil {
			return f.Sel.Name
		}
	case *ast.Ident:
		return f.Name
	case *ast.IndexExpr: // explicit instantiation: slices.Contains[string](…)
		return callFuncName(f.X)
	case *ast.IndexListExpr: // …with more than one type argument
		return callFuncName(f.X)
	}
	return ""
}

// nameLaunderers are string->string calls that pass the leaf name through
// unchanged in every way that matters for identity. `strings.ToUpper(fv.Field)`
// escapes exactly as much as `fv.Field` does. A CONSTRUCTOR taking the name is
// deliberately not here: building a FieldValue from a name is what the values
// package is for, and flagging it would flag the correct code.
// `normalizeAggOutputName` is in the list for the same reason and is not an
// exception to it: it IS two entries of this list composed —
// `strings.ReplaceAll(strings.ToUpper(s), " ", "")` (cascades_translator.go).
// Naming a project helper here rather than only generic `strings` functions is
// what the matcher already supports (callFuncName resolves a bare Ident), and
// it was added on measurement, not on principle: without it the group-by output
// binder's four name-to-ordinal lookups are invisible, and those are the READ
// side of the very contract the `contract:` bucket is named for. The bucket
// listed eleven producers of that name and not one consumer of it, because the
// consumers launder through this one helper.
var nameLaunderers = map[string]bool{
	"ReplaceAll": true, "normalizeAggOutputName": true,
	"ToUpper": true, "ToLower": true, "TrimSpace": true, "Clone": true,
	"TrimPrefix": true, "TrimSuffix": true, "Title": true,
}

// funcTouchesFieldValue reports whether fn names the type *values.FieldValue
// anywhere — a type assertion, a type-switch case, a parameter, a var decl.
//
// This is the discriminator the gate needs and cannot get from syntax alone:
// `.Field` is a common struct-field name. `UnresolvableOrdinalError.Field`,
// `CorrelatedShadowError.Field` and `plans.SortKey.Field` are all display
// strings on unrelated types, and flagging their Error() methods would bury the
// real signal under noise the reader learns to scroll past. Full type
// information would answer this exactly, but it would mean loading and
// type-checking the whole tree from a test; naming the type in the same
// function is the cheap approximation, and it errs toward silence rather than
// toward a gate nobody trusts.
//
// unqualified widens it to the bare identifier `FieldValue`, and is set for
// files IN the values package. Matching only the QUALIFIED selector made the
// gate blind precisely where FieldValue is declared: nothing in
// cascades/values/ writes `values.FieldValue`, it writes `*FieldValue`, so the
// return-escape check never armed for a single function in the package that
// owns the type. `ProjectionColumnName` — which returns the display name and is
// the naming authority the rest of the engine reads through — sat in the one
// directory the gate could not see into.
func funcTouchesFieldValue(fn ast.Node, unqualified bool) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel != nil && x.Sel.Name == "FieldValue" {
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "values" {
					found = true
				}
			}
		case *ast.Ident:
			// Covers both `FieldValue` and `*FieldValue`: ast.Inspect descends
			// through the StarExpr to the identifier underneath.
			if unqualified && x.Name == "FieldValue" {
				found = true
			}
		}
		return !found
	})
	return found
}

// taintedIdentIn names the first name-derived local appearing in e, for the
// report only. Empty when the decision owes nothing to the taint set.
func taintedIdentIn(e ast.Expr, taint nameTaint) string {
	var name string
	ast.Inspect(e, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && taint.has(id) {
			name = id.Name
		}
		return name == ""
	})
	return name
}

// compositeLitKeysAreValues returns a composite literal's elements when its
// keys are VALUES — i.e. when it is a map literal, or an untyped nested literal
// whose enclosing type the walk cannot see. A struct or array literal's keys are
// field names and integer indices; reporting those as name-keyed decisions is a
// spelling collision, not a conflation (see the CompositeLit arm).
//
// An untyped literal keeps the check deliberately. `map[string]T{"a": {…}}`
// elides the element type, so a nested literal with no Type of its own can still
// be a map element — and erring toward reporting there costs precision on a
// nested struct, while erring the other way would be a hole in exactly the shape
// the check exists for.
func compositeLitKeysAreValues(lit *ast.CompositeLit) ([]ast.Expr, bool) {
	switch lit.Type.(type) {
	case *ast.MapType:
		return lit.Elts, true
	case nil:
		return lit.Elts, true
	}
	return nil, false
}

// isNilIdent reports whether e is the identifier nil.
func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// isEmptyStringLit reports whether e is the literal "".
func isEmptyStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

// isOrderingOp reports whether op orders two values. Sorting BY leaf name is
// leaf-name-as-identity exactly as much as comparing by it — a
// `sort.Slice(cols, func(i,j int) bool { return cols[i].Field < cols[j].Field })`
// reintroduces the same conflation and was previously unchecked.
func isOrderingOp(op string) bool {
	switch op {
	case "<", ">", "<=", ">=":
		return true
	}
	return false
}

// scanFieldDecisions walks one parsed file and calls report for every site
// where FieldValue.Field reaches a decision. Split out of the tree walk so the
// detector itself is testable against synthetic source — a gate whose RECALL is
// never exercised is indistinguishable from a gate that matches nothing, and
// the first version of this one silently missed the function it was named for.
func scanFieldDecisions(f *ast.File, report func(pos token.Pos, form string)) {
	// Whether the enclosing top-level func names *values.FieldValue, tracked
	// so a closure inherits its parent's answer.
	handlesFieldValue := false

	// Inside the values package the type is written unqualified. Read off the
	// AST's own package clause rather than a path, so the detector fixtures
	// exercise the same derivation the tree walk does.
	inValuesPkg := f.Name != nil && f.Name.Name == "values"

	// Identifiers the enclosing top-level func assigns the name to, so a sink
	// reached through one local hop is judged on what actually flows into it.
	tainted := nameTaint{}

	// A sink decides on the name if the name PROVABLY reaches it (readsFieldName,
	// safe without type information), or if it reaches it through arbitrary
	// wrapping in a function that demonstrably handles a FieldValue.
	//
	// The deep tier needs the type discriminator and the shallow tier must not,
	// and measurement is what settled that. Deep containment ungated reports four
	// protobuf sites — `expression.Field.GetFanType() == gen.Field_FAN_OUT` in
	// index_expansion.go and match_candidate_index.go — where `.Field` is a
	// KeyExpression VARIANT holding a message, and what reaches the comparison is
	// an enum off a getter. That is the same non-decision the nil exclusion below
	// documents, arriving through a method call instead of a nil test; the
	// shape-matched tier never saw it because a non-launderer call is not a
	// wrapper it trusts.
	//
	// Gating BOTH tiers on the discriminator was tried and is wrong: it silences
	// in_memory_sort.go:142 and rowdiff/ordering.go:241, two sites the gate holds
	// today, because a sort key and an oracle compare names in functions that
	// never name the type. Trading two known sites for four false positives is a
	// worse gate on both axes, so the tiers are additive — reach is never
	// narrowed, depth is only added where the type is in play.
	//
	// The PRICE of the ungated shallow tier, stated so nobody has to rediscover
	// it: a direct `.Field` selector is typed by SPELLING ALONE. `x.Field == s`
	// on a type unrelated to FieldValue, in a function with no FieldValue
	// anywhere in it, is reported — the gate cannot tell it apart from
	// in_memory_sort.go:142 (`plans.SortKey.Field`) or rowdiff/ordering.go:241,
	// and those two are NAME-TYPED CARRIERS: the string they compare came off a
	// FieldValue upstream and carries the conflation with it. They are real debt.
	// So this is a deliberate trade, not an oversight — precision on unrelated
	// `.Field` structs is spent to keep carriers visible, and it is spent knowing
	// the type discriminator would buy the precision back at exactly that cost.
	// Both halves are pinned by fixtures (the SortKey carrier must fire; the
	// unrelated struct fires too, and the test says so), so type-based gating
	// cannot be added as a "precision fix" without re-deriving the trade.
	decides := func(e ast.Expr) bool {
		return readsFieldName(e, tainted) || (handlesFieldValue && containsFieldNameRead(e, tainted))
	}

	// localNote names the local a decision arrived THROUGH, so the report points
	// at the hop rather than at a sink whose operand reads as an ordinary
	// variable. `layout[key]` on its own tells the reader nothing; "a map key via
	// local key derived from the name" tells them where to look.
	//
	// raw is the same predicate with an EMPTY taint set: if the sink decides
	// without the taint, the name is right there in the expression and there is
	// no hop to name. Only a decision the taint set made possible gets the
	// suffix, so an unrelated tainted identifier sitting elsewhere in a sink that
	// already reads `.Field` directly cannot mislabel it.
	localNote := func(raw func(ast.Expr) bool, es ...ast.Expr) string {
		for _, e := range es {
			if raw(e) {
				return ""
			}
		}
		for _, e := range es {
			if name := taintedIdentIn(e, tainted); name != "" {
				return " via local " + name + " derived from the name"
			}
		}
		return ""
	}
	decidesRaw := func(e ast.Expr) bool {
		return readsFieldName(e, nil) || (handlesFieldValue && containsFieldNameRead(e, nil))
	}
	escapesRaw := func(e ast.Expr) bool { return escapesFieldName(e, nil) }

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			handlesFieldValue = funcTouchesFieldValue(x, inValuesPkg)
			tainted = nameDerivedIdents(x)
		case *ast.BinaryExpr:
			op := x.Op.String()
			if op == "==" || op == "!=" || isOrderingOp(op) {
				// `.Field` against nil is decidable without type information:
				// FieldValue.Field is a string and cannot be compared to nil, so
				// the receiver is some other type. `expression.Field != nil` in
				// match_candidate_index.go selects a protobuf KeyExpression
				// variant and has nothing to do with column identity — it was
				// sitting in the debt list, under a description of a decision it
				// does not make, telling its reader to consult a Resolved
				// accessor that does not exist on it.
				if isNilIdent(x.X) || isNilIdent(x.Y) {
					break
				}
				// Against the EMPTY string it is not an identity decision
				// either. `acc.Field == ""` asks whether an accessor is pure
				// ordinal access (Java's null accessor name) — it partitions
				// "has a name" from "has none" and can never confuse column A
				// with column B, which is the only failure this gate is about.
				if isEmptyStringLit(x.X) || isEmptyStringLit(x.Y) {
					break
				}
				if decides(x.X) || decides(x.Y) {
					report(x.Pos(), "a "+op+" comparison"+localNote(decidesRaw, x.X, x.Y))
				}
			}
		case *ast.SwitchStmt:
			// Switching on a display name is equality N times over. (An
			// EMPTY-tag switch needs no arm here: ast.Inspect still visits
			// each case's boolean expression as an ordinary BinaryExpr.)
			if x.Tag != nil && decides(x.Tag) {
				report(x.Pos(), "a switch tag"+localNote(decidesRaw, x.Tag))
			}
		case *ast.IndexExpr:
			// Keying a map by display name conflates same-named columns.
			if decides(x.Index) {
				report(x.Pos(), "a map key"+localNote(decidesRaw, x.Index))
			}
		case *ast.CompositeLit:
			// map[string]T{fv.Field: …} builds the same conflation through
			// a composite literal, which never produces an IndexExpr.
			//
			// Matched on the COMPOSITE LITERAL rather than on the KeyValueExpr,
			// because only the literal knows what its keys mean. In a STRUCT
			// literal `extraSortCol{name: name}` the key is a FIELD NAME, and
			// go/parser — which has no type information — resolves that bare
			// identifier to whatever declaration is in scope with the same
			// spelling. A local holding the display name is such a declaration,
			// so a struct field that merely SHARES ITS SPELLING was reported as
			// a name-keyed decision. That is the identical failure the taint set
			// already fixed on its own side by keying on the parser's *ast.Object
			// instead of the spelling; here the object IS the local's, and the
			// spelling collision happens one level up, in what the key MEANS.
			//
			// The literal's own Type settles it syntactically: only a map has
			// keys that are values. Anything else — a struct by name, an array,
			// a slice — has field names or integer indices there, neither of
			// which can confuse column A with column B. An UNTYPED nested literal
			// (`map[string]T{...}{{k: v}}` elements) keeps the check, because a
			// nested element of a map type is where a real key still appears.
			if lit, isMap := compositeLitKeysAreValues(x); isMap {
				for _, elt := range lit {
					kv, isKV := elt.(*ast.KeyValueExpr)
					if !isKV {
						continue
					}
					if decides(kv.Key) {
						report(kv.Pos(), "a composite-literal key"+localNote(decidesRaw, kv.Key))
					}
				}
			}
		case *ast.ReturnStmt:
			// The name ESCAPING as a bare string is the shape that defeated
			// the first version of this gate, and it defeated it in the very
			// function the gate was named after: `correlatedInnerField`
			// returns fv.Field, and its caller then writes `want[field]`. By
			// the caller the AST node is an Ident, not a selector, so every
			// check downstream is blind. Catching the RETURN catches it while
			// the type is still visible.
			if !handlesFieldValue {
				break
			}
			for _, r := range x.Results {
				if escapesFieldName(r, tainted) {
					report(x.Pos(), "the name escaping as a bare string (return)"+
						localNote(escapesRaw, r))
					break
				}
			}
		case *ast.CallExpr:
			if name := callFuncName(x.Fun); stringCompareHelpers[name] {
				for _, arg := range x.Args {
					if decides(arg) {
						report(x.Pos(), "a "+name+" call"+localNote(decidesRaw, arg))
						break
					}
				}
			}
		}
		return true
	})
}

// An allowlist entry must name a LINE. Nothing in the walk would reject
// `{site: "pkg/relational/core/query/cascades_translator.go"}` — it simply
// would never match, so the entry would sit there reading like an exemption
// while granting none, and the next person to "fix" it would reach for prefix
// matching and re-open the file-wide hole this replaced.
func TestFieldDecisionAllowlistIsPerSite(t *testing.T) {
	t.Parallel()
	for _, bad := range invalidAllowlistEntries(allowedFieldDecisions) {
		t.Error(bad)
	}
}

// The buckets are a PARTITION, and the tag is what makes that checkable.
//
// The informal categories this replaced were prose, and prose overlaps: a site
// could read as both an escape and a name-keyed lookup, so it got counted in
// both, and "31 sites migrate when the translator lands" was arithmetic over a
// multiset. A plan sized from double-counted buckets is not a plan. Requiring
// ONE tag per entry, checked mechanically, makes the per-bucket counts sum to
// the list by construction.
func TestFieldDebtBucketsArePartition(t *testing.T) {
	t.Parallel()

	counts, untagged := bucketCounts(knownFieldDecisionDebt)

	if len(untagged) > 0 {
		t.Errorf("%d knownFieldDecisionDebt entry/entries do not start with a bucket tag:\n    %s\n\n"+
			"Every reason must begin with exactly one of %s followed by \": \".\n"+
			"The tag names the site's SINGLE owning migration bucket — not a description of "+
			"everything the site does. The buckets are a partition precisely so the per-bucket "+
			"counts sum to the list; an untagged site is in no bucket and a site that reads as "+
			"two is counted twice, and either way the migration arithmetic built on those "+
			"counts is fiction.",
			len(untagged), strings.Join(untagged, "\n    "),
			"boundary|escape|contract|dotted|name-keyed|translator|harness")
	}

	buckets := make([]string, 0, len(counts))
	for b := range counts {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)
	var sum int
	var summary strings.Builder
	for _, b := range buckets {
		fmt.Fprintf(&summary, "\n  %-11s %3d", b, counts[b])
		sum += counts[b]
	}
	t.Logf("field-name debt by owning bucket:%s\n  %-11s %3d (over %d entries)",
		summary.String(), "TOTAL", sum, len(knownFieldDecisionDebt))

	// The group headers claim these same numbers, and a claim nothing checks is
	// how this list starts lying. Reading THIS file back is the only way to
	// check them: the counts live in comments, which the compiler discards.
	src, err := os.ReadFile(filepath.Join(sourceTreeRoot(t), "pkg/docscheck/field_name_decision_test.go"))
	if err != nil {
		t.Fatalf("read own source: %v — without it the header counts are unchecked prose", err)
	}
	header, headerProblems := bucketHeaderCounts(src)
	if len(headerProblems) > 0 {
		t.Errorf("the group headers are not readable:\n  %s", strings.Join(headerProblems, "\n  "))
	}
	if len(header) == 0 {
		t.Fatal("no `// <bucket> (N)` group headers found inside the knownFieldDecisionDebt " +
			"literal — the reader stopped matching, so a green result proves nothing " +
			"about the advertised counts")
	}
	if bad := bucketHeaderMismatches(header, counts); len(bad) > 0 {
		t.Errorf("group-header counts disagree with the entries they head:\n  %s\n\n"+
			"These numbers are quoted into RFC-197 and road-to-prod.md as migration "+
			"arithmetic. Fix the header, or fix the tags — but they cannot differ.",
			strings.Join(bad, "\n  "))
	}
}

func TestFieldNameNeverDecides(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var offenses []string
	var scanned int
	seenDebt := map[string]int{}
	seenAllowed := map[string]int{}

	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if isGeneratedFile(src, nil) {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			continue
		}
		if isGeneratedFile(src, f) {
			continue
		}
		scanned++

		offenses = append(offenses, tallyFieldDecisions(rel, fset, f,
			allowedFieldDecisions, knownFieldDecisionDebt, seenAllowed, seenDebt)...)
	}

	if scanned == 0 {
		t.Fatal("scanned no files — the walk is broken, so a green result proves nothing")
	}

	stale := append(allowlistMismatches(allowedFieldDecisions, seenAllowed),
		debtMismatches(knownFieldDecisionDebt, seenDebt)...)
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("knownFieldDecisionDebt has %d entry/entries that no longer match a "+
			"FieldValue.Field decision:\n  %s\n\nIf you FIXED the site, delete its line — the "+
			"debt list only earns its keep by shrinking. If the line merely MOVED, update it and "+
			"check whether Resolved can answer it now while you are there.",
			len(stale), strings.Join(stale, "\n  "))
	}

	if len(offenses) > 0 {
		sort.Strings(offenses)
		t.Fatalf("FieldValue.Field is a DISPLAY name and must not decide anything.\n\n%s\n\n"+
			"Seven wrong proofs in this codebase came from comparing leaf names: two columns "+
			"with the same name treated as one, or one column reached two ways treated as two. "+
			"None were caught by the suite.\n\n"+
			"Use FieldValue.Resolved (the construction-time resolved accessor) or "+
			"SemanticEqualsUnderAliasMap (comparison under a correlation mapping) instead. "+
			"CockroachDB assigns a column id at name resolution and its optimizer never sees a "+
			"name again.\n\n"+
			"If comparing the NAME is genuinely right here — because the name is the identity at "+
			"that layer, as in metadata key expressions — add the file to allowedFieldDecisions "+
			"with a reason that answers: why can Resolved not answer this?\n\n"+
			"scanned %d files", strings.Join(offenses, "\n"), scanned)
	}
	t.Logf("no FieldValue.Field decisions outside the allowlist (%d files scanned)", scanned)
}

// TestFieldIndexBlindSpotSitesAreCurrent keeps the FieldIndex blind-spot
// inventory honest about line drift.
//
// The main ratchet gets this for free: its entries must match a decision the
// detector reports, so a moved line goes stale and fails. These entries have no
// detector behind them by definition, so without this check the list would be
// prose in a map — the same decay, one data structure further along.
//
// It asserts the recorded line still holds a FieldIndex-shaped lookup. It
// deliberately does NOT assert the list is complete; that claim needs the
// detector widening the list exists to justify, and pretending otherwise here
// would be the vacuous-green failure the census gate documents at length.
func TestFieldIndexBlindSpotSitesAreCurrent(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	if len(fieldIndexBlindSpotDebt) == 0 {
		t.Fatal("fieldIndexBlindSpotDebt is empty — either the hole closed (delete " +
			"this test and the header bullet together) or the inventory was dropped, " +
			"which is how a blind spot goes back to being rediscovered")
	}

	var problems []string
	for site, why := range fieldIndexBlindSpotDebt {
		rel, lineStr, found := strings.Cut(site, ":")
		if !found {
			problems = append(problems, fmt.Sprintf("%s: not in file:line form", site))
			continue
		}
		line, err := strconv.Atoi(lineStr)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: line is not a number", site))
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: read: %v", site, err))
			continue
		}
		lines := strings.Split(string(src), "\n")
		if line < 1 || line > len(lines) {
			problems = append(problems, fmt.Sprintf("%s: file has %d lines", site, len(lines)))
			continue
		}
		if !strings.Contains(lines[line-1], "FieldIndex(") {
			problems = append(problems, fmt.Sprintf(
				"%s: line holds %q, which is not a FieldIndex lookup (reason on file: %s)",
				site, strings.TrimSpace(lines[line-1]), why))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("fieldIndexBlindSpotDebt has %d stale entry/entries:\n  %s\n\n"+
			"If the call MOVED, update the line. If it is GONE, delete the entry — this "+
			"inventory earns its keep by shrinking. If it was replaced by an ordinal "+
			"lookup, that is the migration this list is tracking, so say so in the commit.",
			len(problems), strings.Join(problems, "\n  "))
	}
}
