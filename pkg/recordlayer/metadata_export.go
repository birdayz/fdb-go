package recordlayer

import (
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// WriteRecordMetaData serializes a RecordMetaData to the given writer in
// the canonical `com.apple.foundationdb.record.RecordMetaDataProto.MetaData`
// wire format — the same bytes FDBMetaDataStore writes into FDB.
//
// Intended for apps with programmatic metadata that want to ship a .pb
// artifact alongside their binaries so operator tooling (e.g. `frl --meta-file`)
// can introspect the store without adopting FDBMetaDataStore. Typical use:
//
//	func main() {
//	    meta := buildMyMetaData() // normal RecordMetaDataBuilder
//	    f, err := os.Create("meta.pb")
//	    // ... error handling ...
//	    if err := recordlayer.WriteRecordMetaData(meta, f); err != nil { … }
//	}
//
// The serialized MetaData includes the embedded `records` FileDescriptorProto
// and its transitive `dependencies`, so consumers don't need out-of-band
// access to the app's .proto files to decode records.
func WriteRecordMetaData(meta *RecordMetaData, w io.Writer) error {
	if meta == nil {
		return fmt.Errorf("WriteRecordMetaData: nil metadata")
	}
	mdProto, err := meta.ToProto()
	if err != nil {
		return fmt.Errorf("WriteRecordMetaData: convert to proto: %w", err)
	}
	bytes, err := proto.Marshal(mdProto)
	if err != nil {
		return fmt.Errorf("WriteRecordMetaData: marshal: %w", err)
	}
	if _, err := w.Write(bytes); err != nil {
		return fmt.Errorf("WriteRecordMetaData: write: %w", err)
	}
	return nil
}
