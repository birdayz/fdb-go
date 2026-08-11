package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// RFC-197 quotes the field-name debt census as migration arithmetic, and for a
// long while nothing checked those copies. They rotted, in three distinct ways
// at once, while `TestFieldDebtBucketsArePartition` — which checks the group
// headers INSIDE knownFieldDecisionDebt — stayed green throughout:
//
//   - the totals disagreed with the instrument (the RFC claimed 52 escape sites
//     over 34 authorities; the instrument measured 44 over 33);
//   - the RFC's own per-bucket escape numbers summed to 43, not the 52 the same
//     paragraph stated, so the document contradicted itself;
//   - its largest named concentration, `AggregateResultColumnName` at "6 of 52",
//     had retired to ZERO entries without the sentence moving.
//
// Two durable homes holding one fact is worse than one home, because the stale
// copy is indistinguishable from the live one at the point somebody plans from
// it. The fix is not to delete the RFC's numbers — a migration RFC without sizes
// is not usable — but to make them the SAME fact: the RFC carries the census as
// a marked table, and this gate fails the build when it drifts from what
// `knownFieldDecisionDebt` actually holds.
//
// THE FIRST VERSION OF THIS GATE CHECKED ONLY THE TABLES, AND THAT WAS NOT
// ENOUGH. The `## Order` section's PROSE was a third ungated copy: reverting its
// lead sentence to `34 AUTHORITIES (52 escape sites)` left the whole suite green,
// three lines below a paragraph asserting there were now only two homes for the
// fact. Gating a third copy would have been the wrong repair — a number that
// exists in one place cannot disagree with itself — so the prose counts were
// DELETED and `TestFieldDebtRFCOrderProseStatesNoCounts` keeps them out.
//
// The gate is deliberately two-directional throughout. An entry missing from a
// table is as much a failure as a wrong number, because a silently absent bucket
// would let the sum still tie out while the RFC under-reported the work.
//
// Every decision below is a PURE function over explicit state so that
// `TestFieldDebtCensusGateArms` can drive each arm directly. The corpus reading
// exercises only the arms the live document happens to reach — which was two of
// nine — and an untested arm's first real firing reads as a finding rather than
// as a branch nobody had run.

const (
	fieldDebtRFCPath = "rfcs/197-column-identity-is-an-ordinal.md"

	// The markers the RFC carries. They are HTML comments so they render as
	// nothing, and they name this test so a reader who changes a table knows
	// what will fail.
	fieldDebtCensusMarker        = "<!-- FIELD-DEBT-CENSUS -->"
	fieldDebtConcentrationMarker = "<!-- FIELD-DEBT-CONCENTRATION -->"

	// Any authority carrying at least this many escapes must be listed in the
	// concentration table. Without this the table could go stale in the other
	// direction — a NEW concentration appearing and never being written down —
	// which the per-row equality check alone cannot see.
	fieldDebtConcentrationFloor = 3

	// Below this a file cannot be the RFC, so checking a census against it would
	// be vacuous rather than lenient.
	fieldDebtRFCMinBytes = 1000
)

// fieldDebtAuthorityFunc reduces an authority key ("path/to/file.go # FuncName")
// to the declaration name the RFC's tables use.
func fieldDebtAuthorityFunc(authority string) string {
	parts := strings.Split(authority, " # ")
	return strings.TrimSpace(parts[len(parts)-1])
}

// parseFieldDebtTable returns the rows of the first markdown table following
// marker, separator rows dropped. The bool reports whether a table was found at
// all — an absent marker must fail loudly rather than scan an empty set.
func parseFieldDebtTable(src, marker string) ([][]string, bool) {
	i := strings.Index(src, marker)
	if i < 0 {
		return nil, false
	}
	var rows [][]string
	started := false
	for _, ln := range strings.Split(src[i+len(marker):], "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "|") {
			if started {
				break // the table ended
			}
			continue // prose between the marker and the table
		}
		started = true
		cells := strings.Split(strings.Trim(t, "|"), "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(strings.Trim(strings.TrimSpace(cells[j]), "`"))
		}
		if len(cells) == 0 || strings.HasPrefix(cells[0], "---") {
			continue
		}
		rows = append(rows, cells)
	}
	return rows, started
}

// rfcSourceProblem validates the file the census is read from. Returns "" when
// the source is usable.
func rfcSourceProblem(src []byte, err error) string {
	if err != nil {
		return fmt.Sprintf("reading %s: %v — the RFC is the durable home for this "+
			"census, so an unreadable file makes the gate vacuous rather than lenient",
			fieldDebtRFCPath, err)
	}
	if len(src) < fieldDebtRFCMinBytes {
		return fmt.Sprintf("%s is %d bytes — too small to be the RFC; refusing to check "+
			"a census against a file that cannot contain one", fieldDebtRFCPath, len(src))
	}
	return ""
}

// tablePresenceProblem covers the two ways a table can fail to exist: no marker,
// or a marker over something too short to be a census. Returns "" when usable.
func tablePresenceProblem(rows [][]string, found bool, marker string) string {
	if !found {
		return fmt.Sprintf("no %s marker in %s — the marked table is how the RFC's "+
			"numbers stay true, and without it this gate would pass while the prose drifted",
			marker, fieldDebtRFCPath)
	}
	if len(rows) < 2 {
		return fmt.Sprintf("the table under %s parsed %d row(s); a green from an empty "+
			"table is exactly the false pass this gate exists to prevent", marker, len(rows))
	}
	return ""
}

// checkCensusTable holds the per-bucket table to the instrument. Pure over
// explicit state so every arm is drivable.
func checkCensusTable(rows [][]string, escapes, authorities map[string]int, distinctAuthorities, totalEscapes int) []string {
	var problems []string
	claimed := map[string]bool{}
	sawTotal := false

	for _, r := range rows {
		if len(r) < 3 {
			problems = append(problems, fmt.Sprintf(
				"census row %q has %d cell(s), want 3 (bucket | authorities | escapes)", r, len(r)))
			continue
		}
		bucket := r[0]
		if strings.EqualFold(bucket, "bucket") {
			continue // header
		}
		wantAuth, err1 := strconv.Atoi(r[1])
		wantEsc, err2 := strconv.Atoi(r[2])
		if err1 != nil || err2 != nil {
			problems = append(problems, fmt.Sprintf(
				"census row %q: non-numeric counts (%v / %v)", r, err1, err2))
			continue
		}
		if strings.EqualFold(bucket, "TOTAL") {
			sawTotal = true
			if wantAuth != distinctAuthorities || wantEsc != totalEscapes {
				problems = append(problems, fmt.Sprintf(
					"%s TOTAL row claims %d authorities / %d escape sites; the instrument "+
						"measures %d / %d. This is the number the RFC leads with and the one "+
						"people plan from.",
					fieldDebtRFCPath, wantAuth, wantEsc, distinctAuthorities, totalEscapes))
			}
			continue
		}
		claimed[bucket] = true
		if got := authorities[bucket]; got != wantAuth {
			problems = append(problems, fmt.Sprintf(
				"%s claims bucket %q has %d authorities; the instrument measures %d",
				fieldDebtRFCPath, bucket, wantAuth, got))
		}
		if got := escapes[bucket]; got != wantEsc {
			problems = append(problems, fmt.Sprintf(
				"%s claims bucket %q has %d escape sites; the instrument measures %d",
				fieldDebtRFCPath, bucket, wantEsc, got))
		}
	}

	if !sawTotal {
		problems = append(problems, fmt.Sprintf(
			"the census table in %s has no TOTAL row. The per-bucket rows can each be "+
				"right while the stated total is wrong — that is precisely how this section "+
				"rotted before (its own per-bucket numbers summed to 43 against a claimed 52).",
			fieldDebtRFCPath))
	}

	var missing []string
	for b, n := range escapes {
		if !claimed[b] {
			missing = append(missing, fmt.Sprintf("%s (%d authorities, %d escapes)",
				b, authorities[b], n))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d bucket(s) carry debt and are absent from %s's census table: %s. "+
				"An omitted bucket under-reports the work while every listed row still ties out.",
			len(missing), fieldDebtRFCPath, strings.Join(missing, "; ")))
	}
	return problems
}

// checkConcentrationTable holds the "which authorities carry several escapes"
// table to the instrument, in both directions.
func checkConcentrationTable(rows [][]string, actual map[string]int, floor int) []string {
	var problems []string
	listed := map[string]bool{}

	for _, r := range rows {
		if len(r) < 2 {
			problems = append(problems, fmt.Sprintf(
				"concentration row %q has %d cell(s), want 2 (authority | escapes)", r, len(r)))
			continue
		}
		name := r[0]
		if strings.EqualFold(name, "authority") {
			continue // header
		}
		want, err := strconv.Atoi(r[1])
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"concentration row %q: non-numeric escape count: %v", r, err))
			continue
		}
		listed[name] = true
		got := actual[name]
		switch {
		case got == want:
		case got == 0:
			problems = append(problems, fmt.Sprintf(
				"%s lists %q as carrying %d escape site(s); it carries NONE — the authority "+
					"has RETIRED. Remove the row; a retired declaration left in a concentration "+
					"table is the exact rot this gate was added for.",
				fieldDebtRFCPath, name, want))
		default:
			problems = append(problems, fmt.Sprintf(
				"%s lists %q at %d escape site(s); the instrument measures %d",
				fieldDebtRFCPath, name, want, got))
		}
	}

	var unlisted []string
	for name, n := range actual {
		if n >= floor && !listed[name] {
			unlisted = append(unlisted, fmt.Sprintf("%s (%d escapes)", name, n))
		}
	}
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d declaration(s) carry >= %d escape sites and are absent from %s's "+
				"concentration table: %s. The table's job is to say where the work is "+
				"concentrated, so an unlisted concentration makes it wrong by omission.",
			len(unlisted), floor, fieldDebtRFCPath, strings.Join(unlisted, "; ")))
	}
	return problems
}

// orderSectionOf returns the `## Order` section body.
func orderSectionOf(src string) (string, bool) {
	i := strings.Index(src, "\n## Order\n")
	if i < 0 {
		return "", false
	}
	rest := src[i+len("\n## Order\n"):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}

// THE POLARITY HERE IS THE WHOLE DESIGN, and it is deny-by-default because the
// allowlist version failed.
//
// The first version of this gate carried seven regexes naming the count
// phrasings its author had thought of — `N authorities`, `N escape sites`,
// `stays at N`, and so on. That is a statement of intent, not an invariant: it
// can only ever catch shapes somebody anticipated. It missed four live claims in
// the very section it was built to protect, including a bucket size off by one
// against the table three screens above it, and a phrasing (`6 escapes`) the
// document already used one line below the concentration table.
//
// So: any BARE INTEGER in `## Order` prose is a violation unless it matches one
// of the closed exemptions below, and count-shaped word-numbers get their own
// check. The exemptions are non-census forms the section legitimately needs —
// source citations, RFC/CQ references, markdown list markers. Read the list
// itself rather than this sentence: an earlier revision of this paragraph went
// on advertising a traffic-arrow exemption after that exemption was deleted, in
// the same commit that deleted it, which is the rot this gate is named for
// turned on the gate's own self-description. Adding an exemption is a deliberate
// act; failing to imagine a phrasing is not.
//
// THE SCOPE STOPS AT `## Order`, DELIBERATELY — this is a decision, not an
// oversight. `## Decision` opens by declaring its counts to be the sizes each
// bucket had WHEN ITS PARAGRAPH WAS DRAFTED, names `knownFieldDecisionDebt` as
// the authority, and states that where the two disagree the test is right. That
// is a frozen-at-drafting record with a named authority and a tie-break, so
// counts BELONG there and drift in them is not rot. Measured before deciding:
// widening this gate fires 162 times against the document's own declared
// convention (150 of them in `## Decision`), versus 0 in `## Order`. That 162 is
// the SECTIONWISE total — the sum over the document's `##` sections, which is
// how this gate reads a document. Scanning the file as one blob gives 169; the
// extra 7 live in the front matter before `## The problem` and belong to no
// section. The qualifier is stated because a bare "162" invites the next reader
// to re-derive 169 and conclude the record rotted.
//
// `## Order` is the section that claims to describe the CURRENT residual, which
// is why it — and only it — is held to direction-not-magnitude.

// fieldDebtProseNumericExemptions are the non-census numeric forms permitted in
// `## Order` prose. They are STRIPPED before the bare-integer scan, so anything
// left carrying a digit is a count claim.
var fieldDebtProseNumericExemptions = []*regexp.Regexp{
	// Source citations: `cascades_translator.go:3925`, `FieldValue.java:272-300`,
	// `GroupByExpression.java:754,758`, and bare `:161-167` continuations.
	regexp.MustCompile(`[\w./-]+\.(?:go|java|g4|md|proto):\d+(?:[-,:]\d+)*`),
	regexp.MustCompile(`:\d+(?:[-,]\d+)*`),
	// Design-doc references: RFC-197, CQ-53, `sec 4.4/4.5`.
	regexp.MustCompile(`\b(?:RFC|CQ)-\d+`),
	regexp.MustCompile(`(?i)\bsec\.?\s*\d+(?:\.\d+)*(?:/\d+(?:\.\d+)*)*`),
	// Markdown ordered-list markers at line start.
	regexp.MustCompile(`(?m)^\s*\d+\.\s`),
}

// NO ARROW EXEMPTION, deliberately. `21865 → 0` reads as a measurement, but
// `the translator bucket went 17 → 12` is the same syntax carrying a magnitude
// PAIR, and nothing distinguishes them mechanically — so exempting arrows would
// have re-opened the hole for every census claim willing to spell itself with an
// arrow. This section's rule is DIRECTION, not magnitude, and an arrow is
// magnitude twice over; measured traffic figures belong at the fix site or in a
// table. This exemption existed and was removed for exactly that reason.

// fieldDebtProseWordCount catches counts spelled as words in front of a census
// noun. It is deliberately noun-anchored: `two durable homes for one fact` is
// ordinary prose, while `four entries` and `two declarations` are census claims.
var fieldDebtProseWordCount = regexp.MustCompile(
	`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\s+` +
		// Intervening adjectives, because the count and its noun are not always
		// adjacent: `all eleven REMAINING sites` is the exact claim that shipped.
		`(?:[a-z][a-z-]*\s+){0,2}` +
		`(readers?|entries|entry|sites?|authorities|authority|escapes?|buckets?|declarations?|mints?|producers?)\b`)

// fieldDebtProseElidedTally catches the OTHER half of the word-spelled class:
// a tally whose noun is ELIDED because the surrounding sentence already supplied
// it. Noun-anchoring cannot see these, and they are not hypothetical — `The
// three that remain are each blocked on…` shipped as a live, wrong bucket size
// one bullet above a tally that had just been fixed, and `two more left by
// deletion as unreachable` shipped beside it.
//
// The trigger is a word-number followed by a relative clause or a bare
// quantifier rather than by a noun. `one of the two reversed edges` does not
// match (the `of` is followed by `the`, not by them/these/those), and ordinary
// prose like `two durable homes` is noun-anchored and handled above.
var fieldDebtProseElidedTally = regexp.MustCompile(
	`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\s+` +
		`(?:more\b|that\s|which\s|of\s+(?:them|these|those)\b|remain\b|left\b)`)

// An HTML comment SPAN, not a whole line. Skipping any line merely STARTING
// with `<!--` let `<!-- note --> the residual is 33 authorities` through intact:
// the marker lines this needs to ignore are the census markers, and those are
// comments end to end, so strip the comment and scan whatever follows it.
var fieldDebtHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// fieldDebtProseScannable returns the prose of a section — table rows, HTML
// comment spans and fenced code blocks removed — and reports whether a code
// fence was left OPEN at the end.
//
// That second return exists because an unbalanced fence is a silent kill switch:
// `inFence` toggles, so one stray ``` makes every line below it unscanned and
// the gate reports GREEN over a section it never read. That is the
// green-from-an-empty-set shape, inside the instrument built to prevent it, so
// it is surfaced as a failure rather than trusted to never happen.
//
// INLINE BACKTICKS ARE DELIBERATELY NOT STRIPPED. Backticks mark code, not
// quotation, so `the residual is 33 authorities` is a live claim wearing a
// costume; exempting them would reopen the hole by another door. Citations
// inside backticks are already covered by the numeric exemptions.
//
// Fence tracking spans lines. An earlier version's doc comment claimed exactly
// that while its code reset the state per line, so fence bodies passed straight
// through — harmless then (the section has no fences) but precisely the
// record-outlived-the-code failure this file exists to catch.
func fieldDebtProseScannable(s string) (prose string, unbalancedFence bool) {
	var b strings.Builder
	inFence := false
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		stripped := fieldDebtHTMLComment.ReplaceAllString(ln, " ")
		if strings.HasPrefix(strings.TrimSpace(stripped), "|") {
			continue
		}
		b.WriteString(stripped)
		b.WriteByte('\n')
	}
	return b.String(), inFence
}

var fieldDebtBareInteger = regexp.MustCompile(`\d+`)

// checkOrderProseHasNoCounts is the arm that closes the third-copy hole.
func checkOrderProseHasNoCounts(section string) []string {
	prose, unbalancedFence := fieldDebtProseScannable(section)
	for _, re := range fieldDebtProseNumericExemptions {
		prose = re.ReplaceAllString(prose, " ")
	}

	seen := map[string]bool{}
	var problems []string
	if unbalancedFence {
		problems = append(problems, "the `## Order` section ends inside an unclosed code "+
			"fence. Everything after the stray fence marker was NOT scanned, so a green "+
			"from this gate would be a green over an unread section — the exact "+
			"empty-set false pass it exists to prevent. Close the fence.")
	}
	add := func(kind, match, line string) {
		key := kind + "\x00" + match + "\x00" + line
		if seen[key] {
			return
		}
		seen[key] = true
		problems = append(problems, fmt.Sprintf(
			"`## Order` prose states a census count (%s): %q, in: %q. The tables are the "+
				"only home for these numbers — prose copies cannot be checked and have "+
				"already rotted. State the shape (GREW, REOPENED) or name the sites, and "+
				"let the table carry the magnitude. If this is a non-census number, add its "+
				"form to fieldDebtProseNumericExemptions deliberately.",
			kind, match, line))
	}

	for _, ln := range strings.Split(prose, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		for _, m := range fieldDebtBareInteger.FindAllString(ln, -1) {
			add("bare integer", m, trimmed)
		}
		for _, m := range fieldDebtProseWordCount.FindAllString(ln, -1) {
			add("word-spelled count", strings.TrimSpace(m), trimmed)
		}
		for _, m := range fieldDebtProseElidedTally.FindAllString(ln, -1) {
			add("elided-noun tally", strings.TrimSpace(m), trimmed)
		}
	}
	sort.Strings(problems)
	return problems
}

func readFieldDebtRFC(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(sourceTreeRoot(t), fieldDebtRFCPath))
	if problem := rfcSourceProblem(src, err); problem != "" {
		t.Fatal(problem)
	}
	return string(src)
}

// fieldDebtInstrument reads the live census off knownFieldDecisionDebt.
func fieldDebtInstrument(t *testing.T) (escapes, authorities map[string]int, distinct, total int) {
	t.Helper()
	escapes, untagged := bucketCounts(knownFieldDecisionDebt)
	if len(untagged) > 0 {
		t.Fatalf("%d untagged entry/entries — TestFieldDebtBucketsArePartition owns that "+
			"failure; this gate cannot size buckets until it passes", len(untagged))
	}
	authorities = bucketAuthorityCounts(knownFieldDecisionDebt)
	seen := map[string]struct{}{}
	for site := range knownFieldDecisionDebt {
		seen[fieldDecisionAuthorityOf(site)] = struct{}{}
	}
	return escapes, authorities, len(seen), len(knownFieldDecisionDebt)
}

func TestFieldDebtRFCCensusMatchesTheInstrument(t *testing.T) {
	t.Parallel()

	rows, found := parseFieldDebtTable(readFieldDebtRFC(t), fieldDebtCensusMarker)
	if problem := tablePresenceProblem(rows, found, fieldDebtCensusMarker); problem != "" {
		t.Fatal(problem)
	}
	escapes, authorities, distinct, total := fieldDebtInstrument(t)
	for _, p := range checkCensusTable(rows, escapes, authorities, distinct, total) {
		t.Error(p)
	}
	t.Logf("RFC-197 CENSUS GATE: %d table row(s) checked against %d entries over %d authorities",
		len(rows), total, distinct)
}

func TestFieldDebtRFCConcentrationMatchesTheInstrument(t *testing.T) {
	t.Parallel()

	rows, found := parseFieldDebtTable(readFieldDebtRFC(t), fieldDebtConcentrationMarker)
	if problem := tablePresenceProblem(rows, found, fieldDebtConcentrationMarker); problem != "" {
		t.Fatal(problem)
	}
	actual := map[string]int{}
	for site := range knownFieldDecisionDebt {
		actual[fieldDebtAuthorityFunc(fieldDecisionAuthorityOf(site))]++
	}
	for _, p := range checkConcentrationTable(rows, actual, fieldDebtConcentrationFloor) {
		t.Error(p)
	}
	t.Logf("RFC-197 CONCENTRATION GATE: %d table row(s) checked over %d distinct declarations",
		len(rows), len(actual))
}

// TestFieldDebtRFCOrderProseStatesNoCounts closes the third-copy hole directly:
// the `## Order` prose may describe DIRECTION but never magnitude.
func TestFieldDebtRFCOrderProseStatesNoCounts(t *testing.T) {
	t.Parallel()

	section, ok := orderSectionOf(readFieldDebtRFC(t))
	if !ok {
		t.Fatalf("no `## Order` section in %s — this gate would otherwise scan an empty "+
			"string and pass while the prose said anything it liked", fieldDebtRFCPath)
	}
	if len(section) < 500 {
		t.Fatalf("the `## Order` section is %d bytes — too short to be the real section, "+
			"so a clean result would prove nothing", len(section))
	}
	for _, p := range checkOrderProseHasNoCounts(section) {
		t.Error(p)
	}
	t.Logf("RFC-197 ORDER PROSE GATE: %d bytes scanned deny-by-default, %d numeric "+
		"exemption form(s) permitted",
		len(section), len(fieldDebtProseNumericExemptions))
}
