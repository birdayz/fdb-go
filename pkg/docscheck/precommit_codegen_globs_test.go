package docscheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The pre-commit hook's codegen-owned glob list must cover every path the
// `generate` recipe chain actually writes.
//
// The hook scopes its untracked-drift attribution to those globs, because all it
// can observe is that a file APPEARED while `just generate` was running — a
// multi-minute window, in a worktree several agents share. Scoping fixes the
// misattribution, and it fails OPEN: a codegen output landing outside the globs
// would stop being caught, silently, and an uncommitted generated file would
// sail through the commit.
//
// So the list is pinned against the recipes it was read off. This gate re-derives
// the write sites from the justfile — the `rm -rf` targets, the `find … -delete`
// pattern, the gazelle runs, the buf download — follows any `bazelisk run` into
// the script it executes, and requires the hook's own bash matcher to claim each
// one. Adding a codegen output the globs do not cover is then a build break with
// a message saying what to add and where, not silence.
//
// The matcher is the hook's, extracted and run under bash, not a Go
// reimplementation of shell `case` semantics: a second implementation could
// agree with the first about these samples and disagree about the file that
// matters.
func TestCodegenOwnedGlobsCoverTheGenerateRecipes(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	justfile := readDoc(t, root, "justfile")
	recipes := parseJustRecipes(t, justfile)

	chain := generateRecipeChain(t, recipes)
	sites := deriveCodegenWriteSites(t, root, recipes, chain)

	hook := extractPreCommitHook(t)
	globs := extractCodegenOwnedGlobs(t, hook)

	// Vacuity guards. Each of the three populations below can collapse to empty
	// and leave every assertion in this test trivially satisfied, which is the
	// dominant way a gate like this dies without anyone noticing.
	if len(chain) < 4 {
		t.Fatalf("only %d recipe(s) reachable from `generate` (%v) — the chain parse has "+
			"broken, so this gate is deriving write sites from almost nothing and would "+
			"pass over an uncovered codegen output", len(chain), chain)
	}
	if len(sites) < 5 {
		t.Fatalf("only %d codegen write site(s) derived from the justfile — the extractors "+
			"have stopped matching the recipe text, so coverage below is vacuous. Sites: %v",
			len(sites), sites)
	}
	if len(globs) < 5 {
		t.Fatalf("the hook's codegen_owned_globs list has %d entr(ies) — a list this short "+
			"cannot cover the %d write sites the recipes have, so either the list was "+
			"emptied or its extraction broke", len(globs), len(sites))
	}

	// Positive control for the matcher itself. A `path_is_codegen_owned` that
	// answered "owned" unconditionally — a stray `return 0`, a pattern that
	// degenerated to `*` — would satisfy every coverage check below while making
	// the hook's scoping do nothing at all.
	const control = "scratch-agent.log"
	samples := []string{control}
	for _, s := range sites {
		samples = append(samples, s.samples...)
	}
	owned := classifyWithHookMatcher(t, hook, samples)
	if owned[control] {
		t.Fatalf("the hook's matcher claims %q as codegen-owned. It is a scratch file in no "+
			"codegen path; a matcher that owns it owns everything, and the scoping that "+
			"keeps unrelated files from being reported as codegen drift is inert.", control)
	}

	for _, s := range sites {
		for _, p := range s.samples {
			if owned[p] {
				continue
			}
			t.Errorf("`just generate` writes %s (%s), and the pre-commit hook does not count\n"+
				"  %q as codegen-owned. The hook would report a file created there as an\n"+
				"  unattributable NOTE instead of failing the commit, so an uncommitted\n"+
				"  generated file would ship.\n"+
				"  Fix: add a pattern covering it to `codegen_owned_globs` in the\n"+
				"  `install-hooks` recipe in justfile, then run `just install-hooks`.",
				s.path, s.origin, p)
		}
	}

	t.Logf("checked %d codegen write site(s) (%d sample path(s)) from %d recipe(s) reachable "+
		"from `generate` (%v), against %d glob(s) in the hook's codegen_owned_globs.\nsites: %v",
		len(sites), len(samples)-1, len(chain), chain, len(globs), sites)
}

// writeSite is one place a recipe in the `generate` chain writes, plus the
// representative repo-relative paths the hook must claim for it.
type writeSite struct {
	path    string // the recipe's own spelling, for the failure message
	origin  string // which recipe/script line it came from
	samples []string
}

func (w writeSite) String() string { return w.path + " (" + w.origin + ")" }

var (
	justRecipeHeader = regexp.MustCompile(`^([a-z][a-zA-Z0-9_-]*)((?:\s+[^:]*?)?):\s*(.*)$`)
	shellAssignment  = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)="?([^"\s]*)"?\s*$`)
	rmRecursive      = regexp.MustCompile(`\brm\s+-rf\s+"?([^"\s]+)"?`)
	rmFile           = regexp.MustCompile(`\brm\s+-f\s+"?([^"\s]+)"?`)
	findDelete       = regexp.MustCompile(`\bfind\s+(\S+)\s+-name\s+'([^']+)'\s+-delete\b`)
	curlOutput       = regexp.MustCompile(`\bcurl\b[^\n]*\s-o\s+"?([^"\s]+)"?`)
	bazelRun         = regexp.MustCompile(`\bbazelisk\s+run\s+//([A-Za-z0-9_./-]*):([A-Za-z0-9_.-]+)`)
)

type justRecipe struct {
	deps []string
	body []string
}

// parseJustRecipes splits the justfile into recipes: a name at column 0, its
// dependency list, and the indented lines that follow.
func parseJustRecipes(t *testing.T, justfile string) map[string]justRecipe {
	t.Helper()
	out := map[string]justRecipe{}
	lines := strings.Split(justfile, "\n")
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if ln == "" || ln[0] == ' ' || ln[0] == '\t' || ln[0] == '#' || strings.Contains(ln, ":=") {
			continue
		}
		m := justRecipeHeader.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		r := justRecipe{deps: strings.Fields(m[3])}
		for j := i + 1; j < len(lines); j++ {
			nxt := lines[j]
			if nxt == "" {
				continue
			}
			if nxt[0] != ' ' && nxt[0] != '\t' {
				break
			}
			r.body = append(r.body, nxt)
		}
		out[m[1]] = r
	}
	if len(out) == 0 {
		t.Fatal("no recipes parsed out of the justfile — the recipe grammar this gate " +
			"assumes has changed and it is now deriving nothing")
	}
	return out
}

// generateRecipeChain returns `generate` and everything it depends on,
// transitively, sorted for a stable report.
func generateRecipeChain(t *testing.T, recipes map[string]justRecipe) []string {
	t.Helper()
	if _, ok := recipes["generate"]; !ok {
		t.Fatal("the justfile has no `generate` recipe; the pre-commit hook runs " +
			"`just generate`, so either the recipe was renamed (and the hook is broken) " +
			"or this gate's parse is")
	}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		r, ok := recipes[name]
		if !ok {
			return
		}
		seen[name] = true
		for _, d := range r.deps {
			walk(d)
		}
	}
	walk("generate")
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// deriveCodegenWriteSites reads the write sites out of the recipe bodies in the
// chain, following `bazelisk run` into the shell script the target executes.
func deriveCodegenWriteSites(t *testing.T, root string, recipes map[string]justRecipe, chain []string) []writeSite {
	t.Helper()
	var sites []writeSite
	seen := map[string]bool{}
	add := func(s writeSite) {
		key := s.path + "|" + s.origin
		if seen[key] {
			return
		}
		seen[key] = true
		sites = append(sites, s)
	}
	for _, name := range chain {
		for _, s := range scanShellBody(t, root, recipes[name].body, "justfile recipe `"+name+"`") {
			add(s)
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].path < sites[j].path })
	return sites
}

// scanShellBody extracts write sites from shell text. Variables assigned in the
// same text are resolved, and $BUILD_WORKSPACE_DIRECTORY — how a bazel-run script
// addresses the source tree — is stripped to leave a repo-relative path.
func scanShellBody(t *testing.T, root string, lines []string, origin string) []writeSite {
	t.Helper()
	vars := map[string]string{}
	for _, ln := range lines {
		if m := shellAssignment.FindStringSubmatch(ln); m != nil {
			vars[m[1]] = m[2]
		}
	}
	resolve := func(s string) string {
		for name, val := range vars {
			s = strings.ReplaceAll(s, "${"+name+"}", val)
			s = strings.ReplaceAll(s, "$"+name, val)
		}
		s = strings.ReplaceAll(s, "${BUILD_WORKSPACE_DIRECTORY}/", "")
		s = strings.ReplaceAll(s, "$BUILD_WORKSPACE_DIRECTORY/", "")
		return strings.Trim(s, "/")
	}

	var out []writeSite
	for _, raw := range lines {
		ln := strings.TrimSpace(raw)
		// Comments and console output quote commands without running them; a
		// `just generate-parser` message telling the reader to run gazelle is not
		// itself a write site.
		if strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "echo ") {
			continue
		}
		if m := rmRecursive.FindStringSubmatch(ln); m != nil {
			p := resolve(m[1])
			if repoRelative(p) {
				out = append(out, writeSite{path: p, origin: origin + " (rm -rf)", samples: dirSamples(p)})
			}
		}
		if m := rmFile.FindStringSubmatch(ln); m != nil {
			p := resolve(m[1])
			if repoRelative(p) {
				out = append(out, writeSite{path: p, origin: origin + " (rm -f)", samples: []string{deglob(p)}})
			}
		}
		if m := findDelete.FindStringSubmatch(ln); m != nil {
			dir, pat := resolve(m[1]), m[2]
			if repoRelative(dir) {
				out = append(out, writeSite{
					path:   dir + "/**/" + pat,
					origin: origin + " (find -delete)",
					// `find` is recursive, so both depths must be claimed.
					samples: []string{dir + "/" + deglob(pat), dir + "/nested/" + deglob(pat)},
				})
			}
		}
		if m := curlOutput.FindStringSubmatch(ln); m != nil {
			p := resolve(m[1])
			if repoRelative(p) {
				out = append(out, writeSite{path: p, origin: origin + " (curl -o)", samples: []string{deglob(p)}})
			}
		}
		for _, m := range bazelRun.FindAllStringSubmatch(ln, -1) {
			pkg, target := m[1], m[2]
			if pkg == "" && target == "gazelle" {
				out = append(out, writeSite{
					path:   "BUILD.bazel (any package)",
					origin: origin + " (bazelisk run //:gazelle)",
					// gazelle writes a BUILD.bazel at the repo root and at any depth.
					samples: []string{"BUILD.bazel", "pkg/newthing/BUILD.bazel", "pkg/a/b/c/BUILD.bazel"},
				})
				continue
			}
			// Any other bazel-run step in the codegen chain executes a script that
			// writes into the source tree. Convention here is <pkg>/<target>.sh; a
			// target that breaks it must be taught to this gate rather than
			// silently contributing no write sites.
			script := filepath.Join(root, filepath.FromSlash(pkg), target+".sh")
			body, err := os.ReadFile(script)
			if err != nil {
				t.Fatalf("recipe step `bazelisk run //%s:%s` runs a codegen target whose script\n"+
					"  could not be read at %s (%v).\n"+
					"  This gate follows bazel-run codegen steps into their script to find what\n"+
					"  they write; one it cannot read contributes NO write sites, which is a\n"+
					"  silently weaker gate. Declare the script as a `data` dep of\n"+
					"  //pkg/docscheck:docscheck_test, or teach this gate the target.",
					pkg, target, script, err)
			}
			nested := scanShellBody(t, root, strings.Split(string(body), "\n"),
				fmt.Sprintf("%s → //%s:%s (%s.sh)", origin, pkg, target, target))
			out = append(out, nested...)
		}
	}
	return out
}

// repoRelative rejects operands that do not name a path inside the checkout —
// temp dirs, absolute paths, and unresolved variables.
func repoRelative(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "$") {
		return false
	}
	return !strings.HasPrefix(p, "..")
}

func deglob(p string) string { return strings.ReplaceAll(p, "*", "probe") }

// dirSamples returns paths a directory-wide write could plausibly produce. Both
// depths matter: a glob of `gen/*` claims either, but one of `gen/*.go` would
// claim neither, and that difference is exactly what fails open.
func dirSamples(p string) []string {
	if strings.Contains(p, "*") {
		return []string{deglob(p)}
	}
	return []string{p + "/probe.pb.go", p + "/nested/probe.pb.go"}
}

// extractCodegenOwnedGlobs reads the glob list out of the hook body.
func extractCodegenOwnedGlobs(t *testing.T, hook string) []string {
	t.Helper()
	const marker = "codegen_owned_globs='"
	i := strings.Index(hook, marker)
	if i < 0 {
		t.Fatalf("the hook has no %s assignment — the untracked-drift attribution is no "+
			"longer scoped to the paths codegen writes, and this gate has nothing to pin", marker)
	}
	rest := hook[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatal("the hook's codegen_owned_globs single-quoted list is never closed")
	}
	var globs []string
	for _, ln := range strings.Split(rest[:j], "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			globs = append(globs, ln)
		}
	}
	return globs
}

// classifyWithHookMatcher runs the hook's OWN `path_is_codegen_owned` under bash
// over the sample paths and returns which it claims. Reusing the shipped bash
// avoids a second matcher that could agree here and disagree in the hook.
func classifyWithHookMatcher(t *testing.T, hook string, paths []string) map[string]bool {
	t.Helper()

	const fnMarker = "path_is_codegen_owned() {"
	fi := strings.Index(hook, fnMarker)
	if fi < 0 {
		t.Fatalf("the hook has no %s function; nothing in it decides whether a path is "+
			"codegen-owned", fnMarker)
	}
	fn := hook[fi:]
	if end := strings.Index(fn, "\n}\n"); end >= 0 {
		fn = fn[:end+3]
	} else {
		t.Fatal("the hook's path_is_codegen_owned function is never closed at column 0")
	}

	gi := strings.Index(hook, "codegen_owned_globs='")
	ge := strings.Index(hook[gi:], "'\n")
	if gi < 0 || ge < 0 {
		t.Fatal("cannot slice codegen_owned_globs out of the hook")
	}
	assignment := hook[gi : gi+ge+1]

	dir := t.TempDir()
	script := filepath.Join(dir, "match.sh")
	writeFile(t, script, "#!/usr/bin/env bash\nset -euo pipefail\n"+assignment+"\n"+fn+
		"\nfor p in \"$@\"; do\n  if path_is_codegen_owned \"$p\"; then echo \"OWNED $p\"; "+
		"else echo \"FOREIGN $p\"; fi\ndone\n")

	raw, err := exec.Command("bash", append([]string{script}, paths...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("running the hook's matcher failed: %v\n%s", err, raw)
	}
	out := map[string]bool{}
	var seen int
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.HasPrefix(ln, "OWNED "):
			out[strings.TrimPrefix(ln, "OWNED ")] = true
			seen++
		case strings.HasPrefix(ln, "FOREIGN "):
			out[strings.TrimPrefix(ln, "FOREIGN ")] = false
			seen++
		default:
			t.Fatalf("unexpected line from the hook matcher: %q\nfull output:\n%s", ln, raw)
		}
	}
	if seen != len(paths) {
		t.Fatalf("the hook matcher classified %d of %d sample paths — a verdict was lost, "+
			"so an unclassified path would read as not-owned and could not be told from a "+
			"real gap.\noutput:\n%s", seen, len(paths), raw)
	}
	return out
}
