package testkit

// Composite-primary-key axis of the indexed/unindexed twin.
//
// Every other twin in this family keys its table on a single `id`. That is the
// one shape under which a primary-key intersection can never be built over a
// leg that fixes a primary-key component: a single-component key fixed in a leg
// makes that leg max-cardinality 1, and the partition is pruned as redundant.
// With PRIMARY KEY (pk1, pk2) the planner CAN build such a merge, and did —
// intersecting the (b, pk1) and (pk2) covering scans on (pk1) alone, so that
// `WHERE b = 1 AND pk2 = 3` returned every b = 1 record whose pk1 also had some
// pk2 = 3 record (pk_intersection_leg_bound_component_fdb_test.go pins the
// repair; RFC-245 has the write-up). This file is the net that found it and the
// axis nothing else here reaches: a composite primary key, a three-column
// mixed-type index, and indexes that repeat primary-key components.
//
// Two sweeps. The READ sweep runs every query against both schemas and then
// again on the indexed side through a pinned connection with a scanned-rows
// limit of 3, so it pages through continuations: a twin difference is a
// planner-soundness finding, a paged/one-shot difference is a continuation
// finding. The DML sweep applies the same UPDATE/DELETE/INSERT sequence to both
// schemas — predicates that reach the intersection shapes, index-key columns
// set to NULL and back, aggregate indexes maintained across every statement —
// and compares the whole table and the index-backed reads after each one.
//
// The oracle is the engine's own full scan, so it is blind to a defect shared
// by both paths; those are pinned by hand-expected tests. Each sweep carries
// non-vacuity floors stated with the population they were measured over.

import (
	"context"
	"database/sql"
	"strings"
)

// mhcpkRowsOnConn is mmRows over a pinned *sql.Conn (the paged reader).
func MhcpkRowsOnConn(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			v := c.(*sql.NullString)
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
