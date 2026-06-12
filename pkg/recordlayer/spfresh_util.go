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
	m := rabitq.Metric(config.Metric)
	if config.Metric == VectorMetricEuclideanSquare {
		// rabitq has no square variant — its Euclidean estimator IS squared
		// L2 (same ordering); only the exact re-rank differs (no sqrt),
		// which vectorDistance handles. The raw int cast would otherwise
		// feed rabitq an enum value it does not define.
		m = rabitq.MetricEuclidean
	}
	return rabitq.NewQuantizer(m, config.NumExBits)
}
