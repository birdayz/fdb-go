package cmd

import (
	"context"
	"errors"
	"fmt"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// withStoreE is the ergonomic twin of withStore for commands whose store
// closure doesn't need a return value. Most `record scan` / `index ls` /
// `index scan` style commands stream output directly to the writer;
// threading a sentinel struct{} through their withStore calls was pure
// boilerplate.
func withStoreE(
	ctx context.Context,
	cfgCtx *configv1.Context,
	metaOverride meta.Source,
	fn func(store *recordlayer.FDBRecordStore) error,
) error {
	_, err := withStore(ctx, cfgCtx, metaOverride,
		func(s *recordlayer.FDBRecordStore) (struct{}, error) {
			return struct{}{}, fn(s)
		})
	return err
}

// withStore wires the plumbing that every data command needs: open the FDB
// connection from the context's cluster_file, resolve the metadata source,
// open the record store inside a read-write transaction, and hand the
// ready-to-use store to the caller. Returns the caller's result + error.
//
// metaOverride, if non-nil, replaces ctx.metadata for this call — used by
// the --meta-file flag to let operators run commands against an ad-hoc
// meta.pb without editing config.
func withStore[T any](
	ctx context.Context,
	cfgCtx *configv1.Context,
	metaOverride meta.Source,
	fn func(store *recordlayer.FDBRecordStore) (T, error),
) (T, error) {
	var zero T

	if cfgCtx.GetKeyspacePath() == "" {
		return zero, fmt.Errorf("context %q has empty keyspace_path", cfgCtx.GetName())
	}

	db, err := openDatabase(cfgCtx.GetClusterFile())
	if err != nil {
		return zero, err
	}
	rec := recordlayer.NewFDBDatabase(db)

	ss, err := parseKeyspacePath(cfgCtx.GetKeyspacePath())
	if err != nil {
		return zero, err
	}

	src := metaOverride
	if src == nil {
		src, err = meta.FromContext(cfgCtx, rec, parseKeyspacePath)
		if err != nil {
			if errors.Is(err, meta.ErrMissingSource) {
				return zero, fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
			}
			return zero, err
		}
	}
	md, err := src.Load(ctx)
	if err != nil {
		return zero, err
	}

	result, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			Open()
		if err != nil {
			return nil, fmt.Errorf("open store at %s: %w", cfgCtx.GetKeyspacePath(), err)
		}
		return fn(store)
	})
	if err != nil {
		return zero, err
	}
	v, _ := result.(T)
	return v, nil
}
