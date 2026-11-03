package client

// captureCommit captures a commit of muts and writeConflicts with the read conflicts
// a commit would ship now (conflictRangesNowLocked), as the commit tests need.
func (tx *Transaction) captureCommit(muts []Mutation, writeConflicts []KeyRange) *commitInput {
	tx.conflictMu.Lock()
	reads, _ := tx.conflictRangesNowLocked()
	tx.conflictMu.Unlock()
	return tx.captureCommitVectors(muts, false, writeConflicts, false, reads)
}
