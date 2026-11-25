//go:build bazelrunfiles

package main

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/directory"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	ourtuple "fdb.dev/pkg/fdbgo/fdb/tuple"
)

func (sm *StackMachine) executeDirectoryLifecycleOp(ctx context.Context, idx int, dirOp string, isDatabase, isSnapshot bool) bool {
	switch dirOp {
	case "CREATE_SUBSPACE":
		t := sm.popTupleRaw()
		rawPrefix := sm.popBytes()
		ss := subspace.FromBytes(append(rawPrefix, t.Pack()...))
		sm.appendDir(dirEntry{ss: ss})
	case "CREATE_LAYER":
		sm.execDirCreateLayer()
	case "CREATE_OR_OPEN":
		sm.execDirCreateOrOpen(ctx, idx, isDatabase)
	case "CREATE":
		sm.execDirCreate(ctx, idx, isDatabase)
	case "OPEN":
		sm.execDirOpen(ctx, idx, isDatabase, isSnapshot)
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
		sm.execDirMove(ctx, idx, isDatabase)
	case "MOVE_TO":
		sm.execDirMoveTo(ctx, idx, isDatabase)
	case "REMOVE":
		sm.execDirRemove(ctx, idx, isDatabase, false)
	case "REMOVE_IF_EXISTS":
		sm.execDirRemove(ctx, idx, isDatabase, true)
	case "LIST":
		sm.execDirList(ctx, idx, isDatabase, isSnapshot)
	case "EXISTS":
		sm.execDirExists(ctx, idx, isDatabase, isSnapshot)
	default:
		return false
	}
	return true
}

func (sm *StackMachine) execDirCreateLayer() {
	idx1 := int(sm.popInt64())
	idx2 := int(sm.popInt64())
	allowManual := sm.popInt64() != 0

	e1 := &sm.dirList[idx1]
	e2 := &sm.dirList[idx2]
	if e1.isNull() || e2.isNull() {
		sm.appendNull()
		return
	}
	ss1 := e1.asSubspace()
	ss2 := e2.asSubspace()
	if ss1 == nil || ss2 == nil {
		sm.appendNull()
		return
	}
	dl := directory.NewDirectoryLayer(ss1, ss2, allowManual)
	sm.appendDir(dirEntry{dir: dl})
}

func (sm *StackMachine) execDirCreateOrOpen(ctx context.Context, idx int, isDatabase bool) {
	path := sm.popTupleAsPath()
	layer := sm.popLayer()
	tr := sm.getTransactor(ctx, isDatabase)
	d := sm.currentDir()
	if d.dir == nil {
		sm.pushError(idx, fmt.Errorf("directory error"))
		sm.appendNull()
		return
	}
	ds, err := d.dir.CreateOrOpen(tr, path, layer)
	if err != nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
	} else {
		sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
	}
}

func (sm *StackMachine) execDirCreate(ctx context.Context, idx int, isDatabase bool) {
	path := sm.popTupleAsPath()
	layer := sm.popLayer()
	prefix := sm.popBytesOrNil()
	tr := sm.getTransactor(ctx, isDatabase)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
		return
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
}

func (sm *StackMachine) execDirOpen(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	path := sm.popTupleAsPath()
	layer := sm.popLayer()
	rt := sm.getReadTransactor(ctx, isDatabase, isSnapshot)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
		return
	}
	ds, err := d.dir.Open(rt, path, layer)
	if err != nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
	} else {
		sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
	}
}

func (sm *StackMachine) execDirMove(ctx context.Context, idx int, isDatabase bool) {
	oldPath := sm.popTupleAsPath()
	newPath := sm.popTupleAsPath()
	tr := sm.getTransactor(ctx, isDatabase)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
		return
	}
	ds, err := d.dir.Move(tr, oldPath, newPath)
	if err != nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
	} else {
		sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
	}
}

func (sm *StackMachine) execDirMoveTo(ctx context.Context, idx int, isDatabase bool) {
	newAbsPath := sm.popTupleAsPath()
	tr := sm.getTransactor(ctx, isDatabase)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
		return
	}
	ds, err := d.dir.MoveTo(tr, newAbsPath)
	if err != nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		sm.appendNull()
	} else {
		sm.appendDir(dirEntry{dir: ds, dsb: ds, ss: ds})
	}
}

func (sm *StackMachine) execDirRemove(ctx context.Context, idx int, isDatabase, ifExists bool) {
	count := int(sm.popInt64())
	var path []string
	if count == 1 {
		path = sm.popTupleAsPath()
	}
	tr := sm.getTransactor(ctx, isDatabase)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		return
	}
	removed, err := d.dir.Remove(tr, path)
	if err != nil || (!ifExists && !removed) {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
	}
}

func (sm *StackMachine) execDirList(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	count := int(sm.popInt64())
	var path []string
	if count == 1 {
		path = sm.popTupleAsPath()
	}
	rt := sm.getReadTransactor(ctx, isDatabase, isSnapshot)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
		return
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
}

func (sm *StackMachine) execDirExists(ctx context.Context, idx int, isDatabase, isSnapshot bool) {
	count := int(sm.popInt64())
	var path []string
	if count == 1 {
		path = sm.popTupleAsPath()
	}
	rt := sm.getReadTransactor(ctx, isDatabase, isSnapshot)
	d := sm.currentDir()
	if d.dir == nil {
		sm.push(idx, int64(0))
		return
	}
	exists, err := d.dir.Exists(rt, path)
	if err != nil {
		sm.push(idx, []byte("DIRECTORY_ERROR"))
	} else if exists {
		sm.push(idx, int64(1))
	} else {
		sm.push(idx, int64(0))
	}
}
