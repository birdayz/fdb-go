package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// frl renders errors through fang, which TITLE-CASES THE FIRST WORD of the
// message before printing the banner (charm.land/fang/v2@v2.0.1, theme.go:220:
// `cases.Title(language.AmericanEnglish).String(firstWord)`). A message that
// leads with a flag therefore reaches the operator with the flag rewritten:
// `--no-fdb` prints as `--No-Fdb`, which reads like a typo in the very message
// telling them how to fix their invocation.
//
// This was written down as a three-point comment at ONE call site while other
// sites in the same binary kept leading with a flag. A property worth a comment
// is worth a gate — the comment cannot see the next error someone writes.
//
// The gate lives here rather than in cmd/frl because a test that reads Go
// SOURCE must resolve the real source tree: under Bazel a test runs in a
// runfiles tree holding only its declared inputs, so a package-local
// `filepath.Glob("*.go")` matches nothing and the gate passes having read
// nothing at all. That is not hypothetical — the first version of this test did
// exactly that, and only its own empty-set guard caught it.

// fangModulePath is the renderer whose banner title-cases the first word. The
// set of binaries in scope is DERIVED from who imports it rather than listed:
// a hand-maintained list of command trees is the same rot the ledger elsewhere
// in this package exists to prevent, and it would silently exclude the next
// fang-rendered binary someone adds.
//
// Scope matters in both directions. `cmd/test-budget` and
// `cmd/verify-corpus-retirement-history` print errors straight to stderr with
// no renderer, and their messages lead with a `flag`-package flag name
// (`-timeouts`) — which is correct there, and would be a false positive under
// a blanket `cmd/` scan.
const fangModulePath = "charm.land/fang"

// TestFangTitleCasesMangleALeadingFlag pins the upstream behaviour the gate
// below protects against. Asserting the dependency's behaviour rather than
// describing it means that if fang ever stops title-casing, this fails and the
// gate becomes removable on evidence instead of on a guess.
func TestFangTitleCasesMangleALeadingFlag(t *testing.T) {
	t.Parallel()
	if got := cases.Title(language.AmericanEnglish).String("--no-fdb"); got != "--No-Fdb" {
		t.Fatalf("fang's title-caser no longer mangles a leading flag: "+
			"cases.Title(%q) = %q, expected %q.\nIf this changed upstream, "+
			"TestCLIErrorMessagesDoNotLeadWithAFlag protects against something "+
			"that no longer happens and should be retired.", "--no-fdb", got, "--No-Fdb")
	}
	// The control: an ordinary sentence word survives title-casing readably,
	// which is precisely why leading with one is the fix.
	if got := cases.Title(language.AmericanEnglish).String("offline"); got != "Offline" {
		t.Fatalf("cases.Title(%q) = %q, expected %q", "offline", got, "Offline")
	}
}

// TestCLIErrorMessagesDoNotLeadWithAFlag fails on any fmt.Errorf under a
// fang-rendered command tree whose format literal begins with a flag.
func TestCLIErrorMessagesDoNotLeadWithAFlag(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var candidates []string
	fangTrees := map[string]bool{}
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") || !strings.HasPrefix(rel, "cmd/") {
			continue
		}
		candidates = append(candidates, rel)
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.Contains(string(src), fangModulePath) {
			fangTrees[commandTree(rel)] = true
		}
	}

	if len(fangTrees) == 0 {
		t.Fatalf("no command tree under cmd/ imports %s, across %d non-test files. "+
			"Either the renderer changed — in which case this gate and its "+
			"upstream pin should be retired together — or the enumeration broke "+
			"and this gate would report success having scanned nothing.",
			fangModulePath, len(candidates))
	}

	var sources []string
	for _, rel := range candidates {
		if fangTrees[commandTree(rel)] {
			sources = append(sources, rel)
		}
	}

	// A green from an empty set is not a green.
	if len(sources) < 10 {
		t.Fatalf("only %d non-test Go files are in scope across fang-rendered "+
			"trees %v; that is too few for this repo's CLI and suggests the "+
			"enumeration broke. A gate that reads no files fails OPEN.",
			len(sources), sortedKeys(fangTrees))
	}

	var offenders []string
	for _, rel := range sources {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			// A file that does not parse is not this gate's business; the
			// compiler reports it far more clearly.
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, name, ok := errorFormatLiteral(n)
			if !ok {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(text, "-") {
				return true
			}
			first := text
			if i := strings.IndexByte(text, ' '); i > 0 {
				first = text[:i]
			}
			pos := fset.Position(lit.Pos())
			offenders = append(offenders, strconv.Itoa(pos.Line)+" in "+rel+
				" — fmt."+name+" starting "+strconv.Quote(first)+", which renders as "+
				strconv.Quote(cases.Title(language.AmericanEnglish).String(first)))
			return true
		})
	}

	if len(offenders) > 0 {
		t.Fatalf("%d CLI error message(s) lead with a flag, which fang title-cases "+
			"in the banner:\n\t%s\n\nLead with a sentence word and name the flag inside "+
			"the sentence — \"offline rendering (--no-fdb) needs a file metadata "+
			"source\" rather than \"--no-fdb needs a file metadata source\".",
			len(offenders), strings.Join(offenders, "\n\t"))
	}
}

// commandTree returns the `cmd/<name>` prefix a repo-relative path sits under,
// which is the unit a binary is built from.
func commandTree(rel string) string {
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) < 2 {
		return rel
	}
	return parts[0] + "/" + parts[1]
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errorFormatLiteral reports whether n is a fmt.Errorf call whose first
// argument is a string literal, returning that literal and the function name.
//
// Errorf ONLY, deliberately. fmt.Sprintf builds display strings that never
// reach the error banner — `frl sql` uses it for a SQL comment header
// ("-- frl sql — edit and save…") and a psql-style record separator
// ("-[ RECORD 1 ]-"), both of which legitimately lead with a dash.
func errorFormatLiteral(n ast.Node) (*ast.BasicLit, string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil, "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return nil, "", false
	}
	if sel.Sel.Name != "Errorf" {
		return nil, "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil, "", false
	}
	return lit, sel.Sel.Name, true
}
