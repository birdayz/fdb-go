package types

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// TestRelOffDebug traces the exact positions of all objects and relative
// offsets in a GetKeyServerLocationsRequest to find the codegen bug.
func TestRelOffDebug(t *testing.T) {
	req := GetKeyServerLocationsRequest{
		Begin:            []byte("test_key"),
		Limit:            100,
		Reverse:          false,
		Reply:            ReplyPromise{},
		Tenant:           TenantInfo{TenantId: -1},
		MinTenantVersion: -1,
	}

	// Step 1: measureEndOff
	endOff := 0
	e0 := endOff
	endOff = wire.MeasureBytesOOL(endOff, req.Begin)
	t.Logf("After Begin OOL: endOff=%d (delta=%d)", endOff, endOff-e0)

	e1 := endOff
	endOff = req.Tenant.measureEndOff(endOff)
	t.Logf("After Tenant: endOff=%d (delta=%d)", endOff, endOff-e1)

	e2 := endOff
	endOff = req.SpanContext.measureEndOff(endOff)
	t.Logf("After SpanContext: endOff=%d (delta=%d)", endOff, endOff-e2)

	e3 := endOff
	endOff = req.Reply.measureEndOff(endOff)
	t.Logf("After Reply: endOff=%d (delta=%d)", endOff, endOff-e3)

	t.Logf("Total endOff before root object: %d", endOff)

	// Step 2: MarshalFDB size calculation
	vt := GetKeyServerLocationsRequestVTable
	bodySize := int(vt[1]) - 4
	t.Logf("Root VTable: vtSize=%d objSize=%d bodySize=%d", vt[0], vt[1], bodySize)

	msgObjEnd := ((endOff + bodySize + 4 - 1) &^ (4 - 1)) + 4
	t.Logf("msgObjEnd=%d", msgObjEnd)

	fakeRootEnd := ((msgObjEnd + 4 + 3) &^ 3) + 4
	t.Logf("fakeRootEnd=%d", fakeRootEnd)

	tmpl := GetKeyServerLocationsRequestTemplate
	vtableSize := tmpl.PackedVTablesLen()
	vtableEnd := fakeRootEnd + vtableSize
	t.Logf("vtableSize=%d vtableEnd=%d", vtableSize, vtableEnd)

	totalSize := (vtableEnd + 8 + 7) &^ 7
	vtablePos := totalSize - vtableEnd
	fakeRootPos := totalSize - fakeRootEnd
	msgObjPos := totalSize - msgObjEnd
	t.Logf("totalSize=%d vtablePos=%d fakeRootPos=%d msgObjPos=%d", totalSize, vtablePos, fakeRootPos, msgObjPos)

	// Step 3: writeDirect trace
	buf := req.MarshalFDB()
	t.Logf("Marshaled %d bytes", len(buf))
	t.Logf("Go hex:  %s", hex.EncodeToString(buf))

	// Parse the C++ ground truth for comparison
	cppHex := "" // Will fill from testdata.json
	vecs := loadTestVectors(t)
	for _, v := range vecs {
		if v.Name == "GetKeyServerLocationsRequest_basic" {
			cppBytes, _ := hex.DecodeString(v.Hex)
			if len(cppBytes) >= 8 && cppBytes[7] == 0x0F && cppBytes[6] == 0xDB {
				cppBytes = cppBytes[8:]
			}
			cppHex = hex.EncodeToString(cppBytes)
			t.Logf("C++ hex: %s", cppHex)
			t.Logf("C++ len: %d", len(cppBytes))

			// Parse C++ object positions
			cppRootOff := binary.LittleEndian.Uint32(cppBytes[0:4])
			t.Logf("C++ rootOff=%d", cppRootOff)

			// FakeRoot at cppRootOff
			if int(cppRootOff)+8 <= len(cppBytes) {
				frVTSoff := int32(binary.LittleEndian.Uint32(cppBytes[cppRootOff : cppRootOff+4]))
				msgRelOff := binary.LittleEndian.Uint32(cppBytes[cppRootOff+4 : cppRootOff+8])
				cppMsgObjPos := int(cppRootOff) + 4 + int(msgRelOff)
				t.Logf("C++ FakeRoot: vtSoff=%d msgRelOff=%d → msgObjPos=%d", frVTSoff, msgRelOff, cppMsgObjPos)

				// Parse message object fields
				if cppMsgObjPos+int(vt[1]) <= len(cppBytes) {
					msgObj := cppBytes[cppMsgObjPos:]
					t.Logf("C++ message object at %d:", cppMsgObjPos)
					nFields := (int(vt[0]) - 4) / 2
					for i := 0; i < nFields && i < 10; i++ {
						fieldOff := int(vt[i+2])
						if fieldOff > 0 && fieldOff+4 <= int(vt[1]) {
							val := binary.LittleEndian.Uint32(msgObj[fieldOff : fieldOff+4])
							t.Logf("  field[%d] at objOff=%d: raw=0x%08x (%d)", i, fieldOff, val, val)
						}
					}
				}
			}

			// Same for Go
			goRootOff := binary.LittleEndian.Uint32(buf[0:4])
			t.Logf("Go rootOff=%d", goRootOff)
			if int(goRootOff)+8 <= len(buf) {
				goFRVTSoff := int32(binary.LittleEndian.Uint32(buf[goRootOff : goRootOff+4]))
				goMsgRelOff := binary.LittleEndian.Uint32(buf[goRootOff+4 : goRootOff+8])
				goMsgObjPos := int(goRootOff) + 4 + int(goMsgRelOff)
				t.Logf("Go FakeRoot: vtSoff=%d msgRelOff=%d → msgObjPos=%d", goFRVTSoff, goMsgRelOff, goMsgObjPos)

				if goMsgObjPos+int(vt[1]) <= len(buf) {
					msgObj := buf[goMsgObjPos:]
					t.Logf("Go message object at %d:", goMsgObjPos)
					nFields := (int(vt[0]) - 4) / 2
					for i := 0; i < nFields && i < 10; i++ {
						fieldOff := int(vt[i+2])
						if fieldOff > 0 && fieldOff+4 <= int(vt[1]) {
							val := binary.LittleEndian.Uint32(msgObj[fieldOff : fieldOff+4])
							t.Logf("  field[%d] at objOff=%d: raw=0x%08x (%d)", i, fieldOff, val, val)
						}
					}
				}
			}
		}
	}
}
