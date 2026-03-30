package client

import (
	"context"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
)

// GRVBatcher batches concurrent GetReadVersion requests into a single
// RPC to a GRV proxy, then fans out the result. This is critical for
// performance — without batching, every transaction independently
// requests a read version.
//
// Adaptive batching window: batchTime = 0.1 * (replyLatency * 0.5) + 0.9 * batchTime
type GRVBatcher struct {
	cluster   *Cluster
	mu        sync.Mutex
	pending   []grvRequest
	batchTime time.Duration
	timer     *time.Timer
}

type grvRequest struct {
	reply chan grvResult
}

type grvResult struct {
	version int64
	err     error
}

// NewGRVBatcher creates a batcher for GRV requests.
func NewGRVBatcher(cluster *Cluster) *GRVBatcher {
	return &GRVBatcher{
		cluster:   cluster,
		batchTime: 1 * time.Millisecond, // initial batch window
	}
}

// GetReadVersion requests a read version. Multiple concurrent calls
// are batched into a single proxy RPC.
func (b *GRVBatcher) GetReadVersion(ctx context.Context) (int64, error) {
	req := grvRequest{reply: make(chan grvResult, 1)}

	b.mu.Lock()
	b.pending = append(b.pending, req)
	if len(b.pending) == 1 {
		// First request in batch — start timer.
		b.timer = time.AfterFunc(b.batchTime, b.flush)
	}
	b.mu.Unlock()

	select {
	case result := <-req.reply:
		return result.version, result.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// flush sends the batched GRV request to a proxy and fans out the result.
func (b *GRVBatcher) flush() {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	start := time.Now()
	version, err := b.sendGRVRequest()
	elapsed := time.Since(start)

	// Adaptive batch window.
	b.mu.Lock()
	b.batchTime = time.Duration(0.1*float64(elapsed)/2 + 0.9*float64(b.batchTime))
	if b.batchTime < 100*time.Microsecond {
		b.batchTime = 100 * time.Microsecond
	}
	if b.batchTime > 10*time.Millisecond {
		b.batchTime = 10 * time.Millisecond
	}
	b.mu.Unlock()

	// Fan out result to all waiting goroutines.
	result := grvResult{version: version, err: err}
	for _, req := range batch {
		req.reply <- result
	}
}

func (b *GRVBatcher) sendGRVRequest() (int64, error) {
	proxy, err := b.cluster.GetGRVProxy()
	if err != nil {
		return 0, err
	}

	conn, err := b.cluster.getOrDial(context.Background(), proxy.Address)
	if err != nil {
		return 0, err
	}

	// Build GetReadVersionRequest.
	req := protocol.GetReadVersionRequest{
		TransactionCount: 1,
		Flags:            0, // default priority
	}
	body := req.MarshalFDB()

	// Send and wait for reply.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	replyBody, err := conn.SendAndWait(ctx, proxy.Token, body)
	if err != nil {
		return 0, err
	}

	// Parse reply.
	var reply protocol.GetReadVersionReply
	if err := reply.UnmarshalFDB(replyBody); err != nil {
		return 0, err
	}

	return reply.Version, nil
}
