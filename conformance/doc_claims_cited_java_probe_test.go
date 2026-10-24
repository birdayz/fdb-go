//go:build bazelrunfiles

package conformance_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/plandiff/seedcorpus"
)

// docClaimCitation is a backticked multi-word name in a document; a DSN key is
// not a citation.
var docClaimCitation = regexp.MustCompile("(DSN )?`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")

// TestDocClaimsCited checks that SQL_CONFORMANCE.md and DIVERGENCES.md cite every
// DocClaimsJavaProbe case, and cite no multi-word name that is not a case or a
// cross-engine corpus entry.
func TestDocClaimsCited(t *testing.T) {
	t.Parallel()
	divergences := readRepoFile(t, "DIVERGENCES.md")
	// DIVERGENCES.md cites cases in the Go-only extensions table and the Java
	// upstream bugs rows above the signed-zero row.
	section := func(from, to string) string {
		begin, end := strings.Index(divergences, from), strings.Index(divergences, to)
		if begin < 0 || end <= begin {
			t.Fatalf("DIVERGENCES.md has no section from %q to %q", from, to)
		}
		return divergences[begin:end]
	}
	cited := readRepoFile(t, "SQL_CONFORMANCE.md") + section("## Go-Only Extensions", "### Go-only SQLSTATEs") +
		section("## Java Upstream Bugs", "| Signed-zero comparison")

	known := map[string]bool{}
	for _, c := range docClaimCases {
		if known[c.name] {
			t.Errorf("duplicate case %s", c.name)
		}
		known[c.name] = true
		if !strings.Contains(cited, "`"+c.name+"`") {
			t.Errorf("case %s is cited by neither document", c.name)
		}
	}
	for _, q := range seedcorpus.SeedRunCorpus() {
		known[q.Name] = true
	}
	for _, m := range docClaimCitation.FindAllStringSubmatch(cited, -1) {
		if m[1] == "" && !known[m[2]] {
			t.Errorf("the documents cite %s, which is neither a case nor a cross-engine entry", m[2])
		}
	}
}

// readRepoFile reads rel from the nearest directory above the working
// directory that has it.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err == nil {
			return string(raw)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above the working directory", rel)
		}
		dir = parent
	}
}
