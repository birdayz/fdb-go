package api

import (
	"errors"
	"fmt"
	"time"
)

// TransactionTimeLimitError marks an error as the driver PRE-EMPTING an explicit
// transaction whose reads have outlived FDB's 5-second MVCC window, as opposed to
// any other condition that shares its SQLSTATE.
//
// It is a CAUSE carried under an *Error{Code: ErrCodeSerializationFailure}, never
// a replacement for it. 40001 is deliberate and stays: no limit the caller can
// raise makes an FDB transaction live longer than five seconds, and it is the same
// code translateFDBCode assigns to FDB's own transaction_too_old (1007), so retry
// logic is uniform whether the driver pre-empts or FDB does.
//
// THE CODE ALONE CANNOT SAY WHICH CONDITION OCCURRED, and that is why this type
// exists. 40001 is shared with at least two other conditions, not one: a genuine
// read/write conflict (FDB's not_committed, 1020) and a stale-plan retry
// (index_state_planning.go:270). The conflict case is the sharpest — it demands
// the OPPOSITE response, since a conflict is retried as-is and usually succeeds
// while an exhausted window is retried identically forever, the remedy being to
// commit sooner or decompose the work. A caller that cannot tell them apart
// either retries a hopeless statement or gives up on a recoverable one.
//
// JAVA'S PRECEDENT IS EXCEPTION TYPES, and it is the direct analogue of what this
// is. FDBExceptions.java gives transaction_too_old (1007),
// transaction_timed_out (1031) and not_committed (1020) DISTINCT exception
// classes — FDBStoreTransactionIsTooOldException:130,
// FDBStoreTransactionTimeoutException:120, FDBStoreTransactionConflictException:144,
// mapped from the raw FDBException at :189-197 — and callers consume them BY TYPE
// rather than by reading a code or a message.
//
// The consuming sites, in non-test main sources: three instanceof
// (LuceneExceptions.java:51 and :89, ExceptionUtil.java:64) and one catch
// (RecordContextTransaction.java:67). The rest of the references to these classes
// are javadoc {@link}, not consumption.
//
// THE TWO LUCENE SITES ARE THE PRECISE PRECEDENT, because both match
// FDBStoreTransactionIsTooOldException — window-loss itself, matched by TYPE. That
// is the same handle on the same condition this file adds, so the port is a MATCH
// rather than a Go-only elaboration.
// Continuation.Reason.TRANSACTION_LIMIT_REACHED (Continuation.java:38) is the
// analogue of the END STATE instead — the continuation boundary this becomes under
// RFC-203, per the retirement condition below.
//
// Go's interim pre-emption (RFC-198 Decision 6) collapsed the distinction into an
// untyped 40001, so the only handle anyone had was the rendered message — which
// *Error's own doc forbids parsing, and which the driver's own budget test
// nonetheless had to reach for.
//
// THE SQLSTATE IS A DELIBERATE DEPARTURE FROM JAVA, recorded here because this
// file's whole subject is which code carries which condition. Java maps
// limit-reached to 54F01 (ErrorCode.java:159) and reserves 40001 for conflict
// (ErrorCode.java:108); Go puts window-loss under 40001. The departure is
// intentional: Java's 54F01 covers scan/byte/row limits, every one of which the
// caller can RAISE, and 54F01's contract is "you hit a limit — raise it". FDB's
// MVCC window is raisable by nobody, so 54F01 would send the caller to a knob
// that does not exist. 40001 is also what translateFDBCode already assigns to
// FDB's own 1007, so the choice keeps Go internally consistent: one code, one
// condition, two producers.
//
// NOTE THE SCOPE OF THAT DEPARTURE, because it runs the opposite way for the
// condition this file is actually about. Java's SQLSTATE discriminates only the
// RAISABLE-LIMIT family, where 54F01 genuinely says "you hit a limit, raise it".
// For WINDOW-LOSS Java's code says nothing at all: 1007 is unmapped, so it
// reaches the caller as ErrorCode.UNKNOWN (see below). Go's 40001 is therefore
// MORE informative than Java's on exactly the condition marked here, not less.
//
// And the marker is not compensation for the SQLSTATE choice either way — Java
// carries the same typed distinction BELOW its SQLSTATE regardless, in
// FDBExceptions, and consumes it by instanceof. Both implementations reach the
// same place: the code is too coarse for this condition, so the condition gets a
// type.
//
// The RETIREMENT CONDITION is the one RFC-198 Decision 6 already states: when
// RFC-203's in-transaction continuation surface lands, this stops being an error
// and becomes a continuation boundary carrying Java's TRANSACTION_LIMIT_REACHED
// reason. The type is what that boundary will be named from; it is not deleted
// then, it is moved.

// TimeLimitSource names WHICH ceiling stopped the transaction.
//
// THE CONDITION HAS EXACTLY TWO PRODUCERS, and this is where that set is stated.
// Both attach the marker, so a consumer asks ONE question rather than enumerating
// spellings — which is the bug this type was introduced to close. A detector that
// named one producer and missed the other shipped green and took a CI lane down
// the first time the box was slow enough for the other to fire.
//
//   - TimeLimitPreempted — the driver's own budget check
//     (paginatingRows.preflightTxBudget) at 4s. On this driver it is the
//     COMMON case by a wide margin: it fires a full second before FDB's wall,
//     so a caller inside an explicit transaction almost never sees the other.
//   - TimeLimitFDBTooOld — FDB's transaction_too_old (1007) at 5s, attached by
//     translateFDBCode. It is the residual: it arrives when the driver could not
//     pre-empt, which is any read outside the page preflight or a backend that
//     reports no read-version instant (the cgo escape hatch has no accessor).
//
// HOW RESIDUAL 1007 IS, corroborated by the reference implementation: Java's
// relational layer does not map it AT ALL. ExceptionUtil.java's instanceof chain
// has an arm for FDBStoreTransactionTimeoutException (1031) and none for
// FDBStoreTransactionIsTooOldException (1007) — zero references to that class in
// fdb-relational-core — so a 1007 reaching Java falls through to the default
// ErrorCode.UNKNOWN at ExceptionUtil.java:63. Java has the identical two-ceiling
// structure (its own scan/time limits stop the cursor before FDB's wall) with the
// identical consequence: nobody ever needed to map the ceiling that does not
// fire. That is exactly the trap the Go-side detector fell into — matching the
// unreachable ceiling and missing the one that actually stops the query — which
// is why BOTH are marked here rather than only the common one.
//
// A THIRD SPELLING WOULD BE A BUG IN THIS LIST, not in the consumers. Adding a
// producer means adding a constant here and attaching the marker there; nothing
// downstream changes. The near misses that must NOT be marked are recorded
// because they are the tempting mistakes: FDB's not_committed (1020) also maps to
// 40001 but is a genuine conflict, and transaction_timed_out (1031) is the
// client's own transaction timeout rather than the MVCC window.
type TimeLimitSource string

const (
	// TimeLimitPreempted is the driver's read-budget pre-emption.
	TimeLimitPreempted TimeLimitSource = "driver-preempted"
	// TimeLimitFDBTooOld is FDB's own transaction_too_old (1007).
	TimeLimitFDBTooOld TimeLimitSource = "fdb-transaction-too-old"
)

type TransactionTimeLimitError struct {
	// Source names which of the two producers raised it. Callers that only need
	// "was the window lost" ignore it; it exists so a run can record WHICH
	// ceiling bound it, which is the fact that distinguishes a healthy driver
	// from one whose pre-emption stopped working.
	Source TimeLimitSource
	// Elapsed is how long the transaction's MVCC window had been open, measured
	// on the environment's clock from the read-version instant. ZERO when Source
	// is TimeLimitFDBTooOld: FDB reports that the version aged out, not by how
	// much, and inventing a number here would be worse than admitting none.
	Elapsed time.Duration
	// Limit is the budget that was exceeded — the driver's own ceiling, set below
	// FDB's 5-second wall so the pre-emption happens first. Zero alongside
	// Elapsed for the same reason.
	Limit time.Duration

	// cause keeps the underlying FDB error reachable through this marker, so
	// inserting the marker into the chain does not hide the raw fdb.Error /
	// *wire.FDBError from anything that already matched on it. Unexported: it is
	// a chain link, not a field callers read.
	cause error
}

// Unwrap keeps the original FDB error visible to errors.As/Is through the marker.
func (e *TransactionTimeLimitError) Unwrap() error { return e.cause }

func (e *TransactionTimeLimitError) Error() string {
	if e.Limit == 0 {
		return string(e.Source)
	}
	return fmt.Sprintf("read version %v old, budget %v",
		e.Elapsed.Round(time.Millisecond), e.Limit)
}

// NewTransactionTimeLimitError builds the 40001 the driver returns when an
// explicit transaction's read budget is exhausted, with the typed marker as its
// cause. The message is the caller-facing remedy; the marker is what code matches.
func NewTransactionTimeLimitError(elapsed, limit time.Duration) *Error {
	return WrapError(ErrCodeSerializationFailure,
		"transaction read budget exhausted: the explicit transaction's reads are bound to FDB's 5-second MVCC window, which opened at the transaction's first read; commit sooner or decompose the work into smaller transactions",
		&TransactionTimeLimitError{
			Source: TimeLimitPreempted, Elapsed: elapsed, Limit: limit,
		})
}

// MarkFDBTransactionTooOld wraps FDB's own transaction_too_old (1007) with the
// same marker the driver's pre-emption carries, so that ONE predicate answers
// "did this transaction outlive its MVCC window" whichever ceiling enforced it.
//
// Called from translateFDBCode's 1007 arm — beside the SQLSTATE mapping rather
// than anywhere else, because the two must not be able to drift: the code says
// 40001 and the marker says why, and a future edit that changes one without the
// other is visible in a single switch case.
func MarkFDBTransactionTooOld(cause error) *Error {
	return WrapError(ErrCodeSerializationFailure, "FDB transaction too old",
		&TransactionTimeLimitError{Source: TimeLimitFDBTooOld, cause: cause})
}

// IsTransactionTimeLimit reports whether err says the transaction outlived FDB's
// MVCC window, from EITHER producer — the driver's read-budget pre-emption or
// FDB's own transaction_too_old. This is the single question consumers ask; they
// never enumerate the producers themselves.
//
// Typed, never a string match: *Error explicitly documents that its rendered
// wording is not API, and a recogniser built on the wording breaks silently the
// first time the remedy sentence is reworded.
//
// It is deliberately NARROWER than "SQLSTATE is 40001". A serialization failure
// from a genuine conflict must NOT satisfy this predicate — a caller (or a test)
// that treats every 40001 as an exhausted window reads a real conflict bug as
// weather.
func IsTransactionTimeLimit(err error) bool {
	var e *TransactionTimeLimitError
	return errors.As(err, &e)
}
