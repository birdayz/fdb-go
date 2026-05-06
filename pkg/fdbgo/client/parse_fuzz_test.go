package client

// Fuzz targets for wire protocol reply parsers.
// These verify that no combination of input bytes causes a panic.
// A production FDB server should never send garbage, but network corruption
// or a misbehaving proxy must not crash the client.

import "testing"

func FuzzParseGetKeyReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Errors are fine.
		parseGetKeyReply(data)
	})
}

func FuzzParseGetValueReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseGetValueReply(data)
	})
}

func FuzzParseGetKeyValuesReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 200))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseGetKeyValuesReply(data)
	})
}

func FuzzParseGetReadVersionReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseGetReadVersionReply(data)
	})
}

func FuzzParseWatchValueReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(make([]byte, 50))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseWatchValueReply(data)
	})
}

func FuzzParseWaitMetricsReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(make([]byte, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseWaitMetricsReply(data)
	})
}

func FuzzParseSplitRangeReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(make([]byte, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseSplitRangeReply(data)
	})
}

func FuzzParseGetKeyServerLocationsReply(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(make([]byte, 200))

	f.Fuzz(func(t *testing.T, data []byte) {
		parseGetKeyServerLocationsReply(data)
	})
}

// FuzzParseTagThrottleInfo: hand-rolled custom parser at
// tag_throttle.go:50. Wire format: uint32 count + per-entry
// (uint32 tagLen + tagLen bytes + 8-byte float64 tpsRate +
// 8-byte float64 duration). Each step has a bounds check; this
// fuzz target verifies no combination of input bytes causes a
// panic — important because the parser receives a server-attacker-
// controlled blob from the GRV reply.
func FuzzParseTagThrottleInfo(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}) // count=0
	// Single entry: count=1, tagLen=3 ("abc"), tpsRate=10.0, duration=5.0.
	f.Add([]byte{
		0x01, 0x00, 0x00, 0x00, // count=1
		0x03, 0x00, 0x00, 0x00, // tagLen=3
		'a', 'b', 'c',
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x24, 0x40, // tpsRate=10.0
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x14, 0x40, // duration=5.0
	})
	// Truncated mid-entry shapes — each must return without panic.
	f.Add([]byte{0x01, 0x00, 0x00, 0x00})                         // count=1, no entry data
	f.Add([]byte{0x01, 0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00}) // huge tagLen
	f.Add([]byte{0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) // count=5, missing entries
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})                         // count=2^32-1

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Errors and nil returns are both fine.
		parseTagThrottleInfo(data)
	})
}
