package main

import (
	"bytes"
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

// popTuple pops a count and then that many elements from the stack,
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

// popBytesOrNil pops a value that may be nil (Python NONE) or bytes.
func (sm *StackMachine) popBytesOrNil() []byte {
	e := sm.pop()
	if e.value == nil {
		return nil
	}
	switch v := e.value.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		panic(fmt.Sprintf("expected bytes or nil, got %T", e.value))
	}
}

// fdbDB returns an fdb.Database wrapping the stack machine's client.Database.
func (sm *StackMachine) fdbDB() gofdb.Database {
	return gofdb.WrapDatabase(sm.db)
}

// fdbTr returns an fdb.Transaction wrapping the current client.Transaction.
func (sm *StackMachine) fdbTr() gofdb.Transaction {
	return gofdb.WrapTransaction(sm.currentTr(), sm.fdbDB())
}

// getTransactor returns the appropriate fdb.Transactor based on whether
// this is a _DATABASE operation or a regular transaction operation.
func (sm *StackMachine) getTransactor(isDatabase bool) gofdb.Transactor {
	if isDatabase {
		return sm.fdbDB()
	}
	return sm.fdbTr()
}

// getReadTransactor returns the appropriate fdb.ReadTransactor based on
// operation suffix.
func (sm *StackMachine) getReadTransactor(isDatabase, isSnapshot bool) gofdb.ReadTransactor {
	if isDatabase {
		return sm.fdbDB()
	}
	if isSnapshot {
		return sm.fdbTr().Snapshot()
	}
	return sm.fdbTr()
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
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			handled = true
			err = nil
		}
	}()

	dirOp := strings.TrimPrefix(op, "DIRECTORY_")

	switch dirOp {
	case "CREATE_SUBSPACE":
		t := sm.popTupleRaw()
		rawPrefix := sm.popBytes()
		ss := subspace.FromBytes(append(rawPrefix, t.Pack()...))
		sm.appendDir(dirEntry{ss: ss})

	case "CREATE_LAYER":
		idx1 := int(sm.popInt64())
		idx2 := int(sm.popInt64())
		allowManual := sm.popInt64() != 0

		e1 := &sm.dirList[idx1]
		e2 := &sm.dirList[idx2]
		if e1.isNull() || e2.isNull() {
			sm.appendNull()
		} else {
			ss1 := e1.asSubspace()
			ss2 := e2.asSubspace()
			if ss1 == nil || ss2 == nil {
				sm.appendNull()
			} else {
				dl := directory.NewDirectoryLayer(ss1, ss2, allowManual)
				sm.appendDir(dirEntry{dir: dl})
			}
		}

	case "CREATE_OR_OPEN":
		path := sm.popTupleAsPath()
		layer := sm.popBytesOrNil()
		tr := sm.getTransactor(isDatabase)
		d := sm.currentDir()
		if d.dir == nil {
			sm.pushError(idx, fmt.Errorf("directory error"))
			sm.appendNull()
			return true, nil
		}
		ds, err := d.dir.CreateOrOpen(tr, path, layer)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
		} else {
			sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
		}

	case "CREATE":
		path := sm.popTupleAsPath()
		layer := sm.popBytesOrNil()
		prefix := sm.popBytesOrNil()
		tr := sm.getTransactor(isDatabase)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
			return true, nil
		}
		var ds directory.DirectorySubspace
		var err error
		if prefix != nil {
			ds, err = d.dir.CreatePrefix(tr, path, layer, prefix)
		} else {
			ds, err = d.dir.Create(tr, path, layer)
		}
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
		} else {
			sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
		}

	case "OPEN":
		path := sm.popTupleAsPath()
		layer := sm.popBytesOrNil()
		rt := sm.getReadTransactor(isDatabase, isSnapshot)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
			return true, nil
		}
		ds, err := d.dir.Open(rt, path, layer)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
		} else {
			sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
		}

	case "CHANGE":
		newIndex := int(sm.popInt64())
		if newIndex >= len(sm.dirList) || sm.dirList[newIndex].isNull() {
			sm.dirIndex = sm.dirErrorIndex
		} else {
			sm.dirIndex = newIndex
		}

	case "SET_ERROR_INDEX":
		sm.dirErrorIndex = int(sm.popInt64())

	case "MOVE":
		oldPath := sm.popTupleAsPath()
		newPath := sm.popTupleAsPath()
		tr := sm.getTransactor(isDatabase)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
			return true, nil
		}
		ds, err := d.dir.Move(tr, oldPath, newPath)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
		} else {
			sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
		}

	case "MOVE_TO":
		newAbsPath := sm.popTupleAsPath()
		tr := sm.getTransactor(isDatabase)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
			return true, nil
		}
		ds, err := d.dir.MoveTo(tr, newAbsPath)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
		} else {
			sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
		}

	case "REMOVE":
		count := int(sm.popInt64())
		var path []string
		if count == 1 {
			path = sm.popTupleAsPath()
		}
		tr := sm.getTransactor(isDatabase)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true, nil
		}
		_, err := d.dir.Remove(tr, path)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		}

	case "REMOVE_IF_EXISTS":
		count := int(sm.popInt64())
		var path []string
		if count == 1 {
			path = sm.popTupleAsPath()
		}
		tr := sm.getTransactor(isDatabase)
		d := sm.currentDir()
		if d.dir == nil {
			// If null, just no-op for if_exists variant.
			return true, nil
		}
		d.dir.Remove(tr, path) // ignore error for if_exists

	case "LIST":
		count := int(sm.popInt64())
		var path []string
		if count == 1 {
			path = sm.popTupleAsPath()
		}
		rt := sm.getReadTransactor(isDatabase, isSnapshot)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true, nil
		}
		children, err := d.dir.List(rt, path)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		} else {
			t := make(ourtuple.Tuple, len(children))
			for i, c := range children {
				t[i] = c
			}
			sm.push(idx, t.Pack())
		}

	case "EXISTS":
		count := int(sm.popInt64())
		var path []string
		if count == 1 {
			path = sm.popTupleAsPath()
		}
		rt := sm.getReadTransactor(isDatabase, isSnapshot)
		d := sm.currentDir()
		if d.dir == nil {
			sm.push(idx, int64(0))
			return true, nil
		}
		exists, err := d.dir.Exists(rt, path)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		} else if exists {
			sm.push(idx, int64(1))
		} else {
			sm.push(idx, int64(0))
		}

	case "PACK_KEY":
		t := sm.popTupleRaw()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true, nil
		}
		sm.push(idx, []byte(ss.Pack(t)))

	case "UNPACK_KEY":
		key := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true, nil
		}
		t, err := ss.Unpack(gofdb.Key(key))
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		} else {
			for _, elem := range t {
				sm.push(idx, ourtuple.Tuple{elem}.Pack())
			}
		}

	case "RANGE":
		t := sm.popTupleRaw()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true, nil
		}
		sub := ss.Sub(t...)
		begin, end := sub.FDBRangeKeys()
		sm.push(idx, []byte(begin.FDBKey()))
		sm.push(idx, []byte(end.FDBKey()))

	case "CONTAINS":
		key := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, int64(0))
			return true, nil
		}
		if ss.Contains(gofdb.Key(key)) {
			sm.push(idx, int64(1))
		} else {
			sm.push(idx, int64(0))
		}

	case "OPEN_SUBSPACE":
		t := sm.popTupleRaw()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			sm.appendNull()
			return true, nil
		}
		newSS := ss.Sub(t...)
		sm.appendDir(dirEntry{ss: newSS})

	case "LOG_SUBSPACE":
		prefix := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			return true, nil
		}
		logKey := append(append([]byte{}, prefix...), ourtuple.Tuple{int64(sm.dirIndex)}.Pack()...)
		tr := sm.currentTr()
		tr.Set(logKey, ss.Bytes())

	case "LOG_DIRECTORY":
		prefix := sm.popBytes()
		logSS := subspace.FromBytes(append(append([]byte{}, prefix...), ourtuple.Tuple{int64(sm.dirIndex)}.Pack()...))
		d := sm.currentDir()

		// Log path
		var pathTuple ourtuple.Tuple
		if d.dir != nil {
			for _, p := range d.dir.GetPath() {
				pathTuple = append(pathTuple, p)
			}
		}
		tr := sm.currentTr()
		tr.Set(logSS.Pack(ourtuple.Tuple{"path"}), pathTuple.Pack())

		// Log layer
		var layerTuple ourtuple.Tuple
		if d.dir != nil {
			layerTuple = ourtuple.Tuple{d.dir.GetLayer()}
		} else {
			layerTuple = ourtuple.Tuple{[]byte{}}
		}
		tr.Set(logSS.Pack(ourtuple.Tuple{"layer"}), layerTuple.Pack())

		// Log exists
		rt := sm.getReadTransactor(isDatabase, isSnapshot)
		var existsVal int64
		if d.dir != nil {
			exists, err := d.dir.Exists(rt, nil)
			if err == nil && exists {
				existsVal = 1
			}
		}
		tr.Set(logSS.Pack(ourtuple.Tuple{"exists"}), ourtuple.Tuple{existsVal}.Pack())

		// Log children
		var childTuple ourtuple.Tuple
		if d.dir != nil {
			children, err := d.dir.List(rt, nil)
			if err == nil {
				for _, c := range children {
					childTuple = append(childTuple, c)
				}
			}
		}
		tr.Set(logSS.Pack(ourtuple.Tuple{"children"}), childTuple.Pack())

	case "STRIP_PREFIX":
		data := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true, nil
		}
		prefix := ss.Bytes()
		if !bytes.HasPrefix(data, prefix) {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		} else {
			sm.push(idx, data[len(prefix):])
		}

	default:
		return false, nil
	}

	return true, nil
}
