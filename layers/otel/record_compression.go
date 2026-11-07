package otel

import "fdb.dev/pkg/recordlayer"

// An absent mode preserves the original untransformed Record Layer format.
func recordCompressionMode(mode string) string {
	if mode == "none" {
		return ""
	}
	return mode
}

// RecordSerializer is the Record Layer serializer of a --record-compression
// mode, nil for none. The zlib modes are Java-readable deflate; zlib-default
// is Deflater.DEFAULT_COMPRESSION, the relational engine's own write format.
func RecordSerializer(mode string) (*recordlayer.TransformedRecordSerializer, error) {
	return recordSerializer(mode)
}

func recordSerializer(mode string) (*recordlayer.TransformedRecordSerializer, error) {
	b := recordlayer.NewTransformedRecordSerializerBuilder()
	switch recordCompressionMode(mode) {
	case "":
		return nil, nil
	case "zlib-1":
		return b.SetCompressWhenSerializing(true).SetCompressionLevel(1).Build()
	case "zlib-default":
		return b.SetCompressWhenSerializing(true).SetCompressionLevel(recordlayer.DeflaterDefaultCompression).Build()
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
