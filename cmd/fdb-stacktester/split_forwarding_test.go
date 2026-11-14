//go:build bazelrunfiles

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Record attempted SplitRange writes before sending, using the plaintext 12-byte frame header.
// Framing matches client/wire_recorder_test.go's recordingConn.
type splitRequestRecorder struct {
	mu       sync.Mutex
	requests []types.SplitRangeRequest
	problems []error
}

type splitRecordingConn struct {
	net.Conn
	recorder  *splitRequestRecorder
	mu        sync.Mutex
	buf       []byte
	connected bool
}

func (r *splitRequestRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &splitRecordingConn{Conn: raw, recorder: r}, nil
}

func (r *splitRequestRecorder) snapshot() ([]types.SplitRangeRequest, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.SplitRangeRequest(nil), r.requests...), append([]error(nil), r.problems...)
}

func (c *splitRecordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A fast reply may be consumed before Conn.Write returns. Publish first so
	// the operation cannot complete ahead of its request observation.
	c.observe(p)
	return c.Conn.Write(p)
}

// Caller holds c.mu; the parser-only unit test has no concurrent caller.
func (c *splitRecordingConn) observe(p []byte) {
	c.buf = append(c.buf, p...)
	if !c.connected {
		if len(c.buf) < transport.ConnectPacketSize {
			return
		}
		c.buf = c.buf[transport.ConnectPacketSize:]
		c.connected = true
	}
	for len(c.buf) >= 12 {
		size := int(binary.LittleEndian.Uint32(c.buf))
		if size < 16 || size > 100<<20 {
			c.recorder.mu.Lock()
			c.recorder.problems = append(c.recorder.problems, fmt.Errorf("invalid outgoing frame size%d", size))
			c.recorder.mu.Unlock()
			c.buf = nil
			return
		}
		if len(c.buf) < 12+size {
			return
		}
		_, body, err := transport.ReadFrame(bytes.NewReader(c.buf[:12+size]), false)
		if err != nil {
			c.recorder.mu.Lock()
			c.recorder.problems = append(c.recorder.problems, err)
			c.recorder.mu.Unlock()
		} else {
			offset := 0
			if len(body) >= 16 && body[7] == 0x0f && body[6] == 0xdb {
				offset = 8
			}
			if len(body) >= offset+8 && binary.LittleEndian.Uint32(body[offset+4:offset+8]) == types.SplitRangeRequestFileID {
				var request types.SplitRangeRequest
				err := request.UnmarshalFDB(body)
				c.recorder.mu.Lock()
				if err != nil {
					c.recorder.problems = append(c.recorder.problems, fmt.Errorf("decode SplitRangeRequest: %w", err))
				} else {
					request.Keys.Begin = bytes.Clone(request.Keys.Begin)
					request.Keys.End = bytes.Clone(request.Keys.End)
					c.recorder.requests = append(c.recorder.requests, request)
				}
				c.recorder.mu.Unlock()
			}
		}
		c.buf = c.buf[12+size:]
	}
}

type splitWriteObserverConn struct {
	net.Conn
	write func([]byte) (int, error)
}

func (c splitWriteObserverConn) Write(p []byte) (int, error) { return c.write(p) }

func TestSplitRequestRecorderPublishesBeforeRawWrite(t *testing.T) {
	t.Parallel()
	r := &splitRequestRecorder{}
	request := &types.SplitRangeRequest{Keys: types.KeyRangeRef{Begin: []byte("a"), End: []byte("z")}, ChunkSize: 131072}
	var frame bytes.Buffer
	if err := transport.WriteFrame(&frame, transport.UID{First: 1}, request.MarshalFDB(), false); err != nil {
		t.Fatal(err)
	}
	writes := 0
	c := &splitRecordingConn{connected: true, recorder: r, Conn: splitWriteObserverConn{write: func(p []byte) (int, error) {
		writes++
		// A peer may reply as soon as this write becomes visible, before return.
		requests, problems := r.snapshot()
		if len(problems) != 0 || len(requests) != 1 || requests[0].ChunkSize != 131072 || !bytes.Equal(requests[0].Keys.Begin, []byte("a")) || !bytes.Equal(requests[0].Keys.End, []byte("z")) {
			t.Errorf("raw write preceded request publication: requests=%v problems=%v", requests, problems)
		}
		if !bytes.Equal(p, frame.Bytes()) {
			t.Error("recorder changed raw write bytes")
		}
		return len(p), nil
	}}}
	n, err := c.Write(frame.Bytes())
	if err != nil || n != frame.Len() || writes != 1 {
		t.Fatalf("write=%d,%v calls=%d", n, err, writes)
	}
}

func TestSplitRequestRecorderHandlesFragmentedAndCoalescedFrames(t *testing.T) {
	t.Parallel()
	r := &splitRequestRecorder{}
	c := &splitRecordingConn{recorder: r}
	var input bytes.Buffer
	input.Write((&transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73}).Marshal())
	for _, size := range []int64{4096, 131072} {
		request := &types.SplitRangeRequest{Keys: types.KeyRangeRef{Begin: []byte("a"), End: []byte("z")}, ChunkSize: size}
		body := request.MarshalFDB()
		if size == 131072 {
			prefix := make([]byte, 8)
			binary.LittleEndian.PutUint64(prefix, transport.ProtocolVersion73)
			body = append(prefix, body...)
		}
		if err := transport.WriteFrame(&input, transport.UID{First: 1}, body, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := transport.WriteFrame(&input, transport.UID{First: 2}, (&types.VoidReply{}).MarshalFDB(), false); err != nil {
		t.Fatal(err)
	}
	all := bytes.Clone(input.Bytes())
	// Every byte boundary is a possible transport Write split.
	for split := 0; split <= len(all); split++ {
		r = &splitRequestRecorder{}
		c = &splitRecordingConn{recorder: r}
		c.observe(all[:split])
		c.observe(all[split:])
		requests, problems := r.snapshot()
		if len(c.buf) != 0 || len(problems) != 0 || len(requests) != 2 || requests[0].ChunkSize != 4096 || requests[1].ChunkSize != 131072 {
			t.Fatalf("split%d pending%d requests%v problems%v", split, len(c.buf), requests, problems)
		}
	}
	// Negative control: an empty recording cannot prove argument forwarding.
	empty, problems := (&splitRequestRecorder{}).snapshot()
	if len(empty) != 0 || len(problems) != 0 {
		t.Fatal("nonempty new recorder")
	}
	// Fail closed on invalid length/checksum/typed decode, not false absence.
	oversized := make([]byte, 12)
	binary.LittleEndian.PutUint32(oversized, 101<<20)
	badChecksum := append([]byte(nil), all[transport.ConnectPacketSize:]...)
	badChecksum[4] ^= 1
	badBody := make([]byte, 8)
	binary.LittleEndian.PutUint32(badBody, 1000)
	binary.LittleEndian.PutUint32(badBody[4:], types.SplitRangeRequestFileID)
	var badDecode bytes.Buffer
	if err := transport.WriteFrame(&badDecode, transport.UID{First: 1}, badBody, false); err != nil {
		t.Fatal(err)
	}
	for _, frame := range [][]byte{make([]byte, 12), oversized, badChecksum, badDecode.Bytes()} {
		r = &splitRequestRecorder{}
		c = &splitRecordingConn{recorder: r, connected: true}
		c.observe(frame)
		_, problems := r.snapshot()
		if len(problems) == 0 {
			t.Fatal("malformed frame was not reported")
		}
	}
}

func TestSplitPointsOpcodeForwardsTwoDistinctPoppedChunkSizes(t *testing.T) {
	t.Parallel()
	recorder := &splitRequestRecorder{}
	db, ctx := stacktesterDatabase(t, client.WithDialFunc(recorder.dial))
	begin, end := []byte("split-forward/a"), []byte("split-forward/z")
	for _, chunkSize := range []int64{4096, 131072} {
		t.Run(fmt.Sprint(chunkSize), func(t *testing.T) {
			before, problems := recorder.snapshot()
			if len(problems) != 0 {
				t.Fatalf("capture problems: %v", problems)
			}
			sm := NewStackMachine(db, []byte(fmt.Sprintf("split-forward/%d", chunkSize)))
			got := executeStackOperation(t, ctx, sm, "GET_RANGE_SPLIT_POINTS", begin, end, chunkSize)
			if !bytes.Equal(got, []byte("GOT_RANGE_SPLIT_POINTS")) {
				t.Fatalf("opcode result=%x", got)
			}
			after, problems := recorder.snapshot()
			if len(problems) != 0 {
				t.Fatalf("capture problems: %v", problems)
			}
			requests := after[len(before):]
			if len(requests) == 0 {
				t.Fatal("GET_RANGE_SPLIT_POINTS emitted zero observable SplitRange requests")
			}
			// A range may span multiple shards or retry; every request must
			// carry the exact operand, not merely at least one matching RPC.
			for _, request := range requests {
				if request.ChunkSize != chunkSize {
					t.Fatalf("emitted chunkSize=%d want popped literal%d", request.ChunkSize, chunkSize)
				}
				if bytes.Compare(request.Keys.Begin, begin) < 0 || bytes.Compare(request.Keys.End, end) > 0 || bytes.Compare(request.Keys.Begin, request.Keys.End) >= 0 {
					t.Fatalf("emitted range[%q,%q) outside requested[%q,%q)", request.Keys.Begin, request.Keys.End, begin, end)
				}
			}
		})
	}
}
