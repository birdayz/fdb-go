package executor

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
)

// positionReplayCursor gives a DETERMINISTIC eagerly-materialized cursor a
// correct resume: the continuation carries {emitted count, the encoded last
// emitted row}, and a resume RE-EXECUTES the base (recursion is a pure
// function of the store) then skips the already-emitted prefix, verifying
// the row at the resume boundary still encodes identically — data drift
// under the token fails LOUDLY instead of re-emitting or skipping rows.
//
// This is the resume layer for the DISTINCT recursion arms — a Go
// extension (Java rejects recursive UNION distinct outright: "only UNION
// ALL is supported"), whose cross-level seen-set makes the recursion
// inherently materialized: the seen-set cannot ride a streaming
// continuation (it is the whole emission history), but the re-execution
// rebuilds it deterministically. The re-execution cost per page matches
// the pre-existing eager arms (the Tier-3 restart-inefficiency class).
type positionReplayCursor struct {
	base recordlayer.RecordCursor[QueryResult]

	emitted     int64
	lastRowEnc  []byte
	resumeSkip  int64
	resumeCheck []byte

	closed bool
}

func newPositionReplayCursor(
	_ context.Context,
	baseFn func() (recordlayer.RecordCursor[QueryResult], error),
	continuation []byte,
) (*positionReplayCursor, error) {
	c := &positionReplayCursor{}
	if continuation != nil {
		posVal, rest, err := readContValue(continuation)
		if err != nil {
			return nil, &recordlayer.ContinuationParseError{Message: "replay continuation: position", RawBytes: continuation, Cause: err}
		}
		pos, ok := posVal.(int64)
		if !ok || pos <= 0 {
			return nil, &recordlayer.ContinuationParseError{Message: fmt.Sprintf("replay continuation: position is %T(%v), want positive int64", posVal, posVal), RawBytes: continuation}
		}
		checkVal, _, err := readContValue(rest)
		if err != nil {
			return nil, &recordlayer.ContinuationParseError{Message: "replay continuation: check row", RawBytes: continuation, Cause: err}
		}
		check, ok := checkVal.([]byte)
		if !ok || len(check) == 0 {
			return nil, &recordlayer.ContinuationParseError{Message: fmt.Sprintf("replay continuation: check row is %T, want bytes", checkVal), RawBytes: continuation}
		}
		c.resumeSkip = pos
		c.resumeCheck = check
	}
	base, err := baseFn()
	if err != nil {
		return nil, err
	}
	c.base = base
	return c, nil
}

func (c *positionReplayCursor) continuation() (recordlayer.RecordCursorContinuation, error) {
	buf, err := appendContValue(nil, c.emitted)
	if err != nil {
		return nil, err
	}
	buf, err = appendContValue(buf, c.lastRowEnc)
	if err != nil {
		return nil, err
	}
	return recordlayer.NewBytesContinuation(buf), nil
}

func (c *positionReplayCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for c.emitted < c.resumeSkip {
		r, err := c.base.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !r.HasNext() {
			if !r.GetNoNextReason().IsSourceExhausted() {
				// A resource limit cut the REPLAY itself short (an
				// out-of-band base — today's eager bases stop only
				// in-band, but the contract must not lie). Nothing
				// advanced: re-emit the INCOMING token verbatim so a
				// retry under a bigger budget resumes correctly.
				buf, encErr := appendContValue(nil, c.resumeSkip)
				if encErr == nil {
					buf, encErr = appendContValue(buf, c.resumeCheck)
				}
				if encErr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, encErr
				}
				return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), recordlayer.NewBytesContinuation(buf)), nil
			}
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf(
				"replay resume: the recursion produced %d rows, the continuation claims %d were emitted — the data changed under the token", c.emitted, c.resumeSkip)
		}
		c.emitted++
		if c.emitted == c.resumeSkip {
			enc, encErr := encodeRecursiveRow(r.GetValue())
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			if !bytes.Equal(enc, c.resumeCheck) {
				return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf(
					"replay resume: row %d no longer matches the continuation's boundary row — the data changed under the token", c.resumeSkip)
			}
			c.lastRowEnc = enc
		}
	}
	r, err := c.base.OnNext(ctx)
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	if !r.HasNext() {
		if r.GetNoNextReason().IsSourceExhausted() {
			return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
		}
		if c.emitted == 0 {
			// No row emitted yet: resume restarts from the beginning.
			return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), &recordlayer.StartContinuation{}), nil
		}
		cont, cerr := c.continuation()
		if cerr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, cerr
		}
		return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), cont), nil
	}
	enc, err := encodeRecursiveRow(r.GetValue())
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	c.emitted++
	c.lastRowEnc = enc
	cont, err := c.continuation()
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	return recordlayer.NewResultWithValue(r.GetValue(), cont), nil
}

func (c *positionReplayCursor) Close() error {
	c.closed = true
	return c.base.Close()
}

func (c *positionReplayCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*positionReplayCursor)(nil)
