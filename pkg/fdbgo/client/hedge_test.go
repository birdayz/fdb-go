package client

import (
	"context"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
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
