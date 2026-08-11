package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
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
// The gate is deliberately two-directional. An entry missing from the table is
// as much a failure as a wrong number, because a bucket silently absent would
// let the sum still tie out while the RFC under-reported the work.

const (
	fieldDebtRFCPath = "rfcs/197-column-identity-is-an-ordinal.md"

	// The markers the RFC carries. They are HTML comments so they render as
	// nothing, and they name this test so a reader who changes the table knows
	// what will fail.
	fieldDebtCensusMarker        = "<!-- FIELD-DEBT-CENSUS -->"
	fieldDebtConcentrationMarker = "<!-- FIELD-DEBT-CONCENTRATION -->"

	// Any authority carrying at least this many escapes must be listed in the
	// concentration table. Without this the table could go stale in the other
	// direction — a NEW concentration appearing and never being written down —
	// which the per-row equality check alone cannot see.
	fieldDebtConcentrationFloor = 3
)

// fieldDebtAuthorityFunc reduces an authority key ("path/to/file.go # FuncName")
// to the declaration name the RFC's prose uses.
func fieldDebtAuthorityFunc(authority string) string {
	parts := strings.Split(authority, " # ")
	return strings.TrimSpace(parts[len(parts)-1])
}

// parseFieldDebtTable returns the rows of the first markdown table following
// marker, header and separator rows dropped. The bool reports whether a table
// was found at all — an absent marker must fail loudly rather than scan an empty
// set, which is the whole failure mode this gate exists to end.
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

func readFieldDebtRFC(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sourceTreeRoot(t), fieldDebtRFCPath)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v — the RFC is the durable home for this census, "+
			"so an unreadable file makes the gate vacuous rather than lenient", fieldDebtRFCPath, err)
	}
	if len(src) < 1000 {
		t.Fatalf("%s is %d bytes — too small to be the RFC; refusing to check a census "+
			"against a file that cannot contain one", fieldDebtRFCPath, len(src))
	}
	return string(src)
}

// TestFieldDebtRFCCensusMatchesTheInstrument holds RFC-197's per-bucket table to
// what knownFieldDecisionDebt actually contains.
func TestFieldDebtRFCCensusMatchesTheInstrument(t *testing.T) {
	t.Parallel()

	src := readFieldDebtRFC(t)
	rows, found := parseFieldDebtTable(src, fieldDebtCensusMarker)
	if !found {
		t.Fatalf("no %s marker in %s — the census table is how the RFC's numbers stay "+
			"true, and without it this gate would pass while the prose drifted",
			fieldDebtCensusMarker, fieldDebtRFCPath)
	}
	if len(rows) < 2 {
		t.Fatalf("the census table under %s parsed %d row(s); a green from an empty "+
			"table is exactly the false pass this gate exists to prevent",
			fieldDebtCensusMarker, len(rows))
	}

	escapes, untagged := bucketCounts(knownFieldDecisionDebt)
	if len(untagged) > 0 {
		t.Fatalf("%d untagged entry/entries — TestFieldDebtBucketsArePartition owns that "+
			"failure; this gate cannot size buckets until it passes", len(untagged))
	}
	authorities := bucketAuthorityCounts(knownFieldDecisionDebt)
	distinct := map[string]struct{}{}
	for site := range knownFieldDecisionDebt {
		distinct[fieldDecisionAuthorityOf(site)] = struct{}{}
	}
	totalEscapes := len(knownFieldDecisionDebt)

	claimed := map[string]bool{}
	var sawTotal bool
	for _, r := range rows {
		if len(r) < 3 {
			t.Errorf("census row %q has %d cells, want 3 (bucket | authorities | escapes)", r, len(r))
			continue
		}
		bucket := r[0]
		if strings.EqualFold(bucket, "bucket") {
			continue // header
		}
		wantAuth, err1 := strconv.Atoi(r[1])
		wantEsc, err2 := strconv.Atoi(r[2])
		if err1 != nil || err2 != nil {
			t.Errorf("census row %q: non-numeric counts (%v / %v)", r, err1, err2)
			continue
		}
		if strings.EqualFold(bucket, "TOTAL") {
			sawTotal = true
			if wantAuth != len(distinct) || wantEsc != totalEscapes {
				t.Errorf("%s TOTAL row claims %d authorities / %d escape sites; the instrument "+
					"measures %d / %d.\n\nThis is the number the RFC leads with and the one "+
					"people plan from. Update the table in the same commit that moves the debt.",
					fieldDebtRFCPath, wantAuth, wantEsc, len(distinct), totalEscapes)
			}
			continue
		}
		claimed[bucket] = true
		if got := authorities[bucket]; got != wantAuth {
			t.Errorf("%s claims bucket %q has %d authorities; the instrument measures %d",
				fieldDebtRFCPath, bucket, wantAuth, got)
		}
		if got := escapes[bucket]; got != wantEsc {
			t.Errorf("%s claims bucket %q has %d escape sites; the instrument measures %d",
				fieldDebtRFCPath, bucket, wantEsc, got)
		}
	}
	if !sawTotal {
		t.Errorf("the census table has no TOTAL row. The per-bucket rows can each be right " +
			"while the stated total is wrong — that is precisely how this section rotted " +
			"before (its own per-bucket numbers summed to 43 against a claimed 52).")
	}

	// The other direction: a bucket the instrument reports and the table omits.
	var missing []string
	for b := range escapes {
		if !claimed[b] {
			missing = append(missing, fmt.Sprintf("%s (%d authorities, %d escapes)",
				b, authorities[b], escapes[b]))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d bucket(s) carry debt and are absent from %s's census table:\n  %s\n\n"+
			"An omitted bucket under-reports the work while every listed row still ties out.",
			len(missing), fieldDebtRFCPath, strings.Join(missing, "\n  "))
	}

	t.Logf("RFC-197 CENSUS GATE: %d bucket row(s) checked against %d entries over %d authorities",
		len(claimed), totalEscapes, len(distinct))
}

// TestFieldDebtRFCConcentrationMatchesTheInstrument holds the RFC's
// "four authorities carry N of the M" claim to the list.
//
// This is the arm that would have caught `AggregateResultColumnName`: the RFC
// named it as the single largest concentration at 6 escapes long after its last
// entry retired, and no per-bucket total moved when it went to zero.
func TestFieldDebtRFCConcentrationMatchesTheInstrument(t *testing.T) {
	t.Parallel()

	src := readFieldDebtRFC(t)
	rows, found := parseFieldDebtTable(src, fieldDebtConcentrationMarker)
	if !found {
		t.Fatalf("no %s marker in %s — without it the RFC's concentration claim is "+
			"unchecked prose, which is how it came to name a retired authority",
			fieldDebtConcentrationMarker, fieldDebtRFCPath)
	}
	if len(rows) < 2 {
		t.Fatalf("the concentration table under %s parsed %d row(s), which cannot be a "+
			"census", fieldDebtConcentrationMarker, len(rows))
	}

	// Actual escapes per declaration name.
	actual := map[string]int{}
	for site := range knownFieldDecisionDebt {
		actual[fieldDebtAuthorityFunc(fieldDecisionAuthorityOf(site))]++
	}

	listed := map[string]bool{}
	for _, r := range rows {
		if len(r) < 2 {
			t.Errorf("concentration row %q has %d cells, want 2 (authority | escapes)", r, len(r))
			continue
		}
		name := r[0]
		if strings.EqualFold(name, "authority") {
			continue // header
		}
		want, err := strconv.Atoi(r[1])
		if err != nil {
			t.Errorf("concentration row %q: non-numeric escape count: %v", r, err)
			continue
		}
		listed[name] = true
		got := actual[name]
		if got == want {
			continue
		}
		if got == 0 {
			t.Errorf("%s lists %q as carrying %d escape site(s); it carries NONE — the "+
				"authority has RETIRED.\n\nRemove the row. A retired declaration left in a "+
				"concentration table is the exact rot this gate was added for.",
				fieldDebtRFCPath, name, want)
			continue
		}
		t.Errorf("%s lists %q at %d escape site(s); the instrument measures %d",
			fieldDebtRFCPath, name, want, got)
	}

	// A NEW concentration must not be able to appear unlisted.
	var unlisted []string
	for name, n := range actual {
		if n >= fieldDebtConcentrationFloor && !listed[name] {
			unlisted = append(unlisted, fmt.Sprintf("%s (%d escapes)", name, n))
		}
	}
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Errorf("%d declaration(s) carry >= %d escape sites and are absent from %s's "+
			"concentration table:\n  %s\n\n"+
			"The table's job is to say where the work is concentrated, so an unlisted "+
			"concentration makes it wrong by omission rather than by arithmetic.",
			len(unlisted), fieldDebtConcentrationFloor, fieldDebtRFCPath,
			strings.Join(unlisted, "\n  "))
	}

	t.Logf("RFC-197 CONCENTRATION GATE: %d listed authority/ies checked over %d distinct declarations",
		len(listed), len(actual))
}
