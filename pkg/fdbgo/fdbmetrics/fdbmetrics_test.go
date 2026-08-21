package fdbmetrics

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
)

type fakeSource struct{ s client.ClientMetricsSnapshot }

func (f fakeSource) Metrics() client.ClientMetricsSnapshot { return f.s }

func TestHandler_TextExposition(t *testing.T) {
	t.Parallel()
	src := fakeSource{s: client.ClientMetricsSnapshot{
		TransactionsCommitStarted:        7,
		TransactionsCommitCompleted:      6,
		TransactionsNotCommitted:         3,
		TransactionsThrottled:            22,
		TransactionsMaybeCommitted:       21,
		TransactionReadVersionsCompleted: 42,
		GRVCacheHits:                     9,
		GRVInBandMaybeDelivered:          11,
		TransactionRetries:               4,
		ClientConnectionFailures:         2,
		CoordinatorChanges:               1,
		// Every counter gets a DISTINCT NON-ZERO value. That is the half still
		// load-bearing; the history of the other half is below.
		//
		// Each requirement was wrong here in turn:
		//
		//   - two counters shared the value 1, so crossing their getters passed;
		//   - one was asserted as 0, which every unset counter renders as, so it
		//     held whether its getter was wired or not;
		//   - the values were distinct but the matcher was then strings.Contains,
		//     and 1, 2 and 4 are PREFIXES of 11-18, 21-22 and 42 -- eleven
		//     shadowed pairs. A symmetric swap still failed on its other half,
		//     but a one-directional mis-wire into a shadowing counter passed:
		//     pointing coordinator_changes at TransactionsTooOld rendered 14, and
		//     "…_total 1" matched it.
		//
		// The matcher is whole-LINE membership NOW, which closed that family and
		// the line-start boundary with it. The expectations still carry a
		// trailing newline, but it is no longer load-bearing -- membership trims
		// it -- and it is kept only so the strings read as the exposition lines
		// they represent. Dropping one is a no-op; the terminal LF that the
		// suffix used to cover incidentally is asserted on its own, further down.
		TransactionsResourceConstrained:           12,
		TransactionsProcessBehind:                 13,
		TransactionsTooOld:                        14,
		TransactionsFutureVersions:                15,
		TransactionBatchReadVersionsCompleted:     16,
		TransactionDefaultReadVersionsCompleted:   17,
		TransactionImmediateReadVersionsCompleted: 18,
		ReadLatency:        client.LatencyStats{Count: 100, Sum: 1.5, Mean: 0.015, Median: 0.001, P90: 0.005, P99: 0.02, Max: 0.03},
		CommitLatency:      client.LatencyStats{Count: 10, Sum: 0.5, Mean: 0.05, Median: 0.04, P90: 0.06, P99: 0.07, Max: 0.08},
		GRVLatency:         client.LatencyStats{Count: 20, Sum: 0.6, Mean: 0.03, Median: 0.02, P90: 0.05, P99: 0.09, Max: 0.1},
		TransactionLatency: client.LatencyStats{Count: 30, Sum: 0.7, Mean: 0.023, Median: 0.01, P90: 0.03, P99: 0.04, Max: 0.05},
	}}

	rec := httptest.NewRecorder()
	Handler(src).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text exposition", ct)
	}
	body := rec.Body.String()
	// The terminal LF, asserted SEPARATELY because whole-line membership stopped
	// covering it. strings.Split returns an unterminated final sample as the last
	// element, so the trimmed lookup below succeeds either way -- the previous
	// newline-bearing assertions caught this only as a side effect, and switching
	// matchers silently traded one boundary for another. Prometheus text format
	// 0.0.4 requires the last line to end with LF.
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("exposition does not end with LF, which the 0.0.4 text format "+
			"requires; last 40 bytes: %q", body[max(0, len(body)-40):])
	}
	lines := map[string]struct{}{}
	for _, l := range strings.Split(body, "\n") {
		lines[l] = struct{}{}
	}
	for _, want := range []string{
		"# TYPE fdb_client_transactions_commit_started_total counter\n",
		"fdb_client_transactions_commit_started_total 7\n",
		"fdb_client_transactions_commit_completed_total 6\n",
		"fdb_client_transactions_not_committed_total 3\n",
		"fdb_client_transactions_maybe_committed_total 21\n",
		"fdb_client_transaction_read_versions_completed_total 42\n",
		"# TYPE fdb_client_grv_cache_hits_total counter\n",
		"fdb_client_grv_cache_hits_total 9\n",
		"# TYPE fdb_client_grv_in_band_maybe_delivered_total counter\n",
		"fdb_client_grv_in_band_maybe_delivered_total 11\n",
		"fdb_client_transaction_retries_total 4\n",
		"fdb_client_transactions_throttled_total 22\n",
		// RFC-114 counters.
		"# TYPE fdb_client_connection_failures_total counter\n",
		"fdb_client_connection_failures_total 2\n",
		"fdb_client_coordinator_changes_total 1\n",
		// The remaining counters, so that deleting ANY counterDef reddens this
		// test by name. Without these the exposition named 11 of 18, and the
		// TYPE-count check below cannot see a deletion at all.
		"fdb_client_transactions_resource_constrained_total 12\n",
		"fdb_client_transactions_process_behind_total 13\n",
		"fdb_client_transactions_too_old_total 14\n",
		"fdb_client_transactions_future_versions_total 15\n",
		"fdb_client_transaction_batch_read_versions_completed_total 16\n",
		"fdb_client_transaction_default_read_versions_completed_total 17\n",
		"fdb_client_transaction_immediate_read_versions_completed_total 18\n",
		// and every summary, same reason.
		"# TYPE fdb_client_commit_latency_seconds summary\n",
		"fdb_client_commit_latency_seconds_count 10\n",
		"# TYPE fdb_client_grv_latency_seconds summary\n",
		"fdb_client_grv_latency_seconds_count 20\n",
		"# TYPE fdb_client_transaction_latency_seconds summary\n",
		"fdb_client_transaction_latency_seconds_count 30\n",
		// RFC-114 latency summary.
		"# TYPE fdb_client_read_latency_seconds summary\n",
		"fdb_client_read_latency_seconds{quantile=\"0.5\"} 0.001\n",
		"fdb_client_read_latency_seconds{quantile=\"0.9\"} 0.005\n",
		"fdb_client_read_latency_seconds{quantile=\"0.99\"} 0.02\n",
		"fdb_client_read_latency_seconds_sum 1.5\n",
		"fdb_client_read_latency_seconds_count 100\n",
	} {
		// WHOLE-LINE membership, not strings.Contains.
		//
		// A trailing \n anchors only the END of a line. With a substring match
		// the START is still open, so output that ran two lines together --
		// "# HELP x…# TYPE x counter\n" -- still contains the TYPE expectation
		// and passed; a metric name with a prefix would satisfy a value entry
		// the same way. Both boundaries are needed, and comparing against the
		// set of rendered lines gives both at once.
		if _, ok := lines[strings.TrimSuffix(want, "\n")]; !ok {
			t.Errorf("exposition missing the whole line %q\nbody:\n%s", want, body)
		}
	}
	// Every defined counter and summary renders a TYPE line.
	//
	// This does NOT catch a missing counterDef, and cannot: both sides derive
	// from `counters`, so deleting an entry moves them together and the equality
	// still holds. Measured -- removing one fired only the explicit string
	// assertions above, and this check stayed silent. It catches a renderer that
	// skips or duplicates an entry it WAS given.
	//
	// What catches an entry that was never defined is the explicit list above,
	// and only because that list now names ALL of them. It named 11 of 18
	// counters and 1 of 4 summaries when this comment first claimed otherwise:
	// deleting transactions_too_old was invisible to both checks. Verified after
	// filling it in -- deleting that same counterDef, and separately a summary,
	// each fails BY NAME.
	if got, want := strings.Count(body, "# TYPE "), len(counters)+len(summaries); got != want {
		t.Errorf("rendered %d TYPE lines, want %d", got, want)
	}

	// EVERY counter DECLARES ITS ORIGIN, AND EVERY Go-only ONE SAYS SO IN HELP.
	//
	// Iterates `counters` itself rather than a hardcoded name list. The previous
	// version listed five names, so a nineteenth Go-only counter was never
	// examined -- adding one with no provenance passed, and silently falsified
	// the split in the table's own doc, which nothing checked in the ADD
	// direction.
	//
	// counterOrigin's zero value is invalid, so a counterDef that forgets to
	// declare an origin fails here. That is what makes this fail CLOSED: the
	// iteration cannot miss an entry, and an entry cannot opt out by omission.
	//
	// The classification only matters if it survives into the exposition, since
	// whoever audits it is reading a scrape, not this repository -- which is why
	// BOTH arms assert on the rendered HELP LINE rather than on the help string
	// in source -- the Go-only arm that it declares provenance, the twin arm
	// that it does not claim any. An earlier version said only the Go-only arm
	// did, which was true when the twin arm asserted nothing.
	for _, c := range counters {
		switch c.origin {
		case originUnset:
			t.Errorf("counter %s declares no origin; counterOrigin's zero value is "+
				"invalid precisely so this cannot pass unnoticed", c.name)
		case originCPPTwin:
			// The COMPLEMENT. Without it the audit fails closed on the zero value
			// only, not over the value space: relabelling a Go-only counter as a
			// twin removes it from the Go-only arm while its help text still reads
			// "Go-only", and that self-contradiction ships green. Same shape as a
			// name deleted from a hardcoded list, one spelling further on.
			if line, ok := helpLine(body, c.name); !ok {
				t.Errorf("no HELP line for %s", c.name)
			} else if declaresGoOnly(line) {
				t.Errorf("%s is declared originCPPTwin but its HELP claims it is "+
					"Go-only: %q", c.name, line)
			}
		case originGoOnly:
			// "Go aggregate" is transaction_retries' spelling: C++ retries those
			// codes without a counter, which is the same claim.
			if line, ok := helpLine(body, c.name); !ok {
				t.Errorf("no HELP line for %s; its provenance cannot be checked", c.name)
			} else if !declaresGoOnly(line) {
				t.Errorf("HELP for %s does not declare it Go-only: %q", c.name, line)
			}
		default:
			// A new counterOrigin class with no arm here would silently opt every
			// counter carrying it out of BOTH checks above -- the same escape as
			// a name quietly dropped from a hardcoded list.
			t.Errorf("%s has counterOrigin %d, which this audit does not handle; "+
				"add an arm rather than letting a new class opt out", c.name, c.origin)
		}
	}
}

// helpLine returns the single "# HELP <name> ..." line for name from a rendered
// exposition body. Split out because both origin arms need it, and because an
// inline copy in each is how the two drift apart.
func helpLine(body, name string) (string, bool) {
	i := strings.Index(body, "# HELP "+name+" ")
	if i < 0 {
		return "", false
	}
	line := body[i:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	return line, true
}

// THE DOCUMENTED SPLIT IS ASSERTED IN THE ADD DIRECTION.
//
// `counters`' doc states the entry count and its C++-twin split; Handler's
// states the summary count. Nothing checked either, so adding a counter or a
// summary left both sentences quietly false -- and those sentences are what a
// maintainer audits the classification against.
//
// The numbers are NOT restated here. A guard's prose should name WHERE a claim
// lives, never repeat WHAT it says: a location stays true as the table grows,
// a value does not, and a restatement above the assertion is one nothing can
// ever redden for. The literals live in the assertion, which is the one place
// a change has to walk past.
// and those sentences are what a maintainer audits the classification against.
//
// Deliberately brittle: a legitimate addition SHOULD fail here, because the
// same change has to update the prose. That is the whole point of pinning a
// number written in a comment.
func TestDocumentedCounterSplitMatchesTheTable(t *testing.T) {
	t.Parallel()

	var goOnly, cppTwin, other int
	for _, c := range counters {
		switch c.origin {
		case originGoOnly:
			goOnly++
		case originCPPTwin:
			cppTwin++
		default:
			other++
		}
	}
	if len(counters) != 18 || cppTwin != 13 || goOnly != 5 || other != 0 {
		t.Errorf("table is %d counters (%d C++ twin, %d Go-only, %d unclassified). "+
			"The TOTAL 18 is written in two places, the package doc and the doc on "+
			"`counters`; the 13 + 5 SPLIT only in the latter. Update the ones your "+
			"change actually falsifies -- a split that holds the total needs one "+
			"edit, not two",
			len(counters), cppTwin, goOnly, other)
	}
	if len(summaries) != 4 {
		t.Errorf("table is %d summaries. The CLAIM about how many there are lives "+
			"in the package doc, in Handler's doc, and in `summaries`' own doc, "+
			"which encodes it by enumerating them. Fix all three -- and note the "+
			"word alone is not the claim; it also appears in `counters`' doc about "+
			"something else", len(summaries))
	}
}

// declaresGoOnly reports whether a HELP line says its counter has no C++
// TransactionMetrics twin. "Go aggregate" is transaction_retries' spelling:
// C++ retries those codes without a counter, which is the same claim.
//
// ONE predicate, shared by both origin arms. helpLine was extracted for exactly
// this reason while the phrase list stayed copied into each -- the same defect
// one layer in. Adding a third spelling to the Go-only arm (the arm you must
// touch to admit a new counter) while leaving the twin arm alone admitted a
// twin whose HELP declared it Go-only; that was green before this was shared.
// goOnlyMarkers is the provenance vocabulary, named once so the predicate and
// the test that pins it cannot hold different lists. An earlier version of that
// test hardcoded its own copy, so a marker added to the predicate was never
// examined -- the subset-list defect, inside the test written to prevent it.
var goOnlyMarkers = []string{"Go-only", "Go aggregate"}

func declaresGoOnly(help string) bool {
	for _, marker := range goOnlyMarkers {
		if strings.Contains(help, marker) {
			return true
		}
	}
	return false
}

// THE PROVENANCE VOCABULARY IS GUARDED ON THREE INDEPENDENT AXES.
//
// Sharing declaresGoOnly between the two origin arms stopped the vocabulary
// being EXTENDED on one side only. It does not stop it drifting, and the ways
// it can drift are not reducible to each other. Each axis below was added after
// a mutation walked past the ones already here, and each fires ALONE for its
// own case -- measured, not assumed:
//
//   - the LITERAL pin, on the slice. Broadening a marker to a commoner word
//     ("Go aggregate" -> "aggregate") keeps every member load-bearing and every
//     control passing, so only this fires.
//   - NECESSITY, on the prose. Drifting a help text so one marker is no longer
//     the sole carrier of any line ("(Go-only aggregate;") leaves the slice
//     untouched, so only this fires. It also catches a marker that is a
//     substring of another, which an occurrence check cannot.
//   - the NEGATIVE CONTROL, on the predicate. {"Go"} passes the other two --
//     no C++-twin help contains "Go" while every Go-only one does -- and fails
//     here.
//
// The arms are exact complements only over the vocabulary the current help
// strings exercise, which is a property of today's prose rather than of the
// predicate. That is what the three axes together replace.
func TestProvenanceVocabularyIsGuarded(t *testing.T) {
	t.Parallel()

	body := renderExposition(t)

	// The vocabulary itself, pinned to LITERALS rather than derived from the
	// same slice the predicate reads. NECESSITY below leaves goOnlyMarkers as its
	// own oracle -- it asks whether each member is load-bearing, which stays true
	// under a broadening that keeps every member load-bearing. The negative
	// control does not: it tests the predicate against fixed strings.
	// Changing "Go aggregate" to "aggregate" survives it -- the retries help line
	// still supplies the only hit for that member, and no twin help contains the
	// word -- while the predicate now accepts ordinary prose.
	//
	// A literal is the one oracle a change to the slice cannot move with it.
	//
	// It does NOT subsume the necessity loop below, and neither subsumes it:
	// the literal watches the SLICE, necessity watches the PROSE. Drifting a help
	// text to "(Go-only aggregate;" fires necessity alone; broadening a marker
	// fires the literal alone. Written down because a guard that never fires on
	// its own reads as dead code and gets deleted in a later cleanup.
	//
	// The literal also disambiguates necessity, which under redundancy flags BOTH
	// members of a pair -- correct, but a maintainer acting on that alone could
	// delete the canonical one.
	if want := []string{"Go-only", "Go aggregate"}; !slices.Equal(goOnlyMarkers, want) {
		t.Errorf("goOnlyMarkers = %q, want %q. These are the canonical provenance "+
			"spellings; widening one to a commoner word lets non-provenance prose "+
			"satisfy the origin audit. Change the help texts, not the vocabulary",
			goOnlyMarkers, want)
	}

	// Every marker is NECESSARY, not merely present. Drop it and some Go-only
	// counter must lose its declaration under the markers that remain.
	//
	// Occurrence is the weaker test and was the first thing written here: it
	// passes for a marker that is a SUBSTRING of another, since the substring
	// occurs wherever the longer one does. {"Go-only","Go aggregate","Go-"} was
	// fully green that way -- and admitting "Go-" then accepts a help text
	// reading "Go-side rollup", which declares no absent C++ twin at all.
	for i, marker := range goOnlyMarkers {
		remaining := make([]string, 0, len(goOnlyMarkers)-1)
		remaining = append(remaining, goOnlyMarkers[:i]...)
		remaining = append(remaining, goOnlyMarkers[i+1:]...)

		needed := false
		for _, c := range counters {
			if c.origin != originGoOnly {
				continue
			}
			line, ok := helpLine(body, c.name)
			if !ok {
				continue
			}
			declaredWithout := false
			for _, m := range remaining {
				if strings.Contains(line, m) {
					declaredWithout = true
					break
				}
			}
			if !declaredWithout {
				needed = true // only this marker carries that line
				break
			}
		}
		if !needed {
			t.Errorf("marker %q is unnecessary: every Go-only help line is still "+
				"declared without it. An unnecessary marker only widens what the "+
				"predicate accepts -- a substring of another marker passes an "+
				"occurrence check while admitting text that declares nothing",
				marker)
		}
	}

	// The negative control. These are the shapes a broadened predicate would
	// start accepting, and none of them is a provenance declaration.
	for _, notAClaim := range []string{
		"Go",
		"Golang",
		"Read versions served from the GRV cache",
		"retries in the Go client",
		"",
	} {
		if declaresGoOnly(notAClaim) {
			t.Errorf("declaresGoOnly(%q) is true; the predicate has been broadened "+
				"to text that does not declare an absent C++ twin", notAClaim)
		}
	}
}

// renderExposition returns the handler's text output, so a test that only needs
// the body does not restate the httptest wiring.
func renderExposition(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(fakeSource{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
