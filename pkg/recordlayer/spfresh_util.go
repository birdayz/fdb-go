package recordlayer

import (
	"context"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/rabitq"
)

// spfreshRun adapts FDBDatabase.Run for error-only transaction bodies.
func spfreshRun(ctx context.Context, db *FDBDatabase, fn func(rtx *FDBRecordContext) error) error {
	_, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		return nil, fn(rtx)
	})
	return err
}

func spfreshNowMs() int64 { return time.Now().UnixMilli() }

// spfreshNewRaBitQ builds the posting-residual quantizer from the config —
// the same in-tree RaBitQ the HNSW index uses, applied to residuals here
// (RFC-094 §7).
func spfreshNewRaBitQ(config SPFreshConfig) VectorQuantizer {
	return rabitq.NewQuantizer(rabitq.Metric(config.Metric), config.NumExBits)
}
