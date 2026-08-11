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

	// ---------------------------------------------------------------------
	// (?) UNCERTAIN — recorded with the reason, not guessed.
	// ---------------------------------------------------------------------

	// ---------------------------------------------------------------------
	// (d) LIVE DEFECT — mis-handled on this base RIGHT NOW.
	// ---------------------------------------------------------------------

	"pkg/relational/core/embedded/cascades_generator.go#deriveColumnsFromProjection": {
		class: arityLiveDefect, exprs: 2,
		why: "WAS (?), and the uncertainty was resolved the only way it could be — by " +
			"instrumenting the arm and running a query, not by reading. The two arity tests " +
			"gate ORDINAL type inheritance to single-accessor reads and that gate is right; " +
			"the defect is the FALL-THROUGH it selects, `innerByName[fv.Field]`, where for " +
			"a fused nested reference `Field` is the struct ROOT. The open question was " +
			"whether the enclosing `TypeName == \"\" || == \"UNKNOWN\"` precondition is " +
			"reachable for a nested reference; the reason for doubt was that the catalog " +
			"types the leaf, so TypeName is normally known. That holds for every SCALAR " +
			"leaf and for no other: an ARRAY or BYTES leaf is a kind the type derivation " +
			"has no name for. MEASURED end-to-end on this base — " +
			"`SELECT q.s.vals FROM (SELECT s FROM t) AS q` over " +
			"`STRUCT sst (top BIGINT, vals BIGINT ARRAY)` reports DatabaseTypeName " +
			"\"STRUCT\" for a BIGINT ARRAY member: the lookup found the struct ROOT, a " +
			"different column of a different type. Over a BASE scan the same read reports " +
			"UNKNOWN — swallowed by the arm's own `ic.TypeName != \"UNKNOWN\"` guard, which " +
			"is exactly why the suite stayed green; a PROJECTION underneath types the root " +
			"and the wrong hit fires. The identical column at TOP level " +
			"(`vals2 BIGINT ARRAY`) reports BIGINT in both shapes, which is the control. " +
			"FIXED on branch rfc/231-field-mint-reconcile, not on this one — this entry " +
			"records the state of THIS base.",
	},
}

// Population facts, MEASURED at the base this census was taken against. Each is
// asserted independently so a shift between them (e.g. a code line becoming a
// comment) is not absorbed silently by the total.
const (
	// rfcPublishedPopulation is RFC-230 rev 6 §7.3's sweep-2 result:
	// `git grep -n "len(.*Accessors)" -- '*.go' | grep -v _test.go | wc -l`.
	// Reproduced exactly. It is a count of grep LINES, not of gates.
	rfcPublishedPopulation = 52
	// The decomposition of those 52 lines.
	arityGeneratedLines = 8  // protobuf marshal loops over PFieldPath.FieldAccessors
	arityCommentLines   = 1  // prose inside a doc comment
	arityCodeLines      = 43 // the real population
	// Four of the 43 code lines hold more than one arity expression.
	arityExpressions = 47
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
	// member leaving (?) must arrive somewhere and the total is the check that
	// it did. deriveColumnsFromProjection went (?) -> (d): uncertain 1 -> 0,
	// live defect 0 -> 1, total 36 unchanged.
	pinned := map[accessorArityClass]int{
		arityCorrectDecline: 13,
		arityBlocker:        0,
		arityNestingOK:      22,
		arityLiveDefect:     1,
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

	// THE (d) FLOOR HAS FIRED, AND IT IS RECONCILED RATHER THAN RELAXED. It said
	// zero was the measured steady state and the alarm direction was GROWTH.
	// Growth happened: deriveColumnsFromProjection was (?), and the uncertainty
	// resolved to a live wrong-column type read. So zero is no longer the
	// expected value and a floor demanding it would be unsatisfiable — the count
	// check above now owns the arithmetic in both directions (a SECOND (d), and
	// a silent drop back to none without the fix landing on this base).
	//
	// What is left for this block is the part a number cannot carry: a (d) is
	// only ever recordable from a MEASUREMENT. Every wrong classification this
	// census has produced came from reading a condition and reasoning about it —
	// three (b) blockers refuted that way, and this very site sat at (?) for two
	// revisions because reading could not settle it. So the entry must show it
	// was executed, not argued.
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
