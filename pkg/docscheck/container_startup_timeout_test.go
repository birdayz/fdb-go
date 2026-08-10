package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every FDB testcontainer is started under a BOUNDED context.
//
// The rule this enforces is CLAUDE.md's: "Container setup MUST have timeouts ...
// Bare context.Background() blocks forever when Docker is slow." The failure it
// prevents is the one that reports nothing — a test or benchmark whose container
// start never returns produces no assertion failure and no timing, only a run
// that sits there until some outer watchdog kills it, at which point the message
// names the watchdog and not the cause. A deadline turns that into a loud,
// attributable error naming the call site.
//
// It is a GATE rather than a one-time sweep because the shape reappears: a bare
// context.Background() is the natural thing to write, it compiles, and on a quiet
// machine it behaves identically to a bounded one. The difference only shows up
// under load — which is exactly when nobody is in a position to diagnose it, and
// exactly the regime this repo's Docker-backed suites run in when several agents
// share one host.
//
// The gate reads the TRACKED source tree (not the runfiles staging), the same way
// TestSourceCommentHygiene does, and it guards its own population: a run that
// finds fewer call sites than the floor below has stopped scanning the tree and
// says so, rather than passing on an empty set.

// containerStartFunc is the constructor every FDB testcontainer goes through.
// Selector-matched (`<pkg>.Run`) against the import path below, so an aliased
// import — every call site in this repo aliases it as foundationdbtc — is still
// seen.
const (
	containerStartPkgPath = "fdb.dev/pkg/testcontainers/foundationdb"
	containerStartFunc    = "Run"
)

// containerStartFloor is the population guard.
//
// It is a FLOOR on the number of Run call sites the scan reaches, and it exists
// because every verdict this gate reaches is vacuous if the scan found nothing.
// A resolution bug, a changed import path, or a runfiles tree standing in for the
// source tree would each produce zero call sites and a green gate — the
// never-ran-reads-as-passed failure, in the direction that ships bugs.
//
// Set below the measured population rather than at it: the count churns with
// ordinary work (a new integration suite adds one), and what this must detect is
// the scan going DARK, not drift. Measured at 18 call sites across pkg/ when this
// landed.
const containerStartFloor = 10

// ctxVerdict classifies the context a container start is handed.
type ctxVerdict int

const (
	// ctxBounded: derived from context.WithTimeout or context.WithDeadline.
	ctxBounded ctxVerdict = iota
	// ctxUndetermined: the argument is not something this gate can trace (a
	// struct field, a function result, a helper with no caller in its package).
	// Treated as a FAILURE, never as a pass — a gate that cannot see the
	// deadline must not certify one. Widening the tracer is the fix; silence is
	// not.
	ctxUndetermined
	// ctxUnbounded: context.Background() or context.TODO(), directly or via a
	// local binding or a caller. This is the defect.
	//
	// Ordered LAST deliberately: the verdicts are ranked by severity so that
	// combining several callers keeps the worst, and "unbounded" outranks
	// "undetermined" because it is the more actionable message — it names a
	// concrete missing deadline rather than a hole in the tracer.
	ctxUnbounded
)

func (v ctxVerdict) String() string {
	switch v {
	case ctxBounded:
		return "bounded"
	case ctxUnbounded:
		return "unbounded"
	default:
		return "undetermined"
	}
}

// pkgIndex is one Go package's functions and the calls made within it — enough
// to answer "who calls this helper, and what do they pass at that position?".
//
// The index is per-PACKAGE because that is the scope in which a test helper is
// reachable. A helper called from another package would resolve to no callers
// and therefore to `undetermined`, which fails loudly rather than quietly, and
// is the correct direction for a gate that cannot see the whole picture.
type pkgIndex struct {
	funcs map[string]*ast.FuncDecl
	// callers[name] lists (enclosing function, call) pairs for calls to name.
	callers map[string][]pkgCall
}

type pkgCall struct {
	in   *ast.FuncDecl
	call *ast.CallExpr
}

// ctxParamIndex reports the position of the named identifier in fn's flattened
// parameter list, or -1 when it is not a parameter. Flattened because Go allows
// `func f(a, b context.Context)` — one Field, two names, two argument slots.
func ctxParamIndex(fn *ast.FuncDecl, name string) int {
	if fn == nil || fn.Type == nil || fn.Type.Params == nil {
		return -1
	}
	pos := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			pos++
			continue
		}
		for _, id := range field.Names {
			if id.Name == name {
				return pos
			}
			pos++
		}
	}
	return -1
}

// containerCtxMaxDepth bounds the caller-chain walk. A helper wrapping a helper
// wrapping a helper is already unusual; deeper than this the gate says
// `undetermined` rather than recursing forever on a cycle.
const containerCtxMaxDepth = 4

// classifyContainerCtx decides the verdict for the expression passed as a
// container start's context argument.
//
// It traces local bindings within the enclosing function AND, when the context
// is that function's own parameter, up into the package's callers of it. The
// interprocedural step is not a refinement — without it every container start
// that lives in a `func startX(t *testing.T, ctx context.Context)` helper is
// invisible to the gate, and in this repo that is most of them. A gate blind to
// the dominant shape guards nothing.
//
// Split out from the walk so every arm can be driven from a unit test rather
// than only from whatever the corpus happens to contain — the arms that matter
// most here (unbounded-via-binding, undetermined) are the ones a clean tree
// never exercises.
func classifyContainerCtx(fn *ast.FuncDecl, arg ast.Expr) (ctxVerdict, string) {
	return classifyContainerCtxIn(nil, fn, arg, 0)
}

func classifyContainerCtxIn(idx *pkgIndex, fn *ast.FuncDecl, arg ast.Expr, depth int) (ctxVerdict, string) {
	// Inline: foundationdbtc.Run(context.Background(), ...)
	if v, detail, ok := classifyCtxCall(arg); ok {
		return v, detail
	}
	ident, ok := arg.(*ast.Ident)
	if !ok || fn == nil || fn.Body == nil {
		return ctxUndetermined, "argument is not a call this gate recognises and not a local identifier"
	}
	if pos := ctxParamIndex(fn, ident.Name); pos >= 0 {
		if idx == nil {
			return ctxUndetermined, fmt.Sprintf("%q is a parameter of %s and no package index is available to trace its callers", ident.Name, fn.Name.Name)
		}
		if depth >= containerCtxMaxDepth {
			return ctxUndetermined, fmt.Sprintf("caller chain for %s exceeded depth %d", fn.Name.Name, containerCtxMaxDepth)
		}
		calls := idx.callers[fn.Name.Name]
		if len(calls) == 0 {
			return ctxUndetermined, fmt.Sprintf("%s takes ctx as a parameter and has no caller in its package", fn.Name.Name)
		}
		worst, worstDetail := ctxBounded, ""
		for _, c := range calls {
			if pos >= len(c.call.Args) {
				return ctxUndetermined, fmt.Sprintf("a call to %s passes too few arguments to locate ctx", fn.Name.Name)
			}
			v, d := classifyContainerCtxIn(idx, c.in, c.call.Args[pos], depth+1)
			// The WORST caller decides: one unbounded caller is enough to hang.
			if v > worst {
				worst, worstDetail = v, fmt.Sprintf("via caller %s: %s", callerName(c.in), d)
			}
		}
		if worst == ctxBounded {
			return ctxBounded, fmt.Sprintf("every caller of %s passes a bounded context", fn.Name.Name)
		}
		return worst, worstDetail
	}
	// Trace the identifier to its assignment inside this function. The LAST
	// assignment wins: a ctx reassigned to a bounded one after a bare
	// Background() is bounded at the call, and vice versa — and the vice versa
	// is the direction that must not be missed.
	verdict, detail := ctxUndetermined, fmt.Sprintf("no assignment to %q found in the enclosing function", ident.Name)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != ident.Name {
				continue
			}
			if len(assign.Rhs) == 0 {
				continue
			}
			if v, d, ok := classifyCtxCall(assign.Rhs[0]); ok {
				verdict, detail = v, d
			} else {
				verdict, detail = ctxUndetermined,
					fmt.Sprintf("%q is assigned from an expression this gate cannot trace", ident.Name)
			}
		}
		return true
	})
	return verdict, detail
}

// classifyCtxCall classifies a direct context.X(...) call. The bool reports
// whether the expression was a context-package call at all.
func classifyCtxCall(e ast.Expr) (ctxVerdict, string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return 0, "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "context" {
		return 0, "", false
	}
	switch sel.Sel.Name {
	case "WithTimeout", "WithDeadline":
		return ctxBounded, "context." + sel.Sel.Name, true
	case "Background", "TODO":
		return ctxUnbounded, "context." + sel.Sel.Name, true
	case "WithCancel", "WithValue":
		// Cancellable is not bounded: nothing fires on its own, so a slow
		// Docker still hangs. Named separately so the failure says why.
		return ctxUnbounded, "context." + sel.Sel.Name + " (cancellable, but nothing bounds it)", true
	}
	return 0, "", false
}

// containerStartSite is one located call.
type containerStartSite struct {
	file    string
	line    int
	verdict ctxVerdict
	detail  string
}

func TestFDBContainerStartsAreBounded(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)
	if len(files) == 0 {
		t.Fatalf("no tracked Go files found under %s — the scan is dark, so every verdict below is vacuous", root)
	}

	// Group by directory: a Go package is the scope in which a test helper is
	// reachable, so it is the scope the caller trace has to run in.
	byDir := map[string][]string{}
	for _, rel := range files {
		slash := filepath.ToSlash(rel)
		if !strings.HasPrefix(slash, "pkg/") {
			continue
		}
		byDir[filepath.Dir(slash)] = append(byDir[filepath.Dir(slash)], slash)
	}

	var sites []containerStartSite
	fset := token.NewFileSet()
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		parsed := map[string]*ast.File{}
		importsIt := false
		for _, rel := range byDir[dir] {
			src, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				// A tracked file the scan cannot read is a broken scan, not a pass.
				t.Fatalf("read %s: %v", rel, err)
			}
			f, err := parser.ParseFile(fset, rel, src, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			parsed[rel] = f
			if containerStartAlias(f) != "" {
				importsIt = true
			}
		}
		if !importsIt {
			continue
		}
		idx := buildPkgIndex(parsed)
		for _, rel := range byDir[dir] {
			f := parsed[rel]
			alias := containerStartAlias(f)
			if alias == "" {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != containerStartFunc {
						return true
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != alias || len(call.Args) == 0 {
						return true
					}
					verdict, detail := classifyContainerCtxIn(idx, fn, call.Args[0], 0)
					sites = append(sites, containerStartSite{
						file:    rel,
						line:    fset.Position(call.Lparen).Line,
						verdict: verdict,
						detail:  detail,
					})
					return true
				})
			}
		}
	}

	if msg := containerStartVerdict(sites, containerStartFloor); msg != "" {
		t.Fatal(msg)
	}

	t.Logf("FDB container starts scanned: %d, all bounded (floor %d)", len(sites), containerStartFloor)
}

// containerStartVerdict renders the gate's decision, or "" when the scan is both
// large enough to be meaningful and clean.
//
// Pure, and separate from the walk, so BOTH arms can be driven from a test. The
// population guard in particular is an arm a healthy tree never exercises: it
// fires only when the scan has gone dark, which is exactly the state where a
// silent pass would be read as a clean tree. An untested collapse-detector is
// indistinguishable from an absent one.
func containerStartVerdict(sites []containerStartSite, floor int) string {
	if len(sites) < floor {
		return fmt.Sprintf("found %d container-start call sites under pkg/, want at least %d.\n"+
			"  This is the gate's POPULATION GUARD, and it is failing. Every verdict this\n"+
			"  test reaches is a statement about the sites it found, so a scan that finds\n"+
			"  none passes while guarding nothing. Either the import path moved (this gate\n"+
			"  matches %q), or the source tree resolution is landing somewhere that is not\n"+
			"  the checkout. Fix the scan; do not lower the floor to match a dark run.",
			len(sites), floor, containerStartPkgPath)
	}
	var bad []string
	for _, s := range sites {
		if s.verdict == ctxBounded {
			continue
		}
		bad = append(bad, fmt.Sprintf("  %s:%d — %s (%s)", s.file, s.line, s.verdict, s.detail))
	}
	sort.Strings(bad)
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d FDB container starts are not bounded by a deadline:\n%s\n"+
		"  Wrap the start in `ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)`\n"+
		"  with `defer cancel()`. An unbounded start does not fail when Docker is slow — it\n"+
		"  HANGS, producing no assertion and no timing, and the eventual watchdog names\n"+
		"  itself instead of the cause. On a host running several container suites at once\n"+
		"  that is the difference between a legible error and a run nobody can explain.\n"+
		"  An `undetermined` verdict means this gate could not see the deadline, which is\n"+
		"  a failure by construction: widen classifyContainerCtx rather than trusting it.",
		len(bad), len(sites), strings.Join(bad, "\n"))
}

// TestContainerStartVerdictArms drives the gate's two decision arms — the
// population guard and the per-site verdict — from explicit state.
//
// The corpus reaches exactly one of them (clean, above the floor). The other two
// states are the ones that matter: a dark scan, which must NOT read as clean,
// and a dirty scan, which must name the sites.
func TestContainerStartVerdictArms(t *testing.T) {
	t.Parallel()

	bounded := containerStartSite{file: "pkg/a/a_test.go", line: 10, verdict: ctxBounded}
	unbounded := containerStartSite{file: "pkg/b/b_test.go", line: 20, verdict: ctxUnbounded, detail: "context.Background"}
	undet := containerStartSite{file: "pkg/c/c_test.go", line: 30, verdict: ctxUndetermined, detail: "no caller"}

	for _, tc := range []struct {
		name  string
		sites []containerStartSite
		floor int
		want  string // substring the message must contain; "" means it must pass
	}{
		{name: "clean and above the floor passes", sites: []containerStartSite{bounded, bounded, bounded}, floor: 2, want: ""},
		{name: "empty scan trips the population guard", sites: nil, floor: 2, want: "POPULATION GUARD"},
		{name: "below the floor trips the population guard even when clean", sites: []containerStartSite{bounded}, floor: 2, want: "POPULATION GUARD"},
		{name: "an unbounded site is named", sites: []containerStartSite{bounded, unbounded}, floor: 2, want: "pkg/b/b_test.go:20"},
		{name: "an undetermined site fails, never passes", sites: []containerStartSite{bounded, undet}, floor: 2, want: "pkg/c/c_test.go:30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := containerStartVerdict(tc.sites, tc.floor)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("want a pass, got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("verdict does not mention %q; got:\n%s\n"+
					"  Each arm here is a state the corpus cannot produce on a healthy tree. The\n"+
					"  population guard especially: it fires only when the scan has gone dark, and\n"+
					"  a dark scan that passes reads exactly like a clean one.", tc.want, got)
			}
		})
	}
}

// buildPkgIndex indexes one package's function declarations and the calls made
// inside them, so a ctx that arrives as a parameter can be traced to what the
// callers actually pass.
func buildPkgIndex(parsed map[string]*ast.File) *pkgIndex {
	idx := &pkgIndex{funcs: map[string]*ast.FuncDecl{}, callers: map[string][]pkgCall{}}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			idx.funcs[fn.Name.Name] = fn
		}
	}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if _, known := idx.funcs[id.Name]; known {
					idx.callers[id.Name] = append(idx.callers[id.Name], pkgCall{in: fn, call: call})
				}
				return true
			})
		}
	}
	return idx
}

// callerName names a function for a failure message.
func callerName(fn *ast.FuncDecl) string {
	if fn == nil || fn.Name == nil {
		return "<unknown>"
	}
	return fn.Name.Name
}

// containerStartAlias returns the local name the file imports the testcontainer
// package under, or "" when it does not import it.
func containerStartAlias(f *ast.File) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != containerStartPkgPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "foundationdb"
	}
	return ""
}

// TestClassifyContainerCtxArms drives EVERY arm of the classifier from explicit
// source, rather than from whatever the corpus happens to contain.
//
// The corpus reading is not a substitute here and the asymmetry is stark: on a
// healthy tree the corpus exercises exactly ONE arm (bounded), so the two arms
// that decide whether the gate can ever fail — unbounded-via-local-binding and
// undetermined — would ship never having run. Their first real firing would then
// be read as a finding about the tree rather than as an untested branch.
func TestClassifyContainerCtxArms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want ctxVerdict
	}{
		{
			name: "inline background is unbounded",
			body: `start(context.Background(), "")`,
			want: ctxUnbounded,
		},
		{
			name: "inline TODO is unbounded",
			body: `start(context.TODO(), "")`,
			want: ctxUnbounded,
		},
		{
			name: "bound local binding",
			body: `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); _ = cancel; start(ctx, "")`,
			want: ctxBounded,
		},
		{
			name: "deadline local binding",
			body: `ctx, cancel := context.WithDeadline(context.Background(), when); _ = cancel; start(ctx, "")`,
			want: ctxBounded,
		},
		{
			name: "unbounded local binding",
			body: `ctx := context.Background(); start(ctx, "")`,
			want: ctxUnbounded,
		},
		{
			name: "WithCancel is not a deadline",
			body: `ctx, cancel := context.WithCancel(context.Background()); _ = cancel; start(ctx, "")`,
			want: ctxUnbounded,
		},
		{
			name: "reassignment to unbounded after bounded still loses",
			body: `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); _ = cancel; ctx = context.Background(); start(ctx, "")`,
			want: ctxUnbounded,
		},
		{
			name: "untraceable local is undetermined, never a pass",
			body: `ctx := makeCtx(); start(ctx, "")`,
			want: ctxUndetermined,
		},
		{
			name: "non-identifier argument is undetermined",
			body: `start(outer.ctx, "")`,
			want: ctxUndetermined,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn, arg := parseClassifierFixture(t, tc.body)
			got, detail := classifyContainerCtx(fn, arg)
			if got != tc.want {
				t.Fatalf("classifyContainerCtx = %s (%s), want %s.\n"+
					"  This arm decides whether an unbounded container start is CAUGHT. An arm\n"+
					"  that answers 'bounded' by accident makes the whole gate green on a tree\n"+
					"  that hangs under load; one that answers 'unbounded' by accident makes it\n"+
					"  unusable. Both directions are pinned here on purpose.", got, detail, tc.want)
			}
		})
	}
}

// TestClassifyContainerCtxAcrossCallers drives the INTERPROCEDURAL arms — the
// ones that decide the verdict for every container start living in a
// `func startX(t *testing.T, ctx context.Context)` helper, which is most of them
// in this repo.
//
// Without these arms the helper shape resolves to `undetermined` and the gate is
// a gate over the minority of call sites that inline their context. That was the
// state this test was written in, and the corpus could not tell: every helper
// site failed identically whether the callers were bounded or not.
func TestClassifyContainerCtxAcrossCallers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  string
		want ctxVerdict
	}{
		{
			name: "helper whose only caller is bounded",
			src: `package p
func helper(t *T, ctx context.Context) { start(ctx, "") }
func TestA(t *T) { ctx, cancel := context.WithTimeout(context.Background(), time.Minute); _ = cancel; helper(t, ctx) }`,
			want: ctxBounded,
		},
		{
			name: "helper whose only caller is unbounded",
			src: `package p
func helper(t *T, ctx context.Context) { start(ctx, "") }
func TestA(t *T) { helper(t, context.Background()) }`,
			want: ctxUnbounded,
		},
		{
			name: "one unbounded caller among bounded ones loses",
			src: `package p
func helper(t *T, ctx context.Context) { start(ctx, "") }
func TestA(t *T) { ctx, cancel := context.WithTimeout(context.Background(), time.Minute); _ = cancel; helper(t, ctx) }
func TestB(t *T) { helper(t, context.TODO()) }`,
			want: ctxUnbounded,
		},
		{
			name: "helper with no caller in its package is undetermined",
			src: `package p
func helper(t *T, ctx context.Context) { start(ctx, "") }`,
			want: ctxUndetermined,
		},
		{
			name: "two hops of helper, bounded at the top",
			src: `package p
func inner(ctx context.Context) { start(ctx, "") }
func outer(ctx context.Context) { inner(ctx) }
func TestA(t *T) { ctx, cancel := context.WithTimeout(context.Background(), time.Minute); _ = cancel; outer(ctx) }`,
			want: ctxBounded,
		},
		{
			name: "two hops of helper, unbounded at the top",
			src: `package p
func inner(ctx context.Context) { start(ctx, "") }
func outer(ctx context.Context) { inner(ctx) }
func TestA(t *T) { outer(context.Background()) }`,
			want: ctxUnbounded,
		},
		{
			name: "ctx is the second of two contexts in one field group",
			src: `package p
func helper(other, ctx context.Context) { start(ctx, "") }
func TestA(t *T) { helper(context.Background(), mustBounded()) }`,
			want: ctxUndetermined,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}
			idx := buildPkgIndex(map[string]*ast.File{"fixture.go": f})
			fn, arg := findStartCall(t, f)
			got, detail := classifyContainerCtxIn(idx, fn, arg, 0)
			if got != tc.want {
				t.Fatalf("classifyContainerCtxIn = %s (%s), want %s.\n"+
					"  The helper shape is how most container starts in this repo are written,\n"+
					"  so these arms are what decides whether the gate sees them at all. An arm\n"+
					"  answering 'bounded' by accident here certifies a hang; one answering\n"+
					"  'undetermined' by accident makes the gate unusable and invites relaxing it.",
					got, detail, tc.want)
			}
		})
	}
}

// findStartCall locates the `start(...)` marker call in a fixture and returns
// its enclosing function plus its first argument.
func findStartCall(t *testing.T, f *ast.File) (*ast.FuncDecl, ast.Expr) {
	t.Helper()
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var arg ast.Expr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "start" && len(call.Args) > 0 {
				arg = call.Args[0]
				return false
			}
			return true
		})
		if arg != nil {
			return fn, arg
		}
	}
	t.Fatalf("fixture contains no start(...) call — the fixture, not the classifier, is broken")
	return nil, nil
}

// parseClassifierFixture compiles a fixture body into a FuncDecl and returns the
// first argument of its `start(...)` call — the same expression the walk hands
// to the classifier.
func parseClassifierFixture(t *testing.T, body string) (*ast.FuncDecl, ast.Expr) {
	t.Helper()
	src := "package p\nfunc f() {\n" + body + "\n}\n"
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parsing fixture %q: %v", body, err)
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok {
		t.Fatalf("fixture %q did not produce a func decl", body)
	}
	var arg ast.Expr
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "start" && len(call.Args) > 0 {
			arg = call.Args[0]
			return false
		}
		return true
	})
	if arg == nil {
		t.Fatalf("fixture %q contains no start(...) call — the fixture, not the classifier, is broken", body)
	}
	return fn, arg
}
