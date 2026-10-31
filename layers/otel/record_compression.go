package otel

import "fdb.dev/pkg/recordlayer"

// An absent mode preserves the original untransformed Record Layer format.
func recordCompressionMode(mode string) string {
	if mode == "none" {
		return ""
	}
	return mode
}

func recordSerializer(mode string) (*recordlayer.TransformedRecordSerializer, error) {
	b := recordlayer.NewTransformedRecordSerializerBuilder()
	switch recordCompressionMode(mode) {
	case "":
		return nil, nil
	case "zlib-1":
		return b.SetCompressWhenSerializing(true).SetCompressionLevel(1).Build()
	case "brotli-0":
		return b.SetGoOnlyBrotliCompression(0).Build()
	case "brotli-1":
		return b.SetGoOnlyBrotliCompression(1).Build()
	case "brotli-3":
		return b.SetGoOnlyBrotliCompression(3).Build()
	case "brotli-4":
		return b.SetGoOnlyBrotliCompression(4).Build()
	default:
		return nil, fail("config", "record compression", "unsupported Record Layer compression mode")
	}
}
