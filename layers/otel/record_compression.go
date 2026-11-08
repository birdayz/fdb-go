package otel

import "fdb.dev/pkg/recordlayer"

// RecordSerializer is the Record Layer serializer of a --record-compression
// mode, nil for none ("" or "none"). The zlib modes are Java-readable deflate;
// zlib-default is Deflater.DEFAULT_COMPRESSION, the relational engine's own
// write format.
func RecordSerializer(mode string) (*recordlayer.TransformedRecordSerializer, error) {
	b := recordlayer.NewTransformedRecordSerializerBuilder()
	switch mode {
	case "", "none":
		return nil, nil
	case "zlib-1":
		return b.SetCompressWhenSerializing(true).SetCompressionLevel(1).Build()
	case "zlib-default":
		return b.SetCompressWhenSerializing(true).SetCompressionLevel(recordlayer.DeflaterDefaultCompression).Build()
	default:
		return nil, fail("config", "record compression", "unsupported Record Layer compression mode")
	}
}
