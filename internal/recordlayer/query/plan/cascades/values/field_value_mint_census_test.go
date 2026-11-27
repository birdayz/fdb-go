package values

import "testing"

// BenchmarkNewFlatFieldValueGateOff measures the same cost where it is actually
// paid: through the constructor, so the figure includes the call the hook adds
// rather than only the hook body.
func BenchmarkNewFlatFieldValueGateOff(b *testing.B) {
	leg, ordering := Census(), OrderingCensus()
	SetCensusHooks(nil, nil)
	b.Cleanup(func() { SetCensusHooks(leg, ordering) })
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = newFlatFieldValue("AID", UnknownType)
	}
}
