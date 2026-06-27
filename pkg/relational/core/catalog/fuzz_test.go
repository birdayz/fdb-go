package catalog

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
)

// FuzzDeserializeTemplate stresses the META_DATA blob deserialiser. This is
// the read-path for schema templates persisted in FDB — a malformed or
// attacker-crafted blob must never panic and must surface errors as typed
// `*api.Error` so callers can switch on SQLSTATE.
//
// Invariants:
//   - no panic, any bytes
//   - success returns (non-nil SchemaTemplate, nil error)
//   - failure returns (nil, *api.Error with a non-empty SQLSTATE)
//
// The second invariant catches "nil, nil" degraded-success paths some
// deserialisers fall into on empty input. The third ensures every error
// carries a code `errors.As` can extract.
func FuzzDeserializeTemplate(f *testing.F) {
	// An empty-but-valid MetaData proto — exercises "parses as proto but is
	// semantically minimal" path. RecordMetaDataFromProto should error on it.
	if empty, err := proto.Marshal(&gen.MetaData{}); err == nil {
		f.Add(empty)
	}
	// Pathological inputs:
	f.Add([]byte{})                       // zero-length
	f.Add([]byte{0x00})                   // single null byte
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}) // varint overflow bait
	f.Add([]byte{0x08})                   // lone tag without payload
	f.Add(make([]byte, 1024))             // long zero run

	f.Fuzz(func(t *testing.T, blob []byte) {
		msg := &gen.Templates{
			TEMPLATE_NAME:    proto.String("fuzz"),
			TEMPLATE_VERSION: proto.Int32(1),
			META_DATA:        blob,
		}
		tmpl, err := deserializeTemplate(msg)
		if err == nil {
			if tmpl == nil {
				t.Fatalf("deserializeTemplate succeeded but returned nil template for blob %x", blob)
			}
			return
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("deserializeTemplate returned non-api error %T: %v (blob %x)", err, err, blob)
		}
		if apiErr.Code == "" {
			t.Fatalf("deserializeTemplate returned api error with empty code: %v (blob %x)", err, blob)
		}
	})
}
