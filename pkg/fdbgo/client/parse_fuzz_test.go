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
