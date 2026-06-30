package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"github.com/onsi/gomega"
)

func TestHedge_PrimaryRepliesBeforeTimer(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	replyCh := make(chan transport.Response, 1)
	replyCh <- transport.Response{Body: []byte("primary-wins")}

	primary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     replyCh,
			replyHandle: &transport.ReplyHandle{},
			addr:        "primary",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	secondaryCalled := false
	secondary := func() inFlightRPC {
		secondaryCalled = true
		return inFlightRPC{err: nil, addr: "secondary"}
	}

	result := sendFrameWithHedge(context.Background(), 1*time.Second, primary, secondary, 5*time.Second)
	g.Expect(result.err).NotTo(gomega.HaveOccurred())
	g.Expect(string(result.body)).To(gomega.Equal("primary-wins"))
	g.Expect(result.addr).To(gomega.Equal("primary"))
	g.Expect(secondaryCalled).To(gomega.BeFalse(), "secondary should not be called when primary replies fast")
}

func TestHedge_SecondaryWinsRace(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	// Primary: slow (never replies within test)
	primaryCh := make(chan transport.Response, 1)
	primary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     primaryCh,
			replyHandle: &transport.ReplyHandle{},
			addr:        "primary",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	// Secondary: replies immediately
	secondaryCh := make(chan transport.Response, 1)
	secondaryCh <- transport.Response{Body: []byte("secondary-wins")}
	secondary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     secondaryCh,
			replyHandle: &transport.ReplyHandle{},
			addr:        "secondary",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	// Hedge delay very short so secondary fires quickly
	result := sendFrameWithHedge(context.Background(), 1*time.Millisecond, primary, secondary, 5*time.Second)
	g.Expect(result.err).NotTo(gomega.HaveOccurred())
	g.Expect(string(result.body)).To(gomega.Equal("secondary-wins"))
	g.Expect(result.addr).To(gomega.Equal("secondary"))
}

func TestHedge_PrimarySendFails_FallsBackToSecondary(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	primary := func() inFlightRPC {
		return inFlightRPC{err: context.DeadlineExceeded, addr: "primary"}
	}

	secondaryCh := make(chan transport.Response, 1)
	secondaryCh <- transport.Response{Body: []byte("secondary-fallback")}
	secondary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     secondaryCh,
			replyHandle: &transport.ReplyHandle{},
			addr:        "secondary",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	result := sendFrameWithHedge(context.Background(), 10*time.Millisecond, primary, secondary, 5*time.Second)
	g.Expect(result.err).NotTo(gomega.HaveOccurred())
	g.Expect(string(result.body)).To(gomega.Equal("secondary-fallback"))
}

func TestHedge_NoSecondary_WaitsForPrimary(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	replyCh := make(chan transport.Response, 1)
	replyCh <- transport.Response{Body: []byte("only-server")}

	primary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     replyCh,
			replyHandle: &transport.ReplyHandle{},
			addr:        "only",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	result := sendFrameWithHedge(context.Background(), 10*time.Millisecond, primary, nil, 5*time.Second)
	g.Expect(result.err).NotTo(gomega.HaveOccurred())
	g.Expect(string(result.body)).To(gomega.Equal("only-server"))
}

func TestHedge_ContextCancellation(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Primary never replies
	primary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     make(chan transport.Response),
			replyHandle: &transport.ReplyHandle{},
			addr:        "primary",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	result := sendFrameWithHedge(ctx, 1*time.Second, primary, nil, 5*time.Second)
	g.Expect(result.err).To(gomega.MatchError(context.Canceled))
}

// TestHedge_ContextCancellation_AccountsPrimary pins RFC-010 #5 for the top-level ctx.Done branch:
// when the caller's ctx is cancelled DURING the primary-wait (before the hedge timer / secondary),
// the started primary request must still be recoverable for endRequest — else its QueueModel
// startRequest delta leaks permanently and biases server selection. Pre-fix the ctx.Done branch
// returned a bare hedgeResult{err}, dropping addr/delta/start, so applyCallerAccounting could not
// balance the primary (the leak sentinel totalOf stays elevated). Mirrors waitForReply's ctx.Done.
func TestHedge_ContextCancellation_AccountsPrimary(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately, before the primary reply

	const primaryDelta = 1.5
	primary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     make(chan transport.Response), // never replies
			replyHandle: &transport.ReplyHandle{},
			addr:        "primary",
			delta:       primaryDelta,
			start:       time.Now(),
		}
	}
	// A non-nil secondary is REQUIRED to reach the top-level select's ctx.Done branch (with a nil
	// secondary, sendFrameWithHedge returns via waitForReply, whose ctx.Done already accounts). The
	// secondary is never actually sent: ctx is already cancelled and hedgeDelay is large, so the
	// select takes ctx.Done before the hedge timer. (This is why the existing
	// TestHedge_ContextCancellation, which passes a nil secondary, never exercised the leak.)
	secondary := func() inFlightRPC {
		t.Error("secondary must not be sent: ctx is cancelled before the hedge timer")
		return inFlightRPC{replyHandle: &transport.ReplyHandle{}, addr: "secondary"}
	}
	result := sendFrameWithHedge(ctx, 1*time.Second, primary, secondary, 5*time.Second)
	g.Expect(result.err).To(gomega.MatchError(context.Canceled))
	// The result MUST carry the primary's accounting so the readpath caller endRequests its
	// startRequest delta (RFC-010 #5) — exactly as waitForReply's ctx.Done branch does. Pre-fix the
	// top-level ctx.Done branch returned a bare {err} (addr==""), so the caller's
	// `if result.addr != ""` guard skipped endRequest → the started request's QueueModel delta leaked.
	g.Expect(result.addr).To(gomega.Equal("primary"), "ctx.Done must return the primary addr for endRequest")
	g.Expect(result.delta).To(gomega.Equal(primaryDelta), "ctx.Done must return the primary delta for endRequest")
}

// mkInflight builds an inFlightRPC for hedge tests. If ready, its reply channel
// is pre-filled so it wins any race it enters.
func mkInflight(addr string, delta float64, ready bool) inFlightRPC {
	ch := make(chan transport.Response, 1)
	if ready {
		ch <- transport.Response{Body: []byte(addr)}
	}
	return inFlightRPC{
		replyCh:     ch,
		replyHandle: &transport.ReplyHandle{},
		addr:        addr,
		delta:       delta,
		start:       time.Now(),
	}
}

// applyCallerAccounting mirrors the post-hedge QueueModel accounting in
// readpath.go: end every non-winner started request (result.others), then the
// winner. Shared by the hedge + waitForReply accounting tests.
func applyCallerAccounting(qm *QueueModel, res hedgeResult) {
	for _, o := range res.others {
		qm.endRequest(o.addr, o.delta, time.Since(o.start), false)
	}
	if res.addr != "" {
		qm.endRequest(res.addr, res.delta, time.Since(res.start), res.err == nil)
	}
}

// totalOf returns a server's raw smoothOutstanding running total (the leak sentinel).
func totalOf(qm *QueueModel, addr string) float64 {
	return qm.getOrCreate(addr).smoothOutstanding.total
}

// TestRaceReplies_AccountsEveryStartedRequest pins RFC-010 #5: every request
// that startRequest was called for must be recoverable for endRequest. The
// winner is in result.addr/delta; every OTHER started request (the loser, or
// both arms on timeout/cancel) must appear in result.others exactly once.
func TestRaceReplies_AccountsEveryStartedRequest(t *testing.T) {
	t.Parallel()

	t.Run("a wins — loser b in others", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		res := raceReplies(context.Background(), mkInflight("a", 1.5, true), mkInflight("b", 2.5, false), 5*time.Second)
		g.Expect(res.addr).To(gomega.Equal("a"))
		g.Expect(res.others).To(gomega.HaveLen(1))
		g.Expect(res.others[0].addr).To(gomega.Equal("b"))
		g.Expect(res.others[0].delta).To(gomega.Equal(2.5))
	})

	t.Run("b wins — loser a in others", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		res := raceReplies(context.Background(), mkInflight("a", 1.5, false), mkInflight("b", 2.5, true), 5*time.Second)
		g.Expect(res.addr).To(gomega.Equal("b"))
		g.Expect(res.others).To(gomega.HaveLen(1))
		g.Expect(res.others[0].addr).To(gomega.Equal("a"))
		g.Expect(res.others[0].delta).To(gomega.Equal(1.5))
	})

	t.Run("timeout — no winner, both in others", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		res := raceReplies(context.Background(), mkInflight("a", 1.5, false), mkInflight("b", 2.5, false), 20*time.Millisecond)
		g.Expect(res.addr).To(gomega.Equal(""))
		g.Expect(res.err).To(gomega.HaveOccurred())
		g.Expect(res.others).To(gomega.HaveLen(2))
		g.Expect([]string{res.others[0].addr, res.others[1].addr}).To(gomega.ConsistOf("a", "b"))
	})

	t.Run("cancel — no winner, both in others", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := raceReplies(ctx, mkInflight("a", 1.5, false), mkInflight("b", 2.5, false), 5*time.Second)
		g.Expect(res.addr).To(gomega.Equal(""))
		g.Expect(res.others).To(gomega.HaveLen(2))
		g.Expect([]string{res.others[0].addr, res.others[1].addr}).To(gomega.ConsistOf("a", "b"))
	})
}

// TestHedge_QueueModelOutstandingReturnsToBaseline proves the end-to-end effect
// of RFC-010 #5: after a hedged round, applying the caller's accounting (end
// every result.others, then the winner) brings each server's smoothOutstanding
// running total back to its pre-request baseline. Before the fix the hedge loser
// (and both arms on timeout) leaked their startRequest delta forever.
func TestHedge_QueueModelOutstandingReturnsToBaseline(t *testing.T) {
	t.Parallel()

	t.Run("winner+loser both released", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		qm := newQueueModel()
		baseA, baseB := totalOf(qm, "a"), totalOf(qm, "b")
		dA := qm.startRequest("a")
		dB := qm.startRequest("b")
		g.Expect(totalOf(qm, "a")).To(gomega.BeNumerically(">", baseA))
		g.Expect(totalOf(qm, "b")).To(gomega.BeNumerically(">", baseB))

		res := raceReplies(context.Background(), mkInflight("a", dA, true), mkInflight("b", dB, false), 5*time.Second)
		applyCallerAccounting(qm, res)

		g.Expect(totalOf(qm, "a")).To(gomega.BeNumerically("~", baseA, 1e-9))
		g.Expect(totalOf(qm, "b")).To(gomega.BeNumerically("~", baseB, 1e-9), "loser b's delta must be released, not leaked")
	})

	t.Run("timeout releases both", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		qm := newQueueModel()
		baseA, baseB := totalOf(qm, "a"), totalOf(qm, "b")
		dA := qm.startRequest("a")
		dB := qm.startRequest("b")

		res := raceReplies(context.Background(), mkInflight("a", dA, false), mkInflight("b", dB, false), 20*time.Millisecond)
		applyCallerAccounting(qm, res)

		g.Expect(totalOf(qm, "a")).To(gomega.BeNumerically("~", baseA, 1e-9))
		g.Expect(totalOf(qm, "b")).To(gomega.BeNumerically("~", baseB, 1e-9))
	})

	t.Run("context cancel releases both", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		qm := newQueueModel()
		baseA, baseB := totalOf(qm, "a"), totalOf(qm, "b")
		dA := qm.startRequest("a")
		dB := qm.startRequest("b")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := raceReplies(ctx, mkInflight("a", dA, false), mkInflight("b", dB, false), 5*time.Second)
		applyCallerAccounting(qm, res)

		g.Expect(totalOf(qm, "a")).To(gomega.BeNumerically("~", baseA, 1e-9))
		g.Expect(totalOf(qm, "b")).To(gomega.BeNumerically("~", baseB, 1e-9))
	})
}

func TestHedge_ConnErrorOnReply(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	replyCh := make(chan transport.Response, 1)
	replyCh <- transport.Response{Err: context.DeadlineExceeded}

	primary := func() inFlightRPC {
		return inFlightRPC{
			replyCh:     replyCh,
			replyHandle: &transport.ReplyHandle{},
			addr:        "primary",
			delta:       1.0,
			start:       time.Now(),
		}
	}

	result := sendFrameWithHedge(context.Background(), 10*time.Millisecond, primary, nil, 5*time.Second)
	g.Expect(result.connErr).To(gomega.BeTrue())
	g.Expect(result.err).To(gomega.HaveOccurred())
}

// TestWaitForReply_AccountsSingleRequest covers the non-hedge (single-server)
// path's QueueModel accounting (RFC-010 #5 / codex gap 7): on timeout and on
// context-cancel, waitForReply returns the started request's addr/delta so the
// caller ends it exactly once and outstanding returns to baseline. Complements
// the raceReplies (two-server) accounting tests.
func TestWaitForReply_AccountsSingleRequest(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		qm := newQueueModel()
		base := totalOf(qm, "a")
		d := qm.startRequest("a")
		res := waitForReply(context.Background(), mkInflight("a", d, false), 20*time.Millisecond)
		g.Expect(res.addr).To(gomega.Equal("a"), "timeout must report the started addr so the caller can end it")
		applyCallerAccounting(qm, res)
		g.Expect(totalOf(qm, "a")).To(gomega.BeNumerically("~", base, 1e-9))
	})

	t.Run("context cancel", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)
		qm := newQueueModel()
		base := totalOf(qm, "a")
		d := qm.startRequest("a")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := waitForReply(ctx, mkInflight("a", d, false), 5*time.Second)
		applyCallerAccounting(qm, res)
		g.Expect(totalOf(qm, "a")).To(gomega.BeNumerically("~", base, 1e-9))
	})
}
