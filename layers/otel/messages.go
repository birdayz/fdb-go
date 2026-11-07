package otel

// Store-side helpers of the GenAI message store (messages_rel.go): the TEXT
// pipeline sizer, BunchedMap counters and the oversized-transaction check.

import (
	"fdb.dev/pkg/recordlayer"
)

// messagePipelineSizer overlaps token-map reads for TEXT_INDEX_UPDATE only;
// other operations retain Java’s default pipeline sizing.
func messagePipelineSizer(op recordlayer.PipelineOperation) int {
	if op == recordlayer.PipelineOperationTextIndexUpdate {
		return messageTextPipeline
	}
	return recordlayer.DefaultPipelineSizer(op)
}

// messageTimerStats reads the BunchedMap counters of one message transaction.
func messageTimerStats(t *recordlayer.StoreTimer, s *MessageStoreStats) {
	s.TextIndexWrites = t.GetCount(recordlayer.CountSaveIndexKey)
	s.TextIndexKeyBytes = t.GetCount(recordlayer.CountSaveIndexKeyBytes)
	s.TextIndexValueBytes = t.GetCount(recordlayer.CountSaveIndexValueBytes)
	s.TextIndexReadBytes = t.GetCount(recordlayer.CountLoadIndexKeyBytes) + t.GetCount(recordlayer.CountLoadIndexValueBytes)
}

// isTransactionTooLarge reports FDB's transaction_too_large (2101).
func isTransactionTooLarge(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == 2101
}
