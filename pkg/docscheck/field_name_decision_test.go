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

// allowedFieldDecisions are sites where returning/comparing display text is the
// operation's declared purpose. Exemptions are per site, never per file.
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
var allowedFieldDecisions = []fieldDecisionSite{
	{
		site: "pkg/recordlayer/query/plan/cascades/values/field_value.go # (fieldValue).DisplayName # the name escaping as a bare string (return) # 1",
		n:    1,
		why:  "DisplayName is the sealed read-only display-metadata accessor itself; Resolved supplies identity/path, but cannot supply the user-facing label this method explicitly returns. Decisions made by callers remain separately visible and are not exempted by this per-site entry.",
	},
	{
		site: "pkg/recordlayer/query/plan/cascades/values/values.go # DisplayColumnName # the name escaping as a bare string (return) via local leaf derived from the name # 1",
		n:    1,
		why:  "DisplayColumnName IS the user-visible projection label and decides nothing: its answer is a string a result set prints, never a lookup key or a comparison. It reaches the name THROUGH Resolved — the leaf is the last resolved accessor's Field, not a last-dot split of a rendered name — which is what lets a column legally named with a dot in it survive. Resolved cannot supply the label itself; supplying identity is what it already does here.",
	},
}

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
	// boundary (2)
	"pkg/recordlayer/query/plan/cascades/values/values.go # protoFieldByName # a != comparison via local name derived from the name # 1": {
		1, "boundary: proto descriptor descent still tries the escaped accessor spelling; retires when exact descriptor ordinals are resolved at construction.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go # protoFieldByName # a EqualFold call via local name derived from the name # 1": {
		1, "boundary: proto descriptor descent still has a case-insensitive name fallback; Java evaluates the resolved ordinal, so this retires with exact descriptor-path resolution.",
	},

	// escape (0)

	// contract (4)
	"pkg/recordlayer/query/plan/cascades/values/values.go # explainValueOrdinalsWithAliases # the name escaping as a bare string (return) via local name derived from the name # 1": {
		1, "contract: the ordinal-bearing renderer returns a display label used by output naming; retires when output slots carry stored names independently of value rendering.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go # explainValueOrdinalsWithAliases # the name escaping as a bare string (return) via local name derived from the name # 2": {
		1, "contract: the non-ordinal renderer is still an output-column naming authority; retires when positional output metadata no longer re-reads rendered values.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go # explainValueOrdinalsWithAliases # a dotted-name MINT (qualifier joined to the name) via local name derived from the name # 1": {
		1, "contract: Explain/column rendering joins a correlation label to the display leaf; the column-name face retires when naming is stored on the output slot.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go # ProjectionColumnName # the name escaping as a bare string (return) # 1": {
		1, "contract: projection output metadata is still named through the value renderer; retires when the projection stores its output label explicitly.",
	},

	// dotted (3)
	"pkg/recordlayer/query/plan/cascades/values/accessor_name_path.go # AccessorNamePath # a Contains call # 1": {
		1, "dotted: the lazy compatibility channel refuses to interpret a dotted display label as one accessor; retires when no unresolved name-only FieldValue can reach matching.",
	},
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a dotted-name MINT (qualifier joined to the name) # 1": {
		1, "dotted: projection metadata still emits one qualified display alias for downstream textual lookup; retires when column provenance is carried structurally.",
	},
	"pkg/relational/core/embedded/logical_predicate.go # (existsSubqueryPlanner).buildCorrelatedScalar # a dotted-name MINT (qualifier joined to the name) # 1": {
		1, "dotted: the correlated-scalar output row still exposes a LEG.COLUMN compatibility label; retires when its consumer addresses the exact leg window.",
	},

	// name-keyed (4)
	"pkg/recordlayer/query/plan/cascades/rule_implement_in_union.go # uniqueUpperFieldIndex # a EqualFold call via local name derived from the name # 1": {
		1, "name-keyed: IN-union compatibility resolution searches a result record by unique folded display name; retires when the comparison value carries its exact result ordinal.",
	},
	"pkg/relational/core/query/unnest_gather.go # slotInGatheredSeed # a map key via local col derived from the name # 1": {
		1, "name-keyed: the gathered-seed compatibility lookup uses a parsed column label after choosing the source leg; retires when the seed exposes an exact field window.",
	},
	"pkg/recordlayer/query/plan/cascades/referenced_fields.go # collectFieldNamesFromValue # a map key # 1": {
		1, "name-keyed: this legacy property explicitly collects display-name membership rather than column identity; retires when its consumers accept exact field paths.",
	},
	"pkg/recordlayer/query/plan/cascades/values/map_field_values.go # EqualsWithoutChildren # a == comparison # 1": {
		1, "name-keyed: the compatibility map-field node still compares its display field without children; retires when the node carries an exact accessor identity.",
	},

	// translator (4)
	"pkg/relational/core/embedded/cascades_generator.go # (legRead).column # a EqualFold call # 1": {
		1, "translator: parsed projection aliases are matched case-insensitively before exact column metadata is available; the emitted column must remain exact.",
	},
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a map key # 1": {
		1, "translator: bare projection display names seed the SQL-scope lookup map; retires when the logical projection stores resolved column identifiers.",
	},
	"pkg/relational/core/embedded/cascades_generator.go # deriveColumnsFromProjection # a map key via local qualified derived from the name # 1": {
		1, "translator: the qualified compatibility label seeds the same SQL-scope map; retires with structural projection provenance.",
	},
	"pkg/relational/core/query/cascades_translator.go # rewriteUnnestPredicate # a switch tag # 1": {
		1, "translator: the logical unnest boundary still selects declared element/ordinality aliases from SQL text; output accesses are exact ordinals after this boundary.",
	},

	// harness (0)
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
// Currently 25 escapes across 14 authorities. The gap is concentrated rather
// than noise: groupByOutputBaker and deriveColumnsFromProjection carry five
// each, and explainValueOrdinalsWithAliases carries three.
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
const fieldDebtAuthorityTotal = 12

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

// isFieldNameRead reports whether e reads a FieldValue's display name through
// either representation generation: the former concrete `.Field` field or the
// RFC-232 read-only `DisplayName()` interface accessor. Matching both is
// deliberate. A detector that followed only the removed concrete field went
// green when callers migrated to the accessor even though the same display
// string still reached the same decisions.
func isFieldNameRead(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel != nil && x.Sel.Name == "Field"
	case *ast.CallExpr:
		if len(x.Args) != 0 {
			return false
		}
		sel, ok := x.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel != nil && sel.Sel.Name == "DisplayName"
	}
	return false
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
	if isFieldNameRead(e) {
		return true
	}
	switch x := e.(type) {
	case *ast.Ident:
		return taint.has(x)
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
		case *ast.CallExpr:
			if isFieldNameRead(x) {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			// Only the RECEIVER side is searched. A selector's Sel is a bare
			// Ident, so descending into it would let a tainted local sharing a
			// method's name match a call that never touches the name.
			if isFieldNameRead(x) || containsFieldNameRead(x.X, taint) {
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
	if isFieldNameRead(e) {
		return true
	}
	switch x := e.(type) {
	case *ast.Ident:
		return taint.has(x)
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
// exception to it: it is a whitespace strip, `strings.ReplaceAll(s, " ", "")`
// (cascades_translator.go), which passes the leaf name through unchanged in
// every way that matters for identity. It used to be
// `strings.ReplaceAll(strings.ToUpper(s), " ", "")` — two entries of this list
// composed — and RFC-237 removed the fold; the launderer classification is
// unaffected, because a strict SUBSET of what it laundered still qualifies.
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
			if unqualified && (x.Name == "FieldValue" || x.Name == "fieldValue") {
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
