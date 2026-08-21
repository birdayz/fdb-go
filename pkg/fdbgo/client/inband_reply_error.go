package client

// IN-BAND REPLY ERRORS: WHAT C++ basicLoadBalance DOES, AS ONE RULE.
//
// A proxy that is dying does not necessarily drop the connection. It can answer
// the RPC with an error INSIDE the ErrorOr<> reply -- broken_promise (1100) is
// the characteristic one. That arrives at the reply-parse boundary, not at any
// of the transport arms, so it is invisible to every teardown mapping the
// transport does (those all mint 1030, see transport/conn.go).
//
// C++ never lets that reach the application. basicLoadBalance
// (LoadBalance.actor.h:812-830) treats 1100 and 1030 as ONE class and then
// branches on AtMostOnce:
//
//	if err is not (broken_promise | request_maybe_delivered) -> throw it
//	if atMostOnce                                            -> throw request_maybe_delivered
//	otherwise                                                -> try the next alternative
//
// Go had no equivalent at either reply boundary: `grv.go` returned the parsed
// error verbatim and `commitpath.go` returned it unchanged, and 1100 is
// retryable under NONE of the three Go predicates (fdb.IsRetryable,
// IsOnErrorRetryable, client.onErrorRetryable). So a GRV or commit issued while
// a proxy was dying failed TERMINALLY where C++ retries or converts.
//
// The rule lives here, once, because the two call sites disagree on exactly one
// input -- their AtMostOnce setting -- and inlining it twice is how the two
// drift apart.
type replyDisposition int

const (
	// replySurfaceError: not in the maybeDelivered class. C++ rethrows it
	// unchanged and so does Go.
	replySurfaceError replyDisposition = iota
	// replyTryNextAlternative: maybeDelivered at AtMostOnce::False. GRV uses
	// this (NativeAPI.actor.cpp:3865) -- ask another proxy.
	replyTryNextAlternative
	// replyConvertToMaybeDelivered: maybeDelivered at AtMostOnce::True. Commit
	// uses this (NativeAPI.actor.cpp:6638-6643). The request must NOT be
	// re-sent -- it may already have been applied -- so the outcome is reported
	// as unknown instead.
	replyConvertToMaybeDelivered
)

// dispositionForReplyError classifies an in-band reply error exactly as
// basicLoadBalance does. A nil error is not a disposition question; callers
// check that first, and it maps to replySurfaceError so a mistaken call cannot
// invent a retry.
func dispositionForReplyError(err error, atMostOnce bool) replyDisposition {
	if err == nil || !isMaybeDelivered(err) {
		return replySurfaceError
	}
	if atMostOnce {
		return replyConvertToMaybeDelivered
	}
	return replyTryNextAlternative
}
