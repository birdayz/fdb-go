package embedded

import (
	"math"
	"testing"
)

// TestPageRowBudgetKeysOnStatementKindNotRouting pins RFC-198 Decision 4: the
// page-scan bound (pageRowBudget) keys on the STATEMENT-KIND fact (isUpdate),
// never on the transaction-routing fact (tx != nil). The two used to share one
// field (`respectActiveTx`), and redefining that field to mean "in an explicit
// transaction" would have silently unbounded the page scan of every in-tx
// SELECT that sets MAX_ROWS — a performance regression invisible to any test
// that only checks returned rows, which is why this asserts on the bound
// itself (criterion 8).
//
// Mutation direction: gate the budget on r.tx != nil (the conflated meaning)
// → the in-tx SELECT case below returns 0 and this test reddens.
func TestPageRowBudgetKeysOnStatementKindNotRouting(t *testing.T) {
	t.Parallel()

	inTx := &embeddedTx{}

	cases := []struct {
		name string
		r    paginatingRows
		want int
	}{
		{
			// The load-bearing case: an in-transaction SELECT with MAX_ROWS
			// keeps its page bound even though it now routes through the
			// explicit transaction.
			name: "in-tx SELECT with MAX_ROWS keeps its page bound",
			r:    paginatingRows{tx: inTx, isUpdate: false, maxRows: 5},
			want: 5,
		},
		{
			name: "in-tx DML is never scan-bounded by the returned-row cap",
			r:    paginatingRows{tx: inTx, isUpdate: true, maxRows: 5},
			want: 0,
		},
		{
			name: "auto-commit DML is never scan-bounded either",
			r:    paginatingRows{tx: nil, isUpdate: true, maxRows: 5},
			want: 0,
		},
		{
			name: "auto-commit SELECT with MAX_ROWS keeps its page bound",
			r:    paginatingRows{tx: nil, isUpdate: false, maxRows: 5},
			want: 5,
		},
		{
			name: "in-tx SELECT budget shrinks by rows already emitted",
			r:    paginatingRows{tx: inTx, isUpdate: false, maxRows: 5, emitted: 3},
			want: 2,
		},
		{
			name: "no cap leaves the page unbounded",
			r:    paginatingRows{tx: inTx, isUpdate: false, maxRows: math.MaxInt32},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.r.pageRowBudget(); got != tc.want {
				t.Errorf("pageRowBudget() = %d, want %d — the bound must key on isUpdate "+
					"(statement kind), independent of tx (routing); see RFC-198 Decision 4",
					got, tc.want)
			}
		})
	}
}
