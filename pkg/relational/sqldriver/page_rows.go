package sqldriver

import (
	"context"
	"database/sql"
	"sync"

	"fdb.dev/pkg/relational/api"
)

const continuationExhaustionMessage = "Continuation can only be returned once the result set has been exhausted"

// Queryer is the shared QueryContext surface implemented by *sql.DB,
// *sql.Conn, and *sql.Tx.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// PageRows is a continuation-aware, result-scoped wrapper around *sql.Rows.
// It intentionally does not embed or expose the underlying rows: observing the
// terminating Next call is what makes Continuation and Reason safe to use.
//
// A non-terminal driver page is represented to ordinary database/sql callers
// by api.ContinuationAvailableError in sql.Rows.Err. PageRows consumes only
// that typed boundary, making Err return nil and exposing the continuation on
// this exact result. Natural source exhaustion produces api.EndContinuation.
type PageRows struct {
	rows *sql.Rows

	mu           sync.Mutex
	closed       bool
	terminated   bool
	continuation api.Continuation
	boundary     *api.ContinuationAvailableError
	reason       api.ContinuationReason
	err          error
}

// QueryPage executes one SQL result page through q.
func QueryPage(
	ctx context.Context,
	q Queryer,
	query string,
	args ...any,
) (*PageRows, error) {
	if q == nil {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "queryer must not be nil")
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, api.NewError(api.ErrCodeInternalError, "QueryContext returned nil rows without an error")
	}
	return &PageRows{rows: rows}, nil
}

// Next prepares the next row for Scan. It returns false at either natural EOF,
// a visible continuation boundary, or an error; call Err to distinguish the
// latter. Continuation and Reason become available only after this terminating
// false result has been observed.
func (r *PageRows) Next() bool {
	if r == nil || r.rows == nil {
		return false
	}

	r.mu.Lock()
	if r.closed || r.terminated {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()

	if r.rows.Next() {
		return true
	}

	rowsErr := r.rows.Err()
	var (
		continuation api.Continuation
		boundary     *api.ContinuationAvailableError
		reason       api.ContinuationReason
		terminalErr  error
	)
	if rowsErr == nil {
		continuation = api.EndContinuation()
		reason = api.ContinuationCursorAfterLast
	} else {
		// Suppress only the exact top-level carrier produced by the driver.
		// errors.As would also find one inside errors.Join or a wrapper and
		// could thereby hide context cancellation, an execution failure, or
		// driver.ErrBadConn that accompanied it. The carrier is sealed by its
		// private snapshot; its exported zero value reports USER_REQUESTED and
		// is not a valid GO_V1 page reason.
		if candidate, ok := rowsErr.(*api.ContinuationAvailableError); ok &&
			candidate != nil &&
			(candidate.Reason() == api.ContinuationQueryExecutionLimitReached ||
				candidate.Reason() == api.ContinuationTransactionLimitReached) {
			boundary = candidate
			reason = candidate.Reason()
		} else {
			terminalErr = rowsErr
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		// Close won a race with iteration. That is an early close, not an
		// observed end of page, so it must not manufacture a continuation.
		return false
	}
	r.terminated = true
	r.continuation = continuation
	r.boundary = boundary
	r.reason = reason
	r.err = terminalErr
	return false
}

// Scan copies the columns from the current row into dest.
func (r *PageRows) Scan(dest ...any) error {
	if r == nil || r.rows == nil {
		return api.NewError(api.ErrCodeInvalidCursorState, "result set is not available")
	}
	return r.rows.Scan(dest...)
}

// Columns returns the result column names.
func (r *PageRows) Columns() ([]string, error) {
	if r == nil || r.rows == nil {
		return nil, api.NewError(api.ErrCodeInvalidCursorState, "result set is not available")
	}
	return r.rows.Columns()
}

// ColumnTypes returns the result column metadata.
func (r *PageRows) ColumnTypes() ([]*sql.ColumnType, error) {
	if r == nil || r.rows == nil {
		return nil, api.NewError(api.ErrCodeInvalidCursorState, "result set is not available")
	}
	return r.rows.ColumnTypes()
}

// Close releases the underlying rows. Closing before Next has returned false
// is an early close and leaves no continuation available.
func (r *PageRows) Close() error {
	if r == nil || r.rows == nil {
		return nil
	}

	r.mu.Lock()
	if !r.terminated {
		r.continuation = nil
		r.boundary = nil
		r.err = nil
	}
	r.closed = true
	r.mu.Unlock()

	err := r.rows.Close()
	if err != nil {
		r.mu.Lock()
		if r.err == nil {
			r.err = err
		}
		r.mu.Unlock()
	}
	return err
}

// Err reports an iteration error. A valid continuation boundary is a clean
// PageRows terminus and is therefore suppressed; every other error is retained.
func (r *PageRows) Err() error {
	if r == nil || r.rows == nil {
		return nil
	}

	r.mu.Lock()
	terminated := r.terminated
	err := r.err
	r.mu.Unlock()
	if terminated || err != nil {
		return err
	}
	return r.rows.Err()
}

// Continuation returns the result-scoped continuation after Next has returned
// false at natural EOF or a valid page boundary.
func (r *PageRows) Continuation() (api.Continuation, error) {
	if r == nil {
		return nil, continuationExhaustionError()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if !r.terminated || (r.continuation == nil && r.boundary == nil) {
		return nil, continuationExhaustionError()
	}
	if r.boundary != nil {
		return r.boundary.Continuation(), nil
	}
	return r.continuation, nil
}

// Reason returns the reason carried by Continuation after Next has returned
// false at natural EOF or a valid page boundary.
func (r *PageRows) Reason() (api.ContinuationReason, error) {
	if r == nil {
		return 0, continuationExhaustionError()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	if !r.terminated || (r.continuation == nil && r.boundary == nil) {
		return 0, continuationExhaustionError()
	}
	return r.reason, nil
}

func continuationExhaustionError() error {
	return api.NewError(api.ErrCodeUnsupportedOperation, continuationExhaustionMessage)
}
