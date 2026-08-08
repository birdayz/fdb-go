package docscheck

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// The detector behind TestUnexportedFuncsAreReferenced (unreferenced_func_gate_test.go).
// It is a pure function over a package's sources so every arm can be driven from a
// unit pin rather than only by whatever the repository happens to contain today.
//
// THE SHAPE IT LOOKS FOR is narrow on purpose: an **unexported top-level func with
// no receiver** that no NON-TEST file in its own package references. Unexported and
// no-receiver together exclude, by construction, the two populations that make this
// class of check unusable elsewhere — the external API surface (an exported func may
// have no in-tree caller and still be the product) and interface satisfaction (a
// method can be reached through an interface the scanner cannot see without type
// information). What is left is code that only its own package can call, reachable
// only by name, which makes a syntactic reference count sound.
//
// WHY THE SHAPE IS WORTH A GATE. Twice in this tree a function in exactly this state
// was protecting a live defect, and neither was found by reading — both were found by
// trying to delete them. A range-building helper returned a single TupleRange, which
// could not express the second range a `> 3.14` DOUBLE bound requires, so it dropped
// rows while 27 test call sites reported green. A compiled key-expression twin had a
// test asserting that a nested record-type key returns nil, which pinned a silent
// data-loss bug — RecordTypeKey().Nest() collapsing every record of a type onto one
// primary key — as CORRECT. A test suite cannot distinguish "this code is right" from
// "this code is not the code that runs"; only a reference count can.
//
// REFERENCE COUNTING IS SYNTACTIC AND DELIBERATELY CONSERVATIVE. Every arm that could
// be wrong is wrong in the QUIET direction — it over-counts references, so the gate
// under-reports rather than crying wolf:
//
//   - a selector's `.Sel` is never counted (`x.parse` is a field or method, never a
//     reference to a package-level func named `parse`); neither is a struct field's,
//     parameter's or result's declared NAME, nor any func's own declared name. These
//     three are precision GAINS rather than conservatism, and all three are sound for
//     the same reason: each of those positions is a declaration, and within a package
//     a top-level func is referenced by bare identifier only;
//   - a composite-literal KEY is counted, so a struct field named like a func silently
//     discharges it. Under-reporting, on purpose: distinguishing a struct field key
//     from a map key holding a func value needs type information;
//   - files excluded by a build constraint other than `ignore` still contribute both
//     candidates and references, so a build-tagged caller keeps its callee alive;
//   - GENERATED files contribute references but never candidates.
//
// The one place it is deliberately STRICT is recursion: a func's own body does not
// discharge it. A function that only calls itself is dead, and reading recursion as a
// reference is how a dead recursive helper hides. MUTUAL recursion between two dead
// funcs does still hide both — a known limit, pinned in the unit tests so it is a
// documented property rather than a surprise.
//
// WHAT GREEN HERE DOES NOT PROVE. It proves no *unexported no-receiver func* is
// unreferenced from production. It says nothing about dead exported funcs, dead
// methods, dead types, or a func that is referenced by a caller which is itself dead.
// Read a pass as exactly the narrow claim it makes.

// funcSite identifies one candidate function. Path is repo-relative.
type funcSite struct {
	Path string
	Name string
	Line int
}

// key is the ledger key: path plus name, and deliberately NOT the line. A line
// number would make every entry stale on any edit above it, which trains readers
// to re-stamp the ledger without reading it — the opposite of what a ratchet is for.
func (s funcSite) key() string { return s.Path + " # " + s.Name }

// scanOutcome is one package's reading.
type scanOutcome struct {
	// Candidates is every unexported no-receiver func considered, whether or not
	// it is referenced. It is the population whose collapse would make a clean
	// result meaningless, so the gate guards it.
	Candidates int
	// Unreferenced are the candidates with zero references from non-test files.
	Unreferenced []funcSite
	// TestRefs counts references from _test.go files, keyed by funcSite.key().
	// Zero here means dead everywhere; non-zero is the dead-twin shape.
	TestRefs map[string]int
}

// scanPackageSources reads one directory's Go sources. Keys of sources are
// repo-relative paths; values are the file contents.
func scanPackageSources(sources map[string]string) (scanOutcome, error) {
	out := scanOutcome{TestRefs: map[string]int{}}

	type parsedFile struct {
		path      string
		file      *ast.File
		isTest    bool
		generated bool
	}
	paths := make([]string, 0, len(sources))
	for p := range sources {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	fset := token.NewFileSet()
	var files []parsedFile
	for _, p := range paths {
		src := []byte(sources[p])
		f, err := parser.ParseFile(fset, p, src, parser.ParseComments)
		if err != nil {
			return scanOutcome{}, err
		}
		if buildIgnored(f) {
			continue
		}
		files = append(files, parsedFile{
			path:      p,
			file:      f,
			isTest:    strings.HasSuffix(filepath.Base(p), "_test.go"),
			generated: isGeneratedFile(src, f),
		})
	}

	// Candidates come from non-test, non-generated files only.
	candidates := map[string]funcSite{}
	for _, pf := range files {
		if pf.isTest || pf.generated {
			continue
		}
		for _, decl := range pf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name == nil {
				continue
			}
			name := fd.Name.Name
			// `init` is called by the runtime and `_` is a compile-time-only
			// declaration; neither is reachable by name, so a reference count
			// says nothing about them.
			if name == "init" || name == "_" || ast.IsExported(name) {
				continue
			}
			// `main` is linker-invoked ONLY in package main. In a library
			// package `func main()` is an ordinary unexported function, and
			// exempting it unconditionally would hide exactly the kind of dead
			// helper this gate exists to find — under a name nobody would think
			// to grep for.
			if name == "main" && pf.file.Name != nil && pf.file.Name.Name == "main" {
				continue
			}
			if _, seen := candidates[name]; seen {
				// Same name in two files means build-tag variants. Keep the
				// first by sorted path so the reading is deterministic; the
				// reference count below is shared, which is the quiet direction.
				continue
			}
			candidates[name] = funcSite{Path: pf.path, Name: name, Line: fset.Position(fd.Pos()).Line}
		}
	}
	out.Candidates = len(candidates)

	prodRefs := map[string]int{}
	for _, pf := range files {
		for name, n := range referencesIn(pf.file, candidates) {
			if pf.isTest {
				out.TestRefs[candidates[name].key()] += n
			} else {
				prodRefs[name] += n
			}
		}
	}

	for name, site := range candidates {
		if prodRefs[name] == 0 {
			out.Unreferenced = append(out.Unreferenced, site)
		}
	}
	sort.Slice(out.Unreferenced, func(i, j int) bool {
		if out.Unreferenced[i].Path != out.Unreferenced[j].Path {
			return out.Unreferenced[i].Path < out.Unreferenced[j].Path
		}
		return out.Unreferenced[i].Line < out.Unreferenced[j].Line
	})
	return out, nil
}

// referencesIn counts, per candidate name, the identifier references in f —
// excluding each candidate's own declaration (signature and body), so direct
// recursion does not keep a dead function alive, and excluding selector `.Sel`
// idents, which within a package are always fields or methods.
func referencesIn(f *ast.File, candidates map[string]funcSite) map[string]int {
	counts := map[string]int{}

	count := func(n ast.Node, own string) {
		var visit func(ast.Node) bool
		visit = func(x ast.Node) bool {
			switch v := x.(type) {
			case *ast.SelectorExpr:
				// `x.parse` is a field or a method. Within a package a
				// top-level func is referenced by bare identifier only.
				ast.Inspect(v.X, visit)
				return false
			case *ast.Field:
				// A struct field's, parameter's or result's NAMES are
				// declarations, never references. Only the type can refer.
				if v.Type != nil {
					ast.Inspect(v.Type, visit)
				}
				return false
			case *ast.Ident:
				if _, ok := candidates[v.Name]; ok && v.Name != own {
					counts[v.Name]++
				}
				return false
			}
			return true
		}
		ast.Inspect(n, visit)
	}

	for _, decl := range f.Decls {
		if fd, isFunc := decl.(*ast.FuncDecl); isFunc {
			// A declaration is never a reference to itself, so fd.Name is
			// skipped for EVERY func — including methods, whose name may
			// coincide with a package-level func's.
			own := ""
			if fd.Recv == nil && fd.Name != nil {
				if _, isCandidate := candidates[fd.Name.Name]; isCandidate {
					// Direct recursion does not keep a dead function alive.
					own = fd.Name.Name
				}
			}
			if fd.Recv != nil {
				count(fd.Recv, own)
			}
			if fd.Type != nil {
				count(fd.Type, own)
			}
			if fd.Body != nil {
				count(fd.Body, own)
			}
			continue
		}
		count(decl, "")
	}
	return counts
}

// buildIgnored reports whether a file is excluded from every build by an
// `ignore` constraint — a generator or a scratch program, not package code.
// Other constraints (GOOS, cgo, a release tag) are NOT treated as exclusions:
// such a file is real package code under some configuration, and dropping it
// would both lose its candidates and, worse, lose the references it makes.
func buildIgnored(f *ast.File) bool {
	for _, group := range f.Comments {
		// Only the header matters: a constraint must precede the package clause.
		if group.Pos() > f.Package {
			break
		}
		for _, c := range group.List {
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				// Not a build constraint (an ordinary comment), or one the
				// toolchain itself would reject. Either way it excludes nothing.
				continue
			}
			// The constraint is PARSED, not token-scanned. A scan for the word
			// `ignore` drops `//go:build ignore || tools`, which IS built under
			// `-tags tools` — and dropping a built file loses both its
			// candidates and, worse, the references it makes to candidates
			// elsewhere, so the gate's answer would depend on which tags
			// happened to be in the scan.
			//
			// The test is "does ANY tag assignment with `ignore` unset build
			// this file?". It has to be a real satisfiability question, not a
			// single evaluation: fixing every other tag to true reports
			// `//go:build !race` as unbuildable, which is exactly backwards.
			if !buildableWithoutIgnoreTag(expr) {
				return true
			}
		}
	}
	return false
}

// buildableWithoutIgnoreTag reports whether some assignment of build tags with
// `ignore` FALSE satisfies expr — i.e. whether the file is real package code
// under any configuration a normal build could use.
//
// Exhaustive over the constraint's own tags rather than a single evaluation,
// because build expressions contain negations and a fixed assignment answers
// the wrong question in both directions: all-true reports `!race` as
// unbuildable, all-false reports `linux` as unbuildable. Real constraints carry
// a handful of tags; the cap below keeps a pathological one from being
// exponential, and it fails toward KEEPING the file, which only ever makes the
// gate stricter.
func buildableWithoutIgnoreTag(expr constraint.Expr) bool {
	var tags []string
	seen := map[string]bool{"ignore": true}
	var collect func(constraint.Expr)
	collect = func(e constraint.Expr) {
		switch t := e.(type) {
		case *constraint.TagExpr:
			if !seen[t.Tag] {
				seen[t.Tag] = true
				tags = append(tags, t.Tag)
			}
		case *constraint.NotExpr:
			collect(t.X)
		case *constraint.AndExpr:
			collect(t.X)
			collect(t.Y)
		case *constraint.OrExpr:
			collect(t.X)
			collect(t.Y)
		}
	}
	collect(expr)

	const maxTags = 16
	if len(tags) > maxTags {
		return true
	}
	for mask := 0; mask < 1<<len(tags); mask++ {
		set := map[string]bool{}
		for i, tag := range tags {
			set[tag] = mask&(1<<i) != 0
		}
		if expr.Eval(func(tag string) bool { return set[tag] }) {
			return true
		}
	}
	return false
}
