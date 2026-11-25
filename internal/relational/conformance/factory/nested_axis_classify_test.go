package factory_test

import (
	"errors"
	"testing"
)

// TestClassifyAxisDrivesEveryVerdict is the unit pin under the nested-axis
// gate's classification.
//
// The FDB probe can only exercise the arms today's engine happens to produce:
// it reaches `working`, `gap-declining` and `refusing`, and NOTHING else. Every
// other arm — the ones that fire on the day RFC-230 lands, or the day a
// by-design refusal is lost — would otherwise have its first real firing read
// as a finding rather than as an untested branch, which is the most expensive
// way to discover a bug in an instrument.
//
// Both halves of the split are driven here, and they are not symmetric: a GAP
// that starts answering is good news to be re-pinned, while a BY-DESIGN refusal
// that starts answering is a conformance divergence. A classifier that
// collapsed them would report the second as progress.
func TestClassifyAxisDrivesEveryVerdict(t *testing.T) {
	t.Parallel()
	boom := errors.New(`0AF00: grouping by the nested field "N.SK" is not supported`)
	other := errors.New("42703: Unknown reference N.SK")
	ambiguous := errors.New("42702: Ambiguous reference N.SK")

	for _, tc := range []struct {
		name string
		ax   nestedAxis
		got  []string
		err  error
		want axisVerdict
	}{
		// --- the answering axes ------------------------------------------
		{
			name: "rows match",
			ax:   nestedAxis{axis: "a", want: []string{"1", "2"}},
			got:  []string{"1", "2"},
			want: verdictWorking,
		},
		{
			name: "rows differ is a live defect, not a gap",
			ax:   nestedAxis{axis: "a", want: []string{"1", "2"}},
			got:  []string{"1", "3"},
			want: verdictWrong,
		},
		{
			name: "a different ROW COUNT is also wrong rows",
			ax:   nestedAxis{axis: "a", want: []string{"1", "2"}},
			got:  []string{"1"},
			want: verdictWrong,
		},
		{
			name: "an answering axis that now errors has regressed",
			ax:   nestedAxis{axis: "a", want: []string{"1"}},
			err:  other,
			want: verdictRegressed,
		},

		// --- the GAP population: expected to drain -----------------------
		{
			name: "booked gap still refusing as booked",
			ax:   nestedAxis{axis: "g", declines: "is not supported"},
			err:  boom,
			want: verdictGapDeclining,
		},
		{
			name: "booked gap now answers -- good news",
			ax:   nestedAxis{axis: "g", declines: "is not supported"},
			got:  []string{"11"},
			want: verdictGapAnswers,
		},
		{
			name: "booked gap fails on a different reason -- stale booking",
			ax:   nestedAxis{axis: "g", declines: "is not supported"},
			err:  other,
			want: verdictGapMoved,
		},

		// --- the BY-DESIGN population: must never drain ------------------
		{
			name: "by-design refusal still refusing as booked",
			ax:   nestedAxis{axis: "r", refuses: "Ambiguous reference N.SK"},
			err:  ambiguous,
			want: verdictRefusing,
		},
		{
			// The asymmetry that makes the split worth having. The same
			// observation — no error, rows returned — is good news for a gap and
			// a conformance divergence for a by-design refusal.
			name: "by-design refusal now answers -- a conformance divergence",
			ax:   nestedAxis{axis: "r", refuses: "Ambiguous reference N.SK"},
			got:  []string{"11"},
			want: verdictRefusalLost,
		},
		{
			name: "by-design refusal fails on a different reason",
			ax:   nestedAxis{axis: "r", refuses: "Ambiguous reference N.SK"},
			err:  other,
			want: verdictRefusalMoved,
		},

		// --- malformed entries -------------------------------------------
		{
			name: "an entry booking BOTH a gap and a by-design refusal",
			ax:   nestedAxis{axis: "x", declines: "a", refuses: "b"},
			err:  boom,
			want: verdictMisdeclared,
		},
		{
			name: "an entry booking a refusal AND pinning rows",
			ax:   nestedAxis{axis: "x", declines: "a", want: []string{"1"}},
			err:  boom,
			want: verdictMisdeclared,
		},
		{
			name: "an entry booking a by-design refusal AND pinning rows",
			ax:   nestedAxis{axis: "x", refuses: "a", want: []string{"1"}},
			err:  ambiguous,
			want: verdictMisdeclared,
		},
		{
			// Neither rows nor a refusal: the entry asserts nothing at all, and
			// without this arm it would classify as `working` on any empty
			// result and count toward the floor that proves the gate ran.
			name: "an entry asserting nothing",
			ax:   nestedAxis{axis: "x"},
			want: verdictMisdeclared,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyAxis(tc.ax, tc.got, tc.err); got != tc.want {
				t.Errorf("classifyAxis = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNestedAxesTableIsWellFormed runs the classifier's malformedness check over
// the REAL table without needing a cluster.
//
// The FDB gate fails on a malformed entry, but only when Docker is present —
// so on a machine without it the table could go malformed and nothing would say
// so. This costs milliseconds and covers that hole.
func TestNestedAxesTableIsWellFormed(t *testing.T) {
	t.Parallel()
	axes := nestedAxes()
	if len(axes) == 0 {
		t.Fatal("no axes: the gate would report green having measured nothing")
	}
	seen := map[string]bool{}
	var gaps, byDesign, answering int
	for _, ax := range axes {
		if ax.axis == "" || ax.query == "" {
			t.Errorf("an axis entry has no name or no query: %+v", ax)
		}
		if seen[ax.axis] {
			t.Errorf("duplicate axis name %q — the log cannot tell the two apart", ax.axis)
		}
		seen[ax.axis] = true
		// Feed the entry an outcome that MATCHES its own declaration; anything
		// other than the corresponding healthy verdict means the entry is
		// malformed rather than that the engine did something.
		switch {
		case ax.declines != "" && ax.refuses != "":
			t.Errorf("axis %q books both a gap and a by-design refusal", ax.axis)
		case ax.declines != "":
			gaps++
			if v := classifyAxis(ax, nil, errors.New(ax.declines)); v != verdictGapDeclining {
				t.Errorf("axis %q books a gap but classifies as %v", ax.axis, v)
			}
		case ax.refuses != "":
			byDesign++
			if v := classifyAxis(ax, nil, errors.New(ax.refuses)); v != verdictRefusing {
				t.Errorf("axis %q books a by-design refusal but classifies as %v", ax.axis, v)
			}
		default:
			answering++
			if v := classifyAxis(ax, ax.want, nil); v != verdictWorking {
				t.Errorf("axis %q pins rows but classifies as %v", ax.axis, v)
			}
		}
	}
	// The by-design population is the one with a floor, and it has one here too:
	// the FDB gate's floor cannot fire without a cluster, so the table-level
	// claim that this population is non-empty is made where it always runs.
	if byDesign == 0 {
		t.Error("the axis table books no BY-DESIGN refusal. That population must never drain — an empty " +
			"bucket means the conformance direction of the gate stopped being measured, not that it succeeded.")
	}
	if answering == 0 {
		t.Error("no axis pins rows, so the gate asserts nothing about what the engine ANSWERS")
	}
	// No floor on `gaps` — reaching zero is the success condition. It is logged
	// so a reader can see the trajectory, and the number is shapes, not gaps:
	// group-by-depth2 and having-depth2 are two shapes of the ONE RFC-230 gap.
	t.Logf("axis table: %d total, %d pinning rows, %d booked gap shapes (expected to reach 0), "+
		"%d by-design refusals (must never reach 0)", len(axes), answering, gaps, byDesign)
}
