package docscheck

import (
	"strings"
	"testing"
)

// Every arm of checkLedgerEntry is driven here rather than by whatever the
// ledger happens to hold. An arm exercised only by the live ledger ships
// untested, and its first real firing then reads as a finding about the entry
// rather than as an untested branch of the instrument doing the judging.

// pinWhy is a justification long enough to clear the floor, so that length is
// not silently the thing under test in the cases testing something else.
const pinWhy = "the live counterpart evaluates the key expression and inspects only the grouped suffix, " +
	"which is what Java does; this one walks the proto structurally and cannot tell a grouping column from a " +
	"grouped one, so the two disagree exactly when the grouping prefix is non-empty"

func TestCheckLedgerEntry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		key   string
		entry unreferencedFuncDisposition
		// want is a substring the reported problems must contain; empty means
		// the entry must be accepted with no problems at all.
		want string
	}{
		{
			name:  "a well-formed keep entry is accepted",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep, why: pinWhy},
		},
		{
			name:  "a well-formed twin entry is accepted",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionTwin, counterpart: "helperWithContext", why: pinWhy},
		},
		{
			name:  "a well-formed in-flight entry is accepted",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionInFlight, why: pinWhy},
		},
		{
			name:  "a well-formed defect entry is accepted",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionDefect, why: pinWhy},
		},

		// The failure this ledger exists to make impossible.
		{
			name:  "a justification that defers itself is rejected",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep, why: "justification pending"},
			want:  "placeholder",
		},
		{
			name: "a placeholder inside an otherwise long justification is still rejected",
			key:  "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{
				tag: dispositionKeep,
				why: pinWhy + " — someone should look into whether this still holds",
			},
			want: "placeholder",
		},
		{
			name:  "a one-word justification is rejected on length",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep, why: "legacy"},
			want:  "at least",
		},
		{
			name:  "an empty justification is rejected",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep},
			want:  "at least",
		},
		{
			name:  "whitespace does not satisfy the length floor",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep, why: strings.Repeat(" ", 400)},
			want:  "at least",
		},

		// The arm that makes a twin entry mean anything at all.
		{
			name:  "a twin without a named counterpart is rejected",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionTwin, why: pinWhy},
			want:  "requires `counterpart`",
		},
		{
			name:  "a whitespace counterpart does not satisfy a twin",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionTwin, counterpart: "   ", why: pinWhy},
			want:  "requires `counterpart`",
		},
		{
			name:  "a keep entry may not carry a counterpart",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep, counterpart: "helperWithContext", why: pinWhy},
			want:  "must not carry a counterpart",
		},
		{
			name:  "an in-flight entry may not carry a counterpart",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: dispositionInFlight, counterpart: "helperWithContext", why: pinWhy},
			want:  "must not carry a counterpart",
		},

		// The tag is a closed set: an invented one must fail loudly rather than
		// fall through a default arm as unrecognised-but-tolerated.
		{
			name:  "a missing tag is rejected",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{why: pinWhy},
			want:  "no disposition tag",
		},
		{
			name:  "an invented tag is rejected",
			key:   "pkg/x/y.go # helper",
			entry: unreferencedFuncDisposition{tag: "probably-fine", why: pinWhy},
			want:  "unknown disposition tag",
		},

		{
			name:  "a malformed key is rejected",
			key:   "pkg/x/y.go:helper",
			entry: unreferencedFuncDisposition{tag: dispositionKeep, why: pinWhy},
			want:  "is not",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkLedgerEntry(tc.key, tc.entry)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected the entry to be accepted, got problems: %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected a problem containing %q, but the entry was accepted", tc.want)
			}
			joined := strings.Join(got, " | ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("problems %q do not mention %q", joined, tc.want)
			}
		})
	}
}

// The ratchet's two arms, driven with explicit state. Both were proved once by
// hand — a bogus entry naming a non-violation did make the corpus gate report
// it stale — and that proof lives here rather than in a scratch edit somebody
// reverted, because the conclusion outlives the measurement.
func TestReconcileLedgerBothDirections(t *testing.T) {
	t.Parallel()

	violation := func(path, name string) funcSite { return funcSite{Path: path, Name: name, Line: 12} }
	entry := unreferencedFuncDisposition{tag: dispositionKeep, why: pinWhy}

	cases := []struct {
		name         string
		violations   []funcSite
		ledger       map[string]unreferencedFuncDisposition
		wantUnlisted []string // substrings, one per expected line
		wantStale    []string
	}{
		{
			name:       "a violation with a ledger entry is quiet",
			violations: []funcSite{violation("pkg/a/x.go", "helper")},
			ledger:     map[string]unreferencedFuncDisposition{"pkg/a/x.go # helper": entry},
		},
		{
			name:         "a violation with NO ledger entry is reported unlisted",
			violations:   []funcSite{violation("pkg/a/x.go", "helper")},
			ledger:       map[string]unreferencedFuncDisposition{},
			wantUnlisted: []string{"pkg/a/x.go:12\thelper"},
		},
		{
			// The arm the bogus-entry probe proved: this is what a ledger line
			// looks like the moment someone deletes its function or wires it
			// back into production.
			name:       "a ledger entry naming no violation is reported stale",
			violations: nil,
			ledger:     map[string]unreferencedFuncDisposition{"pkg/a/x.go # helper": entry},
			wantStale:  []string{"pkg/a/x.go # helper"},
		},
		{
			name:       "a ledger entry whose function MOVED is stale, not silently re-matched by name",
			violations: []funcSite{violation("pkg/a/moved.go", "helper")},
			ledger:     map[string]unreferencedFuncDisposition{"pkg/a/x.go # helper": entry},
			// Both fire: the new site is unlisted AND the old line is stale.
			wantUnlisted: []string{"pkg/a/moved.go:12\thelper"},
			wantStale:    []string{"pkg/a/x.go # helper"},
		},
		{
			name:       "same func name in two packages is two independent entries",
			violations: []funcSite{violation("pkg/a/x.go", "helper"), violation("pkg/b/y.go", "helper")},
			ledger:     map[string]unreferencedFuncDisposition{"pkg/a/x.go # helper": entry},
			// pkg/b's is unlisted; pkg/a's entry must NOT be counted stale.
			wantUnlisted: []string{"pkg/b/y.go:12\thelper"},
		},
		{
			name:       "an empty ledger and no violations is the clean state",
			violations: nil,
			ledger:     map[string]unreferencedFuncDisposition{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unlisted, stale := reconcileLedger(tc.violations, map[string]int{}, tc.ledger)
			if len(unlisted) != len(tc.wantUnlisted) {
				t.Fatalf("unlisted = %v, want %d entries %v", unlisted, len(tc.wantUnlisted), tc.wantUnlisted)
			}
			for i, want := range tc.wantUnlisted {
				if !strings.Contains(unlisted[i], want) {
					t.Errorf("unlisted[%d] = %q, want it to contain %q", i, unlisted[i], want)
				}
			}
			if strings.Join(stale, ",") != strings.Join(tc.wantStale, ",") {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
		})
	}
}

// The test-reference count is what separates unambiguous dead code from the
// dead-twin shape, and it is what a reader acts on first, so it must reach the
// failure message rather than being dropped on the way.
func TestReconcileLedgerReportsTestRefCount(t *testing.T) {
	t.Parallel()
	unlisted, _ := reconcileLedger(
		[]funcSite{{Path: "pkg/a/x.go", Name: "twin", Line: 7}},
		map[string]int{"pkg/a/x.go # twin": 27},
		map[string]unreferencedFuncDisposition{},
	)
	if len(unlisted) != 1 || !strings.Contains(unlisted[0], "test=27") {
		t.Fatalf("unlisted = %v, want one line reporting test=27 — without it the reader cannot tell "+
			"unambiguous dead code from the dead-twin shape, which is the first thing they need", unlisted)
	}
}

// Every placeholder spelling must actually fire. A list that grew a typo
// silently stops catching the word it names, and nothing else would notice —
// the check would still look thorough at a glance.
func TestLedgerPlaceholdersAllFire(t *testing.T) {
	t.Parallel()
	if len(ledgerPlaceholders) < 5 {
		t.Fatalf("only %d placeholder spellings — the list collapsed and the check is near-vacuous", len(ledgerPlaceholders))
	}
	for _, p := range ledgerPlaceholders {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			// Each pattern is driven by a phrase it must match, upper-cased:
			// matching has to be case-insensitive, or the ledger evades the
			// check by shouting.
			sample, ok := placeholderSamples[p.String()]
			if !ok {
				t.Fatalf("no sample phrase for the pattern %q — every pattern needs one, or it ships unexercised "+
					"and a typo in it would silently stop catching the deferral it names", p)
			}
			entry := unreferencedFuncDisposition{tag: dispositionKeep, why: pinWhy + ". " + strings.ToUpper(sample) + " here."}
			got := checkLedgerEntry("pkg/x/y.go # helper", entry)
			joined := strings.Join(got, " | ")
			if !strings.Contains(joined, "placeholder") {
				t.Fatalf("the sample %q did not fire pattern %q (problems: %q) — a pattern that cannot fire is "+
					"dead weight pretending to be coverage", sample, p, joined)
			}
		})
	}
}

// placeholderSamples maps each placeholder pattern to a phrase it must reject.
// Keyed by the pattern's own source so a pattern added without a sample fails
// loudly above rather than silently going unexercised.
var placeholderSamples = map[string]string{
	`(?i)\b(justification|reason|rationale|analysis|investigation|verdict)\s+pending\b`: "justification pending",
	`(?i)\bpending\s+(justification|investigation|analysis|review of this)\b`:           "pending investigation",
	`(?i)\btbd\b`:              "tbd",
	`(?i)\bto be determined\b`: "to be determined",
	`(?i)\bfixme\b`:            "fixme",
	`(?i)\btodo\b(?:[^.]|$)`:   "todo later",
	`(?i)\b(needs|requires|wants)\s+(further\s+)?(investigation|analysis|a look)\b`: "needs further investigation",
	`(?i)\b(not|never)\s+(yet\s+)?investigated\b`:                                   "not yet investigated",
	`(?i)\b(should|must|need to|needs to|someone)\s+(\w+\s+)?look\s+into\b`:         "should look into",
	`(?i)\b(reason|why|purpose)\s+(is\s+)?(unclear|unknown)\b`:                      "reason is unclear",
	`(?i)\bno\s+(reason|justification)\s+(given|recorded)\b`:                        "no reason given",
	`(?i)\bsee\s+above\b`: "see above",
	`(?i)\bditto\b`:       "ditto",
	`(?i)\bn/a\b`:         "n/a",
}

// The phrases that must NOT be rejected. Each is drawn from a real justification
// this ledger wants: a bare-substring check refused every one of them, and a
// check whose false positives land on the best entries trains its own removal.
func TestLedgerPlaceholdersDoNotRejectRealJustifications(t *testing.T) {
	t.Parallel()
	for _, phrase := range []string{
		"the monitor pings only when the pending set is non-empty",
		"TODO.md CQ-30 is stale on this function",
		"see TODO.md for the migration order",
		"the analysis below names the live counterpart",
		"the investigation found no divergence on any input",
		"it looks into the nested descriptor before descending",
	} {
		t.Run(phrase, func(t *testing.T) {
			t.Parallel()
			entry := unreferencedFuncDisposition{tag: dispositionKeep, why: pinWhy + ". " + phrase + "."}
			for _, p := range checkLedgerEntry("pkg/x/y.go # helper", entry) {
				if strings.Contains(p, "placeholder") {
					t.Errorf("a legitimate justification was rejected: %q\n  %s", phrase, p)
				}
			}
		})
	}
}
