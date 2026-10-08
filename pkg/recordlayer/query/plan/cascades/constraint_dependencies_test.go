package cascades

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// TestRuleConstraintDependencies_DeclareWhatTheyRead is RFC-257 WS-F D2's
// declaration guard. A re-exploration re-queues a rule only when one of its
// declared constraint keys changed since the group's last committed
// exploration (shouldPushRule, Java's CascadesPlanner.java:956-970), and an
// undeclared rule gets Java's empty set. So a rule that READS a constraint
// without declaring it would miss the re-fire its new input calls for, and plan
// on a stale constraint.
//
// The check is over the package's own sources: every method of a *Rule type
// whose body reads a constraint -- the GetRequestedOrderings accessor or a
// constraint key -- needs a ConstraintDependencies method on the same type that
// names the key. A read inside a free helper is not seen; such a helper takes
// the rule call, so keep the read in the rule's own method.
func TestRuleConstraintDependencies_DeclareWhatTheyRead(t *testing.T) {
	t.Parallel()
	readKey := map[string]string{
		"GetRequestedOrderings":          "RequestedOrderingConstraintKey",
		"RequestedOrderingConstraintKey": "RequestedOrderingConstraintKey",
		"ReferencedFieldsConstraintKey":  "ReferencedFieldsConstraintKey",
		"OrdinalLayoutConstraintKey":     "OrdinalLayoutConstraintKey",
	}
	entries, err := cascadesSourceFS.ReadDir(".")
	if err != nil {
		t.Fatalf("reading embedded sources: %v", err)
	}
	fset := token.NewFileSet()
	reads := map[string]map[string]bool{}    // rule type -> keys its methods read
	declares := map[string]map[string]bool{} // rule type -> keys it declares
	ruleMethods := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := cascadesSourceFS.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			recv, ok := star.X.(*ast.Ident)
			if !ok || !strings.HasSuffix(recv.Name, "Rule") {
				continue
			}
			ruleMethods++
			target := reads
			if fn.Name.Name == "ConstraintDependencies" {
				target = declares
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var ident string
				switch x := n.(type) {
				case *ast.Ident:
					ident = x.Name
				case *ast.SelectorExpr:
					ident = x.Sel.Name
				}
				if key, ok := readKey[ident]; ok {
					if target[recv.Name] == nil {
						target[recv.Name] = map[string]bool{}
					}
					target[recv.Name][key] = true
				}
				return true
			})
		}
	}
	// Anti-vacuity: the parse must see the package's rule methods and its
	// known readers.
	if ruleMethods < 250 || len(reads) < 20 {
		t.Fatalf("parsed %d rule methods and %d constraint readers -- the scan is broken", ruleMethods, len(reads))
	}
	var missing []string
	for rule, keys := range reads {
		for key := range keys {
			if !declares[rule][key] {
				missing = append(missing, rule+" reads "+key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("rules read a planner constraint they do not declare in ConstraintDependencies:\n%s", strings.Join(missing, "\n"))
	}
}
