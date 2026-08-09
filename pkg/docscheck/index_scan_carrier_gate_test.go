package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// RFC-220 made coveringness a plan TYPE: RecordQueryCoveringIndexPlan WRAPS a
// RecordQueryIndexPlan as a plain struct FIELD, GetChildren returns nil, and so
// plans.Walk never descends into it. Every `case *plans.RecordQueryIndexPlan`
// and every `.(*plans.RecordQueryIndexPlan)` in the tree is therefore blind to
// a covering scan BY CONSTRUCTION — and because the access path emits
// Fetch(Covering(IndexScan)) for every index-backed access, the blindness is
// the ordinary case rather than an exotic one.
//
// Four such sites were found by hand and fixed one at a time. Fixing instances
// does not close the class, and the class fails in the silent direction: a
// missed index scan reads as "no index scan here", which downstream reads as
// "no comparison ranges", i.e. as an UNRESTRICTED scan. Two of the four were
// outright wrong answers, not lost precision — a RecordConstructorValue left
// unstamped and evaluating NAME-keyed, and a nil probed outer type licensing
// exactly the name-keyed binding the probe exists to forbid.
//
// So the gate is structural, like TestSourceCommentHygiene: scan every tracked
// non-test Go file, and fail on any type-switch or type-assertion that names
// the bare index plan without also handling the covering one. The remedy at a
// site is usually plans.IndexPlanOf / plans.IndexScanCarrier, which answer the
// question once for both shapes.
//
// Scope is by PROPERTY, reusing this package's established scope helpers:
// IN is `git ls-files '*.go'` minus generated files minus _test.go. Test files
// are OUT because a test that asserts on the bare plan specifically is normal
// and its blindness cannot ship a wrong answer to a user — it fails its own
// assertion.

const (
	indexPlanTypeName    = "RecordQueryIndexPlan"
	coveringPlanTypeName = "RecordQueryCoveringIndexPlan"
)

// carrierGateAllowlist exempts individual sites from the gate. Every entry
// needs a written reason saying why the site legitimately means "the bare index
// plan specifically, and a covering plan must NOT be treated the same way".
//
// An entry is keyed by FILE plus ENCLOSING FUNCTION, never by line. A
// line-numbered key was tried first and rotted within the same change that
// introduced it: fixing the blind sites shifted the exempt ones down, and a
// stale key does not fail loudly — it stops matching, and the gate reports a
// site the list already ruled on as if it were new. File+function is stable
// under edits above it and still names one site precisely. Match is an AND: all
// fragments must appear in the offense, so neither a bare filename nor a bare
// function name can exempt something it did not mean to.
var carrierGateAllowlist = []carrierExemption{
	{
		Match: []string{"abstract_data_access_rule.go", "in wrapScanPlanWithCoverage()"},
		Reason: "wrapScanPlanWithCoverage is where the covering plan is CONSTRUCTED. " +
			"Its input is the match candidate's pre-covering shape, so a covering plan " +
			"reaching it would mean the access path had already wrapped and was about to " +
			"wrap again. Treating one as an index plan here is not seeing more, it is " +
			"double-wrapping.",
	},
	{
		Match: []string{"planning_cost_model.go", "in isFetchExpression()"},
		Reason: "isFetchExpression asks whether the node RESOLVES ENTRIES TO BASE RECORDS. " +
			"A bare index scan does (Java's executeIndexScan semantics); a COVERING scan is " +
			"defined by not doing it. Accepting the covering plan here would count a " +
			"fetchless access as a fetch, which is the contradiction the covering type " +
			"exists to remove.",
	},
	{
		Match: []string{"planning_cost_model.go", "in concretePlanMatches()"},
		Reason: "concretePlanMatches/matchFetch is the concrete-tree twin of " +
			"isFetchExpression and answers the same question about fetching base records, " +
			"so it takes the same answer for the same reason. The two are required to " +
			"agree; exempting one and not the other would split them.",
	},
	{
		Match: []string{"index_scan.go", "in RecordQueryIndexPlan.EqualsPlanWithoutChildren()"},
		Reason: "EqualsPlanWithoutChildren is plan IDENTITY. A covering scan and the bare " +
			"scan it wraps emit different rows (a partial record rebuilt from the entry " +
			"versus the base record), so they are not interchangeable memo members and " +
			"must not compare equal. The covering plan has its own Equals doing the " +
			"mirror-image check.",
	},
}

type carrierExemption struct {
	// Match is the set of substrings that must ALL appear in the offense for
	// this exemption to apply. Use file plus enclosing function; never a line.
	Match []string
	// Reason is why the bare plan is the correct and complete match here.
	Reason string
}

// carrierSite is one place the source names *RecordQueryIndexPlan as a runtime
// type test.
type carrierSite struct {
	Rel string
	// Line is the 1-indexed line of the type name.
	Line int
	// Kind is "type switch" or "type assertion".
	Kind string
	// Func is the enclosing top-level declaration's name — the STABLE half of
	// the allowlist key, since line numbers move under any edit above.
	Func string
	// Covered reports whether the covering plan is handled alongside — for a
	// type switch, by a case in the SAME switch; for a bare assertion, by the
	// enclosing function referring to the covering type at all.
	Covered bool
}

func (s carrierSite) offense() string {
	return s.Rel + ":" + strconv.Itoa(s.Line) + ": " + s.Kind + " in " + s.Func + "() on *" +
		indexPlanTypeName + " with no " + coveringPlanTypeName + " arm"
}

// isPlanTypeExpr reports whether e is `*RecordQueryIndexPlan`-shaped for the
// given type name, accepting both the in-package spelling (`*Name`) and the
// qualified one (`*plans.Name`). The package QUALIFIER is deliberately not
// checked: `plans` is the only package in the tree defining these names, and
// pinning the qualifier would make an import alias silently disable the gate —
// failing open, the direction that ships bugs.
func isPlanTypeExpr(e ast.Expr, name string) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch t := star.X.(type) {
	case *ast.Ident:
		return t.Name == name
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == name
	}
	return false
}

// mentionsType reports whether any expression under n names the given plan type
// as a pointer type. Used to decide whether the function around a bare type
// ASSERTION also deals with the covering shape; an assertion, unlike a switch,
// carries no sibling arms to inspect.
func mentionsType(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if found || x == nil {
			return false
		}
		if e, ok := x.(ast.Expr); ok && isPlanTypeExpr(e, name) {
			found = true
			return false
		}
		return true
	})
	return found
}

// scanCarrierSites reports every index-plan runtime type test in one parsed
// file, each already classified as covered or blind.
// The scope of both passes is the WHOLE top-level declaration, receiver and
// signature included, not just the body: a method ON the covering plan, or a
// function that takes or returns one, is by construction dealing with the
// covering shape. Scoping to the body alone flagged
// RecordQueryCoveringIndexPlan's own methods, which is the gate reporting on
// itself rather than on a blindness.
func scanCarrierSites(fset *token.FileSet, f *ast.File, rel string) []carrierSite {
	var sites []carrierSite
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Body == nil {
			continue // an external/assembly declaration has no runtime type test
		}
		sites = append(sites, scanCarrierSitesInDecl(fset, decl, rel)...)
	}
	return sites
}

// declName is the STABLE half of a site's identity — what an allowlist entry is
// keyed on, since a line number moves under any edit above it. A method reads
// as "Recv.Name"; a package-level declaration that is not a function has no
// name of its own and reads as "<decl>", which is enough because such a
// declaration holding a type assertion is vanishingly rare and would be
// exempted by file anyway.
func declName(decl ast.Decl) string {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok || fd.Name == nil {
		return "<decl>"
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if star, isStar := recv.(*ast.StarExpr); isStar {
		recv = star.X
	}
	if id, isID := recv.(*ast.Ident); isID {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func scanCarrierSitesInDecl(fset *token.FileSet, decl ast.Decl, rel string) []carrierSite {
	var sites []carrierSite
	name := declName(decl)

	// Case-clause type expressions belong to pass 1 and must not be re-counted
	// by pass 2; they are plain Exprs, never TypeAssertExpr, so the two passes
	// cannot overlap. Recorded anyway as a positional guard against that
	// assumption changing.
	switchCasePos := map[token.Pos]bool{}

	// Pass 1: type switches, judged one switch at a time. A switch is the
	// natural unit here because its sibling arms are right there to inspect,
	// so two switches in one function are judged separately.
	ast.Inspect(decl, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSwitchStmt)
		if !ok || ts.Body == nil {
			return true
		}
		var hits []ast.Expr
		covering := false
		for _, stmt := range ts.Body.List {
			cc, isCase := stmt.(*ast.CaseClause)
			if !isCase {
				continue
			}
			for _, e := range cc.List {
				switchCasePos[e.Pos()] = true
				if isPlanTypeExpr(e, indexPlanTypeName) {
					hits = append(hits, e)
				}
				if isPlanTypeExpr(e, coveringPlanTypeName) {
					covering = true
				}
			}
		}
		for _, e := range hits {
			sites = append(sites, carrierSite{
				Rel:     rel,
				Line:    fset.Position(e.Pos()).Line,
				Kind:    "type switch",
				Func:    name,
				Covered: covering,
			})
		}
		return true
	})

	// Pass 2: bare type assertions, scoped to the whole declaration. A
	// declaration that names the covering type ANYWHERE counts as handling it —
	// coarser than the switch rule on purpose, because an assertion's sibling
	// handling is genuinely elsewhere in the body (a second `if ok :=` arm, a
	// helper call built from it), and a stricter rule would flag correct code.
	covering := mentionsType(decl, coveringPlanTypeName)
	ast.Inspect(decl, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok || ta.Type == nil {
			return true
		}
		if !isPlanTypeExpr(ta.Type, indexPlanTypeName) || switchCasePos[ta.Type.Pos()] {
			return true
		}
		sites = append(sites, carrierSite{
			Rel:     rel,
			Line:    fset.Position(ta.Type.Pos()).Line,
			Kind:    "type assertion",
			Func:    name,
			Covered: covering,
		})
		return true
	})
	return sites
}

func carrierAllowlisted(offense string) bool {
	return carrierAllowlisted2(offense, carrierGateAllowlist)
}

// carrierSiteFloor is the anti-vacuity floor on the number of index-plan
// runtime type tests the scan must FIND. A filter that matches nothing reports
// PASS, and this gate's entire value is that it examines a large existing
// population — so a scan that suddenly sees a handful of sites has almost
// certainly lost the source tree (runfiles staging, a scope-helper change), not
// had the population deleted.
//
// Set well below the measured population so ordinary deletions do not trip it.
// If real work genuinely drives the count under the floor, LOWER the floor in
// the same change and say what removed the sites — do not delete the guard.
const carrierSiteFloor = 35

// carrierFileFloor is the same guard on the other axis: the scan must have seen
// a real source tree. Set far below the measured file count so it trips only on
// a staging/scope failure, never on ordinary churn.
const carrierFileFloor = 500

// carrierGateVacuity is the gate's anti-vacuity decision, taken as EXPLICIT
// STATE rather than read off the surrounding loop, so every arm can be driven
// by a test instead of only by whatever the tree happens to contain. Both
// directions are guarded: the file scan must have seen a real source tree, and
// the SITE scan must have found the population the gate exists to police.
// Either collapsing to ~zero is the never-ran state, which a plain pass/fail
// verdict renders as success — failing OPEN, the direction that ships bugs.
func carrierGateVacuity(scannedFiles, totalSites int) error {
	if scannedFiles >= carrierFileFloor && totalSites >= carrierSiteFloor {
		return nil
	}
	return fmt.Errorf("carrier gate examined %d non-test Go files (floor %d) and found %d "+
		"index-plan type tests (floor %d) — that is not the real population; the gate examined "+
		"nothing and would otherwise have reported PASS. Fix sourceTreeRoot/runfiles staging, or "+
		"lower the floor in the same change that legitimately removed the sites",
		scannedFiles, carrierFileFloor, totalSites, carrierSiteFloor)
}

// TestIndexScanCarrierGate fails on any non-test type-switch or type-assertion
// on *RecordQueryIndexPlan that does not also handle the covering plan.
func TestIndexScanCarrierGate(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var scannedFiles, totalSites, coveredSites, exemptSites int
	var offenses []string

	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(filepath.Join(root, rel))
		if readErr != nil {
			t.Errorf("read %s: %v", rel, readErr)
			continue
		}
		if isGeneratedFile(src, nil) {
			continue
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if parseErr != nil {
			t.Errorf("parse %s: %v", rel, parseErr)
			continue
		}
		if isGeneratedFile(src, f) {
			continue
		}
		scannedFiles++
		for _, s := range scanCarrierSites(fset, f, rel) {
			totalSites++
			if s.Covered {
				coveredSites++
				continue
			}
			o := s.offense()
			if carrierAllowlisted(o) {
				exemptSites++
				continue
			}
			offenses = append(offenses, o)
		}
	}

	if err := carrierGateVacuity(scannedFiles, totalSites); err != nil {
		t.Fatalf("%v (scan root %s)", err, root)
	}

	// The census is logged as a full DECOMPOSITION, not as a blind count. A
	// lone "0 blind" is the number least worth printing: it is what a scan that
	// found nothing also prints, and it cannot be reconciled against anything.
	// The four figures sum to the population, so a write-up quoting them can be
	// checked against this line instead of being taken on trust — which is how
	// the site total came to be quoted off by one in the first place.
	t.Logf("carrier gate census: %d non-test Go files scanned; %d index-plan type tests examined "+
		"= %d already covered + %d allowlisted + %d blind",
		scannedFiles, totalSites, coveredSites, exemptSites, len(offenses))
	if coveredSites+exemptSites+len(offenses) != totalSites {
		t.Errorf("census does not decompose: %d covered + %d allowlisted + %d blind != %d examined",
			coveredSites, exemptSites, len(offenses), totalSites)
	}

	for _, o := range offenses {
		t.Errorf("index-scan blindness: %s", o)
	}
	if len(offenses) > 0 {
		t.Errorf("%d site(s) test for *%s without handling *%s. A covering scan holds its index "+
			"scan as a FIELD and GetChildren returns nil, so these sites never see one — and the "+
			"access path emits Fetch(Covering(IndexScan)) for every index-backed access. Use "+
			"plans.IndexPlanOf / plans.IndexScanCarrier, which answer for both shapes. A site that "+
			"genuinely means the bare plan goes on carrierGateAllowlist WITH a reason.",
			len(offenses), indexPlanTypeName, coveringPlanTypeName)
	}
}

// TestCarrierGateAllowlistEntriesCarryReasons pins the half of the allowlist
// contract that is prose in the comment above it: an exemption without a stated
// reason is an exemption nobody can audit, and the list's only real risk is
// silent growth.
func TestCarrierGateAllowlistEntriesCarryReasons(t *testing.T) {
	t.Parallel()
	for i, ex := range carrierGateAllowlist {
		if len(ex.Match) == 0 {
			t.Errorf("carrierGateAllowlist[%d] has no Match fragments — it can never exempt anything", i)
		}
		if len(strings.TrimSpace(ex.Reason)) < 20 {
			t.Errorf("carrierGateAllowlist[%d] (%v) has no substantive Reason; state why the bare "+
				"index plan is the correct and COMPLETE match at this site", i, ex.Match)
		}
		for _, frag := range ex.Match {
			if strings.TrimSpace(frag) == "" {
				t.Errorf("carrierGateAllowlist[%d] (%v) has an empty Match fragment, which can "+
					"never appear in an offense — the entry exempts nothing", i, ex.Match)
			}
		}
	}
}

// TestCarrierGateAllowlistEntriesAllMatchSomething is the anti-vacuity guard on
// the ALLOWLIST itself, and it is the mirror of the gate's own site floor. A
// stale entry — the file renamed, the function gone — does not fail loudly; it
// simply stops matching, and the next time that site is reported nobody knows
// the list already ruled on it. So every entry must exempt exactly one live
// blind site.
func TestCarrierGateAllowlistEntriesAllMatchSomething(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var blind []string
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || isGeneratedFile(src, nil) {
			continue
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if parseErr != nil || isGeneratedFile(src, f) {
			continue
		}
		for _, s := range scanCarrierSites(fset, f, rel) {
			if !s.Covered {
				blind = append(blind, s.offense())
			}
		}
	}
	if len(blind) == 0 {
		t.Fatalf("no blind sites found at all under %s — the allowlist cannot be checked against "+
			"an empty population, and the gate itself would be vacuous", root)
	}

	for i, ex := range carrierGateAllowlist {
		hits := 0
		for _, o := range blind {
			if carrierAllowlisted2(o, []carrierExemption{ex}) {
				hits++
			}
		}
		if hits == 0 {
			t.Errorf("carrierGateAllowlist[%d] (%v) matches NO blind site — the file or function "+
				"it names has moved or been fixed. Remove the entry (the exemption is no longer "+
				"needed) or re-key it; a stale entry silently stops exempting", i, ex.Match)
		}
	}
}

// TestScanCarrierSites drives every arm of the detector on synthetic sources,
// because the tree scan above only ever exercises the arms the tree happens to
// contain today. A gate whose blind-detection arm is never driven by a test is
// a gate whose first real firing is read as a finding rather than as an
// untested branch.
func TestScanCarrierSites(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		src         string
		wantSites   int
		wantBlind   int
		wantKind    string
		wantAtLines []int
	}{
		{
			name: "type switch with no covering arm is BLIND",
			src: `package p
func f(x any) int {
	switch x.(type) {
	case *plans.RecordQueryIndexPlan:
		return 1
	}
	return 0
}`,
			wantSites: 1, wantBlind: 1, wantKind: "type switch", wantAtLines: []int{4},
		},
		{
			name: "type switch with a covering arm is COVERED",
			src: `package p
func f(x any) int {
	switch x.(type) {
	case *plans.RecordQueryIndexPlan:
		return 1
	case *plans.RecordQueryCoveringIndexPlan:
		return 2
	}
	return 0
}`,
			wantSites: 1, wantBlind: 0, wantKind: "type switch",
		},
		{
			name: "both types in ONE case clause is COVERED",
			src: `package p
func f(x any) int {
	switch x.(type) {
	case *plans.RecordQueryScanPlan, *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan:
		return 1
	}
	return 0
}`,
			wantSites: 1, wantBlind: 0, wantKind: "type switch",
		},
		{
			name: "bare assertion in a function that never names covering is BLIND",
			src: `package p
func f(x any) bool {
	_, ok := x.(*plans.RecordQueryIndexPlan)
	return ok
}`,
			wantSites: 1, wantBlind: 1, wantKind: "type assertion", wantAtLines: []int{3},
		},
		{
			name: "assertion in a function that also handles covering is COVERED",
			src: `package p
func f(x any) *plans.RecordQueryIndexPlan {
	if p, ok := x.(*plans.RecordQueryIndexPlan); ok {
		return p
	}
	if c, ok := x.(*plans.RecordQueryCoveringIndexPlan); ok {
		return c.GetIndexPlan()
	}
	return nil
}`,
			wantSites: 1, wantBlind: 0, wantKind: "type assertion",
		},
		{
			name: "in-package spelling without the plans qualifier is detected",
			src: `package plans
func f(x any) bool {
	_, ok := x.(*RecordQueryIndexPlan)
	return ok
}`,
			wantSites: 1, wantBlind: 1, wantKind: "type assertion", wantAtLines: []int{3},
		},
		{
			name: "an aliased import qualifier does not disable the gate",
			src: `package p
func f(x any) bool {
	_, ok := x.(*pl.RecordQueryIndexPlan)
	return ok
}`,
			wantSites: 1, wantBlind: 1, wantKind: "type assertion", wantAtLines: []int{3},
		},
		{
			name: "a blind switch and a covered switch in ONE function are judged separately",
			src: `package p
func f(x, y any) int {
	switch x.(type) {
	case *plans.RecordQueryIndexPlan:
		return 1
	case *plans.RecordQueryCoveringIndexPlan:
		return 2
	}
	switch y.(type) {
	case *plans.RecordQueryIndexPlan:
		return 3
	}
	return 0
}`,
			wantSites: 2, wantBlind: 1, wantKind: "type switch", wantAtLines: []int{10},
		},
		{
			name: "the covering type alone is not a site",
			src: `package p
func f(x any) bool {
	_, ok := x.(*plans.RecordQueryCoveringIndexPlan)
	return ok
}`,
			wantSites: 0, wantBlind: 0,
		},
		{
			name: "an unrelated plan type is not a site",
			src: `package p
func f(x any) bool {
	_, ok := x.(*plans.RecordQueryAggregateIndexPlan)
	return ok
}`,
			wantSites: 0, wantBlind: 0,
		},
		{
			name: "a value type test, not a pointer, is not a site",
			src: `package p
func f(x any) bool {
	_, ok := x.(plans.RecordQueryIndexPlan)
	return ok
}`,
			wantSites: 0, wantBlind: 0,
		},
		{
			name: "an assertion inside a func literal is scoped to the enclosing declaration",
			src: `package p
var g = func(x any) bool {
	_, ok := x.(*plans.RecordQueryIndexPlan)
	return ok
}`,
			wantSites: 1, wantBlind: 1, wantKind: "type assertion", wantAtLines: []int{3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "synthetic.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sites := scanCarrierSites(fset, f, "synthetic.go")
			if len(sites) != tc.wantSites {
				t.Fatalf("found %d sites, want %d: %+v", len(sites), tc.wantSites, sites)
			}
			var blind []carrierSite
			for _, s := range sites {
				if !s.Covered {
					blind = append(blind, s)
				}
				if tc.wantKind != "" && s.Kind != tc.wantKind {
					t.Errorf("site kind = %q, want %q", s.Kind, tc.wantKind)
				}
			}
			if len(blind) != tc.wantBlind {
				t.Fatalf("found %d BLIND sites, want %d: %+v", len(blind), tc.wantBlind, blind)
			}
			for i, want := range tc.wantAtLines {
				if i >= len(blind) {
					t.Fatalf("expected a blind site at line %d, got only %d blind sites", want, len(blind))
				}
				if blind[i].Line != want {
					t.Errorf("blind site %d at line %d, want %d", i, blind[i].Line, want)
				}
			}
		})
	}
}

// TestCarrierGateOffenseNamesTheSite pins that a failure identifies WHERE, in
// the file:line form the allowlist matches on. A gate that reports only a count
// cannot be acted on and cannot be exempted.
func TestCarrierGateOffenseNamesTheSite(t *testing.T) {
	t.Parallel()
	o := carrierSite{Rel: "pkg/x/y.go", Line: 42, Kind: "type switch", Func: "doThing"}.offense()
	if !strings.HasPrefix(o, "pkg/x/y.go:42: ") {
		t.Errorf("offense %q does not lead with file:line", o)
	}
	if !strings.Contains(o, "in doThing()") {
		t.Errorf("offense %q does not name the enclosing function — the allowlist key", o)
	}

	match := func(m ...string) bool {
		return carrierAllowlisted2(o, []carrierExemption{{Match: m, Reason: "r"}})
	}
	if !match("y.go", "in doThing()") {
		t.Errorf("a file+function entry does not match the offense %q it should exempt", o)
	}
	// The AND is the load-bearing half: either fragment alone would exempt
	// sites in other files or other functions of the same name.
	if match("y.go", "in otherThing()") {
		t.Errorf("an entry naming a DIFFERENT function matched offense %q", o)
	}
	if match("z.go", "in doThing()") {
		t.Errorf("an entry naming a DIFFERENT file matched offense %q", o)
	}
	if match("") {
		t.Errorf("an empty Match fragment matched offense %q — it would exempt everything", o)
	}
	if carrierAllowlisted2(o, []carrierExemption{{Reason: "r"}}) {
		t.Errorf("an entry with NO Match fragments matched offense %q", o)
	}
	// A line number is a rotting key and must not be how entries are written;
	// the offense still carries one so a human can find the site.
	if !strings.Contains(o, ":42:") {
		t.Errorf("offense %q dropped the line number a reader needs to find the site", o)
	}
}

// TestCarrierGateVacuityGuard drives every arm of the anti-vacuity decision.
// The tree scan can only ever exercise the arm the tree currently satisfies —
// the PASSING one — so without this the guard's whole job (turning a never-ran
// scan into a loud failure instead of a green) is untested code that first runs
// on the day something is already broken.
func TestCarrierGateVacuityGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		files, sites int
		wantErr      bool
		wantMentions string
		whatItGuards string
	}{
		{
			name: "a healthy scan passes", files: carrierFileFloor, sites: carrierSiteFloor,
			wantErr: false, whatItGuards: "the floors are inclusive — exactly at the floor is fine",
		},
		{
			name: "no files at all is the never-ran state", files: 0, sites: 0,
			wantErr: true, wantMentions: "0 non-test Go files",
			whatItGuards: "sourceTreeRoot resolving to an empty runfiles tree",
		},
		{
			name: "files but no sites is a scope failure", files: 5000, sites: 0,
			wantErr: true, wantMentions: "found 0",
			whatItGuards: "the detector silently matching nothing (a renamed plan type)",
		},
		{
			name: "one file below the file floor trips", files: carrierFileFloor - 1, sites: 5000,
			wantErr: true, wantMentions: "non-test Go files",
			whatItGuards: "a partial tree that still contains plenty of sites",
		},
		{
			name: "one site below the site floor trips", files: 5000, sites: carrierSiteFloor - 1,
			wantErr: true, wantMentions: "index-plan type tests",
			whatItGuards: "the population collapsing while the tree stays whole",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := carrierGateVacuity(tc.files, tc.sites)
			if tc.wantErr && err == nil {
				t.Fatalf("vacuity guard PASSED on files=%d sites=%d; it must catch %s",
					tc.files, tc.sites, tc.whatItGuards)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("vacuity guard failed a healthy scan (files=%d sites=%d): %v",
					tc.files, tc.sites, err)
			}
			if tc.wantMentions != "" && !strings.Contains(err.Error(), tc.wantMentions) {
				t.Errorf("vacuity failure %q does not say which population collapsed (want %q)",
					err, tc.wantMentions)
			}
		})
	}
}

// TestDeclNameNamesTheEnclosingDeclaration pins the allowlist key's stable half.
// A method must read as Type.Method, or two same-named methods on different
// plan types would share one exemption.
func TestDeclNameNamesTheEnclosingDeclaration(t *testing.T) {
	t.Parallel()
	src := `package p
func plain() {}
func (p *RecordQueryIndexPlan) Method() {}
func (p RecordQueryScanPlan) ValueMethod() {}
var v = 1
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "d.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"plain", "RecordQueryIndexPlan.Method", "RecordQueryScanPlan.ValueMethod", "<decl>"}
	if len(f.Decls) != len(want) {
		t.Fatalf("parsed %d decls, want %d", len(f.Decls), len(want))
	}
	for i, decl := range f.Decls {
		if got := declName(decl); got != want[i] {
			t.Errorf("declName(decl %d) = %q, want %q", i, got, want[i])
		}
	}
}

// carrierAllowlisted2 is carrierAllowlisted against an explicit list, so the
// matching rule can be asserted directly instead of only through the package
// global (which a test must never mutate — every test here runs in parallel).
//
// An empty Match never matches. A list of fragments is an AND, so an entry
// cannot exempt more than the one site its file+function names.
func carrierAllowlisted2(offense string, list []carrierExemption) bool {
	for _, ex := range list {
		if len(ex.Match) == 0 {
			continue
		}
		all := true
		for _, frag := range ex.Match {
			if frag == "" || !strings.Contains(offense, frag) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
