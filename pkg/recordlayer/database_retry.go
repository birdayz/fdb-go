// Portions derived from FoundationDB Record Layer (FDBRecordContext.java,
// FDBDatabase.java, FDBDatabaseRunner.java, FDBDatabaseFactory.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// Run executes fn in a transaction and commits it, retrying as Java's
// FDBDatabaseRunner does (FDBDatabaseRunnerImpl.RunRetriable): at most
// MaxAttempts attempts (default 10), each a fresh transaction, retried while
// some cause of the error is retriable, with Java's ExponentialDelay between
// attempts (attemptLoop). Before committing it runs the commit checks and
// flushes queued versionstamp mutations, as FDBRecordContext.commitAsync does;
// the post-commit hooks run once, after the attempt that committed.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	result, _, err := d.runContexts(ctx, d.policy("run"), RouteAttempt, nil, false, fn)
	return result, err
}

// RunWithMaxAttempts is Run with its own attempt bound in place of the
// database's, as an FDBDatabaseRunner with setMaxAttempts runs. One attempt is
// how the relational layer runs a DDL statement: a conflict surfaces instead of
// re-executing the statement.
func (d *FDBDatabase) RunWithMaxAttempts(ctx context.Context, maxAttempts int, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	policy := d.policy("run")
	if maxAttempts > 0 {
		policy.maxAttempts = maxAttempts
	}
	result, _, err := d.runContexts(ctx, policy, RouteAttempt, nil, false, fn)
	return result, err
}

// RunClientLoop runs fn through the transactor's own retry loop, which is
// unbounded on the pure-Go client and libfdb_c and capped at 100 retries on
// SimFDB. Only SPFresh's background lifecycles use it (spfreshRun): RFC-094
// keeps them on the client loop, and nothing else may.
func (d *FDBDatabase) RunClientLoop(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	result, _, err := d.runContexts(ctx, attemptPolicy{owner: "spfresh.lifecycle", maxAttempts: 1}, RouteClientLoop, nil, false, fn)
	return result, err
}

// runContexts is Run and its variants. Each attempt is one transactor call
// whose body opens a route-owned record context; on RouteAttempt the backend's
// own retry limit is 0, so the backend makes exactly one attempt and the loop
// counts it. weak applies to the first execution only, as Java's
// TransactionalRunner clears weak-read semantics on a retry
// (TransactionalRunner.java:181-192), so a retry never reuses an old read
// version. versionstamp asks for the committed versionstamp.
func (d *FDBDatabase) runContexts(ctx context.Context, policy attemptPolicy, route AttemptRoute, weak *WeakReadSemantics, versionstamp bool, fn func(rtx *FDBRecordContext) (any, error)) (any, []byte, error) {
	var (
		lastCtx     *FDBRecordContext
		commitStart time.Time
		vsFuture    fdb.FutureKey
		body        bodyErrorRecorder
	)
	execute := func(call AttemptCall) (any, error) {
		result, err := d.transactAttempt(ctx, call, func(tx fdb.WritableTransaction) (any, error) {
			// Each execution starts clean: a previous execution's context,
			// commit timing and versionstamp future are stale.
			body.start()
			lastCtx, commitStart, vsFuture = nil, time.Time{}, nil
			if route == RouteAttempt {
				if err := tx.Options().SetRetryLimit(0); err != nil {
					return nil, body.record(err)
				}
			}
			d.applyReadSystemKeys(tx.Options())
			if weak != nil && weak.IsCausalReadRisky && call.Execution == 0 {
				tx.Options().SetCausalReadRisky()
			}
			recordCtx := &FDBRecordContext{
				createTime:    d.env.Now(),
				transactionID: nextTransactionID.Add(1),
				tx:            tx,
				ctx:           ctx,
				env:           d.env,
				database:      d,
				routeOwned:    true,
			}
			recordCtx.SetTimer(d.Timer())
			lastCtx = recordCtx

			result, err := fn(recordCtx)
			if err != nil {
				return nil, body.record(err)
			}
			result, err = d.precommit(recordCtx, result, &commitStart)
			if err != nil {
				return nil, body.record(err)
			}
			if versionstamp && recordCtx.HasVersionMutations() {
				vsFuture = tx.GetVersionstamp()
			}
			return result, nil
		})
		return result, body.resolve(err)
	}

	var result any
	var err error
	if route == RouteClientLoop {
		result, err = execute(AttemptCall{Route: route, Owner: policy.owner, CallID: nextAttemptCallID.Add(1)})
	} else {
		result, err = attemptLoop(ctx, d.env, d.observer(), policy, route, execute)
	}
	if err != nil {
		return nil, nil, err
	}
	recordCommitSince(lastCtx, commitStart)
	// Post-commit callbacks run once, after the attempt that committed.
	if lastCtx != nil {
		lastCtx.runPostCommits()
	}
	if vsFuture != nil {
		vs, err := vsFuture.Get()
		if err != nil {
			return result, nil, fmt.Errorf("failed to get versionstamp: %w", err)
		}
		return result, []byte(vs), nil
	}
	return result, nil, nil
}

// precommit is the part of FDBRecordContext.commitAsync that runs before the
// transactor commits: the commit checks and the versionstamp flush.
func (d *FDBDatabase) precommit(recordCtx *FDBRecordContext, result any, commitStart *time.Time) (any, error) {
	// EventCommit spans the pre-commit checks plus the commit itself, which is
	// the interval Java measures: startTimeNanos is taken at the first line of
	// commitAsync, before commit checks, and the event is recorded when the
	// commit future completes (FDBRecordContext.java:478, :513-533; the
	// FDBStoreTimer javadoc at :81-86 says so explicitly). Post-commit hooks
	// are outside the span in Java and are outside it here.
	//
	// It has to be recorded out here rather than in FDBRecordContext.Commit
	// because on this path the commit belongs to the transactor's retry loop —
	// nothing ever calls Commit, which is why the whole autocommit SQL path
	// reported zero commits while committing on every statement.
	*commitStart = time.Now()
	if err := recordCtx.drainCommitChecksAndSealWrites(); err != nil {
		return nil, err
	}
	// Flush queued version mutations before the transactor commits.
	recordCtx.flushVersionMutations()
	return result, nil
}

// recordCommitSince records EventCommit for a transaction that committed inside
// the transactor's retry loop. On a retry, commitStart is overwritten by each
// attempt, so the recorded span is the successful attempt's — the same one Java
// records, since Java's per-attempt commitAsync only reaches its success arm on
// the attempt that commits.
//
// Zero commitStart means the closure returned before reaching the commit (fn
// failed), so there was no commit to time.
//
// Java's other two commit outcomes are NOT ported here, deliberately.
// COMMIT_READ_ONLY needs the committed version to tell a write-free transaction
// apart (FDBRecordContext.java:492, :524) and COMMIT_FAILURE needs the commit's
// own exception (:517); on this path the transactor owns the commit and surfaces
// neither. Recording a plain COMMIT for those cases would be worse than
// recording nothing — it would inflate the commit count with transactions that
// did not commit.
func recordCommitSince(rc *FDBRecordContext, commitStart time.Time) {
	if rc == nil || commitStart.IsZero() {
		return
	}
	rc.Timer().RecordSince(EventCommit, commitStart)
}

// runTransactCtx threads ctx into the transactor's retry loop + backoff + reads when
// the transactor supports it (Database/Tenant via fdb.CtxTransactor), else falls back
// to the ctx-less Transact (RFC-090). The dispatched commit + commit_unknown barrier
// run detached regardless, so ctx never cancels an in-flight commit.
func runTransactCtx(t fdb.Transactor, ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ct, ok := t.(fdb.CtxTransactor); ok {
		return ct.TransactCtx(ctx, fn)
	}
	return t.Transact(fn)
}

// runReadTransactCtx is the read-side analog of runTransactCtx.
func runReadTransactCtx(t fdb.ReadTransactor, ctx context.Context, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	if ct, ok := t.(fdb.CtxReadTransactor); ok {
		return ct.ReadTransactCtx(ctx, fn)
	}
	return t.ReadTransact(fn)
}

// RunRead executes a read-only function with automatic retry but no commit.
// Uses ReadTransact under the hood — no write conflict ranges, no commit
// round-trip. Suitable for statistics, metadata reads, or any read-only
// operation where a full read-write transaction would waste a commit.
//
// ctx bounds the read-retry loop + backoff when the transactor supports it
// (fdb.CtxReadTransactor); the entry check also returns early if already cancelled.
func (d *FDBDatabase) RunRead(ctx context.Context, fn func(rtx fdb.ReadTransaction) (any, error)) (any, error) {
	var body bodyErrorRecorder
	return attemptLoop(ctx, d.env, d.observer(), d.policy("run.read"), RouteAttempt, func(call AttemptCall) (any, error) {
		result, err := d.readTransactAttempt(ctx, call, func(rtx fdb.ReadTransaction) (any, error) {
			body.start()
			if err := rtx.Options().SetRetryLimit(0); err != nil {
				return nil, body.record(err)
			}
			d.applyReadSystemKeys(rtx.Options())
			result, err := fn(rtx)
			if err != nil {
				return nil, body.record(err)
			}
			return result, nil
		})
		return result, body.resolve(err)
	})
}

// RunWithWeakReads is like Run but applies weak read semantics to the transaction.
// If IsCausalReadRisky is set, the transaction reads from any replica.
// Matches Java's FDBDatabase.openContext(config, timer, weakReadSemantics, ...).
func (d *FDBDatabase) RunWithWeakReads(ctx context.Context, weak WeakReadSemantics, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	result, _, err := d.runContexts(ctx, d.policy("run.weak"), RouteAttempt, &weak, false, fn)
	return result, err
}

// RunWithVersionstamp is like Run but also returns the committed versionstamp.
// Use this when you need the versionstamp after commit (e.g. for record versioning).
// Returns (result, versionstamp, error). Versionstamp is nil for read-only transactions.
func (d *FDBDatabase) RunWithVersionstamp(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, []byte, error) {
	return d.runContexts(ctx, d.policy("run.versionstamp"), RouteAttempt, nil, true, fn)
}
