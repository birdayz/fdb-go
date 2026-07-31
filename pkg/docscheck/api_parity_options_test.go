package docscheck

import (
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

// API_PARITY.md vs options.go — the honest-options gate.
//
// `pkg/fdbgo/fdb/API_PARITY.md` splits every transaction option into three
// classes: honored, accepted-but-ignored (no-op), and rejected with
// `*UnsupportedOptionError`. That split exists because the page's own preamble
// says counting silent no-op setters as "implemented" is a migration trap. The
// page is what a prospective adopter reads to decide whether the pure-Go
// backend is safe for their workload.
//
// It was wrong. Measured on the audit tree: `ReportConflictingKeys` and
// `BypassStorageQuota` were listed under "Accepted but ignored — no-op (fails
// safe)" while `options.go` returned `*UnsupportedOptionError` for both, each
// with a comment saying in terms that it "fails UNSAFE if ignored" — the doc
// stated the exact opposite of the code for two options, and the Rejected table
// listed three entries against seven rejecting sites in code. `SpanParent` was
// filed as a no-op while its body forwards to the transaction.
//
// A one-time correction re-rots on the next option added, so the correction is
// not the deliverable — this gate is. It parses every option method body in
// `options.go`, classifies it from the CODE, and fails when the classification
// disagrees with the page. Same shape as TestNightlyWindowGatesAreReconciled:
// the doc cannot drift because drifting fails the build.

const (
	optionsGoRel  = "pkg/fdbgo/fdb/options.go"
	apiParityRel  = "pkg/fdbgo/fdb/API_PARITY.md"
	txOptReceiver = "goTransactionOptions"
	dbOptReceiver = "DatabaseOptions"
)

// optionClass is what an option body does on the pure-Go backend.
type optionClass int

const (
	classHonored optionClass = iota
	classNoOp
	classRejected
)

func (c optionClass) String() string {
	switch c {
	case classHonored:
		return "honored"
	case classNoOp:
		return "no-op"
	default:
		return "rejected"
	}
}

// classifyOptionBody reads one option method body and decides its class.
//
// Rejected: the body returns an `&UnsupportedOptionError{...}` composite.
// No-op: the body's only statement is `return nil` — nothing is forwarded, so
// the option is silently swallowed.
// Honored: anything else (it calls into the transaction/client, computes, or
// returns a real error from real work).
//
// Comments are irrelevant to the classification on purpose. The defect this
// gate exists to catch was a comment and a doc row that disagreed with the
// statements around them, so only statements may decide.
func classifyOptionBody(fn *ast.FuncDecl) optionClass {
	rejected := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ue, ok := n.(*ast.UnaryExpr)
		if !ok || ue.Op != token.AND {
			return true
		}
		cl, ok := ue.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "UnsupportedOptionError" {
			rejected = true
		}
		return true
	})
	if rejected {
		return classRejected
	}
	// `return nil` and nothing else.
	if len(fn.Body.List) == 1 {
		if ret, ok := fn.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			if id, ok := ret.Results[0].(*ast.Ident); ok && id.Name == "nil" {
				return classNoOp
			}
		}
	}
	return classHonored
}

// receiverName returns the bare type name of a method receiver.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// scanOptions classifies every `Set*` option method on the two option
// receivers in options.go.
// Non-setter methods on the option receivers (EnsureMutationCapacity is one)
// are returned in `other` so the reverse check can tell "the page names a
// method that is not an option" from "the page names something that does not
// exist at all" — only the latter is a defect.
func scanOptions(t *testing.T, root string) (tx, db map[string]optionClass, other map[string]bool) {
	t.Helper()
	path := filepath.Join(root, optionsGoRel)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", optionsGoRel, err)
	}
	tx, db, other = map[string]optionClass{}, map[string]optionClass{}, map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		recv := receiverName(fn)
		if recv != txOptReceiver && recv != dbOptReceiver {
			continue
		}
		if !strings.HasPrefix(fn.Name.Name, "Set") {
			other[fn.Name.Name] = true
			continue
		}
		name := strings.TrimPrefix(fn.Name.Name, "Set")
		if recv == txOptReceiver {
			tx[name] = classifyOptionBody(fn)
		} else {
			db[name] = classifyOptionBody(fn)
		}
	}
	return tx, db, other
}

var backticked = regexp.MustCompile("`([A-Za-z][A-Za-z0-9]*)`")

// parityTables pulls the three TransactionOptions classifications out of
// API_PARITY.md. Each section is delimited by its `###` heading; the option
// names are the backticked identifiers inside it, normalised by dropping the
// `Set` prefix so the page may spell them either way.
func parityTables(t *testing.T, root string) map[string]optionClass {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, apiParityRel))
	if err != nil {
		t.Fatalf("read %s: %v", apiParityRel, err)
	}
	body := string(b)

	sections := []struct {
		heading string
		class   optionClass
	}{
		{"### Honored", classHonored},
		{"### Rejected", classRejected},
		{"### Accepted but ignored", classNoOp},
	}
	out := map[string]optionClass{}
	for _, s := range sections {
		start := strings.Index(body, s.heading)
		if start < 0 {
			t.Fatalf("%s: section %q not found — the gate's section delimiters have drifted from the page",
				apiParityRel, s.heading)
		}
		rest := body[start+len(s.heading):]
		if end := strings.Index(rest, "\n## "); end >= 0 {
			rest = rest[:end]
		}
		if end := strings.Index(rest, "\n### "); end >= 0 {
			rest = rest[:end]
		}
		for _, m := range backticked.FindAllStringSubmatch(rest, -1) {
			name := strings.TrimPrefix(m[1], "Set")
			if prev, dup := out[name]; dup && prev != s.class {
				t.Errorf("%s lists %q under BOTH %s and %s — an option has one class",
					apiParityRel, name, prev, s.class)
			}
			out[name] = s.class
		}
	}
	return out
}

// TestAPIParityTablesMatchOptionsGo is the gate. Every option method in
// options.go must appear in API_PARITY.md under the class its BODY has, and
// the page may not name an option that does not exist.
func TestAPIParityTablesMatchOptionsGo(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	code, dbCode, otherMethods := scanOptions(t, root)
	doc := parityTables(t, root)

	// Anti-vacuity floor. A broken parse — a renamed receiver, a moved file, a
	// go/ast change — would otherwise make this whole gate pass over an empty
	// set and report nothing, which is the exact failure mode (a check that is
	// green because it examined nothing) the page's own history is about.
	if len(code) < 40 {
		t.Fatalf("parsed only %d %s option methods from %s — the parse is broken, "+
			"not the doc; fix the scan rather than trusting this run",
			len(code), txOptReceiver, optionsGoRel)
	}
	if len(doc) < 40 {
		t.Fatalf("parsed only %d option names from %s — the section delimiters or the "+
			"backtick convention have drifted; fix the scan rather than trusting this run",
			len(doc), apiParityRel)
	}
	// The rejected class must be non-empty in both. It is the class that carries
	// the safety claim, and it is the one that was wrong.
	var rejectedInCode int
	for _, c := range code {
		if c == classRejected {
			rejectedInCode++
		}
	}
	if rejectedInCode == 0 {
		t.Fatalf("no rejecting option bodies found in %s — classifyOptionBody no longer "+
			"recognises UnsupportedOptionError, so every reject would silently read as honored",
			optionsGoRel)
	}

	var names []string
	for n := range code {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		want := code[n]
		got, listed := doc[n]
		if !listed {
			t.Errorf("option %q is %s in %s but appears in NO table of %s — "+
				"an unlisted option is exactly the migration trap the split exists to remove",
				n, want, optionsGoRel, apiParityRel)
			continue
		}
		if got != want {
			t.Errorf("option %q: %s says %s, %s says %s — the doc must follow the code",
				n, apiParityRel, got, optionsGoRel, want)
		}
	}
	for n := range doc {
		if _, ok := code[n]; ok {
			continue
		}
		// The page's prose also names DB-level defaults and the receivers'
		// non-setter methods; only a name that exists nowhere is a defect.
		if _, ok := dbCode[n]; ok {
			continue
		}
		if _, ok := dbCode["Transaction"+n]; ok {
			continue
		}
		if otherMethods[n] {
			continue
		}
		t.Errorf("%s lists option %q, which is not a method on %s or %s — "+
			"the page names an option that does not exist",
			apiParityRel, n, txOptReceiver, dbOptReceiver)
	}
}

// TestAPIParityDatabaseOptionRejectsAreDocumented pins the DB-level half. The
// per-transaction and DB-default surfaces have to keep the SAME taxonomy — a DB
// default that silently swallows what the per-transaction setter rejects
// re-opens the trap on the other surface — so every rejecting DatabaseOptions
// method must be named on the page.
func TestAPIParityDatabaseOptionRejectsAreDocumented(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	_, db, _ := scanOptions(t, root)
	if len(db) == 0 {
		t.Fatalf("parsed no %s Set* methods from %s — the parse is broken", dbOptReceiver, optionsGoRel)
	}
	page := readDoc(t, root, apiParityRel)
	found := 0
	for name, class := range db {
		if class != classRejected {
			continue
		}
		found++
		if !strings.Contains(page, "Set"+name) && !strings.Contains(page, "`"+name+"`") {
			t.Errorf("%s.Set%s rejects with UnsupportedOptionError but %s never names it",
				dbOptReceiver, name, apiParityRel)
		}
	}
	if found == 0 {
		t.Fatalf("no rejecting %s methods found in %s — either the DB-level rejects were "+
			"removed (a real regression in the taxonomy) or classifyOptionBody stopped "+
			"recognising them", dbOptReceiver, optionsGoRel)
	}
}
