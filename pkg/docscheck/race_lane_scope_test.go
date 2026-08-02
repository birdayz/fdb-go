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
// execution target alone runs 5000 scenarios holding ~5 GB RSS, and
// co-scheduling four such targets on a 7.6 GB box drove a 31s target
// (//pkg/relational/conformance/plandiff) into its 900s timeout. RFC-107 §3
// already stated the rule — Docker-backed conformance suites stay out of the
// PR race lane — and the wildcard had silently grown past it.
//
// Subtracting is only legitimate because the same targets keep running under
// the detector NIGHTLY. That second half is invisible at the subtraction site,
// which is exactly how it would get lost: someone tidies the nightly step,
// nothing in the PR lane changes, and the corpora quietly stop being raced
// anywhere while every gate stays green. This test is what makes that a build
// failure rather than a discovery six months later.
func TestRaceLaneSubtractionsStayCoveredNightly(t *testing.T) {
	t.Parallel()

	prScopes := raceScopes(t, filepath.Join(workflowDir(t), "ci.yml"))
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
		return
	}

	nightly := nightlyRacedPatterns(t)
	for _, pat := range subtracted {
		if !coveredBy(pat, nightly) {
			t.Errorf("ci.yml's race lane subtracts %s, but no nightly workflow races it (nightly race patterns: %v).\n"+
				"Excluding a scope from the PR lane is a latency decision; dropping it from every lane is a coverage decision. "+
				"Either put it back in the nightly race step or delete it from the PR lane's exclusion deliberately.",
				pat, nightly)
		}
	}
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
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "$RACE ") {
			continue
		}
		if !strings.Contains(trimmed, " -- ") {
			t.Errorf("race invocation passes its scope without an end-of-options marker, so a negative pattern in that scope would fail the job:\n  %s", trimmed)
		}
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
func nightlyRacedPatterns(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(workflowDir(t))
	if err != nil {
		t.Fatalf("read workflow dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "nightly-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workflowDir(t), e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var wf workflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				if !strings.Contains(step.Run, "go/config:race") {
					continue
				}
				for _, tok := range strings.Fields(step.Run) {
					if strings.HasPrefix(tok, "//") {
						out = append(out, tok)
					}
				}
			}
		}
	}
	return out
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
