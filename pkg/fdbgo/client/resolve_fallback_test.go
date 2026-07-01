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
	live := context.Background // a never-cancelled read context

	t.Run("version_err_surfaced_not_masked_as_timeout", func(t *testing.T) {
		t.Parallel()
		// Hedge timed out; every fallback replica reports future_version. The old loop returned
		// errReplyTimeout (1007); the fix surfaces 1009.
		_, err := resolveFallback(live(), errReplyTimeout, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			return nil, fdbErr(ErrFutureVersion)
		})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != ErrFutureVersion {
			t.Fatalf("want future_version (1009) surfaced, got %v", err)
		}
	})

	t.Run("version_beats_timeout", func(t *testing.T) {
		t.Parallel()
		_, err := resolveFallback(live(), nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
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
		val, err := resolveFallback(live(), nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
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
		_, err := resolveFallback(live(), nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
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
		_, err := resolveFallback(live(), nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
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
		_, err := resolveFallback(live(), nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
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
		// An error matching NO switch arm (not version / timeout / wrong-shard / all-alternatives) with a
		// LIVE ctx is dropped and the scan continues; with no version error and no timeout remembered, the
		// post-loop DEFAULT branch flattens to all_alternatives_failed. This exercises that default (Torvalds
		// nit: the old subtest fed all_alternatives and hit the early-return case, leaving the default untested).
		tried := 0
		_, err := resolveFallback(live(), nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
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
		_, err := resolveFallback(live(), errReplyTimeout, two, 0, 1, func(ServerInfo) ([]byte, error) {
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

	// The next three pin the codex context-handling fixes. The discriminator is the CALLER's read context
	// (ctx.Err()), NOT the per-server error type: a cancelled read must return ctx.Err() and never be
	// masked by a remembered version error, while a per-server dial/RPC timeout (a wrapped
	// DeadlineExceeded under the db-scoped DefaultRPCTimeout) that arrives while the read ctx is still LIVE
	// must fall back to other replicas, not abort the scan. Revert-proofs are called out per subtest.

	t.Run("context_cancel_mid_scan_propagates_over_remembered_version_err", func(t *testing.T) {
		t.Parallel()
		// s2 returns future_version (remembered); then the CALLER cancels and s3's trySingle observes it.
		// The post-loop must NOT surface the remembered future_version — the cancellation wins.
		// Revert-proof: drop the in-loop `if ctx.Err() != nil` guard → the s3 error is dropped and the
		// remembered future_version surfaces instead of ctx.Canceled.
		ctx, cancel := context.WithCancel(context.Background())
		_, err := resolveFallback(ctx, nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
			if s.Address == "s2" {
				return nil, fdbErr(ErrFutureVersion)
			}
			cancel() // caller cancels the read before s3 completes
			return nil, context.Canceled
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled read must return ctx.Err(), not a remembered version error; got %v", err)
		}
	})

	t.Run("precancelled_ctx_returns_immediately_without_fallback", func(t *testing.T) {
		t.Parallel()
		// The read ctx is already done at entry → propagate immediately, scan no replicas.
		// Revert-proof: drop the entry `if ctx.Err() != nil` guard → the loop runs (called=true) and
		// surfaces errReplyTimeout instead of ctx.Canceled.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		_, err := resolveFallback(ctx, errReplyTimeout, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			called = true
			return nil, errReplyTimeout
		})
		if called {
			t.Fatal("a read whose ctx is already done must not scan fallbacks")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a pre-cancelled read must return ctx.Err(); got %v", err)
		}
	})

	t.Run("per_server_dial_timeout_with_live_ctx_falls_back", func(t *testing.T) {
		t.Parallel()
		// s2's trySingle returns a wrapped context.DeadlineExceeded (a cold-dial timeout under the
		// db-scoped DefaultRPCTimeout) while the READ ctx is still LIVE — this must fall back to the healthy
		// s3, NOT abort. Revert-proof for the codex P2-of-P2: gating on errors.Is(err, DeadlineExceeded)
		// instead of ctx.Err() aborts here and returns DeadlineExceeded, never trying s3.
		ctx := context.Background() // live — never cancelled
		val, err := resolveFallback(ctx, nil, servers, 0, 1, func(s ServerInfo) ([]byte, error) {
			if s.Address == "s2" {
				return nil, context.DeadlineExceeded // per-server dial timeout, read ctx still live
			}
			return []byte("v"), nil // s3 is healthy
		})
		if err != nil || string(val) != "v" {
			t.Fatalf("a per-server dial DeadlineExceeded with a LIVE read ctx must fall back to a healthy replica; got val=%q err=%v", val, err)
		}
	})

	t.Run("all_replicas_dial_timeout_surfaces_reply_timeout_not_all_alternatives", func(t *testing.T) {
		t.Parallel()
		// Every fallback replica dial-times-out (context.DeadlineExceeded) with a LIVE read ctx. Each is a
		// per-server TIMEOUT, so the scan must surface errReplyTimeout (getValue re-sends to the same
		// location) — NOT all_alternatives_failed (1006), which getValueImpl absorbs via the wrong-shard
		// invalidate+relocate path, the wrong response to merely-slow servers. Revert-proof: dropping
		// context.DeadlineExceeded from isServerTimeout flattens this to all_alternatives_failed.
		_, err := resolveFallback(context.Background(), nil, servers, 0, 1, func(ServerInfo) ([]byte, error) {
			return nil, context.DeadlineExceeded // per-server dial timeout, read ctx live
		})
		if !errors.Is(err, errReplyTimeout) {
			t.Fatalf("all fallback replicas dial-timing-out (live ctx) must surface errReplyTimeout, not all_alternatives_failed; got %v", err)
		}
	})

	t.Run("hedge_dial_timeout_seeds_reply_timeout_with_no_fallback", func(t *testing.T) {
		t.Parallel()
		// The HEDGE dial-timed-out (live read ctx) and no untried fallback replicas remain → the seed must
		// carry it as a timeout and surface errReplyTimeout, not all_alternatives_failed.
		two := []ServerInfo{srv("s0"), srv("s1")}
		_, err := resolveFallback(context.Background(), context.DeadlineExceeded, two, 0, 1, func(ServerInfo) ([]byte, error) {
			return nil, nil
		})
		if !errors.Is(err, errReplyTimeout) {
			t.Fatalf("a hedge dial timeout (live ctx) with no fallback must seed errReplyTimeout; got %v", err)
		}
	})
}
