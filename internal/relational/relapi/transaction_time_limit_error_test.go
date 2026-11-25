package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestTransactionTimeLimitError_IsNarrowerThanItsSQLSTATE is the whole point of
// the type existing, stated as a test.
//
// 40001 is deliberately SHARED between the driver's read-budget pre-emption and a
// genuine read/write conflict, so that retry logic is uniform. That sharing is
// exactly what makes the code alone insufficient to identify either: a conflict
// is retried as-is and usually succeeds, an exhausted MVCC window is retried
// identically forever. A predicate that answered "is the SQLSTATE 40001" would
// classify every conflict as an exhausted window.
func TestTransactionTimeLimitError_IsNarrowerThanItsSQLSTATE(t *testing.T) {
	t.Parallel()
	preempt := NewTransactionTimeLimitError(4200*time.Millisecond, 4*time.Second)
	conflict := WrapError(ErrCodeSerializationFailure,
		"transaction not committed due to conflict", errors.New("not_committed"))

	if !IsTransactionTimeLimit(preempt) {
		t.Fatalf("IsTransactionTimeLimit did not recognise the error its own "+
			"constructor produced: %v", preempt)
	}
	if IsTransactionTimeLimit(conflict) {
		t.Fatal("IsTransactionTimeLimit accepted a genuine serialization CONFLICT. " +
			"The two share SQLSTATE 40001 and demand opposite responses — retry " +
			"as-is versus decompose the work — so a predicate that cannot separate " +
			"them makes one of the two always handled wrongly.")
	}
	// Both must still be 40001: the marker ADDS a distinction, it never replaces
	// the code, because retry logic keys on the code.
	for name, err := range map[string]error{"pre-emption": preempt, "conflict": conflict} {
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%s is not an *api.Error: %v", name, err)
		}
		if e.Code != ErrCodeSerializationFailure {
			t.Fatalf("%s carries SQLSTATE %s, want %s — the marker must not change "+
				"the code, or uniform retry handling breaks",
				name, e.Code, ErrCodeSerializationFailure)
		}
	}
}

// TestTransactionTimeLimitError_BothProducersAnswerOnePredicate is the point of
// unifying the two spellings, stated as a test.
//
// The condition "this transaction outlived FDB's 5-second MVCC window" has two
// producers — the driver's 4 s pre-emption and FDB's own 1007 at 5 s — and the
// defect that motivated this type was a consumer that enumerated ONE of them. So
// the invariant is not "the marker exists", it is that ONE predicate answers for
// BOTH, and that each still says which ceiling bound it.
func TestTransactionTimeLimitError_BothProducersAnswerOnePredicate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		err        error
		wantSource TimeLimitSource
	}{
		{
			"the driver's pre-emption",
			NewTransactionTimeLimitError(4200*time.Millisecond, 4*time.Second),
			TimeLimitPreempted,
		},
		{
			"FDB's own transaction_too_old",
			MarkFDBTransactionTooOld(errors.New("fdb error 1007")),
			TimeLimitFDBTooOld,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !IsTransactionTimeLimit(tc.err) {
				t.Fatalf("IsTransactionTimeLimit is false for %s (%v). Both producers "+
					"mean the same thing to a caller; a predicate that answers for only "+
					"one forces every consumer to enumerate spellings, and the consumer "+
					"that enumerated only 1007 is what took a CI lane down.",
					tc.name, tc.err)
			}
			var ttl *TransactionTimeLimitError
			if !errors.As(tc.err, &ttl) {
				t.Fatalf("no marker extractable from %s", tc.name)
			}
			if ttl.Source != tc.wantSource {
				t.Fatalf("%s reports Source %q, want %q — unifying the predicate must "+
					"not erase WHICH ceiling bound the transaction, which is how a "+
					"driver whose pre-emption stopped working would look identical to "+
					"a healthy one", tc.name, ttl.Source, tc.wantSource)
			}
		})
	}
}

// TestMarkFDBTransactionTooOld_KeepsTheRawErrorReachable pins that inserting the
// marker into the chain does not HIDE the FDB error underneath it. Code that
// already matched the raw fdb.Error must keep working — a marker that improves
// one consumer by blinding another is a net loss.
func TestMarkFDBTransactionTooOld_KeepsTheRawErrorReachable(t *testing.T) {
	t.Parallel()
	raw := errors.New("fdb error 1007: transaction_too_old")
	marked := MarkFDBTransactionTooOld(raw)
	if !errors.Is(marked, raw) {
		t.Fatalf("the raw FDB error is no longer reachable through the marker: %v.\n"+
			"MarkFDBTransactionTooOld inserts a link in the chain; severing the tail "+
			"would silently break every existing errors.As on the FDB error type.",
			marked)
	}
	var e *Error
	if !errors.As(marked, &e) || e.Code != ErrCodeSerializationFailure {
		t.Fatalf("the marked 1007 is not a 40001 *api.Error: %v", marked)
	}
}

// TestTransactionTimeLimitError_SurvivesWrapping pins that the marker reaches a
// caller through the wrapping the driver and database/sql actually apply. A
// marker only findable on the bare value would be dead at every real call site.
func TestTransactionTimeLimitError_SurvivesWrapping(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("page fetch: %w",
		fmt.Errorf("execute: %w", NewTransactionTimeLimitError(5*time.Second, 4*time.Second)))
	if !IsTransactionTimeLimit(wrapped) {
		t.Fatalf("the marker did not survive two layers of fmt.Errorf: %v", wrapped)
	}
	var ttl *TransactionTimeLimitError
	if !errors.As(wrapped, &ttl) {
		t.Fatalf("errors.As could not extract the marker from %v", wrapped)
	}
	if ttl.Elapsed != 5*time.Second || ttl.Limit != 4*time.Second {
		t.Fatalf("the marker lost its measurements: elapsed=%v limit=%v, want 5s and 4s",
			ttl.Elapsed, ttl.Limit)
	}
}

// TestTransactionTimeLimitError_MessageKeepsTheRemedy pins the human half. The
// marker tells code WHAT happened; the message must still tell an operator what
// to DO, and both the diagnosis and the remedy have to survive together.
func TestTransactionTimeLimitError_MessageKeepsTheRemedy(t *testing.T) {
	t.Parallel()
	msg := NewTransactionTimeLimitError(4500*time.Millisecond, 4*time.Second).Error()
	for _, want := range []string{
		"40001",              // the SQLSTATE a client sees
		"5-second MVCC",      // the diagnosis
		"decompose the work", // the remedy
		"4.5s",               // how far past the budget this run actually got
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the rendered pre-emption does not contain %q: %s", want, msg)
		}
	}
}
