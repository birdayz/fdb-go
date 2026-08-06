package client

// Fuzz targets for wire protocol reply parsers.
// These verify that no combination of input bytes causes a panic.
// A production FDB server should never send garbage, but network corruption
// or a misbehaving proxy must not crash the client.

import (
	"bytes"
	"testing"
)

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

// FuzzParseGetReadVersionReply drives whole GRV reply payloads through the
// wire decoder and the tag-throttle conversion. The tag throttle map is decoded
// by the generated wire layer as a vector of objects, so the hostile-input
// surface is the reply decode itself — a byte-level parser for this field no
// longer exists, and seeding raw tag blobs here would fuzz nothing real.
//
// The GRV reply is server-controlled, so no combination of bytes may panic;
// errors and zero values are both acceptable outcomes.
func FuzzParseGetReadVersionReplyTagThrottle(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	// Real server->client payloads are exercised byte-exactly by
	// TestReplyGroundTruth; this target only has to prove no input panics.
	f.Add(bytes.Repeat([]byte{0xFF}, 64))

	f.Fuzz(func(t *testing.T, data []byte) {
		version, _, _, _, entries, _, err := parseGetReadVersionReply(data)
		if err != nil {
			return
		}
		_ = version
		// Must not panic on any decoded entry set.
		_ = parseTagThrottleInfo(entries)
	})
}
