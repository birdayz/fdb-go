package docscheck

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The `.Field`-decides ratchet is quoted OUT of `knownFieldDecisionDebt` into the
// production-status authority page, and until this file existed nothing carried the
// number across that boundary.
//
// The gap was not theoretical and it was not small. `road-to-prod.md` lists the ratchet
// in its drift-guard table with the guarantee column reading *"Yes — TestFieldNameNeverDecides
// + TestFieldDebtBucketsArePartition"*. Neither test reads `road-to-prod.md`. Both check the
// list against ITSELF: the entries against the group headers, the headers against the entries.
// That is a real property and it is the wrong boundary — it proves the ratchet is internally
// consistent, and says nothing whatever about the figure the status page prints from it.
//
// So the page could drift, and did. It stated a total of 52 across four separate claims while
// the list held 53, because a second `boundary` entry arrived with the struct-type work and no
// mechanism existed to notice. A stale number is survivable; a stale number sitting under a
// column that says "drift-guarded: Yes" is the fake-green shape the nightly-safety-net audit
// existed to end, one level up and in the document a prod adopter is handed first.
//
// What closes it is the direction of authority, stated once: `knownFieldDecisionDebt` is the
// count, the page QUOTES it, and a quote that disagrees with its source fails the build. The
// page's own numbers can then be trusted for the reason the page claims they can.
//
// Deliberately NOT extended to `rfcs/197-column-identity-is-an-ordinal.md`. That document
// fixes its numbers on purpose — it says so in the text, so its per-item paragraphs can be
// read against the sizes they were planned from — and gating a plan's historical arithmetic
// against today's list would force the plan to be rewritten every time the list moves, which
// destroys exactly the record it is keeping. Currency is claimed by the status page alone, and
// it is the status page alone that is held to it.

// fieldDebtBucketRow matches one row of the per-bucket residual table on the status page:
// `| dotted | 6 | **14** | The WRITERS are resolved; ... |`. Column 2 is the audit-day figure
// (history — deliberately unchecked, it is a measurement of a tree that no longer exists) and
// column 3 is the CURRENT count, which is the one that has to be true.
var fieldDebtBucketRow = regexp.MustCompile(
	`(?m)^\|\s*(boundary|escape|contract|dotted|name-keyed|translator|harness)\s*\|\s*\d+\s*\|\s*\*\*(\d+)\*\*\s*\|`)

// fieldDebtTotalClaims are the phrasings on the status page that assert the CURRENT size of
// the list. Each is anchored on enough surrounding text to be unambiguous, and each captures
// the number in group 1.
//
// They are split by whether the page is REQUIRED to carry them, and the split is the point:
//
//   - the two required claims define the contract (the table exists and sums to a stated
//     total). If a revision drops them, the page has stopped quoting the ratchet and the test
//     says so rather than passing vacuously — a guard that goes green when the thing it guards
//     is deleted is not a guard.
//   - the optional claims are prose that a future revision may legitimately rewrite away (the
//     trajectory sentence, the summary rows). While they are there they must be right; they
//     are not required to be there.
var fieldDebtTotalClaims = []struct {
	name     string
	pattern  *regexp.Regexp
	required bool
}{
	{
		name:     "the per-bucket table's stated sum",
		pattern:  regexp.MustCompile(`partition, so they sum to the list: \*\*(\d+)\*\*`),
		required: true,
	},
	{
		name:     "the drift-guard table's ratchet row",
		pattern:  regexp.MustCompile("`\\.Field`-decides ratchet \\(RFC-197\\) \\| \\*\\*(\\d+)\\*\\* sites"),
		required: true,
	},
	{
		name:     "the B4 blocking-item row",
		pattern:  regexp.MustCompile(`\*\*\d+ at inception → (\d+) now\*\*`),
		required: false,
	},
	{
		name:     "the measured-trajectory sentence's HEAD figure",
		pattern:  regexp.MustCompile(`→ \*\*(\d+)\*\* \(HEAD\)`),
		required: false,
	},
}

// TestStatusPageQuotesTheLiveFieldDebt holds the status page's `.Field`-ratchet numbers to the
// live `knownFieldDecisionDebt`, in both directions: per bucket and in total.
//
// The mutation that must red this test is the one that actually happened — a debt entry added
// or removed without the page being updated. Both directions matter. A page that lags a
// MIGRATION overstates the remaining work, which is merely wrong; a page that lags a newly
// VISIBLE site understates it, which is the direction that gets a release shipped.
func TestStatusPageQuotesTheLiveFieldDebt(t *testing.T) {
	t.Parallel()

	live, untagged := bucketCounts(knownFieldDecisionDebt)
	if len(untagged) > 0 {
		// TestFieldDebtBucketsArePartition owns this failure and reports it far better.
		// Bailing rather than duplicating it keeps one authority on the message.
		t.Fatalf("debt entries carry no bucket tag, so no per-bucket total is well-defined here; "+
			"TestFieldDebtBucketsArePartition is the authority on this failure:\n  %s",
			strings.Join(untagged, "\n  "))
	}

	var liveTotal int
	for _, n := range live {
		liveTotal += n
	}

	page := readDoc(t, repoRoot(t), productionStatusAuthority)

	// Per bucket first: a total can be right while two buckets are wrong in opposite
	// directions, and the per-bucket table is what the migration is actually planned against.
	rows := fieldDebtBucketRow.FindAllStringSubmatch(page, -1)
	stated := make(map[string]int, len(rows))
	for _, m := range rows {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("bucket %q: unparsable count %q in %s: %v", m[1], m[2], productionStatusAuthority, err)
		}
		if prev, dup := stated[m[1]]; dup {
			t.Errorf("%s states bucket %q twice (%d and %d) — two answers for one bucket is the "+
				"condition the partition test exists to prevent, one document up",
				productionStatusAuthority, m[1], prev, n)
		}
		stated[m[1]] = n
	}

	// Iterate the canonical partition, not the live map's keys. A MIGRATED bucket has no
	// entries and so does not appear in `live` at all — reading the map's keys would stop
	// checking a bucket at the exact moment it reached zero, and a row claiming a zero that
	// nothing verifies is how a bucket comes back without the page noticing.
	for _, b := range fieldDebtBuckets {
		got, ok := stated[b]
		if !ok {
			t.Errorf("%s's per-bucket residual table has no row for bucket %q (live count %d) — "+
				"every bucket is quoted, including the ones at zero, or the table's total cannot "+
				"be read as a partition", productionStatusAuthority, b, live[b])
			continue
		}
		if got != live[b] {
			t.Errorf("bucket %q: %s says %d, knownFieldDecisionDebt holds %d. The debt list is the "+
				"authority and the page quotes it — update the page's row, never the list, to agree",
				b, productionStatusAuthority, got, live[b])
		}
	}

	// Then every claim of the TOTAL. These are separate sentences in separate sections, and
	// each one is read by someone who never reaches the others.
	for _, claim := range fieldDebtTotalClaims {
		m := claim.pattern.FindStringSubmatch(page)
		if m == nil {
			if claim.required {
				t.Errorf("%s no longer carries %s (pattern %s). It is REQUIRED: without it the page "+
					"states no current total for the `.Field` ratchet, and this guard would pass by "+
					"having nothing left to check",
					productionStatusAuthority, claim.name, claim.pattern)
			}
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s: unparsable count %q: %v", claim.name, m[1], err)
		}
		if n != liveTotal {
			t.Errorf("%s says %d, knownFieldDecisionDebt holds %d (%s)",
				claim.name, n, liveTotal, describeBucketTotals(live))
		}
	}
}

// describeBucketTotals renders the live per-bucket breakdown for a failure message, so the
// person fixing the page is told what to write instead of only that it is wrong.
func describeBucketTotals(live map[string]int) string {
	parts := make([]string, 0, len(fieldDebtBuckets))
	for _, b := range fieldDebtBuckets {
		parts = append(parts, fmt.Sprintf("%s %d", b, live[b]))
	}
	return strings.Join(parts, ", ")
}
