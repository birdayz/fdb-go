//go:build bazelrunfiles

package main

import (
	"context"

	"fdb.dev/internal/fdbwire"
)

func (sm *StackMachine) executeTxnOp(ctx context.Context, idx int, baseOp string, _, isSnapshot bool) (bool, error) {
	if sm.executeTxnMgmtOp(ctx, idx, baseOp) {
		return true, nil
	}
	if sm.executeVersionOp(ctx, idx, baseOp, isSnapshot) {
		return true, nil
	}
	return sm.executeConflictAndErrorOp(ctx, idx, baseOp), nil
}

func (sm *StackMachine) executeTxnMgmtOp(ctx context.Context, idx int, baseOp string) bool {
	switch baseOp {
	case "NEW_TRANSACTION":
		sm.trMu.Lock()
		sm.replaceTransaction(sm.trName)
		sm.trMu.Unlock()
	case "USE_TRANSACTION":
		name := sm.popString()
		sm.trMu.Lock()
		sm.trName = name
		if _, ok := sm.trMap[name]; !ok {
			sm.replaceTransaction(name)
		}
		sm.trMu.Unlock()
	case "COMMIT":
		tr, release := sm.acquireCommitTransaction()
		defer release()
		err := tr.Commit(ctx)
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("RESULT_NOT_PRESENT"))
		}
	case "RESET":
		sm.trMu.Lock()
		sm.replaceTransaction(sm.trName)
		sm.trMu.Unlock()
	case "CANCEL":
		sm.currentTr().Cancel()
	default:
		return false
	}
	return true
}

func (sm *StackMachine) executeVersionOp(ctx context.Context, idx int, baseOp string, isSnapshot bool) bool {
	switch baseOp {
	case "GET_READ_VERSION":
		var ver int64
		var err error
		if isSnapshot {
			ver, err = sm.currentTr().Snapshot().GetReadVersion(ctx)
		} else {
			ver, err = sm.currentTr().GetReadVersion(ctx)
		}
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.lastVer = ver
			sm.push(idx, []byte("GOT_READ_VERSION"))
		}
	case "SET_READ_VERSION":
		sm.currentTr().SetReadVersion(sm.lastVer)
	case "GET_COMMITTED_VERSION":
		ver, err := sm.currentTr().GetCommittedVersion()
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.lastVer = ver
			sm.push(idx, []byte("GOT_COMMITTED_VERSION"))
		}
	case "GET_VERSIONSTAMP":
		// Deferred versionstamp: we push a future that resolves after commit.
		// Since our client is synchronous, we use the versionstampFuture mechanism.
		f := &versionstampFuture{tr: sm.currentTr()}
		sm.push(idx, f)
	case "GET_APPROXIMATE_SIZE":
		// Reference testers wait on the future and surface its error as an
		// ERROR tuple — a deferred-error-poisoned txn (2000/2018) must compare
		// identically across bindings, not be swallowed.
		if _, err := sm.currentTr().GetApproximateSize(); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("GOT_APPROXIMATE_SIZE"))
		}
	case "GET_ESTIMATED_RANGE_SIZE":
		begin, end := sm.popBytes(), sm.popBytes()
		if _, err := sm.currentTr().GetEstimatedRangeSizeBytes(ctx, begin, end); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("GOT_ESTIMATED_RANGE_SIZE"))
		}
	case "GET_RANGE_SPLIT_POINTS":
		begin, end, chunkSize := sm.popBytes(), sm.popBytes(), sm.popInt64()
		if _, err := sm.currentTr().GetRangeSplitPoints(ctx, begin, end, chunkSize); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("GOT_RANGE_SPLIT_POINTS"))
		}
	default:
		return false
	}
	return true
}

func (sm *StackMachine) executeConflictAndErrorOp(ctx context.Context, idx int, baseOp string) bool {
	switch baseOp {
	case "READ_CONFLICT_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		if err := sm.currentTr().AddReadConflictRange(begin, end); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("SET_CONFLICT_RANGE"))
		}
	case "READ_CONFLICT_KEY":
		key := sm.popBytes()
		sm.currentTr().AddReadConflictKey(key)
		sm.push(idx, []byte("SET_CONFLICT_KEY"))
	case "WRITE_CONFLICT_RANGE":
		begin := sm.popBytes()
		end := sm.popBytes()
		if err := sm.currentTr().AddWriteConflictRange(begin, end); err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("SET_CONFLICT_RANGE"))
		}
	case "WRITE_CONFLICT_KEY":
		key := sm.popBytes()
		sm.currentTr().AddWriteConflictKey(key)
		sm.push(idx, []byte("SET_CONFLICT_KEY"))
	case "ON_ERROR":
		code := int(sm.popInt64())
		err := sm.currentTr().OnError(ctx, &wire.FDBError{Code: code})
		if err != nil {
			sm.pushError(idx, err)
		} else {
			sm.push(idx, []byte("RESULT_NOT_PRESENT"))
		}
	case "DISABLE_WRITE_CONFLICT":
		sm.currentTr().SetNextWriteNoWriteConflictRange()
	default:
		return false
	}
	return true
}
