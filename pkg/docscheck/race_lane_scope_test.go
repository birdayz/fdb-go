package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The PR race lane is scoped, and a scope is a place coverage can leak out of.
//
// ci.yml's race job subtracts //pkg/relational/conformance/... from its
// relational scope, because those Docker-backed corpus targets dominate the
// lane on the 4-vCPU runner: measured under -race on four cores, the corpus
// execution target alone runs 5000 scenarios in 1328s and is by far the
// heaviest resident set in the lane, and co-scheduling four such targets on a
// 7.6 GB box exhausted it and drove a 31s target
// (//pkg/relational/conformance/plandiff) into its 900s timeout. RFC-107 §3
// made exactly this scoping call for //pkg/fdbgo/..., on measured-latency
// grounds: keep the Docker- and cgo-backed suites out of the PR race lane
// because race instrumentation turns them into the lane's whole cost. It said
// nothing about the relational wildcard, which nobody widened — directories
// grew into it until it carried the same class of target, so the same
// reasoning now applies here.
//
// Subtracting is only legitimate because the same targets keep running under
// the detector NIGHTLY. That second half is invisible at the subtraction site,
// which is exactly how it would get lost: someone tidies the nightly step,
// nothing in the PR lane changes, and the corpora quietly stop being raced
// anywhere while every gate stays green. This test is what makes that a build
// failure rather than a discovery six months later.
func TestRaceLaneSubtractionsStayCoveredNightly(t *testing.T) {
	t.Parallel()

	unraced, nightly := unracedSubtractions(t, workflowDir(t))
	for _, pat := range unraced {
		t.Errorf("ci.yml's race lane subtracts %s, but no nightly workflow races it (nightly race patterns: %v).\n"+
			"Excluding a scope from the PR lane is a latency decision; dropping it from every lane is a coverage decision. "+
			"Either put it back in the nightly race step or delete it from the PR lane's exclusion deliberately.",
			pat, nightly)
	}
}

// A target pattern named only inside a SHELL COMMENT is not raced.
//
// The gate's whole job is to answer "does some nightly lane still run the
// detector over this?", and the cheapest way for that answer to go wrong is the
// drift where someone comments the pattern out of the invocation and leaves the
// text behind as a note. The scope subtraction in ci.yml does not change, every
// gate stays green, and nothing races the corpora. So the fixture below is the
// realistic shape, not a contrived one, and it must FAIL.
func TestRaceLaneGateIgnoresCommentedOutPatterns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("ci.yml", `
jobs:
  race:
    steps:
      - name: -race tests
        run: |
          REL_SCOPE="//pkg/relational/... -//pkg/relational/conformance/..."
          $RACE -- $REL_SCOPE
`)
	write("nightly-coverage.yml", `
jobs:
  coverage:
    steps:
      - name: Race detector
        run: |
          bazelisk test //pkg/recordlayer:recordlayer_test \
            --@rules_go//go/config:race
          # TODO restore //pkg/relational/conformance/... here
`)

	unraced, nightly := unracedSubtractions(t, dir)
	if len(unraced) == 0 {
		t.Fatalf("the fixture races nothing under //pkg/relational/conformance/... — it names that pattern only in a shell comment — "+
			"yet the gate reported full coverage (patterns it believed were raced: %v). "+
			"A commented-out pattern satisfies the gate, so the gate cannot detect the drift it exists for.", nightly)
	}
}

// Stripping comments must not eat a `#` that is part of an argument.
//
// The stripper's rule is "a token that BEGINS with #", not "any #", precisely so
// that a flag value carrying one survives. Pinned because the obvious
// simplification — cut at the first `#` on the line — silently truncates such an
// invocation and would drop every target pattern after it, making the gate
// under-report coverage instead of over-reporting it.
func TestStripShellCommentsKeepsInlineHashes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// The trailing "\n" on each want is the input's own final newline: the
		// stripper is line-oriented and emits one newline per line, including
		// the empty one that a trailing newline produces.
		{"trailing comment", "bazelisk test //a/... # and //b/... too\n", "bazelisk test //a/... \n\n"},
		{"whole-line comment", "  # //a/...\nbazelisk test //b/...\n", "\nbazelisk test //b/... \n\n"},
		{"hash inside a flag value", "bazelisk test --x=a#b //a/...\n", "bazelisk test --x=a#b //a/... \n\n"},
		{"no comment", "bazelisk test //a/...\n", "bazelisk test //a/... \n\n"},
	} {
		if got := stripShellComments(tc.in); got != tc.want {
			t.Errorf("%s: stripShellComments(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// unracedSubtractions returns the patterns ci.yml's race lane subtracts that no
// nightly workflow races, alongside every pattern the nightly lanes DO race.
func unracedSubtractions(t *testing.T, dir string) (unraced, nightly []string) {
	t.Helper()

	prScopes := raceScopes(t, filepath.Join(dir, "ci.yml"))
	if len(prScopes) == 0 {
		t.Fatal("no race scope found in ci.yml — either the race lane is gone or this gate stopped reading it; both need a human")
	}

	var subtracted []string
	for _, s := range prScopes {
		for _, tok := range strings.Fields(s) {
			if strings.HasPrefix(tok, "-//") {
				subtracted = append(subtracted, strings.TrimPrefix(tok, "-"))
			}
		}
	}
	if len(subtracted) == 0 {
		// Nothing subtracted is a legitimate state — it means the PR lane
		// carries everything. Say so rather than passing silently, so a run
		// that stopped parsing the scopes cannot look like this one.
		t.Log("PR race lane subtracts nothing; nothing to reconcile against nightly")
		return nil, nil
	}

	nightly = nightlyRacedPatterns(t, dir)
	for _, pat := range subtracted {
		if !coveredBy(pat, nightly) {
			unraced = append(unraced, pat)
		}
	}
	return unraced, nightly
}

// A negative target pattern must appear after the end-of-options marker.
//
// Bazel rejects `bazel test //a/... -//a/b/...` outright ("Invalid options
// syntax: -//a/b/..."), so an exclusion written without a preceding `--` does
// not narrow the lane — it breaks the job. Cheap to pin, and it costs a CI
// round trip to rediscover.
func TestRaceLaneNegativePatternsFollowEndOfOptions(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(workflowDir(t), "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	matched := 0
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "$RACE ") {
			continue
		}
		matched++
		if !strings.Contains(trimmed, " -- ") {
			t.Errorf("race invocation passes its scope without an end-of-options marker, so a negative pattern in that scope would fail the job:\n  %s", trimmed)
		}
	}
	// Zero matches is the failure mode this check is most likely to die of: it
	// recognises today's `$RACE <scope>` shape, and a rewrite that calls
	// `bazelisk test` directly would leave it inspecting nothing and passing.
	// A gate that cannot find its subject has not verified it.
	if matched == 0 {
		t.Fatal("no `$RACE ` invocation found in ci.yml — the race lane was rewritten out of the shape this check recognises, so it verified nothing; re-point it at however the scope is passed now")
	}
}

// workflowDir resolves the way the other workflow-reading gates do. The walk-up
// to MODULE.bazel lands in the runfiles tree under Bazel, and .github is not a
// declared data dep there; sourceTreeRoot resolves the staged MODULE.bazel
// symlink back to the real checkout, which is where the workflows live.
func workflowDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(sourceTreeRoot(t), ".github", "workflows")
}

// raceScopes returns the value of every *_SCOPE assignment in ci.yml's race job.
func raceScopes(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	job, ok := wf.Jobs["race"]
	if !ok {
		t.Fatalf("%s has no `race` job", path)
	}
	var out []string
	for _, step := range job.Steps {
		for _, line := range strings.Split(step.Run, "\n") {
			line = strings.TrimSpace(line)
			name, value, found := strings.Cut(line, "=")
			if !found || !strings.HasSuffix(name, "_SCOPE") {
				continue
			}
			out = append(out, strings.Trim(value, `"`))
		}
	}
	return out
}

// nightlyRacedPatterns collects every target pattern passed to a `bazel test`
// carrying --@rules_go//go/config:race in a nightly-*.yml workflow.
func nightlyRacedPatterns(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflow dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "nightly-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var wf workflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				// Comments are stripped BEFORE both checks: a commented-out
				// invocation neither races anything nor names anything raced,
				// and counting either would let the gate certify coverage that
				// the shell will never execute.
				script := stripShellComments(step.Run)
				if !strings.Contains(script, "go/config:race") {
					continue
				}
				for _, tok := range strings.Fields(script) {
					if strings.HasPrefix(tok, "//") {
						out = append(out, tok)
					}
				}
			}
		}
	}
	return out
}

// stripShellComments drops `#`-to-end-of-line content from a shell snippet.
//
// Only a token that BEGINS with `#` opens a comment, which is also the shell's
// own rule for an unquoted `#`: `--flag=a#b` keeps its `#`, and so would any
// future flag value carrying one. This is deliberately not a shell parser — it
// does not know about quoting or here-documents — but it is exact on the shape
// that matters, a note appended to or interleaved with a bazel invocation.
func stripShellComments(script string) string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		for _, tok := range strings.Fields(line) {
			if strings.HasPrefix(tok, "#") {
				break
			}
			b.WriteString(tok)
			b.WriteByte(' ')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// coveredBy reports whether some pattern in have covers want. Both are Bazel
// target patterns; a `//a/b/...` recursive pattern covers anything under //a/b.
func coveredBy(want string, have []string) bool {
	wantPkg := strings.TrimSuffix(strings.TrimSuffix(want, "..."), "/")
	for _, h := range have {
		if h == want {
			return true
		}
		if !strings.HasSuffix(h, "...") {
			continue
		}
		havePkg := strings.TrimSuffix(strings.TrimSuffix(h, "..."), "/")
		if wantPkg == havePkg || strings.HasPrefix(wantPkg+"/", havePkg+"/") {
			return true
		}
	}
	return false
}
