package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A test target that starts an FDB testcontainer must DECLARE what it costs.
//
// Bazel packs local test targets by --local_test_jobs alone unless a target
// declares a resource cost. That is the whole failure: .bazelrc sets
// --local_test_jobs=4 globally, the runners are 4 vCPU / 8 GB, and the
// container-backed suites are the memory consumers — so any lane can land four
// of them on one box at once. It did. Runners OOM-killed themselves, and on one
// the RUNNER SERVICE ITSELF was the kill victim, which left the agent holding a
// job it could never finish: GitHub still reported busy=true while the process
// was gone, so the slot was neither running nor free and the fleet quietly ran
// at 4/5 capacity until a human noticed.
//
// Two mechanisms look like they solve this and DO NOT, both measured here on
// Bazel 9.0.1 rather than assumed:
//
//   - size = "large"/"enormous" is a TIMEOUT class, not a memory class. Six
//     identical tests at --local_test_jobs=4 ran 4-wide at every size.
//   - `tags = ["resources:memory:N"]` weighed against
//     --local_resources=memory=B is the documented way to price a local test,
//     and it genuinely works — for sh_test. It does NOT work for rules_go's
//     go_test, which is every test target in this repo. Measured directly: four
//     go_test targets each tagged `resources:memory:400`, run under
//     --local_resources=memory=400, all four still executed CONCURRENTLY (BEP
//     start/duration overlap, not sampling). The same experiment on sh_test
//     serialises them exactly as floor(B/N) predicts.
//
// That last point is why this file does not set a memory budget anywhere: for
// go_test a budget is inert, and a `--local_resources=memory=...` line in
// .bazelrc would read as a control while gating nothing — the precise illusion
// that let the fleet believe it was capped. The race lane already carries such a
// line; it is decorative for the same reason.
//
// What DOES gate a go_test is `tags = ["exclusive"]`, measured in the same run:
// the exclusive target waited for every other test to finish and overlapped
// nothing. So the enforcement here is coarse by necessity — a target either is
// safe to co-schedule up to --local_test_jobs deep, or it is exclusive.
//
// The resources:memory tag is therefore kept as the DECLARED MEASURED COST, not
// as a control: it is the number TestContainerSuitesFitTheRunner does arithmetic
// on to decide whether a target may share a box at all. Keeping it in the tag
// (rather than a comment) means the guard can read it, and means it is stated in
// the same place a future Bazel that honours it would look.
//
// The target set is derived STRUCTURALLY — the reverse-dependency closure of the
// testcontainers helper over the BUILD graph, the same thing
// `bazel query 'kind("go_test rule", rdeps(//..., //pkg/testcontainers/foundationdb:foundationdb))'`
// computes. Deriving it from a checked-in list, or from target NAMES, would rot
// the moment someone adds a suite; the closure cannot, because depending on the
// helper is precisely what makes a target start a container.
func TestFDBContainerTestTargetsDeclareMemoryCost(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	graph := parseBuildGraph(t, root)
	targets := containerBackedTests(graph, testcontainerHelperLabel)
	if len(targets) == 0 {
		t.Fatalf("the reverse-dependency closure of %s contains no go_test targets. "+
			"That is not plausible — this repo has ~26. The BUILD graph parser stopped "+
			"recognising the dep edges (a formatting change, a select(), a renamed helper), "+
			"so this gate now passes by seeing nothing rather than by finding everything tagged.",
			testcontainerHelperLabel)
	}

	for _, label := range targets {
		rule := graph[label]
		if _, ok := memoryTagMB(rule.tags); !ok {
			t.Errorf("%s starts FDB testcontainers but declares no memory cost.\n"+
				"  Bazel packs it up to --local_test_jobs deep on a 4 vCPU / 8 GB runner, and an\n"+
				"  undeclared suite is invisible to the arithmetic in TestContainerSuitesFitTheRunner —\n"+
				"  so it can push the worst co-scheduled set past what the box holds while every gate\n"+
				"  stays green. That is how the runner agents got OOM-killed.\n"+
				"  REMEDY: measure peak memory the way CI runs it — 4 cores, GOMAXPROCS=4 — and count\n"+
				"  BOTH the Go test process (VmHWM) and its FDB containers (their cgroup memory.current,\n"+
				"  summed at the same instant). They are separate cgroups and both sit on the box; for\n"+
				"  several suites here the CONTAINERS are the larger half. Then declare it:\n"+
				"      tags = [\"resources:memory:<peak MB, rounded up>\"],\n"+
				"  and add \"exclusive\" too if it is large enough that peers no longer fit alongside it.",
				label)
		}
	}
}

// testEnvelopeMB is how much of a runner may be spent on concurrently running
// tests, derived from measurements on the fleet rather than picked:
//
//	7745  MemTotal on a cpx32 runner
//	- 730  measured baseline on an IDLE runner (runner agent + dockerd +
//	       containerd + kernel): 7745 total - 7017 MemAvailable
//	-2500  the bazel JVM, page cache and slack. The race lane caps its server at
//	       -Xmx3g; heap is not RSS, but this must not be the term that is wrong.
//	=4515  -> 4500, rounded down.
//
// Staying inside this is what keeps the box off its swapfile. Swap is why this
// outage presented as 60-second "context deadline exceeded" failures instead of
// a clean OOM: the box did not fall over, it went slow enough that unrelated
// tests blew their deadlines, which reads like flakiness rather than capacity.
const testEnvelopeMB = 4500

// A runner must survive the WORST CO-SCHEDULED SET, not the worst single target.
//
// Bazel packs up to --local_test_jobs tests at once and, per the file header,
// nothing prices a go_test — so the worst case is simply the N most expensive
// non-exclusive suites running together, where N is --local_test_jobs. This gate
// does that arithmetic against the measured declarations and fails if the set
// does not fit.
//
// The pair that actually took the fleet down is the reason this is a SET check
// and not a per-target check: factory_test and full_test were resident at
// 3.75 GiB and 3.30 GiB simultaneously on a 7 GB box whose 4 GiB swapfile was
// also exhausted. Neither is individually too big for a runner. Note what that
// rules out — --local_test_jobs=2 would NOT have saved this box, because two is
// already too many when it is THESE two. The only fix that holds is making such
// targets non-co-schedulable at all, which for go_test means `exclusive`.
//
// So a target has exactly two ways to satisfy this gate: be cheap enough that
// --local_test_jobs of its peers still fit, or declare `exclusive`.
//
// Note where the enforcement actually lives. The resources:memory numbers this
// reads are DATA — measured declarations that Bazel ignores for go_test (file
// header). Nothing stops the scheduler from co-scheduling any two of them. What
// gates is THIS TEST refusing a worst-set that exceeds the envelope, and the
// `exclusive` tags it forces onto the targets that cannot fit one. Read the
// numbers as documentation of cost and this gate as the control; the tag has no
// effect on scheduling on its own, and treating it as if it did is exactly the
// misreading that left --local_resources looking like a cap for so long.
//
// This gate has already earned its keep once during its own development: a
// `git checkout` of a BUILD file to undo a mutation silently reverted a real tag
// too, and this is what reported it rather than CI discovering it on a runner.
func TestContainerSuitesFitTheRunner(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	jobs := localTestJobs(t, filepath.Join(root, ".bazelrc"))
	graph := parseBuildGraph(t, root)

	type suite struct {
		label string
		mb    int
	}
	var shared []suite
	for _, label := range containerBackedTests(graph, testcontainerHelperLabel) {
		rule := graph[label]
		mb, ok := memoryTagMB(rule.tags)
		if !ok {
			continue // TestFDBContainerTestTargetsDeclareMemoryCost owns this failure.
		}
		if slices.Contains(rule.tags, "exclusive") {
			continue // Cannot co-schedule with anything, so it cannot form a bad set.
		}
		shared = append(shared, suite{label, mb})
	}
	if len(shared) == 0 {
		t.Fatalf("every container suite is exclusive, or none declares a cost — either way this gate " +
			"is doing no arithmetic and would not notice a new suite that is too big to share a box")
	}
	sort.Slice(shared, func(i, j int) bool { return shared[i].mb > shared[j].mb })

	worst := shared
	if len(worst) > jobs {
		worst = worst[:jobs]
	}
	total := 0
	var names []string
	for _, s := range worst {
		total += s.mb
		names = append(names, fmt.Sprintf("%s=%dMB", s.label, s.mb))
	}
	if total > testEnvelopeMB {
		t.Errorf("the %d most expensive co-schedulable container suites do not fit a runner: %s = %d MB > %d MB.\n"+
			"  Bazel packs --local_test_jobs=%d of them at once, so this set CAN be resident together on a\n"+
			"  4 vCPU / 8 GB box, and past that envelope the box starts swapping — which surfaces as\n"+
			"  unrelated tests failing their deadlines, not as an obvious OOM.\n"+
			"  THE BUDGET YOU ARE SPENDING AGAINST (all measured, see testEnvelopeMB):\n"+
			"      7745 MB  MemTotal on a cpx32 runner\n"+
			"    -  730 MB  idle baseline (runner agent + dockerd + containerd + kernel)\n"+
			"    - 2500 MB  bazel JVM, page cache and slack\n"+
			"    = %d MB  available for CONCURRENT tests; the set above wants %d MB\n"+
			"  REMEDY: add tags = [\"exclusive\"] to the offender (measured: exclusive is the only thing that\n"+
			"  actually serialises a go_test — resources:memory is inert for rules_go), or lower\n"+
			"  --local_test_jobs in .bazelrc, or re-measure if the declaration is stale.",
			len(worst), strings.Join(names, " + "), total, testEnvelopeMB, jobs, testEnvelopeMB, total)
	}
}

// localTestJobs reads --local_test_jobs from .bazelrc. It is half of the worst
// case above, so the gate must react to it changing rather than hardcode 4.
func localTestJobs(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := regexp.MustCompile(`^test\s+--local_test_jobs=(\d+)$`).FindStringSubmatch(line)
		if m != nil {
			n, convErr := strconv.Atoi(m[1])
			if convErr != nil || n <= 0 {
				t.Fatalf("unusable --local_test_jobs value %q in .bazelrc", m[1])
			}
			return n
		}
	}
	t.Fatalf(".bazelrc sets no `test --local_test_jobs=N`. Without it Bazel packs local tests by CPU count " +
		"(the runners report 4, a dev box far more), and the worst co-scheduled set this gate checks becomes " +
		"unbounded — the gate cannot state what it is protecting.")
	return 0
}

// The closure must be TRANSITIVE, and nothing else in this file proves that.
//
// Every real container target in this repo happens to reach the helper through
// at least one intermediate library, so a parser that followed only DIRECT deps
// would find almost none of them — and the gate above would then report "no
// go_test targets", which its own guard catches. But a parser that followed two
// levels and stopped, or one that lost `embed` edges (rules_go routes a
// go_test's library deps through embed, not deps), would still find MOST
// targets and quietly miss the deepest ones. Those are the misses nobody sees: a
// green gate that skipped exactly the suite that was added last.
//
// So this pins the property directly, on a fixture whose only path to the helper
// is three hops long and runs through an embed edge.
func TestContainerClosureFollowsTransitiveAndEmbedEdges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(pkg, body string) {
		t.Helper()
		full := filepath.Join(dir, pkg)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, "BUILD.bazel"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", pkg, err)
		}
	}
	// helper <- mid <- lib <- (embed) test. The test names the helper nowhere.
	write("pkg/testcontainers/foundationdb", "go_library(\n    name = \"foundationdb\",\n)\n")
	write("pkg/mid", "go_library(\n    name = \"mid\",\n    deps = [\"//pkg/testcontainers/foundationdb\"],\n)\n")
	write("pkg/lib", "go_library(\n    name = \"lib\",\n    deps = [\"//pkg/mid\"],\n)\n")
	write("pkg/deep", "go_test(\n    name = \"deep_test\",\n    embed = [\"//pkg/lib\"],\n)\n")
	// A control: reachable from nothing, must NOT be reported.
	write("pkg/pure", "go_test(\n    name = \"pure_test\",\n)\n")

	graph := parseBuildGraph(t, dir)
	got := containerBackedTests(graph, testcontainerHelperLabel)

	want := []string{"pkg/deep:deep_test"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("closure over a 3-hop chain ending in an `embed` edge = %v, want %v.\n"+
			"The gate's whole target set comes from this walk. If it does not cross every hop and "+
			"every embed edge, container suites go unpriced while the gate stays green.", got, want)
	}
}

// A nested git checkout is another repository's territory, and the walk must
// not cross into it.
//
// A worktree marks itself with a .git FILE (not a directory), so a name-based
// directory skip never fires on it. Without the boundary check, a leftover
// worktree under .claude/worktrees/ carrying an unpriced container target
// reddens this checkout's gate — while CI, which has no worktrees, stays
// green. The failure is then unreproducible exactly where the gate runs.
func TestBuildGraphWalkStopsAtNestedCheckouts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(pkg, name, body string) {
		t.Helper()
		full := filepath.Join(dir, pkg)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", pkg, name, err)
		}
	}
	// The root's own target: must be seen (control — proves the walk still
	// descends everywhere a boundary marker is absent, including when the
	// ROOT itself carries a .git entry, as the real repo does).
	write("", ".git", "gitdir: /elsewhere\n")
	write("pkg/real", "BUILD.bazel", "go_test(\n    name = \"real_test\",\n    deps = [\"//pkg/testcontainers/foundationdb\"],\n)\n")
	write("pkg/testcontainers/foundationdb", "BUILD.bazel", "go_library(\n    name = \"foundationdb\",\n)\n")
	// A nested worktree: .git is a FILE, and its unpriced container target
	// must be invisible to the graph.
	write(".claude/worktrees/stale", ".git", "gitdir: /elsewhere\n")
	write(".claude/worktrees/stale/cmd/thing", "BUILD.bazel", "go_test(\n    name = \"thing_test\",\n    deps = [\"//pkg/testcontainers/foundationdb\"],\n)\n")

	graph := parseBuildGraph(t, dir)
	got := containerBackedTests(graph, testcontainerHelperLabel)

	want := []string{"pkg/real:real_test"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("container-backed tests with a nested checkout present = %v, want %v.\n"+
			"A nested worktree's targets never run from this checkout; pricing them here lets a "+
			"leftover tree redden a gate CI cannot reproduce, and skipping the root's own tree "+
			"would unprice every real target.", got, want)
	}
}

// A tag whose value is not a positive integer is not a declaration.
//
// `resources:memory:` with junk after it, or a zero, parses as "present" to any
// check that only looks for the prefix — and a zero cost is worse than no tag at
// all, because it reads as deliberate while still occupying no budget. Bazel
// itself will not reject it in a way anyone in this repo will see.
func TestMemoryTagValueMustBePositiveInteger(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		tag    string
		wantMB int
		wantOK bool
	}{
		{"resources:memory:1400", 1400, true},
		{"resources:memory:5600", 5600, true},
		{"resources:memory:0", 0, false},
		{"resources:memory:-100", 0, false},
		{"resources:memory:", 0, false},
		{"resources:memory:1.5", 0, false},
		{"resources:memory:1400x", 0, false},
		{"resources:cpu:2", 0, false},
		{"manual", 0, false},
	} {
		gotMB, gotOK := memoryTagMB([]string{"manual", tc.tag})
		if gotOK != tc.wantOK || (tc.wantOK && gotMB != tc.wantMB) {
			t.Errorf("memoryTagMB(%q) = (%d, %v), want (%d, %v)", tc.tag, gotMB, gotOK, tc.wantMB, tc.wantOK)
		}
	}
}

// testcontainerHelperLabel is the go_library every FDB-container suite reaches.
// Depending on it is what makes a target start a container, which is why the
// closure is rooted here rather than at a name pattern.
const testcontainerHelperLabel = "pkg/testcontainers/foundationdb:foundationdb"

// memoryTagRe matches a well-formed memory cost declaration. The value is
// megabytes, matching --local_resources=memory=N.
var memoryTagRe = regexp.MustCompile(`^resources:memory:([0-9]+)$`)

// memoryTagMB returns the declared cost in MB, and whether a valid one is present.
func memoryTagMB(tags []string) (int, bool) {
	for _, tag := range tags {
		m := memoryTagRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		mb, err := strconv.Atoi(m[1])
		if err != nil || mb <= 0 {
			continue
		}
		return mb, true
	}
	return 0, false
}

// buildRule is the slice of a BUILD rule the gates in this package need.
type buildRule struct {
	kind  string
	label string
	pkg   string
	deps  []string
	tags  []string
	// srcs are the literal srcs entries, package-relative as written.
	// TestEveryTestFileIsInABuildTarget reads these to answer "is this file on
	// disk actually compiled by anything?"; srcsCalls records any function
	// (glob, …) used to build the list, since a computed srcs means the literal
	// entries are not the whole set.
	srcs      []string
	srcsCalls []string
}

// containerBackedTests returns the go_test targets that transitively depend on
// root, sorted. This is the Go-side equivalent of bazel's
// `kind("go_test rule", rdeps(//..., root))`, computed from the BUILD files so
// the gate needs no bazel subprocess.
func containerBackedTests(graph map[string]buildRule, root string) []string {
	// Reverse edges: dep -> dependents.
	rdeps := map[string][]string{}
	for label, rule := range graph {
		for _, dep := range rule.deps {
			rdeps[dep] = append(rdeps[dep], label)
		}
	}
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dependent := range rdeps[cur] {
			if !seen[dependent] {
				seen[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	var out []string
	for label := range seen {
		if label != root && graph[label].kind == "go_test" {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

// parseBuildGraph reads every BUILD.bazel under root into a label -> rule map.
//
// The files are buildifier-formatted: every rule invocation opens at column 0
// and closes with a `)` at column 0, and the repo uses no select(). Both are
// checked, so a future file that breaks either assumption fails loudly here
// rather than silently dropping edges — a dropped edge is an unpriced target.
func parseBuildGraph(t *testing.T, root string) map[string]buildRule {
	t.Helper()
	graph := map[string]buildRule{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "fdb-record-layer" {
				return filepath.SkipDir
			}
			// A nested git checkout (a worktree's .git is a FILE, so the name
			// check above never sees it) is another repository's territory:
			// its targets run under its own gates, never from this checkout,
			// so pricing them here only lets a leftover tree redden a gate
			// CI cannot reproduce.
			if path != root {
				if _, statErr := os.Lstat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Name() != "BUILD.bazel" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		pkg := filepath.ToSlash(rel)
		if pkg == "." {
			pkg = ""
		}
		for _, rule := range parseBuildFile(t, path, pkg, stripBuildComments(string(raw))) {
			graph[rule.label] = rule
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking BUILD files under %s: %v", root, err)
	}
	return graph
}

// ruleStart matches a rule invocation opening at column 0, e.g. `go_test(`.
var ruleStart = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\($`)

func parseBuildFile(t *testing.T, path, pkg, content string) []buildRule {
	t.Helper()
	var out []buildRule
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		m := ruleStart.FindStringSubmatch(strings.TrimRight(lines[i], " \t"))
		if m == nil {
			continue
		}
		kind := m[1]
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(lines[end], ")") {
			end++
		}
		if end >= len(lines) {
			t.Fatalf("%s: rule %s( opening at line %d never closes with a `)` at column 0. "+
				"This parser relies on buildifier formatting; if the file is hand-formatted now, "+
				"the gate would silently lose every rule after this point.", path, kind, i+1)
		}
		body := strings.Join(lines[i+1:end], "\n")
		i = end
		if strings.Contains(body, "select(") {
			t.Fatalf("%s: rule %s( uses select(), which this parser does not evaluate. "+
				"Its deps would be dropped and any container target behind them would go unpriced. "+
				"Teach the parser to read select() branches before landing that BUILD change.", path, kind)
		}
		name, ok := scalarAttr(body, "name")
		if !ok {
			continue
		}
		rule := buildRule{kind: kind, label: pkg + ":" + name, pkg: pkg}
		rule.srcs = listAttr(body, "srcs")
		rule.srcsCalls = attrCalls(body, "srcs")
		// rules_go routes a go_test's library through `embed`, not `deps`, so a
		// walk that reads only `deps` misses the edge that matters most here.
		for _, attr := range []string{"deps", "embed"} {
			for _, raw := range listAttr(body, attr) {
				if dep, ok := normalizeLabel(pkg, raw); ok {
					rule.deps = append(rule.deps, dep)
				}
			}
		}
		rule.tags = listAttr(body, "tags")
		out = append(out, rule)
	}
	return out
}

// scalarAttr reads `name = "value"` from a rule body.
func scalarAttr(body, attr string) (string, bool) {
	re := regexp.MustCompile(`(?m)^\s*` + attr + `\s*=\s*"([^"]*)"\s*,?\s*$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// listAttr reads the quoted strings of `attr = [...]` from a rule body,
// bracket-matching so a nested list cannot truncate the read.
func listAttr(body, attr string) []string {
	re := regexp.MustCompile(`(?m)^\s*` + attr + `\s*=\s*\[`)
	loc := re.FindStringIndex(body)
	if loc == nil {
		return nil
	}
	depth := 0
	end := -1
	for i := loc[1] - 1; i < len(body); i++ {
		switch body[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(body[loc[1]:end], -1) {
		out = append(out, m[1])
	}
	return out
}

// normalizeLabel turns a BUILD label into the `pkg:name` form this graph keys
// on. External labels (@repo//...) are not part of the closure and are dropped.
func normalizeLabel(pkg, raw string) (string, bool) {
	if strings.HasPrefix(raw, "@") {
		return "", false
	}
	if strings.HasPrefix(raw, ":") {
		return pkg + raw, true
	}
	if !strings.HasPrefix(raw, "//") {
		return "", false
	}
	rest := strings.TrimPrefix(raw, "//")
	if idx := strings.Index(rest, ":"); idx >= 0 {
		return rest[:idx] + ":" + rest[idx+1:], true
	}
	// `//pkg/x` is shorthand for `//pkg/x:x`.
	if rest == "" {
		return "", false
	}
	return rest + ":" + rest[strings.LastIndex(rest, "/")+1:], true
}

// stripBuildComments removes `#` comments while preserving `#` inside strings —
// BUILD files here carry long `# keep:` rationales inside dep lists, and those
// comments quote target labels that must NOT be read as edges.
func stripBuildComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inString = !inString
			b.WriteByte(c)
		case c == '#' && !inString:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			inString = false
		case c == '\n':
			inString = false
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
