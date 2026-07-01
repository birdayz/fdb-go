package client

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

func fdbErr(code int) error { return &wire.FDBError{Code: code} }

func srv(addr string) ServerInfo { return ServerInfo{Address: addr} }

// TestResolveFallback pins finding #22: the sequential get-value fallback used to match only
// timeout / wrong_shard / all_alternatives and let a future_version (1009) / process_behind (1037) reply
// from a fallback replica fall through — silently dropped and flattened to errReplyTimeout (1007) or
// all_alternatives_failed (1006), hiding a retryable condition. C++ getValue re-throws any error that is
// not wrong_shard_server / all_alternatives_failed (NativeAPI.actor.cpp:3738), so a version error MUST
// surface. resolveFallback now remembers it and surfaces it with precedence:
//
//	future_version / process_behind  >  reply-timeout  >  all_alternatives_failed
//
// Revert-proof for the fix: drop the `case isFutureVersionOrProcessBehind(err)` arm and the first two
// subtests go red (the version error is dropped → 1007/1006 surfaces instead).
func TestResolveFallback(t *testing.T) {
	t.Parallel()
	// best=0, second=1 → the fallback scan tries s2 then s3.
	servers := []ServerInfo{srv("s0"), srv("s1"), srv("s2"), srv("s3")}

	t.Run("version_err_surfaced_not_masked_as_timeout", func(t *testing.T) {
		t.Parallel()
		// Hedge timed out; every fallback replica reports future_version. The old loop returned
		// errReplyTimeout (1007); the fix surfaces 1009.
		_, err := resolveFallback(errReplyTimeout, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			return nil, fdbErr(ErrFutureVersion)
		})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrFutureVersion {
			t.Fatalf("want future_version (1009) surfaced, got %v", err)
		}
	})

	t.Run("version_beats_timeout", func(t *testing.T) {
		t.Parallel()
		_, err := resolveFallback(nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
			if s.Address == "s2" {
				return nil, errReplyTimeout
			}
			return nil, fdbErr(ErrProcessBehind) // s3
		})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrProcessBehind {
			t.Fatalf("a version error must take precedence over a timeout; got %v", err)
		}
	})

	t.Run("real_value_wins_over_remembered_version_err", func(t *testing.T) {
		t.Parallel()
		val, err := resolveFallback(nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
			if s.Address == "s2" {
				return nil, fdbErr(ErrFutureVersion)
			}
			return []byte("v"), nil // s3 has the value
		})
		if err != nil || string(val) != "v" {
			t.Fatalf("a later replica's real value must win over a remembered version error; got val=%q err=%v", val, err)
		}
	})

	t.Run("wrong_shard_stops_immediately", func(t *testing.T) {
		t.Parallel()
		tried := 0
		_, err := resolveFallback(nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			tried++
			return nil, fdbErr(ErrWrongShardServer)
		})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrWrongShardServer {
			t.Fatalf("wrong_shard_server must surface immediately; got %v", err)
		}
		if tried != 1 {
			t.Fatalf("wrong_shard_server must end the scan on the first replica; tried %d", tried)
		}
	})

	t.Run("all_timeout_flattens_to_reply_timeout", func(t *testing.T) {
		t.Parallel()
		_, err := resolveFallback(nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			return nil, errReplyTimeout
		})
		if !errors.Is(err, errReplyTimeout) {
			t.Fatalf("all-timeout must flatten to errReplyTimeout; got %v", err)
		}
	})

	t.Run("all_alternatives_returns_immediately", func(t *testing.T) {
		t.Parallel()
		// An all_alternatives_failed reply hits the early-return case (like wrong_shard) and stops the
		// scan — it does NOT fall through to the post-loop default branch.
		tried := 0
		_, err := resolveFallback(nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			tried++
			return nil, fdbErr(ErrAllAlternativesFailed)
		})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrAllAlternativesFailed {
			t.Fatalf("want all_alternatives_failed (1006); got %v", err)
		}
		if tried != 1 {
			t.Fatalf("all_alternatives_failed must end the scan on the first replica; tried %d", tried)
		}
	})

	t.Run("default_flattens_unclassified_error_to_all_alternatives", func(t *testing.T) {
		t.Parallel()
		// An error matching NO switch arm (not ctx / version / timeout / wrong-shard / all-alternatives)
		// is dropped and the scan continues; with no version error and no timeout remembered, the post-loop
		// DEFAULT branch flattens to all_alternatives_failed. This exercises that default (Torvalds nit:
		// the old subtest fed all_alternatives and hit the early-return case, leaving the default untested).
		tried := 0
		_, err := resolveFallback(nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			tried++
			return nil, fdbErr(1000) // operation_failed — unclassified here
		})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrAllAlternativesFailed {
			t.Fatalf("default branch must flatten an unclassified error to all_alternatives_failed (1006); got %v", err)
		}
		if tried != 2 {
			t.Fatalf("an unclassified error must NOT stop the scan; want both fallback replicas tried, got %d", tried)
		}
	})

	t.Run("hedge_timeout_seeds_surfaced_error_with_no_fallback_servers", func(t *testing.T) {
		t.Parallel()
		two := []ServerInfo{srv("s0"), srv("s1")} // best=0, second=1 → no untried fallback servers
		called := false
		_, err := resolveFallback(errReplyTimeout, two, 0, 1, func(ServerInfo) ([]byte, error) {
			called = true
			return nil, nil
		})
		if called {
			t.Fatal("no fallback servers should be tried when best+second cover all replicas")
		}
		if !errors.Is(err, errReplyTimeout) {
			t.Fatalf("the hedge's own timeout must seed the surfaced errReplyTimeout; got %v", err)
		}
	})

	// TestResolveFallback_ContextCancel* pin the codex P2 fix: a cancelled/expired context must
	// propagate IMMEDIATELY and never be masked by a remembered version error (a cancelled Get must
	// return ctx.Err(), not a stale future_version). Revert-proof: drop the two ctx switch/guard cases
	// in resolveFallback → these go red (the version error / all_alternatives surfaces instead).

	t.Run("context_cancel_after_version_err_propagates", func(t *testing.T) {
		t.Parallel()
		// s2 returns future_version (remembered); then the caller/tx ctx is cancelled and s3's trySingle
		// returns context.Canceled. Without the fix the loop drops it and the post-loop surfaces the
		// remembered future_version — masking the cancellation. With the fix, ctx.Canceled wins.
		_, err := resolveFallback(nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
			if s.Address == "s2" {
				return nil, fdbErr(ErrFutureVersion)
			}
			return nil, context.Canceled // s3
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during the fallback scan must propagate, not be masked by a remembered version error; got %v", err)
		}
	})

	t.Run("hedge_context_cancel_propagates_immediately", func(t *testing.T) {
		t.Parallel()
		// The hedge itself failed with a deadline; no fallback should be tried and the ctx error surfaces
		// directly (covers the no-untried-fallback path where the loop can't catch it).
		called := false
		_, err := resolveFallback(context.DeadlineExceeded, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			called = true
			return nil, nil
		})
		if called {
			t.Fatal("a cancelled hedge must not scan fallbacks — the ctx error propagates immediately")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hedge deadline must propagate; got %v", err)
		}
	})
}
