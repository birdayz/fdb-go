package yamsql

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FeatureMatrix generation. This produces FEATURE_MATRIX.md — the authoritative,
// exhaustive inventory of the SQL surface that is actually exercised by the
// yamsql conformance corpus. It is GENERATED from the corpus (one row per
// scenario file: name, #tests, and the scenario's own leading-comment
// description) so it cannot rot: a drift guard (TestFeatureMatrixUpToDate)
// fails the build if the committed file falls out of sync.
//
// Categorisation is purely NAME-based (ordered substring rules on the scenario
// name, never on the SQL text — engine feature detection must use the parse tree,
// but this is a doc grouping). Unmatched names fall to "Other", which stays
// visible so a new feature area is never silently mislabelled.

// FeatureScenario is one row of the generated matrix.
type FeatureScenario struct {
	Name        string
	Category    string
	Tests       int
	Description string
}

// categoryRule maps a scenario whose name contains Substr to Category. Rules are
// evaluated in order; the first match wins, so more specific buckets precede
// broader ones (e.g. "outer_join" → Joins before "join" anything else).
type categoryRule struct {
	substr   string
	category string
}

// categoryRules is an ordered, name-based bucketing. Deliberately conservative:
// anything unmatched lands in "Other" rather than being force-fit.
var categoryRules = []categoryRule{
	{"join", "Joins"},
	{"flatmap", "Joins"},
	{"nlj", "Joins"},
	{"exists", "Subqueries (EXISTS / IN / scalar)"},
	{"subquer", "Subqueries (EXISTS / IN / scalar)"},
	{"correlat", "Subqueries (EXISTS / IN / scalar)"},
	{"in_select", "Subqueries (EXISTS / IN / scalar)"},
	{"in_list", "Subqueries (EXISTS / IN / scalar)"},
	{"derived_table", "Subqueries (EXISTS / IN / scalar)"},
	{"cte", "CTEs"},
	{"union", "Set operations (UNION / INTERSECT / EXCEPT)"},
	{"intersect", "Set operations (UNION / INTERSECT / EXCEPT)"},
	{"except", "Set operations (UNION / INTERSECT / EXCEPT)"},
	{"aggregate", "Aggregates & GROUP BY"},
	{"group_by", "Aggregates & GROUP BY"},
	{"groupby", "Aggregates & GROUP BY"},
	{"having", "Aggregates & GROUP BY"},
	{"avg", "Aggregates & GROUP BY"},
	{"_sum", "Aggregates & GROUP BY"},
	{"count", "Aggregates & GROUP BY"},
	// "distinct_from" (IS [NOT] DISTINCT FROM) is a comparison predicate, NOT an
	// aggregate — must precede the broad "distinct" (COUNT(DISTINCT)) rule.
	{"distinct_from", "Predicates & WHERE"},
	{"distinct", "Aggregates & GROUP BY"},
	{"insert", "DML (INSERT / UPDATE / DELETE)"},
	{"update", "DML (INSERT / UPDATE / DELETE)"},
	{"delete", "DML (INSERT / UPDATE / DELETE)"},
	{"dml", "DML (INSERT / UPDATE / DELETE)"},
	{"upsert", "DML (INSERT / UPDATE / DELETE)"},
	{"order", "Ordering & pagination"},
	{"sort", "Ordering & pagination"},
	{"limit", "Ordering & pagination"},
	{"offset", "Ordering & pagination"},
	{"pagination", "Ordering & pagination"},
	{"continuation", "Ordering & pagination"},
	{"like", "Scalar functions & expressions"},
	{"case_", "Scalar functions & expressions"}, // CASE expressions; "case_" not "case" so it won't catch "..._edge_cases"
	{"coalesce", "Scalar functions & expressions"},
	{"nullif", "Scalar functions & expressions"},
	{"cast", "Scalar functions & expressions"},
	{"function", "Scalar functions & expressions"},
	{"upper", "Scalar functions & expressions"},
	{"lower", "Scalar functions & expressions"},
	{"arith", "Scalar functions & expressions"},
	{"concat", "Scalar functions & expressions"},
	{"expr", "Scalar functions & expressions"},
	{"null", "NULL handling"},
	{"boolean", "NULL handling & boolean logic"},
	{"covering", "Index usage"},
	{"index", "Index usage"},
	{"rank", "Index usage"},
	{"bitmap", "Index usage"},
	{"permuted", "Index usage"},
	{"vector", "Vector / K-NN"},
	{"knn", "Vector / K-NN"},
	{"uuid", "Types"},
	{"enum", "Types"},
	{"bytes", "Types"},
	{"decimal", "Types"},
	{"type", "Types"},
	{"pk", "Keys & primary keys"},
	{"primary_key", "Keys & primary keys"},
	{"error", "Error codes & validation"},
	{"violation", "Error codes & validation"},
	{"overflow", "Scalar functions & expressions"},
	{"numeric", "Scalar functions & expressions"},
	{"bitwise", "Scalar functions & expressions"},
	{"greatest", "Scalar functions & expressions"},
	{"least", "Scalar functions & expressions"},
	{"between", "Predicates & WHERE"},
	{"where", "Predicates & WHERE"},
	{"predicate", "Predicates & WHERE"},
	{"qualified_star", "Column resolution & aliasing"},
	{"qualifier", "Column resolution & aliasing"},
	{"alias", "Column resolution & aliasing"},
	{"ambiguous", "Column resolution & aliasing"},
	{"e2e", "End-to-end scenarios"},
}

// categoryOrder is the display order for known categories; "Other" is always last.
var categoryOrder = []string{
	"Aggregates & GROUP BY",
	"Joins",
	"Subqueries (EXISTS / IN / scalar)",
	"CTEs",
	"Set operations (UNION / INTERSECT / EXCEPT)",
	"DML (INSERT / UPDATE / DELETE)",
	"Ordering & pagination",
	"Scalar functions & expressions",
	"Predicates & WHERE",
	"Column resolution & aliasing",
	"NULL handling",
	"NULL handling & boolean logic",
	"Index usage",
	"Vector / K-NN",
	"Types",
	"Keys & primary keys",
	"Error codes & validation",
	"End-to-end scenarios",
	"Other",
}

func categoryFor(name string) string {
	lower := strings.ToLower(name)
	// Substring match, first rule wins. Rules are written to avoid the cross-feature
	// false matches a codex review found: the over-broad "with_" rule is gone (CTEs
	// match on "cte"), "distinct_from" (a comparison predicate) precedes "distinct"
	// (the COUNT(DISTINCT) aggregate), and "case_" (with the trailing underscore)
	// matches CASE-expression scenarios without catching "..._edge_cases".
	for _, r := range categoryRules {
		if strings.Contains(lower, r.substr) {
			return r.category
		}
	}
	return "Other"
}

// ParseFeatureScenarios reads every *.yaml under dir and returns one
// FeatureScenario per file, sorted by name.
func ParseFeatureScenarios(dir string) ([]FeatureScenario, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no scenarios found under %s", dir)
	}
	sort.Strings(matches)

	out := make([]FeatureScenario, 0, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		// Parse tolerantly: the matrix only needs name + test count, NOT the full
		// scenario validation Load() does (one corpus file uses `schema:` instead
		// of `schema_template:` and would fail Load — it still belongs in the
		// inventory).
		var s Scenario
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if s.Name == "" {
			s.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		}
		out = append(out, FeatureScenario{
			Name:        s.Name,
			Category:    categoryFor(s.Name),
			Tests:       len(s.Tests),
			Description: extractDescription(raw, s.Name),
		})
	}
	return out, nil
}

// extractDescription pulls a one-line "what it pins" summary from a scenario's
// leading `#` comment block. Format seen across the corpus is either
// `# <name>.yaml — <title>` (title on the first line) or `# <name>.yaml` followed
// by a prose description on subsequent comment lines. Returns "" when there is no
// usable comment.
func extractDescription(raw []byte, name string) string {
	var comments []string
	for _, ln := range strings.Split(string(raw), "\n") {
		t := strings.TrimRight(ln, "\r")
		trimmed := strings.TrimSpace(t)
		if strings.HasPrefix(trimmed, "#") {
			comments = append(comments, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
			continue
		}
		if trimmed == "" && len(comments) == 0 {
			continue // blank lines before the comment block
		}
		break // first content (non-comment) line ends the block
	}
	if len(comments) == 0 {
		return ""
	}

	// First comment line is typically "<name>.yaml[ <sep> <title>]". Strip the
	// filename token; if a title follows a separator, that's the description.
	first := comments[0]
	fileToken := name + ".yaml"
	if strings.HasPrefix(first, fileToken) {
		rest := strings.TrimSpace(strings.TrimPrefix(first, fileToken))
		rest = strings.TrimLeft(rest, "—-: ")
		rest = strings.TrimSpace(rest)
		if rest != "" {
			return cleanDescription(rest)
		}
		// Title was on its own line(s): use the next non-empty comment line.
		for _, c := range comments[1:] {
			if c != "" {
				return cleanDescription(c)
			}
		}
		return ""
	}
	// No filename prefix: the first comment line is the description.
	return cleanDescription(first)
}

// cleanDescription collapses whitespace, takes the first sentence, escapes the
// markdown table delimiter, and caps the length.
func cleanDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	// First sentence (greedy up to ". " — keeps abbreviations like "e.g." intact
	// only loosely, which is fine for a summary).
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	const max = 160
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

// RenderFeatureMatrix renders the markdown document from parsed scenarios.
func RenderFeatureMatrix(scenarios []FeatureScenario) string {
	byCat := map[string][]FeatureScenario{}
	totalTests := 0
	for _, s := range scenarios {
		byCat[s.Category] = append(byCat[s.Category], s)
		totalTests += s.Tests
	}

	// Order categories: known order first, then any unexpected ones alphabetically,
	// then "Other" last.
	known := map[string]bool{}
	for _, c := range categoryOrder {
		known[c] = true
	}
	var cats []string
	for _, c := range categoryOrder {
		if c == "Other" {
			continue
		}
		if _, ok := byCat[c]; ok {
			cats = append(cats, c)
		}
	}
	var extra []string
	for c := range byCat {
		if !known[c] {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	cats = append(cats, extra...)
	if _, ok := byCat["Other"]; ok {
		cats = append(cats, "Other")
	}

	var b strings.Builder
	b.WriteString("# SQL Feature Matrix\n\n")
	b.WriteString("<!-- GENERATED FILE — DO NOT EDIT BY HAND.\n")
	b.WriteString("     Regenerate with `just feature-matrix` (or `go run ./cmd/gen-feature-matrix`).\n")
	b.WriteString("     Source: pkg/relational/conformance/yamsql/testdata/*.yaml — the cross-engine\n")
	b.WriteString("     conformance corpus. A drift guard (TestFeatureMatrixUpToDate) fails CI if this\n")
	b.WriteString("     file is stale. -->\n\n")
	b.WriteString("This is the **authoritative, exhaustive inventory** of the SQL surface exercised by the\n")
	b.WriteString("yamsql conformance corpus — one row per scenario, generated directly from the corpus so\n")
	b.WriteString("it never drifts. For the curated high-level summary see the SQL section of `README.md`;\n")
	b.WriteString("for known gaps, Go-only extensions, and Java-divergence detail see `DIVERGENCES.md`.\n\n")
	fmt.Fprintf(&b, "**%d scenarios · %d query/assertion cases** across %d feature areas.\n\n",
		len(scenarios), totalTests, len(cats))

	// Summary table.
	b.WriteString("| Feature area | Scenarios | Cases |\n|---|--:|--:|\n")
	for _, c := range cats {
		ss := byCat[c]
		t := 0
		for _, s := range ss {
			t += s.Tests
		}
		fmt.Fprintf(&b, "| %s | %d | %d |\n", c, len(ss), t)
	}
	b.WriteString("\n")

	// Per-category detail.
	for _, c := range cats {
		ss := byCat[c]
		sort.Slice(ss, func(i, j int) bool { return ss[i].Name < ss[j].Name })
		fmt.Fprintf(&b, "## %s\n\n", c)
		b.WriteString("| Scenario | Cases | What it pins |\n|---|--:|---|\n")
		for _, s := range ss {
			desc := s.Description
			if desc == "" {
				desc = "—"
			}
			fmt.Fprintf(&b, "| `%s` | %d | %s |\n", s.Name, s.Tests, desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// GenerateFeatureMatrix parses the corpus under dir and renders the markdown.
func GenerateFeatureMatrix(dir string) (string, error) {
	scenarios, err := ParseFeatureScenarios(dir)
	if err != nil {
		return "", err
	}
	return RenderFeatureMatrix(scenarios), nil
}
