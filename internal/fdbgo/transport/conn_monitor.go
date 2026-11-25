package transport

import (
	"encoding/binary"
	mathrand "math/rand/v2"
	"time"

	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// WLTokenPingPacket is the well-known token for PING keepalive.
const WLTokenPingPacket = 1

// handlePing responds to a PingRequest from the server.
// PingRequest has file_identifier 4707015 and a single field: ReplyPromise<Void>.
// The reply token is a 16-byte UID embedded in the body. We extract it and
// send back a minimal ErrorOr<Void> reply.
func (c *Conn) handlePing(body []byte) {
	replyToken, ok := extractPingReplyToken(body)
	if !ok {
		return
	}

	replyBody := buildVoidReply()

	// Enqueue ping reply fire-and-forget; dropped if tryEnqueue rejects admission
	// (high-water, pending FIFO waiters, close, or full legacy writeCh).
	c.tryEnqueue(writeReq{token: replyToken, body: replyBody})
}

// extractPingReplyToken extracts the reply UID from a PingRequest FlatBuffers body.
func extractPingReplyToken(body []byte) (UID, bool) {
	if len(body) < 40 {
		return UID{}, false
	}

	r, err := wire.NewReader(body)
	if err != nil {
		return UID{}, false
	}

	// Object serialization wraps ReplyPromise's inline UID in a nested table.
	if !r.FieldPresent(0) {
		return UID{}, false
	}

	nestedR, err := r.ReadNestedReader(0)
	if err != nil {
		return UID{}, false
	}

	// Read UID inline at the nested struct's field 0.
	if !nestedR.FieldPresent(0) {
		return UID{}, false
	}
	off := nestedR.FieldOffset(0)
	obj := nestedR.ObjectBytes()
	if off+16 > len(obj) {
		return UID{}, false
	}
	uid := UID{
		First:  binary.LittleEndian.Uint64(obj[off:]),
		Second: binary.LittleEndian.Uint64(obj[off+8:]),
	}
	return uid, true
}

func buildVoidReply() []byte {
	return (&types.VoidReply{}).MarshalFDB()
}

// connectionMonitor detects dead connections by sending outbound PINGs.
// Matches C++ FlowTransport.actor.cpp connectionMonitor() (lines 621-702):
//
// Outer loop:
//  1. Sleep CONNECTION_MONITOR_LOOP_TIME (1s), jittered — the client's ONLY
//     per-cycle delay; C++'s other one at :631 is server-only
//  2. If no pending requests → skip the PING and loop (see the divergence note
//     at the check itself)
//  3. Send PING
//  4. Inner loop: wait CONNECTION_MONITOR_TIMEOUT (2s) per round
//     - If bytesReceived unchanged → kill connection
//     - If bytesReceived changed → update baseline, continue inner loop
//     - If PING reply arrives → break (connection alive)
//
// The inner retry loop is key: a single 2s timeout with no bytes kills,
// but if ANY bytes arrive (server PINGs, other responses), the baseline
// resets and we wait another 2s. This tolerates slow-but-alive connections.
// jitteredDelay ports flow's delayJittered (flow.h:1481):
//
//	seconds * (DELAY_JITTER_OFFSET + DELAY_JITTER_RANGE * random01())
//
// with the knob values from Knobs.cpp:54-55 — offset 0.9, range 0.2 — so the
// result is uniform over [0.9d, 1.1d). Both constants are named rather than
// folded into a single ±10% because they are two independent knobs in C++ and a
// future divergence should show up against the knob it came from.
const (
	delayJitterOffset = 0.9
	delayJitterRange  = 0.2
)

func jitteredDelay(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (delayJitterOffset + delayJitterRange*mathrand.Float64()))
}

func (c *Conn) connectionMonitor() {
	defer c.loopWG.Done()
	defer c.recoverLoop("connectionMonitor")

	for {
		// ONE jittered delay per cycle, and it is the client's only one.
		//
		// C++'s FIRST `delay(CONNECTION_MONITOR_LOOP_TIME)`
		// (FlowTransport.actor.cpp:631) sits inside a block guarded by
		// `!FlowTransport::isClient() && !peer->destination.isPublic()` (:624),
		// so a CLIENT never executes it. The client path is delay(0) (:647),
		// the idle/unreferenced checks (:649-660), then ONE delayJittered
		// (:662), the PING, and delay(CONNECTION_MONITOR_TIMEOUT) (:672).
		//
		// Go used to sleep here AS WELL as below, porting the server-only
		// delay, which put worst-case dead-connection detection at
		// 2*loop+timeout instead of loop+timeout. That is why correcting the
		// knob VALUE alone made things worse rather than better: at the real
		// 1.0s the doubled loop moved the worst case to 4.0s, further from the
		// C client's 3.0s than the 3.5s the wrong value happened to give.
		//
		// The delay sits BEFORE the idle check rather than after it, unlike
		// C++. That is forced by the divergence noted below: Go's idle branch
		// loops instead of throwing, so a delay placed only on the ping path
		// would leave the idle path spinning hot.
		//
		// JITTERED, matching `delayJittered` at :662. Not cosmetic — without it
		// every connection a client holds pings on the same period, so a process
		// that opened many at once keeps them in lockstep and the PINGs arrive
		// as a synchronized burst.
		select {
		case <-time.After(jitteredDelay(c.monitorLoopInterval)):
		case <-c.ctx.Done():
			return
		}

		// C++ checks peer->reliable.empty() && peer->unsent.empty() &&
		// peer->outstandingReplies == 0 (:649).
		//
		// KNOWN DIVERGENCE, pre-existing and not introduced here: on that
		// branch C++ either THROWS — connection_unreferenced (:653) or
		// connection_idle (:658, past CONNECTION_MONITOR_IDLE_TIMEOUT = 180s) —
		// or falls through and PINGs anyway. Go instead skips the ping and
		// loops, so it never pings an idle connection and never closes one:
		// a dead idle socket goes undetected until TCP keepalive. Closing that
		// gap needs both missing knobs (IDLE_TIMEOUT and
		// UNREFERENCED_CLOSE_DELAY) and a peer-reference count Go does not
		// track, so it is a separate change with its own client-review lap.
		c.monitorCycles.Add(1)

		if !c.hasAnyPending() {
			continue
		}

		switch c.monitorPing() {
		case pingStop:
			return
		case pingDead:
			// monitorPing released the reply first, as the C++ actor destroys its
			// state before publishing connection_failed (ActorCompiler.cs:387-391).
			c.failConnection(errConnClosed)
			return
		}
	}
}

// pingOutcome is how one monitor PING wait ended.
type pingOutcome uint8

const (
	pingContinue pingOutcome = iota // reply, or a dropped PING with traffic still flowing
	pingStop                        // the connection context was cancelled
	pingDead                        // no bytes arrived within the timeout
)

// monitorPing sends one PING and waits for its reply (FlowTransport.actor.cpp:665-699).
// It owns the reply as C++ actor state does, releasing it on the joined monitor goroutine.
func (c *Conn) monitorPing() pingOutcome {
	replyCh, reply := c.sendPingWithReply()
	received := false
	if reply != nil {
		defer func() { releasePing(reply, received) }()
	}
	// A dropped PING has no handle and a nil replyCh, which is never selected.
	// Wait monitorTimeout per round. Kill only if bytesReceived is truly frozen.
	startingBytes := c.bytesReceived.Load()
	for {
		timer := time.NewTimer(c.monitorTimeout)
		select {
		case <-replyCh:
			// PING reply arrived (C++ :690-695), or failAllPending's teardown
			// response, after which the next wait sees the cancelled context.
			received = true
			timer.Stop()
			return pingContinue
		case <-timer.C:
			// Timeout. Check if ANY bytes arrived.
			current := c.bytesReceived.Load()
			if current == startingBytes {
				// No bytes at all since PING was sent — connection is dead.
				// C++ :674-679: throw connection_failed.
				return pingDead
			}
			// Bytes arrived (server PINGs, other traffic) but not our PING reply.
			// C++ :687-688: update baseline, loop again.
			// C++ uses timeouts counter here only for logging (ConnectionSlowPing),
			// NOT for kill decisions — the kill condition is solely bytesReceived.
			startingBytes = current
			if replyCh == nil {
				// The PING was never sent (writeCh was saturated), so no reply can
				// arrive on this nil channel — the wait has no success path.
				// We just confirmed bytes are still flowing, so the connection is
				// NOT frozen; staying here would wait forever for an impossible
				// reply and kill a healthy-but-quiet connection on the next idle
				// window (failing slow-but-live requests). Return to the outer loop
				// to re-attempt the PING next cycle, by which point writeCh has
				// likely drained. C++ never hits this — Peer::send is unbounded, so
				// the ping always goes out; this is the bounded-writeCh fallback.
				return pingContinue
			}
		case <-c.ctx.Done():
			timer.Stop()
			return pingStop
		}
	}
}

// releasePing ends the PING reply's registration. TakeReadyOrCancel takes a reply
// published after the wait ended (Close's teardown when the ctx arm won), so it is pooled.
func releasePing(reply *ReplyHandle, received bool) {
	if !received {
		reply.TakeReadyOrCancel()
	}
	reply.Release()
}

// sendPingWithReply registers and enqueues a PING, returning its reply channel and
// handle for releasePing; both are nil when the PING could not be enqueued.
func (c *Conn) sendPingWithReply() (<-chan Response, *ReplyHandle) {
	replyToken, replyCh, replyHandle := c.PrepareReply()
	transferred := false
	defer func() {
		if !transferred {
			replyHandle.Cancel()
			replyHandle.Release()
		}
	}()
	pingEP := WellKnownToken(WLTokenPingPacket)
	body := BuildPingRequest(replyToken)
	if !c.tryEnqueue(writeReq{token: pingEP, body: body}) {
		// Admission rejected (high-water, pending FIFO waiters, close, or full legacy
		// writeCh): return nil so the monitor falls through to bytesReceived liveness.
		return nil, nil
	}
	transferred = true
	return replyCh, replyHandle
}

// pingRequestFileID is from C++ fdbrpc/FlowTransport.actor.cpp.
const pingRequestFileID uint32 = 4707015

// pingRequestVTable: PingRequest has 1 field (ReplyPromise/RelativeOffset, align 4).
// vtable[0] = 2*1+4 = 6 (vtable wire size)
// vtable[1] = 4+4 = 8 (object size with soffset)
// vtable[2] = 4 (field 0 offset)
var pingRequestVTable = wire.VTable{6, 8, 4}

var pingRequestVTableClosure = []wire.VTable{
	{6, 8, 4}, // PingRequest
	types.ReplyPromiseVTable,
	{6, 8, 4}, // FakeRoot
}

var pingRequestTemplate = wire.NewMessageTemplate(
	pingRequestFileID, pingRequestVTable, 4, pingRequestVTableClosure,
)

// BuildPingRequest builds a PingRequest FlatBuffers body.
// ReplyPromise's object-serializer trait emits a nested table with an inline UID.
func BuildPingRequest(replyToken UID) []byte {
	return wire.MarshalDirect(
		pingRequestTemplate,
		func(endOff int) int {
			return wire.MeasureObject(endOff, types.ReplyPromiseVTable, types.ReplyPromiseMaxAlign)
		},
		func(dw *wire.DirectWriter) int {
			replyPos, reply := dw.WriteObject(types.ReplyPromiseVTable, types.ReplyPromiseMaxAlign)
			tokenOff := int(types.ReplyPromiseVTable[types.ReplyPromiseSlotToken+2])
			binary.LittleEndian.PutUint64(reply[tokenOff:], replyToken.First)
			binary.LittleEndian.PutUint64(reply[tokenOff+8:], replyToken.Second)

			// Write the PingRequest object.
			objPos, obj := dw.WriteObject(pingRequestVTable, 4)

			// Field 0: RelativeOffset to the ReplyPromise table.
			fieldOff := int(pingRequestVTable[2]) // offset 4
			wire.PatchRelOff(obj, fieldOff, objPos, replyPos)

			return objPos
		},
	)
}
