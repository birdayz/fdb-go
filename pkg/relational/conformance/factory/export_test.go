package factory

// CheckPartitionForTest exposes the TLP partition property to the package's
// external tests.
//
// The property is not exported on the production surface because nothing
// outside this package should be able to declare a partition sound; it is
// exposed to tests because a checker with no detector is a checker that
// blesses everything, and the detector has to be able to hand it partitions a
// real run cannot produce on demand.
func CheckPartitionForTest(unfiltered, pos, neg, unknown [][]any) string {
	return checkPartition(unfiltered, pos, neg, unknown)
}
