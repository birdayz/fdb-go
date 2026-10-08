package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// dusrvDrainTxErr is dusrvDrainTx that RETURNS its driver error, which is what
// a retried transaction body needs. A drain crosses many page boundaries by
// design, and every one of those pages is a place the whole-transaction budget
// pre-emption can arrive; fatalling there would end the test before the retry
// loop could classify the error.
func DusrvDrainTxErr(ctx context.Context, tx *sql.Tx, q string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan %q: %w", q, err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err %q: %w", q, err)
	}
	return out, nil
}

// explainPlanOnErr is explainPlanOn that RETURNS its driver error, for use
// inside a retried transaction body. An EXPLAIN issued on a transaction is a
// read page like any other and can be pre-empted for outliving the MVCC window.
func ExplainPlanOnErr(ctx context.Context, q DusrvQueryer, stmt string) (string, error) {
	rows, err := q.QueryContext(ctx, "EXPLAIN "+stmt)
	if err != nil {
		return "", fmt.Errorf("EXPLAIN %s: %w", stmt, err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan explain %s: %w", stmt, err)
		}
		for _, v := range vals {
			switch s := v.(type) {
			case string:
				plan.WriteString(s)
			case []byte:
				plan.Write(s)
			}
			plan.WriteString(" ")
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("explain rows.Err %s: %w", stmt, err)
	}
	return plan.String(), nil
}

// dusrvQueryer is satisfied by *sql.DB, *sql.Conn and *sql.Tx alike.
type DusrvQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
