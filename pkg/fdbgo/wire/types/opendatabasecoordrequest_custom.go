package types

// opendatabasecoordrequest_custom.go — the OpenDatabaseCoordRequest a client
// sends, with its vector fields encoded as C++ does.
//
// The generated OpenDatabaseCoordRequest carries coordinators, hostnames,
// issues and supportedVersions as opaque byte strings, but in C++ they are
// vectors (CoordinationInterface.h:260-290): a coordinator decodes a byte
// string there as an empty or garbled vector. The coordinators field is load
// bearing: the first request a restarted coordinator sees seeds its leader
// monitor (Coordination.actor.cpp:331-333), so an empty list wedges it.

import (
	"encoding/binary"

	"fdb.dev/pkg/fdbgo/wire"
)

// ClientVersionRef is C++ ClientVersionRef (fdbclient/ClientVersion.h:27-75).
type ClientVersionRef struct {
	ClientVersion   []byte
	SourceVersion   []byte
	ProtocolVersion []byte
}

// Hostname is C++ Hostname (flow/Hostname.h:29-70).
type Hostname struct {
	Host    []byte
	Service []byte // decimal port
	IsTLS   bool
}

// FlagTLS is NetworkAddress::FLAG_TLS (flow/network.h).
const FlagTLS uint16 = 2

var (
	clientVersionRefVTable = wire.VTable{10, 16, 4, 8, 12}
	hostnameVTable         = wire.VTable{10, 13, 4, 8, 12}
)

// CoordOpenDatabaseRequest is OpenDatabaseCoordRequest with typed vectors. Its
// encoding is byte-identical to libfdb_c 7.3.77 (pinned by a captured request).
type CoordOpenDatabaseRequest struct {
	Issues            [][]byte
	SupportedVersions []ClientVersionRef
	TraceLogGroup     []byte
	KnownClientInfoID [16]byte
	ClusterKey        []byte
	Coordinators      []NetworkAddress
	Reply             ReplyPromise
	Hostnames         []Hostname
	Internal          bool
}

// precomputeVector mirrors the PrecomputeSize pass of C++ LoadSaveHelper::save
// for a vector whose elements are 4-byte offsets (flat_buffers.h:1206-1232).
func precomputeVector(ps *wire.PrecomputeSize, n int, elem func(i int)) {
	if n == 0 && ps.EmptyVectorOffset != -1 {
		return
	}
	self := ps.GetMessageWriter(n * 4)
	for i := 0; i < n; i++ {
		elem(i)
	}
	start := wire.RightAlign(ps.CurrentBufferSize+n*4, 4) + 4
	ps.Write(start)
	self.WriteToAt(ps, start-4)
	if n == 0 {
		ps.EmptyVectorOffset = ps.CurrentBufferSize
	}
}

// writeVector is the WriteToBuffer pass of precomputeVector; elem returns the
// element's end offset, which the vector stores as a relative offset.
func writeVector(wb *wire.WriteToBuffer, n int, elem func(i int) int) int {
	if n == 0 && wb.EmptyVectorOffset != -1 {
		return wb.EmptyVectorOffset
	}
	size := n * 4
	self := wb.GetMessageWriter(size, false)
	for i := 0; i < n; i++ {
		self.WriteRelativeOffset(elem(i), i*4)
	}
	start := wire.RightAlign(wb.CurrentBufferSize+size, 4) + 4
	padding := start - 4 - (wb.CurrentBufferSize + size)
	wb.WriteUint32(uint32(n), start)
	self.WriteToAt(start - 4)
	wb.WriteZeros(start-size-4, padding)
	if n == 0 {
		wb.EmptyVectorOffset = wb.CurrentBufferSize
	}
	return wb.CurrentBufferSize
}

// precomputeTable reserves a table object (SaveVisitorLambda, flat_buffers.h:897-978).
func precomputeTable(ps *wire.PrecomputeSize, vt wire.VTable, align int) {
	n := ps.GetMessageWriter(int(vt[1]))
	n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+int(vt[1])-4, align)+4)
}

// beginTable opens a table object and writes its vtable soffset.
func beginTable(wb *wire.WriteToBuffer, vt wire.VTable, vtableStart int, tmpl *wire.MessageTemplate) wire.BufferMessageWriter {
	w := wb.GetMessageWriter(int(vt[1]), true)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(int32(vtableStart-tmpl.VTableOffset(vt)-w.FinalLocation)))
	w.WriteScalar(b[:], 0)
	return w
}

// writeStringsTable writes a table whose fields are all byte strings followed by
// an optional bool; ClientVersionRef and Hostname both have that shape.
func writeStringsTable(wb *wire.WriteToBuffer, vt wire.VTable, vtableStart int, tmpl *wire.MessageTemplate, flag *bool, fields ...[]byte) int {
	offs := make([]int, len(fields))
	for i, f := range fields {
		offs[i], _ = wb.VisitDynamicSize(f)
	}
	w := beginTable(wb, vt, vtableStart, tmpl)
	if flag != nil && *flag {
		w.WriteScalar([]byte{1}, int(vt[2+len(fields)]))
	}
	for i, off := range offs {
		w.WriteRelativeOffset(off, int(vt[2+i]))
	}
	w.WriteToAt(w.FinalLocation)
	return w.FinalLocation
}

func (m *CoordOpenDatabaseRequest) precomputeSize(ps *wire.PrecomputeSize) {
	precomputeVector(ps, len(m.Issues), func(i int) { ps.VisitDynamicSize(len(m.Issues[i])) })
	precomputeVector(ps, len(m.SupportedVersions), func(i int) {
		v := &m.SupportedVersions[i]
		ps.VisitDynamicSize(len(v.ClientVersion))
		ps.VisitDynamicSize(len(v.SourceVersion))
		ps.VisitDynamicSize(len(v.ProtocolVersion))
		precomputeTable(ps, clientVersionRefVTable, 4)
	})
	ps.VisitDynamicSize(len(m.TraceLogGroup))
	ps.VisitDynamicSize(len(m.ClusterKey))
	precomputeVector(ps, len(m.Coordinators), func(i int) { m.Coordinators[i].precomputeSize(ps) })
	m.Reply.precomputeSize(ps)
	precomputeVector(ps, len(m.Hostnames), func(i int) {
		h := &m.Hostnames[i]
		ps.VisitDynamicSize(len(h.Host))
		ps.VisitDynamicSize(len(h.Service))
		precomputeTable(ps, hostnameVTable, 4)
	})
	precomputeTable(ps, OpenDatabaseCoordRequestVTable, OpenDatabaseCoordRequestMaxAlign)
}

func (m *CoordOpenDatabaseRequest) writeToBuffer(wb *wire.WriteToBuffer, vtableStart int, tmpl *wire.MessageTemplate) int {
	issuesOff := writeVector(wb, len(m.Issues), func(i int) int {
		off, _ := wb.VisitDynamicSize(m.Issues[i])
		return off
	})
	versionsOff := writeVector(wb, len(m.SupportedVersions), func(i int) int {
		v := &m.SupportedVersions[i]
		return writeStringsTable(wb, clientVersionRefVTable, vtableStart, tmpl, nil, v.ClientVersion, v.SourceVersion, v.ProtocolVersion)
	})
	traceLogGroupOff, _ := wb.VisitDynamicSize(m.TraceLogGroup)
	clusterKeyOff, _ := wb.VisitDynamicSize(m.ClusterKey)
	coordinatorsOff := writeVector(wb, len(m.Coordinators), func(i int) int {
		return m.Coordinators[i].writeToBuffer(wb, vtableStart, tmpl)
	})
	replyStart := m.Reply.writeToBuffer(wb, vtableStart, tmpl)
	hostnamesOff := writeVector(wb, len(m.Hostnames), func(i int) int {
		h := &m.Hostnames[i]
		return writeStringsTable(wb, hostnameVTable, vtableStart, tmpl, &h.IsTLS, h.Host, h.Service)
	})
	vt := OpenDatabaseCoordRequestVTable
	w := beginTable(wb, vt, vtableStart, tmpl)
	w.WriteScalar(m.KnownClientInfoID[:], int(vt[OpenDatabaseCoordRequestSlotKnownClientInfoID+2]))
	if m.Internal {
		w.WriteScalar([]byte{1}, int(vt[OpenDatabaseCoordRequestSlotInternal+2]))
	}
	w.WriteRelativeOffset(issuesOff, int(vt[OpenDatabaseCoordRequestSlotIssues+2]))
	w.WriteRelativeOffset(versionsOff, int(vt[OpenDatabaseCoordRequestSlotSupportedVersions+2]))
	w.WriteRelativeOffset(traceLogGroupOff, int(vt[OpenDatabaseCoordRequestSlotTraceLogGroup+2]))
	w.WriteRelativeOffset(clusterKeyOff, int(vt[OpenDatabaseCoordRequestSlotClusterKey+2]))
	w.WriteRelativeOffset(coordinatorsOff, int(vt[OpenDatabaseCoordRequestSlotCoordinators+2]))
	w.WriteRelativeOffset(replyStart, int(vt[OpenDatabaseCoordRequestSlotReply+2]))
	w.WriteRelativeOffset(hostnamesOff, int(vt[OpenDatabaseCoordRequestSlotHostnames+2]))
	w.WriteToAt(w.FinalLocation)
	return w.FinalLocation
}

// MarshalFDB encodes the request as an ObjectWriter message (save_with_vtables, flat_buffers.h:803-822).
func (m *CoordOpenDatabaseRequest) MarshalFDB() []byte {
	return m.marshalWith(OpenDatabaseCoordRequestTemplate)
}

// marshalWith takes the vtable order as a parameter: C++ packs vtables in
// pointer order (a std::set<const VTable*>, flat_buffers.h:770), which differs between binaries.
func (m *CoordOpenDatabaseRequest) marshalWith(t *wire.MessageTemplate) []byte {
	packedVT := t.PackedVTables()

	ps := wire.NewPrecomputeSize()
	defer wire.ReleasePrecomputeSize(ps)
	vtNoop := ps.GetMessageWriter(len(packedVT))
	m.precomputeSize(ps)
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+4, 4)+4)
	}
	vtNoop.WriteTo(ps)
	vtableStart := ps.CurrentBufferSize
	{
		n := ps.GetMessageWriter(8)
		n.WriteToAt(ps, wire.RightAlign(ps.CurrentBufferSize+8, 8))
	}

	buf := make([]byte, ps.CurrentBufferSize)
	wb := wire.NewWriteToBuffer(buf, vtableStart, ps.WriteToOffsets)
	defer wire.ReleaseWriteToBuffer(wb)
	vtW := wb.GetMessageWriter(len(packedVT), false)
	vtW.WriteScalar(packedVT, 0)
	rootStart := m.writeToBuffer(wb, vtableStart, t)

	fakeRootW := wb.GetMessageWriter(8, true)
	fakeRootStart := fakeRootW.FinalLocation
	fakeRootW.WriteRelativeOffset(rootStart, int(wire.FakeRootVTable[2]))
	{
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(int32(vtableStart-t.VTableOffset(wire.FakeRootVTable)-fakeRootStart)))
		fakeRootW.WriteScalar(b[:], 0)
	}
	fakeRootW.WriteToAt(fakeRootStart)

	vtW.WriteTo()
	footerW := wb.GetMessageWriter(8, false)
	footerW.WriteRelativeOffset(fakeRootStart, 0)
	{
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], OpenDatabaseCoordRequestFileID)
		footerW.WriteScalar(b[:], 4)
	}
	footerW.WriteToAt(wire.RightAlign(wb.CurrentBufferSize+8, 8))
	return buf
}
