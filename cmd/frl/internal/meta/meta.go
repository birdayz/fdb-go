// Package meta loads a RecordMetaData from one of the two supported
// sources declared in a Context's MetadataSource field:
//
//   - FileSource:     a serialized RecordMetaDataProto.MetaData on disk
//   - FDBStoreSource: an FDBMetaDataStore at a given keyspace path
//
// Both produce the same *recordlayer.RecordMetaData and callers never need
// to know which source produced it. See cmd/frl/docs/operator-guide.md
// for the app-side wiring (Go + Java) for each path.
package meta

import (
	"context"
	"errors"
	"fmt"
	"os"

	"google.golang.org/protobuf/proto"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// Source abstracts where a RecordMetaData comes from. Implementations are
// expected to be cheap to construct; Load is where the actual IO happens.
type Source interface {
	// Name is a short human-readable identifier used in errors and logs,
	// e.g. "file:/etc/myapp/meta.pb" or "fdb_store:/myapp/_meta".
	Name() string

	// Load resolves the source into a RecordMetaData. Errors wrap the
	// underlying cause (file IO, FDB transaction, proto unmarshal, or
	// metadata build) and are safe to surface directly to the operator.
	Load(ctx context.Context) (*recordlayer.RecordMetaData, error)
}

// ErrMissingSource is returned by FromContext when the context has no
// metadata source configured and one is required by the caller.
var ErrMissingSource = errors.New("context has no metadata source; add metadata.meta_file or metadata.meta_store_keyspace to the context")

// ErrFDBStoreNotAvailable is returned by FromContext when the context
// is wired for an FDBMetaDataStore source but the caller passed a nil
// db handle or keyspaceResolver — i.e. the command explicitly opted
// out of FDB-store support. Callers that want to surface a friendlier
// "this command doesn't support fdb_store sources" message detect
// this with errors.Is().
//
// The message starts with "this command" (rather than "fdb_store …")
// so fang's auto-capitalized error banner reads as a sentence —
// "fdb_store" gets up-cased to "Fdb_store" which looks like a typo.
var ErrFDBStoreNotAvailable = errors.New("this command does not support fdb_store metadata sources — configure `meta_file` in the context or pass --meta-file")

// FromContext builds a Source from a Context's metadata field. Returns
// ErrMissingSource if neither meta_file nor meta_store_keyspace is set
// (commands that can tolerate missing metadata — like `store info` —
// should check errors.Is(err, ErrMissingSource) and skip loading).
//
// keyspaceResolver is the hook used by FDBStoreSource to turn a string
// keyspace path into an FDB subspace. Callers inject this so the meta
// package doesn't duplicate parse-path logic living in internal/cmd.
func FromContext(
	ctx *configv1.Context,
	db *recordlayer.FDBDatabase,
	keyspaceResolver func(string) (subspace.Subspace, error),
) (Source, error) {
	ms := ctx.GetMetadata()
	if ms == nil {
		return nil, ErrMissingSource
	}
	switch s := ms.GetSource().(type) {
	case *configv1.MetadataSource_MetaFile:
		if s.MetaFile == "" {
			// Quote the YAML key so fang's banner capitalization doesn't
			// turn it into "Meta_file is empty …".
			return nil, fmt.Errorf("empty `meta_file` in context %q", ctx.GetName())
		}
		return &FileSource{Path: s.MetaFile}, nil
	case *configv1.MetadataSource_MetaStoreKeyspace:
		if s.MetaStoreKeyspace == "" {
			return nil, fmt.Errorf("empty `meta_store_keyspace` in context %q", ctx.GetName())
		}
		if db == nil || keyspaceResolver == nil {
			// Surface as a sentinel so command-level callers can wrap
			// with `(context %q)` the same way they do for ErrMissingSource.
			return nil, ErrFDBStoreNotAvailable
		}
		ss, err := keyspaceResolver(s.MetaStoreKeyspace)
		if err != nil {
			return nil, fmt.Errorf("resolve meta_store_keyspace %q: %w", s.MetaStoreKeyspace, err)
		}
		return &FDBStoreSource{
			DB:           db,
			Subspace:     ss,
			KeyspacePath: s.MetaStoreKeyspace,
		}, nil
	default:
		return nil, ErrMissingSource
	}
}

// FileSource reads a serialized com.apple.foundationdb.record.RecordMetaDataProto.MetaData
// message from disk. Typically produced by an app's build/deploy tooling
// using recordlayer.WriteRecordMetaData (or Java's meta.toProto().writeTo()).
type FileSource struct {
	Path string
}

func (s *FileSource) Name() string {
	return "file:" + s.Path
}

func (s *FileSource) Load(_ context.Context) (*recordlayer.RecordMetaData, error) {
	bytes, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Path, err)
	}
	return buildFromBytes(bytes, s.Name())
}

// FDBStoreSource reads the current RecordMetaData from an FDBMetaDataStore
// at the configured subspace. Loads are performed in a new read
// transaction for each call.
type FDBStoreSource struct {
	DB           *recordlayer.FDBDatabase
	Subspace     subspace.Subspace
	KeyspacePath string // retained for Name() / error context
}

func (s *FDBStoreSource) Name() string {
	return "fdb_store:" + s.KeyspacePath
}

func (s *FDBStoreSource) Load(ctx context.Context) (*recordlayer.RecordMetaData, error) {
	metaStore := recordlayer.NewFDBMetaDataStore(s.Subspace)
	result, err := s.DB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return metaStore.LoadRecordMetaDataProto(rtx.Transaction())
	})
	if err != nil {
		return nil, fmt.Errorf("read metadata from %s: %w", s.Name(), err)
	}
	mdProto, _ := result.(*gen.MetaData)
	if mdProto == nil {
		return nil, fmt.Errorf("no metadata stored at %s; app must call SaveRecordMetaData before frl can inspect this store", s.Name())
	}
	return buildFromProto(mdProto, s.Name())
}

// buildFromBytes unmarshals a RecordMetaDataProto.MetaData from raw bytes
// and constructs a RecordMetaData. All errors are wrapped with the source
// name so operators can tell which input was bad.
func buildFromBytes(data []byte, sourceName string) (*recordlayer.RecordMetaData, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty; expected a serialized RecordMetaDataProto.MetaData", sourceName)
	}
	mdProto := &gen.MetaData{}
	if err := proto.Unmarshal(data, mdProto); err != nil {
		return nil, fmt.Errorf("%s: unmarshal MetaData: %w", sourceName, err)
	}
	return buildFromProto(mdProto, sourceName)
}

// buildFromProto is the common tail for both file and FDB sources — it
// validates that the proto carries a non-empty record descriptor (the
// requirement we mandate for frl-readable metadata) and calls the
// record-layer's proto-to-object converter.
func buildFromProto(mdProto *gen.MetaData, sourceName string) (*recordlayer.RecordMetaData, error) {
	if mdProto.GetRecords() == nil {
		return nil, fmt.Errorf("%s: MetaData.records is empty. frl needs metadata with embedded proto descriptors — apps using programmatic metadata must call WriteRecordMetaData (Go) or meta.toProto().writeTo() (Java) with the full RecordMetaData, not a partial/incremental MetaData", sourceName)
	}
	meta, err := recordlayer.RecordMetaDataFromProto(mdProto)
	if err != nil {
		return nil, fmt.Errorf("%s: build RecordMetaData: %w", sourceName, err)
	}
	return meta, nil
}
