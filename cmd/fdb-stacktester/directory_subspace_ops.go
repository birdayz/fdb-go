//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	ourtuple "fdb.dev/pkg/fdbgo/fdb/tuple"
)

func (sm *StackMachine) executeDirectorySubspaceOp(ctx context.Context, idx int, dirOp string, isDatabase, isSnapshot bool) bool {
	switch dirOp {
	case "PACK_KEY":
		t := sm.popTupleRaw()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true
		}
		sm.push(idx, []byte(ss.Pack(t)))

	case "UNPACK_KEY":
		key := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true
		}
		t, err := ss.Unpack(gofdb.Key(key))
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		} else {
			for _, elem := range t {
				sm.push(idx, stackTupleElement(elem))
			}
		}

	case "RANGE":
		t := sm.popTupleRaw()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true
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
			return true
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
			return true
		}
		newSS := ss.Sub(t...)
		sm.appendDir(dirEntry{ss: newSS})

	case "LOG_SUBSPACE":
		prefix := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			return true
		}
		logKey := append(append([]byte{}, prefix...), ourtuple.Tuple{int64(sm.dirIndex)}.Pack()...)
		tr := sm.currentTr()
		tr.Set(logKey, ss.Bytes())

	case "LOG_DIRECTORY":
		sm.execDirLogDirectory(ctx, idx, isDatabase, isSnapshot)

	case "STRIP_PREFIX":
		data := sm.popBytes()
		ss := sm.currentDir().asSubspace()
		if ss == nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return true
		}
		prefix := ss.Bytes()
		if !bytes.HasPrefix(data, prefix) {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
		} else {
			sm.push(idx, data[len(prefix):])
		}

	default:
		return false
	}
	return true
}

func (sm *StackMachine) execDirLogDirectory(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	prefix := sm.popBytes()
	logSS := subspace.FromBytes(append(append([]byte{}, prefix...), ourtuple.Tuple{int64(sm.dirIndex)}.Pack()...))
	d := sm.currentDir()

	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		return
	}
	// Evaluate reads before writing any log fields, as the Python tester does.
	rt := sm.getReadTransactor(ctx, isDatabase, isSnapshot)
	exists, err := d.dir.Exists(rt, nil)
	if err != nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		return
	}
	var existsVal int64
	var childTuple ourtuple.Tuple
	if exists {
		existsVal = 1
		children, err := d.dir.List(rt, nil)
		if err != nil {
			sm.push(idx, []byte("DIRECTORY_ERROR"))
			return
		}
		for _, c := range children {
			childTuple = append(childTuple, c)
		}
	}
	var pathTuple ourtuple.Tuple
	for _, p := range d.dir.GetPath() {
		pathTuple = append(pathTuple, p)
	}
	layerTuple := ourtuple.Tuple{d.dir.GetLayer()}
	tr := sm.currentTr()
	tr.Set(logSS.Pack(ourtuple.Tuple{"path"}), pathTuple.Pack())
	tr.Set(logSS.Pack(ourtuple.Tuple{"layer"}), layerTuple.Pack())
	tr.Set(logSS.Pack(ourtuple.Tuple{"exists"}), ourtuple.Tuple{existsVal}.Pack())
	tr.Set(logSS.Pack(ourtuple.Tuple{"children"}), childTuple.Pack())
}
