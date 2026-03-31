package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
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

	// Allocate reply token first — must be embedded in request body.
	replyToken, replyCh := conn.PrepareReply()

	// Build GetReadVersionRequest with reply token.
	body := buildGetReadVersionRequest(replyToken)

	// Send to the GRV proxy's endpoint token.
	if err := conn.SendFrame(proxy.Token, body); err != nil {
		return 0, err
	}

	// Wait for reply.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return 0, resp.Err
		}
		return parseGetReadVersionReply(resp.Body)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// buildGetReadVersionRequest constructs the request with embedded reply token.
// Real vtable from C++ test vector: {20, 37, 12, 16, 20, 36, 24, 28, 32, 4}
// Slot 5 (Reply) at offset 28: nested ReplyPromise struct (UID vtable {6,20,4})
// Slot 7 (MaxVersion) at offset 4: int64 (-1 = latest)
func buildGetReadVersionRequest(replyToken transport.UID) []byte {
	vt := types.GetReadVersionRequestVTable
	fileID := types.GetReadVersionRequestFileID

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// slot 7: MaxVersion at offset 4 (int64, -1 = latest version)
		obj.WriteInt64(4, -1)

		// slot 0: TransactionCount at offset 12 (uint32)
		obj.WriteUint32(12, 1)

		// slot 1: Flags at offset 16 (uint32, 0 = default priority)
		obj.WriteUint32(16, 0)

		// slot 5: Reply at offset 28 (nested ReplyPromise struct)
		replyVT := types.ReplyPromiseVTable
		obj.WriteStruct(28, replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})
	})
}

// parseGetReadVersionReply parses the ErrorOr-wrapped GRV response.
// The response follows the same flattened-FakeRoot ErrorOr pattern as ClientDBInfo.
func parseGetReadVersionReply(data []byte) (int64, error) {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return 0, fmt.Errorf("GRV: %w", err)
	}
	var reply types.GetReadVersionReply
	if err := reply.UnmarshalFDB(data); err != nil {
		return 0, fmt.Errorf("unmarshal GRV reply: %w", err)
	}
	return reply.Version, nil
}
