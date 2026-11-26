package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// Watch watches a key for changes. The server holds the connection open until
// the watched key's value changes from the version observed by this transaction.
// Returns nil when the key has changed, or an error on failure.
//
// Matches C++ NativeAPI.actor.cpp watchValueMap/watchValue flow: locate storage
// server, send WatchValueRequest with current read version, long-poll for
// WatchValueReply. Retries on wrong_shard_server with cache invalidation.
//
// The watch is a long-poll: there is no short timeout. The context's deadline
// (if any) controls the maximum wait time.
// RFC-170 (#8) note: this SYNCHRONOUS client API registers the watch at the READ version — it blocks in
// WatchPoll until the watch fires, so it cannot wait for a commit that (by construction) can only happen
// AFTER it returns. The committed-version deferral (register post-commit) is done by the ASYNC fdb facade
// Watch (which returns a future and blocks that future's goroutine on the commit-completion signal). Callers
// of this sync API that need committed-version semantics must commit before/around the watch (C++
// setupWatches' committedVersion>0?committedVersion:readVersion applies at the facade, not here).
func (tx *Transaction) Watch(ctx context.Context, key []byte) error {
	value, readVersion, span, watchCtx, watchCancel, err := tx.WatchSetup(ctx, key)
	if err != nil {
		return err
	}
	// Nothing commits before this poll: it carries the tags captured at setup.
	return tx.watchPoll(watchCtx, watchCancel, key, value, readVersion, span, tx.watchSetupTags(watchCtx))
}

// WatchSetup performs the SYNCHRONOUS part of a watch: pin the read version, add
// the read conflict, and read the value to watch — all at the transaction's read
// version. It returns BOTH the value bytes and the read version the watch must be
// registered against.
//
// This MUST run within the watching transaction's active window (e.g. directly
// from Transaction.Watch, not from a detached goroutine that races later
// mutations). Two things are captured here, both for the same reason:
//   - the VALUE: if read late — after some other transaction already changed the
//     key — the storage server would be told to watch the *new* value, see the
//     current value already equals it, and never fire (a silent 10s-timeout flake);
//   - the READ VERSION: the async WatchPoll's sendWatch must NOT read tx.readVersion
//     later, because the common `w := tr.Watch(k)` inside Database.Transact pattern
//     commits and postCommitReset()s the transaction (readVersion → 0) before the
//     future goroutine runs — sending the watch at version 0, which can error or
//     register incorrectly. So the read version is captured synchronously here and
//     threaded through to sendWatch.
func (tx *Transaction) WatchSetup(ctx context.Context, key []byte) ([]byte, int64, types.SpanContext, context.Context, context.CancelFunc, error) {
	callerCtx := ctx
	ctx, release := tx.opContext(ctx)
	defer release()
	if err := tx.readEntryError(ctx); err != nil {
		return nil, 0, types.SpanContext{}, nil, nil, err
	}
	// Deferred failure was captured before RYW/resetPromise dispatch. Lifetime
	// failure still precedes watch options, key validation and cap acquisition.
	if cerr := tx.checkCancelled(); cerr != nil {
		return nil, 0, types.SpanContext{}, nil, nil, cerr // transaction_cancelled (1025)
	}
	if cerr := tx.readLifetimeError(ctx); cerr != nil {
		return nil, 0, types.SpanContext{}, nil, nil, cerr
	}
	// C++ RYW::watch: watches are disabled when RYW is disabled
	// (ReadYourWrites.actor.cpp:2448-2449). Returns watches_disabled (1034).
	if tx.rywDisabled {
		return nil, 0, types.SpanContext{}, nil, nil, &wire.FDBError{Code: 1034} // watches_disabled
	}
	// Eager legal-range + key-size validation, BEFORE the read — C++ RYW watch
	// (ReadYourWrites.actor.cpp:2450-2456): a key >= getMaxReadKey() (and != metadataVersionKey) is
	// key_outside_legal_range (2004), an oversized key is key_too_large (2102). Without this a normal
	// (non-system) transaction could register a watch on a \xff system key or an oversized key that
	// libfdb_c rejects. Mirrors the Get/Set legal-range + commit key-size gates.
	if bytes.Compare(key, tx.maxReadKey()) >= 0 && !bytes.Equal(key, metadataVersionKeyBytes) {
		return nil, 0, types.SpanContext{}, nil, nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	rawAccess := (tx.writeSystemKeys || tx.readSystemKeys) && tx.tenantId < 0
	if len(key) > getMaxWriteKeySize(key, rawAccess) {
		return nil, 0, types.SpanContext{}, nil, nil, &wire.FDBError{Code: 2102} // key_too_large
	}

	// Reserve the outstanding-watch slot HERE — synchronously, at registration order — matching C++
	// Transaction::watch, which calls increaseWatchCounter at watch() time (NativeAPI.actor.cpp:5694),
	// NOT in the async poll. Charging it inside the async WatchPoll goroutine let two Watch() calls
	// under MAX_WATCHES=1 race for the slot, so the first-registered watch could lose to the
	// second. Released by WatchPoll on the success path, or on a setup error below (matching C++'s
	// catch → decreaseWatchCounter, NativeAPI:5679). Charged AFTER the malformed-key rejections above
	// so an illegal/oversized-key watch never holds a slot.
	if err := tx.db.tryAcquireWatch(); err != nil {
		return nil, 0, types.SpanContext{}, nil, nil, err // too_many_watches (1032)
	}
	// The watch is in `reading` from here until setup ends (ReadYourWrites.actor.cpp:1290),
	// and its read's conflict is classified against the write map as of this call (:382).
	defer tx.leaveIssuedRead(tx.enterIssuedRead(ctx))
	addConflict := tx.readConflictForKeyRYW(key)

	// Mint THIS watch's own cancellable context HERE — right after the acquire and BEFORE the blocking
	// GRV / value read below. Those reads RUN on watchCtx (below), so a Cancel()/reset() during
	// setup cancels THIS context and releases the slot; teardown also cancels an uncommitted poll.
	// Commit hands the poll to its future/parent, independent of later teardown. watchCancel is the SCOPED cancel
	// (cancels+deregisters only this watch): the setup-error paths below call it, WatchPoll defers it on
	// completion, and the fdb facade wires the Watch future's Cancel() to it — so one watch is freed
	// without touching siblings. The async WatchPoll USES this context, never a lazy fetch
	// (which races Cancel/reset — RFC-115 §4).
	watchCtx, watchCancel := tx.newWatchCtx(callerCtx)
	setupCtx, setupCancel := context.WithCancel(ctx)
	stopWatch := context.AfterFunc(watchCtx, setupCancel)
	defer func() { stopWatch(); setupCancel() }()

	// The blocking setup reads (GRV here, value read below) run on watchCtx — NOT the caller ctx — so a
	// Cancel()/reset() DURING setup (which cancels watchCtx via cancelWatches) unblocks the read and
	// releases the reserved slot PROMPTLY, matching C++'s watch actor: its setup waits are inside a
	// `catch (Error& e){ cx->decreaseWatchCounter(); throw; }`, so cancelling the watch actor on a txn
	// reset decrements the watch counter at once (NativeAPI.actor.cpp:5637-5682). watchCtx is a CHILD of
	// ctx, so the caller's own ctx cancellation still propagates. Using the caller ctx here instead
	// (the bug) left the slot charged until the caller ctx / RPC timeout when a stuck read raced a
	// Cancel — a starve under a low MAX_WATCHES. A Cancel that ran BEFORE the mint above (its
	// cancelWatches was a no-op) is caught by ensureReadVersion's leading checkCancelled.
	if err := tx.ensureReadVersion(setupCtx); err != nil {
		tx.db.releaseWatch()
		watchCancel() // setup failed — cancel+deregister THIS watch's context
		return nil, 0, types.SpanContext{}, nil, nil, tx.watchSetupErr(err)
	}
	tx.readVersionMu.Lock()
	readVersion := tx.readVersion
	tx.readVersionMu.Unlock()
	// Capture the trace span here, SYNCHRONOUSLY — same reason as readVersion: the async
	// WatchPoll must NOT read the tx span later, because a `w := tr.Watch(k)` inside
	// Database.Transact commits and postCommitReset()s the tx (regenerateSpan rotates
	// the span) before the future goroutine runs — a lazy read would stamp the NEXT
	// incarnation's span on this watch (RFC-115 §4). Threaded through to sendWatch.
	span := tx.currentSpan()

	// Read current value so we can send it with the watch request.
	// C++ getValueOrStandby in watchValue actor reads the value at the watch version.
	// NOT tracked in readErr: the C++ watch actor reading.add's its `done`
	// future (ReadYourWrites.actor.cpp:1290), but every error path sends
	// done.send(Void()) BEFORE rethrowing (:1299-1302, :1325-1329) — done
	// completes successfully, so a failed watch read never poisons commit;
	// reading only barriers on watch-setup completion (RFC-098): the C++ source
	// deliberately excludes watch errors.
	value, err := tx.ryw.get(setupCtx, key, tx.getValue)
	if err != nil {
		tx.db.releaseWatch() // setup failed after the slot was reserved (C++ catch → decreaseWatchCounter)
		watchCancel()        // cancel+deregister THIS watch's context
		return value, readVersion, span, nil, nil, tx.watchSetupErr(err)
	}
	// Only a successful read adds its conflict (ReadYourWrites.actor.cpp:384-388). A failed watch
	// read does not fail commit, so a conflict added before it would outlive it.
	if addConflict {
		if err := tx.addReadConflictsFor(ctx, [][2][]byte{{key, keyAfterBytes(key)}}); err != nil {
			tx.db.releaseWatch()
			watchCancel()
			return value, readVersion, span, nil, nil, err
		}
	}
	// watchCtx/watchCancel were minted synchronously above (before this blocking read), so a
	// Cancel/reset during setup cancels the very context the read (above) and WatchPoll hold; WatchPoll
	// defers watchCancel to deregister on completion, and the facade wires the future's Cancel() to it.
	return value, readVersion, span, watchCtx, watchCancel, nil
}

// watchSetupErr maps a setup-read failure to the faithful terminal error. The blocking GRV / value
// read run on watchCtx (a child of the caller ctx); a Cancel()/reset() during setup cancels watchCtx
// via cancelWatches, so the read surfaces a context cancellation. Only THAT is reinterpreted: when the
// txn itself was Cancel()ed, the cancellation out-ranks the raw ctx error as transaction_cancelled
// (1025) — matching what C++'s watch read observes after resetPromise.sendError(transaction_cancelled)
// fires (ReadYourWrites.actor.cpp:1284-1333). A caller-ctx cancellation (txn not cancelled) is
// returned as-is, and a GENUINE read error (an FDBError, a decode failure) is NEVER masked by 1025
// even if a Cancel raced in just after it — whichever surfaced first wins, as in C++.
func (tx *Transaction) watchSetupErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if cerr := tx.checkCancelled(); cerr != nil {
			return cerr // the watchCtx cancellation was a txn Cancel → transaction_cancelled (1025)
		}
	}
	return err
}

// WatchPoll performs the ASYNCHRONOUS long-poll part of a watch: locate the
// storage server and wait for the WatchValueReply that fires when key's value
// differs from `value` (captured by WatchSetup), registered at `readVersion`
// (also captured by WatchSetup — NOT re-read from the possibly-reset transaction).
// Retries on wrong_shard_server with cache invalidation. Intended to run in the
// watch future's goroutine.
//
// Its requests carry the read tags as of the commit that activated the watch
// (setupWatches, NativeAPI.actor.cpp:6418-6432), or those captured at setup
// when no commit activated it.
func (tx *Transaction) WatchPoll(watchCtx context.Context, watchCancel context.CancelFunc, key, value []byte, readVersion int64, span types.SpanContext) error {
	return tx.watchPoll(watchCtx, watchCancel, key, value, readVersion, span, tx.watchCommitTags(watchCtx))
}

// watchPoll sends tags, the watch's parameters->tags, never the live transaction's.
func (tx *Transaction) watchPoll(watchCtx context.Context, watchCancel context.CancelFunc, key, value []byte, readVersion int64, span types.SpanContext, tags []byte) error {
	tenantID := tx.watchTenant(watchCtx)
	// watchCtx is captured SYNCHRONOUSLY by WatchSetup and passed in (NOT fetched here via
	// newWatchCtx): this runs in the async watch future, so a lazy fetch would race
	// Cancel()/reset()'s cancelWatches on watchCtx/watchCancel AND, if cancelWatches won, leak a
	// never-cancelled poll. Binding at setup makes Reset()/Cancel() cancel the very context this
	// loop holds until commit hands it off; later Reset/Cancel leave committed watches alone. span
	// is likewise captured synchronously by WatchSetup (a lazy read here would stamp a later
	// incarnation's regenerated span on this watch, RFC-115 §4).

	// The outstanding-watch slot was reserved SYNCHRONOUSLY in WatchSetup (registration order, C++
	// increaseWatchCounter at watch() time). Release it on EVERY exit path of this poll — fire /
	// error / cancel — matching C++ decreaseWatchCounter in the async watch actor (NativeAPI:5679).
	// tx.db is always set on a WatchPoll (the locate/sendWatch below dereference it), so no nil guard.
	defer tx.db.releaseWatch()
	// Deregister THIS watch's scoped context on completion (fire / error / cancel), so the per-txn
	// watchCancels map self-cleans across a reused post-commit handle. Idempotent with the future's
	// Cancel() and the cancelWatches path. Production callers always pass a non-nil watchCancel (from
	// WatchSetup's success path), but guard defensively so a future caller on an error path can't panic
	// the async watch goroutine.
	if watchCancel != nil {
		defer watchCancel()
	}

	// The WatchValueRequest carries a CHILD of the tx span, derived ONCE here and
	// reused across the wrong-shard retry loop — matching C++ watchValue's single
	// `state Span span("NAPI:watchValue", parameters->spanContext)` (NativeAPI.actor.cpp:3933),
	// whose span.context is stamped on the request (:3965). locate below is passed the
	// raw tx span (C++ hands getKeyLocation parameters->spanContext, not the watch child).
	watchSpan := childSpanContext(span)

	// A watch polls until its value changes, its future/parent cancels (also precommit Reset/Cancel),
	// or a non-retryable error surfaces. Mirrors C++ watchValue's `loop { ... } catch` (NativeAPI.actor.cpp:3993-4012). Only wrong_shard /
	// all_alternatives is bounded (a relocate storm); the SS poll-signals below are unbounded — the
	// watch keeps polling, as C++ does.
	wrongShardRetries := 0
	lb := lbRetry{observe: tx.db.readBackoffObserver()}
	for {
		if err := watchCtx.Err(); err != nil {
			return err
		}
		loc, locErr := tx.db.locCache.locate(tx.db, watchCtx, key, tenantID, span, false)
		if locErr != nil {
			return fmt.Errorf("locate key: %w", locErr)
		}
		if len(loc.Servers) == 0 {
			return fmt.Errorf("no storage servers for key")
		}

		// A WatchValueRequest per iteration (NativeAPI.actor.cpp:3970).
		watchErr := tx.sendWatchPaced(watchCtx, key, value, readVersion, watchSpan, loc.Servers, tx.db.sampleTagSet(tags), &lb)
		if watchErr == nil {
			return nil
		}
		// Every replica failed maybe-delivered: watchValue's loadBalance retries it itself,
		// unbounded, paced per request by sendWatchPaced; re-located as getValueImpl explains.
		if isMaybeDelivered(watchErr) {
			tx.db.locCache.invalidate(key, tenantID, false)
			continue
		}
		lb = lbRetry{}
		delay, retryable, invalidate := classifyWatchError(watchErr)
		if !retryable {
			return watchErr
		}
		if invalidate {
			// wrong_shard / all_alternatives: transient relocate; bounded (a wrong-shard storm
			// shouldn't spin forever — the unbounded poll signals below reset this budget).
			wrongShardRetries++
			if wrongShardRetries > MaxWrongShardRetries {
				return &wire.FDBError{Code: ErrAllAlternativesFailed}
			}
			tx.db.locCache.invalidate(key, tenantID, false)
		} else {
			wrongShardRetries = 0 // a completed round-trip clears the relocate budget
		}
		if err := sleepCtx(watchCtx, delay); err != nil {
			return err
		}
	}
}

// classifyWatchError decides how WatchPoll handles a watch RPC error, mirroring C++ watchValue's
// catch arms (NativeAPI.actor.cpp:3993-4012):
//   - wrong_shard_server / all_alternatives_failed → re-locate after WRONG_SHARD_SERVER_DELAY
//     (invalidate=true, the only BOUNDED retry).
//   - watch_cancelled (1029, SS watch limit exceeded) / process_behind (1037) → poll after
//     WATCH_POLLING_TIME. Unbounded — a watch is a long-lived poll.
//   - timed_out (1004, the SS occasionally times out a watch) / future_version (1009, the SS hasn't
//     caught up to the watch's read version) → re-arm after FUTURE_VERSION_RETRY_DELAY. Unbounded.
//     (C++ retries future_version in the SS-response actor, watchStorageServerResp; Go's flatter
//     WatchPoll handles it here.)
//   - anything else → terminal (retryable=false).
func classifyWatchError(err error) (delay time.Duration, retryable, invalidate bool) {
	switch {
	case isWrongShardServer(err) || isAllAlternativesFailed(err):
		return wrongShardRetryDelay, true, true
	case isWatchErrorCode(err, ErrWatchCancelled) || isWatchErrorCode(err, ErrProcessBehind):
		return watchPollingTime, true, false
	case isWatchErrorCode(err, ErrTimedOut) || isWatchErrorCode(err, ErrFutureVersion):
		return futureVersionDelay, true, false
	default:
		// Terminal: surface immediately. C++ watchValue delays FUTURE_VERSION_RETRY_DELAY before
		// re-throwing (NativeAPI.actor.cpp:4010) — a PREVENT_FAST_SPIN guard for its actor loop. Go's
		// WatchPoll returns the error to the caller (the watch is done; no spin), so the delay is
		// unnecessary and deliberately omitted.
		return 0, false, false
	}
}

// isWatchErrorCode reports whether err is an FDBError with the given code.
func isWatchErrorCode(err error, code int) bool {
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == code
}

// buildWatchValueRequest constructs a WatchValueRequest. `span` is the watchValue
// CHILD span (derived once in WatchPoll, reused across the wrong-shard retry loop) —
// stamped verbatim, matching C++ WatchValueRequest(span.context,…) (NativeAPI.actor.cpp:3965).
func buildWatchValueRequest(key, value []byte, readVersion int64, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID) []byte {
	req := types.WatchValueRequest{
		Key:         key,
		Version:     readVersion,
		Reply:       types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:  types.TenantInfo{TenantId: tenantId},
		SpanContext: span,
	}
	tags.apply(&req.HasTags, &req.Tags)
	if value != nil {
		req.HasValue = true
		req.Value = value
	}
	return req.MarshalFDB()
}

func (tx *Transaction) sendWatch(ctx context.Context, key, value []byte, readVersion int64, span types.SpanContext, servers []ServerInfo, tags readTagSample) error {
	return tx.sendWatchPaced(ctx, key, value, readVersion, span, servers, tags, nil)
}

// sendWatchPaced is sendWatch counting each maybe-delivered request as one loadBalance attempt
// in lb, which delays the request after it (LoadBalance.actor.h:301-308, 721-725).
func (tx *Transaction) sendWatchPaced(ctx context.Context, key, value []byte, readVersion int64, span types.SpanContext, servers []ServerInfo, tags readTagSample, lb *lbRetry) error {
	alternatives := len(servers)
	servers, ok := tx.db.failMon.liveServers(servers, EndpointWatchValue)
	if !ok {
		return tx.db.allAlternativesFailed(ctx)
	}
	_, chosenIdx := tx.db.queueModel.chooseServer(servers)
	order := loadBalanceOrder(servers, chosenIdx)

	// readVersion is captured synchronously by WatchSetup and passed in — it must
	// NOT be re-read from tx here, because the transaction may have been
	// postCommitReset() (readVersion → 0) by the time this async poll runs.
	tenantId := tx.watchTenant(ctx)
	var replyCleanup replyGuard
	defer replyCleanup.close()
	var sendCleanup sendGuard
	defer sendCleanup.close()

	var lastErr error
	fail := func(err error) error {
		lastErr = err
		if lb != nil && isMaybeDelivered(err) {
			return lb.pace(ctx, alternatives, 1)
		}
		return nil
	}
	for _, server := range order {
		conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
		if err != nil {
			tx.db.handleDialError(ctx, server.Address)
			if err := fail(err); err != nil {
				return err
			}
			continue
		}
		sendCleanup.permit = permit
		watchToken := getAdjustedEndpoint(server.Token, EndpointWatchValue)
		replyToken, replyCh, replyHandle := conn.PrepareReplyTo(watchToken)
		replyCleanup.handle = replyHandle
		reqData := buildWatchValueRequest(key, value, readVersion, tenantId, span, tags, replyToken)

		// watchValue's loadBalance has no QueueModel (NativeAPI.actor.cpp:3962-3972):
		// a watch's duration must not become the server's read latency.
		if err := sendReadFrame(ctx, conn, watchToken, reqData, replyHandle, permit, nil); err != nil {
			replyHandle.Cancel()
			replyCleanup.release()
			tx.db.handleReadConnError(server.Address, conn, err)
			if err := fail(err); err != nil {
				return err
			}
			continue
		}
		// Long-poll: no short timeout. Use the caller's context deadline.
		select {
		case resp := <-replyCh:
			replyCleanup.release()
			if resp.Err != nil {
				tx.db.handleReadConnError(server.Address, conn, resp.Err)
				if err := fail(resp.Err); err != nil {
					return err
				}
				continue
			}
			err := parseWatchValueReply(resp.Body)
			tx.db.noteBrokenPromise(err, server.Address, watchToken)
			if isMaybeDelivered(err) {
				if paceErr := fail(err); paceErr != nil {
					return paceErr
				}
				continue
			}
			return err
		case <-ctx.Done():
			replyHandle.Cancel()
			replyCleanup.release()
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Every replica failed to connect or tore down: loadBalance's own retry, paced above,
	// not an all_alternatives_failed (see resolveFallback).
	if isMaybeDelivered(lastErr) {
		return lastErr
	}
	return &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func parseWatchValueReply(data []byte) error {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return fmt.Errorf("WatchValue: %w", err)
	}
	// Reply parsed successfully — key has changed.
	return nil
}
