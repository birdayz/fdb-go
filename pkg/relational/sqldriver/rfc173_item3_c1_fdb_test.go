package sqldriver_test

// RFC-173 item 3 commit 1 — the mixed-nesting LEFT widening's FDB row pins
// (design-ruling amendments E and F): a clustered NULL-SUPPLYING leg's
// padded rows read every buried column as NULL through the widened seed
// window (both nesting directions), and a WHERE-EXISTS over a clustered
// LEFT box flows through the existential machinery the gate auto-widened.

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestFDB_RFC173Item3_ClusteredBoxRows(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173i3c1"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173i3c1_tmpl"+
		" CREATE TABLE pa (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE pb (bid BIGINT, ref BIGINT, PRIMARY KEY (bid))"+
		" CREATE TABLE pc (cid BIGINT, w BIGINT, PRIMARY KEY (cid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173i3c1_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// pa: 1 matches, 9 doesn't (the padded row). pb⋈pc joins on ref=cid.
	if _, err := db.ExecContext(ctx, "INSERT INTO pa VALUES (1, 10), (9, 90)"); err != nil {
		t.Fatalf("seed pa: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO pb VALUES (1, 5), (2, 6)"); err != nil {
		t.Fatalf("seed pb: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO pc VALUES (5, 50), (6, 60)"); err != nil {
		t.Fatalf("seed pc: %v", err)
	}

	// Amendment E: clustered NULL-SUPPLYING leg — `pa LEFT JOIN (pb ⋈ pc)`.
	// pa.id=1 matches pb.bid=1 (ref 5 → pc.cid=5, w=50); pa.id=9 matches
	// nothing → the padded row must read BOTH buried legs' columns as NULL
	// (the null birth spans the whole multi-column window).
	t.Run("E_clustered_null_supplying", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT pa.id, b.bid, c.w FROM pb AS b JOIN pc AS c ON b.ref = c.cid RIGHT JOIN pa ON pa.id = b.bid")
		if err != nil {
			t.Fatalf("clustered null-supplying LEFT errored: %v", err)
		}
		defer rows.Close()
		got := map[int64][2]sql.NullInt64{}
		for rows.Next() {
			var id int64
			var bid, w sql.NullInt64
			if err := rows.Scan(&id, &bid, &w); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[id] = [2]sql.NullInt64{bid, w}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("rows = %d, want 2 (one matched, one padded)", len(got))
		}
		if m := got[1]; !m[0].Valid || m[0].Int64 != 1 || !m[1].Valid || m[1].Int64 != 50 {
			t.Errorf("matched row = %v, want bid=1 w=50", m)
		}
		if p := got[9]; p[0].Valid || p[1].Valid {
			t.Errorf("padded row = %v, want BOTH buried columns NULL (the null birth must span the whole window)", p)
		}
	})

	// The preserved-side twin — `(pa ⋈ pb) LEFT JOIN pc` with the ON
	// correlating the BURIED pa (the rfc153 shape, now ordinal): rows
	// unchanged from the name-model era.
	t.Run("E_clustered_preserved_buried_on", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id, c.w FROM pa AS a JOIN pb AS b ON a.id = b.bid LEFT JOIN pc AS c ON c.cid = a.id + 4")
		if err != nil {
			t.Fatalf("buried-ON preserved cluster errored: %v", err)
		}
		defer rows.Close()
		got := map[int64]sql.NullInt64{}
		for rows.Next() {
			var id int64
			var w sql.NullInt64
			if err := rows.Scan(&id, &w); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[id] = w
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		// pa(1)⋈pb(1): c.cid=5 → w=50. (pa 9 has no pb match — inner drops.)
		if len(got) != 1 || !got[1].Valid || got[1].Int64 != 50 {
			t.Errorf("rows = %v, want {1: 50}", got)
		}
	})

	// Amendment G: FULL over a LEFT box goes live with S3 (the FULL arm
	// gates before the LEFT arm, so eligibility makes `(A LEFT B) FULL C`
	// seed with a LEFT-box leg). Drain births over the nested nullable
	// window: the FULL drain's unmatched-C row pads the WHOLE box side.
	t.Run("G_full_over_left_box", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT pa.id, b.bid, c2.cid FROM pa LEFT JOIN pb AS b ON pa.id = b.bid FULL JOIN pc AS c2 ON c2.cid = pa.v / 2")
		if err != nil {
			t.Fatalf("FULL over LEFT box errored: %v", err)
		}
		defer rows.Close()
		n, boxPadded := 0, 0
		for rows.Next() {
			var id, bid, cid sql.NullInt64
			if err := rows.Scan(&id, &bid, &cid); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
			if !id.Valid {
				boxPadded++
				if bid.Valid {
					t.Errorf("FULL drain row: box side must pad WHOLE (id NULL but bid=%v)", bid)
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		// pa(1,10): v/2=5 → c2.cid=5 matches. pa(9,90): v/2=45 → no c2 → c2 NULL.
		// c2(6) unmatched → the FULL drain births a box-padded row.
		if n != 3 || boxPadded != 1 {
			t.Errorf("FULL-over-LEFT = %d rows (%d box-padded), want 3 rows with exactly 1 box-padded drain row", n, boxPadded)
		}
	})

	// Amendment F: WHERE-EXISTS over the clustered LEFT box — the gate
	// auto-widened existsOuterGatesFresh; the below-FOD machinery must
	// serve the correlated read.
	t.Run("F_exists_over_clustered_box", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT pa.id FROM pb AS b JOIN pc AS c ON b.ref = c.cid RIGHT JOIN pa ON pa.id = b.bid"+
				" WHERE EXISTS (SELECT 1 FROM pb WHERE pb.bid = pa.id)")
		if err != nil {
			t.Fatalf("EXISTS over clustered box errored: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{1}) {
			t.Errorf("EXISTS over clustered box = %v, want [1] (pa.id=1 has a pb match; 9 does not)", got)
		}
	})
}
