package docscheck

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const rfc238Path = "rfcs/238-a-qualifier-is-structure-not-punctuation.md"

// A CITE NAMING A BASENAME THE TREE HOLDS THREE OF IS NOT A CITE, and this is
// the half of a cite gate that is worth building. RFC-238 §7d measures the
// other half -- "does the cited line look like code" -- and rejects it: 918 of
// 2066 resolved cites repo-wide read weak, 644 of them deliberate cites into
// doc comments, so that check scores citation STYLE. Resolution is different.
// An ambiguous or dangling cite is wrong under every style.
//
// It is not hypothetical. §7d's own population was computed twice by a scratch
// checker that indexed Go files by BASENAME, so `metadata.go:1343-1345`
// resolved against whichever of the tree's three `metadata.go` files the walk
// met first, and the resulting classification was reported as a fact about
// this document. The two cites are now written with their directory and this
// test holds them there.
//
// SCOPE, stated as what it does NOT cover: only `rfcs/238-...md`. Repo-wide
// the same run reports 341 ambiguous and 113 unresolved cites across
// `rfcs/*.md`, so a repo-wide form of this test is a real cleanup and not a
// one-line widening. It is not attempted here.
func TestRFC238CitesResolveUniquely(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	index := goFileSuffixIndex(t, root)

	cites := citesIn(t, filepath.Join(root, rfc238Path))
	if len(cites) < 40 {
		t.Fatalf("only %d distinct cites parsed out of %s; the scan lost the document "+
			"(check sourceTreeRoot/runfiles staging) rather than the document losing its cites — "+
			"a resolution test over an empty population passes for the wrong reason", len(cites), rfc238Path)
	}

	for _, c := range cites {
		cands := index[c.path]
		switch {
		case len(cands) == 0:
			t.Errorf("cite %q resolves to NO file in the tracked tree", c.text)
			continue
		case len(cands) > 1:
			t.Errorf("cite %q is AMBIGUOUS — %d tracked files match that path suffix (%s). "+
				"Qualify it with enough leading directory to name one file; a reader (and any "+
				"tool) otherwise resolves it to whichever the walk meets first",
				c.text, len(cands), strings.Join(cands, ", "))
			continue
		}
		n := lineCount(t, filepath.Join(root, cands[0]))
		if c.start < 1 || c.start > n {
			t.Errorf("cite %q names line %d of %s, which has %d lines", c.text, c.start, cands[0], n)
		}
		if c.end != 0 && c.end > n {
			t.Errorf("cite %q names end line %d of %s, which has %d lines", c.text, c.end, cands[0], n)
		}
	}
}

// The WEAK set §7d enumerates, held as a set rather than a count. A count says
// nothing about which cite moved; this fails naming the difference, and the
// failure means §7d's enumeration needs re-running — not that the cite is bad.
//
// A range counts as code when EITHER endpoint is code, which is the rule §7d
// states. Classifying by the FIRST line alone is what made the scratch checker
// report `pkg/recordlayer/metadata.go:1356-1358` and
// `record_types_property.go:37-51` as weak, and a draft of §7d then wrote a
// sentence explaining the two — a sentence that was false for a third cite.
func TestRFC238WeakCitesAreTheOnesSection7dNames(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	index := goFileSuffixIndex(t, root)

	want := []string{
		"colref.go:95",
		"derived_unnest.go:250",
		"full_unordered_scan.go:110-118",
		"pkg/recordlayer/metadata.go:1343-1351",
		"positional_row.go:7",
	}

	var got []string
	classified := 0
	for _, c := range citesIn(t, filepath.Join(root, rfc238Path)) {
		cands := index[c.path]
		if len(cands) != 1 {
			// Reported by TestRFC238CitesResolveUniquely; not this test's alarm.
			continue
		}
		lines := fileLines(t, filepath.Join(root, cands[0]))
		if c.start < 1 || c.start > len(lines) {
			continue
		}
		classified++
		class := classifyCiteLine(lines[c.start-1])
		if class != "code" && c.end > c.start && c.end <= len(lines) &&
			classifyCiteLine(lines[c.end-1]) == "code" {
			class = "code"
		}
		if class != "code" {
			got = append(got, c.text)
		}
	}
	if classified < 40 {
		t.Fatalf("only %d cites were classified; the population collapsed and an empty "+
			"weak set would otherwise read as agreement with §7d", classified)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("weak-cite set has moved.\n got: %v\nwant: %v\n"+
			"§7d enumerates this set and explains each entry; re-run the census and update the "+
			"section rather than editing this list alone", got, want)
	}
}

type rfcCite struct {
	text  string
	path  string
	start int
	end   int
}

func classifyCiteLine(line string) string {
	t := strings.TrimSpace(line)
	switch {
	case t == "":
		return "blank"
	case t == "}" || t == "{" || t == "})" || t == "},":
		return "brace"
	case strings.HasPrefix(t, "//"):
		return "comment"
	default:
		return "code"
	}
}

// goFileSuffixIndex maps every path SUFFIX at a directory boundary to the
// tracked files carrying it, so a cite disambiguates itself with as much
// leading directory as it chooses to write.
func goFileSuffixIndex(t *testing.T, root string) map[string][]string {
	t.Helper()
	files := trackedGoFiles(t, root)
	if len(files) < 1000 {
		t.Fatalf("only %d tracked Go files enumerated; the scan lost the tree, and every cite "+
			"would then resolve to nothing", len(files))
	}
	index := map[string][]string{}
	for _, rel := range files {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := range parts {
			suf := strings.Join(parts[i:], "/")
			index[suf] = append(index[suf], rel)
		}
	}
	return index
}

func citesIn(t *testing.T, path string) []rfcCite {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	seen := map[string]bool{}
	var out []rfcCite
	for _, line := range strings.Split(string(b), "\n") {
		for _, c := range scanCitesInLine(line) {
			if seen[c.text] {
				continue
			}
			seen[c.text] = true
			out = append(out, c)
		}
	}
	return out
}

func scanCitesInLine(s string) []rfcCite {
	var out []rfcCite
	for i := 0; i < len(s); i++ {
		j := strings.Index(s[i:], ".go:")
		if j < 0 {
			break
		}
		j += i
		k := j
		for k > 0 && isCitePathByte(s[k-1]) {
			k--
		}
		p := j + len(".go:")
		st := p
		for p < len(s) && s[p] >= '0' && s[p] <= '9' {
			p++
		}
		if p == st {
			i = j + len(".go")
			continue
		}
		start, _ := strconv.Atoi(s[st:p])
		end := 0
		if p < len(s) && s[p] == '-' {
			q := p + 1
			for q < len(s) && s[q] >= '0' && s[q] <= '9' {
				q++
			}
			if q > p+1 {
				end, _ = strconv.Atoi(s[p+1 : q])
				p = q
			}
		}
		out = append(out, rfcCite{
			text:  s[k:p],
			path:  strings.TrimPrefix(s[k:j]+".go", "/"),
			start: start,
			end:   end,
		})
		i = p - 1
	}
	return out
}

func isCitePathByte(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
		ch == '_' || ch == '/' || ch == '.' || ch == '-'
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(string(b), "\n")
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	return len(fileLines(t, path))
}
