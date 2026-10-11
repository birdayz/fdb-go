package client

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// Every successful GRV, commit and read reply classifies a nil error. Not
// parallel: AllocsPerRun panics under t.Parallel and counts process-wide.
func TestNilReplyErrorClassifiesWithoutAllocating(t *testing.T) {
	for name, classify := range map[string]func(error) bool{
		"isMaybeDelivered":               isMaybeDelivered,
		"isWrongShardServer":             isWrongShardServer,
		"isAllAlternativesFailed":        isAllAlternativesFailed,
		"isFutureVersionOrProcessBehind": isFutureVersionOrProcessBehind,
		"isFutureVersion":                isFutureVersion,
		"isOperationFailed":              isOperationFailed,
	} {
		if n := testing.AllocsPerRun(100, func() { _ = classify(nil) }); n != 0 {
			t.Errorf("REPLY_ERR_ALLOC: %s(nil) allocated %v times, want 0", name, n)
		}
	}
	if n := testing.AllocsPerRun(100, func() { _ = dispositionForReplyError(nil, true) }); n != 0 {
		t.Errorf("REPLY_ERR_ALLOC: dispositionForReplyError(nil) allocated %v times, want 0", n)
	}
	wrapped := errors.Join(errors.New("context"), &wire.FDBError{Code: ErrWrongShardServer})
	if !isWrongShardServer(wrapped) || isAllAlternativesFailed(wrapped) || isMaybeDelivered(wrapped) {
		t.Error("REPLY_ERR_CLASSIFY: a wrapped 1001 is no longer classified by its code")
	}
}
