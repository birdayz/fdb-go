package types

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

func TestTraceGRV(t *testing.T) {
	req := GetReadVersionRequest{
		Flags:            1,
		TransactionCount: 1,
		MaxVersion:       -1,
	}

	// Measure
	endOff := 0
	endOff = wire.MeasureRawOOL(endOff, req.Tags)
	t.Logf("After Tags: endOff=%d", endOff)
	endOff = req.Reply.measureEndOff(endOff)
	t.Logf("After Reply: endOff=%d", endOff)
	endOff = req.SpanContext.measureEndOff(endOff)
	t.Logf("After SpanContext: endOff=%d", endOff)

	vt := GetReadVersionRequestVTable
	bodySize := int(vt[1]) - 4
	t.Logf("VTable: vtSize=%d objSize=%d bodySize=%d maxAlign=8", vt[0], vt[1], bodySize)

	msgObjEnd := ((endOff + bodySize + 8 - 1) &^ (8 - 1)) + 4
	t.Logf("msgObjEnd=%d", msgObjEnd)

	fakeRootEnd := ((msgObjEnd + 4 + 3) &^ 3) + 4
	t.Logf("fakeRootEnd=%d", fakeRootEnd)

	vtableSize := GetReadVersionRequestTemplate.PackedVTablesLen()
	vtableEnd := fakeRootEnd + vtableSize
	t.Logf("vtableSize=%d vtableEnd=%d", vtableSize, vtableEnd)

	totalSize := (vtableEnd + 8 + 7) &^ 7
	t.Logf("totalSize=%d (Go)", totalSize)
	t.Logf("C++ totalNoPrefix=168")
	t.Logf("Delta=%d", totalSize-168)

	// Layout
	vtablePos := totalSize - vtableEnd
	fakeRootPos := totalSize - fakeRootEnd
	msgObjPos := totalSize - msgObjEnd
	t.Logf("vtablePos=%d fakeRootPos=%d msgObjPos=%d", vtablePos, fakeRootPos, msgObjPos)
	t.Logf("C++: vtPos=40 fakeRootPos=60 msgObjPos=68")
}
