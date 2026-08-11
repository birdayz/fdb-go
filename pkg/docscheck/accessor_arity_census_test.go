package docscheck

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The `Accessors` ARITY census — a CLASSIFIED population, pinned so it cannot
// silently change.
//
// RFC-230 (nested GROUP BY keys) needs to know which sites refuse a
// multi-accessor `values.FieldPath` and WHY. Five successive revisions of that
// RFC enumerated those sites by reading and published five different counts —
// 2, 2, 2, 3, 7 — each presented as complete. The defect was the method, not
// the arithmetic: every sweep searched for symbols that were ALREADY
// suspected, so it could only re-find what was already known. Rev 6 swept by
// BEHAVIOUR instead (`git grep -n "len(.*Accessors)"`, non-test) and published
// a population of 52 with the classification explicitly booked as OWED.
//
// This file is that classification, and it is a test rather than prose so the
// population cannot grow without someone deciding what the new site is. A
// classified census that lives in a document rots on the first refactor; one
// that fails the build does not.
//
// WHAT THE FOUR CLASSES MEAN
//
//	(a) CORRECT DECLINE — the site legitimately refuses a multi-accessor value
//	    because the value genuinely cannot be what the site needs. A decline
//	    costs at most an optimization; it never costs rows.
//	(b) BLOCKER — the site would refuse or mis-handle a LEGITIMATE nested
//	    grouping key. These are the sites RFC-230 must change.
//	(c) ALREADY CORRECT FOR NESTING — handles multi-accessor paths properly
//	    (descends them, preserves the suffix, compares element-wise, or is
//	    arity-tolerant by construction). No change needed.
//	(d) LIVE DEFECT — mis-handles a multi-accessor value TODAY, independent of
//	    RFC-230.
//	(?) UNCERTAIN — honestly unresolved. An unknown that says why is worth more
//	    than a confident wrong classification; that is precisely what cost this
//	    census five revisions.
//
// THE POPULATION IS NOT 52 SITES. It is 52 grep LINES, and they decompose:
//
//	52 = 8 generated + 1 comment + 43 code lines
//
// The 8 are protobuf marshal/unmarshal loops over `PFieldPath.FieldAccessors`
// in gen/record_query_plan_vtproto.pb.go — a wire-format field list, not a
// `values.FieldPath`, and not a gate of any kind. The 1 is prose inside a doc
// comment. The 43 code lines carry 47 arity EXPRESSIONS (four lines hold more
// than one, e.g. `len(a.Accessors) == 0 || len(a.Accessors) != len(b.Accessors)`)
// spread over 36 enclosing symbols. Those 36 symbols are the thing worth
// classifying, and they are what accessorAritySites pins.
//
// Sites are keyed by `file#symbol`, NEVER by line. The RFC's own anchors drifted
// across three bases during its review (the group-key ladder moved +86 lines
// once), which is why the enclosing symbol is the citation here and the line is
// not recorded at all.
//
// WHAT THIS CENSUS DOES NOT SEE, stated so the next reader does not mistake it
// for exhaustive: a site that DISCARDS an accessor chain without ever testing
// its length is invisible to a `len(...Accessors)` sweep. Sweeping by one
// behaviour is still sweeping by one behaviour.
//
// THAT BLIND SPOT IS NO LONGER HYPOTHETICAL, and it is worth knowing how badly
// it read. cascades_generator.go#operandTypeNameViaDesc took a FieldValue's
// `Field` straight into a top-level descriptor lookup with no arity test at all.
// It is in a file this census already covers, in the same function family as the
// one site the census could not classify — and it is where the live defect
// actually was. The sweep could not see it precisely BECAUSE it was unguarded:
// the census finds sites that ASK about arity, so a site that never asks is
// absent from the population by the same property that makes it wrong. A site's
// absence from this list is therefore not evidence about the site.
//
// Two consequences for anyone extending this census. First, the complement sweep
// — the reads of `.Field` on a value that may be fused — is the one that would
// have found it, and it has not been done. Second, every gate added since is
// visible here only because it was added; fixing an unguarded site GROWS this
// population, so a rising count is the instrument working rather than drifting.

// accessorArityClass is the classification of one arity site.
type accessorArityClass string

const (
	arityCorrectDecline accessorArityClass = "a" // legitimately refuses a multi-accessor value
	arityBlocker        accessorArityClass = "b" // must change for nested grouping keys to work
	arityNestingOK      accessorArityClass = "c" // already handles multi-accessor paths
	arityLiveDefect     accessorArityClass = "d" // mis-handles one today
	arityUncertain      accessorArityClass = "?" // honestly unresolved — reason recorded
)

type accessorAritySite struct {
	class accessorArityClass
	// exprs is how many `len(...Accessors)` expressions this symbol contains.
	// Pinned so an arity test ADDED to an already-classified function still
	// fails the census rather than hiding behind its neighbour's verdict.
	exprs int
	why   string
}

// accessorAritySites is the classified population, keyed `file#symbol`.
var accessorAritySites = map[string]accessorAritySite{
	// ---------------------------------------------------------------------
	// (a) CORRECT DECLINES — refusing a value that genuinely cannot be what
	//     the site needs. A decline here costs an optimization, never rows.
	// ---------------------------------------------------------------------

	// THE THREE FORMER (b) BLOCKERS. Nested-path GROUP BY landed with all three
	// UNCHANGED — cascades_translator.go is byte-identical across that change —
	// so the classification that made them blockers is refuted by measurement,
	// not by argument. Each carries what was observed rather than what was read.
	"pkg/relational/core/query/cascades_translator.go#groupByOutputBaker": {
		class: arityCorrectDecline, exprs: 1,
		why: "the early return is LOAD-BEARING, not a structural no-op, and the difference " +
			"is measured rather than argued: disabling it turns group_by_output_baker_test's " +
			"`multi` case RED — a 2-accessor path whose Field is COL1, with a nil child and " +
			"keyOrds{COL1:0}, reaches the name channel and is rewritten into a " +
			"single-accessor read of output slot 0, i.e. a nested member read silently " +
			"becomes a read of the whole struct root's slot. So the decline is CORRECT, and " +
			"a leaf that collides with a group-key output name is what makes it necessary. " +
			"What keeps a nested grouping key from needing the rebind is UPSTREAM and is a " +
			"different mechanism: rebaseHavingGroupKeyPredicate asks the shared decider " +
			"(cascades.PredicatePushesBelowGroupBy) and, for every predicate that will NOT " +
			"be pushed, rebasePostAggregateGroupKeyValue pins the reference to a " +
			"single-accessor FrontierPinned value addressed against the aggregate output " +
			"row — so it exits at the FrontierPinned guard ABOVE this expression and never " +
			"reaches it. Pushdown handles only the narrow complement: " +
			"predicateReferencesOnlyKeys admits a SINGLE ComparisonPredicate with a " +
			"key-only comparand, and buildGroupKeySet disables pushdown outright when two " +
			"keys share an accessor path, so AND / OR / NOT and any aggregate reference stay " +
			"above and travel the pinning route. Both routes are exercised end-to-end by " +
			"TestFDB_GroupByNestedPathKey. The predicted post-aggregate rebind was therefore " +
			"never needed HERE — it already exists one layer up.",
	},
	"pkg/relational/core/query/cascades_translator.go#cascadesTranslator.translateSort": {
		class: arityCorrectDecline, exprs: 1,
		why: "MEASURED: a nested ORDER BY key over a grouped nested key never enters the " +
			"block this expression lives in. Instrumenting the arm selection, `GROUP BY " +
			"r.v.z ORDER BY r.v.z` arrives with AggregateOutputValueExact set, so it takes " +
			"the canonicalizeAggregateOutputValue arm and the `sortGB != nil && " +
			"!exactAggregateValue` block is skipped entirely; the correlated-scalar variant " +
			"arrives with HasAggregateOutputOrdinal set and takes the ordinal arm. Both " +
			"upstream arms hand this site a value already rebound to ONE accessor of the " +
			"aggregate output row, which is Java's FieldPath.ofSingle — so the arity test " +
			"passes rather than blocking.",
	},

	"pkg/relational/core/query/cascades_translator.go#canonicalizeAggregateOutputValue": {
		class: arityCorrectDecline, exprs: 1,
		why: "RFC-230 §7.1's RULED gate. Java's pull-up mints FieldPath.ofSingle " +
			"(CompensateRecordConstructorRule.java:88-92), so nesting is consumed BELOW the " +
			"aggregate; by the time a value addresses the aggregate's output row it " +
			"addresses one flat slot of it, and a multi-accessor path there is genuinely " +
			"malformed. Must NOT be narrowed.",
	},
	"pkg/recordlayer/query/executor/streaming_cursors.go#bakedLegOperand": {
		class: arityCorrectDecline, exprs: 1,
		why: "the RFC's confirmed example, and reading confirms it. A fused path's ROOT " +
			"ordinal addresses a merge-shaped intermediate, not the leg row this predicate " +
			"is asked about, so there is no leg-local slot for it to name. Declining costs " +
			"one equijoin-cursor specialisation.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.Single": {
		class: arityCorrectDecline, exprs: 1,
		why: "the type's own single-step accessor. `Single()` means single by definition; " +
			"the multi-accessor callers use Root()/Last()/ReAnchorRootInto instead.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.OrdinalIn": {
		class: arityCorrectDecline, exprs: 1,
		why: "\"which slot of THIS layout\" is only defined for a one-step path; a deeper " +
			"ordinal indexes a nested record, not the caller's layout. The arity-TOLERANT " +
			"counterpart RootOrdinalIn exists in the same file for callers that want the " +
			"root, which is what makes this a decline rather than a gap.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldValue.SourceRelativeBaked": {
		class: arityCorrectDecline, exprs: 1,
		why: "deliberately narrow, with the broader twin RootIsLegRelativeUnpinned adjacent " +
			"and documented as the one the fused shape needs. The narrowness is the " +
			"predicate's contract, not an oversight — its own doc names the two consequences " +
			"the broad form was needed for.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#CanBridgeOrderingFieldValues": {
		class: arityCorrectDecline, exprs: 2,
		why: "the representation bridge between a baked and a lazy ordering key is by " +
			"ordinal-free NAME. A leaf name does not identify a nested path's source slot, " +
			"so bridging one would equate two different reads. Fail-closed: a refused bridge " +
			"costs a sort elision.",
	},
	"pkg/recordlayer/query/plan/cascades/values/column_identity.go#OrderingIdentityOf": {
		class: arityCorrectDecline, exprs: 1,
		why: "a chained path has no single (correlation, domain, ordinal) triple to state — " +
			"the root's layout is not the layout the deeper ordinal indexes. Declining keeps " +
			"the ordering comparator an equivalence relation; admitting it would make the " +
			"sets it builds depend on insertion order, i.e. a nondeterministic plan.",
	},
	"pkg/recordlayer/query/plan/cascades/values/ordinal_join_seed.go#AssertOrdinalJoinSeed": {
		class: arityCorrectDecline, exprs: 1,
		why: "a construction INVARIANT, not a query gate: the join seed is single-accessor " +
			"by construction, so a fused path in one means a compose rule fired where it " +
			"must not. The len() here only formats the panic message; Single() makes the " +
			"decision.",
	},
	"pkg/recordlayer/query/plan/cascades/intersector_primary_key.go#bakedIntersectionKeys": {
		class: arityCorrectDecline, exprs: 1,
		why: "declines the INTERSECTION CANDIDATE when a comparison key will not bake to one " +
			"flat leg slot. The ordinal row model has no runtime name fallback, so an " +
			"unbakeable key would be a loud merge-time failure; a plan-time decline costs " +
			"the intersection plan and nothing else.",
	},
	"pkg/recordlayer/query/plan/cascades/rule_projection_merge.go#ProjectionMergeRule.OnMatch": {
		class: arityCorrectDecline, exprs: 1,
		why: "the merge substitutes an outer slot read by the inner value at that ordinal; " +
			"a nested read is not a slot selection the composition can PROVE. Declining " +
			"leaves both projections — one extra operator, correct rows. Conservative rather " +
			"than necessary (root-selects-inner-then-descend is derivable), but a missed " +
			"rewrite is not a blocker.",
	},
	"pkg/recordlayer/query/plan/plans/ordering.go#RecordQueryMultiIntersectionOnValuesPlan.HintOrdering": {
		class: arityCorrectDecline, exprs: 1,
		why: "restating a child-relative comparison key in the OUTPUT row's domain is proven " +
			"only for \"grouping column i, read at slot i\". Anything else returns the " +
			"unrestated key — a weaker advertised ordering, never a wrong one.",
	},

	// ---------------------------------------------------------------------
	// (c) ALREADY CORRECT FOR NESTING.
	// ---------------------------------------------------------------------

	"pkg/relational/core/embedded/logical_predicate.go#groupedScalarSortKeys": {
		class: arityNestingOK, exprs: 1,
		why: "the CORRELATED-SCALAR-SUBQUERY grouped ORDER BY path, classified a blocker on " +
			"the reading that a nested key would leave `ordinal < 0` and take " +
			"ErrCodeGroupingError on legal SQL. MEASURED, it does not: " +
			"bindPostAggregateValueToNativeOrdinals rebinds the walked nested key to a " +
			"SINGLE-accessor reference over the aggregate's native output row BEFORE this " +
			"test runs, so the test sees 1 accessor and binds the slot. `SELECT id, (SELECT " +
			"max(n2.id) FROM nested AS n2 WHERE n2.id = nested.id GROUP BY n2.r.v.z ORDER BY " +
			"n2.r.v.z LIMIT 1) FROM nested` answers. The arity test is the CONSUMER of that " +
			"rebind, not a second gate in front of it.",
	},

	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldValue.descendResolvedPath": {
		class: arityNestingOK, exprs: 1,
		why: "this IS the nested descent — `<= 1` is the nothing-to-descend fast path, and " +
			"everything past it walks Accessors[1:].",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.ReAnchorRootInto": {
		class: arityNestingOK, exprs: 2,
		why: "requires `>= 2` because it EXISTS for multi-accessor paths: derives the root " +
			"ordinal from the target layout, asserts the carried one against it, preserves " +
			"the suffix verbatim.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.RootOrdinalIn": {
		class: arityNestingOK, exprs: 1,
		why: "the arity-TOLERANT counterpart to OrdinalIn — guards emptiness only and answers " +
			"about the root, which is exactly the question a nested path can answer.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.WithSuffix": {
		class: arityNestingOK, exprs: 3,
		why: "the fuse itself: concatenates both accessor lists. The len() uses are an " +
			"empty-suffix guard and a capacity computation.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.Last": {
		class: arityNestingOK, exprs: 1,
		why: "indexes the final accessor — the multi-accessor case is the point of the " +
			"method, not an exclusion.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#FieldPath.Equals": {
		class: arityNestingOK, exprs: 2,
		why: "element-wise ordinal equality over the whole accessor list (Java " +
			"FieldValue.java:411-420). Arity participates as LENGTH, not as a refusal.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#NestedResolvedPath": {
		class: arityNestingOK, exprs: 1,
		why: "the nesting PREDICATE — `> 1` selects the nested arm. It is the shared " +
			"authority the naming sites ask.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#ProjectionOutputIdentityKey": {
		class: arityNestingOK, exprs: 1,
		why: "folds every accessor's Field into the identity key for a multi-accessor path, " +
			"so two nested reads that render differently stay distinct.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#explainValueOrdinals": {
		class: arityNestingOK, exprs: 2,
		why: "renders every step as name#ordinal, dot-joined (Java FieldPath.toString, " +
			"FieldValue.java:428-433); the single-accessor rendering is its one-step case.",
	},
	"pkg/recordlayer/query/plan/cascades/values/column_identity.go#statesColumnPath": {
		class: arityNestingOK, exprs: 1,
		why: "guards emptiness, then requires a non-negative ordinal at EVERY step — " +
			"arity-tolerant by construction.",
	},
	"pkg/recordlayer/query/plan/cascades/values/column_identity.go#SameColumnPath": {
		class: arityNestingOK, exprs: 3,
		why: "element-wise over both paths with a length equality; multi-accessor paths " +
			"compare correctly.",
	},
	"pkg/recordlayer/query/plan/cascades/values/replace.go#fieldValueLegOrdinal": {
		class: arityNestingOK, exprs: 1,
		why: "root-only by design (`> 0` guard, Accessors[0]). Its caller " +
			"LegAwareRootOrdinal rebases the ROOT and the surrounding collapse re-fuses " +
			"Accessors[1:] onto the resolved slot — the suffix is preserved, not dropped.",
	},
	"pkg/recordlayer/query/plan/cascades/left_outer_existential.go#rebaseOuterLegValueOrdinal": {
		class: arityNestingOK, exprs: 1,
		why: "carries an EXPLICIT nested arm above the pinned/name dispatch, precisely " +
			"because arity is orthogonal to the frontier pin. Its own comment records the " +
			"wrong-rows defect that arm fixed: the name arm baked the merged address of the " +
			"STRUCT and dropped the descent, so an EXISTS correlation compared a BIGINT " +
			"against a whole struct and quietly matched nothing. Fixed on master — this is " +
			"a (d) that has already been closed here.",
	},
	"pkg/recordlayer/query/executor/ordinal_join.go#resolveSpanLeaf": {
		class: arityNestingOK, exprs: 1,
		why: "loops WHILE the path is multi-accessor, walking a leg at a time and composing " +
			"the remaining accessors onto each slot's own path.",
	},
	"pkg/recordlayer/query/plan/cascades/match_info_merge.go#decomposeGroupFieldPath": {
		class: arityNestingOK, exprs: 1,
		why: "guards emptiness, then emits one path step per accessor — normalising fused " +
			"and chained forms into one root-to-leaf path.",
	},
	"pkg/recordlayer/query/plan/cascades/max_match_map.go#expandValueForMatching": {
		class: arityNestingOK, exprs: 1,
		why: "`>= 2` SELECTS the fused case: Java's ExpandFusedFieldValueRule, emitting " +
			"every split form so a candidate in any partial-to-full shape matches.",
	},
	"pkg/recordlayer/query/plan/cascades/leg_local_bake_census.go#describeLegIdentityDecline": {
		class: arityNestingOK, exprs: 2,
		why: "diagnostic string formatting (`accessors=%d`), not a decision. Arity is " +
			"REPORTED here, never tested.",
	},
	"pkg/relational/core/query/cascades_translator.go#fieldOntoCorrelatedScalarRow": {
		class: arityNestingOK, exprs: 1,
		why: "explicitly preserves a structured-column suffix: rebases the root onto the " +
			"materialized [outer..., scalar] row and re-attaches Accessors[1:] with " +
			"WithSuffix.",
	},
	"pkg/relational/core/query/cascades_translator.go#rebaseCorrelatedScalarFilterPredicate": {
		class: arityNestingOK, exprs: 2,
		why: "guards emptiness only, reads Root().Ordinal, and hands the value to " +
			"fieldOntoCorrelatedScalarRow, which keeps the suffix.",
	},
	"pkg/relational/core/query/ddl/generator_predicate.go#indexPredicateIsSupported": {
		class: arityNestingOK, exprs: 1,
		why: "guards emptiness, then requires a NAME at every accessor — a nested index " +
			"predicate path is supported, an unnamed step is not.",
	},
	"pkg/relational/core/query/ddl/generator_predicate.go#comparisonPredicateToProto": {
		class: arityNestingOK, exprs: 1,
		why: "builds the stored field-path name list from EVERY accessor — Java's " +
			"getFieldPathNames (IndexPredicate.java:562-566).",
	},

	"pkg/relational/core/embedded/cascades_generator.go#deriveColumnsFromProjection": {
		class: arityNestingOK, exprs: 3,
		why: "WAS (?), THEN (d) LIVE DEFECT, NOW FIXED. RFC-230's base recorded the (d) and " +
			"instructed that the entry be RE-READ rather than carried if the fix landed " +
			"afterwards; it did, and this is that re-read. It stays as an entry rather than " +
			"being deleted because a (d) that outlives its defect and a (d) that silently " +
			"vanishes are the same unwatched-revival failure — the class moved because the " +
			"CODE moved, not because the verdict was revised. THE CONTROL THAT IDENTIFIES " +
			"THE MECHANISM, kept from that base: `q.s.label`, a SCALAR member under the same " +
			"projection, was always CORRECT (\"STRING\"), because the catalog types it and " +
			"the fall-through never runs; and the identical kinds at TOP level " +
			"(`top BIGINT ARRAY`, `topbin BYTES`) reported BIGINT and BINARY in both shapes, " +
			"so the defect was nesting-specific and not an array- or bytes-typing gap. " +
			"THE DETAIL: the fall-through was REACHABLE and it was a LIVE " +
			"DEFECT, now fixed at its cause. The two arity tests gate ORDINAL type " +
			"inheritance to single-accessor reads and that gate was always right — a " +
			"leg-relative multi-accessor root ordinal is not an index into the flattened " +
			"inner columns. What could not be decided by reading was the FALL-THROUGH it " +
			"selects, `innerByName[fv.Field]`. Measured by instrumenting the arm and running " +
			"a real query: `SELECT s.vals FROM t` over `STRUCT sarr(vals BIGINT ARRAY,...)` " +
			"reaches it with accs=2, because an ARRAY leaf is the one leaf valueTypeName had " +
			"no name for, so TypeName really did arrive UNKNOWN. The prior entry's reason " +
			"for doubt (\"the catalog types the leaf, so TypeName is known\") held for every " +
			"SCALAR leaf and for no other. With fuseNestedAccessors naming the reference " +
			"after the struct ROOT the lookup found the ROOT COLUMN — `found=true`, a " +
			"different column of a different type. Over a base scan the hit was discarded by " +
			"the arm's own `ic.TypeName != \"UNKNOWN\"` guard (a scan derives a struct " +
			"column UNKNOWN), which is why no test saw it; put a PROJECTION underneath and " +
			"the guard passes, and `SELECT q.s.vals FROM (SELECT s FROM t) AS q` reported " +
			"STRUCT — the enclosing struct's type — for a BIGINT ARRAY member. FIXED at the " +
			"MINT, not here: fuseNestedAccessors now names the fused value after its LEAF, " +
			"agreeing with every other mint of the shape and with Java's getLastFieldName " +
			"(FieldValue.java:134-135/463-466), so the lookup asks for the member it means. " +
			"valueTypeName also grew the ARRAY arm that made the shape reachable. Pinned " +
			"end-to-end by TestFDB_NestedArrayLeafDoesNotInheritTheStructRootsMetadata. " +
			"THE SITE HAS SINCE BEEN EDITED, and the earlier text here claimed it never " +
			"would need to be — that fixing the mint was sufficient because the arm 'handles " +
			"a multi-accessor path correctly once it is handed a correctly-named one'. That " +
			"is true of the ORDINAL arms and false of the three NAME arms, which recover a " +
			"column from the reference's single display name whatever that name is. A " +
			"correctly-named fused value still offers a LEAF, and a leaf lives in the " +
			"enclosing struct's namespace while these maps are keyed by the record's; a " +
			"shared spelling types the column from an unrelated one. A third expression now " +
			"declines inheritance outright for a multi-accessor reference, which leaves the " +
			"leaf's own type standing — Java's answer (FieldValue.computeResultType is " +
			"fieldPath.getLastFieldType, FieldValue.java:143-148). NOT reached by any query " +
			"in //pkg/relational/... — instrumented and measured at 0 hits across the full " +
			"suite including the FDB corpus, so the gate is fail-safe rather than " +
			"corpus-proven, and that is recorded here rather than dressed up as coverage.",
	},

	"pkg/relational/core/embedded/cascades_generator.go#operandTypeNameViaDesc": {
		class: arityNestingOK, exprs: 1,
		why: "THE SITE THIS CENSUS COULD NOT HAVE SEEN, and the one the live defect was " +
			"actually at — see the header's blind-spot note. It read `t.Field` straight into " +
			"a top-level descriptor lookup with NO arity test, so it was absent from a " +
			"`len(...Accessors)` sweep by the same property that made it wrong. For an " +
			"arithmetic operand that is a fused nested reference the lookup asked the " +
			"RECORD's namespace for a name minted in a STRUCT's, and a shared spelling " +
			"answered: with `STRUCT nn(sk BIGINT)` beside a top-level `sk DOUBLE`, " +
			"`SELECT n.sk + 1` reported DOUBLE where BIGINT is right. MEASURED both ways — " +
			"the defect appeared exactly when fuseNestedAccessors began naming a fused value " +
			"after its LEAF, because the previous ROOT name (`N`) was a MessageKind that " +
			"protoFieldTypeName answered UNKNOWN for, so the guard skipped and the value's " +
			"own type won. Master was therefore right BY ACCIDENT, and the accident does not " +
			"survive a leaf name that names a real flat column. FIXED by declining the " +
			"descriptor for a multi-accessor reference and returning the value's own type, " +
			"which is Java's rule and not a Go choice: FieldValue.computeResultType is " +
			"`fieldPath.getLastFieldType()` (FieldValue.java:143-148) and there is no " +
			"name-keyed re-derivation upstream of it anywhere — this whole function is a " +
			"Go-only invention. Pinned by " +
			"TestFDB_NestedOperandTypeComesFromItsOwnLeafNotAFlatNamesake, whose fixture " +
			"KEEPS the type collision because without it the test is green either way.",
	},

	"pkg/relational/core/query/cascades_translator.go#bakeUnnestElementRefOrdinal": {
		class: arityNestingOK, exprs: 1,
		why: "The site HANDLES multi-accessor paths; it is not a gate on them. A reference " +
			"whose ROOT names an unnest element slot is baked onto that slot over the merged " +
			"row, and `len(Accessors) > 1` selects the extra work a DESCENT needs — carrying " +
			"`Accessors[1:]` onto the baked root via FieldPath.WithSuffix — rather than " +
			"refusing it. Only the root read moves; the pin and domain come from the root " +
			"step, which is what makes the fused node machinery-owned. " +
			"THIS SITE USED TO BE A DECLINE AND THAT WAS A LIVE DEFECT (class (d)), which " +
			"is why the arity expression here is now a branch and not a condition: the " +
			"function selected candidates with `SourceRelativeBaked()`, which additionally " +
			"requires len(Accessors) == 1, so a struct-element MEMBER (`x.ek` — two unpinned " +
			"accessors) was skipped, reached the merged row still addressing the EXISTS " +
			"scope's own layout, and EXISTS dropped every row SILENTLY. MEASURED: " +
			"`SELECT x.ek FROM t, t.arr AS x WHERE EXISTS (SELECT 1 FROM t AS m WHERE " +
			"m.id = 1 AND x.ek = 10)` returned `EK|` where `EK|10` is correct. Candidate " +
			"selection now keys on RootIsLegRelativeUnpinned(), which excludes the real " +
			"invariant (machinery-owned FrontierPinned nodes) at any arity — the same " +
			"correction legRef in ordinal_seed.go received. Pinned by " +
			"TestFDB_UnnestElementMemberInExists (the outer_element_* arms) and " +
			"TestFDB_UnnestElementMemberInExistsConvertedSentinel, both of which assert " +
			"ROWS: the failure mode is a silent empty, so an absence-of-error assertion " +
			"would pass with the defect fully present.",
	},

	"pkg/relational/core/query/cascades_translator.go#rewriteUnnestPredicate": {
		class: arityCorrectDecline, exprs: 2,
		why: "The element-substitution arm rewrites a reference that IS the bound unnest " +
			"element into the element's quantifier object, selecting that arm by comparing " +
			"the reference's single display name against the binding's AS/AT alias. A " +
			"DESCENT into the element is never the element, so it must not reach those arms: " +
			"the QOV they return carries the accessor suffix nowhere, and substituting it " +
			"reads the whole struct where a member was named. MEASURED, and the measurement " +
			"is why this is a decline rather than a shrug: under the mint that named a fused " +
			"value after its struct ROOT the comparison matched for EVERY member reference, " +
			"because the root of a descent through an unnest binding IS the alias — " +
			"`WHERE i.sku = 'x'` over `orders.items AS i` reached this switch with " +
			"Field=\"I\" and MATCHED. Naming the value after its LEAF narrowed that to the " +
			"shapes where a member is spelled like the alias, which the resolver currently " +
			"refuses 42702 before any consumer sees it (pinned by " +
			"TestFDB_NestedMemberSpelledLikeItsUnnestAliasIsRefused — a NEGATIVE result " +
			"pinned because this gate's reachability rests on it). The gate closes the " +
			"remainder so the arm is correct without depending on that refusal. " +
			"NO REPRODUCER for the decline itself — the same epistemic class as " +
			"rebaseUnnestOuterLegPredicateOrdinal and deriveColumnsFromProjection's name " +
			"arms, and stated as plainly here so the MEASURED history above does not read " +
			"as coverage it does not have. What was measured is an ARM-LEVEL wrong READ " +
			"under the old mint. " +
			"THE MASKING CLAIM THIS ENTRY USED TO CARRY IS SUPERSEDED and must not be " +
			"carried forward: it said the emblematic shape (`WHERE i.sku = 'x'` over " +
			"`orders.items AS i`) returns ZERO ROWS identically before and after, so no " +
			"query's rows were demonstrably corrected. That predicate shape ANSWERS — " +
			"MEASURED, `SELECT x.ek FROM t, t.arr AS x WHERE x.d.dk = 91` returns `EK|10`, " +
			"pinned by TestFDB_UnnestElementMemberInExists's " +
			"control_two_level_member_beside_exists arm — so the masking defect the claim " +
			"rested on is not there to mask anything. " +
			"SECOND EXPRESSION: the member-rebase arm, which is the positive twin of the " +
			"decline above and is class (c). A CHILDLESS multi-accessor path over the " +
			"EXISTS scope's one-column virtual source IS a member of a correlated-primary " +
			"unnest element, and `len(Accessors) > 1` selects it for rebasing onto the " +
			"Explode's flowed element rather than refusing it. The two expressions are " +
			"deliberately opposite readings of the same arity fact and belong together: " +
			"arity says 'not the element itself', the first arm concludes 'so do not " +
			"substitute the whole element' and the second concludes 'so bind it as a " +
			"member'. Pinned by rows — TestFDB_UnnestElementMemberInExists's " +
			"flat_member_in_exists / two_level_member_in_exists / both_depths_in_exists " +
			"arms, each of which goes RED with an unbound-context error when the rebase is " +
			"disabled.",
	},

	"pkg/relational/core/query/cascades_translator.go#rebaseUnnestOuterLegPredicateOrdinal": {
		class: arityCorrectDecline, exprs: 1,
		why: "The rebase resolves a slot by NAME within the qualifier's per-leg window and " +
			"bakes it with NewFieldValueOfOrdinal, which keeps the ordinal and DROPS the " +
			"accessor suffix. For a multi-accessor reference that is a read of the struct " +
			"ROOT where a member was named — and it is silent, because the name offered to " +
			"the window is one segment of a path: it either misses (the rebase declines, " +
			"which is fine) or hits a DIFFERENT column sharing the leaf's spelling, which " +
			"is not. Declining is the fail-closed direction this function already takes for " +
			"an unresolvable slot, and it is the honest one — a rebase that cannot carry the " +
			"suffix must not pretend it did. NOT reached by any query in //pkg/relational/... " +
			"— instrumented and measured at 0 hits across the full suite including the FDB " +
			"corpus. That is a statement about the corpus's coverage of correlated nested " +
			"references over a multi-leg outer, NOT a proof of unreachability, and it is " +
			"recorded that way on purpose.",
	},
}

// Population facts, MEASURED at the base this census was taken against. Each is
// asserted independently so a shift between them (e.g. a code line becoming a
// comment) is not absorbed silently by the total.
const (
	// rfcPublishedPopulation started as RFC-230 rev 6 §7.3's sweep-2 result:
	// `git grep -n "len(.*Accessors)" -- '*.go' | grep -v _test.go | wc -l`,
	// reproduced exactly at 52. It is a count of grep LINES, not of gates, and it
	// is a MEASUREMENT rather than a budget — it moves when the tree does, and the
	// mover has to say what moved.
	//
	// 52 → 56: four arity gates were ADDED, closing name-keyed reads that took a
	// fused reference's single display name and looked it up in a namespace that
	// is not the one the name came from. Three are new symbols
	// (operandTypeNameViaDesc, rewriteUnnestPredicate,
	// rebaseUnnestOuterLegPredicateOrdinal) and the fourth is a third expression
	// inside deriveColumnsFromProjection, which was already classified.
	// 56 → 58: two of those gates are spelled `SourceRelativeBaked()`, whose name
	// does not convey that it requires len(Accessors) == 1, so each carries a
	// one-line comment SAYING it does — and those comments quote the predicate,
	// which the sweep's regexp matches. The lines are prose, not gates, so they
	// land in arityCommentLines and the CODE population is unchanged. Worth
	// knowing before someone "fixes" the drift by rewording a comment: making the
	// arity meaning legible is exactly the readability failure these comments
	// exist to prevent, and it costs two lines in the wrong bucket.
	// 58 → 60: two arity sites were added by the fix that made a struct-element
	// MEMBER reference resolve inside an EXISTS body. Both are class (c) — they
	// HANDLE a multi-accessor path rather than gating on it — and both are new
	// expressions inside functions the census already classified:
	// bakeUnnestElementRefOrdinal (which now fuses `Accessors[1:]` onto the baked
	// element slot; it previously SKIPPED such a path, which was a live silent
	// 0-row defect) and a second expression in rewriteUnnestPredicate (the
	// member-rebase arm, the positive twin of that function's existing decline).
	// Both land in the CODE population, so this is the first move since 52 → 56
	// that changes arityCodeLines rather than only the comment bucket.
	rfcPublishedPopulation = 60
	// The decomposition of those 60 lines.
	arityGeneratedLines = 8  // protobuf marshal loops over PFieldPath.FieldAccessors
	arityCommentLines   = 3  // prose inside a doc comment (see above: 1 + 2 legibility notes)
	arityCodeLines      = 49 // the real population
	// Four of the 49 code lines hold more than one arity expression.
	arityExpressions = 53
)

// accessorArityLine is the RFC's own sweep, as a regexp.
var accessorArityLine = regexp.MustCompile(`len\(.*Accessors\)`)

// TestAccessorArityCensusIsClassified pins the classified population. It fails
// when a new arity site appears, when one disappears, or when an already
// classified symbol gains or loses an arity expression.
func TestAccessorArityCensusIsClassified(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)

	var (
		rawLines, genLines, commentLines, codeLines int
		scanned                                     int
		liveExprs                                   int
	)
	live := map[string]int{}

	for _, rel := range trackedGoFiles(t, root) {
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		scanned++
		if !bytes.Contains(src, []byte("Accessors")) {
			continue
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		generated := ast.IsGenerated(f)

		// Line arm: reproduce the RFC's grep exactly, then classify each hit as
		// generated / comment-only / code so the published 52 stays checkable
		// AND its decomposition stays honest.
		commentLine := map[int]bool{}
		for _, group := range f.Comments {
			for _, c := range group.List {
				lo := fset.Position(c.Pos()).Line
				hi := fset.Position(c.End()).Line
				for ln := lo; ln <= hi; ln++ {
					commentLine[ln] = true
				}
			}
		}
		exprLine := map[int]bool{}
		symbols := funcSpansOf(fset, f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "len" || len(call.Args) != 1 {
				return true
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, call.Args[0]); err != nil {
				t.Fatalf("render len() argument in %s: %v", rel, err)
			}
			if !strings.HasSuffix(buf.String(), "Accessors") {
				return true
			}
			exprLine[fset.Position(call.Pos()).Line] = true
			if generated {
				return true
			}
			liveExprs++
			live[rel+"#"+enclosingSymbol(symbols, call.Pos())]++
			return true
		})

		for i, line := range strings.Split(string(src), "\n") {
			if !accessorArityLine.MatchString(line) {
				continue
			}
			rawLines++
			switch {
			case generated:
				genLines++
			case exprLine[i+1]:
				codeLines++
			case commentLine[i+1]:
				commentLines++
			default:
				t.Errorf("%s:%d matches the arity sweep but is neither generated, an "+
					"expression, nor a comment — the census decomposition does not cover "+
					"it:\n\t%s", rel, i+1, strings.TrimSpace(line))
			}
		}
	}

	// ---- Anti-vacuity. A green from an empty set is the dominant false
	// positive here: an empty scan classifies nothing and reports success.
	if scanned < 100 {
		t.Fatalf("the census scanned only %d production Go files under %s — that is not the "+
			"real source tree. Fix sourceTreeRoot/runfiles staging; do NOT trust this run",
			scanned, root)
	}
	if liveExprs == 0 || len(live) == 0 {
		t.Fatalf("the census found ZERO arity expressions across %d files. The sweep is "+
			"broken, not the tree — an empty population cannot classify anything", scanned)
	}

	// ---- The RFC's published number, and its decomposition.
	if rawLines != rfcPublishedPopulation {
		t.Errorf("the arity sweep now matches %d non-test lines, RFC-230 rev 6 §7.3 published "+
			"%d.\n\tRe-run: git grep -n \"len(.*Accessors)\" -- '*.go' | grep -v _test.go\n"+
			"\tA changed population is a finding, not a nit: classify what moved and update "+
			"this census and the RFC together.", rawLines, rfcPublishedPopulation)
	}
	for _, c := range []struct {
		name        string
		got, pinned int
	}{
		{"generated (protobuf marshal loops, not gates)", genLines, arityGeneratedLines},
		{"comment-only (prose, not a gate)", commentLines, arityCommentLines},
		{"code lines (the real population)", codeLines, arityCodeLines},
		{"arity expressions across those code lines", liveExprs, arityExpressions},
	} {
		if c.got != c.pinned {
			t.Errorf("arity population decomposition drifted — %s: got %d, pinned %d",
				c.name, c.got, c.pinned)
		}
	}

	// ---- The classification itself.
	var unclassified, stale []string
	for key, n := range live {
		site, known := accessorAritySites[key]
		if !known {
			unclassified = append(unclassified, fmt.Sprintf("%s (%d expression(s))", key, n))
			continue
		}
		if site.exprs != n {
			t.Errorf("%s now holds %d arity expression(s), the census pinned %d.\n"+
				"\tRe-read the symbol: an added arity test in an already-classified function "+
				"does NOT inherit its neighbour's verdict. Classify the new expression, then "+
				"update `exprs`.", key, n, site.exprs)
		}
	}
	for key := range accessorAritySites {
		if _, still := live[key]; !still {
			stale = append(stale, key)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)

	if len(unclassified) > 0 {
		t.Errorf("%d arity site(s) are NOT classified:\n\t%s\n\n"+
			"WHAT TO DO: read the site AND its callers — not the shape of its condition; two "+
			"sites with identical `len(Accessors) != 1` checks classify differently. Decide "+
			"which it is:\n"+
			"\t(a) a correct decline  — the value genuinely cannot be what the site needs\n"+
			"\t(b) a blocker          — it would refuse a LEGITIMATE nested grouping key\n"+
			"\t(c) already correct    — it handles multi-accessor paths\n"+
			"\t(d) a live defect      — it mis-handles one TODAY; stop and fix that first\n"+
			"\t(?) uncertain          — record WHY; an honest unknown beats a wrong verdict\n"+
			"Then add it to accessorAritySites with its reason. Leaving it out is how this "+
			"census was wrong five times.",
			len(unclassified), strings.Join(unclassified, "\n\t"))
	}
	if len(stale) > 0 {
		t.Errorf("%d classified arity site(s) no longer exist:\n\t%s\n\n"+
			"A site that vanished was either fixed or renamed. If it was a (b) blocker that "+
			"RFC-230 resolved, delete the entry and say so in the RFC; if it merely moved, "+
			"re-key it. Do not leave a verdict pointing at nothing.",
			len(stale), strings.Join(stale, "\n\t"))
	}
}

// TestAccessorArityClassCounts pins the per-class totals. The site table above
// could drift class-by-class while staying the same size; these counts are the
// summary RFC-230 quotes, so they are asserted rather than recomputed by a
// reader.
func TestAccessorArityClassCounts(t *testing.T) {
	t.Parallel()

	// RECLASSIFICATION MOVES TWO CELLS, NEVER ONE — this is a POPULATION, so a
	// member leaving a class must arrive somewhere, and the total is the check
	// that it did.
	//
	// THIS IS THE RECONCILIATION OF TWO CHANGES THAT BOTH EDITED THIS FILE, done
	// as arithmetic rather than by picking a side. RFC-230 landed first and left
	// the base at (a) 13, (b) 0, (c) 22, (d) 1, (?) 0 — total 36: it retired all
	// three (b) BLOCKERS to (a) after instrumenting the arms each was said to
	// block, and it recorded deriveColumnsFromProjection as (d) LIVE DEFECT with
	// an explicit instruction to RE-READ rather than carry that entry if the fix
	// landed afterwards. It did. Two independent movements then apply:
	//
	//	the FIX      (d) 1 → 0, (c) 22 → 23   deriveColumnsFromProjection,
	//	                                      fixed at the mint, not reclassified
	//	the GATES    (a) 13 → 15, (c) 23 → 24  three name-keyed reads that had NO
	//	                                      arity test are now gated, so each
	//	                                      becomes a site this census can SEE
	//	                                      for the first time
	//
	// giving (a) 15, (b) 0, (c) 24, (d) 0, (?) 0 — total 39, and 36 symbols → 39.
	// The gates are rewriteUnnestPredicate and
	// rebaseUnnestOuterLegPredicateOrdinal (both (a) — they decline, fail-closed)
	// and operandTypeNameViaDesc ((c) — it answers correctly for a multi-accessor
	// value). A fourth gate is a third expression inside
	// deriveColumnsFromProjection and so moves exprs, not the symbol count.
	//
	// FIXING AN UNGUARDED READ GROWS THIS POPULATION BY CONSTRUCTION — see the
	// header's blind-spot note. A rising count here is the instrument working,
	// not drift.
	pinned := map[accessorArityClass]int{
		arityCorrectDecline: 15,
		arityBlocker:        0,
		arityNestingOK:      25,
		arityLiveDefect:     0,
		arityUncertain:      0,
	}

	got := map[accessorArityClass]int{}
	for key, site := range accessorAritySites {
		switch site.class {
		case arityCorrectDecline, arityBlocker, arityNestingOK, arityLiveDefect, arityUncertain:
		default:
			t.Errorf("%s carries an unknown class %q", key, site.class)
		}
		if strings.TrimSpace(site.why) == "" {
			t.Errorf("%s is classified %q with no reason. A verdict without a reason is the "+
				"thing this census exists to replace", key, site.class)
		}
		got[site.class]++
	}

	total := 0
	for class, want := range pinned {
		if got[class] != want {
			t.Errorf("class (%s): %d sites, pinned %d — reclassifying a site is a real "+
				"decision, so it moves this number deliberately", class, got[class], want)
		}
		total += want
	}
	if total != len(accessorAritySites) {
		t.Errorf("class counts sum to %d but the site table holds %d entries",
			total, len(accessorAritySites))
	}

	// THE (d) FLOOR IS RESTORED, AND ITS ALARM DIRECTION IS GROWTH AGAIN.
	// RFC-230's base deleted it on a premise that was correct at the time: zero
	// had stopped being the steady state, because deriveColumnsFromProjection had
	// resolved from (?) to a live wrong-column type read, and a floor demanding
	// zero would have been unsatisfiable. That premise EXPIRED when the fix
	// landed. Zero is the steady state once more, so the floor comes back rather
	// than staying deleted — a deleted floor is an unwatched revival, which is
	// the same failure mode as a (d) that outlives its defect.
	//
	// READ THE ZERO CORRECTLY: it means FOUND AND FIXED, twice, not "never
	// present". Two sites have held (d), and both returned to zero BY A FIX and
	// not by a reclassification:
	//
	//   - deriveColumnsFromProjection, reached through an ARRAY leaf of a struct;
	//   - operandTypeNameViaDesc, which this census could not see AT ALL because
	//     it never tested arity (header, blind-spot note). It reported an
	//     arithmetic operand's type from a top-level column that merely shared
	//     the nested leaf's spelling.
	//
	// The second is the one to remember when reading this zero, because it says
	// what the zero does NOT cover: a (d) can only be counted here once someone
	// has written the gate that makes the site visible. A live defect can be
	// sitting in an unguarded read right now and this floor will report zero.
	if got[arityLiveDefect] > 0 {
		t.Errorf("%d site(s) classified (d) LIVE DEFECT. Zero is the measured steady state "+
			"on this base and the alarm direction here is GROWTH, so this is a REGRESSION "+
			"rather than a backlog entry. Stop the survey: verify each end-to-end against "+
			"real FDB with values that make the wrong answer visible as wrong DATA, fix it, "+
			"and pin the reproducer — before any further work builds on top of it",
			got[arityLiveDefect])
	}

	// Kept from RFC-230's base and still load-bearing even at a population of
	// zero: a (d) is only ever recordable from a MEASUREMENT. Every wrong
	// classification this census has produced came from reading a condition and
	// reasoning about it — three (b) blockers refuted that way, and
	// deriveColumnsFromProjection sat at (?) for two revisions because reading
	// could not settle it. So an entry must show it was executed, not argued.
	// This is retained for the NEXT (d) rather than deleted along with the last
	// one; it runs over an empty population today and costs nothing.
	for key, site := range accessorAritySites {
		if site.class != arityLiveDefect {
			continue
		}
		if !strings.Contains(site.why, "MEASURED") {
			t.Errorf("%s is classified (d) LIVE DEFECT with no measurement in its reason.\n"+
				"\tA live defect is a claim about what the engine DOES, and this census has "+
				"been wrong every time it answered that by reading. Run it end-to-end "+
				"against real FDB with values that make the wrong answer visible as wrong "+
				"DATA, record what you saw, and keep a control that separates the defect "+
				"from the shape merely being unsupported.", key)
		}
	}

	// The (?) guard, whose direction INVERTED when the last unknown was
	// resolved. While a (?) existed the danger was that it quietly stayed one —
	// an unknown carried long enough to read as a verdict. It is now zero and
	// the danger is GROWTH: a new (?) means the survey has stopped being able to
	// answer for a site, and the site it could not answer for last time turned
	// out to be a live defect that a downstream guard was accidentally masking.
	// So a (?) is a STOP, not a bookkeeping entry. The pinned-count check above
	// already fails on it; this says what the failure MEANS.
	if got[arityUncertain] > 0 {
		t.Errorf("%d site(s) are back to (?) UNCERTAIN. The census reached a complete "+
			"classification; re-opening one is a finding. Instrument the site and run a real "+
			"query through it rather than reasoning about reachability from the source — "+
			"that is what settled the last one, and reading it had produced the wrong answer "+
			"twice", got[arityUncertain])
	}

	// The (b) floor HAS INVERTED, and the inversion is retired deliberately
	// rather than deleted. It used to guard against a quiet drift to zero,
	// because zero would have meant RFC-230's Phase 1 was silently complete.
	// Zero is now the MEASURED steady state: nested-path GROUP BY landed with
	// all three former blockers unchanged (RFC-230 §7.4), because each was
	// classified from reading and each was refuted by instrumenting the arm it
	// actually takes. So the alarm direction flips to GROWTH — a (b) here means
	// a site has been found that genuinely refuses a legitimate nested grouping
	// key, i.e. a capability that works today has been taken away or a new arm
	// blocks a shape the shipped tests do not cover.
	if got[arityBlocker] > 0 {
		t.Errorf("%d site(s) classified (b) BLOCKER. Nested-path GROUP BY is IMPLEMENTED and "+
			"its shapes are pinned end-to-end, so a blocker is no longer a to-do item — it is "+
			"a refusal of something that works. Name the SQL shape it blocks and show it "+
			"failing against real FDB before recording the class", got[arityBlocker])
	}
}

// funcSpan is one top-level function's name and source extent.
type funcSpan struct {
	name   string
	lo, hi token.Pos
}

// funcSpansOf lists every top-level func in f, methods rendered `Recv.Name`.
func funcSpansOf(fset *token.FileSet, f *ast.File) []funcSpan {
	var spans []funcSpan
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			var buf bytes.Buffer
			if printer.Fprint(&buf, fset, fd.Recv.List[0].Type) == nil {
				name = strings.TrimPrefix(buf.String(), "*") + "." + name
			}
		}
		spans = append(spans, funcSpan{name: name, lo: fd.Pos(), hi: fd.End()})
	}
	return spans
}

// enclosingSymbol names the innermost listed func containing pos. A site at
// file scope (a package-level var initializer) is keyed "<file scope>" rather
// than dropped — an unnamed site still has to be classified.
func enclosingSymbol(spans []funcSpan, pos token.Pos) string {
	name := "<file scope>"
	for _, s := range spans {
		if pos >= s.lo && pos < s.hi {
			name = s.name
		}
	}
	return name
}
