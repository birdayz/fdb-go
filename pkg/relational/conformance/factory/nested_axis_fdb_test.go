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
	// declines names the BOOKED gap this shape currently dies on, and its
	// presence is an assertion in its own right: the axis must still fail, and
	// fail with a message containing this text.
	//
	// Recording the refusal rather than omitting the shape is what makes this a
	// two-directional gate. A one-directional version -- fail only on wrong rows
	// -- would stay green while a working axis silently regressed to an error,
	// and green is the direction that ships bugs. When a gap closes, the axis
	// fails HERE with its real rows in the log, and the fix is to drop this
	// field and pin `want`.
	declines string
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
		{axis: "depth3-select", query: "SELECT n.dp.sk FROM nt ORDER BY id", declines: `qualifier "N.DP" cannot be resolved`},
		{axis: "depth3-qualified", query: "SELECT a.n.dp.sk FROM nt AS a ORDER BY a.id", declines: `qualifier "A.N.DP" cannot be resolved`},
		{axis: "depth4-table-qualified", query: "SELECT nt.n.dp.co FROM nt ORDER BY nt.id", declines: `qualifier "NT.N.DP" cannot be resolved`},

		// --- qualification ---------------------------------------------------
		{axis: "unqualified", query: "SELECT n.co FROM nt ORDER BY id", want: []string{"a", "b", "c"}},
		{axis: "alias-qualified", query: "SELECT a.n.co FROM nt AS a ORDER BY a.id", declines: `qualifier "A.N" cannot be resolved`},
		{axis: "table-qualified", query: "SELECT nt.n.co FROM nt ORDER BY nt.id", declines: `qualifier "NT.N" cannot be resolved`},

		// --- collision -------------------------------------------------------
		// Two leaves of ONE struct root. The labelling defect produced two
		// output columns with the same name here.
		{axis: "two-leaves-one-root", query: "SELECT n.sk, n.co FROM nt ORDER BY id", want: []string{"11|a", "22|b", "33|c"}},
		// A nested leaf whose last segment collides with a flat column. The
		// wrong-column read lived exactly here.
		{axis: "leaf-collides-with-flat", query: "SELECT sk, n.sk FROM nt ORDER BY id", want: []string{"100|11", "200|22", "300|33"}},
		{axis: "leaf-collides-across-depths", query: "SELECT n.sk, n.dp.sk FROM nt ORDER BY id", declines: `qualifier "N.DP" cannot be resolved`},

		// --- clause ----------------------------------------------------------
		{axis: "where-depth2", query: "SELECT id FROM nt WHERE n.sk > 11 ORDER BY id", want: []string{"2", "3"}},
		{axis: "where-depth3", query: "SELECT id FROM nt WHERE n.dp.sk = 222", declines: "could not plan query"},
		{axis: "order-by-depth2", query: "SELECT id FROM nt ORDER BY n.sk DESC", want: []string{"3", "2", "1"}},
		{axis: "order-by-depth3", query: "SELECT id FROM nt ORDER BY n.dp.co DESC", declines: "not resolvable in the runtime row"},
		{axis: "order-by-qualified-depth3", query: "SELECT a.id FROM nt AS a ORDER BY a.n.dp.sk DESC", declines: "not resolvable in the runtime row"},
		{axis: "agg-arg-depth2", query: "SELECT MAX(n.sk) FROM nt", want: []string{"33"}},
		{axis: "agg-arg-depth3", query: "SELECT SUM(n.dp.sk) FROM nt", declines: `qualifier "N.DP" cannot be resolved`},
		{axis: "group-by-depth2", query: "SELECT n.sk, COUNT(*) FROM nt GROUP BY n.sk ORDER BY n.sk", declines: "not resolvable in the runtime row"},
		{axis: "having-depth2", query: "SELECT n.sk FROM nt GROUP BY n.sk HAVING COUNT(*) > 0 ORDER BY n.sk", declines: "not resolvable in the runtime row"},
		{axis: "join-on-depth2", query: "SELECT l.id, r.id FROM nt AS l JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id", declines: "FullId with 3 segments"},
		{axis: "join-on-depth3", query: "SELECT l.id FROM nt AS l JOIN nt AS r ON l.n.dp.sk = r.n.dp.sk ORDER BY l.id", declines: "FullId with 4 segments"},

		// --- sources ---------------------------------------------------------
		{axis: "derived-table", query: "SELECT d.x FROM (SELECT n.dp.sk AS x FROM nt) AS d ORDER BY d.x", declines: `qualifier "N.DP" cannot be resolved`},
		{axis: "cte", query: "WITH c AS (SELECT n.sk AS x FROM nt) SELECT c.x FROM c ORDER BY c.x", want: []string{"11", "22", "33"}},
		{axis: "join-projection", query: "SELECT l.n.sk, r.n.dp.co FROM nt AS l JOIN nt AS r ON l.id = r.id ORDER BY l.id", declines: `qualifier "L.N" cannot be resolved`},

		// --- shadowing -------------------------------------------------------
		// The alias renames the table, so the TABLE name is no longer a legal
		// qualifier. Whether the engine accepts or rejects this, it must not
		// silently read a different column.
		{axis: "alias-shadows-table", query: "SELECT n.sk FROM nt AS n ORDER BY n.id", declines: "Ambiguous reference N.SK"},
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

	var wrong, regressed, closed []string
	var working, declining int
	for _, ax := range axes {
		got, err := queryRowStrings(ctx, db, ax.query)
		if ax.declines != "" {
			// A booked gap. It must still fail, and fail on the booked reason.
			switch {
			case err == nil:
				closed = append(closed, fmt.Sprintf("%s: %s\n    now answers %v", ax.axis, ax.query, got))
			case !strings.Contains(err.Error(), ax.declines):
				closed = append(closed, fmt.Sprintf("%s: %s\n    booked %q, got %v", ax.axis, ax.query, ax.declines, err))
			default:
				declining++
				t.Logf("DECLINES     %-28s %s\n               %v", ax.axis, ax.query, err)
			}
			continue
		}
		switch {
		case err != nil:
			regressed = append(regressed, fmt.Sprintf("%s: %s\n    %v", ax.axis, ax.query, err))
		case !equalStrings(got, ax.want):
			wrong = append(wrong, fmt.Sprintf("%s: %s\n    want %v\n    got  %v", ax.axis, ax.query, ax.want, got))
		default:
			working++
			t.Logf("OK           %-28s %s", ax.axis, ax.query)
		}
	}

	// Wrong rows first and alone: an axis that answers incorrectly is a live
	// defect, and it must not be read past on the way to a gap report.
	if len(wrong) > 0 {
		t.Fatalf("%d nested-projection axes returned WRONG ROWS — a live defect, not a gap:\n%s",
			len(wrong), strings.Join(wrong, "\n"))
	}
	if len(regressed) > 0 {
		t.Errorf("%d nested-projection axes that ANSWERED now error. The generator emits these shapes, so "+
			"this is coverage lost, not a shape nobody runs:\n%s", len(regressed), strings.Join(regressed, "\n"))
	}
	if len(closed) > 0 {
		t.Errorf("%d booked gaps no longer refuse as booked. If the capability LANDED, that is good news and "+
			"the fix is to drop the axis's `declines` and pin its rows in `want` — the rows are in this log. If it "+
			"merely fails DIFFERENTLY, the refusal moved and the booking is stale:\n%s",
			len(closed), strings.Join(closed, "\n"))
	}
	if working == 0 || declining == 0 {
		t.Fatalf("this gate measured nothing in one direction: %d working, %d declining. Both populations must be "+
			"non-empty — an all-declining table proves the schema never built, and an all-working table means the "+
			"booked gaps stopped being asserted.", working, declining)
	}
	t.Logf("nested axes: %d total, %d answering correctly, %d declining on a booked gap", len(axes), working, declining)
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
