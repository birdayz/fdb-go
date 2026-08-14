package docscheck

import (
	"go/build/constraint"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Build-membership gate: every deliverable *_test.go file must either be in the srcs
// of a Bazel test target, or be excused by a lane that demonstrably runs it.
//
// A test file that exists on disk and is in no BUILD srcs is SILENT FALSE
// COVERAGE — the worst failure mode this repo has. `go test ./...` compiles and
// runs it locally, so it emits real passing output that gets quoted as proof,
// while Bazel never compiles the file at all and reports the package green. Both
// signals say "green" and one of them is a lie. This happened three separate
// times on one branch: a paging reproducer and a single-read-version test each
// ran only on a developer's machine and never once in CI, and a build-tag-gated
// const pair landed in no target, leaving the const undefined under Bazel.
//
// The check has to come from the FILESYSTEM side. A file Bazel was never told
// about is invisible to every gate that reads Bazel's view of the world — that
// is the bug, definitionally — so the only way to catch it is to enumerate what
// is on disk and ask whether each file is known.
//
// # Why parse BUILD files instead of `bazelisk query`
//
// `bazelisk query 'labels(srcs, kind("go_test rule", //...))'` is the more robust
// oracle: it is Bazel's own answer, immune to any Starlark a hand-rolled parser
// does not understand. It is not available from here. These gates run UNDER
// Bazel, and a nested bazelisk inside a test action contends for the output base
// the outer build already holds, needs network for toolchain resolution inside a
// sandbox that has none, and would make the action depend on the whole repo.
// Every source-tree gate in this package takes the other route —
// TestSourceCommentHygiene walks and parses the real checkout rather than asking
// a build tool — and the BUILD parser they share (parseBuildGraph, in
// container_memory_tag_test.go) already exists and is already trusted to compute
// an rdeps closure. This gate reuses it rather than standing up a second,
// divergent one.
//
// What that parser can and cannot read is ASSERTED, never assumed: it aborts on
// select(), TestGoRuleSrcsAreLiteralLists aborts on a computed srcs in any go_*
// rule, and the anti-vacuity floors abort if the scan stops seeing the real
// tree. Every way this gate could quietly under-report is a loud failure.

// testSrcRuleKinds are the rule kinds whose srcs cause a test file to be
// compiled AND RUN. Nothing else counts: a *_test.go in a go_library's srcs is
// compiled but never executed as a test, which is the same false coverage in a
// different costume, so those are reported separately rather than credited.
var testSrcRuleKinds = map[string]bool{
	"go_test": true,
}

// attrCalls returns the function names invoked inside `attr = …` of a rule body
// (glob, select, …), read over the attribute's balanced region so a call in a
// LATER attribute cannot be misattributed to this one. Comments are already
// stripped by stripBuildComments before a body reaches here.
//
// It exists so a computed srcs is REPORTED rather than read as an empty list:
// listAttr on `srcs = glob([...])` returns nothing, and nothing is
// indistinguishable from a rule with no srcs — which would make every globbed
// file look like an orphan and invite weakening the gate instead of fixing it.
func attrCalls(body, attr string) []string {
	loc := regexp.MustCompile(`(?m)^\s*` + attr + `\s*=\s*`).FindStringIndex(body)
	if loc == nil {
		return nil
	}
	rest := body[loc[1]:]
	depth := 0
	end := len(rest)
scan:
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '"':
			if q := strings.IndexByte(rest[i+1:], '"'); q >= 0 {
				i += q + 1
			}
		case '[', '(', '{':
			depth++
		case ']', ')', '}':
			depth--
			if depth < 0 {
				end = i
				break scan
			}
		case '\n':
			// Inside a rule body an attribute ends at its depth-0 comma; the
			// newline after it is the boundary to the next attribute.
			if depth == 0 && i > 0 && rest[i-1] == ',' {
				end = i
				break scan
			}
		}
	}
	var out []string
	for _, m := range regexp.MustCompile(`\b([a-z_][a-z0-9_]*)\(`).FindAllStringSubmatch(rest[:end], -1) {
		out = append(out, m[1])
	}
	return out
}

// outOfBazelLane is a sanctioned reason a test file may be absent from every
// go_test, together with the lane that actually runs it.
//
// Absence is allowed for exactly two reasons, and both must be DECLARED here
// rather than inferred. Silence is what this gate exists to eliminate: an
// unlisted absence is indistinguishable from the incidents above, and a build
// tag nobody declared — say `//go:build bazelrunfile`, one character short — is
// the same bug wearing a costume, because gazelle drops the file and `go test`
// without that tag drops it too, so it runs literally nowhere while looking
// perfectly normal in review.
//
// Each entry names the file that runs the lane, and the gate reads that file and
// requires it to mention the tag or module. An exemption whose lane was deleted
// or renamed goes RED instead of quietly becoming a permanent hole.
type outOfBazelLane struct {
	// buildTag excuses files whose //go:build constraint cannot be satisfied
	// because this tag is not among gazelle's declared build_tags.
	buildTag string
	// modulePath excuses every test file under a separate Go module that is
	// deliberately outside the Bazel workspace.
	modulePath string
	// laneFile is the CI workflow or justfile recipe that runs these tests, and
	// laneMentions is a string it must contain for the lane to count as real.
	laneFile     string
	laneMentions string
	why          string
}

var outOfBazelLanes = []outOfBazelLane{
	{
		buildTag:     "libfdbc",
		laneFile:     ".github/workflows/nightly-libfdbc.yml",
		laneMentions: "test -tags libfdbc",
		why: "the cgo backend links libfdb_c, which the Bazel build does not provide; " +
			"the nightly builds it and runs the cross-client differential",
	},
	{
		buildTag:     "yamsql",
		laneFile:     "justfile",
		laneMentions: "go test -tags=yamsql",
		why:          "the corpus lives in the gitignored Java checkout, which Bazel's sandbox cannot see",
	},
	{
		buildTag:     "realcluster",
		laneFile:     ".github/workflows/nightly-stress.yml",
		laneMentions: "realcluster",
		why:          "the 10M scales need a real cluster, not a testcontainer; the nightly type-checks them",
	},
	{
		modulePath:   "tools/bazelscaleset",
		laneFile:     ".github/workflows/ci.yml",
		laneMentions: "tools/bazelscaleset",
		why: "a standalone module for the runner supervisor, kept out of the root go.mod so its " +
			"GitHub-API deps never enter the record-layer dependency graph; CI runs its tests directly",
	},
}

// freeBuildTags are the tags gazelle does NOT resolve from the build_tags
// directive — the toolchain decides them per configuration, so a file gated on
// one is still emitted into srcs and rules_go applies the constraint at compile
// time. `race` is the load-bearing case: the race lanes under
// pkg/relational/sqldriver and pkg/recordlayer carry `//go:build race` and are
// listed unconditionally.
//
// Treating these as FREE (satisfiable either way) rather than true or false is
// what makes `cgo && libfdbc` come out unsatisfiable for the right reason —
// libfdbc is the undeclared tag, not cgo.
var freeBuildTags = map[string]bool{
	"cgo": true, "race": true, "msan": true, "asan": true,
	"gc": true, "gccgo": true, "unix": true,
	"linux": true, "darwin": true, "windows": true,
	"amd64": true, "arm64": true,
}

var (
	buildTagsDirective = regexp.MustCompile(`(?m)^#\s*gazelle:build_tags\s+(\S+)\s*$`)
	constraintIdent    = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)
	goVersionTag       = regexp.MustCompile(`^go1\.[0-9]+$`)
)

// declaredBuildTags reads `# gazelle:build_tags a,b,c` from the root BUILD.bazel.
// Reading the directive rather than hardcoding the list is what keeps this gate
// correct for free: adding a tag there makes gazelle start emitting those files,
// and the gate immediately starts REQUIRING them.
func declaredBuildTags(t *testing.T, root string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "BUILD.bazel"))
	if err != nil {
		t.Fatalf("read root BUILD.bazel: %v", err)
	}
	m := buildTagsDirective.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `# gazelle:build_tags` directive in the root BUILD.bazel. Without it this gate cannot tell a " +
			"deliberately-excluded file from a lost one; if the directive genuinely went away, every custom-tagged " +
			"file is now excluded from Bazel and that needs deciding, not defaulting.")
	}
	tags := map[string]bool{}
	for _, tag := range strings.Split(string(m[1]), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags[tag] = true
		}
	}
	return tags
}

// buildConstraint returns the //go:build expression of a Go file, or nil when it
// has none. Only the header before the package clause is considered, so a
// constraint quoted in ordinary prose lower down cannot be mistaken for one.
func buildConstraint(src []byte) constraint.Expr {
	head := string(src)
	if idx := strings.Index(head, "\npackage "); idx > 0 {
		head = head[:idx]
	}
	for _, line := range strings.Split(head, "\n") {
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return nil
		}
		return expr
	}
	return nil
}

// constraintTags lists the identifiers a constraint expression tests.
func constraintTags(expr constraint.Expr) []string {
	seen := map[string]bool{}
	var out []string
	for _, ident := range constraintIdent.FindAllString(expr.String(), -1) {
		if !seen[ident] {
			seen[ident] = true
			out = append(out, ident)
		}
	}
	sort.Strings(out)
	return out
}

// satisfiable reports whether a constraint can hold under gazelle's view of the
// world, and returns the tags that forced it to false.
//
// Declared tags are TRUE (gazelle was told to set them), free tags are enumerated
// both ways (the toolchain picks per configuration), and everything else is
// FALSE — which is exactly what gazelle does with a tag it was never told about.
// A file whose constraint cannot hold is one gazelle deliberately omits from
// srcs, so requiring it there would be demanding something wrong.
func satisfiable(expr constraint.Expr, declared map[string]bool) (bool, []string) {
	tags := constraintTags(expr)
	var free, undeclared []string
	for _, tag := range tags {
		switch {
		case declared[tag]:
		case freeBuildTags[tag] || goVersionTag.MatchString(tag):
			free = append(free, tag)
		default:
			undeclared = append(undeclared, tag)
		}
	}
	if len(free) > 16 {
		// Not a real shape here; enumerating 2^17 assignments would be a silent
		// hang, and a silent anything is what this file exists to prevent.
		return true, nil
	}
	for combo := 0; combo < 1<<len(free); combo++ {
		on := map[string]bool{}
		for i, tag := range free {
			if combo&(1<<i) != 0 {
				on[tag] = true
			}
		}
		if expr.Eval(func(tag string) bool { return declared[tag] || on[tag] }) {
			return true, nil
		}
	}
	return false, undeclared
}

// trackedFiles enumerates the local deliverable matching a git pathspec:
// tracked files plus untracked, non-ignored files, with tracked-but-deleted
// paths removed. That is the same scope rule TestSourceCommentHygiene uses,
// with the same fallback to a filesystem walk when git is unavailable.
//
// The deliverable set is what makes the gitignored trees drop out for free:
// fdb-record-layer/ (the Java checkout), bazel-* (convenience symlinks) and
// .claude/worktrees/ (other agents' checkouts) are ignored, so no path list
// has to enumerate them and a newly-gitignored tree needs no maintenance here.
//
// The fallback walk cannot inherit that for free — it has no tracked set to
// consult — so it names its exclusions (fallbackWalkSkippedTrees) instead. That
// list is closed, which is what makes the fallback a SUPERSET of the deliverable set
// and therefore able to make the gate only stricter, never quieter: an ignored
// tree nobody listed gets walked and its files reported. The rule it replaced —
// skip anything whose name starts with a dot — got that backwards and dropped
// the 31 tracked paths under .claude/skills and .github.
func trackedFiles(t *testing.T, root, pattern string) []string {
	t.Helper()
	files, err := gitDeliverableFiles(root, pattern)
	if err == nil && len(files) > 0 {
		return files
	}
	t.Logf("git deliverable-file enumeration unavailable (%v) — falling back to a filesystem walk over everything but the "+
		"named excluded trees (%v), a superset of tracked plus untracked non-ignored files", err, sortedFallbackWalkSkips())
	base := filepath.Base(pattern)
	files, walkErr := fallbackWalk(root, func(name string) bool {
		ok, _ := filepath.Match(base, name)
		return ok
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}
	return files
}

// trackedTestFiles is the scan set: tracked *_test.go files, minus fixture trees.
//
// A *_test.go under a testdata/ directory is INPUT to a test, not a test — Bazel
// ignores testdata by convention and gazelle never lists it in srcs. The
// exclusion is on a path COMPONENT, never a substring: conformance/testdata_test.go
// is a real test whose name merely begins with the word, and a substring match
// would silently exempt exactly one file from the gate, forever.
func trackedTestFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, rel := range trackedFiles(t, root, "*_test.go") {
		if hasPathComponent(rel, "testdata") {
			continue
		}
		out = append(out, rel)
	}
	return out
}

func hasPathComponent(rel, component string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == component {
			return true
		}
	}
	return false
}

// srcPaths maps a rule's srcs entries to repo-relative paths. Entries are
// package-relative names in gazelle output; the `//pkg/x:f.go` and `:f.go` label
// forms are resolved to the file they name, so a cross-package src is credited
// to the file rather than to the referencing package.
func srcPaths(r buildRule) []string {
	var out []string
	for _, entry := range r.srcs {
		switch {
		case strings.HasPrefix(entry, "@"):
			continue // external repository — not a file of this checkout
		case strings.HasPrefix(entry, "//"):
			body := entry[2:]
			if colon := strings.LastIndex(body, ":"); colon >= 0 {
				out = append(out, joinPkg(body[:colon], body[colon+1:]))
				continue
			}
			out = append(out, body)
		case strings.HasPrefix(entry, ":"):
			out = append(out, joinPkg(r.pkg, entry[1:]))
		default:
			out = append(out, joinPkg(r.pkg, entry))
		}
	}
	return out
}

func joinPkg(pkg, name string) string {
	if pkg == "" || pkg == "." {
		return name
	}
	return pkg + "/" + name
}

// testTargetSrcs maps every file compiled by a go_test to the targets that do so.
func testTargetSrcs(t *testing.T, root string) (inTest, inOther map[string][]string, rules int) {
	t.Helper()
	inTest, inOther = map[string][]string{}, map[string][]string{}
	graph := parseBuildGraph(t, root)
	for _, r := range graph {
		for _, src := range srcPaths(r) {
			label := "//" + r.label + " (" + r.kind + ")"
			if testSrcRuleKinds[r.kind] {
				inTest[src] = append(inTest[src], label)
			} else {
				inOther[src] = append(inOther[src], label)
			}
		}
	}
	return inTest, inOther, len(graph)
}

// TestEveryTestFileIsInABuildTarget is the gate.
func TestEveryTestFileIsInABuildTarget(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	declared := declaredBuildTags(t, root)
	inTest, inOther, ruleCount := testTargetSrcs(t, root)

	testFiles := trackedTestFiles(t, root)
	var orphans []string
	var excused int
	var sawSelf bool
	for _, rel := range testFiles {
		if rel == "pkg/docscheck/build_membership_test.go" && len(inTest[rel]) > 0 {
			sawSelf = true
		}
		if len(inTest[rel]) > 0 {
			continue
		}
		if lane, ok := excusedBy(t, root, rel, declared); ok {
			excused++
			_ = lane
			continue
		}
		detail := ""
		if others := inOther[rel]; len(others) > 0 {
			sort.Strings(others)
			detail = " (listed only in non-test rule(s) " + strings.Join(others, ", ") +
				" — compiled, but never RUN as a test)"
		}
		orphans = append(orphans, rel+detail)
	}
	sort.Strings(orphans)

	// Anti-vacuity: a gate whose scan came up empty is worse than no gate,
	// because the green result reads as coverage. Both sides of the comparison
	// must be non-trivially populated, and this file itself must be found in a
	// go_test — a membership gate cannot credibly demand membership it does not
	// have, and unregistered it would not run under Bazel at all.
	if len(testFiles) < 1500 {
		t.Fatalf("scan saw %d test files under %s — that is not the real source tree; fix sourceTreeRoot/runfiles staging", len(testFiles), root)
	}
	if len(inTest) < 1500 {
		t.Fatalf("scan collected go_test srcs for only %d files across %d rules — the BUILD parse is not seeing the real rules", len(inTest), ruleCount)
	}
	if !sawSelf {
		t.Errorf("this gate's own file (pkg/docscheck/build_membership_test.go) is in no go_test srcs — run `just gazelle`. " +
			"An unregistered membership gate proves nothing: under Bazel it never runs.")
	}
	t.Logf("%d test files scanned, %d excused by a declared out-of-Bazel lane", len(testFiles), excused)

	for _, o := range orphans {
		t.Errorf("test file in NO Bazel test target: %s", o)
	}
	if len(orphans) > 0 {
		t.Errorf("%d *_test.go file(s) exist on disk but are in no go_test srcs — run `just gazelle` and commit the BUILD change. "+
			"Why this is a hard failure and not a nit: `go test ./...` compiles and runs these files locally, so they emit real passing "+
			"output that gets quoted as proof, while Bazel never compiles them and reports the package green. Both signals say green and "+
			"one of them is a lie — the test is SILENT FALSE COVERAGE. Three such files shipped on one branch (a paging reproducer, a "+
			"single-read-version test, and a build-tag-gated const pair that left the const undefined under Bazel) before this gate existed. "+
			"If a file genuinely belongs outside Bazel, it needs an entry in outOfBazelLanes naming the lane that runs it — never silence.",
			len(orphans))
	}
}

// excusedBy reports whether an absent test file is covered by a declared
// out-of-Bazel lane.
func excusedBy(t *testing.T, root, rel string, declared map[string]bool) (outOfBazelLane, bool) {
	t.Helper()
	for _, lane := range outOfBazelLanes {
		if lane.modulePath != "" && strings.HasPrefix(rel, lane.modulePath+"/") {
			return lane, true
		}
	}
	src, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	expr := buildConstraint(src)
	if expr == nil {
		return outOfBazelLane{}, false
	}
	return laneFor(expr, declared)
}

// laneFor returns the out-of-Bazel lane that actually makes an unsatisfiable
// constraint compile, or false when no single lane does.
//
// The question asked is "does this lane's tag set make the constraint
// SATISFIABLE", never "does the constraint mention a tag some lane names". The
// difference is a silent hole of exactly the kind this file exists to close:
// `//go:build libfdbc && bazelrunfile` MENTIONS libfdbc, so a tag-mention match
// hands it the libfdbc nightly's exemption — but that nightly runs
// `go test -tags libfdbc` and never sets the second tag, so gazelle omits the
// file, the nightly skips it, and it runs in NO lane at all while carrying a
// sanctioned-looking excuse. That is the one-character typo shape wearing the
// exemption table as a costume.
//
// Re-checking satisfiability under declared ∪ {lane tag} answers the question
// the exemption is actually claiming. It also gets the multi-lane case right by
// construction: a file needing two lanes' tags at once is compiled by neither
// lane alone, so neither excuses it.
func laneFor(expr constraint.Expr, declared map[string]bool) (outOfBazelLane, bool) {
	// A constraint that already holds needs no lane; without this guard a
	// satisfiable expression would match the first lane vacuously.
	if ok, _ := satisfiable(expr, declared); ok {
		return outOfBazelLane{}, false
	}
	for _, lane := range outOfBazelLanes {
		if lane.buildTag == "" {
			continue
		}
		withLane := map[string]bool{lane.buildTag: true}
		for tag := range declared {
			withLane[tag] = true
		}
		if ok, _ := satisfiable(expr, withLane); ok {
			return lane, true
		}
	}
	return outOfBazelLane{}, false
}

// TestOutOfBazelLanesAreReal keeps the exemption table honest from both ends.
//
// An exemption is a permanent hole in the gate, so it has to pay rent: the lane
// file must exist and must still mention the tag or module it claims to run
// (a renamed workflow or a deleted recipe turns the exemption into exactly the
// silent no-coverage it was granted to avoid), and the entry must still be USED
// by at least one file — a stale exemption is how the next genuinely-lost file
// slips through under cover of a rule written for something else.
func TestOutOfBazelLanesAreReal(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	declared := declaredBuildTags(t, root)
	inTest, _, _ := testTargetSrcs(t, root)

	used := map[string]bool{}
	for _, rel := range trackedTestFiles(t, root) {
		if len(inTest[rel]) > 0 {
			continue
		}
		if lane, ok := excusedBy(t, root, rel, declared); ok {
			used[lane.buildTag+"|"+lane.modulePath] = true
		}
	}

	for _, lane := range outOfBazelLanes {
		name := lane.buildTag
		if name == "" {
			name = lane.modulePath
		}
		raw, err := os.ReadFile(filepath.Join(root, lane.laneFile))
		if err != nil {
			t.Errorf("out-of-Bazel lane %q claims %s runs it, but that file is unreadable: %v", name, lane.laneFile, err)
			continue
		}
		if !strings.Contains(string(raw), lane.laneMentions) {
			t.Errorf("out-of-Bazel lane %q claims %s runs it, but that file no longer contains %q. "+
				"Either the lane moved (update the entry) or it is gone — in which case these tests now run NOWHERE, "+
				"which is the exact failure this gate exists to catch.", name, lane.laneFile, lane.laneMentions)
		}
		if !used[lane.buildTag+"|"+lane.modulePath] {
			t.Errorf("out-of-Bazel lane %q excuses no file any more — delete the entry. A stale exemption is a "+
				"standing hole: the next file that lands under it is exempted by a rule written for something else.", name)
		}
		if lane.why == "" {
			t.Errorf("out-of-Bazel lane %q has no stated reason; an unexplained exemption cannot be reviewed", name)
		}
	}
}

// TestGoRuleSrcsAreLiteralLists pins the assumption the membership gate rests on:
// no go_* rule computes its srcs with glob() (or anything else).
//
// This is the gate's own load-bearing negative result. A glob in a go_test's
// srcs is perfectly valid Bazel and would make the membership scan UNDER-report:
// files matched only by the glob would look like orphans, and the tempting fix
// for the resulting noise is to loosen the membership check rather than teach
// the parser. Pinning the property here means the day someone writes such a
// glob, THIS test fails with instructions instead of the real gate drifting.
//
// `data = glob(["testdata/**"])` is deliberately untouched, and is why the check
// is scoped to the srcs attribute: data is not compiled, and every glob in the
// repo today is on data or on a non-Go rule (java_library srcs, filegroup srcs of
// *.md). select() needs no check here — parseBuildGraph already aborts on it.
func TestGoRuleSrcsAreLiteralLists(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var offenders []string
	var goRules int
	for _, r := range parseBuildGraph(t, root) {
		if !strings.HasPrefix(r.kind, "go_") {
			continue
		}
		goRules++
		for _, call := range r.srcsCalls {
			offenders = append(offenders, "//"+r.label+": "+r.kind+" builds srcs with "+call+"()")
		}
	}
	sort.Strings(offenders)
	if goRules < 100 {
		t.Fatalf("only %d go_* rules parsed under %s — the BUILD scan is not seeing the real tree", goRules, root)
	}
	for _, o := range offenders {
		t.Errorf("non-literal srcs: %s", o)
	}
	if len(offenders) > 0 {
		t.Errorf("%d go_* rule(s) compute srcs dynamically. TestEveryTestFileIsInABuildTarget reads srcs as literal string "+
			"lists, so a computed srcs makes it UNDER-report — files it cannot see look like orphans, and the tempting fix is to "+
			"weaken the membership check. Teach the parser to expand the construct instead.", len(offenders))
	}
}

// TestBuildTagModelMatchesGazelle is the strongest check here: for EVERY
// build-tag-gated test file in the tree, the satisfiability model must agree
// with what gazelle actually did.
//
// The exemption path only works if "gazelle omitted this deliberately" is
// decided the same way gazelle decides it. If the model were too generous, a
// genuinely lost file would be waved through as "deliberately excluded"; if it
// were too strict, the gate would demand srcs entries gazelle refuses to write
// and get switched off. Checking the model against all ~100 tagged files —
// rather than against a couple of invented examples — is what makes it credible,
// and it re-verifies on every run as new tags appear.
func TestBuildTagModelMatchesGazelle(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	declared := declaredBuildTags(t, root)
	inTest, _, _ := testTargetSrcs(t, root)

	var tagged int
	for _, rel := range trackedTestFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		expr := buildConstraint(src)
		if expr == nil {
			continue
		}
		tagged++
		sat, undeclared := satisfiable(expr, declared)
		registered := len(inTest[rel]) > 0

		switch {
		case sat && !registered:
			t.Errorf("%s: constraint %q is satisfiable under the declared build_tags, so gazelle should have listed it "+
				"in srcs — it is absent. A build tag is not an excuse: gazelle lists satisfiable-tag files unconditionally "+
				"and rules_go honours the constraint at compile time. Run `just gazelle`.", rel, expr.String())
		case !sat && registered:
			t.Errorf("%s: constraint %q cannot hold under the declared build_tags (blocked by %v), yet the file IS in a "+
				"go_test's srcs. The model disagrees with gazelle: fix freeBuildTags/declaredBuildTags before trusting the "+
				"exemption path, because the same disagreement can wave a genuinely lost file through.", rel, expr.String(), undeclared)
		case !sat:
			// Deliberately excluded — every blocking tag must be declared as an
			// out-of-Bazel lane, or the file runs nowhere at all.
			if _, ok := excusedBy(t, root, rel, declared); !ok {
				t.Errorf("%s: constraint %q is blocked by undeclared build tag(s) %v, so gazelle omits the file AND "+
					"`go test` without those tags skips it — it runs NOWHERE. Either add the tag to the root BUILD.bazel's "+
					"`# gazelle:build_tags` directive, or add an outOfBazelLanes entry naming the CI lane that runs it. "+
					"A one-character typo in a build tag produces exactly this state and looks entirely normal in review.",
					rel, expr.String(), undeclared)
			}
		}
	}
	if tagged < 50 {
		t.Fatalf("found only %d build-tag-gated test files under %s — the race-tagged lanes under pkg/relational/sqldriver "+
			"and pkg/recordlayer plus the bazelrunfiles corpus are expected; the scan is not reading the real tree", tagged, root)
	}
}

// TestSatisfiableUnderDeclaredTags pins the model on the constraint shapes that
// actually occur in this repo, each with the behaviour observed from gazelle's
// real output. A whole-tree scan shows only that the tree is currently
// consistent; these cases pin WHY, so a change to freeBuildTags that happens to
// keep the tree green still has to keep the reasoning right.
func TestSatisfiableUnderDeclaredTags(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{"bazelrunfiles": true, "stress": true, "factorycorpus": true}

	cases := []struct {
		line    string
		want    bool
		blocked []string
	}{
		{"//go:build bazelrunfiles", true, nil},
		{"//go:build stress", true, nil},
		// A free tag is satisfiable in one of its two configurations, which is
		// why race-tagged files are in srcs and rules_go decides at compile time.
		{"//go:build race", true, nil},
		{"//go:build !race", true, nil},
		{"//go:build bazelrunfiles && linux", true, nil},
		// The negation of an undeclared tag is TRUE, not unknown: gazelle emits
		// these, so the gate must require them.
		{"//go:build !libfdbc", true, nil},
		// Undeclared tags: gazelle drops the file.
		{"//go:build libfdbc", false, []string{"libfdbc"}},
		{"//go:build yamsql", false, []string{"yamsql"}},
		{"//go:build stress && realcluster", false, []string{"realcluster"}},
		// cgo is free, so the verdict must be driven by libfdbc alone —
		// reporting cgo as the blocker would point the fix at the wrong tag.
		{"//go:build cgo && libfdbc", false, []string{"libfdbc"}},
		// A disjunction survives one undeclared arm.
		{"//go:build libfdbc || stress", true, nil},
		// The typo shape: one character off a declared tag, invisible in review,
		// and the file then runs in no lane at all.
		{"//go:build bazelrunfile", false, []string{"bazelrunfile"}},
	}
	for _, tc := range cases {
		expr, err := constraint.Parse(tc.line)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.line, err)
		}
		got, blocked := satisfiable(expr, declared)
		if got != tc.want {
			t.Errorf("satisfiable(%q) = %v, want %v", tc.line, got, tc.want)
		}
		if !tc.want && strings.Join(blocked, ",") != strings.Join(tc.blocked, ",") {
			t.Errorf("satisfiable(%q) blocked by %v, want %v", tc.line, blocked, tc.blocked)
		}
	}
}

// TestLaneExemptionRequiresTheLaneToCompileTheFile pins the exemption path
// against the permissive-direction hole it shipped with: excusing a file because
// its constraint MENTIONS a tag some lane names, rather than because that lane
// can actually compile it.
//
// The permissive direction is the dangerous one. Too strict and the gate demands
// srcs entries gazelle refuses to write, which is loud and gets fixed; too
// generous and a file that runs NOWHERE carries a sanctioned-looking excuse and
// nobody ever looks again. `//go:build libfdbc && bazelrunfile` is the exact
// shape: it names libfdbc, so the old tag-mention match handed it the nightly's
// exemption, while the nightly's `go test -tags libfdbc` never sets the second
// tag and gazelle omits the file — the one-character typo the gate was built to
// catch, laundered through the exemption table.
func TestLaneExemptionRequiresTheLaneToCompileTheFile(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{"bazelrunfiles": true, "stress": true, "factorycorpus": true}

	cases := []struct {
		line string
		// want is the excusing lane's build tag, or "" for no exemption.
		want string
		why  string
	}{
		{"//go:build libfdbc", "libfdbc", "the nightly's -tags libfdbc compiles it"},
		{"//go:build cgo && libfdbc", "libfdbc", "cgo is free, so the lane's tag is enough"},
		{"//go:build yamsql", "yamsql", "the justfile recipe sets it"},
		{"//go:build stress && realcluster", "realcluster", "stress is declared; the lane supplies realcluster"},
		{"//go:build libfdbc || yamsql", "libfdbc", "a disjunction one lane does satisfy"},

		// The hole. Each names a real lane tag but needs one no lane sets.
		{"//go:build libfdbc && bazelrunfile", "", "the typo shape: -tags libfdbc never sets bazelrunfile"},
		{"//go:build libfdbc && faketag", "", "a second undeclared tag no lane supplies"},
		{"//go:build yamsql && realcluster", "", "two lanes' tags at once — neither lane sets both"},
		{"//go:build libfdbc && yamsql", "", "no single lane runs both tags"},

		// Already satisfiable: gazelle lists these, so no exemption may apply.
		{"//go:build race", "", "free tag — gazelle lists it, an exemption would be vacuous"},
		{"//go:build stress", "", "declared — gazelle lists it"},
		{"//go:build !libfdbc", "", "the negation holds under the declared set"},
	}
	for _, tc := range cases {
		expr, err := constraint.Parse(tc.line)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.line, err)
		}
		lane, ok := laneFor(expr, declared)
		got := ""
		if ok {
			got = lane.buildTag
		}
		if got != tc.want {
			t.Errorf("laneFor(%q) excused by %q, want %q (%s)", tc.line, got, tc.want, tc.why)
		}
	}
}

// TestBuildConstraintHeaderOnly pins that only the file HEADER is read for a
// constraint. A `//go:build` line quoted inside a comment further down (this
// package's gates quote build lines in their prose) must not be mistaken for the
// file's own constraint — doing so would let a file exempt itself from the gate
// by mentioning a tag in passing.
func TestBuildConstraintHeaderOnly(t *testing.T) {
	t.Parallel()

	if expr := buildConstraint([]byte("//go:build race\n\npackage p\n")); expr == nil || expr.String() != "race" {
		t.Errorf("header constraint = %v, want race", expr)
	}
	if expr := buildConstraint([]byte("package p\n\n// A file carrying //go:build libfdbc is excluded.\nvar X int\n")); expr != nil {
		t.Errorf("constraint quoted after the package clause was read as the file's own: %v", expr)
	}
	if expr := buildConstraint([]byte("package p\n")); expr != nil {
		t.Errorf("unconstrained file reported constraint %v", expr)
	}
}

// TestSrcsParsingShapes pins the parse on the shapes that actually occur,
// because a whole-tree scan can only ever show the tree is currently CLEAN — it
// can never show the gate would NOTICE a missing file. Each case is a real shape
// from this repo: multi-line gazelle srcs, `# keep:` comments interleaved with
// entries, a glob on data (not to be mistaken for a glob on srcs), a
// cross-package file label in data (which must NOT be credited as membership),
// and a build-tag-gated file (an ordinary srcs entry).
func TestSrcsParsingShapes(t *testing.T) {
	t.Parallel()

	const content = `go_library(
    name = "widget",
    srcs = [
        "doc.go",
        "widget.go",
    ],
    importpath = "fdb.dev/pkg/widget",
)

go_test(
    name = "widget_test",
    srcs = [
        # keep: race-tagged lane, compiled only under --race
        "race_lane_test.go",
        "widget_test.go",
    ],
    data = glob(["testdata/**"]) + [
        "//pkg/other:other_test.go",
    ],
    embed = [":widget"],
    tags = ["external"],
)
`
	rules := parseBuildFile(t, "pkg/widget/BUILD.bazel", "pkg/widget", stripBuildComments(content))
	if len(rules) != 2 {
		t.Fatalf("parsed %d rules, want 2: %+v", len(rules), rules)
	}
	lib, test := rules[0], rules[1]
	if lib.kind != "go_library" || test.kind != "go_test" {
		t.Fatalf("kinds = %q, %q; want go_library, go_test", lib.kind, test.kind)
	}
	if got, want := strings.Join(srcPaths(lib), ","), "pkg/widget/doc.go,pkg/widget/widget.go"; got != want {
		t.Errorf("go_library srcs = %q, want %q", got, want)
	}
	// The `# keep:` comment must not become an entry; the build-tag-gated file
	// must; and the cross-package label under `data` must NOT leak into srcs —
	// crediting a data dep as membership is exactly how a file that is merely
	// STAGED gets mistaken for a file that is RUN.
	if got, want := strings.Join(srcPaths(test), ","), "pkg/widget/race_lane_test.go,pkg/widget/widget_test.go"; got != want {
		t.Errorf("go_test srcs = %q, want %q", got, want)
	}
	if len(test.srcsCalls) != 0 {
		t.Errorf("go_test srcs reported calls %v, want none — the glob is on data, not srcs", test.srcsCalls)
	}

	// A glob in srcs must be REPORTED, not silently read as an empty list: an
	// empty list would make every globbed file look like an orphan, and a
	// silently-dropped attribute is how a gate starts lying.
	globbed := parseBuildFile(t, "pkg/g/BUILD.bazel", "pkg/g",
		stripBuildComments("go_test(\n    name = \"g\",\n    srcs = glob([\"*_test.go\"]),\n    tags = [\"external\"],\n)\n"))
	if len(globbed) != 1 {
		t.Fatalf("parsed %d rules for the globbed case, want 1", len(globbed))
	}
	if got := globbed[0].srcsCalls; len(got) != 1 || got[0] != "glob" {
		t.Errorf("globbed srcs reported calls %v, want [glob]", got)
	}
	if got := globbed[0].tags; len(got) != 1 || got[0] != "external" {
		t.Errorf("the srcs-region scan bled into the next attribute: tags = %v, want [external]", got)
	}
}

// TestSrcPathsLabelForms pins label resolution: a bare name is package-relative,
// `//pkg/x:f.go` names its own package, `:f.go` is the local form, and an
// external `@repo//…` src is not a file of this checkout.
func TestSrcPathsLabelForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pkg, entry, want string
	}{
		{"pkg/widget", "widget_test.go", "pkg/widget/widget_test.go"},
		{"pkg/widget", "sub/widget_test.go", "pkg/widget/sub/widget_test.go"},
		{"pkg/widget", "//pkg/other:other_test.go", "pkg/other/other_test.go"},
		{"pkg/widget", ":gen_test.go", "pkg/widget/gen_test.go"},
		{"", "root_test.go", "root_test.go"},
		{"pkg/widget", "@com_example//x:y.go", ""},
	}
	for _, tc := range cases {
		got := srcPaths(buildRule{pkg: tc.pkg, srcs: []string{tc.entry}})
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("srcPaths(%q in %q) = %v, want none", tc.entry, tc.pkg, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("srcPaths(%q in %q) = %v, want [%s]", tc.entry, tc.pkg, got, tc.want)
		}
	}
}

// TestHasPathComponent pins the testdata exclusion against the substring bug it
// would otherwise have. conformance/testdata_test.go is a REAL test whose name
// begins with the excluded word; strings.Contains would drop it from the scan
// set and quietly exempt exactly one file from the gate, forever.
func TestHasPathComponent(t *testing.T) {
	t.Parallel()
	excluded := []string{
		"pkg/relational/core/parser/testdata/x_test.go",
		"testdata/x_test.go",
		"a/b/testdata/c/d_test.go",
	}
	for _, rel := range excluded {
		if !hasPathComponent(rel, "testdata") {
			t.Errorf("%q should be excluded as a testdata fixture", rel)
		}
	}
	kept := []string{
		"conformance/testdata_test.go",
		"pkg/x/testdata_helpers_test.go",
		"pkg/testdatabase/x_test.go",
	}
	for _, rel := range kept {
		if hasPathComponent(rel, "testdata") {
			t.Errorf("%q is a real test file, not a testdata fixture — it must stay in the scan set", rel)
		}
	}
}
