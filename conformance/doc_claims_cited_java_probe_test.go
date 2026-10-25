//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javacorpus"
	"fdb.dev/pkg/relational/conformance/plandiff/seedcorpus"
)

// docCitation is a backticked lowercase name in a document; a DSN key is not a
// citation.
var docCitation = regexp.MustCompile("(DSN )?`([a-z][a-z0-9_]*)`")

// docWords are the backticked lowercase words the documents use as SQL or
// column names rather than as citations.
var docWords = map[string]bool{"a": true, "and": true, "explain": true, "ln": true, "or": true, "s": true, "v": true}

// TestDocClaimsCited prevents citations, support cells and pins from drifting
// independently when Java or the feature matrix changes.
func TestDocClaimsCited(t *testing.T) {
	t.Parallel()
	sqlConformance := readRepoFile(t, "SQL_CONFORMANCE.md")
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
	cited := sqlConformance + section("## Go-Only Extensions", "### Go-only SQLSTATEs") +
		section("## Java Upstream Bugs", "| Signed-zero comparison")

	names := make([]string, len(docClaimCases))
	for i, c := range docClaimCases {
		names[i] = c.name
	}
	corpus := map[string]bool{}
	for _, q := range seedcorpus.SeedRunCorpus() {
		corpus[q.Name] = true
	}
	problems := citationProblems(cited, names, corpus)
	problems = append(problems, pinProblems(names, docClaimWantJava, docClaimWantGo)...)
	cells, _ := matrixCellProblems(sqlConformance, docClaimWantJava, docClaimWantGo)
	problems = append(problems, cells...)
	for _, p := range problems {
		t.Error(p)
	}
}

// citationProblems checks that cited names every case and cites only cases,
// cross-engine corpus entries and docWords.
func citationProblems(cited string, cases []string, corpus map[string]bool) []string {
	var problems []string
	known := map[string]bool{}
	for _, name := range cases {
		if known[name] {
			problems = append(problems, "duplicate case "+name)
		}
		known[name] = true
		if !strings.Contains(cited, "`"+name+"`") {
			problems = append(problems, "case "+name+" is cited by neither document")
		}
	}
	matched := 0
	for _, m := range docCitation.FindAllStringSubmatch(cited, -1) {
		if m[1] != "" || docWords[m[2]] {
			continue
		}
		matched++
		if !known[m[2]] && !corpus[m[2]] {
			problems = append(problems, "the documents cite "+m[2]+", which is neither a case nor a cross-engine entry")
		}
	}
	if matched == 0 {
		problems = append(problems, "the documents cite nothing")
	}
	return problems
}

// pinProblems checks that every case has a Java pin and every pin names a case.
func pinProblems(cases []string, java, goPins map[string]string) []string {
	var problems []string
	known := map[string]bool{}
	for _, name := range cases {
		known[name] = true
		if _, ok := java[name]; !ok {
			problems = append(problems, "case "+name+" has no Java pin")
		}
	}
	for _, pins := range []map[string]string{java, goPins} {
		for name := range pins {
			if !known[name] {
				problems = append(problems, "pin "+name+" names no case")
			}
		}
	}
	return problems
}

// matrixCellProblems distinguishes correct answers from pinned wrong results,
// so merely accepting SQL cannot justify a feature's Y classification.
func matrixCellProblems(doc string, java, goPins map[string]string) ([]string, int) {
	isErr := func(answer string) bool { return strings.HasPrefix(answer, "ERROR ") }
	goOf := func(name string) string {
		if a, ok := goPins[name]; ok {
			return a
		}
		return java[name]
	}
	// stateOf is the SQLSTATE of an error pin: Java pins carry the exception
	// class first, Go pins do not.
	stateOf := func(answer string, field int) string {
		if f := strings.Fields(answer); isErr(answer) && len(f) > field {
			return f[field]
		}
		return ""
	}
	some := func(names []string, pred func(string) bool) bool {
		for _, n := range names {
			if pred(n) {
				return true
			}
		}
		return false
	}
	var problems []string
	checked := 0
	mode := ""
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "| Feature | Java | Go |"):
			mode = "feature"
			continue
		case strings.HasPrefix(line, "| Error | SQLSTATE | Java | Go |"):
			mode = "error"
			continue
		case !strings.HasPrefix(line, "|"):
			mode = ""
		}
		if mode == "" || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.ReplaceAll(line, `\|`, ""), "| "), " | ")
		var cited []string
		for _, m := range docCitation.FindAllStringSubmatch(line, -1) {
			if _, ok := java[m[2]]; ok {
				cited = append(cited, m[2])
			}
		}
		if len(cited) == 0 || len(cells) < 4 {
			continue
		}
		checked++
		bad := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("SQL_CONFORMANCE.md row %q: ", cells[0])+fmt.Sprintf(format, args...))
		}
		if mode == "feature" {
			javaCell, goCell := cells[1], cells[2]
			correct, wrong, rejected := 0, 0, 0
			for _, name := range cited {
				// Overflow/unsupported INSERT forms and a duplicate-free tree are
				// controls, not evidence for the surrounding feature's support.
				switch name {
				case "cast_overflow", "insert_select_column_list", "recursive_union_distinct":
					continue
				}
				switch {
				case isErr(java[name]):
					rejected++
				case java[name] != goOf(name):
					wrong++
				default:
					correct++
				}
			}
			// These rows cite Java's aggregate-index tests for the supported arm;
			// their probes exercise only the unsupported, unindexed shape.
			if cells[0] == "GROUP BY expression" || cells[0] == "ORDER BY aggregate" {
				correct++
			}
			wantJava := "N"
			if correct > 0 {
				wantJava = "Y"
				if wrong+rejected > 0 {
					wantJava = "P"
				}
			}
			if correct+wrong+rejected == 0 {
				bad("no feature-support case remains after excluding controls")
			} else if javaCell != wantJava {
				bad("Java is %s, want %s from %d correct, %d wrong-result and %d rejected cases", javaCell, wantJava, correct, wrong, rejected)
			}
			if (goCell == "Y" || goCell == "Ext") && !some(cited, func(n string) bool { return !isErr(goOf(n)) }) {
				bad("Go is %s but Go refuses every cited case %v", goCell, cited)
			}
			if goCell == "N" && !some(cited, func(n string) bool { return isErr(goOf(n)) }) {
				bad("Go is N but Go answers every cited case %v", cited)
			}
			continue
		}
		state, javaCell, goCell := cells[1], cells[2], cells[3]
		javaHas := func(n string) bool { return stateOf(java[n], 2) == state }
		javaOther := func(n string) bool { return isErr(java[n]) && !javaHas(n) }
		goHas := func(n string) bool { return stateOf(goOf(n), 1) == state }
		switch {
		case javaCell == "Y" && !some(cited, javaHas), javaCell == "N" && some(cited, javaHas),
			javaCell == "P" && !(some(cited, javaHas) && some(cited, javaOther)):
			bad("Java is %s for %s, against the pins of %v", javaCell, state, cited)
		}
		if goCell == "Y" && !some(cited, goHas) || goCell == "N" && some(cited, goHas) {
			bad("Go is %s for %s, against the pins of %v", goCell, state, cited)
		}
	}
	if checked == 0 {
		problems = append(problems, "no SQL_CONFORMANCE.md matrix row cites a case")
	}
	return problems, checked
}

func TestDocClaimSupportClassifications(t *testing.T) {
	t.Parallel()
	java := map[string]string{
		"works": `[] [[1]]`, "wrong": `[] [[2]]`, "refused": `ERROR X 0AF00 "no"`,
		"cast_overflow": `ERROR X 22F3H "overflow"`,
	}
	goPins := map[string]string{
		"wrong": `[] [[1]]`, "refused": `[] [[1]]`, "cast_overflow": `ERROR 22F3H "overflow"`,
	}
	for _, tc := range []struct {
		name, cell, cases, want string
	}{
		{"wrong rows", "N", "`wrong`", ""},
		{"wrong rows", "Y", "`wrong`", "Java is Y"},
		{"wrong rows", "P", "`wrong`", "Java is P"},
		{"mixed support", "P", "`works`, `refused`", ""},
		{"mixed support", "Y", "`works`, `refused`", "Java is Y"},
		{"mixed support", "N", "`works`, `refused`", "Java is N"},
		{"GROUP BY expression", "P", "`refused`", ""},
		{"GROUP BY expression", "Y", "`refused`", "Java is Y"},
		{"GROUP BY expression", "N", "`refused`", "Java is N"},
		{"CAST", "Y", "`works`, `cast_overflow`", ""},
		{"controls only", "N", "`cast_overflow`", "no feature-support case"},
		{"ORDER BY aggregate", "P", "`refused`", ""},
		{"ORDER BY aggregate", "Y", "`works`", ""},
	} {
		t.Run(tc.name+"/"+tc.cell, func(t *testing.T) {
			doc := "| Feature | Java | Go | ANSI | Notes |\n| " + tc.name + " | " + tc.cell + " | P | Y | " + tc.cases + " |"
			problems, rows := matrixCellProblems(doc, java, goPins)
			if rows != 1 {
				t.Fatalf("checked %d rows, want 1", rows)
			}
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("valid classification: %v", problems)
				}
			} else if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %v, want one containing %q", problems, tc.want)
			}
		})
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	path, err := javacorpus.FindAbove(rel)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDocClaimHelperArms(t *testing.T) {
	t.Parallel()
	want := func(problems []string, substrings ...string) {
		t.Helper()
		if len(problems) != len(substrings) {
			t.Errorf("got %d problems %q, want %d", len(problems), problems, len(substrings))
			return
		}
		for i, s := range substrings {
			if !strings.Contains(problems[i], s) {
				t.Errorf("problem %q lacks %q", problems[i], s)
			}
		}
	}

	corpus := map[string]bool{"corpus_entry": true}
	want(citationProblems("`one_case` `corpus_entry` `v` DSN `some_key`", []string{"one_case"}, corpus))
	want(citationProblems("`one_case` `typo_case`", []string{"one_case", "one_case", "other_case"}, corpus),
		"duplicate case one_case", "case other_case is cited", "cite typo_case")
	want(citationProblems("no citations, DSN `some_key`, `v`", nil, corpus), "cite nothing")

	java := map[string]string{"ok": `[] [[1]]`, "refused": `ERROR X 0AF00 "no"`, "wrong": `[] [[2]]`}
	goPins := map[string]string{"refused": `[] [[1]]`, "wrong": `[] [[1]]`}
	want(pinProblems([]string{"ok", "refused", "unpinned"}, java, map[string]string{"refused": `[] [[1]]`}),
		"unpinned has no Java pin", "pin wrong names no case")

	doc := strings.Join([]string{
		"| Feature | Java | Go | ANSI | Notes |",
		"|---|:---:|:---:|:---:|---|",
		"| good | Y | Y | Y | `ok` |",
		"| ext | N | Ext | Y | `refused`, \\| escaped |",
		"| wrong rows | N | Y | Y | `wrong` |",
		"| uncited | Y | N | Y | none |",
		"| bad java Y | Y | Ext | Y | `refused` |",
		"| bad java N | N | Y | Y | `ok` |",
		"| bad go N | N | N | Y | `refused` |",
		"",
		"| Error | SQLSTATE | Java | Go | Notes |",
		"|---|---|:---:|:---:|---|",
		"| good error | 0AF00 | Y | N | `refused` |",
		"| bad error | 42601 | Y | Y | `refused` |",
		"| bad partial | 0AF00 | P | N | `refused` |",
	}, "\n")
	problems, rows := matrixCellProblems(doc, java, goPins)
	if rows != 9 {
		t.Errorf("checked %d rows, want 9", rows)
	}
	want(problems, `"bad java Y": Java is Y`, `"bad java N": Java is N`, `"bad go N": Go is N`,
		`"bad error": Java is Y for 42601`, `"bad error": Go is Y for 42601`, `"bad partial": Java is P`)

	java["shared_refused"] = `ERROR X 0AF00 "no"`
	java["other_refused"] = `ERROR X 42601 "no"`
	goPins["shared_refused"] = `ERROR 0AF00 "no"`
	goPins["other_refused"] = `ERROR 42601 "no"`
	doc = strings.Join([]string{
		"| Feature | Java | Go | ANSI | Notes |",
		"| bad go Y | N | Y | Y | `shared_refused` |",
		"| bad go Ext | N | Ext | Y | `shared_refused` |",
		"| shared refusal | N | N | Y | `shared_refused` |",
		"",
		"| Error | SQLSTATE | Java | Go | Notes |",
		"| shared state | 0AF00 | Y | Y | `shared_refused` |",
		"| bad error Go N | 0AF00 | Y | N | `shared_refused` |",
		"| bad error Java N | 0AF00 | N | Y | `shared_refused` |",
		"| partial state | 0AF00 | P | Y | `shared_refused`, `other_refused` |",
	}, "\n")
	problems, rows = matrixCellProblems(doc, java, goPins)
	if rows != 7 {
		t.Errorf("checked %d rows, want 7", rows)
	}
	want(problems, `"bad go Y": Go is Y`, `"bad go Ext": Go is Ext`,
		`"bad error Go N": Go is N for 0AF00`, `"bad error Java N": Java is N for 0AF00`)
	problems, rows = matrixCellProblems("no matrix rows", java, goPins)
	if rows != 0 {
		t.Errorf("checked %d rows in an empty matrix, want 0", rows)
	}
	want(problems, "no SQL_CONFORMANCE.md matrix row cites a case")
}
