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
// and the reviewer question is always "why can Resolved not answer this?"
//
// The exemption is per SITE (file:line, with a count), never per file. A
// whole-file exemption is not an allowlist, it is a hole with a comment on it:
// it covers every decision the file grows later, for free and silently. The
// earlier version of this list held three FILES; measurement showed they
// exempted nothing at all, so the loophole was pure downside — it would have
// been discovered the first time someone needed one line of a 6000-line
// translator exempted and got the other 5999 with it.
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
	// boundary (2)
	"pkg/recordlayer/query/plan/cascades/match_candidate_index.go:792":          {1, "boundary: covered-column set holds index-definition NAMES, matched against a query value's leaf name (laundered through ToUpper) on every fetch-push; resolve to ordinals at candidate construction"},
	"pkg/recordlayer/query/plan/cascades/windowed_index_match_candidate.go:246": {1, "boundary: same covered-column name set, windowed candidate"},

	// escape (7)
	"pkg/relational/core/query/cascades_translator.go:7392":                       {1, "escape: aggregateOperandColumn hands the qualifier-stripped, upper-cased name back as a bare string; visible only once a launderer's argument is searched to any depth"},
	"pkg/recordlayer/query/plan/cascades/fk_chain_cardinality.go:394":             {1, "escape: leafFieldName returns the bare name; callers key maps by it"},
	"pkg/recordlayer/query/plan/cascades/fk_chain_cardinality.go:421":             {1, "escape: correlated variant, same caller pattern, after a Resolved.Single() guard"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:3691": {1, "escape: returns (alias, column) as bare uppercased strings"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:3735": {1, "escape: bareColumnName, QOV arm"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:3741": {1, "escape: bareColumnName, flat-string fallback arm"},
	"pkg/recordlayer/query/plan/plans/cost.go:643":                                {1, "escape: correlatedInnerField -- the shape the gate is named after; guarded by Resolved.Single() and a flat-QOV child, but the caller still keys want[]/bound[] by the returned name"},

	// contract (5)
	"pkg/recordlayer/query/plan/cascades/values/values.go:1274":       {1, "contract: ProjectionColumnName IS the projection output-column naming contract -- the key the executor writes a projected slot under and every re-reader reads it by; the naming authority the other contract sites delegate to, and invisible until the gate could see unqualified *FieldValue inside the values package"},
	"pkg/relational/core/embedded/cascades_generator.go:3301":         {1, "contract: result-set metadata LABEL selection -- decides whether a dotted label is the internal duplicate-disambiguation key or a user alias by leaf-matching it against the projected value's name; the JDBC label contract, where a name genuinely is the identity"},
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:118": {1, "contract: AggregateKeyColumnName is THE group-key naming contract with the executor; moves only when the contract becomes an ordinal slot"},
	"pkg/relational/core/embedded/logical_predicate.go:6093":          {1, "contract: aggregate group-key output name, same contract family"},
	"pkg/relational/core/query/cascades_translator.go:4748":           {1, "contract: sort-key hidden-field naming (RFC-141), same output-naming contract family"},

	// dotted (9)
	"pkg/recordlayer/query/plan/cascades/values/value_correlation.go:57":          {1, "dotted: keys a correlation set by the QUALIFIER sliced off the flat 'ALIAS.col' name; the slice hid it from every wrapper whitelist"},
	"pkg/relational/core/query/cascades_translator.go:5726":                       {1, "dotted: leg-layout match on the sliced qualifier"},
	"pkg/relational/core/query/cascades_translator.go:5765":                       {1, "dotted: leg-layout map keyed by the sliced qualifier, same channel as 5726"},
	"pkg/relational/core/query/exists_gathered_cluster_wrap.go:131":               {1, "dotted: leg-window map keyed by the sliced qualifier, gathered-EXISTS wrap"},
	"pkg/recordlayer/query/plan/cascades/left_outer_existential.go:112":           {1, "dotted: leg-relative vs qualified ref probed via '.' in the name"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:2337": {1, "dotted: declines re-qualifying an already-dotted ref; Child is a live QOV, so this is the qualified-name channel, not the legacy flat shape"},
	"pkg/recordlayer/query/plan/cascades/values/accessor_name_path.go:61":         {1, "dotted: accessor path derived by splitting the name on dots"},
	"pkg/relational/core/query/box_conjunct.go:149":                               {1, "dotted: frontier read attributed by '.' probe; the only dotted site actually gated on Child == nil"},
	"pkg/relational/core/query/ordinal_seed.go:761":                               {1, "dotted: leg-ref detection via '.' probe on the merged-QOV leg.col channel"},

	// name-keyed (11)
	"pkg/recordlayer/query/plan/cascades/referenced_fields.go:125":             {1, "name-keyed: referenced-field set keyed by leaf name"},
	"pkg/recordlayer/query/plan/cascades/rule_implement_distinct_final.go:197": {1, "name-keyed: distinct-key set keyed by name"},
	"pkg/recordlayer/query/plan/cascades/rule_projection_merge.go:113":         {1, "name-keyed: projection merge matches inner slot by name"},
	"pkg/recordlayer/query/plan/plans/in_memory_sort.go:142":                   {1, "name-keyed: sort key equality by name"},
	"pkg/recordlayer/query/plan/cascades/values/map_field_values.go:354":       {1, "name-keyed: field remap compares leaf names"},
	"pkg/recordlayer/query/plan/cascades/values/pullup.go:210":                 {1, "name-keyed: pull-up picks a struct member by name"},
	"pkg/recordlayer/query/plan/cascades/values/replace.go:498":                {1, "name-keyed: replacement target matched by name"},
	"pkg/recordlayer/query/plan/cascades/values/replace.go:520":                {1, "name-keyed: same, second arm"},
	"pkg/recordlayer/query/plan/cascades/values/simplifier_value.go:243":       {1, "name-keyed: composeFieldOverConstructor picks a constructor member by name, correct only because of its duplicate-name guard"},
	"pkg/relational/core/embedded/logical_predicate.go:4151":                   {1, "name-keyed: ordinal equality ANDed with a name check on two RESOLVED values. Probed: UNREACHABLE as a decision-changer (every caller ORs it with SemanticEqualsUnderAliasMap; suite green with the matcher hard-wired false) and LOAD-BEARING against Ordinal:-1 name-only accessors, where ordinal equality is vacuous -- deleting it binds the wrong column. Pinned both directions in aggregate_group_key_accessor_name_test.go; convert only after RFC-197 step 0 fail-closes negative ordinals"},
	"pkg/relational/core/embedded/logical_predicate.go:6188":                   {1, "name-keyed: group-key match compares two Values' leaf names during aggregate translation -- both operands are .Field, so this is a value-identity matcher, not resolution"},

	// translator (11)
	"pkg/relational/core/embedded/cascades_generator.go:3155": {1, "translator: parsed column ref matched against declared inner columns"},
	"pkg/relational/core/embedded/cascades_generator.go:3169": {1, "translator: same inner-column lookup as 3175, leg-qualified arm -- the map key is a CONCATENATION, which is why the sibling entry was recorded and this one was not"},
	"pkg/relational/core/embedded/cascades_generator.go:3175": {1, "translator: inner-column lookup by parsed name (laundered map key)"},
	"pkg/relational/core/embedded/logical_predicate.go:6617":  {1, "translator: join-side name set during translation (laundered map key)"},
	"pkg/relational/core/query/cascades_translator.go:2093":   {1, "translator: unnest element alias resolution, flat arm; the sibling arm consults the ordinal"},
	"pkg/relational/core/query/cascades_translator.go:2107":   {1, "translator: unnest element/ordinality selection by declared alias, qualified arm (laundered switch tag)"},
	"pkg/relational/core/query/cascades_translator.go:3870":   {1, "translator: element slot lookup during translation (laundered map key)"},
	"pkg/relational/core/query/cascades_translator.go:5048":   {1, "translator: struct field resolution against descriptor"},
	"pkg/relational/core/query/cascades_translator.go:5879":   {1, "translator: column list membership during resolution"},
	"pkg/relational/core/query/cascades_translator.go:6096":   {1, "translator: resolves and emits NewFieldValueWithResolvedOrdinal -- boundary-shaped; first candidate for the per-site allowlist under the mechanical test"},
	"pkg/relational/core/query/cascades_translator.go:6104":   {1, "translator: aggregate projection item match during resolution"},

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
// through the tree walk, they are also unfalsifiable there: all 46 recorded
// sites carry n == 1, so replacing `seen[key]++` with `seen[key] = 1` leaves
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

// readsFieldName reports whether e delivers the leaf name through wrapping that
// PROVES it is still the name — `.Field` itself, in parentheses, or under a
// launderer. This is the shape-matched tier of the sink test, and it needs no
// type information to be safe: `strings.ToUpper(x)` is only well-typed if x is
// a string, so a name read under it is still a name.
//
// Requiring `.Field` to be the IMMEDIATE child of a sink is how
// `coveredColumns[strings.ToUpper(v.Field)]` and `switch strings.ToUpper(fv.Field)`
// stayed invisible: the sink's child is a CallExpr, and one level of indirection
// was enough to hide the decision. Uppercasing a name does not turn it into a
// resolved column, so the wrapper is peeled and the sink is judged on what
// actually reaches it.
func readsFieldName(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return isFieldSelector(x)
	case *ast.ParenExpr:
		return readsFieldName(x.X)
	case *ast.CallExpr:
		if !nameLaunderers[callFuncName(x.Fun)] {
			return false
		}
		for _, arg := range x.Args {
			if readsFieldName(arg) {
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
func containsFieldNameRead(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if x, ok := n.(ast.Expr); ok && isFieldSelector(x) {
			found = true
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
//   - `strings.ToUpper(fv.Field)` — a launderer;
//   - `legPrefix + fv.Field` — concatenation, which escapes the name with a
//     decoration on it, still keyed and compared as a name downstream;
//   - `fv.Field[:dot]` — a slice, i.e. the qualifier or the leaf on its own.
//
// BELOW a launderer the rule relaxes to deep containment, which is what makes
// `strings.ToUpper(stripColumnQualifier(fv.Field))` visible. A launderer's
// argument is already a string, so anything under it is a string-to-string
// derivation of the name — there is no constructor to confuse it with, and
// requiring the inner callee to be whitelisted too would just restart the
// enumeration game one level down.
func escapesFieldName(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return isFieldSelector(x)
	case *ast.ParenExpr:
		return escapesFieldName(x.X)
	case *ast.SliceExpr:
		return escapesFieldName(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return escapesFieldName(x.X) || escapesFieldName(x.Y)
		}
	case *ast.CallExpr:
		if !nameLaunderers[callFuncName(x.Fun)] {
			return false
		}
		for _, arg := range x.Args {
			if containsFieldNameRead(arg) {
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
var nameLaunderers = map[string]bool{
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
	decides := func(e ast.Expr) bool {
		return readsFieldName(e) || (handlesFieldValue && containsFieldNameRead(e))
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			handlesFieldValue = funcTouchesFieldValue(x, inValuesPkg)
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
					report(x.Pos(), "a "+op+" comparison")
				}
			}
		case *ast.SwitchStmt:
			// Switching on a display name is equality N times over. (An
			// EMPTY-tag switch needs no arm here: ast.Inspect still visits
			// each case's boolean expression as an ordinary BinaryExpr.)
			if x.Tag != nil && decides(x.Tag) {
				report(x.Pos(), "a switch tag")
			}
		case *ast.IndexExpr:
			// Keying a map by display name conflates same-named columns.
			if decides(x.Index) {
				report(x.Pos(), "a map key")
			}
		case *ast.KeyValueExpr:
			// map[string]T{fv.Field: …} builds the same conflation through
			// a composite literal, which never produces an IndexExpr.
			if decides(x.Key) {
				report(x.Pos(), "a composite-literal key")
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
				if escapesFieldName(r) {
					report(x.Pos(), "the name escaping as a bare string (return)")
					break
				}
			}
		case *ast.CallExpr:
			if name := callFuncName(x.Fun); stringCompareHelpers[name] {
				for _, arg := range x.Args {
					if decides(arg) {
						report(x.Pos(), "a "+name+" call")
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
