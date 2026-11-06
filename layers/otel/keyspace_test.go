package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

// keyLayoutRows reads every key under ss.
func keyLayoutRows(t *testing.T, ctx context.Context, b *FDBBackend, ss subspace.Subspace) []fdb.KeyValue {
	t.Helper()
	r, err := fdb.PrefixRange(ss.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var rows []fdb.KeyValue
	if _, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		var err error
		rows, err = rc.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// writeOwnerManifest stores m as run's owner manifest, as the code that
// created the run would have.
func writeOwnerManifest(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, run string, m Manifest) {
	t.Helper()
	wire, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		rc.Transaction().Set(subspace.Sub(keyApplication, run).Sub("owner").FDBKey(), wire)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyLayoutsRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	exportAndQuery := func(t *testing.T, e *Exporter) {
		t.Helper()
		if _, err := e.ExportResourceSpans(ctx, deltaConversation(3, "array")); err != nil {
			t.Fatal(err)
		}
		hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 2", Mode: MessageMatchPhrase})
		if len(hits) != 2 {
			t.Fatalf("%d hits, want 2", len(hits))
		}
		if res, err := e.Query(ctx, Attribute{"span", "gen_ai.conversation.id", "string", "conv-283"}); err != nil || len(res.Hits) != 3 {
			t.Fatalf("equality query: %+v %v", res, err)
		}
	}

	t.Run("fresh_run_uses_directory_prefixes", func(t *testing.T) {
		e := openFTSExporter(t, ctx, cluster, nil)
		b := e.backend
		if b.cfg.KeyLayout != keyLayoutDirectory || len(b.keys.data.Bytes()) > 8 || !bytes.HasPrefix(b.store.Bytes(), b.keys.data.Bytes()) {
			t.Fatalf("layout %q, data prefix %x, store prefix %x", b.cfg.KeyLayout, b.keys.data.Bytes(), b.store.Bytes())
		}
		// The prefix is the key space path of the application and the run,
		// both resolved through the directory layer.
		resolver := recordlayer.NewKeySpaceResolver(b.db, recordlayer.ExtendedDirectoryLayerGlobal{})
		app, err := resolver.Resolve(ctx, keyApplication)
		if err != nil {
			t.Fatal(err)
		}
		run, err := resolver.Resolve(ctx, e.Config().Run)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b.keys.data.Bytes(), subspace.Sub(app, run).Bytes()) || !bytes.Equal(b.store.Bytes(), subspace.Sub(app, run, keyStore).Bytes()) {
			t.Fatalf("data prefix %x, want the key space path (%d, %d)", b.keys.data.Bytes(), app, run)
		}
		if name, ok, err := resolver.ReverseLookup(ctx, run); err != nil || !ok || name != e.Config().Run {
			t.Fatalf("reverse lookup %d: %q %v %v", run, name, ok, err)
		}
		exportAndQuery(t, e)
		// Only the owner manifest stays under the run's tuple prefix.
		legacy := keyLayoutRows(t, ctx, b, b.keys.legacy)
		var m Manifest
		if len(legacy) != 1 || !bytes.Equal(legacy[0].Key, b.keys.owner()) || json.Unmarshal(legacy[0].Value, &m) != nil || m.KeyLayout != keyLayoutDirectory {
			t.Fatalf("tuple prefix holds %d keys, manifest %+v", len(legacy), m)
		}
		if len(keyLayoutRows(t, ctx, b, b.keys.data)) == 0 {
			t.Fatal("no data under the directory prefix")
		}
		// A second backend of the run resolves the same prefixes.
		again, err := OpenFDBContext(ctx, b.cfg, cluster)
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		if !reflect.DeepEqual(again.keys, b.keys) {
			t.Fatalf("reopened keys %+v, want %+v", again.keys, b.keys)
		}
		// Drop clears both.
		if err := b.Drop(ctx); err != nil {
			t.Fatal(err)
		}
		if n, m := len(keyLayoutRows(t, ctx, b, b.keys.legacy)), len(keyLayoutRows(t, ctx, b, b.keys.data)); n != 0 || m != 0 {
			t.Fatalf("drop left %d tuple-prefix and %d data keys", n, m)
		}
	})

	t.Run("tuple_layout_run_keeps_its_keys", func(t *testing.T) {
		cfg := DefaultExporterConfig()
		cfg.ClusterFile, cfg.Run, cfg.Tenant, cfg.EnableMessageFullText = cluster, freshRun(t), 11, true
		c := cfg.backendConfig()
		c.Backend = "fdb"
		probe := openTestExporter(t, ctx, cluster, nil)
		writeOwnerManifest(t, ctx, probe.backend.db, cfg.Run, manifest(c, "ready"))
		e, err := NewExporter(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
		b := e.backend
		legacy := subspace.Sub(keyApplication, cfg.Run)
		if b.cfg.KeyLayout != keyLayoutTuple || !bytes.Equal(b.store.Bytes(), legacy.Sub("store").Bytes()) || !bytes.Equal(b.keys.data.Bytes(), legacy.Bytes()) {
			t.Fatalf("layout %q, store prefix %x", b.cfg.KeyLayout, b.store.Bytes())
		}
		exportAndQuery(t, e)
		if len(keyLayoutRows(t, ctx, b, legacy.Sub("store"))) == 0 {
			t.Fatal("no data under the tuple prefix")
		}
	})

	t.Run("unknown_layout_refused", func(t *testing.T) {
		cfg := DefaultExporterConfig()
		cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, freshRun(t), 11
		c := cfg.backendConfig()
		c.Backend, c.KeyLayout = "fdb", "directory_v9"
		probe := openTestExporter(t, ctx, cluster, nil)
		writeOwnerManifest(t, ctx, probe.backend.db, cfg.Run, manifest(c, "ready"))
		_, err := NewExporter(ctx, cfg)
		requireTyped[*OwnershipError](t, "unknown key layout", err)
		if !strings.Contains(err.Error(), "directory_v9") {
			t.Fatalf("refusal does not name the layout: %v", err)
		}
	})
}
