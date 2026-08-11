package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// nestedProbeDDL is the schema the nested-projection axes are probed over.
//
// It is depth-2 struct nesting on purpose. A single level answers only whether
// a dotted name resolves at all; the four-segment paths that the nested-path
// work kept breaking on (`alias.col.inner.leaf`) are unreachable without a
// struct inside a struct. The flat `sk` column is there for the COLLISION axis:
// a nested leaf whose last segment equals a flat column's name is the shape a
// wrong-column read hid in.
const nestedProbeDDL = "CREATE TYPE AS STRUCT inr (sk BIGINT, co STRING) " +
	"CREATE TYPE AS STRUCT nst (sk BIGINT, co STRING, dp inr) " +
	"CREATE TABLE nt (id BIGINT, sk BIGINT, n nst, PRIMARY KEY (id)) " +
	"CREATE INDEX nt_sk ON nt (sk)"

const nestedProbeInsert = "INSERT INTO nt VALUES " +
	"(1, 100, (11, 'a', (111, 'aa'))), " +
	"(2, 200, (22, 'b', (222, 'bb'))), " +
	"(3, 300, (33, 'c', (333, 'cc')))"

// nestedAxis is one generated shape and the rows it must return.
//
// Want holds the FULL expected result as `|`-joined column values per row, in
// the query's own order. Comparing the whole projected row rather than a row
// count is the point: the defect this probe exists for returned the right
// NUMBER of rows with the wrong COLUMN in them, which any count-based check
// reports as a pass.
type nestedAxis struct {
	axis  string
	query string
	want  []string

	// declines names a booked GAP: a capability Java has, Go does not yet, and
	// someone will finish. The value is the text the current refusal must carry,
	// which makes the booking an assertion rather than an omission — a
	// one-directional gate that only failed on wrong rows would stay green while
	// a working axis regressed to an error.
	//
	// This population is expected to DRAIN. Reaching zero is the SUCCESS
	// CONDITION, not a build break, so there is deliberately no floor under it.
	// When a gap closes the axis fails here with its real rows in the log, and
	// the fix is to drop this field and pin `want`.
	//
	// It currently has NO users: the only gap ever booked here was RFC-230's
	// nested group key, in two shapes, and RFC-230 landed — both are pinned to
	// rows above. The field stays because the mechanism is what makes the next
	// gap bookable rather than omitted, and it is not dead code: every arm of
	// its classification is driven by TestClassifyAxisDrivesEveryVerdict, which
	// is the whole reason those arms were unit-pinned instead of left to the
	// corpus reading. An empty `declines` is the instrument working, not the
	// instrument idle — `refuses` is what keeps the gate from going vacuous.
	declines string

	// refuses names a BY-DESIGN refusal: a shape where declining IS the correct
	// answer and Java declines identically, so it will never close.
	//
	// It is a separate field from `declines` because the two populations have
	// OPPOSITE expected trajectories, and one floor cannot serve both. A gap
	// draining to zero is progress; a by-design refusal draining to zero means
	// Go started accepting something Java rejects — a conformance divergence
	// wearing the costume of a fixed bug. The vacuity floor therefore lives on
	// THIS population, because this is the one whose collapse would silently
	// stop asserting anything while every count still looked healthy.
	refuses string
}

// axisVerdict is the classification of one axis's outcome.
//
// It is a value returned by a pure function rather than a branch that mutates
// counters inline, because the FDB probe can only ever exercise the arms that
// today's engine happens to produce. Every other arm — the ones that fire the
// day a gap closes or a by-design refusal is lost — would otherwise ship
// untested and have its first real firing read as a finding rather than as an
// untested branch.
type axisVerdict int

const (
	verdictWorking      axisVerdict = iota // answered, rows match
	verdictWrong                           // answered, WRONG rows: a live defect
	verdictRegressed                       // was expected to answer, now errors
	verdictGapDeclining                    // booked gap, still refusing as booked
	verdictGapAnswers                      // booked gap now ANSWERS: good news, re-pin it
	verdictGapMoved                        // booked gap fails on a different reason: stale booking
	verdictRefusing                        // by-design refusal, still refusing as booked
	verdictRefusalLost                     // by-design refusal now ANSWERS: a conformance divergence
	verdictRefusalMoved                    // by-design refusal fails differently
	verdictMisdeclared                     // the table entry itself is malformed
)

// classifyAxis decides one axis's outcome from its declaration and what the
// engine did, and nothing else — no process globals, no counters.
func classifyAxis(ax nestedAxis, got []string, err error) axisVerdict {
	booked, byDesign := ax.declines, false
	if ax.refuses != "" {
		booked, byDesign = ax.refuses, true
	}
	switch {
	// An entry may book at most one refusal, and a booked refusal may not also
	// pin rows: both would make the entry assert two incompatible things and
	// silently resolve to whichever branch is read first.
	case ax.declines != "" && ax.refuses != "":
		return verdictMisdeclared
	case booked != "" && ax.want != nil:
		return verdictMisdeclared
	case booked == "" && ax.want == nil:
		return verdictMisdeclared
	}
	if booked != "" {
		switch {
		case err == nil:
			if byDesign {
				return verdictRefusalLost
			}
			return verdictGapAnswers
		case !strings.Contains(err.Error(), booked):
			if byDesign {
				return verdictRefusalMoved
			}
			return verdictGapMoved
		case byDesign:
			return verdictRefusing
		default:
			return verdictGapDeclining
		}
	}
	switch {
	case err != nil:
		return verdictRegressed
	case !equalStrings(got, ax.want):
		return verdictWrong
	}
	return verdictWorking
}

// nestedAxes enumerates the projection shapes the generator has to be able to
// emit, one per (depth, qualification, clause, collision, source) point that
// produced a real defect.
//
// This list is the SPECIFICATION the generator is measured against, which is
// why it lives as an executable test rather than as a comment: a generator
// axis nobody runs is indistinguishable from an axis nobody implemented.
func nestedAxes() []nestedAxis {
	return []nestedAxis{
		// --- depth ---------------------------------------------------------
		{axis: "depth2-select", query: "SELECT n.sk FROM nt ORDER BY id", want: []string{"11", "22", "33"}},
		{axis: "depth3-select", query: "SELECT n.dp.sk FROM nt ORDER BY id", want: []string{"111", "222", "333"}},
		{axis: "depth3-qualified", query: "SELECT a.n.dp.sk FROM nt AS a ORDER BY a.id", want: []string{"111", "222", "333"}},
		{axis: "depth4-table-qualified", query: "SELECT nt.n.dp.co FROM nt ORDER BY nt.id", want: []string{"aa", "bb", "cc"}},

		// --- qualification ---------------------------------------------------
		{axis: "unqualified", query: "SELECT n.co FROM nt ORDER BY id", want: []string{"a", "b", "c"}},
		{axis: "alias-qualified", query: "SELECT a.n.co FROM nt AS a ORDER BY a.id", want: []string{"a", "b", "c"}},
		{axis: "table-qualified", query: "SELECT nt.n.co FROM nt ORDER BY nt.id", want: []string{"a", "b", "c"}},

		// --- collision -------------------------------------------------------
		// Two leaves of ONE struct root. The labelling defect produced two
		// output columns with the same name here.
		{axis: "two-leaves-one-root", query: "SELECT n.sk, n.co FROM nt ORDER BY id", want: []string{"11|a", "22|b", "33|c"}},
		// A nested leaf whose last segment collides with a flat column. The
		// wrong-column read lived exactly here.
		{axis: "leaf-collides-with-flat", query: "SELECT sk, n.sk FROM nt ORDER BY id", want: []string{"100|11", "200|22", "300|33"}},
		{axis: "leaf-collides-across-depths", query: "SELECT n.sk, n.dp.sk FROM nt ORDER BY id", want: []string{"11|111", "22|222", "33|333"}},

		// --- clause ----------------------------------------------------------
		{axis: "where-depth2", query: "SELECT id FROM nt WHERE n.sk > 11 ORDER BY id", want: []string{"2", "3"}},
		{axis: "where-depth3", query: "SELECT id FROM nt WHERE n.dp.sk = 222", want: []string{"2"}},
		{axis: "order-by-depth2", query: "SELECT id FROM nt ORDER BY n.sk DESC", want: []string{"3", "2", "1"}},
		{axis: "order-by-depth3", query: "SELECT id FROM nt ORDER BY n.dp.co DESC", want: []string{"3", "2", "1"}},
		{axis: "order-by-qualified-depth3", query: "SELECT a.id FROM nt AS a ORDER BY a.n.dp.sk DESC", want: []string{"3", "2", "1"}},
		{axis: "agg-arg-depth2", query: "SELECT MAX(n.sk) FROM nt", want: []string{"33"}},
		{axis: "agg-arg-depth3", query: "SELECT SUM(n.dp.sk) FROM nt", want: []string{"666"}},
		// These two were booked as a GAP — two shapes of one, both dying on
		// `grouping by the nested field %q is not supported` from a single site,
		// tracked as RFC-230. RFC-230 landed, that refusal no longer exists, and
		// both shapes now answer. They are pinned to rows here, which is the
		// transition the `declines` field exists to make: a gap drains, and
		// draining is the success condition rather than a build break.
		//
		// The gate is what surfaced it, and it surfaced it as instructions
		// rather than as a puzzle — "2 booked GAPS now answer ... drop the
		// axis's `declines` and pin its rows in `want`", with the rows in the
		// log. Keeping the HAVING shape beside the GROUP BY one is still worth
		// it: HAVING reaches the group key by a different path than the SELECT
		// list does, so they can regress independently even though they closed
		// together.
		//
		// Each `n.sk` is distinct across the three probe rows, so every group
		// holds exactly one row and `HAVING COUNT(*) > 0` keeps all three.
		{axis: "group-by-depth2", query: "SELECT n.sk, COUNT(*) FROM nt GROUP BY n.sk ORDER BY n.sk", want: []string{"11|1", "22|1", "33|1"}},
		{axis: "having-depth2", query: "SELECT n.sk FROM nt GROUP BY n.sk HAVING COUNT(*) > 0 ORDER BY n.sk", want: []string{"11", "22", "33"}},
		{axis: "join-on-depth2", query: "SELECT l.id, r.id FROM nt AS l JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id", want: []string{"1|1", "2|2", "3|3"}},
		{axis: "join-on-depth3", query: "SELECT l.id FROM nt AS l JOIN nt AS r ON l.n.dp.sk = r.n.dp.sk ORDER BY l.id", want: []string{"1", "2", "3"}},

		// --- sources ---------------------------------------------------------
		{axis: "derived-table", query: "SELECT d.x FROM (SELECT n.dp.sk AS x FROM nt) AS d ORDER BY d.x", want: []string{"111", "222", "333"}},
		{axis: "cte", query: "WITH c AS (SELECT n.sk AS x FROM nt) SELECT c.x FROM c ORDER BY c.x", want: []string{"11", "22", "33"}},
		{axis: "join-projection", query: "SELECT l.n.sk, r.n.dp.co FROM nt AS l JOIN nt AS r ON l.id = r.id ORDER BY l.id", want: []string{"11|aa", "22|bb", "33|cc"}},

		// --- shadowing -------------------------------------------------------
		// `SELECT n.sk FROM nt AS n` is genuinely AMBIGUOUS, and the refusal is
		// the correct answer rather than a gap. `n` is both the table alias —
		// under which `sk` is a column — and the struct column, under which `sk`
		// is a field, so `n.sk` names two different values and the engine cannot
		// pick one.
		//
		// Java refuses it identically and Go matches it deliberately.
		// SemanticAnalyzer.lookup appends BOTH the direct qualified match and
		// the lookupNestedField match into the same directMatchesBuilder
		// (SemanticAnalyzer.java:475-487), and resolveIdentifier then asserts
		// `attributes.size() == 1` with AMBIGUOUS_COLUMN / "Ambiguous reference
		// %s" (SemanticAnalyzer.java:422). This axis will never close, which is
		// why it is booked as `refuses` and not as `declines`.
		//
		// The comment this replaces described a different defect — that the
		// alias renames the table so the TABLE name is no longer a legal
		// qualifier. That is a real hazard, but it is not this query: nothing
		// here names the table, and the refusal being pinned is the ambiguity.
		{axis: "alias-shadows-table", query: "SELECT n.sk FROM nt AS n ORDER BY n.id", refuses: "Ambiguous reference N.SK"},
	}
}

// TestFDB_NestedProjectionAxes runs every nested-projection axis the generator
// is meant to emit and records, per axis, whether the engine answers, errors,
// or answers WRONG.
//
// A wrong answer is failed loudly and separately from an error: an error is a
// gap (the generator emits the shape, the corpus books it), while a wrong row
// is a live defect and outranks everything else this probe is for.
func TestFDB_NestedProjectionAxes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := openFactorySchema(t, ctx, "zznestaxis", nestedProbeDDL)
	if _, err := db.ExecContext(ctx, nestedProbeInsert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	axes := nestedAxes()
	if len(axes) == 0 {
		t.Fatal("no axes: this probe would report green having measured nothing")
	}

	buckets := map[axisVerdict][]string{}
	for _, ax := range axes {
		got, err := queryRowStrings(ctx, db, ax.query)
		v := classifyAxis(ax, got, err)
		detail := fmt.Sprintf("%s: %s", ax.axis, ax.query)
		switch v {
		case verdictWrong:
			detail += fmt.Sprintf("\n    want %v\n    got  %v", ax.want, got)
		case verdictRegressed, verdictGapMoved, verdictRefusalMoved:
			detail += fmt.Sprintf("\n    booked %q, got %v", ax.declines+ax.refuses, err)
		case verdictGapAnswers, verdictRefusalLost:
			detail += fmt.Sprintf("\n    now answers %v", got)
		case verdictWorking:
			t.Logf("OK           %-28s %s", ax.axis, ax.query)
		case verdictGapDeclining:
			t.Logf("GAP          %-28s %s\n               %v", ax.axis, ax.query, err)
		case verdictRefusing:
			t.Logf("BY DESIGN    %-28s %s\n               %v", ax.axis, ax.query, err)
		}
		buckets[v] = append(buckets[v], detail)
	}
	n := func(v axisVerdict) int { return len(buckets[v]) }
	join := func(v axisVerdict) string { return strings.Join(buckets[v], "\n") }

	// A malformed table entry is checked before anything is read off the
	// classification, because every count below is meaningless if an entry
	// asserted two things at once.
	if n(verdictMisdeclared) > 0 {
		t.Fatalf("%d axis entries are malformed — an entry books at most one refusal and never pins rows "+
			"alongside one:\n%s", n(verdictMisdeclared), join(verdictMisdeclared))
	}
	// Wrong rows first and alone: an axis that answers incorrectly is a live
	// defect, and it must not be read past on the way to a gap report.
	if n(verdictWrong) > 0 {
		t.Fatalf("%d nested-projection axes returned WRONG ROWS — a live defect, not a gap:\n%s",
			n(verdictWrong), join(verdictWrong))
	}
	if n(verdictRegressed) > 0 {
		t.Errorf("%d nested-projection axes that ANSWERED now error. The generator emits these shapes, so "+
			"this is coverage lost, not a shape nobody runs:\n%s", n(verdictRegressed), join(verdictRegressed))
	}
	if n(verdictGapAnswers) > 0 {
		t.Errorf("%d booked GAPS now answer. That is good news: the capability landed, and the fix is to drop "+
			"the axis's `declines` and pin its rows in `want` — the rows are in this log:\n%s",
			n(verdictGapAnswers), join(verdictGapAnswers))
	}
	if n(verdictGapMoved) > 0 {
		t.Errorf("%d booked GAPS still refuse but on a different reason — the refusal moved and the booking "+
			"is stale:\n%s", n(verdictGapMoved), join(verdictGapMoved))
	}
	if n(verdictRefusalLost) > 0 {
		t.Errorf("%d BY-DESIGN refusals now ANSWER. This is not progress: these are shapes Java refuses too, "+
			"so Go answering one is a conformance divergence, and an ambiguous reference that starts resolving "+
			"resolves to one of two columns arbitrarily. Verify against Java before pinning any rows:\n%s",
			n(verdictRefusalLost), join(verdictRefusalLost))
	}
	if n(verdictRefusalMoved) > 0 {
		t.Errorf("%d BY-DESIGN refusals fail on a different reason than booked — the refusal is still there "+
			"but no longer the one Java raises:\n%s", n(verdictRefusalMoved), join(verdictRefusalMoved))
	}

	// The two vacuity floors, and only two. `working` and `refuses` are the
	// populations whose collapse would be SILENT: an all-refusing table proves
	// the schema never built, and a table with no by-design refusal left has
	// stopped asserting that Go still declines what Java declines.
	//
	// There is deliberately NO floor on the gap population. Its expected
	// trajectory is DOWNWARD — RFC-230 closing takes both group-by shapes with
	// it — and a floor there would turn the corpus's own success into a build
	// break. If it reaches zero, delete nothing: the `refuses` floor still
	// guards the instrument.
	if n(verdictWorking) == 0 {
		t.Fatalf("no axis answers correctly: the schema never built, or nested paths stopped resolving entirely. "+
			"%d gaps, %d by-design refusals.", n(verdictGapDeclining), n(verdictRefusing))
	}
	if n(verdictRefusing) == 0 {
		t.Fatalf("no BY-DESIGN refusal is asserted any more. This population must never drain — unlike the " +
			"booked gaps, these are shapes that will never close, so an empty bucket means the conformance " +
			"direction of this gate stopped being measured rather than that it succeeded.")
	}
	t.Logf("nested axes: %d total, %d answering correctly, %d declining on a booked gap (expected to reach 0), "+
		"%d refusing by design (must never reach 0)",
		len(axes), n(verdictWorking), n(verdictGapDeclining), n(verdictRefusing))
}

func queryRowStrings(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				parts[i] = "NULL"
			case []byte:
				parts[i] = string(x)
			default:
				parts[i] = fmt.Sprintf("%v", x)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out, rows.Err()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
