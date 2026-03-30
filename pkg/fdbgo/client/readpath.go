package client

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
)

const (
	// ErrWrongShardServer is returned when the storage server no longer owns the key.
	ErrWrongShardServer = 1062
	// wrongShardRetryDelay matches CLIENT_KNOBS->WRONG_SHARD_SERVER_DELAY.
	wrongShardRetryDelay = 10 * time.Millisecond
)

// getValue sends a GetValueRequest to the appropriate storage server.
// Handles wrong_shard_server by invalidating the locality cache and retrying.
func (tx *Transaction) getValue(ctx context.Context, key []byte) ([]byte, error) {
	for attempts := 0; attempts < 5; attempts++ {
		servers, err := tx.db.locationCache.Locate(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("locate key: %w", err)
		}
		if len(servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		// Try each server (simple failover, no load balancing yet).
		for _, server := range servers {
			conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
			if err != nil {
				continue // try next server
			}

			req := protocol.GetValueRequest{
				Key:     key,
				Version: tx.readVersion,
			}
			body := req.MarshalFDB()

			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			replyBody, err := conn.SendAndWait(rctx, server.Token, body)
			cancel()

			if err != nil {
				continue // try next server
			}

			var reply protocol.GetValueReply
			if err := reply.UnmarshalFDB(replyBody); err != nil {
				return nil, fmt.Errorf("unmarshal GetValueReply: %w", err)
			}

			// TODO: Check for error in reply (LoadBalancedReply::error).
			// For now, return the value.
			return reply.Value, nil
		}

		// All servers failed — possibly wrong shard. Invalidate and retry.
		tx.db.locationCache.Invalidate(key)
		time.Sleep(wrongShardRetryDelay)
	}

	return nil, fmt.Errorf("getValue: all attempts failed for key")
}

// getRange sends a GetKeyValuesRequest for a key range.
func (tx *Transaction) getRange(ctx context.Context, begin, end []byte, limit int) ([]KeyValue, error) {
	servers, err := tx.db.locationCache.Locate(ctx, begin)
	if err != nil {
		return nil, fmt.Errorf("locate range begin: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no storage servers for range")
	}

	server := servers[0]
	conn, err := tx.db.cluster.getOrDial(ctx, server.Address)
	if err != nil {
		return nil, fmt.Errorf("dial storage server: %w", err)
	}

	req := protocol.GetKeyValuesRequest{
		Version: tx.readVersion,
	}
	// TODO: Set begin/end key selectors, limit, limitBytes.
	// These are scalar fields typed as []byte due to codegen gap (base class fields).
	_ = begin
	_ = end
	_ = limit

	body := req.MarshalFDB()

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	replyBody, err := conn.SendAndWait(rctx, server.Token, body)
	if err != nil {
		return nil, fmt.Errorf("getRange RPC: %w", err)
	}

	var reply protocol.GetKeyValuesReply
	if err := reply.UnmarshalFDB(replyBody); err != nil {
		return nil, fmt.Errorf("unmarshal GetKeyValuesReply: %w", err)
	}

	// TODO: Parse reply.Data (VectorRef<KeyValueRef>) into KeyValue slice.
	return nil, nil
}

// KeyValue is a key-value pair returned from reads.
type KeyValue struct {
	Key   []byte
	Value []byte
}
