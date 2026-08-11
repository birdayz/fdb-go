package docscheck

import (
	"strings"
	"testing"
)

// The census gate's decisions are pure functions over explicit state, and this
// file drives EVERY arm of them.
//
// The live RFC exercises only the arms the document happens to reach — two of
// nine when this pin was written. The other seven included the two whose own
// comments say they guard the exact rot that had already happened once: a
// missing TOTAL row (per-bucket rows each right while the stated total is wrong)
// and a silently absent bucket (every listed row ties out while the RFC
// under-reports the work). An arm that has never fired is not a guard; its first
// real firing reads as a finding rather than as a branch nobody ran.
//
// Each case asserts on a SUBSTRING of the message, not merely on the count, so a
// mutation that swaps one arm's detection for another's is caught rather than
// absorbed by "some problem was reported".

func hasProblemContaining(problems []string, want string) bool {
	for _, p := range problems {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}

// A healthy instrument reading, reused as the baseline every case perturbs.
func censusFixture() (rows [][]string, escapes, authorities map[string]int, distinct, total int) {
	rows = [][]string{
		{"bucket", "authorities", "escapes"},
		{"boundary", "1", "2"},
		{"dotted", "2", "3"},
		{"TOTAL", "3", "5"},
	}
	escapes = map[string]int{"boundary": 2, "dotted": 3}
	authorities = map[string]int{"boundary": 1, "dotted": 2}
	return rows, escapes, authorities, 3, 5
}

func TestFieldDebtCensusTableArms(t *testing.T) {
	t.Parallel()

	t.Run("clean fixture reports nothing", func(t *testing.T) {
		t.Parallel()
		rows, esc, auth, distinct, total := censusFixture()
		if got := checkCensusTable(rows, esc, auth, distinct, total); len(got) != 0 {
			t.Fatalf("the baseline must be clean or every case below is vacuous: %v", got)
		}
	})

	cases := []struct {
		name   string
		mutate func(rows [][]string, esc, auth map[string]int) ([][]string, map[string]int, map[string]int)
		want   string
	}{
		{
			name: "wrong escape count for a bucket",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				r[2][2] = "7"
				return r, e, a
			},
			want: `claims bucket "dotted" has 7 escape sites; the instrument measures 3`,
		},
		{
			name: "wrong authority count for a bucket",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				r[1][1] = "9"
				return r, e, a
			},
			want: `claims bucket "boundary" has 9 authorities; the instrument measures 1`,
		},
		{
			name: "TOTAL row disagrees",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				r[3][1], r[3][2] = "34", "52"
				return r, e, a
			},
			want: "TOTAL row claims 34 authorities / 52 escape sites",
		},
		{
			name: "no TOTAL row at all",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				return r[:3], e, a
			},
			want: "has no TOTAL row",
		},
		{
			name: "a bucket carrying debt is absent from the table",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				e["contract"] = 4
				a["contract"] = 3
				return r, e, a
			},
			want: "carry debt and are absent from",
		},
		{
			name: "non-numeric cells",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				r[1][2] = "several"
				return r, e, a
			},
			want: "non-numeric counts",
		},
		{
			name: "row with too few cells",
			mutate: func(r [][]string, e, a map[string]int) ([][]string, map[string]int, map[string]int) {
				r[1] = []string{"boundary", "1"}
				return r, e, a
			},
			want: "cell(s), want 3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, esc, auth, distinct, total := censusFixture()
			rows, esc, auth = tc.mutate(rows, esc, auth)
			got := checkCensusTable(rows, esc, auth, distinct, total)
			if !hasProblemContaining(got, tc.want) {
				t.Errorf("arm did not fire.\nwant a problem containing: %s\ngot: %v", tc.want, got)
			}
		})
	}
}

func TestFieldDebtConcentrationTableArms(t *testing.T) {
	t.Parallel()

	base := func() ([][]string, map[string]int) {
		return [][]string{
				{"authority", "escapes"},
				{"groupByOutputBaker", "5"},
				{"explainValueOrdinals", "3"},
			}, map[string]int{
				"groupByOutputBaker":   5,
				"explainValueOrdinals": 3,
				"somethingSmall":       1,
			}
	}

	t.Run("clean fixture reports nothing", func(t *testing.T) {
		t.Parallel()
		rows, actual := base()
		if got := checkConcentrationTable(rows, actual, fieldDebtConcentrationFloor); len(got) != 0 {
			t.Fatalf("baseline must be clean: %v", got)
		}
	})

	cases := []struct {
		name   string
		mutate func(rows [][]string, actual map[string]int) ([][]string, map[string]int)
		want   string
	}{
		{
			name: "a listed authority has retired to zero",
			mutate: func(r [][]string, a map[string]int) ([][]string, map[string]int) {
				return append(r, []string{"AggregateResultColumnName", "6"}), a
			},
			want: "it carries NONE — the authority has RETIRED",
		},
		{
			name: "a listed authority carries a different number",
			mutate: func(r [][]string, a map[string]int) ([][]string, map[string]int) {
				r[1][1] = "8"
				return r, a
			},
			want: `lists "groupByOutputBaker" at 8 escape site(s); the instrument measures 5`,
		},
		{
			name: "a new concentration nobody wrote down",
			mutate: func(r [][]string, a map[string]int) ([][]string, map[string]int) {
				a["newlyConcentrated"] = 4
				return r, a
			},
			want: "carry >= 3 escape sites and are absent from",
		},
		{
			name: "non-numeric escape count",
			mutate: func(r [][]string, a map[string]int) ([][]string, map[string]int) {
				r[1][1] = "lots"
				return r, a
			},
			want: "non-numeric escape count",
		},
		{
			name: "row with too few cells",
			mutate: func(r [][]string, a map[string]int) ([][]string, map[string]int) {
				r[1] = []string{"groupByOutputBaker"}
				return r, a
			},
			want: "cell(s), want 2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, actual := base()
			rows, actual = tc.mutate(rows, actual)
			got := checkConcentrationTable(rows, actual, fieldDebtConcentrationFloor)
			if !hasProblemContaining(got, tc.want) {
				t.Errorf("arm did not fire.\nwant a problem containing: %s\ngot: %v", tc.want, got)
			}
		})
	}
}

// TestFieldDebtRFCSourceArms drives the two file-level guards, neither of which
// the live document can reach.
func TestFieldDebtRFCSourceArms(t *testing.T) {
	t.Parallel()

	if got := rfcSourceProblem(nil, errForTest{}); !strings.Contains(got, "unreadable file makes the gate vacuous") {
		t.Errorf("unreadable-file arm did not fire: %q", got)
	}
	if got := rfcSourceProblem([]byte("tiny"), nil); !strings.Contains(got, "too small to be the RFC") {
		t.Errorf("file-too-small arm did not fire: %q", got)
	}
	big := make([]byte, fieldDebtRFCMinBytes)
	if got := rfcSourceProblem(big, nil); got != "" {
		t.Errorf("a readable, large-enough file must report no problem, got %q", got)
	}
}

type errForTest struct{}

func (errForTest) Error() string { return "simulated read failure" }

// TestFieldDebtTablePresenceArms drives the missing-marker and too-short-table
// guards, which the live document also cannot reach.
func TestFieldDebtTablePresenceArms(t *testing.T) {
	t.Parallel()

	if got := tablePresenceProblem(nil, false, "<!-- X -->"); !strings.Contains(got, "no <!-- X --> marker") {
		t.Errorf("missing-marker arm did not fire: %q", got)
	}
	if got := tablePresenceProblem([][]string{{"header"}}, true, "<!-- X -->"); !strings.Contains(got, "parsed 1 row(s)") {
		t.Errorf("too-short-table arm did not fire: %q", got)
	}
	ok := [][]string{{"bucket", "a", "e"}, {"dotted", "1", "1"}}
	if got := tablePresenceProblem(ok, true, "<!-- X -->"); got != "" {
		t.Errorf("a present, long-enough table must report no problem, got %q", got)
	}
}

// TestFieldDebtParseTableShapes pins the parser itself, including the two shapes
// that silently produce an empty scan.
func TestFieldDebtParseTableShapes(t *testing.T) {
	t.Parallel()

	const doc = "intro\n\n<!-- M -->\n\nsome prose\n\n| bucket | a | e |\n| --- | --- | --- |\n| dotted | 1 | 2 |\n\nafter the table\n\n| not | this | one |\n"
	rows, found := parseFieldDebtTable(doc, "<!-- M -->")
	if !found {
		t.Fatal("marker present but not found")
	}
	if len(rows) != 2 {
		t.Fatalf("want header + 1 data row, got %d: %v", len(rows), rows)
	}
	if rows[1][0] != "dotted" || rows[1][2] != "2" {
		t.Errorf("row parsed wrong: %v", rows[1])
	}
	// The separator row must be dropped, and a second table after intervening
	// prose must NOT be absorbed.
	for _, r := range rows {
		if strings.HasPrefix(r[0], "---") {
			t.Errorf("separator row leaked into the parse: %v", r)
		}
		if r[0] == "not" {
			t.Errorf("a later, unrelated table was absorbed: %v", r)
		}
	}
	if _, found := parseFieldDebtTable(doc, "<!-- ABSENT -->"); found {
		t.Error("an absent marker must report not-found rather than an empty table")
	}
	// Backticked cells are unwrapped, since the concentration table quotes
	// declaration names as code.
	back, _ := parseFieldDebtTable("<!-- M -->\n| `groupByOutputBaker` | 5 |\n", "<!-- M -->")
	if len(back) != 1 || back[0][0] != "groupByOutputBaker" {
		t.Errorf("backticked cell not unwrapped: %v", back)
	}
}

// TestFieldDebtOrderProseArms drives the gate that closes the third-copy hole,
// including the two shapes that must NOT fire.
func TestFieldDebtOrderProseArms(t *testing.T) {
	t.Parallel()

	firing := []struct {
		name  string
		prose string
		want  string
	}{
		{"the exact sentence a reviewer reverted", "The residual is 34 AUTHORITIES (52 escape sites).", "34"},
		{"escape sites alone", "There are 52 escape sites left.", "52"},
		{"a numbered bucket parenthetical", "3. name-keyed (9, was 15): including the probe", "9"},
		{"harness stays at N", "harness stays at 1 and is out of scope.", "1"},
		{"concentration claim", "four authorities carry 18 of the 52", "18"},
		{"authority sum", "The column sums to 35 against 33 distinct.", "35"},
		{"a bucket that fell to a number", "It fell to 7 as sites were retagged.", "7"},

		// The four the ALLOWLIST version missed. Each is here because a
		// phrasing-based gate could not see it, and deny-by-default can.
		{
			"a stale bucket size the allowlist missed entirely",
			"4. translator (reduced): all eleven remaining sites were read against the two legs",
			"eleven remaining sites",
		},
		{
			"a producer tally", "compare against strings minted by 8 translator:-bucket sites", "8",
		},
		{
			"a decomposition whose parts sum to a table cell",
			"5 downstream of a dotted mint, 2 downstream of other buckets, 5 independent",
			"5",
		},
		{
			"the phrasing the document already used, which no allowlist pattern covered",
			"AggregateResultColumnName used to head this table at 6 escapes",
			"6",
		},
		{"a word-spelled count of readers", "mechanically retires FOUR readers at once", "FOUR readers"},
		{"a word-spelled count of entries", "one producer, four entries", "four entries"},
		{"a word-spelled count of declarations", "two declarations owe debt in two buckets", "two declarations"},

		// ELIDED-NOUN TALLIES. Noun-anchoring cannot see these, and both shipped:
		// the first was a live, wrong bucket size one bullet above a tally that
		// had just been fixed.
		{
			"a tally whose noun the sentence already supplied",
			"The three that remain are each blocked on something outside the bucket",
			"three that",
		},
		{"a bare quantifier tally", "both; two more left by deletion as unreachable", "two more"},

		// An arrow is a magnitude PAIR. Exempting arrows would have let every
		// census claim spell itself with one.
		{"a magnitude pair wearing an arrow", "the translator bucket went 17 → 12", "17"},

		// An HTML comment must not shield the rest of its line.
		{
			"a claim hiding behind an HTML comment on the same line",
			"<!-- note --> the residual is 33 authorities",
			"33",
		},

		// Backticks are a costume, not an exemption.
		{
			"a live claim smuggled inside backticks",
			"The residual is `33 authorities` today.",
			"33",
		},
	}
	for _, tc := range firing {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkOrderProseHasNoCounts(tc.prose)
			if !hasProblemContaining(got, tc.want) {
				t.Errorf("arm did not fire on %q.\nwant a problem containing %q\ngot: %v",
					tc.prose, tc.want, got)
			}
		})
	}

	quiet := []struct {
		name  string
		prose string
	}{
		{"a markdown table row", "| boundary | 1 | 2 |"},
		{"a Java source citation", "GroupByExpression.java:754,758 builds Column.unnamedOf"},
		{"a Go source citation", "killing the mint at query/cascades_translator.go:3925 retires them"},
		{"a bare line-range continuation", "MessageHelpers.java:170-175, and :161-167 beside it"},
		{"an RFC section reference", "blocked on RFC-204 sec 4.4/4.5"},
		{"a CQ reference", "blocked on CQ-55 over CQ-56"},
		{"a shape-only bucket line", "3. name-keyed (much reduced): including the probe"},
		{"an HTML marker line", "<!-- FIELD-DEBT-CENSUS -->"},
		{
			"ordinary prose using a word-number without a census noun",
			"Two durable homes for one fact is worse than one home.",
		},
		{"a fenced code block body", "```\nresidual = 34 authorities\n```"},
		{"a marker line that is a comment end to end", "<!-- FIELD-DEBT-CONCENTRATION -->"},
		{
			"a word-number introducing a prepositional phrase, not a tally",
			"one of the two reversed edges runs backwards",
		},
	}
	// `one that …` is the BROADEST trigger in the elided-tally pattern, and it
	// does over-fire on ordinary prose — measured at 129 `fieldDebtProseElidedTally`
	// hits across `rfcs/*.md`, 43 of them innocuous. That breadth is accepted
	// deliberately: the gate's scope is ONE section of ONE file, where a false
	// positive costs a reword and a false negative costs a wrong number nobody
	// can see. It is pinned as a FIRING case rather than left unexercised so the
	// next reader meets the over-fire here instead of tripping over it, and so
	// narrowing the pattern is a deliberate act against a stated measurement.
	t.Run("the broad one-that trigger fires on ordinary prose, by design", func(t *testing.T) {
		t.Parallel()
		got := checkOrderProseHasNoCounts("the one that fires first wins")
		if !hasProblemContaining(got, "one that") {
			t.Errorf("the `one that` branch is expected to fire even on innocuous prose; "+
				"if it was deliberately narrowed, re-derive the measurement in this "+
				"test's comment rather than deleting the case. got: %v", got)
		}
	})

	for _, tc := range quiet {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := checkOrderProseHasNoCounts(tc.prose); len(got) != 0 {
				t.Errorf("gate must stay quiet on %q, got: %v", tc.prose, got)
			}
		})
	}
}

// TestFieldDebtUnbalancedFenceIsNotSilent pins the kill switch.
//
// `inFence` toggles, so a single stray fence marker makes every line below it
// unscanned. Without an explicit balance assertion the gate reports GREEN over a
// section it never read — the green-from-an-empty-set shape, inside the
// instrument built to prevent it. Both halves are driven here, because "the
// unbalanced case reports something" is only meaningful beside "the balanced
// case still catches what follows it".
func TestFieldDebtUnbalancedFenceIsNotSilent(t *testing.T) {
	t.Parallel()

	const after = "\nThe residual is 33 authorities.\n"

	balanced := "intro\n```\nresidual = 99 authorities\n```" + after
	got := checkOrderProseHasNoCounts(balanced)
	if !hasProblemContaining(got, "33") {
		t.Errorf("a balanced fence must not swallow the prose after it: %v", got)
	}
	if hasProblemContaining(got, "99") {
		t.Errorf("a balanced fence body must still be exempt, got: %v", got)
	}

	unbalanced := "intro\n```\nresidual = 99 authorities" + after
	got = checkOrderProseHasNoCounts(unbalanced)
	if !hasProblemContaining(got, "unclosed code fence") {
		t.Fatalf("an unbalanced fence must be reported, not silently swallow the rest "+
			"of the section — that is a green over an unread document. got: %v", got)
	}
	// And it must be reported LOUDLY rather than merely being one of several:
	// the claim after the stray fence is invisible, so the fence problem is the
	// only signal that anything was missed.
	if hasProblemContaining(got, "33") {
		t.Error("the claim after an unbalanced fence is expected to be UNSCANNED; if it " +
			"is now detected, the fence guard is no longer load-bearing and this pin " +
			"should be re-derived rather than deleted")
	}
}

// TestFieldDebtOrderSectionExtraction pins the section splitter, whose silent
// failure mode is returning an empty string that every prose check then passes.
func TestFieldDebtOrderSectionExtraction(t *testing.T) {
	t.Parallel()

	const doc = "# T\n\n## Before\n\nbefore body\n\n## Order\n\norder body here\n\n## Rejected alternatives\n\nafter body\n"
	sec, ok := orderSectionOf(doc)
	if !ok {
		t.Fatal("`## Order` present but not found")
	}
	if !strings.Contains(sec, "order body here") {
		t.Errorf("section body missing: %q", sec)
	}
	if strings.Contains(sec, "after body") || strings.Contains(sec, "before body") {
		t.Errorf("section bled into a neighbour: %q", sec)
	}
	if _, ok := orderSectionOf("# T\n\n## Other\n\nbody\n"); ok {
		t.Error("a document with no `## Order` must report not-found, never an empty section")
	}
}
