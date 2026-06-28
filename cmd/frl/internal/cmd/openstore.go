package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/cmd/frl/internal/config"
	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/pkg/recordlayer"
)

// resolveContextAndOverride is the shared prelude for record/index
// commands: load the config, pick the context (by --context or current),
// and build the meta-file override Source if --meta-file was supplied.
// Returns the context, an optional meta.Source override, or an error.
func resolveContextAndOverride(contextName, metaFile string) (*configv1.Context, meta.Source, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	cfgCtx, err := config.ResolveContext(cfg, contextName)
	if err != nil {
		if errors.Is(err, config.ErrNoContext) && metaFile == "" {
			path, _ := config.Path()
			return nil, nil, fmt.Errorf("%w (config: %s)", err, path)
		}
		if metaFile == "" {
			return nil, nil, err
		}
		cfgCtx = &configv1.Context{Name: "(cli-flag)"}
	}
	var override meta.Source
	if metaFile != "" {
		override = &meta.FileSource{Path: metaFile}
	}
	return cfgCtx, override, nil
}

// lookupRecordType resolves name against md, returning the RecordType on
// hit and a "not found — available: A, B, C" error on miss. Shared by
// meta types describe / record scan / record count so typos always
// produce the same user-facing message.
func lookupRecordType(md *recordlayer.RecordMetaData, name string) (*recordlayer.RecordType, error) {
	if rt := md.GetRecordType(name); rt != nil {
		return rt, nil
	}
	return nil, fmt.Errorf("record type %q not found — available: %s",
		name, strings.Join(sortedRecordTypeNames(md), ", "))
}

// validateRecordType is the error-only form of lookupRecordType — for
// callers (record scan / count) that only need the presence check, not
// the RecordType itself.
func validateRecordType(md *recordlayer.RecordMetaData, name string) error {
	_, err := lookupRecordType(md, name)
	return err
}

// lookupIndex resolves an index name against md, returning the Index
// on hit and a "not found — available: A, B, C" error on miss. Shared
// by index describe / index scan so typos always produce the same
// user-facing message.
func lookupIndex(md *recordlayer.RecordMetaData, name string) (*recordlayer.Index, error) {
	if idx := md.GetIndex(name); idx != nil {
		return idx, nil
	}
	return nil, fmt.Errorf("index %q not found — available: %s",
		name, strings.Join(sortedIndexNames(md), ", "))
}

// resolveMetaSourceFile returns `override` when non-nil, otherwise
// invokes meta.FromContext(cfgCtx, nil, nil) — i.e. the FDB-store-
// unsupported resolution used by every metadata-reading command in v1.
// Wraps the two well-known sentinels (ErrMissingSource,
// ErrFDBStoreNotAvailable) with the context name so operators can tell
// which context the message is about when they have several.
func resolveMetaSourceFile(cfgCtx *configv1.Context, override meta.Source) (meta.Source, error) {
	if override != nil {
		return override, nil
	}
	src, err := meta.FromContext(cfgCtx, nil, nil)
	if err != nil {
		if errors.Is(err, meta.ErrMissingSource) ||
			errors.Is(err, meta.ErrFDBStoreNotAvailable) {
			return nil, fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
		}
		return nil, err
	}
	return src, nil
}

// withStoreE is the ergonomic twin of withStore for commands whose store
// closure doesn't need a return value. Most `record scan` / `index ls` /
// `index scan` style commands stream output directly to the writer and
// would otherwise thread a sentinel struct{} through their withStore
// calls — that's the boilerplate this wrapper eliminates.
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
