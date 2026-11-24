package infra

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type buildRule struct {
	kind       string
	name       string
	visibility []string
	refs       []string
}

type parsedBuildFile struct {
	rel                      string
	gazelleDefaultVisibility []string
	hasGazelleIgnore         bool
	pkgDefaultVisibility     []string
	hasPkgDefaultVisibility  bool
	publicVisibilityLines    []int
	packageGroups            map[string][]string
	rules                    []buildRule
}

type starlarkToken struct {
	kind string // "ident", "string", "punct", "comment"
	val  string
	line int
}

func scanStarlark(src string) []starlarkToken {
	var tokens []starlarkToken
	line := 1
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == '\n' {
			line++
			i++
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\r' {
			i++
			continue
		}
		if ch == '#' {
			start := i
			for i < len(src) && src[i] != '\n' {
				i++
			}
			tokens = append(tokens, starlarkToken{kind: "comment", val: src[start:i], line: line})
			continue
		}
		if ch == '"' || ch == '\'' {
			startLine := line
			quote := ch
			if i+2 < len(src) && src[i+1] == quote && src[i+2] == quote {
				delim := src[i : i+3]
				i += 3
				start := i
				for i < len(src) {
					if src[i] == '\\' && i+1 < len(src) {
						if src[i+1] == '\n' {
							line++
						}
						i += 2
						continue
					}
					if i+2 < len(src) && src[i:i+3] == delim {
						val := src[start:i]
						i += 3
						tokens = append(tokens, starlarkToken{kind: "string", val: val, line: startLine})
						goto nextToken
					}
					if src[i] == '\n' {
						line++
					}
					i++
				}
				tokens = append(tokens, starlarkToken{kind: "string", val: src[start:], line: startLine})
				continue
			}
			i++
			start := i
			for i < len(src) && src[i] != quote && src[i] != '\n' {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				i++
			}
			val := src[start:i]
			if i < len(src) && src[i] == quote {
				i++
			}
			tokens = append(tokens, starlarkToken{kind: "string", val: val, line: startLine})
			continue
		}
		if isIdentStart(ch) {
			start := i
			i++
			for i < len(src) && isIdentCont(src[i]) {
				i++
			}
			tokens = append(tokens, starlarkToken{kind: "ident", val: src[start:i], line: line})
			continue
		}
		tokens = append(tokens, starlarkToken{kind: "punct", val: src[i : i+1], line: line})
		i++
	nextToken:
	}
	return tokens
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func parseBuildFile(rel, src string) parsedBuildFile {
	parsed := parsedBuildFile{
		rel:           rel,
		packageGroups: map[string][]string{},
	}
	tokens := scanStarlark(src)

	var code []starlarkToken
	for _, tok := range tokens {
		if tok.kind == "comment" {
			body := strings.TrimSpace(strings.TrimPrefix(tok.val, "#"))
			if body == "gazelle:ignore" {
				parsed.hasGazelleIgnore = true
			} else if strings.HasPrefix(body, "gazelle:default_visibility") {
				rest := strings.TrimSpace(strings.TrimPrefix(body, "gazelle:default_visibility"))
				for _, part := range strings.Split(rest, ",") {
					if part = strings.TrimSpace(part); part != "" {
						parsed.gazelleDefaultVisibility = append(parsed.gazelleDefaultVisibility, part)
					}
				}
			}
			continue
		}
		if tok.kind == "string" && tok.val == "//visibility:public" {
			parsed.publicVisibilityLines = append(parsed.publicVisibilityLines, tok.line)
		}
		code = append(code, tok)
	}

	pkgLabel := "//"
	if rel != "BUILD.bazel" {
		pkgLabel = "//" + strings.TrimSuffix(rel, "/BUILD.bazel")
	}

	i := 0
	for i < len(code) {
		if code[i].kind == "ident" && i+1 < len(code) && code[i+1].kind == "punct" && code[i+1].val == "(" {
			callKind := code[i].val
			end := findCallEnd(code, i+1)
			args := code[i+2 : end]
			switch callKind {
			case "package":
				if vis, ok := extractStringListAttr(args, "default_visibility"); ok {
					parsed.hasPkgDefaultVisibility = true
					parsed.pkgDefaultVisibility = vis
				}
			case "package_group":
				name := extractStringAttr(args, "name")
				pkgs, _ := extractStringListAttr(args, "packages")
				if name != "" {
					parsed.packageGroups[pkgLabel+":"+name] = pkgs
				}
			case "go_library", "filegroup", "go_test", "go_binary", "proto_library", "go_proto_library":
				rule := buildRule{
					kind: callKind,
					name: extractStringAttr(args, "name"),
					refs: extractLabelRefs(args),
				}
				if vis, ok := extractStringListAttr(args, "visibility"); ok {
					rule.visibility = vis
				}
				parsed.rules = append(parsed.rules, rule)
			}
			i = end + 1
			continue
		}
		i++
	}
	return parsed
}

func findCallEnd(code []starlarkToken, openParen int) int {
	depth := 0
	for i := openParen; i < len(code); i++ {
		if code[i].kind == "punct" {
			switch code[i].val {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return len(code) - 1
}

func extractStringAttr(args []starlarkToken, attr string) string {
	for i := 0; i+2 < len(args); i++ {
		if isTopLevelAttr(args, i, attr) && args[i+2].kind == "string" {
			return args[i+2].val
		}
	}
	return ""
}

func extractStringListAttr(args []starlarkToken, attr string) ([]string, bool) {
	for i := 0; i+2 < len(args); i++ {
		if !isTopLevelAttr(args, i, attr) {
			continue
		}
		if args[i+2].kind == "string" {
			return []string{args[i+2].val}, true
		}
		if args[i+2].kind == "punct" && args[i+2].val == "[" {
			var out []string
			for j := i + 3; j < len(args); j++ {
				if args[j].kind == "punct" && args[j].val == "]" {
					return out, true
				}
				if args[j].kind == "string" {
					out = append(out, args[j].val)
				}
			}
		}
	}
	return nil, false
}

func isTopLevelAttr(args []starlarkToken, idx int, attr string) bool {
	if idx+2 >= len(args) {
		return false
	}
	if args[idx].kind != "ident" || args[idx].val != attr {
		return false
	}
	if args[idx+1].kind != "punct" || args[idx+1].val != "=" {
		return false
	}
	depth := 0
	for j := 0; j < idx; j++ {
		if args[j].kind == "punct" {
			switch args[j].val {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
		}
	}
	return depth == 0
}

func extractLabelRefs(args []starlarkToken) []string {
	var refs []string
	for _, tok := range args {
		if tok.kind == "string" && strings.HasPrefix(tok.val, "//") {
			if strings.HasPrefix(tok.val, "//visibility:") ||
				strings.HasSuffix(tok.val, ":__pkg__") ||
				strings.HasSuffix(tok.val, ":__subpackages__") ||
				strings.HasSuffix(tok.val, "/...") {
				continue
			}
			refs = append(refs, tok.val)
		}
	}
	return refs
}

func visibilityAllows(vis []string, callerPkg, targetPkg string, pkgGroups map[string][]string) bool {
	if callerPkg == targetPkg {
		return true
	}
	for _, spec := range vis {
		switch {
		case spec == "//visibility:public":
			return true
		case spec == "//visibility:private":
			continue
		case strings.HasSuffix(spec, ":__pkg__"):
			pkg := strings.TrimSuffix(spec, ":__pkg__")
			if callerPkg == pkg {
				return true
			}
		case strings.HasSuffix(spec, ":__subpackages__"):
			pkg := strings.TrimSuffix(spec, ":__subpackages__")
			if pkg == "//" || callerPkg == pkg || strings.HasPrefix(callerPkg, pkg+"/") {
				return true
			}
		default:
			if pkgs, ok := pkgGroups[spec]; ok {
				for _, p := range pkgs {
					if p == "//..." {
						return true
					}
					if strings.HasSuffix(p, "/...") {
						prefix := strings.TrimSuffix(p, "/...")
						if prefix == "//" || callerPkg == prefix || strings.HasPrefix(callerPkg, prefix+"/") {
							return true
						}
					} else if callerPkg == p {
						return true
					}
				}
			}
		}
	}
	return false
}

func visibilityRepoRoot(t *testing.T) string {
	t.Helper()
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		if real, err := filepath.EvalSymlinks(filepath.Join(sd, ws, "MODULE.bazel")); err == nil {
			return filepath.Dir(real)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "MODULE.bazel")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("MODULE.bazel not found")
		}
		dir = parent
	}
}

func collectAllBuildFiles(t *testing.T, root string) []parsedBuildFile {
	t.Helper()
	var files []parsedBuildFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if rel == ".git" || rel == "fdb-record-layer" || rel == "layers/otel/grafana" || strings.HasPrefix(rel, "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "BUILD.bazel" {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		files = append(files, parseBuildFile(rel, string(data)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(files, func(a, b parsedBuildFile) int {
		return strings.Compare(a.rel, b.rel)
	})
	return files
}

func inRestrictedVisibilityScope(rel string) bool {
	return strings.HasPrefix(rel, "internal/") ||
		strings.HasPrefix(rel, "pkg/internal/") ||
		(strings.HasPrefix(rel, "pkg/") && strings.Contains(rel, "/internal/")) ||
		strings.HasPrefix(rel, "pkg/testutil/") ||
		strings.HasPrefix(rel, "pkg/simfdb/") ||
		strings.HasPrefix(rel, "pkg/relational/conformance/") ||
		strings.HasPrefix(rel, "pkg/relational/sqltest/") ||
		strings.HasPrefix(rel, "pkg/fdbgo/bench/") ||
		strings.HasPrefix(rel, "pkg/fdbgo/bindingtest/") ||
		strings.HasPrefix(rel, "pkg/fdbgo/conformance/") ||
		strings.HasPrefix(rel, "pkg/fdbgo/recovery/")
}

var restrictedScopeRoots = []struct {
	prefix      string
	wantDefault []string
}{
	{"internal/", []string{"//:__subpackages__"}},
	{"internal/linters/", []string{"//:__pkg__"}},
	{"pkg/internal/", []string{"//pkg:__subpackages__"}},
	{"pkg/fdbgo/internal/", []string{"//pkg/fdbgo:__subpackages__"}},
	{"pkg/recordlayer/internal/", []string{"//pkg/recordlayer:__subpackages__"}},
	{"pkg/recordlayer/query/plan/cascades/internal/", []string{"//pkg/recordlayer/query/plan/cascades:__subpackages__"}},
	{"pkg/relational/internal/", []string{"//pkg/relational:__subpackages__"}},
	{"pkg/testutil/", []string{"//pkg/recordlayer:__subpackages__", "//pkg/relational:__subpackages__"}},
	{"pkg/simfdb/", []string{"//pkg/simfdb:__subpackages__"}},
	{"pkg/relational/conformance/", []string{"//pkg/relational/conformance:__subpackages__"}},
	{"pkg/relational/sqltest/", []string{"//pkg/relational:__subpackages__"}},
	{"pkg/fdbgo/bench/", []string{"//pkg/fdbgo:__subpackages__"}},
	{"pkg/fdbgo/bindingtest/", []string{"//pkg/fdbgo:__subpackages__"}},
	{"pkg/fdbgo/conformance/", []string{"//pkg/fdbgo:__subpackages__"}},
	{"pkg/fdbgo/recovery/", []string{"//pkg/fdbgo:__subpackages__"}},
}

func TestNoBlanketPublicVisibilityInRestrictedScopes(t *testing.T) {
	t.Parallel()
	files := collectAllBuildFiles(t, visibilityRepoRoot(t))
	scopeCounts := map[string]int{}
	totalChecked := 0

	for _, f := range files {
		if !inRestrictedVisibilityScope(f.rel) {
			continue
		}
		totalChecked++
		for _, root := range restrictedScopeRoots {
			if strings.HasPrefix(f.rel, root.prefix) {
				scopeCounts[root.prefix]++
			}
		}
		for _, line := range f.publicVisibilityLines {
			t.Errorf("%s:%d: restricted package declares //visibility:public", f.rel, line)
		}
	}

	if totalChecked < 90 {
		t.Fatalf("checked %d restricted BUILD.bazel files, want >= 90", totalChecked)
	}
	for _, root := range restrictedScopeRoots {
		if scopeCounts[root.prefix] == 0 {
			t.Errorf("no BUILD.bazel files found under %s", root.prefix)
		}
	}
}

func TestGazelleDefaultVisibilityConfigured(t *testing.T) {
	t.Parallel()
	files := collectAllBuildFiles(t, visibilityRepoRoot(t))
	byRel := make(map[string]parsedBuildFile, len(files))
	for _, f := range files {
		byRel[f.rel] = f
	}

	for _, root := range restrictedScopeRoots {
		rootFile := root.prefix + "BUILD.bazel"
		f, ok := byRel[rootFile]
		if !ok {
			t.Errorf("missing %s with # gazelle:default_visibility", rootFile)
			continue
		}
		if !slices.Equal(f.gazelleDefaultVisibility, root.wantDefault) {
			t.Errorf("%s: # gazelle:default_visibility = %v, want %v", rootFile, f.gazelleDefaultVisibility, root.wantDefault)
		}
		if slices.Contains(f.gazelleDefaultVisibility, "//visibility:public") {
			t.Errorf("%s: # gazelle:default_visibility must not be //visibility:public", rootFile)
		}
	}

	// Verify every BUILD.bazel under a restricted root declares package(default_visibility)
	// matching the most specific ancestor # gazelle:default_visibility directive so
	// running 'just gazelle' produces zero drift.
	checked := 0
	for _, f := range files {
		if !inRestrictedVisibilityScope(f.rel) {
			continue
		}
		var effective []string
		dir := filepath.ToSlash(filepath.Dir(f.rel))
		for {
			candidate := dir + "/BUILD.bazel"
			if dir == "." {
				candidate = "BUILD.bazel"
			}
			if anc, ok := byRel[candidate]; ok && len(anc.gazelleDefaultVisibility) > 0 {
				effective = anc.gazelleDefaultVisibility
				break
			}
			if dir == "." {
				break
			}
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
		if len(effective) == 0 {
			t.Errorf("%s: no ancestor # gazelle:default_visibility directive found", f.rel)
			continue
		}
		checked++
		if !f.hasPkgDefaultVisibility || !slices.Equal(f.pkgDefaultVisibility, effective) {
			t.Errorf("%s: package(default_visibility = %v) (present=%v), want %v to stay in sync with gazelle:default_visibility",
				f.rel, f.pkgDefaultVisibility, f.hasPkgDefaultVisibility, effective)
		}
	}
	if checked < 90 {
		t.Fatalf("checked %d restricted BUILD.bazel files for package(default_visibility), want >= 90", checked)
	}
}

func TestRestrictedTargetsSatisfyAllRepoReverseDeps(t *testing.T) {
	t.Parallel()
	files := collectAllBuildFiles(t, visibilityRepoRoot(t))
	if len(files) < 200 {
		t.Fatalf("found %d BUILD.bazel files, want >= 200", len(files))
	}

	pkgGroups := map[string][]string{}
	type targetEntry struct {
		pkg string
		vis []string
	}
	targets := map[string]targetEntry{}

	for _, f := range files {
		for k, v := range f.packageGroups {
			pkgGroups[k] = v
		}
		if !inRestrictedVisibilityScope(f.rel) {
			continue
		}
		pkg := "//" + strings.TrimSuffix(f.rel, "/BUILD.bazel")
		defaultVis := []string{"//visibility:private"}
		if f.hasPkgDefaultVisibility {
			defaultVis = f.pkgDefaultVisibility
		}
		baseName := pkg[strings.LastIndex(pkg, "/")+1:]
		for _, r := range f.rules {
			if r.name == "" {
				continue
			}
			vis := defaultVis
			if r.visibility != nil {
				vis = r.visibility
			}
			targets[pkg+":"+r.name] = targetEntry{pkg: pkg, vis: vis}
			if r.name == baseName {
				targets[pkg] = targetEntry{pkg: pkg, vis: vis}
			}
		}
	}

	if _, ok := pkgGroups["//internal:unicode15_consumers"]; !ok {
		t.Fatal("missing //internal:unicode15_consumers package_group")
	}

	checkedRefs := 0
	for _, f := range files {
		callerPkg := "//"
		if f.rel != "BUILD.bazel" {
			callerPkg = "//" + strings.TrimSuffix(f.rel, "/BUILD.bazel")
		}
		for _, r := range f.rules {
			for _, ref := range r.refs {
				target, ok := targets[ref]
				if !ok {
					continue
				}
				checkedRefs++
				if !visibilityAllows(target.vis, callerPkg, target.pkg, pkgGroups) {
					t.Errorf("%s (%s) target %q references %s, which has incompatible visibility %v",
						f.rel, callerPkg, r.name, ref, target.vis)
				}
			}
		}
	}
	if checkedRefs < 250 {
		t.Fatalf("checked %d restricted reverse-dep references, want >= 250", checkedRefs)
	}
}

func TestCorePublicPackagesRemainPublic(t *testing.T) {
	t.Parallel()
	root := visibilityRepoRoot(t)
	corePublic := []struct {
		rel  string
		name string
	}{
		{"pkg/fdbgo/fdb/BUILD.bazel", "fdb"},
		{"pkg/fdbgo/fdb/tuple/BUILD.bazel", "tuple"},
		{"pkg/fdbgo/fdb/subspace/BUILD.bazel", "subspace"},
		{"pkg/fdbgo/fdb/directory/BUILD.bazel", "directory"},
		{"pkg/fdbgo/fdbmetrics/BUILD.bazel", "fdbmetrics"},
		{"pkg/recordlayer/BUILD.bazel", "recordlayer"},
		{"pkg/recordlayer/keyspace/BUILD.bazel", "keyspace"},
		{"pkg/recordlayer/vectorindex/BUILD.bazel", "vectorindex"},
		{"pkg/recordlayer/rlmetrics/BUILD.bazel", "rlmetrics"},
		{"pkg/relational/sqldriver/BUILD.bazel", "sqldriver"},
		{"pkg/testcontainers/foundationdb/BUILD.bazel", "foundationdb"},
	}

	for _, tc := range corePublic {
		data, err := os.ReadFile(filepath.Join(root, tc.rel))
		if err != nil {
			t.Fatalf("read %s: %v", tc.rel, err)
		}
		parsed := parseBuildFile(tc.rel, string(data))
		found := false
		for _, r := range parsed.rules {
			if r.kind == "go_library" && r.name == tc.name {
				found = true
				if !slices.Contains(r.visibility, "//visibility:public") {
					t.Errorf("%s: go_library %q visibility = %v, want //visibility:public", tc.rel, tc.name, r.visibility)
				}
			}
		}
		if !found {
			t.Errorf("%s: missing go_library %q", tc.rel, tc.name)
		}
	}
}

func TestVisibilityParserControls(t *testing.T) {
	t.Parallel()

	p := parseBuildFile("pkg/testutil/ex/BUILD.bazel", `
# //visibility:public in a comment must be ignored
# gazelle:default_visibility //pkg/recordlayer:__subpackages__,//pkg/relational:__subpackages__

package(default_visibility = [
    "//pkg/recordlayer:__subpackages__",
    "//pkg/relational:__subpackages__",
])

go_library(
    name = "ex",
    visibility = ["//visibility:public"],
    deps = ["//pkg/fdbgo/fdb"],
)
`)
	if len(p.publicVisibilityLines) != 1 || p.publicVisibilityLines[0] != 12 {
		t.Fatalf("publicVisibilityLines = %v, want [12]", p.publicVisibilityLines)
	}
	if !slices.Equal(p.gazelleDefaultVisibility, []string{"//pkg/recordlayer:__subpackages__", "//pkg/relational:__subpackages__"}) {
		t.Fatalf("gazelleDefaultVisibility = %v", p.gazelleDefaultVisibility)
	}
	if !p.hasPkgDefaultVisibility || !slices.Equal(p.pkgDefaultVisibility, p.gazelleDefaultVisibility) {
		t.Fatalf("pkgDefaultVisibility = %v (present=%v)", p.pkgDefaultVisibility, p.hasPkgDefaultVisibility)
	}
	if len(p.rules) != 1 || !slices.Equal(p.rules[0].refs, []string{"//pkg/fdbgo/fdb"}) {
		t.Fatalf("rules = %+v", p.rules)
	}

	groups := map[string][]string{
		"//internal:consumers": {"//pkg/recordlayer/...", "//conformance"},
	}
	for _, tc := range []struct {
		vis    []string
		caller string
		target string
		want   bool
	}{
		{[]string{"//visibility:private"}, "//pkg/a", "//pkg/a", true},
		{[]string{"//visibility:private"}, "//pkg/b", "//pkg/a", false},
		{[]string{"//cmd/dst-hunt:__pkg__"}, "//cmd/dst-hunt", "//pkg/simfdb/hunt", true},
		{[]string{"//cmd/dst-hunt:__pkg__"}, "//cmd/dst-hunt/sub", "//pkg/simfdb/hunt", false},
		{[]string{"//pkg/fdbgo:__subpackages__"}, "//pkg/fdbgo/client", "//pkg/fdbgo/internal/diag", true},
		{[]string{"//pkg/fdbgo:__subpackages__"}, "//pkg/recordlayer", "//pkg/fdbgo/internal/diag", false},
		{[]string{"//internal:consumers"}, "//pkg/recordlayer/vectorindex", "//internal/unicode15", true},
		{[]string{"//internal:consumers"}, "//conformance", "//internal/unicode15", true},
		{[]string{"//internal:consumers"}, "//conformance/sub", "//internal/unicode15", false},
		{[]string{"//internal:consumers"}, "//pkg/fdbgo/client", "//internal/unicode15", false},
	} {
		if got := visibilityAllows(tc.vis, tc.caller, tc.target, groups); got != tc.want {
			t.Errorf("visibilityAllows(%v, %q, %q) = %v, want %v", tc.vis, tc.caller, tc.target, got, tc.want)
		}
	}
}
