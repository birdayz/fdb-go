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
//   - `Typ.FieldIndexUnique(name)` and friends: resolving a name against a record
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
// The FieldIndex hole is CLOSED, and the inventory that tracked it is gone with
// it. `RecordType.FieldIndex` and `RecordType.LookupField` were first-match
// scans by name: they answered the first field carrying a name even when a row
// carried it twice, so a caller could not tell a correct answer from a guess.
// Both were DELETED rather than kept beside the declining forms, because a
// first-match lookup left in the API is a copy target — the next site reaches
// for it and inherits the guess. What survives is FieldIndexUnique /
// LookupFieldUnique, which resolve only when the name matches exactly one field.
//
// The guard's DIRECTION inverted with the fix. The old list was watched for
// going stale, i.e. for entries whose call had moved; zero entries was the
// failure state, and the test said so. Now zero first-match lookups is the
// steady state and the alarm is GROWTH: the danger is one coming back. That is
// what TestNoFirstMatchNameLookup below watches. Relaxing the old floor instead
// of inverting it would have left the revival unwatched.

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
//
// THE `contract:` BUCKET WAS READ AGAINST THIS LIST AND EARNED NOTHING. It is the
// bucket most likely to, because its sites are naming AUTHORITIES — the argument
// writes itself: `SELECT COUNT(*)` has to label its column something, and that
// text is an API contract with the user, so the render decides nothing. Two
// measurements refuse it.
//
//   - Java has no such contract. An unaliased aggregate is Column.unnamedOf
//     (GroupByExpression.java:754) and surfaces as the positional `_0`
//     (Expressions.java:251-253 mints it as `"_" + index`, Type.java:2645-2651,
//     RelationalStructMetaData.java:81-89), and nothing matches that label back
//     — lookupAlias skips unnamed expressions outright
//     (SemanticAnalyzer.java:521-523) and the group-by pull-up binds by loop
//     index (CompensateRecordConstructorRule.java:73-95). Go's `COUNT(X)`
//     spelling is a Go-only display convention. A site cannot be exempted as
//     "the name IS the identity at this layer" when the reference
//     implementation keeps no name at that layer at all.
//
//     THE PORTABLE FORM OF THIS IS ABOUT A FENCE, NOT AN ABSENCE, and the
//     difference decides what to go looking for in Go. "Java never renders an
//     expression into a column name" is too strong: Star.java:178-179 does
//     exactly that, `expression.getUnderlying().toString()` installed as a
//     StructType FIELD NAME, reached from all three Star factories. What keeps
//     it away from result metadata is call ORDER — Expressions.expanded()
//     (Expressions.java:79-84) flattens every Star before any
//     LogicalOperator.output is built (the expansion runs at
//     LogicalOperator.java:397, 436, 473, 531 and 651), and
//     underlyingAsColumns() (Expressions.java:269-287) has no rendering
//     fallback at all: the name is Optional and stays empty when absent. So
//     Java's guarantee here is DISCIPLINE, not construction. The Go-side
//     question that follows is therefore not "does Go render a name" — it
//     plainly does — but "does Go have an output type whose name comes from a
//     rendered value, fenced only by the order its callers happen to run in".
//     That is the same shape as the two-faces problem below, and it is OPEN:
//     Go carries `Star` as a boolean on logical.AggregateCall rather than as an
//     expression node, so there is no structural mirror to grep for, and no
//     search yet run has been scoped widely enough for its negative to mean
//     anything. Recorded as a question, not as a clean bill — booked as CQ-99.
//
//   - Every renderer in the bucket also FEEDS A MATCH. AggregateKeyColumnName's
//     text is a match key in plans/ordering.go and in the translator's keyOrds;
//     AggregateResultColumnName's fed aggOrds; ColumnNameValue's rendering is
//     compared in CanBridgeOrderingFieldValues and indexed in
//     rule_implement_in_union.go; ProjectionColumnName is the key the executor
//     writes a slot under and the planner reads it back by. So the honest split
//     — legitimate where RENDERED, debt where MATCHED — does not partition these
//     sites. It partitions their CALLERS, and one declaration serves both.
//
// That last point is the mechanism, and it is what any future entry has to
// defeat: values.go's explainValueOrdinals is ONE function behind two faces,
// ExplainValue (display, could never confuse two columns) and ColumnNameValue
// (which NAMES OUTPUT COLUMNS). Allowlisting the display face would exempt the
// naming face, because they are the same lines. Nothing structural stops it
// today — so the prerequisite for ever admitting a renderer here is a display
// renderer no naming authority can reach, not a better-worded reason.
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
//   - dotted:     the flat "ALIAS.col" representation — structure encoded in a
//     string. Both ENDS of it: the READERS (`strings.Contains(fv.Field, ".")`
//     asking whether a reference is qualified, then splitting it back apart) and
//     the MINTS that build it (`corr + "." + fv.Field`). The bucket used to say
//     "these sites are its readers", and that was true because the detector
//     could only see readers — every arm watched a name being consumed. RFC-197
//     orders this migration PRODUCER-FIRST, so the one end that mattered most
//     was the end nothing was counting.
//   - name-keyed: a set/map inside the engine keyed by leaf name, conflating
//     two same-named columns. The original seven bugs.
//   - translator: name resolution in the SQL translator, where a parsed
//     identifier legitimately arrives as text. Each still owes a demonstration
//     that its OUTPUT is resolved.
//   - harness:    test/oracle-side code, not the engine. Engine identity rules
//     do not apply, but the entry stays until the harness is audited.
//
// EVERY ENTRY STATES A RETIREMENT CONDITION, and the marker is not yet uniform.
// An entry with no exit condition is unreachable work: nobody can pick it up,
// because nothing says what closing it means. All entries now carry one, and it
// must be FALSIFIABLE and MECHANICAL — "retires when X carries Y instead of Z",
// naming the site and the property, never "retires when the representation
// improves". Where an entry is genuinely permanent it says WHY rather than
// inventing an exit.
//
// DO NOT COUNT THE CONDITIONS BY GREPPING FOR A MARKER. The literal
// `RETIREMENT CONDITION` string is a convenience for readers, not a census key:
// a large minority of entries state their exit in other words — "Retires
// with…", "closes exactly when…", "the site dies when…", "WHAT CLOSES IT:…" —
// and are no less complete for it. Those four are quoted verbatim from entry
// `why` strings and each is findable; an earlier revision listed "moves only
// when…" as a fifth, which occurs in NO entry — it was lifted from the
// `contract:` BUCKET DESCRIPTION one screen above, i.e. from the wrong
// population, inside the very comment telling readers to parse the entries. An earlier version of this comment prescribed
// accepting the two marker spellings (`:` and `,`); that remedy is WRONG in the
// direction it warns about, because the marker itself is absent from roughly a
// third of the entries and the split is at least five ways, not two. Counting on
// it under-reports badly while looking authoritative.
//
// The population is counted by PARSING the map literal and reading each `why` —
// never by a line-oriented regex, since many entries wrap their `: {` onto a
// later line, so a pattern like `^\t"pkg/.*": \{` misses more than half.
// `TestFieldDecisionAllowlistIsPerSite` and the bucket census read the literal
// itself for exactly this reason.
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
	// boundary (2). It was 0 — MIGRATED (RFC-197 item 1): both covered-column
	// sites now resolve the index definition's column names against each record
	// type's descriptor when the translate function is built, and match a
	// domain-checked ordinal (values.FieldValue.OrdinalIn) at push time.
	//
	// The bucket is NON-EMPTY again, and not because anything regressed. The
	// call-boundary taint made a site visible that was always there: a name
	// handed to a helper as a plain string parameter was invisible until the
	// detector followed it across the call. Reporting the bucket as migrated
	// while the walk could not reach one of its members is exactly the false
	// green this pass was built to end, so the count rises and says why.
	//
	// It rises again, 1 → 2, for a SECOND attempt inside that same descent
	// rather than a second site: protoFieldByName learned to try the escaped
	// spelling of the name, closing a silent-NULL read of any field whose
	// identifier the proto-name escaper rewrites. Both entries retire
	// together, on the same ordinal resolution.
	"pkg/recordlayer/query/plan/cascades/values/values.go # protoFieldByName # a != comparison via local name derived from the name # 1":  {1, "boundary: the same nested-record descent as :941, one attempt further down. protoFieldByName now also tries the ESCAPED spelling of the accessor name (protoname.ToProtoBufCompliantName), because a descriptor emitted from a SQL identifier stores the escaped form and no case folding maps `a$b` onto `a__1b` -- without it a mangled field name resolved to nothing and read back as a silent NULL. It is the SAME debt as :941 and retires with it: the fix is to resolve the nested path to field NUMBERS once at the boundary, at which point neither spelling attempt exists -- GATED, measured: RFC-225 designed that boundary resolution and is BLOCKED on RFC-204 sec 4.4/4.5. The bake needs a nested *values.RecordType, and the candidate-side chain is UnknownType at the first nested level: index_expansion.go GetBaseType -> IndexRowType -> PositionalTypeForDescriptor -> FieldTypeForProtoField, whose MessageKind arm returns UnknownType for everything but UUID. Widening it re-types every array and struct column and changes plan-time typing for every query, which is RFC-204 sec 4.4/4.5 gated work, not a step of this one. This entry retires when that lands, not before"},
	"pkg/recordlayer/query/plan/cascades/values/values.go # protoFieldByName # a EqualFold call via local name derived from the name # 1": {1, "boundary: descendResolvedPath's nested-record step resolves a ResolvedAccessor's per-step name against a PROTO DESCRIPTOR (protoFieldByName, called with acc.Field at :895) -- three spellings tried, then a case-insensitive scan. Newly visible: the name crossed a plain string parameter. This is the documented survivor of the accessor name (values.go's own contract: the name lives on ONLY for nested descent into a proto.Message or nested record map). The hedge this entry used to carry -- 'it may well be correct', 'the fix IF THERE IS ONE' -- is REFUTED by reading the reference: Java descends this exact step by ORDINAL. FieldValue.eval calls MessageHelpers.getFieldValueForFieldOrdinals (FieldValue.java:169), which indexes getFields().get(ordinal) and THROWS out of range (MessageHelpers.java:170-175) rather than missing quietly; the name is consumed once at construction, via recordType.getFieldNameToOrdinalMap() with RECORD_DOES_NOT_CONTAIN_FIELD when absent (FieldValue.java:272-300 -- name branch :283-290, ResolvedAccessor.of store :297). The name-taking overload exists in MessageHelpers but FieldValue.eval is not a caller, so the fix is not conditional and is exactly the one already named: resolve the nested path to field NUMBERS once at the boundary. Also wider than one step -- protoFieldByName has three callers (values.go:895, :1113, :1117). Recorded rather than allowlisted, and now for a stronger reason than before: the name is NOT the identity here even at the descriptor layer, so an allowlist entry would assert something the reference contradicts. What still gates the conversion is the nested-descent audit -- ResolvedAccessor already carries an Ordinal, but on the producers that reach this arm it is KNOWN NOT to be the descriptor's declaration index -- unnest_seed.go and unnest_gather.go mint the struct-descent suffix as Ordinal -1 by design, and expr.fuseNestedAccessors copies a SQL-struct-type position that matches the emitted descriptor only by convention. Converting the read before the producers is a silent wrong-column read; pinned by TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal -- GATED, measured: RFC-225 designed that boundary resolution and is BLOCKED on RFC-204 sec 4.4/4.5. The bake needs a nested *values.RecordType, and the candidate-side chain is UnknownType at the first nested level: index_expansion.go GetBaseType -> IndexRowType -> PositionalTypeForDescriptor -> FieldTypeForProtoField, whose MessageKind arm returns UnknownType for everything but UUID. Widening it re-types every array and struct column and changes plan-time typing for every query, which is RFC-204 sec 4.4/4.5 gated work, not a step of this one. This entry retires when that lands, not before"},

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

	// contract (12)
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
	"pkg/recordlayer/query/plan/cascades/values/values.go # explainValueOrdinals # the name escaping as a bare string (return) via local name derived from the name # 1": {1, "contract: explainValueOrdinals returns the rendered DISPLAY text of a value, and for a FieldValue that text is its leaf name — the rendering ProjectionColumnName (values.go:1710 -- the entry said :1492, stale) and every output-naming authority above it delegate to. Same producer family; surfaced once ReplaceAll joined nameLaunderers, since the '#'-doubling escape is what carried the name past the walk. RETIREMENT CONDITION: retires WITH ProjectionColumnName (the entry below), not separately -- when a projected column's name is STORED on the column at construction (Java's Column.of(Optional<String>, value) -> Field.of(..., fieldNameOptional), Column.java:81-82) and read back by getter, this renderer stops being an output-naming authority and its remaining caller is ExplainValue, which decides nothing. Falsify by finding an output-naming authority that still delegates to explainValueOrdinals after that change"},
	"pkg/recordlayer/query/plan/cascades/values/values.go # explainValueOrdinals # the name escaping as a bare string (return) via local name derived from the name # 2": {1, "contract: same renderer, the un-suffixed (ColumnNameValue) arm — which DROPS the '#ordinal' discriminator, so two baked reads of duplicate-named slots render identically. That is the collision this bucket is about, in the authority itself. RETIREMENT CONDITION: this arm is the row-KEY authority, not merely a display renderer -- with withOrdinals=false it is ColumnNameValue (values.go:1959), which NestedResolvedPath upper-cases and ProjectionColumnName returns as the key the emitted positional row's slot is written under. It retires when executeProjection's posNames come from the projection's ORDINAL vector rather than from any rendering, i.e. when ProjectionColumnName/OutputColumnName feed only the user-visible api.ColumnDef.Name and every downstream re-reader addresses the slot by index. Checkable: no call to ProjectionColumnName/OutputColumnName outside tests has its result used as a Datum/row map key or a GetByName argument. The function's own escape hatch already anticipates this -- it says that if DDL ever admits a '#' in a field name the path must be minted from the accessors directly; the retirement is that mint, made unconditional"},

	"pkg/relational/core/query/cascades_translator.go # groupByOutputBaker # a map key via local key derived from the name # 1": {1, "contract: groupByOutputBaker matches a post-aggregate reference's rendered name against the AGGREGATE output-name map to pick its slot — the READ side of AggregateResultColumnName. Laundered through normalizeAggOutputName, which is why it was absent from this bucket while all six of its producer arms were on it. RETIREMENT CONDITION: this read is already nearly unreachable and the census is booked at the site -- the two structural early returns above it (multi-accessor at :1107, Resolved.FrontierPinned at :1126) take most arrivals, and the aggregate-name arm went from 1014 hits to 1 over //pkg/relational once aggregateCallOutputSlot and the OutputSlots projection bind started RECORDING the slot at the composition that decides it. It retires when that residual is 0 -- when every post-aggregate reference arrives frontier-pinned or multi-accessor -- at which point the keyOrds/aggOrds map[string]int parameters are DELETED from the groupByOutputBaker signature. Falsify by finding a reference that reaches the map read; the count is the test"},
	"pkg/relational/core/query/cascades_translator.go # groupByOutputBaker # a map key via local key derived from the name # 2": {1, "contract: same binder, the GROUP-KEY output-name map — the READ side of AggregateKeyColumnName (group_by.go:116 -- the entry said :118, stale). Java binds here by ordinal instead: the SELECT list is pulled up through the group-by result row by loop index (CompensateRecordConstructorRule.java:92) over columns built with Column.unnamedOf (GroupByExpression.java:754,758). RETIREMENT CONDITION: same signature deletion as the aggregate arm above -- retires when keyOrds/aggOrds are gone from groupByOutputBaker's parameter list because every arrival is served by the multi-accessor or frontier-pinned early return. Until then the narrower condition is the one on the '# 3' entry: groupKeyOrdinalByStructure already decides WHICH key structurally and declines only when either side has Resolved == nil, so this read stops being an identity decision as soon as every reference reaching the site carries a non-nil Resolved"},
	"pkg/relational/core/query/cascades_translator.go # groupByOutputBaker # a map key via local key derived from the name # 3": {1, "contract: same binder, the final group-key lookup that actually emits the baked ordinal. Its map is still last-wins on a duplicated output name (groupByOutputOrdinals `keys[full] = ord`), so two group keys sharing a leaf still collapse to one slot IN THE MAP. THE CLAUSE THAT STOOD HERE IS REFUTED AND IS CORRECTED RATHER THAN DELETED, because it was the stated reason for an ORDERING CONSTRAINT on the whole migration: it said the two keys 'are separated only because the name channel happens to carry the qualifier, which is the dotted bucket's debt propping up this one', i.e. that retiring dotted would ARM a wrong-rows bug here. That is no longer true. groupKeyOrdinalByStructure (cascades_translator.go) decides WHICH key a reference is from the two Values -- SameColumnPath plus sameQuantifierRoot -- and OVERRIDES the last-wins slot at both baker arms (the qualifier-stripped arm and the direct-leaf arm) and at the ORDER BY consumer sortKeyAggregateOutputSlot. So the ordinal-identity replacement the ordering constraint was waiting for is ALREADY IN PLACE. It is also already pinned IN THE POST-DOTTED STATE: TestGroupByOutputBaker_StructureDecidesWhichSameLeafKey builds the maps with the qualified aliases groupByOutputOrdinals registers ('O.K'/'I.K') DELIBERATELY ABSENT -- exactly the world where the name channel no longer carries the qualifier -- and each of the three arms is detected by its own mutation: removing the override at the stripped arm reddens only qualifier_stripped_arm, at the leaf arm only direct_leaf_arm, at the sort consumer only sort_key_arm. WHAT SURVIVES, and is this entry's actual remaining debt: groupKeyOrdinalByStructure DECLINES when either side has Resolved == nil, and on a decline keyOrds' last-wins is the sole decider again. RETIREMENT CONDITION, falsifiable and mechanical: this entry retires when every reference reaching groupByOutputBaker carries a non-nil Resolved -- at which point the structural decider can never decline, keyOrds decides nothing but WHETHER the reference names an output column, and the map read is no longer an identity decision. Falsify it by finding a lazy (Resolved == nil) FieldValue arriving at this site; while one exists the entry stands"},

	// THERE ARE FOUR NAME-DERIVED MAP READS IN groupByOutputBaker AND ONLY THREE
	// ENTRIES ABOVE. The three are cascades_translator.go:1046 (aggOrds[key]),
	// :1055 (the keyOrds[key] probe) and :1085 (the keyOrds[key] that emits the
	// baked ordinal). The FOURTH is :1057,
	// `keyOrds[normalizeAggOutputName(stripped)]` — the qualified-ref-to-bare-key
	// strip, with its ambiguity scan immediately below at :1064-1069.
	//
	// It has no entry because THIS GATE CANNOT SEE IT, and that is the fact worth
	// recording rather than the missing line. The taint reaches `key` — which is
	// why the `!=` comparison at :1056 IS on the list, filed under dotted — but
	// :1057 keys on `stripped`, and `stripped` comes from
	// `stripColumnQualifier(key)`, which is not on nameLaunderers. Taint stops at
	// that call, so the read keys as no form at all. The green run is therefore
	// not evidence the arm is absent; it is evidence the scan never looked.
	// Corroborated by this file's own launderer doc, which already says the
	// binder has FOUR name-to-ordinal lookups while the bucket lists three.
	//
	// The reason this particular blind spot matters more than a missing line
	// usually would: :1057-1069 is the arm that carries the UNAMBIGUOUS-OR-
	// NOTHING guarantee the other three implicitly rely on. It resolves a
	// stripped leaf only when every key bearing that leaf maps to the SAME output
	// ordinal, and leaves the reference lazy — loud at eval — otherwise. So the
	// bucket's strongest safety mechanism is the one piece of it the ratchet is
	// blind to, and a change that relaxed the ambiguity scan into a first-match
	// would move no count and fail no test here. Recorded, not fixed: widening
	// the taint through stripColumnQualifier changes what the gate reports across
	// the whole tree, which is an instrument change with its own review, not a
	// documentation correction.

	// The three binder entries above are RULED STOP, on an ordering dependency
	// rather than a missing capability — worth stating precisely, because "the
	// reference must arrive resolved" reads like a capability that has to be built
	// and is not one.
	//
	// Java's equivalent binding is structural: Expression.pullUp maps a SELECT or
	// HAVING expression onto the group-by result through a map keyed by the
	// sub-Value itself (a LinkedIdentityMap, CompensateRecordConstructorRule
	// .java:63-64,73-95), so no text is involved at any point. Go HAS that matcher
	// — aggregateCallOutputSlot, shared with bindPostAggregateValueToNativeOrdinals
	// — and it is what drained the aggregate arm from 1014 hits to 1.
	//
	// What the surviving group-key traffic cannot use it for is the flat qualified
	// name: a parser-originated `HAVING a.id` arrives as ONE accessor whose Field
	// is the string "I.K", qualifier and leaf fused. A structural matcher has
	// nothing to match on a value whose structure was spelled into its own leaf.
	// So these retire with the dotted bucket's qualified-name MINTS, not before,
	// and converting them first would replace a name lookup that works with an
	// ordinal comparison across two spellings that cannot meet.

	// AggregateResultColumnName's six switch arms were HERE and are RETIRED, by
	// deleting the `case *values.FieldValue: opName = v.Field` arm that tainted
	// the local the six returns formatted. What replaces it is not a relocation:
	// the operand's text now comes only from AggregateSpec.OperandName (the parse
	// text captured once at the sole production mint) or, absent that, from
	// values.ColumnNameValue — the ONE Value→name rendering every output-naming
	// site is required to share. The leaf read was a SECOND copy of that rendering
	// rule and disagreed with it on exactly the shape that decides something: a
	// qualified operand rendered BARE, so SUM(t.v) and SUM(u.v) both spelled
	// SUM(V) and collapsed in the last-wins aggregate half of
	// groupByOutputOrdinals — one output slot unaddressable. Pinned on both sides,
	// producer and map, by aggregate_operand_name_is_data_test.go and
	// aggregate_output_ordinal_leaf_collision_test.go.
	//
	// The conversion is the Java axis, not a Go convenience: Java stores a kept
	// name AS DATA at construction (Column.of(Optional<String>, value) ->
	// Field.of, Column.java:81-82, Type.java:2908-2910) and reads it back with a
	// getter (Type.java:2750-2763), never re-deriving it from the Value.

	// Newly VISIBLE with the MINT arm (see the dotted bucket's note on the hole),
	// and filed under contract rather than dotted because of WHO CALLS IT. The
	// line is Java's FieldPath.toString port (FieldValue.java:428-433) and it has
	// two callers wearing one face: ExplainValue, which is display and could never
	// confuse two columns, and ColumnNameValue, which NAMES OUTPUT COLUMNS. The
	// second is what makes it debt — a rendering that only ever rendered would
	// belong on no list at all.
	//
	// It is also the one site here that already carries its own mitigation: the
	// withOrdinals form appends `#ordinal` precisely so two reads of
	// duplicate-named slots do not render alike. The naming caller is the one that
	// passes false.
	//
	// RULED STOP, and the two-faces fact above is the reason rather than a
	// colourful description of it. The display face would be allowlistable on its
	// own — Java's counterpart, FieldPath.toString, is debug output and names
	// nothing. The naming face has no Java counterpart AT ALL: Java's column names
	// are stored on the Field at construction (Column.java:81-82) and never
	// rendered out of a path. Since one function serves both, any entry admitting
	// the display face exempts the naming face, so the prerequisite is SPLITTING
	// the renderer — a display-only form no output-naming authority can call —
	// and only then is there a face to exempt. Splitting alone retires no escape:
	// the naming copy still reads `.Field`. It is what makes the eventual allow
	// honest, not what earns it.
	"pkg/recordlayer/query/plan/cascades/values/values.go # explainValueOrdinals # a dotted-name MINT (qualifier joined to the name) via local name derived from the name # 1": {1, "contract: FieldPath.toString rendering joins the child's rendering to the field name. Debt through ColumnNameValue (withOrdinals=false), which is an output-NAMING authority, not through ExplainValue beside it; retires when a projected column takes its name from a resolved slot rather than from a rendered path"},

	// The four single-escape authorities below were each ruled convert / allow /
	// stop against the Java source rather than against the family they were filed
	// under, and NONE of them is an allow. The ruling is recorded on the entry
	// because "same contract family" is a statement about Go's call graph and says
	// nothing about whether the contract should exist.
	"pkg/recordlayer/query/plan/cascades/values/values.go # ProjectionColumnName # the name escaping as a bare string (return) # 1":          {1, "contract: ProjectionColumnName IS the projection output-column naming contract -- the key the executor writes a projected slot under and every re-reader reads it by; the naming authority the other contract sites delegate to, and invisible until the gate could see unqualified *FieldValue inside the values package. RULED CONVERT, and Java names the shape: a projected column's name is stored AS DATA on the column at construction (Column.of(Optional<String>, value) -> Field.of(value.getResultType(), fieldNameOptional), Column.java:81-82, Type.java:2908-2910) and read back by getter (Type.java:2750-2763, DataTypeUtils.java:76) -- never re-derived from the Value, which is what this function does on every read. What it costs is a name carried on the projected column through every copy/rebuild/rebase, the same preserve-on-copy contract Resolved already imposes; the executor then WRITES the stored name instead of both sides re-deriving and agreeing by convention. RETIREMENT CONDITION, stated explicitly because the ruling above describes the fix without naming its exit: retires when values.Column (or the projection's column record) carries a Name field populated at construction and preserved across copy/rebuild/rebase, ProjectionColumnName returns THAT stored name rather than calling explainValueOrdinals, and the executor writes the stored name into the row. Falsify by finding a projected column whose output name is still derived from its Value at read time. The two explainValueOrdinals entries retire with this one, since they are the renderer it delegates to"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go # AggregateKeyColumnName # the name escaping as a bare string (return) # 1": {1, "contract: AggregateKeyColumnName is THE group-key naming contract with the executor. RETIREMENT CONDITION: retires when the group-key output column is addressed by ORDINAL rather than named -- when this function and its mirror aggregateGroupKeyOutputName are both deleted because the emitted row's slot is identified positionally. Falsify by finding a consumer that needs the group key's NAME rather than its slot; note plans/ordering.go is exactly such a consumer today, which is what blocks it. RULED STOP, on a blocker that is already booked rather than a new one: its text is a MATCH key at plans/ordering.go, where RichOrdering addresses its ordering set by rendering, so the provided and requested keys meet only as strings -- CQ-55 (ordering matched on structural identity) over CQ-56 (the ordinal domain). Not an allow, and Java is why the direction is settled: Java's group-key output columns carry NO name to be a contract with (Column.unnamedOf, GroupByExpression.java:754,758 -> the positional _0, Type.java:2645-2651) and the pull-up binds by loop index (CompensateRecordConstructorRule.java:73-95)"},
	"pkg/relational/core/embedded/logical_predicate.go # aggregateGroupKeyOutputName # the name escaping as a bare string (return) # 1":      {1, "contract: aggregate group-key output name, the exact mirror of the executor's aggKeyName. RULED STOP, travelling with AggregateKeyColumnName above and blocked on the same CQ-55/CQ-56: two renderings of one slot cannot stop being renderings one at a time, because the agreement between them is the only thing keeping the emitted slot name and the re-read name in lockstep. RETIREMENT CONDITION, made explicit rather than left as a reference to the sibling: retires when the group-key output slot is addressed by ORDINAL on both sides -- when the executor's aggKeyName and this mirror are both deleted because the emitted row's slot is identified positionally, as Java does it (Column.unnamedOf group-key columns, GroupByExpression.java:754,758, pulled up by loop index at CompensateRecordConstructorRule.java:73-95). It cannot retire alone: converting ONE of the two renderings breaks the lockstep that currently keeps them agreeing, so the falsifiable check is that BOTH sites are gone in the same change. Blocked on the ordering-identity work (CQ-55 over CQ-56), because AggregateKeyColumnName's text is also a MATCH key in plans/ordering.go where provided and requested orderings meet only as strings"},
	"pkg/relational/core/query/cascades_translator.go # sortKeyFieldRef # the name escaping as a bare string (return) # 1":                   {1, "contract: sort-key hidden-field naming (RFC-141), same output-naming contract family. RULED CONVERT, and the escape is real -- but the two mechanisms this entry previously blamed were both MEASURED FALSE, so anyone executing the old text did work that fixes nothing. Java is unchanged and still the spec: it appends the same hidden columns for an ORDER BY key absent from the SELECT list (LogicalOperator.java:390-399 -- the old text said 389, off by one) and finds and drops them PURELY BY ORDINAL, the final projection re-using the original output list through Expressions.rewireQov's FieldValue.ofOrdinalNumber counter (Expressions.java:87-96), with membership by value-derivability (Expressions.difference -> canBeDerivedFrom, Expressions.java:124-146, Expression.java:254-264), never name equality. NOT in the old text and load-bearing: Java's difference removes only what is derivable from the OUTPUT and performs NO dedup among the order-by expressions themselves, so Go's `seen[name]` is a Go-only invention with no upstream counterpart. REFUTED (1), AND THE REFUTATION IS ITSELF NOW REFUTED -- both readings are kept because the sequence is the lesson. The claim was: that dedup cannot merge distinct source columns, because sortKeySourceValue depends on the key ONLY through sortKeyFieldRef(k), so alike-rendering keys carry an identical source value by construction and collapsing them is correct. That was TRUE WHEN WRITTEN and FALSE WHEN SHIPPED, and the change it was justifying is what falsified it: RFC-218 added sortKeySourceValue's NESTED arms ABOVE the sortKeyFieldRef call, so a nested key returns a distinct per-member value while still rendering its struct ROOT. Two nested keys of one root (`ORDER BY n.co, n.sk`) then rendered alike, the second key's hidden column was dropped as a duplicate, and the query returned SILENTLY WRONG ROWS -- measured [3 1 2] where SQL requires [3 2 1], and [2 3 1] in the other key order. A dedup keyed on a rendering is only as sound as the claim that the rendering determines the value, which is precisely what a fix that stops re-deriving values from renderings destroys. FIXED (RFC-227): the dedup is keyed on the source VALUE (symmetric semantic equality, Java's Expressions.difference shape) and the appended column is named by its resolved PATH. Pinned by extra_sort_column_identity_test.go and by nested_sort_key_rows_fdb_test.go's TestFDB_TwoNestedSortKeysOfTheSameStructRootDoNotCollapse. REFUTED (2): pullUpSortKeyValue does not recover hidden columns by scanning field names; it recovers them at the VALUE match, proven by renaming an appended column to a string no name-scan could find and watching resolution still succeed. Carrying len(fields)+i fixes neither. THE REAL DEFECT is that the rendering is FLAT and loses nested path segments: a struct-column key renders `N.SK` and the last-dot split yields `SK`, so `ORDER BY t1.n.sk` plans an unresolvable ordinal. The clause that followed -- that `ORDER BY n.sk` SORTS BY THE WHOLE STRUCT -- is MEASURED FALSE and is corrected rather than deleted, because it is the reading that makes the flat rendering look merely cosmetic: there is no struct comparator to sort by, so that shape does not mis-order, it FAILS LOUDLY with `values: no ordering defined between *dynamicpb.Message and *dynamicpb.Message`. The single-key rows follow the MEMBER and only the EXPLAIN name was flat (pinned by TestFDB_NestedSortKeyOrdersByTheMemberNotTheStructRoot). The flat rendering's real cost was not the sort it performed but the IDENTITY it supplied to the hidden-column dedup -- see REFUTED (1) above. The conversion is therefore: the sort-key reference must stop re-deriving a flat NAME from a Value that already carries the resolved path. Scope, measured rather than assumed -- an earlier draft of this entry guessed the leaf was lost upstream and that was wrong: the resolver fuses `n.sk` into ONE FieldValue whose Resolved.Accessors is the full [N, SK] path (expr.go fuseNestedAccessors). (STALE IN ONE DETAIL, corrected here: that Field was the struct root `N`. fuseNestedAccessors copied the root node whole; it now names the fused value after its LEAF, agreeing with the planner-side mints of the same shape and with Java getLastFieldName. Nothing in this entry turns on WHICH segment Field held -- the defect is that a single segment is read where a path is meant -- so the ruling stands unchanged), and the ordinary non-fold sort path is correct precisely BECAUSE it passes that Value through untouched (bakeFlatRefsAgainstColumns returns early on a non-nil Resolved). sortKeyFieldRef's `fv.Child == nil -> ToUpper(fv.Field)` arm at :4967-4968 (the entry said 4947, stale) is where the path is discarded, so for the unqualified shape the defect IS fold-local and the fix IS confined to this file. The three-segment shape (`t1.n.sk`) is a SEPARATE and more general gap: walkColumnRef rejects a 3-segment FullId and upgradeSortKeyValues swallows the error, leaving Value nil everywhere, so that one is not a fold bug and does not convert here. RETIREMENT CONDITION, stated as a checkable property rather than left implicit in the analysis above: retires when sortKeyFieldRef's `fv.Child == nil -> ToUpper(fv.Field)` arm is deleted because the sort key arrives carrying its resolved path -- i.e. when every ORDER BY key reaching the fold has a non-nil Resolved and the reference is built from Resolved.Accessors rather than from a flat rendered name. Falsify by finding an ORDER BY key that reaches sortKeyFieldRef with Child == nil and Resolved == nil. The three-segment shape (`t1.n.sk`) is explicitly NOT covered by this condition and does not retire with it: walkColumnRef rejects a 3-segment FullId upstream and upgradeSortKeyValues swallows the error, which is a separate resolver gap"},

	// dotted (13)
	//
	// WAS 14, AND ONE RETIRED — the first entry this bucket has ever lost, so the
	// old "ZERO are retirable" reading is recorded here rather than quietly
	// replaced. cascades_generator.go # buildAggColumns is GONE (RFC-229 §2.2):
	// it hand-copied the group-key naming rule (name = fv.Field, then an
	// EqualFold of that name against its own bare form) and now READS the
	// authority, expressions.AggregateKeyColumnName. The debt did not evaporate,
	// it CONSOLIDATED: the authority is still listed, in the contract bucket, and
	// so is its other mirror aggregateGroupKeyOutputName. A mirror that reads the
	// authority is one fewer place a correction can be applied to and missed in.
	//
	// The remaining 13 were re-measured to their CURRENT file and line by
	// instrumenting the walk itself (recording fset.Position at the point each
	// key is matched) rather than by reading the prose, and all 13 still match a
	// decision the detector reports. None of THOSE is retirable: deleting any
	// entry while its declaration stands fails TestFieldNameNeverDecides,
	// which SCANS SOURCE.
	//
	// THE LINE NUMBERS IN THESE REASONS ARE THE ROT SURFACE, and the sweep found
	// them rotted four times over (corrected in place below). The cause is
	// structural, not carelessness: these keys are `file # declaration # form #
	// ordinal` and carry NO line number, so a site can drift hundreds of lines
	// with the ratchet green while every `:NNNN` citation in its prose silently
	// stops pointing at anything. Cite the DECLARATION, which the key already
	// pins; a bare `:NNNN` is a claim nothing checks.
	//
	// RECORDED SO NOBODY RE-DERIVES THEM — the claims this sweep checked and
	// found STILL TRUE, which is the half of an audit that otherwise leaves no
	// trace and gets re-run: box_conjunct's "the only dotted site actually gated
	// on Child == nil" (checked against all 14 — every other site is reached
	// through a QuantifiedObjectValue child or a plain string parameter);
	// rebaseUnnestOuterLegPredicate's twin in rule_implement_nested_loop_join.go
	// is indeed GONE, surviving only as a comment quoting the deleted expression,
	// and its Java citation FieldValue.java:335-338 is exact (ofOrdinalNumber's
	// `new Accessor(null, ordinalNumber)`); groupByOutputOrdinals' claim that
	// ProjectionMergeRule's name-matching arm is gone (rule_projection_merge.go
	// now fail-closed declines a lazy outer read); accessor_name_path's census
	// file is standing; clustered_outer_scalar.go:493 is still the exact `LEG.COL`
	// mint; and clusterFieldResolvable's innerKey really is built as
	// `ToUpper(InnerAlias) + "." + scalarCol`.
	//
	// cascades_translator.go's groupByOutputBaker arrived here with the launderer
	// widening, not from another bucket. It asks whether a group-by output name
	// is QUALIFIED by comparing it to its own qualifier-stripped form — the
	// identical shape to buildAggColumns below, and it is filed with it rather
	// than with the binder it sits inside, because the debt is the flat
	// representation, not the lookup around it. (This header said ":972" and
	// ":4447"; the two sites are now at :1056 and :5333. The SHAPE claim it was
	// making is VERIFIED — both compare a name to its own qualifier-stripped
	// form — so only the addresses were wrong.)
	"pkg/relational/core/query/cascades_translator.go # groupByOutputBaker # a != comparison via local key derived from the name # 1": {1, "dotted: groupByOutputBaker asks whether a reference is qualified by comparing its name to stripColumnQualifier of itself, then re-looks-up the leaf (`stripped != key`); same shape as cascades_generator.go's buildAggColumns. CORRECTED: this reason cited `cascades_generator.go:4499`, which is neither buildAggColumns nor a comparison of a name to its own bare form — :4499 is deriveProjectionColumnDef's display-label extraction. buildAggColumns' EqualFold is at :5333. The shape claim survives the correction; only the address was wrong. HALF-CONVERTED ALREADY, and saying so is the point: WHICH key is chosen is decided structurally by groupKeyOrdinalByStructure, and the ambiguity check beside it compares ORDINALS, not names. The string here decides only WHETHER a qualifier prefix exists, never which column. RETIREMENT CONDITION: retires when groupByOutputBaker's keyOrds is keyed by a STRUCTURAL key derived from the groupKeys []values.Value element -- the thing groupKeyOrdinalByStructure already matches on -- instead of map[string]int keyed by normalizeAggOutputName(rendered name), so `key` and `stripped` are never constructed. Falsify by finding a caller that still composes a rendered name to address a group-key output"},
	// The clustered-outer-scalar round trip is HALF gone. flattenClusterLegRefs
	// is deleted: it took a FieldValue already carrying QOV(alias), joined the
	// alias into `LEG.COL` text, and left it for the flat baker to slice apart —
	// destroying an identity that was in hand so a later reader could guess it
	// back. The reference now binds straight to its seed slot from the identity
	// (bakeClusterLegRefs), and the SPLIT that read it (`strings.IndexByte` plus
	// legByBinding[f[:i]]) is gone with it.
	//
	// What remains is the seed's output REPRESENTATION: its columns are literally
	// named `LEG.COL`, so the two sites below still compare against that
	// spelling. They no longer MANUFACTURE a qualifier — the comparison is
	// against the name the seed builder itself constructs, so a column literally
	// named `A.B` is one name here and cannot conjure a leg out of its own dot.
	//
	// One entry became TWO because the walk that was a slice is now an explicit
	// scan. The arithmetic got worse and the code got right, and that is recorded
	// rather than hidden by keeping the comparison fused onto one line.
	"pkg/relational/core/query/clustered_outer_scalar.go # clusterFieldResolvable # a == comparison via local f derived from the name # 1": {1, "dotted: clusterFieldResolvable matches a flat projection name against the inner SCALAR key -- the constructed `INNER.SCALARCOL` spelling of the seed's last column. Retires with the dotted seed representation itself: when the seed's columns carry leg identity plus a bare name instead of a joined one, this becomes an identity lookup"},
	"pkg/relational/core/query/clustered_outer_scalar.go # clusterSeedSlotByName # a EqualFold call via local f derived from the name # 1": {1, "dotted: clusterSeedSlotByName scans the seed's OWN constructed `LEG.COL` names for a flat projection name. It replaced a first-dot SLICE of the reference, which is the difference between reading the producer's spelling and inventing a qualifier -- but the spelling is still dotted text, so it retires with the seed representation, not before"},
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
	// (cascades_translator.go:5678/:5742/:5866 and
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
	// channel, plus the group-key qualification probe.
	//
	// "whose producers are executor-side (CQ-53)" stood here and is REFUTED. The
	// producers are translator-side — the correlated-scalar ordinal seed builders
	// in pkg/relational/core/query (scalar_subquery_seed.go:129 — this said :83,
	// which is a line of PROSE naming buildCorrelatedScalar and joining nothing;
	// the record-constructor field is minted at :129 as
	// `ToUpper(innerAlias) + "." + scalarCol`, and clustered_outer_scalar.go:493,
	// re-derived and still exact) name their record-constructor fields
	// `LEG.COL` literally, and the executor merely CONSUMES that spelling when
	// adaptLegPositional feeds those leg-type column names to rowSlotForLegColumn.
	// Measured: instrumenting both builders reproduced the executor census's own
	// witnesses (`C.CV` and `O.ID` verbatim, `I.QTY` as the scalarCol half of
	// `O.I.QTY`). The distinction is the whole of producer-first — pointed at the
	// executor, this bucket's retirement waits on the wrong file.
	// THE ONE ENTRY IN THIS BUCKET THAT WAS CONCEALING A DEFECT, and the entry's
	// own text is not what concealed it — the entry says only "probed via '.'",
	// which is exactly true. What concealed it is the justification standing at
	// the site, which nobody had measured: "A single source leg has no duplicate
	// column names, so FieldIndex(Field) is the leg-local ordinal."
	//
	// The window is not always a single source leg, and this function's OTHER TWO
	// ARMS both say so in as many words. All three resolve against the SAME
	// `w.Typ`, and only this one trusts a name:
	//
	//   multi-accessor  -> FieldPath.ReAnchorRootInto COUNTS matches and declines
	//                      on `dupes > 1` ("root column name is ambiguous in the
	//                      flowed layout"), and ALSO declines when a carried
	//                      ordinal disagrees with the derived one
	//   FrontierPinned  -> carries acc.Ordinal and looks up no name at all, its
	//                      comment naming this exact hazard: an opaque box leg can
	//                      expose `A.K` and `B.K` as two fields named K, "where
	//                      FieldIndex("K") would remap the already-baked ref to
	//                      the FIRST match and silently probe the WRONG column
	//                      (wrong rows)"
	//   the name arm    -> RecordType.FieldIndex, a first-match scan with no
	//                      duplicate detection at all
	//
	// Such a window is constructible in production, not hypothetical: a clustered
	// box RUN concatenates every buried leaf's columns and finalizeSeedWindows
	// narrows only the RIGHTMOST leaf ("an alias-qualified read must window the
	// leaf rather than FieldIndex across the whole concat", ordinal_seed_layout.go).
	//
	// MEASURED, and sharper than "lazy refs are undefended": the name arm is also
	// where a SOURCE-RELATIVE baked reference lands — resolved, unpinned,
	// single-accessor — and such a reference ARRIVES CARRYING the correct
	// leg-local ordinal, which this arm discards and re-derives by first match.
	// Driving the production symbol with a leg [K, K, Z] at merged offset 10 and
	// the reference `L.K` carrying leg-local ordinal 1: the name arm answers
	// merged slot 10, while the SAME reference differing ONLY in its frontier pin
	// answers 11. One bit of representation, two different columns, silently —
	// slot 10 is a real merged column of the same type, so nothing downstream
	// rejects it.
	//
	// PINNED, NOT FIXED (the fix is a functional query-engine change and needs its
	// own RFC and review): dup_named_leg_window_declines_test.go asserts the
	// wrong answer two-sidedly and fails with instructions to flip it the moment
	// the arm honours the carried ordinal or declines. Its sibling cases pin that
	// the OTHER two arms still defend, so a regression in either direction reports.
	"pkg/recordlayer/query/plan/cascades/left_outer_existential.go # rebaseOuterLegValueOrdinal # a Contains call # 1":   {1, "dotted: leg-relative vs qualified ref probed via '.' in the name. The '.' probe itself is accurate and is what this entry tracks; see the note above for the DUPLICATE-NAME defect the arm's in-place justification concealed, pinned by dup_named_leg_window_declines_test.go and NOT fixed here. RETIREMENT CONDITION: retires when every FieldValue whose Child is a windowed QOV arrives with Resolved != nil -- when the producers feeding `windows` guarantee a resolved FieldPath -- so the switch reduces to its two Resolved arms (the arity arm and the FrontierPinned arm) and BOTH the name case and the default decline are deleted. IT DOES NOT RETIRE MERELY BECAUSE THE DOTTED MINTS STOP, and that is the load-bearing correction: a qualified `A.B` and a quoted identifier `\"A.B\"` are the same bytes, so at zero dotted mints this arm does not become dead code -- it inverts into a quoted-identifier filter and declines a rebase for a legitimately-named leg column. Conservative (no wrong read) but a permanent false negative. The shape question has to move off the name and onto Resolved arity"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go # rebaseOuterLegValue # a Contains call # 1": {1, "dotted: declines re-qualifying an already-dotted ref; Child is a live QOV, so this is the qualified-name channel, not the legacy flat shape. RETIREMENT CONDITION: the gate must test accessor ARITY instead of text -- retires when the guard reads `fv.Resolved != nil && len(fv.Resolved.Accessors) == 1` and the strings.Contains conjunct is removed. The function ALREADY COMPUTES that value twelve lines later as `bareChild` from fv.Resolved.Single(), so the condition reduces to: every FieldValue-over-QOV reaching rebaseOuterLegValue carries a non-nil Resolved, making bareChild total and the dot probe redundant with it. Same quoted-identifier caveat as the left_outer_existential sibling: at zero dotted mints this guard would exclude a genuine bare column named `\"A.B\"` from rewriting, so it goes dead on arity, never on the mints stopping"},
	// MEASURED, not inferred: 3 declines in 269 979 calls over the sqldriver
	// real-FDB corpus, 2 distinct witnesses, both of the form `q$N.AID#0`. The
	// census that produced those numbers is accessor_name_path_census.go and it
	// is standing, so this entry no longer rests on a reading of the code.
	//
	// THE PREVIOUS DESCRIPTION HERE WAS WRONG, and it inverted the site. It read
	// "accessor path derived by splitting the name on dots". The site does the
	// OPPOSITE: it refuses to split and declines the comparison, because a real
	// nested path (addr.city) and an alias-qualified leaf (T.city) are
	// indistinguishable as strings. Removing the arm makes the dotted value an
	// ordinary lazy name and MATCHES it — the conflation this ratchet exists to
	// stop — which is what its mutation test now demonstrates.
	//
	// It is still debt, and the debt is UPSTREAM. The witnesses are not column
	// names: they are rendered Explain labels — correlation, dot, column,
	// #ordinal, exactly the shape values.go's explainValueOrdinals emits (:1904
	// is the joining MINT; the range ":1796-1827" this note used to cite now
	// covers ExplainValue/ColumnNameValue's doc comments and an unrelated
	// ordering-bridge helper, not the rendering code). Something stores a
	// display rendering in a lazy Field and it reaches the one match-domain
	// identity function. Retiring this entry means finding that producer; editing
	// this guard would only re-arm the conflation.
	"pkg/recordlayer/query/plan/cascades/values/accessor_name_path.go # AccessorNamePath # a Contains call # 1":  {1, "dotted: REFUSES to split a flat-dotted lazy name and declines — see the note above. The producer hunt this entry called for is DONE and it split the population in two, which is why the entry stays. MEASURED over the sqldriver real-FDB corpus, before: 4 declines in 318449 calls, 3 distinct witnesses — `q$510549.AID#0`, `q$510687.AID#0` and `N.SK`. The first two are RENDERED EXPLAIN LABELS and all 64 captured lazy-EXPLAIN-RENDERED mint origins pointed at ONE line, RecordQueryInMemorySortPlan.HintOrdering (plans/ordering.go), which re-minted a lazy FieldValue from SortKey.Field — a display string — while SortKey.ValueExpr held the baked identity all along. Fixed there by advertising ValueExpr; that mint class went 21865 to 0 and the declines went 4 to 1. NOT the two plans/ordering.go literals the mint census header credits as 'where the Explain-rendered producer turned out to live' in the plural: only :985 produced it, :131 did not. The surviving witness WAS `N.SK` — a genuinely NESTED path (struct column `n`, field `sk`, from `GROUP BY n.sk`) that the resolver FUSES into one flat Field, i.e. the real `addr.city` versus `T.city` ambiguity this guard was written for rather than a rendering leaking in. That is now stale in the count and intact in the reasoning: re-measured over the whole corpus the arm reports 0 declines in 317738 calls, because a nested-path GROUP BY key is REJECTED before it reaches the planner and the query that minted `N.SK` no longer plans at all. So the arm is currently UNREACHED rather than retired, and removing it would mis-root the same value the moment that rejection is relaxed. THESE NUMBERS ARE NOW ASSERTED, not merely written here: the accessor-path and field-value-mint censuses are gated in sqldriver TestMain (lazy_render_census_gate_test.go) with a ceiling of 0 on both the dotted declines and the lazy-EXPLAIN-RENDERED mints, over a non-vacuity floor on each denominator. This prose is no longer the only place the measurement lives. DEPENDENCY, MEASURED, AND IT RUNS BACKWARDS TO THE BUCKET NAME: this dotted: site is downstream of a contract: producer, not of any dotted mint. The witnesses that reached it were EXPLAIN renders leaking in through plans/ordering.go, and fixing that ONE contract-bucket producer took the mint class 21865 to 0 and the declines 4 to 1 with nothing in dotted: touched. Recorded in RFC-197's dependency-order section as one of the two reversed edges, so nobody re-derives dotted-as-keystone from the bucket names. RETIREMENT CONDITION: retires when the lazy-name channel it guards is gone -- when no producer can mint a FieldValue whose Field is a flat rendering of a nested path, so a dotted name reaching AccessorNamePath is impossible rather than merely unobserved. It is currently UNREACHED, not retired: the arm reports 0 declines because a nested-path GROUP BY key is rejected before the planner sees it, so removing it now would mis-root the same value the moment that rejection is relaxed. Falsify by relaxing the nested-GROUP-BY rejection and re-running the gated census"},
	"pkg/relational/core/query/box_conjunct.go # (cascadesTranslator).classifyLegConjunct # a Contains call # 1": {1, "dotted: frontier read attributed by '.' probe; the only dotted site actually gated on Child == nil. RETIREMENT CONDITION: this site has NO independent name read -- it inherits legRef's, and retires exactly when legRef does (see the ordinal_seed.go entry). Falsify by finding a name comparison in classifyLegConjunct that does not come through legRef"},
	"pkg/relational/core/query/ordinal_seed.go # legRef # a Contains call # 1":                                   {1, "dotted: leg-ref detection via '.' probe on the merged-QOV leg.col channel. The Field text is purely a SHAPE gate here -- the key legRef returns is the CORRELATION name, so the dotted string decides flat-vs-nested and nothing else. RETIREMENT CONDITION: retires when the strings.Contains(fv.Field, \".\") conjunct is replaced by the resolved-path discriminator its own downstream legRefRootInWindow already uses (len(fv.Resolved.Accessors) > 1), with the corpus's classify verdicts unchanged. Precondition, and it is the real gate: every carrier reaching classifyLegConjunct / predicateLegAliases / predicateRefsBuriedLeg has a non-nil Resolved. Falsify by finding one that does not. Same quoted-identifier caveat as the two rebase siblings -- at zero dotted mints this arm would report 'not a leg ref' for a legal quoted column and fall back to the name model, so it goes dead on arity, not on the mints stopping"},

	// THE MINTS. Five sites, all newly VISIBLE rather than new — every one of
	// them predates this entry by many commits. The detector had no arm for name
	// CONSTRUCTION: `corr + "." + fv.Field` is a `+` BinaryExpr and fell through
	// the comparison arm, so the producer end of the channel this bucket is named
	// after was counted by nothing while the reader end was counted exhaustively.
	// Measured at the moment the hole was found: the mint CQ-53 had just deleted
	// (rule_implement_nested_loop_join.go:2854) could be restored verbatim and the
	// whole ratchet stayed GREEN.
	//
	// The count going UP is the honest result of the instrument getting better,
	// and it is the opposite of what closing CQ-53's mint was expected to do to
	// this number. Both facts are true at once: one mint died, five became
	// countable.
	//
	// These are the sites RFC-197's producer-first ordering points at. A reader in
	// the list above cannot be re-keyed while its counterparty still arrives as a
	// joined string, and each of these is somebody's counterparty.
	"pkg/relational/core/query/cascades_translator.go # rebaseUnnestOuterLegPredicate # a dotted-name MINT (qualifier joined to the name) # 1":                  {1, "dotted: MINT. CQ-53's surviving producer — turns QOV(leg).COL into QOV(merged).\"LEG.COL\" so the FlatMap inner's binder can resolve the merged row by that string. Its twin in rule_implement_nested_loop_join.go is deleted (the re-anchor now carries Java's null-named ordinal accessor, FieldValue.java:335-338); this one is on the unnest-merge path and dies with the same work. THE LEVERAGE CLAIM THAT USED TO SIT HERE IS REFUTED 4-FOR-4 AND IS CORRECTED IN PLACE rather than deleted, because this entry -- not the RFC -- is what a next reader plans from, and the RFC's copy was fixed while this one survived. It read: 'HIGHEST-LEVERAGE SINGLE KILL IN THE LIST ... retiring THIS ONE MINT mechanically retires FOUR readers -- groupByOutputBaker's qualification probe, rebaseOuterLegValueOrdinal's default arm, rebaseOuterLegValue, and legRef.' NONE of the four retires behind this mint. Three (rebaseOuterLegValueOrdinal's default arm, rebaseOuterLegValue, legRef) are '.'-probe DECLINERS: they refuse a dotted name, and DOUBLE_QUOTE_ID keeps `A.B` and `\"A.B\"` the same bytes, so the shape stays legal however few dotted names are minted -- they retire on Resolved ARITY, never on a producer going away. The fourth (groupByOutputBaker) is the READ half of a closed producer/consumer pair with groupByOutputOrdinals on a different channel; were this mint's output ever to reach it the composed key would be a TWO-dot MERGED.LEG.COL, which that table never registers. The graph that produced the claim was assembled by CO-OCCURRENCE of the LEG.COL shape rather than by dataflow, which inflates fan-out in one direction only. So the bucket-level reading is NOT misled in the way the old text said: those four are not four independent fixes hiding one, they are four genuinely independent sites that merely share a shape. RETIREMENT CONDITION: retires when the FlatMap inner's binder resolves the merged row by leg identity plus ordinal instead of by the composed \"LEG.COL\" string, i.e. when the re-anchor carries Java's null-named ordinal accessor (FieldValue.java:335-338) on this path as its twin in rule_implement_nested_loop_join.go already does. Falsify by finding a merged-QOV consumer that still addresses a slot by a composed name. MEASURED and worth keeping: the mint currently produces ZERO distinct minted names across the whole real-FDB sqldriver corpus, so it is LATENT rather than dead -- a green corpus is not evidence it is gone"},
	"pkg/relational/core/query/cascades_translator.go # groupByOutputOrdinals # a dotted-name MINT (qualifier joined to the name) # 1":                          {1, "dotted: MINT. Registers the QUALIFIED spelling of a quantifier-addressed group key as an alias of its output ordinal, because SELECT/HAVING/ORDER-BY re-read it qualified while AggregateKeyColumnName names it bare. The ordinal is in hand at the registration — what is missing is a reference that arrives stating it. The projection-merge site that used to be cited here for the same resolver gap is GONE: measured over the whole relational suite, every outer read reaching ProjectionMergeRule arrives baked, so its name-matching arm was removed rather than converted. That is evidence the resolver-side baking this entry waits on already works for projection outputs, and not evidence about the group-key channel, which is a different producer. RETIREMENT CONDITION, stated explicitly since the analysis above names the gap without naming its exit: retires when a post-aggregate reference ARRIVES carrying the group key's output ordinal -- the ordinal is already in hand at the registration, so nothing has to be computed, only stated. Concretely: when the resolver binds SELECT/HAVING/ORDER-BY re-reads of a quantifier-addressed group key to the recorded output slot (the channel aggregateCallOutputSlot and the OutputSlots projection bind already use for aggregates), addKeyAlias's qualified registration has no consumer and is deleted. Retires as one with its READ side, the groupByOutputBaker MINT entry below. Falsify by finding a qualified group-key re-read that still needs the alias to bake"},
	"pkg/relational/core/query/cascades_translator.go # groupByOutputBaker # a dotted-name MINT (qualifier joined to the name) # 1":                             {1, "dotted: MINT. The READ side of the same alias table — composes 'ALIAS.COL' to match a reference against the group-by output names registered by groupByOutputOrdinals' addKeyAlias arm (:949 today; this reason said ':886', which is now inside normalizeAggOutputName's doc comment — the registration had drifted 60-odd lines and the citation did not follow). The pair is one channel and retires as one; splitting them across buckets would let either end look closed while the other holds it open. RETIREMENT CONDITION: identical to its registration half above -- retires when the reference arrives stating the output ordinal, so no qualified spelling is composed to match against. Falsify the PAIR together: if either end still needs the composed name, neither retires"},
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a dotted-name MINT (qualifier joined to the name) # 1":                  {1, "dotted: MINT. Composes 'CORR.FIELD' for the null-born nullability upgrade in deriveColumnsFromProjection. TWO CLAIMS IN THE PRIOR TEXT ARE REFUTED, and both mattered, so they are corrected rather than deleted. (1) It is NOT 'the lookup key into the null-supplying-window metadata map'. The composed string is handed to descriptorForColumn(name, descs) (cascades_generator.go:3921); the null-born map is keyed by protoreflect FullName (`nullBorn[d.FullName()]`, :4193/:4252) and never sees this string. What descriptorForColumn does with the qualifier half is the load-bearing part the old text hid: it matches by BARE name across every join-leaf descriptor and consults the qualifier ONLY as a tie-break when two candidates carry that bare column, comparing it to `d.Name()` — the PROTO DESCRIPTOR's name, i.e. the TABLE. A correlation is not a table name, so for any aliased source (`FROM orders o` mints 'O.VAL' against a descriptor named ORDERS) the tie-break cannot fire and the mint buys nothing there. (2) 'the cheapest of the five to convert' is therefore REFUTED: converting is not swapping a map key for an identity pair, it is giving descriptorForColumn a correlation-to-descriptor mapping it does not have. And the site is already KNOWN not to answer correctly across legs — descriptorForColumn's own comment works the case (two legs agreeing on type+cardinality, differing on null-born membership) and says only positional metadata flowed from the plan's result type can be right; TestFDB_CrossLegAgreementGate_NullBornNotCovered pins the wrong answer. The MINT is real and the entry stands; its cost estimate and its mechanism were both wrong. RETIREMENT CONDITION: retires when the nullBorn set is keyed by (leg plan, leg-relative ordinal) and the lookup uses legPlanFor(proj.GetInner(), qov.Correlation.Name()) plus fv.Resolved.Accessors[0].Ordinal -- the EXACT pair this same function already uses about a hundred lines below -- instead of composing a name and handing it to descriptorForColumn. That is the whole conversion: the correct addressing already exists in the file, on the other arm. Falsify by finding a null-born nullability decision that still needs a composed qualifier"},
	"pkg/relational/core/embedded/logical_predicate.go # (existsSubqueryPlanner).buildCorrelatedScalar # a dotted-name MINT (qualifier joined to the name) # 1": {1, "dotted: MINT. Builds the correlated-scalar column key in classifyProjFieldValue. THE DISCRIMINATOR IN THE PRIOR TEXT WAS WRONG and is corrected rather than deleted, because the wrong one made the entry sound like a scoping bug when the mint is actually arity-driven. It said 'qualified when the scalar is inner-scoped and bare when it is not'. Read the three arms: NOT inner-scoped takes the MATERIALIZED path (`computedScalarVal = fv`) and mints no key at all; inner-scoped AND `len(sq.joins) > 0` mints QUALIFIED, but only when the reference's Child is a QuantifiedObjectValue, else bare; inner-scoped with NO joins mints bare via parseColRef().bare(). So the qualified-vs-bare choice is made by JOIN ARITY plus the reference's own shape, and the scope test decides key-vs-no-key. The entry's POINT survives intact and is why it stays: the SAME column is still two different key spellings depending on a condition evaluated elsewhere, so a reader cannot tell a qualified column from a bare one that happens to contain a dot. Only the named condition was wrong. RETIREMENT CONDITION: retires when the CorrelatedScalarSubquery record carries ScalarOrdinal int -- the inner projection's output slot index -- instead of ScalarCol string, and the scalar-subquery seed derives its slot Name from the inner's own column list at that index rather than composing innerAlias + \".\" + scalarCol. That single FIELD-TYPE change kills this mint, its two sibling arms, and the translator-side fallback carrier together, which is why it is one condition and not three. Falsify by finding a correlated-scalar consumer that needs the column NAME rather than its slot"},

	// name-keyed (4). Two of these are newly VISIBLE rather than new: the
	// call-boundary taint reached them through a plain string parameter.
	//
	// It was 5. ProjectionMergeRule's unique-output-name composition arm left by
	// REMOVAL, not conversion, and its recorded reason was refuted on
	// measurement: it claimed the arm was "HEAVILY LIVE -- a panic at its match
	// point reds dozens of FDB tests". Counted rather than panicked (a panic
	// aborts the whole query's planning and undercounts every sibling), the rule
	// fires 897 times across ./pkg/relational/... -- FDB sqldriver, all four
	// conformance corpora, yamsql, rowdiff -- and the lazy arm is entered ZERO
	// times; the baked arm takes all 1362 slot compositions. Its only callers
	// anywhere were the rule's own unit tests and a random-tree stress
	// generator. The resolver already bakes a projection-output reference to its
	// output ordinal, which is exactly the upstream fix the entry named as
	// blocking, so the arm was dead debt rather than live debt. It is now a
	// fail-closed decline, pinned by TestProjectionMergeRule_LazyOuterReadDeclines
	// and by TestProjectionMergeRule_DuplicateInnerSlotNames_OrdinalPicksTheRightSlot,
	// the two-slots-one-name dimension the name arm could only ever decline.
	"pkg/recordlayer/query/plan/cascades/rule_implement_in_union.go # uniqueUpperFieldIndex # a EqualFold call via local name derived from the name # 1": {1, "name-keyed: uniqueUpperFieldIndex is the SHARED unique-match resolver for ordering-key baking across the whole engine, NOT the single in-union call site this entry used to describe -- and naming the wrong authority made the entry actively misleading for a lap, so the correction is kept visible rather than quietly rewritten. MEASURED, three production consumers: rule_implement_in_union.go:89 (bakeMergeComparisonKeys, fed by comparison keys), intersector_primary_key.go:1239 (bakedIntersectionKeys, which uses it as the VERIFICATION authority) and match_candidate_index.go:217 (bakeOrderingColumnIn, whose five production callers -- match_candidate_index.go:241,:245,:885 and primary_scan_match_candidate.go:245,:316 -- are fed by INDEX METADATA column names). That metadata feed is the DOMINANT one; PlanContext.GetPrimaryKeyColumns' return type is incidental to this site. RETRACTED AS FALSE: the prior claims that this is 'the bucket's next conversion, not a blocked one' and that 'nothing here needs building'. A capability DOES have to be built. RETIREMENT CONDITION: retires when match candidates carry a LAYOUT-RESOLVED COLUMN CONTRACT -- resolved column identities instead of a metadata name plus a layout to resolve it against -- so that bakeOrderingColumnIn disappears entirely. Falsify by finding a match candidate whose columns cannot state an identity against the flowed leg row. Retires TOGETHER with bakeDottedRefsToLegQOVWithRef, which is blocked on the same capability; closing one alone leaves the other holding it open. The refactor this entry used to prescribe, making GetPrimaryKeyColumns return identities, does NOT retire the site and is a REGRESSION RISK. Deleting this entry while the declaration stands fails TestFieldNameNeverDecides -- the gate SCANS SOURCE, it is not list-only -- and would additionally require fieldDebtAuthorityTotal 35 to 34, which the prior text omitted. The conversion it asked for is ALREADY DONE, at the very site it wanted to move away from: commonPrimaryKeyValues' lazy FieldValue{Field: upper(col)} is consumed and resolved inside the same call chain by bakeIntersectionPrimaryKeyValues (intersector_primary_key.go:1101-1122) via bakeOrderingColumnIn against the legs' unanimous layout, fail-closed on a miss, so the name never escapes into a plan. The type system FORCES that placement: OrdinalDomain's sig is unexported and derivable only from a WHOLE LAYOUT (values.go:297-357), while GetPrimaryKeyColumns(recordType string) holds only a type NAME, so an identity minted there would carry the METADATA layout's domain while every consumer checks against the FLOWED leg row; where those differ OrdinalIn fails closed and the plan is LOST. Java agrees with the CURRENT Go placement rather than with the prescription: ImplementIntersectionRule.java:98 takes getComparisonKeyProvidedOrderingParts(), keys resolved from the LEGS' ordering (OrderingPart.java:609-616), never minted from metadata. BLAST RADIUS corrected -- the prior list omitted rule_implement_nested_loop_join.go:4400, rule_ordered_primary_scan.go:55 and rule_primary_scan.go:50, and two things live there: (a) rule_primary_scan.go:50-56 is a SECOND producer minting FieldValue{Field: col} from pkCols WITHOUT the strings.ToUpper that commonPrimaryKeyValues applies, and lazy FieldValue equality is case-SENSITIVE (map_field_values.go:354, av.Field == bv.Field), so the two producers can mint UNEQUAL Values for one column -- tracked as its own TODO item because that is an inconsistency to fix, not documentation; (b) rule_ordered_primary_scan.go consumes pkCols as GENUINE NAMES for path matching (AccessorNamePathMatchesNames, ColumnCanExtendOrderingClaim) with no identity equivalent, which may be legitimate rather than debt and should be classified before it is converted. UNVERIFIED, inherited: the 'ZERO hits over ./pkg/relational/... and 8 real hits through the record-layer intersection path' traffic counts were NOT reproduced -- only the MECHANISM behind the zero was confirmed (builtPlanContext.GetPrimaryKeyColumns returns nil, plan_context_builder.go:282). Treat those two numbers as unmeasured; passing them along as established is the inheritance chain that produced the wrong prescription above. Newly visible through the call boundary"},
	"pkg/relational/core/query/unnest_gather.go # slotInGatheredSeed # a map key via local col derived from the name # 1":                                {1, "name-keyed: slotInGatheredSeed's ELEMENT arm keys elementSlots by leaf name, reached with a col parameter the caller derives from fv.Field. NAMING IT 'THE BARE ARM' IS WHAT THIS ENTRY USED TO DO, AND THAT DESCRIPTION HID A LIVE WRONG-COLUMN READ, so the correction is kept visible rather than quietly rewritten: the arm was NOT gated on qualified and therefore served BOTH namespaces. A QUALIFIED reference that failed the leg lookup -- a correlation absent from the window map, a LegKindNested or LegKindUnset window, or a flat window declaring the column TWICE (newly reachable once the first-match FieldIndex was deleted, since a clustered box run's leg type is a leg-concat that may legitimately repeat a leaf name) -- fell straight through into this map, so U.V read the ELEMENT's V. The neighbouring !qualified guard on the bare-LEG scan WAS real, which is precisely why reading it as this arm's guard too was so easy, and the gap between the two was the defect. The arm is bare-only now: slotInGatheredSeed declines every unresolved qualified read before either bare arm, pinned by TestLegKind_GatheredGroupSlotDeclinesWhatItCannotAddress (all four decline shapes, driven against a POPULATED element map -- the pin used to pass an EMPTY one, which collapses the decline and the leak onto the same (0,false) and therefore asserted nothing) and by TestLegKind_GatheredGroupSlotStillServesTheBareNamespace (the over-decline control). The element-first shadowing rule it implements is a NAME-precedence rule (an element alias shadows a later leg column), so the map key is the shadowing decision itself. RE-COUNTED over ./pkg/relational/... (27 of 27 targets, --nocache_test_results): the arm takes 2 hits and BOTH ARE BARE -- zero qualified reads reach it, so the decline removed no traffic that exists today. One hit is production (TestFDB_UnnestExistsGather/agg_cte_distinct_groupby_element, a GROUP BY on a CTE-qualified column whose qualifier is stripped upstream and which therefore arrives bare), one is a unit probe. NOT RE-VERIFIED: the prior claim that 2 OF 2 are cases where the same bare name ALSO resolves in a flat leg window. The hit COUNT reproduced exactly; that collision sub-claim was not measured here and must not be passed along as established. Java has no analogue to convert toward at this site because Java never builds the flat row: element attributes are FieldValue.ofOrdinalNumber on the Explode's OWN quantifier (LogicalOperator.java:306-332) and RecordQueryFlatMapPlan.executePlan binds outer and inner per-alias on a chained context (RecordQueryFlatMapPlan.java:135-140) -- which is also why the qualified decline is the Java-FAITHFUL answer rather than a Go invention: with every quantifier bound under its own alias there is no shared namespace for a qualified read to fall into. So this closes when the gathered seed carries element identity rather than an element name -- the same per-alias-binding capability CQ-53 owns, not a rewrite of this arm. Newly visible through the call boundary"},

	"pkg/recordlayer/query/plan/cascades/referenced_fields.go # collectFieldNamesFromValue # a map key # 1":        {1, "name-keyed: referenced-field set keyed by leaf name. SEVERITY IS BOUNDED, and this is the fact the entry was missing: NOTHING IN PRODUCTION READS THE SET'S CONTENTS. Contains/Size/Fields have exactly two callers, both tests, and they now live in referenced_fields_readers_test.go so a production reader cannot appear without breaking the build (that file's header is the pin, and says to correct THIS entry when it moves). Java is the same shape -- combine() only reports whether the union GREW (ReferencedFieldsConstraint.java:62-66), and the sole non-push-rule mention of REFERENCED_FIELDS is AbstractDataAccessRule.java:122 listing it as a rule DEPENDENCY, never calling getReferencedFieldValues. So membership decides one thing only: whether the constraint grew, which re-fires exploration. A leaf-name collision merges two entries, so it can only stop growth SOONER and end exploration EARLIER -- a possibly-missed alternative plan, never a wrong one. That is why this is an open STOP and not the bucket's headline wrong-rows shape. Java's member is a Set<FieldValue> (ReferencedFieldsConstraint.java:41), keyed by semanticEquals/semanticHashCode, so the port is unambiguous -- and it was BUILT and MEASURED, then reverted: keying by value makes the set grow where two quantifiers share a leaf name, and this constraint's every growth re-fires the push rules (Go's coupling is an exact port of CascadesRuleCall.java:177-185). 4-table chain tasksRun 10255 -> 12901, 3-spoke ordinal star 9481 -> 12644 (both budget baselines are +-2% pins), and the hub+5 star stops planning entirely -- ErrPlannerCapHit becomes a rule-cycle round-cap divergence at 87642 tasks. plan_shape.golden does not move. The conversion is correct and the coupling between constraint growth and re-exploration is what has to change first; that is planner machinery with its own review gate, not this bucket. RETIREMENT CONDITION, made explicit because the analysis above names the blocker without naming the exit: retires when ReferencedFieldsConstraint's member set is keyed by VALUE (Java's Set<FieldValue> with semanticEquals/semanticHashCode, ReferencedFieldsConstraint.java:41) AND constraint growth no longer re-fires the push rules unconditionally. Both halves are required and the order is fixed -- the value-keyed port was BUILT and MEASURED and had to be reverted, because keying by value grows the set where two quantifiers share a leaf and every growth re-fires exploration: 4-table chain 10255 to 12901 tasks, 3-spoke ordinal star 9481 to 12644, hub+5 star stops planning entirely at 87642 tasks. So the falsifiable condition is on the COUPLING, not on this site: retires when the constraint can grow without re-firing exploration, at which point the value-keyed set lands without a task-count regression"},
	"pkg/recordlayer/query/plan/cascades/values/map_field_values.go # EqualsWithoutChildren # a == comparison # 1": {1, "name-keyed: EqualsWithoutChildren's LAZY-vs-LAZY arm. COUNTED, not panicked -- the panic prose this reason used to carry ('a panic on the true branch reds the sqldriver FDB suite immediately') is exactly the evidence that made the projection-merge entry beside it wrong for a whole lap: a panic proves a program point is REACHABLE, once, and destroys the run that would have said how often or through which arm. The counter, over ./pkg/relational/... (FDB sqldriver, all four conformance corpora, yamsql, rowdiff): 96496 lazy-vs-lazy comparisons, 60926 TRUE and 35570 FALSE; ./pkg/recordlayer/... adds 4962. So it is load-bearing for memo dedup of lazy carriers, and now on a number rather than a red. It is also the one site in this bucket with NO Java counterpart to port: Java's FieldValue is resolved at construction (FieldValue.java:273-299) and its equalsWithoutChildren is fieldPath-only (FieldValue.java:213-217), so a lazy name carrier is a shape Java cannot express, and for such a carrier the pending name IS the whole identity. Failing it closed makes two lazy references to one column unequal, which un-interns memo members rather than fixing a conflation. This closes when lazy FieldValues stop being minted, not before"},

	// translator (12)
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
	// The four :5694/:5742/:5866/exists:131 entries arrived from the dotted
	// bucket (see the note there). They are the QUALIFIER halves of identifiers
	// whose leaf halves are already here; splitting one parsed identifier across
	// two buckets was the misfiling, and the upstream fix that retires all four
	// at once is CQ-52 — the parser already produces the segments and joins them
	// only for the resolver to split them back.
	//
	// THE 42702 PREMISE, which several first-match entries in this bucket rest
	// on. It is TWO-THIRDS TRUE and a conversion is in flight, so it must never
	// be written as a blanket fact — an entry citing it has to say WHICH HALF it
	// rests on, and the two halves have very different standing.
	//
	//   - BARE ambiguity IS rejected upstream, at the semantic layer, with
	//     SQLSTATE 42702. The authority is semantic.Scope.ResolveColumn
	//     (core/query/semantic/scope.go:258-275): more than one match is
	//     terminal, minting *AmbiguousColumnError rather than picking one. It
	//     surfaces as 42702 through mapPredicateWalkError
	//     (logical_predicate.go:687-691) and mapColumnResolveError (:3460-3468),
	//     with the qualified twin at scope.go:375-388. So for a BARE reference,
	//     "the first match is the only match" is an upstream guarantee and the
	//     first-match sites are honest debt rather than latent wrong-column reads.
	//
	//   - DERIVED-TABLE ambiguity is NOT rejected there. `SELECT d.k FROM
	//     (SELECT x.k, y.k …) AS d` and its relatives reach the planner and die
	//     downstream, by accident rather than by a guard — an ordinal that will
	//     not resolve, or a plain refusal to plan. A fix converting these to a
	//     real 42702 is being implemented on break/228-delete-name-lookup. Any
	//     entry leaning on THIS half is leaning on an accident, and on one that
	//     is about to be replaced.
	//
	// The pin for both halves is
	// pkg/relational/sqldriver/ambiguous_column_ref_rejected_fdb_test.go, whose
	// unambiguous-shapes block exists so the rejection block cannot pass by
	// refusing everything. NOT MEASURED HERE, and said plainly rather than
	// implied: the derived-table half was not re-run from this worktree (the test
	// lives on the in-flight branch), so its current failure MODE is taken from
	// that branch's own audit and not from a run recorded here. What WAS checked
	// statically is weak corroboration only — the set of ErrCodeAmbiguousColumn
	// producing sites is the same on both branches, which is consistent with the
	// fix not having landed but does not prove it, since a fix could widen an
	// existing site instead of adding one.
	//
	// A third thing this premise does NOT cover, because it is about columns:
	// a duplicate QUALIFIER. See the legWindowSlot entry below.
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a EqualFold call # 1": {1, "translator: parsed column ref matched against declared inner columns -- the EqualFold that picks which of the leg's derived columns supplies TypeName/Nullable when the ordinal arm did not fire. RETIREMENT CONDITION: retires when the `if !inherited` fallback guarding it is UNREACHABLE -- when every QOV-childed projected FieldValue satisfies fv.Resolved != nil && len(fv.Resolved.Accessors) == 1 with an in-range ordinal, so the ordinal arm immediately above is total. Falsify by finding a projected FieldValue over a QOV that reaches the fallback"},
	// CQ-52 converted the PROJECTION channel: LogicalProject now carries the
	// parser's segment triple beside Projections, so a projected reference
	// arrives at these bakers already segmented and nothing slices its name.
	// Measured over the real-FDB sqldriver corpus, the dotted re-split fell from
	// 110 calls to 3 — those three in the sort-key and aggregate-operand
	// channels — and then to ZERO when the remaining parsed channels (ORDER BY
	// keys, GROUP BY keys, aggregate operands) carried their segments too. These
	// numbers are quoted OUT of here into RFC-197 and road-to-prod.md, which is
	// exactly why the intermediate figure could not be left standing as the
	// current one.
	//
	// The re-split arms survive that zero on purpose: they serve carriers that
	// state NO segments, and one class of those — a projection machinery mints,
	// named for body output columns with no parse tree behind them — never can.
	// That class is STOPPED on an open behaviour question rather than converted
	// by guesswork; the question is stated at the arm in cascades_translator.go.
	//
	// The FIVE entries this replaces (:5769/:5796/:5839/:5998/:6028) are NOT all
	// retirements. Two of the three qualifier slices CONSOLIDATED into one
	// (`segmentsOf` serves both arms of the leg baker, and takes the segments
	// when they are present). The other three moved BEHIND A PLAIN STRING
	// PARAMETER — bakeFlatRefsAgainstColumns' leg walk is now
	// `legWindowSlot(qual, leaf, ...)`.
	//
	// That move made them INVISIBLE, and the invisibility was the point of
	// closing the blind spot: the call-boundary taint now follows a name into a
	// helper's parameter, so :6070 and :6102 below are those comparisons, back
	// on the ratchet where they belong. A refactor can no longer walk this count
	// down without changing a single decision.
	"pkg/relational/core/query/cascades_translator.go # bakeDottedRefsToLegQOVWithRef # the name escaping as a bare string (return) # 1":      {1, "translator: LEAF of an identifier escaping as a bare string from the leg baker's segmentsOf (:6284), consumed by name at the two EqualFold column walks it feeds -- the single-ForEach flat arm (:6382) and legBake (:6404), both reached via :6361/:6420. THE RE-SPLIT THIS ENTRY USED TO DESCRIBE IS GONE, and the old text is not merely stale but actively misleading, so it is replaced rather than annotated: it said the arm 'still re-splits rendered names at every node below its root', that a compound converted projection (`a.b + c.d`, a CASE) was one query away from arming it, and that the entry 'retires when the remaining LogicalProject producers carry ProjectionRefs'. None of that describes the code now. segmentsOf no longer slices anything: qualification is the parser's segment count and a carrier with no segments is UNQUALIFIED, so the non-root path returns the field verbatim as the leaf and never manufactures a qualifier. Its QUALIFIER sibling (entry # 2) and both legWindowSlot entries retired with that change; this one did not, and forcing it would have been wrong. WHAT SURVIVES IS A DIFFERENT AXIS: the leaf is still a STRING, and the column it selects is still chosen by comparing that string against a column-name list case-insensitively, first match. That is name-keyed column resolution, not qualifier recovery -- the two were entangled in one entry while the split existed and are now cleanly separable. WHAT CLOSES IT: a LAYOUT-RESOLVED COLUMN CONTRACT -- the leg carrying resolved column identities so the leaf selects a slot by identity instead of by text -- NOT a segment triple, which this site already honours and which does nothing for the leaf half. That is the same capability rule_implement_in_union.go # uniqueUpperFieldIndex is blocked on, and it is the reason these two should be closed together rather than one at a time. The first-match rests on the BARE half of the 42702 premise noted below this bucket, with that premise's standing caveat: 42702 covers the REFERENCE being ambiguous, not the column list itself declaring one name twice. Guarded by `Child != nil || Resolved != nil -> bail` at the caller, and every match downstream emits a born-baked ordinal, so a miss stays lazy and is loud at evaluation rather than silently wrong"},
	"pkg/relational/core/query/cascades_translator.go # bakeSegmentedColumnRef # a EqualFold call # 1":                                        {1, "translator: bakeSegmentedColumnRef's exact first-match of the SEGMENTED carrier's rendered name against the output column list (:6449-6453) -- the name is still what selects the slot, so it is still debt, but it is no longer the name that decides QUALIFICATION: that comes from the parser's segment count. Same resolve-then-bake shape as bakeFlatRefsAgainstColumns' exact match (:6546) -- both walk `cols`, take the first case-insensitive hit, and bake into OrdinalDomainOfColumnNames(cols) -- on the converted path. The old text pointed at a bare `6130`; nothing at that line answers to the description and the referent could not be recovered, so the sibling is named by SYMBOL, which cannot rot silently the way a number does. WHAT THE FIRST-MATCH RESTS ON: the BARE half of the 42702 premise noted below this bucket, and only that half. A bare reference carried by two visible sources is refused upstream, so the first hit is the only hit -- but that guarantee is about the REFERENCE being unambiguous, not about `cols` being duplicate-free, and a leg-concat row legitimately declares one name twice. RETIREMENT CONDITION: retires when logical.ColumnRef carries a resolved Ordinal int -- the slot the semantic scope ALREADY computed when it built the Present/Qualified/Qualifier/Bare triple -- and this function returns the baked ordinal from ref.Ordinal without scanning cols at all. The pass-through guard at the top already fences resolved carriers, so the entire retirement rests on ColumnRef gaining that one field. Falsify by finding a segmented carrier whose ordinal the semantic scope cannot supply"},
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a map key # 1":                                        {1, "translator: same inner-column lookup as the sibling entry above, leg-qualified arm -- the map key is a CONCATENATION, which is why that sibling was recorded and this one was not. RETIREMENT CONDITION: retires when innerByName stops being a map keyed by the flattened \"LEG.COL\" string and becomes a map[values.CorrelationIdentifier][]executor.ColumnDef addressed by leg plus ordinal, so no qualified string key is ever composed to read it. Falsify by finding a consumer of the inner column list that still addresses it by a composed name"},
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a map key # 2":                                        {1, "translator: inner-column lookup by parsed name (laundered map key) -- the CHILDLESS read's type inheritance, bare-keyed. RETIREMENT CONDITION: retires when the `if !inherited` guarding it is dead, i.e. when the childless ordinal arm above is TOTAL: every childless projected FieldValue reaching this function carries a Resolved with one accessor indexing innerCols. Falsify by finding a childless projected FieldValue that reaches the bare map read"},
	"pkg/relational/core/embedded/logical_predicate.go # validateUnionOrderByColumns # a map key # 1":                                         {1, "translator: NOT a join-side set -- that description was wrong twice over. It is the UNION LEFT BRANCH output-name set: `leftNames`, seeded at logical_predicate.go:7774-7789 from findProjection(leftBranch), where leftBranch is the union's left leg. The second error is about WHICH LINE DECIDES. What the gate flags is the WRITE at :7783, `leftNames[strings.ToUpper(fv.Field)] = true` -- the only operand in the whole function derived from FieldValue.Field -- and a write merely SEEDS the set; it resolves nothing. The actual name-keyed DECISION is the READ at :7810, `!leftNames[upper] && !leftNames[bareName]`, and this gate CANNOT SEE IT: both operands derive from logical.SortKey's Expr/Bare, not from FieldValue.Field, so no taint reaches them. The recorded site and the deciding site are different lines and the ratchet is watching the cheaper one. CONSEQUENCE CLASS, stated because it is what bounds this entry: the read's only outcomes are `api.ErrCodeUndefinedColumn` (42703) or silence, so a miscall is a spurious or a missed error on an ORDER BY key over a UNION -- validation only, never a wrong-column read. POSITIONAL VALIDATION IS REFUTED AS THE FIX, and the refutation is Java's. Java has NO union-level ORDER BY AT ALL: RelationalParser.g4:428-431 attaches orderByClause to `queryTerm`/`simpleTable` (:521-534) and NOT to the `setQuery` alternative, and `parenthesisQuery` is `'(' query ')'` with nothing following, so `(A UNION B) ORDER BY x` does not parse either. QueryVisitor.visitSetQuery (:345-356) contains zero ORDER BY handling; in `A UNION ALL B ORDER BY x` the ORDER BY is parsed INTO B and resolved against B's own select list (:313-315). Where Java resolves such a key it does so BY NAME -- SemanticAnalyzer.lookupAlias (:513-536) is `Identifier.equals` on the display name, with a miss falling through to resolveIdentifier, never an ordinal match against a branch output. So Go's whole union-level check is a Go-side extension with no Java spec to port, and the name-keyed READ is the SQL-correct shape: `ORDER BY <name>` over a UNION legitimately NAMES a result column, and the only positional form -- `ORDER BY <integer>` -- is already handled by the `k.Pos > 0` skip above the read. A rewrite to position/identity would have to invent a resolution Java does not have and SQL does not ask for. What this entry actually waits on is logical.SortKey carrying a RESOLVED Value beside its Expr string, the way Java's OrderByExpression wraps an Expression with an underlying Value (Expression.java:69-85, OrderByExpression.java:39-49); until the logical layer carries that identity there is nothing for either the seed or the read to key on but the name. Deleting the seed at :8025 alone is refused deliberately: it would buy a green ratchet, would narrow the accepted name set, and would leave the deciding READ untouched"},
	"pkg/relational/core/query/cascades_translator.go # rewriteUnnestPredicate # a EqualFold call # 1":                                        {1, "translator: unnest element alias resolution, flat arm; the sibling arm consults the ordinal. THIS ARM IS REFUTED BY ITS OWN TWIN, and that is the retirement condition rather than a separate capability: the very next arm handles struct MEMBERS of the same virtual source and explicitly REFUSES to compare names, on the stated ground that the EXISTS scope's unnest source is a ONE-COLUMN virtual table so slot 0 IS the element, a name comparison on Accessors[0].Field would add no discrimination, and it would make a DISPLAY name decide a binding. This arm is exactly that comparison, on the same source, under the same premise. RETIREMENT CONDITION: retires when the childless single-accessor arm drops the strings.EqualFold(fv.Field, asAlias) conjunct and gates on `u.CorrelatedCollection != nil && !withOrdinality && fv.SourceRelativeBaked() && fv.Resolved.Accessors[0].Ordinal == 0` -- the identical gate its sibling already uses. Falsify by showing the two arms address different sources"},
	"pkg/relational/core/query/cascades_translator.go # bakeUnnestElementRefOrdinal # a map key via local rootName derived from the name # 1": {1, "translator: element slot lookup during translation (laundered map key). MOVED, NOT PAID, and the move is worth reading because it SHRANK the debt without retiring it. The lookup used to key on `fv.Field` directly; it now keys on `fv.Resolved.Root().Field` whenever the node is BAKED, so every reference carrying a resolved path decides by the path's ROOT rather than by a display name -- which is what lets a MULTI-ACCESSOR member reference (`x.ek`) find its element slot at all, instead of missing on the MEMBER's own name and being skipped into a silent zero-row EXISTS. What survives is the LAZY arm: a FieldValue with Resolved == nil has no path to consult and `fv.Field` is the only name in existence for it, so the fallback remains. This site therefore closes exactly when the element-slot lookup can REQUIRE a resolved path -- i.e. when nothing reaching bakeUnnestElementRefOrdinal is still lazy -- and not by renaming anything"},
	"pkg/relational/core/query/cascades_translator.go # rewriteUnnestPredicate # a switch tag # 1":                                            {1, "translator: unnest element/ordinality selection by declared alias, qualified arm (laundered switch tag). RETIREMENT CONDITION: retires when logical.LogicalUnnest RECORDS the element and ordinality slot indices -- 0 and 1, the numbers this switch already hard-codes in its OUTPUTS -- and the resolver bakes the element/ordinality aliases to those ordinals at translation, so the switch becomes `switch fv.Resolved.Accessors[0].Ordinal` and the declared aliases are never upper-cased for comparison. The tell that this is a rename of an existing fact rather than new machinery: the arm already knows the ordinals, it just refuses to read them from the reference. Falsify by finding an unnest whose element slot is not 0"},
	"pkg/relational/core/query/cascades_translator.go # pullUpSortKeyValue # a EqualFold call # 1":                                            {1, "translator: pullUpSortKeyValue resolves a bare ORDER BY key against the FOLDED projection's output fields -- guarded by `Child == nil && Resolved == nil` at :5481 (the entry said 5210, stale), so the key side is a lazy carrier from parsed text, and a match emits NewFieldValueWithResolvedOrdinal. Gated on cascades_translator.go:4715 (the nonempty `src.sortKeyName(k)` guard -- re-derived, still accurate), NOT on ProjectionColumnName, which is values.go:1710; the entry's `values.go:1510` is stale and that line now holds OrdinalBakeError.Error. This site's `fields` are named from p.Projections/p.Aliases (parser text) or a positional `_i`, and the ONLY .Field-derived names in them are the hidden sort columns collectExtraSortColumns appends -- a contract-bucket entry. (STALE SINCE RFC-227 and corrected here rather than silently: those columns are no longer named by sortKeyFieldRef's strings.ToUpper(fv.Field) in every case. sortKeyExtraColumnName now names a NESTED key by its resolved PATH, because the flat root spelling was the dedup identity and two members of one struct root collapsed onto it. A FLAT key still takes the qualified field reference, so this site's `fields` still carry .Field-derived names and the entry still stands.) Converting ProjectionColumnName leaves this one red. RETRACTED AS FALSE, and the retraction is kept visible because the prescription it carried would have shipped a user-visible regression for zero debt payoff: the prior text said this site's gate IS removable by naming the hidden columns positionally, on the reasoning that doing so leaves `fields` with no .Field-derived names. That reasoning reads the WRONG SIDE of the comparison. The gate is a STATIC AST scan and what it flags here is `fv.Field` -- the KEY side of `strings.EqualFold(f.Name, fv.Field)`. The `fields` side is `f.Name`, a RecordConstructorField.Name, which isFieldSelector never taints; no runtime name landing in `fields` is visible to the scan at all. MEASURED both ways rather than predicted: with the hidden columns named positionally the gate stays GREEN with this entry INTACT (the site is still reported, count 1), and DELETING the entry fails TestFieldNameNeverDecides with `pullUpSortKeyValue # a EqualFold call # 1: a EqualFold call uses FieldValue.Field`. The conversion therefore retires NOTHING -- not this entry and not sortKeyFieldRef's contract-bucket one, whose escape (a name returned for sortKeyName, sortKeySourceValue, and this collector's own dedup key and nameability check) survives the rename untouched. ALSO MEASURED and load-bearing against the same prescription: a hidden column's name is NOT plan-internal. It is the DISPLAY name the re-applied sort renders, so it reaches EXPLAIN -- naming the columns positionally moves 5 lines of testdata/plan_shape.golden (every projected_exists_nested_sort_key.yaml stanza, e.g. `InMemorySort([N ASC]` and `InMemorySort([T1.N.SK ASC]` becoming a positional token), degrading EXPLAIN from naming the sorted column to naming a slot. What IS true, and is now PINNED rather than left as prose (hidden_sort_column_value_recovery_test.go): the hidden columns are recovered by VALUE, not by this name scan -- an appended column named a string no scan can find still resolves, and a key whose leaf name matches a DECOY output column holding a different value still resolves to its source column. The real conversion for this site is on the KEY side: the guard `Child == nil && Resolved == nil` is what selects a lazy text-derived carrier, so the site dies when ORDER BY keys arrive already resolved, never by renaming what they are compared against"},
	"pkg/relational/core/query/cascades_translator.go # bakeFlatRefsAgainstColumns # a EqualFold call # 1":                                    {1, "translator: column list membership during resolution -- bakeFlatRefsAgainstColumns' exact first-match, CURRENTLY :6546. The entry carried only `:5982`, its pre-CQ-52 line, and never acquired a current one; the historical fact (the leg walk moved out into legWindowSlot) is kept, the dead number is not. WHAT THE FIRST-MATCH RESTS ON: the BARE half of the 42702 premise noted below this bucket, with the same caveat as bakeSegmentedColumnRef -- `cols` is a FLAT row that a leg-concat can legitimately fill with one name twice, and the upstream rejection covers the REFERENCE being ambiguous, not the row being duplicate-named. RETIREMENT CONDITION: retires when no SEGMENT-LESS carrier is produced at all, so this scan is unreachable and the segmented sibling handles every arrival. The producers are named at the site rather than guessed at -- the logical builder's two channels and the post-aggregate strip projection -- and the condition is that each carries a logical.ColumnRef with Present: true. Checkable exactly as the site already states it: the flat-column-bake name-split census reports 0. Falsify by finding a fourth producer of a segment-less carrier"},
	"pkg/relational/core/query/cascades_translator.go # ordinalSlotInLegWindow # a == comparison via local field derived from the name # 1":   {1, "translator: ordinalSlotInLegWindow scans the selected leg's window for the field NAME, after selecting the leg by IDENTITY (values.SameLeg on the window's Alias, :3868 -- the entry said :3697, stale). Newly visible through the call boundary. Half-migrated by construction and worth reading as such: the QUANTIFIER side is already identity-keyed, the COLUMN side is still a name -- which is the same split the leg census reports from the reader side, and it closes with the column domain, not with the leg table. RETIREMENT CONDITION, stated as a checkable property: retires when the leg window carries a resolved column DOMAIN the reference can be matched against by ordinal -- when ordinalSlotInLegWindow selects the slot by fv.Resolved's accessor against the window's declared layout rather than scanning window entries for the field NAME. The quantifier half is already identity-keyed (values.SameLeg on the window's Alias), so this is the column half of a half-migrated site and nothing about the leg table changes. Falsify by finding a leg window whose columns cannot state an ordinal domain"},

	"pkg/recordlayer/query/plan/cascades/values/values.go # assertSuffixStep # a == comparison # 1":             {1, "contract: the sibling of ReAnchorRootInto above, one step further down. FuseNestedSuffix asserts a nested suffix accessor against the record type it descends into by matching the accessor's per-step NAME, because the question being asked is 'which slot of THIS record does this named step denote' and only that record's field names can answer it -- Resolved is what is being read, not what is missing. Two things keep it from being the conflation this ratchet exists to catch. It DECLINES on a duplicate name rather than first-matching, so the failure is a refused fold and never a wrong-column read. And the comparison is a TRIPWIRE, not the source: the suffix ordinals are CARRIED, because a suffix indexes a struct's own declared field order which no merge restates -- this arm only fires when a record type happens to be available, and on the production path it is not (the positional merge states UNKNOWN for a struct column). Retires with the merged-layout struct-typing work booked in TODO.md, at which point the assert arm becomes the live one and the question of leg provenance arrives with it"},
	"pkg/recordlayer/query/plan/cascades/values/values.go # (FieldPath).ReAnchorRootInto # a == comparison # 1": {1, "contract: the nested-key re-anchor derives a root ordinal by matching the carried root accessor NAME against the flowed layout (RFC-218). It is debt by construction and the RFC says so: the correct discriminator is leg IDENTITY -- which correlation the root belongs to, against which leg each merged slot came from -- and the name is a stand-in until a caller supplies it. What keeps it honest meanwhile is that it DECLINES on a duplicate name rather than first-matching, so the failure is a refused fold and never a wrong-column read. The old text justified that with a counterfactual -- 'RecordType.FieldIndex would have first-matched' -- which is now DEAD: RecordType.FieldIndex and RecordType.LookupField were deleted outright, and FieldIndexUnique / LookupFieldUnique (type.go:827, :847) are the only name resolvers left in the package, so there is no longer a first-matching form for this site to be contrasted against. The CONCLUSION survives untouched and is restated on what actually holds it up: the decline is real -- ReAnchorRootInto returns a reason and refuses rather than resolving when the root name is absent or DUPLICATED in `flowed` -- and declining is now the only behaviour the API offers, not the better of two available ones. Retires when the merged layout carries per-slot leg provenance the re-anchor can match on"},

	// harness (1)
	"pkg/relational/conformance/rowdiff/ordering.go # sortKeysMatchOrderBy # a EqualFold call # 1": {1, "harness: conformance oracle matches a plan sort key against an ORDER BY key so it can tell WHICH sort a node is. AUDITED, and the audit refused the allowlist on its own evidence. The reason this entry used to carry -- 'compares plan sort keys to SQL ORDER BY text', so the name is the identity and engine rules do not apply -- was false in the half that mattered: an OrderKey renders as `ToLower(Qual) + \".\" + Col` (gen.go:1753), so the compared Col is a FRAGMENT of that text, and the generator emits `ORDER BY l.id, r.id` and `l.id, m.id, r.id` -- key vectors whose leaf names are all ID. One plan-key vector matched two DIFFERENT qualified orderings; the guard now REFUSES a qualified key and a unit pin drives both the conflation and the two caller fences (singleTablePlain, requestedOrdering) that kept it latent. It stays debt because what remains is contingent, not an identity: the leaf comparison is sound only while the generator's schema has no duplicate leaf name, which is a property of test data. MEASURED at 250 seeds: 952 sorts, 952 guard matches, 0 rejections -- the guard never fires in the corpus, and nothing censused that until the sweep gained a guard-match floor. TWO CORRECTIONS, both found by reading the site rather than the entry. (1) This is NOT a values.FieldValue.Field read: keys[i] is a plans.SortKey and its .Field is a plan-level string (in_memory_sort.go:26). The gate reports it because the walk matches the SELECTOR, not the type, so the entry is filed under an authority it does not actually belong to -- worth knowing before anyone converts it expecting FieldValue machinery to be involved. (2) The site is ALREADY CONVERTED on the axis that mattered: it flatly REFUSES any qualified ORDER BY key rather than matching on its leaf, which is what kept `ORDER BY l.id DESC, r.id` from matching `r.id DESC, l.id` when both have leaf vector [ID, ID]. What remains is a leaf-vs-leaf comparison over the unqualified single-table population, where the leaf IS the whole name and so decides nothing the name cannot decide. RETIREMENT CONDITION: retires when sortKeysMatchOrderBy compares plans.SortKey.ValueExpr -- documented REQUIRED and plan-time baked (in_memory_sort.go:29) -- against the ORDER BY key's own baked Value or projection ordinal, and neither SortKey.Field nor OrderKey.Col is read in this function. Falsify by finding a sort key whose ValueExpr is nil at this point. LOWEST VALUE OF THE LIST and recorded as such: the ambiguous population is already fenced out, so this buys tidiness rather than correctness"},
}

// fieldDebtBuckets is the RFC-197 migration partition, and the ONE place the seven
// bucket names are written down. Every other form of them — the reason-tag prefix,
// the group-header pattern, the completeness sweeps here and on the status page — is
// derived from this slice.
//
// It is one authority because it was three. The names were spelled out separately in
// the tag regexp, the header regexp and the header-completeness loop, which is the
// same "two authorities on one fact" pathology this whole workstream exists to end,
// sitting in the gate that enforces it: adding a bucket to two of the three would
// have produced a list the tag matcher accepted and the completeness sweep never
// asked about.
var fieldDebtBuckets = []string{"boundary", "escape", "contract", "dotted", "name-keyed", "translator", "harness"}

// fieldDebtBucketAlternation renders the buckets as a regexp alternation group.
func fieldDebtBucketAlternation() string {
	return `(` + strings.Join(fieldDebtBuckets, `|`) + `)`
}

// fieldDebtBucketTag is the mandatory prefix on every knownFieldDecisionDebt
// reason. The seven buckets are the migration partition (RFC-197): a site has
// exactly ONE owning bucket, so the per-bucket counts sum to the list.
var fieldDebtBucketTag = regexp.MustCompile(`^` + fieldDebtBucketAlternation() + `: `)

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

// THE LIST RECORDS ESCAPES; THE REPORT LEADS WITH AUTHORITIES. Two numbers over
// one key set, answering two different questions, and neither replaces the other.
//
//	an ESCAPE   is one site where a name can leave typed context — one entry.
//	an AUTHORITY is the declaration that owns it — `file.go # declaration`.
//
// Currently 47 escapes across 34 authorities. The gap is real concentration
// rather than noise: three authorities carry 12 of the 46 (groupByOutputBaker 5,
// deriveColumnsFromProjection 4, explainValueOrdinals 3) and the other thirty sit
// near 1:1.
//
// The concentration is also where the retirements come from, and the last one is
// the argument for keeping both numbers: AggregateResultColumnName's six arms
// were the largest single authority and they retired TOGETHER, on one deleted
// line, because six escapes shared one taint source. Six off the escape count,
// one off the authority count — which is exactly what "a fix lands on a
// declaration" predicts, and what a list collapsed to authorities could not have
// shown was six holes rather than one.
//
// WHY BOTH, and why the list is not collapsed to authorities:
//
//   - "Where can a name leave typed context?" is ESCAPES, and it must stay
//     per-site. Fix five of six return arms in one switch and the sixth is a live
//     hole; an authority-level entry would report it as retired.
//   - "How much work remains?" is AUTHORITIES, because a fix lands on a
//     declaration, not on a line.
//
// Collapsing the LIST to authorities would also re-open, one level up, exactly
// the hole TestFieldDecisionAllowlistIsPerSite exists to close: an entry that
// covers every escape its declaration grows later, reading like an exemption
// while granting none.
//
// Both are derived here from the SAME keys, which is only possible because the
// declaration is a first-class segment of the site key rather than something
// recoverable from a line number.
//
// BUCKET IS NOT FORM, and anyone deriving per-bucket numbers must key on the
// bucket tag in the `why` string rather than on the key's form segment. A bucket
// is an EDITORIAL statement of why the debt exists; a form is a MECHANICAL
// statement of how the walk detected it, and they legitimately disagree —
// values.go's `explainValueOrdinals` MINT escape is filed under `contract` while
// being reported by the arm that names the `dotted` bucket. Keying per-bucket
// counts on the form segment would "fix" that by moving a correctly-filed entry.
// fieldDebtAuthorityTotal is the DECLARED number of distinct authorities, held
// beside the list the way the per-bucket group headers hold the entry counts and
// asserted the same way.
//
// A derived number that nothing claims is a number that can move without anyone
// deciding it should. The entry count has had that protection since the group
// headers were introduced; the authority count is the figure that now LEADS the
// report, so it needs it more, not less. Changing this constant is how a change
// to the authority count becomes deliberate.
const fieldDebtAuthorityTotal = 33

func bucketAuthorityCounts(m map[string]fieldDebt) map[string]int {
	perBucket := map[string]map[string]struct{}{}
	for site, d := range m {
		bucket, ok := bucketTagOf(d.why)
		if !ok {
			continue // untagged entries are bucketCounts' finding, not this one's
		}
		if perBucket[bucket] == nil {
			perBucket[bucket] = map[string]struct{}{}
		}
		perBucket[bucket][fieldDecisionAuthorityOf(site)] = struct{}{}
	}
	counts := map[string]int{}
	for bucket, set := range perBucket {
		counts[bucket] = len(set)
	}
	return counts
}

// fieldDecisionAuthorityOf projects a site key onto its owning declaration —
// `path/file.go # declaration`, the first two of the key's four segments.
//
// A key that does not have the expected shape is returned whole rather than
// silently truncated: an unparseable key must show up as its own authority and
// be visible, never merge into another one's count.
func fieldDecisionAuthorityOf(site string) string {
	parts := strings.Split(site, " # ")
	if len(parts) < 2 {
		return site
	}
	return parts[0] + " # " + parts[1]
}

// bucketHeaderPattern matches a group-header comment: `// <bucket> (N)` at the
// start of the comment's own text. WHERE the comment sits is decided
// structurally rather than by indentation — see bucketHeaderCounts.
var bucketHeaderPattern = regexp.MustCompile(`^// ` + fieldDebtBucketAlternation() + ` \((\d+)\)`)

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
	for _, b := range fieldDebtBuckets {
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
		// The shape is `path/file.go # declaration # form # ordinal`. What the
		// check is really enforcing has not changed: an entry must name ONE SITE,
		// because a whole-file exemption covers every decision the file grows
		// later, silently and for free. Only the spelling of a site moved, from a
		// line number to a stable identity.
		parts := strings.Split(a.site, " # ")
		file := parts[0]
		badShape := len(parts) != 4 || !strings.HasSuffix(file, ".go")
		if !badShape {
			for _, p := range parts[1:3] {
				if strings.TrimSpace(p) == "" {
					badShape = true
				}
			}
			if ord := parts[3]; ord == "" || strings.Trim(ord, "0123456789") != "" || ord == "0" {
				badShape = true
			}
		}
		if badShape {
			bad = append(bad, fmt.Sprintf("allowlist entry %q must be "+
				"`path/file.go # declaration # form # ordinal` — a whole-file exemption "+
				"covers every decision the file grows later, silently and for free", a.site))
		}
		if a.n < 1 {
			bad = append(bad, fmt.Sprintf("allowlist entry %q must state how many decisions the "+
				"site hosts", a.site))
		}
		if strings.TrimSpace(a.why) == "" {
			bad = append(bad, fmt.Sprintf("allowlist entry %q needs a reason answering: why can "+
				"Resolved not answer this?", a.site))
		}
	}
	return bad
}

// fieldDecisionFileScope is the enclosing-declaration sentinel for a decision
// that sits outside any FuncDecl — a package-level var initializer. Spelled as
// a word rather than left empty so a key reads the same way everywhere and an
// accidental empty function name cannot silently produce a different key.
const fieldDecisionFileScope = "(file-scope)"

// fieldDecisionFuncName renders a top-level declaration's name for the site key.
//
// METHODS CARRY THEIR RECEIVER TYPE, because a bare method name is not unique
// within a file: two types in one file can both have a Field-reading `Equals`,
// and collapsing them would let one entry cover a decision in the other. The
// receiver's POINTERNESS is deliberately dropped — `(T).M` and `(*T).M` cannot
// both exist, so it adds no uniqueness and would churn the key on a
// value-to-pointer receiver change that moves no decision.
func fieldDecisionFuncName(fd *ast.FuncDecl) string {
	if fd == nil || fd.Name == nil {
		return fieldDecisionFileScope
	}
	name := fd.Name.Name
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	// Strip any generic instantiation: a receiver written `T[P]` is the same
	// declaration as `T`, and the parameter spelling is not identity.
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	if idx, ok := typ.(*ast.IndexListExpr); ok {
		typ = idx.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return "(" + id.Name + ")." + name
	}
	return name
}

// fieldDecisionSiteKey is the ratchet's SITE IDENTITY, and replacing a line
// number with it is the whole point of this scheme.
//
// THE OLD KEY WAS `path/file.go:LINE`, and a line number is invalidated by any
// edit ABOVE the site — including, absurdly, adding the census that measures the
// site. That cost eight mechanical re-keys in a single session, one of them a
// rebase conflict across four files that could only be resolved by discarding
// one side and re-deriving every number from the gate's own output. The check
// never needed a line: it needs a stable, unique, human-readable name for a
// decision, and nothing about "which line is it on" is part of that.
//
// THE KEY IS `path/file.go # enclosing-declaration # form`.
//
//	stable    — all three parts move only when the site itself is edited. Inserting
//	            a hundred lines above the function changes none of them.
//	unique    — verified, not assumed: TestFieldDecisionSiteKeysAreUnique walks the
//	            whole tree and fails on any collision, so a future site that
//	            genuinely collides is a red rather than a silently merged entry.
//	derivable — the walk already computes `form`, and the enclosing declaration is
//	            one field on a case arm it already has.
//	readable  — the debt list is read as documentation, and
//	            `values.go # explainValueOrdinals # a map key` says what the entry
//	            is about in a way `values.go:1826` never did.
//
// The separator is " # " rather than ":" so the key cannot be mistaken for, or
// accidentally parsed as, a file:line pair — the format assertion that used to
// demand digits now demands this shape instead, and the two cannot be confused.
//
// `form` INCLUDES the localNote suffix, deliberately. Two decisions in one
// function that differ only by which local carries the name are two different
// decisions with two different fixes, and merging them under one entry would let
// fixing one silently cover the other.
// THE ORDINAL SUFFIX exists because the triple alone is not unique, and that was
// measured rather than assumed: over the tracked tree the triple collapses 199
// decisions onto 154 distinct keys, and 15 of the 52 debt entries land on 5
// shared triples. The worst is AggregateResultColumnName, whose switch returns
// six differently-formatted names through one `opName` local — six genuinely
// separate decisions that the triple cannot tell apart.
//
// It is applied UNIFORMLY (every key ends `# N`, including the 137 that need no
// disambiguation) rather than only on collision. An "only when needed" suffix is
// unstable in the worst way: deleting the first of two makes the survivor change
// key without anyone editing it, so a fix to one entry silently invalidates
// another. Uniform costs four characters and removes that class entirely.
//
// WHAT THE ORDINAL DOES NOT SURVIVE, stated plainly because it is the scheme's
// one residual instability: inserting a NEW decision of the SAME form into the
// SAME function ahead of an existing one renumbers the survivors. That is an edit
// to the very function whose entries are listed, made by someone adding a name
// decision to it — which is exactly when those entries should be re-read. It is
// not the case this scheme exists to fix; that case is an edit ANYWHERE ABOVE the
// site, which the ordinal is completely immune to.
//
// fieldDecisionKeyer holds the per-run counters. It is a type rather than a
// package-level map so two concurrent walks cannot share state, and so the
// counting cannot be forgotten by a caller that builds a key by hand — which had
// already happened once: the closure test carried its own copy of the old
// `fmt.Sprintf("%s:%d", …)` formula and would have silently diverged from the
// tally the moment either changed.
type fieldDecisionKeyer struct {
	seen map[string]int
}

func newFieldDecisionKeyer() *fieldDecisionKeyer {
	return &fieldDecisionKeyer{seen: map[string]int{}}
}

// key returns the next key for this (file, declaration, form) triple. Call order
// is AST order, so the ordinal is source order within the declaration.
func (k *fieldDecisionKeyer) key(rel, fn, form string) string {
	if fn == "" {
		fn = fieldDecisionFileScope
	}
	triple := rel + " # " + fn + " # " + form
	k.seen[triple]++
	return fmt.Sprintf("%s # %d", triple, k.seen[triple])
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
	keyer := newFieldDecisionKeyer()
	scanFieldDecisions(f, func(pos token.Pos, form, fn string) {
		key := keyer.key(rel, fn, form)
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
// The cost of the correction, stated because it is real: cascades_translator.go:5747
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
	return nameDerivedIdentsSeeded(fn, nil)
}

// nameDerivedIdentsSeeded is nameDerivedIdents with an initial taint set — the
// PARAMETERS a caller fed the name into. Seeding rather than unioning
// afterwards is what makes the transitive step work: a parameter tainted at the
// call boundary must be able to taint the locals derived FROM it inside the
// callee, and a set merged after the walk cannot.
func nameDerivedIdentsSeeded(fn ast.Node, seed nameTaint) nameTaint {
	t := nameTaint{}
	for obj := range seed {
		t[obj] = true
	}
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

// callArgParamTaint propagates the name across a CALL BOUNDARY.
//
// A helper whose string parameter is fed a name-derived argument at ANY call
// site holds the display name inside its own body, and a comparison it makes on
// that parameter conflates column A with column B exactly as much as the caller
// would have. Until this pass existed the gate went blind at the boundary, and
// the blindness had a direction: EXTRACTING a helper converted a visible
// `.Field` decision into an invisible plain-string one, so the ratchet's count
// could be walked down by refactoring alone. That is how three sites left the
// ledger while the decisions stayed exactly where they were — which is the
// failure this pass exists to make impossible, not a hypothetical.
//
// The propagation predicate is escapesFieldName, the same one the RETURN check
// uses, and for the same reason: passing an argument IS an escape into another
// frame. It answers "does this expression yield a string that is still the
// name, with at most a decoration on it" — which is precisely what has to be
// true for the callee's parameter to be a display name.
//
// SCOPE, and the two over-approximations it accepts.
//
// Per FILE, not per package. The tree walk parses one file at a time with its
// own FileSet, and *ast.Object identity — the key this taint is built on — does
// not survive across those parses. A cross-file helper is therefore still
// invisible; that hole is real and stated rather than papered over. It is the
// smaller half: an extraction lands beside its caller far more often than in
// another file, and both halves of the shape this pass was built for
// (legWindowSlot, legBake) are same-file.
//
// A call site is matched by the callee's NAME, so two same-named methods on
// different types in one file cross-taint. That over-approximates toward MORE
// reported sites, which is the safe direction for a ratchet: a false report
// costs an audit and an explicit entry, a false silence costs a defect. It is
// deliberate and not a limitation to be quietly fixed by narrowing.
//
// Iterated to a FIXED POINT because a tainted parameter can itself be passed
// on: `a(fv.Field)` → a's param → `b(param)` → b's param. Stopping at one hop
// would be the same arbitrary depth limit the intra-function taint already
// rejected.
func callArgParamTaint(f *ast.File) map[*ast.FuncDecl]nameTaint {
	plain := map[string]*ast.FuncDecl{}
	methods := map[string]*ast.FuncDecl{}
	var decls []*ast.FuncDecl
	for _, d := range f.Decls {
		fn, isFn := d.(*ast.FuncDecl)
		if !isFn || fn.Name == nil || fn.Body == nil {
			continue
		}
		decls = append(decls, fn)
		if fn.Recv == nil {
			if _, dup := plain[fn.Name.Name]; !dup {
				plain[fn.Name.Name] = fn
			}
			continue
		}
		if _, dup := methods[fn.Name.Name]; !dup {
			methods[fn.Name.Name] = fn
		}
	}

	out := map[*ast.FuncDecl]nameTaint{}
	for _, fn := range decls {
		out[fn] = nameTaint{}
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range decls {
			caller := nameDerivedIdentsSeeded(fn, out[fn])
			ast.Inspect(fn, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				var callee *ast.FuncDecl
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = plain[fun.Name]
				case *ast.SelectorExpr:
					callee = methods[fun.Sel.Name]
				}
				if callee == nil || out[callee] == nil {
					return true
				}
				params := flatParams(callee)
				for i, arg := range call.Args {
					p := paramAt(params, i, callee)
					if p == nil || p.Obj == nil || p.Name == "_" {
						continue
					}
					if !escapesFieldName(arg, caller) || out[callee][p.Obj] {
						continue
					}
					out[callee][p.Obj] = true
					changed = true
				}
				return true
			})
		}
	}
	return out
}

// flatParams flattens a signature's parameter list into positional order,
// expanding grouped declarations (`a, b string` is two parameters, not one).
func flatParams(fn *ast.FuncDecl) []*ast.Ident {
	var out []*ast.Ident
	if fn.Type == nil || fn.Type.Params == nil {
		return out
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			out = append(out, nil) // unnamed parameter: positional, unusable
			continue
		}
		out = append(out, field.Names...)
	}
	return out
}

// paramAt maps an argument position to its parameter, folding every trailing
// argument of a VARIADIC signature onto the final parameter — `f(a, b, c)` on
// `f(xs ...string)` puts all three into xs, so any one of them tainting it is
// enough.
func paramAt(params []*ast.Ident, i int, fn *ast.FuncDecl) *ast.Ident {
	if len(params) == 0 {
		return nil
	}
	if i < len(params) {
		return params[i]
	}
	if last := lastParamField(fn); last != nil {
		if _, variadic := last.Type.(*ast.Ellipsis); variadic {
			return params[len(params)-1]
		}
	}
	return nil
}

func lastParamField(fn *ast.FuncDecl) *ast.Field {
	if fn.Type == nil || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return nil
	}
	return fn.Type.Params.List[len(fn.Type.Params.List)-1]
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
// flattenConcat returns the operands of a `+` chain in source order, descending
// through nested `+` BinaryExprs. Anything else is one operand.
// Parentheses are unwrapped first: `a + ("." + b)` is the same mint written
// right-nested, and go/parser keeps the ParenExpr, so a walk that only descends
// through BinaryExpr sees one operand that is neither the separator nor the name
// and reports nothing.
func flattenConcat(e ast.Expr) []ast.Expr {
	for {
		p, isParen := e.(*ast.ParenExpr)
		if !isParen {
			break
		}
		e = p.X
	}
	be, ok := e.(*ast.BinaryExpr)
	if !ok || be.Op != token.ADD {
		return []ast.Expr{e}
	}
	return append(flattenConcat(be.X), flattenConcat(be.Y)...)
}

// isQualifierJoinLit reports whether e is the string literal `"."` — the
// separator that turns a one-level display name into a two-level key.
//
// EXACTLY that literal, not "contains a dot". A message fragment like
// `" in leg "` or `"...: "` is not a qualifier join, and reporting it would
// bury the mint arm's real finding under every error string that happens to
// punctuate. The dotted channel is spelled one way by every producer in this
// tree, and that spelling is what the `dotted` readers split back apart.
func isQualifierJoinLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == "."
}

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
func scanFieldDecisions(f *ast.File, report func(pos token.Pos, form, fn string)) {
	// The enclosing top-level declaration's name, tracked for the SITE KEY.
	//
	// It is the stable half of a site's identity: a line number moves whenever
	// anything above it moves — including the census that measures the site —
	// while a function name moves only when someone edits that function. That
	// is the whole reason this variable exists; see fieldDecisionSiteKey.
	//
	// Granularity is the TOP-LEVEL declaration, matching the two fields below:
	// a closure inherits its parent's name exactly as it inherits its parent's
	// handlesFieldValue answer, so a decision inside a FuncLit is attributed to
	// the FuncDecl containing it. Decisions outside any FuncDecl (a package-level
	// var initializer) get the file-scope sentinel.
	funcName := fieldDecisionFileScope

	// emit stamps every report with the enclosing declaration. Wrapping here
	// rather than threading funcName through seven call sites keeps the arms
	// reading as they did, and makes it impossible for one arm to forget it.
	emit := func(pos token.Pos, form string) { report(pos, form, funcName) }

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

	// Source RANGES already reported by the MINT arm, so one mint reports once
	// however its `+` chain is nested.
	//
	// Ranges rather than start positions. A LEFT-nested chain shares its start
	// with every prefix of itself, so a start-keyed set deduped it — but a
	// RIGHT-nested one (`corr + ("." + fv.Field)`) gives the inner node a
	// different start, and the same set let it report twice. Containment is the
	// property that actually holds in both: pre-order reaches the outermost `+`
	// first, and every sub-chain of it lies inside its range.
	type srcRange struct{ lo, hi token.Pos }
	var mintedRanges []srcRange
	alreadyMinted := func(n ast.Node) bool {
		for _, r := range mintedRanges {
			if n.Pos() >= r.lo && n.End() <= r.hi {
				return true
			}
		}
		return false
	}

	// PARAMETERS a call site in this file feeds the name into. Computed once for
	// the whole file because the propagation is a fixed point over all of it —
	// the callee is routinely declared after the caller, so a per-function pass
	// in source order would see half the call graph.
	paramTaint := callArgParamTaint(f)

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
	// the SORT-KEY CARRIER shape — `plans.SortKey.Field` compared inside a
	// function that never names a FieldValue — of which rowdiff's
	// sortKeysMatchOrderBy is the live instance the debt list holds. Trading that
	// shape for four false positives is a worse gate on both axes, so the tiers
	// are additive — reach is never narrowed, depth is only added where the type
	// is in play.
	//
	// The shape is named rather than pointed at by file:line on purpose, and the
	// purpose is a measurement. This paragraph used to cite "in_memory_sort.go:142
	// and rowdiff/ordering.go:241, two sites the gate holds today", and by the time
	// the harness entry was audited NEITHER citation was true: RFC-197 item 3
	// migrated the in_memory_sort comparison to ValueExpr, so it is not a site at
	// all and appears nowhere in the debt list, and the rowdiff line number had
	// drifted off the function it named. The argument survived — the trade is
	// still the right one — but half of the evidence it rested on had been fixed
	// out from under it and the prose still asserted it. A design trade defended
	// by line numbers decays into an unfalsifiable claim; the fixtures below are
	// what actually holds it.
	//
	// The PRICE of the ungated shallow tier, stated so nobody has to rediscover
	// it: a direct `.Field` selector is typed by SPELLING ALONE. `x.Field == s`
	// on a type unrelated to FieldValue, in a function with no FieldValue
	// anywhere in it, is reported — the gate cannot tell it apart from the
	// sort-key carrier above, which is a NAME-TYPED CARRIER: the string it
	// compares came off a FieldValue upstream and carries the conflation with it.
	// It is real debt, and the audit of the harness entry is what established that
	// rather than assuming it — the leaf name there is a FRAGMENT of the ORDER BY
	// text it is checked against, and it did conflate two legs.
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
			tainted = nameDerivedIdentsSeeded(x, paramTaint[x])
			funcName = fieldDecisionFuncName(x)
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
					emit(x.Pos(), "a "+op+" comparison"+localNote(decidesRaw, x.X, x.Y))
				}
			}
			// THE MINT ARM — the PRODUCER side of the dotted channel.
			//
			// Every other arm here watches a name being READ. RFC-197's rule is
			// producer-first, and the instrument enforcing it could not see a
			// producer at all: `corr + "." + strings.ToUpper(fv.Field)` is a `+`
			// BinaryExpr, so it fell through the comparison arm above and was
			// reported by nothing. Measured — the mint that CQ-53 exists to delete
			// could be restored at rule_implement_nested_loop_join.go and the whole
			// ratchet stayed GREEN.
			//
			// What is reported is narrow on purpose: a concatenation that JOINS a
			// display name to a QUALIFIER SEPARATOR literal. That is the shape that
			// manufactures a two-level key out of a one-level name, and it is the
			// shape every reader in the `dotted` bucket exists to take apart again.
			//
			// THE BOUND, stated because it is not obvious from the arm and because
			// the sites it excludes are the ones that matter most right now. This
			// arm inherits the whole gate's scope: `decides` reads FieldValue.Field
			// and identifiers tainted from it. A mint whose operand is a plain
			// string parameter, or a `.Name` selector off a schema/leg field, is
			// INVISIBLE here. The two producers this branch measured as the live
			// channel are both outside the scope for exactly that reason —
			// scalar_subquery_seed.go:129 joins a plain `scalarCol` string, and
			// clustered_outer_scalar.go:493 joins `leg.typ.Fields[i].Name`. Neither
			// is a FieldValue, so neither is counted, and the `dotted` bucket's
			// tally must not be read as "every dotted producer in the tree".
			//
			// One shape is undetectable rather than merely out of scope, and it is
			// worth naming so nobody spends an afternoon on it: values.go:1713-1715
			// accumulate path steps into a SLICE (`steps[i] = ...`), and the taint
			// tracker cannot follow that assignment — taint() requires an *ast.Ident
			// on the left, and `steps[i]` is an IndexExpr. That blinds the arm twice
			// over: :1713 joins the slice with strings.Join (no `+ "." +` node at
			// all), and :1715 DOES concatenate `+ "." + path`, but `path` arrives
			// clean through the untainted slice, so the separator node is there and
			// the name never taints it. Its sibling at :1720 IS caught, because
			// that one concatenates the tainted name directly. A
			// heuristic for the slice form would key on strings.Join's separator
			// argument and would fire on every path-joining helper in the tree; the
			// bound is stated instead.
			// Plain concatenation is not reported — building a message, or suffixing
			// a name, cannot confuse column A with column B, which is the only
			// failure this gate is about.
			if op == "+" {
				// The chain is FLATTENED before it is judged. `a + "." + b` parses
				// left-nested as `(a + ".") + b`, so neither operand of the outer `+`
				// IS the separator and neither operand of the inner one reads the
				// name — testing the two operands in place reports nothing, which is
				// how this arm silently passed the first time it was written.
				operands := flattenConcat(x)
				hasSep, hasName := false, false
				for _, o := range operands {
					if isQualifierJoinLit(o) {
						hasSep = true
					}
					if decides(o) {
						hasName = true
					}
				}
				// Pre-order traversal reaches the OUTERMOST `+` first, and a
				// left-nested chain shares its starting position with every prefix of
				// itself, so the inner nodes would re-report the same site. The first
				// report at a position wins and the rest are dropped: one mint, one
				// entry.
				if hasSep && hasName && !alreadyMinted(x) {
					mintedRanges = append(mintedRanges, srcRange{x.Pos(), x.End()})
					emit(x.Pos(), "a dotted-name MINT (qualifier joined to the name)"+
						localNote(decidesRaw, operands...))
				}
			}
		case *ast.SwitchStmt:
			// Switching on a display name is equality N times over. (An
			// EMPTY-tag switch needs no arm here: ast.Inspect still visits
			// each case's boolean expression as an ordinary BinaryExpr.)
			if x.Tag != nil && decides(x.Tag) {
				emit(x.Pos(), "a switch tag"+localNote(decidesRaw, x.Tag))
			}
		case *ast.IndexExpr:
			// Keying a map by display name conflates same-named columns.
			if decides(x.Index) {
				emit(x.Pos(), "a map key"+localNote(decidesRaw, x.Index))
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
						emit(kv.Pos(), "a composite-literal key"+localNote(decidesRaw, kv.Key))
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
					emit(x.Pos(), "the name escaping as a bare string (return)"+
						localNote(escapesRaw, r))
					break
				}
			}
		case *ast.CallExpr:
			if name := callFuncName(x.Fun); stringCompareHelpers[name] {
				for _, arg := range x.Args {
					if decides(arg) {
						emit(x.Pos(), "a "+name+" call"+localNote(decidesRaw, arg))
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
	// AUTHORITIES LEAD, escapes follow in parentheses. The primary number is the
	// one that answers "how much work remains", because a fix lands on a
	// declaration; the escape count is what the list actually stores and is kept
	// visible beside it. See bucketAuthorityCounts for why neither replaces the
	// other, and why the list is not collapsed.
	authorities := bucketAuthorityCounts(knownFieldDecisionDebt)
	authSum := 0
	for _, b := range buckets {
		fmt.Fprintf(&summary, "\n  %-11s %3d authority/ies  (%3d escape sites)",
			b, authorities[b], counts[b])
		sum += counts[b]
		authSum += authorities[b]
	}
	totalAuthorities := map[string]struct{}{}
	for site := range knownFieldDecisionDebt {
		totalAuthorities[fieldDecisionAuthorityOf(site)] = struct{}{}
	}
	t.Logf("field-name debt by owning bucket:%s\n  %-11s %3d authority/ies  (%3d escape sites, %d entries)\n"+
		"  the two differ because one declaration can host several escapes — a switch\n"+
		"  with six return arms is six escapes and one place to fix them.",
		summary.String(), "TOTAL", len(totalAuthorities), sum, len(knownFieldDecisionDebt))

	if len(totalAuthorities) != fieldDebtAuthorityTotal {
		t.Errorf("the debt spans %d distinct authorities, but fieldDebtAuthorityTotal "+
			"claims %d.\n\nThe authority count is the number this report LEADS with — "+
			"the answer to 'how much work remains', because a fix lands on a "+
			"declaration. Update the constant in the same commit that moved it, so the "+
			"change is a decision rather than a drift. If entries were retired the "+
			"number should FALL; if it rose, a new declaration started leaking a name.",
			len(totalAuthorities), fieldDebtAuthorityTotal)
	}

	// THE TWO NUMBERS MUST NOT DRIFT. Per-bucket authorities summed across
	// buckets must equal the distinct authorities overall — they can only differ
	// if one declaration's escapes are filed under two different buckets, which
	// is legal (a declaration can owe two kinds of debt) and must therefore be
	// REPORTED rather than silently absorbed. Left unchecked, the day the two
	// disagree with no explanation is the day someone "corrects" one to match the
	// other.
	if authSum != len(totalAuthorities) {
		var split []string
		byAuthority := map[string]map[string]struct{}{}
		for site, d := range knownFieldDecisionDebt {
			b, ok := bucketTagOf(d.why)
			if !ok {
				continue
			}
			a := fieldDecisionAuthorityOf(site)
			if byAuthority[a] == nil {
				byAuthority[a] = map[string]struct{}{}
			}
			byAuthority[a][b] = struct{}{}
		}
		for a, bs := range byAuthority {
			if len(bs) > 1 {
				names := make([]string, 0, len(bs))
				for b := range bs {
					names = append(names, b)
				}
				sort.Strings(names)
				split = append(split, fmt.Sprintf("%s → %v", a, names))
			}
		}
		sort.Strings(split)
		t.Logf("per-bucket authorities sum to %d against %d distinct overall: %d "+
			"declaration(s) owe debt in more than one bucket, which is legal and is "+
			"listed here so the difference is never mistaken for an arithmetic slip:\n  %s",
			authSum, len(totalAuthorities), len(split), strings.Join(split, "\n  "))
	}

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

// TestNoFirstMatchNameLookup is the INVERTED guard that replaced the
// FieldIndex blind-spot inventory. That list watched a population for going
// stale; this one watches a population for coming back.
//
// `RecordType.FieldIndex` and `RecordType.LookupField` resolved a column by
// name and answered the FIRST field carrying it. A record type may legitimately
// declare a name twice — a leg-concat of two sources merges `A.K` and `B.K`
// into one row — so the first match is indistinguishable from a correct answer,
// and the wrong one is a real column of the same type that nothing downstream
// rejects. Both methods were deleted; FieldIndexUnique / LookupFieldUnique
// resolve only on an unambiguous name.
//
// A green here means neither a declaration nor a call has reappeared. It counts
// what it scanned and fails on an empty population, because a walk that reached
// no files reports exactly the same green as a tree with no violations.
// selfExemptFieldDecisionFile is the ONE file TestNoFirstMatchNameLookup skips,
// stated as a repo-relative path so the exemption cannot spread by naming.
const selfExemptFieldDecisionFile = "pkg/docscheck/field_name_decision_test.go"

// firstMatchNameLookups are the deleted first-match lookups and their
// replacements. Watched by NAME, because that is what a revival would reuse.
var firstMatchNameLookups = map[string]string{
	"FieldIndex":  "FieldIndexUnique",
	"LookupField": "LookupFieldUnique",
}

// scanFirstMatchNameLookups reports every revival of a deleted first-match name
// lookup in one file's source, as "rel:LINE: …" strings. parsed is false when
// the source does not parse.
//
// It is SPLIT OUT of the tree walk so the decision can be driven from source
// held in a string. The alternative is what actually happened: the two holes
// below were found by dropping a probe FILE into the tree, watching the gate
// report it, and deleting the probe — which leaves the conclusion in a commit
// message and nothing that fails if either hole reopens. A revival of this gate's
// own blind spot cannot be pinned by a real file, because a real
// `func FieldIndex` in the tree is a permanent red.
func scanFirstMatchNameLookups(rel string, src []byte) (problems []string, parsed bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, false
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			// METHOD OR PLAIN FUNCTION. Bailing on `x.Recv == nil` watched only
			// the method form, so `func FieldIndex(r *RecordType, name string)
			// (int, bool)` declared at package level in `values` — the same
			// first-match lookup with the receiver moved into the parameter
			// list — reintroduced the API this gate deletes and the gate had
			// nothing to say about it.
			if x.Name == nil {
				return true
			}
			if want, bad := firstMatchNameLookups[x.Name.Name]; bad {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s declared — use %s",
					rel, fset.Position(x.Pos()).Line, x.Name.Name, want))
			}
		case *ast.CallExpr:
			// QUALIFIED OR UNQUALIFIED. Matching only *ast.SelectorExpr watched
			// `x.FieldIndex(n)` and missed the bare `FieldIndex(rt, n)` a
			// package-level redeclaration is called by — so the two holes
			// COMPOSED: declare it as a function and call it from its own
			// package, and both arms passed.
			var name string
			switch fn := x.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel != nil {
					name = fn.Sel.Name
				}
			case *ast.Ident:
				name = fn.Name
			}
			if want, bad := firstMatchNameLookups[name]; bad {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: call to %s — use %s",
					rel, fset.Position(x.Pos()).Line, name, want))
			}
		}
		return true
	})
	return problems, true
}

// TestFirstMatchNameLookupScanArms drives every arm of the gate's decision from
// source held in a string, including the two that a tree walk over a CLEAN tree
// cannot reach at all.
//
// A green from the walk above is a statement about the tree, not about the
// detector. Both arms below passed the tree walk while blind: the declaration
// arm bailed on `x.Recv == nil` and the call arm matched only *ast.SelectorExpr,
// so a package-level `func FieldIndex` called unqualified from its own package
// slipped through BOTH. The tree was clean, so the gate reported green and the
// blindness was invisible — which is exactly the shape this repo keeps finding:
// a green from an empty set.
func TestFirstMatchNameLookupScanArms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		src     string
		want    int
		blindTo string
	}{
		{
			"method declaration", "package p\ntype T struct{}\nfunc (t *T) FieldIndex(n string) (int, bool) { return 0, false }\n", 1,
			"the original form — the only one the gate ever watched",
		},
		{
			"package-level function declaration", "package p\ntype T struct{}\nfunc FieldIndex(t *T, n string) (int, bool) { return 0, false }\n", 1,
			"the receiver moved into the parameter list. Identical semantics, and the " +
				"`x.Recv == nil` bail meant the gate never looked",
		},
		{
			"package-level LookupField", "package p\ntype T struct{}\nfunc LookupField(t *T, n string) (int, bool) { return 0, false }\n", 1,
			"the sibling lookup, same evasion — both names must be watched in both forms",
		},
		{
			"qualified call", "package p\nfunc f(t interface{ FieldIndex(string) (int, bool) }) { t.FieldIndex(\"K\") }\n", 1,
			"the original call form",
		},
		{
			"unqualified call", "package p\nfunc FieldIndexUniqueX() {}\nfunc f() { FieldIndex(nil, \"K\") }\n", 1,
			"how a package-level redeclaration is called from its own package. The " +
				"*ast.SelectorExpr-only match saw an *ast.Ident and returned",
		},
		{
			"the Unique forms are not flagged", "package p\ntype T struct{}\nfunc (t *T) FieldIndexUnique(n string) (int, bool) { return 0, false }\nfunc g(t *T) { t.FieldIndexUnique(\"K\"); t.LookupFieldUnique(\"K\") }\n", 0,
			"the NEGATIVE control. Without it every arm above is satisfied by a " +
				"detector that flags everything, and the gate would fail the whole tree",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, parsed := scanFirstMatchNameLookups("probe.go", []byte(tc.src))
			if !parsed {
				t.Fatalf("the probe source did not parse — the case tests nothing:\n%s", tc.src)
			}
			if len(got) != tc.want {
				t.Fatalf("scan reported %d finding(s) %v, want %d.\n  This arm exists because: %s",
					len(got), got, tc.want, tc.blindTo)
			}
		})
	}
}

func TestNoFirstMatchNameLookup(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var problems []string
	scanned := 0
	selfExempted := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Vendored and generated trees are not ours to hold to this rule,
			// and bazel-* are symlinked build outputs that would double-count.
			switch info.Name() {
			case "vendor", "gen", "node_modules", ".git", "fdb-record-layer":
				return filepath.SkipDir
			case ".claude":
				// Agent worktrees live here — other branches' checkouts of this
				// same repo. Scanning them reports THEIR code as this tree's
				// violations, which is how a clean tree fails with 1500 findings.
				return filepath.SkipDir
			}
			if strings.HasPrefix(info.Name(), "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		// This file names both methods in prose and in this test's own tables;
		// scanning it would report itself. The exemption is an EXACT path, not a
		// suffix: `strings.HasSuffix(path, "field_name_decision_test.go")` also
		// exempts `values_field_name_decision_test.go` and any other file whose
		// name merely ends that way, so the one file that must be skipped came
		// with a free skip for every future file that copies its name.
		if filepath.ToSlash(rel) == selfExemptFieldDecisionFile {
			selfExempted++
			return nil
		}
		found, parsed := scanFirstMatchNameLookups(rel, src)
		if !parsed {
			return nil // not our business to fail on unparseable files
		}
		scanned++
		problems = append(problems, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if selfExempted != 1 {
		t.Fatalf("the self-exemption matched %d files, want exactly 1 (%s).\n"+
			"  0 means the path is stale — this file renamed or moved — so the test is\n"+
			"  about to report its own prose as violations. More than 1 means the exact\n"+
			"  match has stopped being exact and the exemption is spreading, which is the\n"+
			"  suffix hole this replaced.", selfExempted, selfExemptFieldDecisionFile)
	}
	if scanned == 0 {
		t.Fatal("scanned 0 Go files — this test cannot distinguish a clean tree " +
			"from a walk that reached nothing, and an empty population reports green")
	}
	if len(problems) > 0 {
		t.Fatalf("a first-match name lookup came back (%d site(s)):\n  %s\n\n"+
			"These resolve a column by NAME and answer the FIRST field carrying it. "+
			"A row can declare one name twice — a leg-concat merges A.K and B.K into "+
			"one row — so the answer is a guess that reads as a fact, and the wrong "+
			"slot is a real column of the same type that nothing downstream rejects. "+
			"Use the Unique form, which declines on an ambiguous name.",
			len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("no first-match name lookup in %d Go files", scanned)
}
