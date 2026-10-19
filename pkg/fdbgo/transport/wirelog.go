package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// WireLog records every frame sent/received on FDB connections.
// Enable via FDB_WIRE_LOG=/path/to/file.bin (binary format) or
// FDB_WIRE_LOG=stderr (human-readable hex to stderr).
//
// Binary format per entry:
//
//	[1] direction: 'S' = send, 'R' = recv
//	[8] timestamp: nanoseconds since Unix epoch, little-endian uint64
//	[8] token.First: little-endian uint64
//	[8] token.Second: little-endian uint64
//	[4] body length: little-endian uint32
//	[N] body bytes
//
// Use cmd/fdb-wirelog-dump to convert binary logs to human-readable form
// or diff two logs.
type WireLog struct {
	mu      sync.Mutex
	w       io.Writer
	binary  bool // true = binary format, false = hex to stderr
	stopped bool // under mu: a stopped log writes nothing
}

// globalWireLog is loaded once per logged frame.
var globalWireLog atomic.Pointer[WireLog]

func init() {
	path := os.Getenv("FDB_WIRE_LOG")
	if path == "" {
		return
	}
	if path == "stderr" {
		globalWireLog.Store(&WireLog{w: os.Stderr, binary: false})
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FDB_WIRE_LOG: cannot create %s: %v\n", path, err)
		return
	}
	globalWireLog.Store(&WireLog{w: f, binary: true})
}

// CaptureWireLog writes the wire log to w in the binary format until the
// returned stop is called. Once stop returns, nothing more is written to w,
// even by a frame logged concurrently, so w may then be read. It replaces any
// log already set (FDB_WIRE_LOG); stop removes the capture, leaving none.
func CaptureWireLog(w io.Writer) (stop func()) {
	wl := &WireLog{w: w, binary: true}
	globalWireLog.Store(wl)
	return func() {
		globalWireLog.CompareAndSwap(wl, nil)
		wl.mu.Lock()
		wl.stopped = true
		wl.mu.Unlock()
	}
}

// LogSend records an outbound frame.
func LogSend(token UID, body []byte) {
	if wl := globalWireLog.Load(); wl != nil {
		wl.log('S', token, body)
	}
}

// LogRecv records an inbound frame.
func LogRecv(token UID, body []byte) {
	if wl := globalWireLog.Load(); wl != nil {
		wl.log('R', token, body)
	}
}

func (wl *WireLog) log(dir byte, token UID, body []byte) {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	if wl.stopped {
		return
	}
	if wl.binary {
		wl.logBinary(dir, token, body)
	} else {
		wl.logText(dir, token, body)
	}
}

func (wl *WireLog) logBinary(dir byte, token UID, body []byte) {
	var hdr [29]byte
	hdr[0] = dir
	binary.LittleEndian.PutUint64(hdr[1:9], uint64(time.Now().UnixNano()))
	binary.LittleEndian.PutUint64(hdr[9:17], token.First)
	binary.LittleEndian.PutUint64(hdr[17:25], token.Second)
	binary.LittleEndian.PutUint32(hdr[25:29], uint32(len(body)))
	wl.w.Write(hdr[:])
	wl.w.Write(body)
}

func (wl *WireLog) logText(dir byte, token UID, body []byte) {
	d := "SEND"
	if dir == 'R' {
		d = "RECV"
	}
	// Print first 64 bytes as hex, truncate if longer
	hexLen := len(body)
	truncated := ""
	if hexLen > 64 {
		hexLen = 64
		truncated = fmt.Sprintf("... (%d more bytes)", len(body)-64)
	}
	fmt.Fprintf(wl.w, "[%s] token=%016x:%016x len=%d %x%s\n",
		d, token.First, token.Second, len(body), body[:hexLen], truncated)
}
