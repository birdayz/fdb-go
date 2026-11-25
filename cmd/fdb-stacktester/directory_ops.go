//go:build bazelrunfiles

package main

import (
	"context"
	"fmt"
	"strings"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/directory"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	ourtuple "fdb.dev/pkg/fdbgo/fdb/tuple"
	appletuple "github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// dirEntry is an entry in the directory list. It can hold a Directory,
// DirectorySubspace, or a raw Subspace. Nil means the slot is a
// placeholder for a failed operation.
type dirEntry struct {
	dir directory.Directory         // non-nil for directories and directory subspaces
	ss  subspace.Subspace           // non-nil for raw subspaces (DIRECTORY_CREATE_SUBSPACE)
	dsb directory.DirectorySubspace // non-nil for directory subspaces specifically
}

// asSubspace returns the subspace for this entry (works for both
// DirectorySubspace and raw Subspace).
func (e *dirEntry) asSubspace() subspace.Subspace {
	if e.dsb != nil {
		return e.dsb
	}
	return e.ss
}

// isNull returns true if this is a placeholder nil entry.
func (e *dirEntry) isNull() bool {
	return e.dir == nil && e.ss == nil && e.dsb == nil
}

// initDirectoryState initializes the directory list with the default
// root directory layer.
func (sm *StackMachine) initDirectoryState() {
	root := directory.Root()
	sm.dirList = []dirEntry{{dir: root}}
	sm.dirIndex = 0
	sm.dirErrorIndex = 0
}

// currentDir returns the current directory entry.
func (sm *StackMachine) currentDir() *dirEntry {
	return &sm.dirList[sm.dirIndex]
}

// appendDir appends a directory entry to the list.
func (sm *StackMachine) appendDir(e dirEntry) {
	sm.dirList = append(sm.dirList, e)
}

// appendNull appends a null entry at the error index position.
func (sm *StackMachine) appendNull() {
	sm.dirList = append(sm.dirList, dirEntry{})
}

// popTupleAsPath pops a count and then that many elements from the stack,
// returning them as a []string path.
func (sm *StackMachine) popTupleAsPath() []string {
	count := int(sm.popInt64())
	path := make([]string, count)
	for i := 0; i < count; i++ {
		path[i] = sm.popString()
	}
	return path
}

// popTupleRaw pops a count and then that many elements from the stack,
// returning them as our internal ourtuple.Tuple (for subspace/directory ops).
// Recursively converts Apple tuple types to our tuple types.
func (sm *StackMachine) popTupleRaw() ourtuple.Tuple {
	count := int(sm.popInt64())
	t := make(ourtuple.Tuple, count)
	for i := 0; i < count; i++ {
		t[i] = convertTupleElement(sm.pop().value)
	}
	return t
}

// convertTupleElement converts Apple binding tuple types to our tuple types.
func convertTupleElement(v any) any {
	switch val := v.(type) {
	case appletuple.Tuple:
		out := make(ourtuple.Tuple, len(val))
		for i, e := range val {
			out[i] = convertTupleElement(e)
		}
		return out
	case appletuple.UUID:
		return ourtuple.UUID(val)
	case appletuple.Versionstamp:
		return ourtuple.Versionstamp{
			TransactionVersion: val.TransactionVersion,
			UserVersion:        val.UserVersion,
		}
	default:
		return v
	}
}

// stackTupleElement preserves raw unpacked values in the stack's tuple dialect.
func stackTupleElement(v any) any {
	switch val := v.(type) {
	case ourtuple.Tuple:
		out := make(appletuple.Tuple, len(val))
		for i, e := range val {
			out[i] = stackTupleElement(e)
		}
		return out
	case ourtuple.UUID:
		return appletuple.UUID(val)
	case ourtuple.Versionstamp:
		return appletuple.Versionstamp{TransactionVersion: val.TransactionVersion, UserVersion: val.UserVersion}
	default:
		return v
	}
}

// popBytesOrNil pops a value that may be nil (Python NONE) or bytes.
func (sm *StackMachine) popBytesOrNil() []byte {
	e := sm.pop()
	if e.value == nil {
		return nil
	}
	switch v := e.value.(type) {
	case []byte:
		// The tuple decoder represents b'' as a typed nil slice, not Python None.
		if v == nil {
			return []byte{}
		}
		return v
	case string:
		return []byte(v)
	default:
		panic(fmt.Sprintf("expected bytes or nil, got %T", e.value))
	}
}

// Python treats an omitted or empty layer as a wildcard when opening a
// directory; the Go directory facade represents that wildcard as nil.
func (sm *StackMachine) popLayer() []byte {
	layer := sm.popBytesOrNil()
	if len(layer) == 0 {
		return nil
	}
	return layer
}

// fdbDB returns an fdb.Database wrapping the stack machine's client.Database.
func (sm *StackMachine) fdbDB(ctx context.Context) gofdb.Database {
	return gofdb.WrapDatabaseContext(ctx, sm.db)
}

// fdbTr returns an fdb.Transaction wrapping the current client.Transaction.
func (sm *StackMachine) fdbTr(ctx context.Context) gofdb.Transaction {
	return gofdb.WrapTransaction(sm.currentTr(), sm.fdbDB(ctx))
}

// getTransactor returns the appropriate fdb.Transactor based on whether
// this is a _DATABASE operation or a regular transaction operation.
func (sm *StackMachine) getTransactor(ctx context.Context, isDatabase bool) gofdb.Transactor {
	if isDatabase {
		return directoryDatabaseTransactor{sm.fdbDB(ctx), ctx, &sm.waits}
	}
	return sm.fdbTr(ctx)
}

// getReadTransactor returns the appropriate fdb.ReadTransactor based on
// operation suffix.
func (sm *StackMachine) getReadTransactor(ctx context.Context, isDatabase, isSnapshot bool) gofdb.ReadTransactor {
	if isDatabase {
		return directoryDatabaseTransactor{sm.fdbDB(ctx), ctx, &sm.waits}
	}
	if isSnapshot {
		return sm.fdbTr(ctx).Snapshot()
	}
	return sm.fdbTr(ctx)
}

// executeDirectoryOp handles DIRECTORY_* operations. Returns true if
// the operation was handled, false if it's not a directory operation.
func (sm *StackMachine) executeDirectoryOp(ctx context.Context, idx int, op string, isDatabase, isSnapshot bool) (handled bool, err error) {
	// Strip DIRECTORY_ prefix for dispatch.
	if !strings.HasPrefix(op, "DIRECTORY_") {
		return false, nil
	}

	// Catch panics from directory partition operations (Sub, Pack, Bytes, etc.
	// all panic when called on the root of a directory partition).
	// The binding tester expects DIRECTORY_ERROR, not a crash.
	defer func() {
		if r := recover(); r != nil {
			// Python reserves a directory-list slot even when a creator raises.
			switch strings.TrimPrefix(op, "DIRECTORY_") {
			case "CREATE_SUBSPACE", "CREATE_LAYER", "CREATE_OR_OPEN", "CREATE", "OPEN", "MOVE", "MOVE_TO", "OPEN_SUBSPACE":
				sm.appendNull()
			}
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			handled = true
			err = nil
		}
	}()

	dirOp := strings.TrimPrefix(op, "DIRECTORY_")
	if sm.executeDirectoryLifecycleOp(ctx, idx, dirOp, isDatabase, isSnapshot) {
		return true, nil
	}
	return sm.executeDirectorySubspaceOp(ctx, idx, dirOp, isDatabase, isSnapshot), nil
}
