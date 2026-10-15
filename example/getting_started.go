// Command example saves and loads a typed record in a disposable FoundationDB.
// It overwrites order 1001, so it requires FDB_CLUSTER_FILE to name a
// disposable cluster and refuses to fall back to FDB's default file. See
// README.md for setup with ./frl fdb up.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Getenv("FDB_CLUSTER_FILE"), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// errNoClusterFile is returned instead of falling back to FDB's default cluster
// file, which may point at a cluster whose data this example would overwrite.
var errNoClusterFile = errors.New("FDB_CLUSTER_FILE is not set; set it to a disposable cluster, for example the path printed by ./frl fdb up")

func run(ctx context.Context, clusterFile string, out io.Writer) error {
	if clusterFile == "" {
		return errNoClusterFile
	}
	if err := fdb.APIVersion(730); err != nil {
		return err
	}
	db, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	recordDB := recordlayer.NewFDBDatabase(db)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	metadata, err := builder.Build()
	if err != nil {
		return fmt.Errorf("build metadata: %w", err)
	}

	// The demo owns this subspace and overwrites order 1001 on each run.
	keyspace := subspace.FromBytes([]byte("record_layer_demo"))
	result, err := recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metadata).
			SetSubspace(keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		order := &demopb.Order{
			OrderId: proto.Int64(1001),
			Price:   proto.Int32(25),
			Flower:  &demopb.Flower{Type: proto.String("Rose"), Color: demopb.Color_RED.Enum()},
		}
		if _, err := store.SaveRecord(order); err != nil {
			return nil, err
		}

		typed, err := recordlayer.GetTypedRecordStore[*demopb.Order](store, "Order")
		if err != nil {
			return nil, err
		}
		loaded, err := typed.LoadRecord(tuple.Tuple{int64(1001)})
		if err != nil {
			return nil, err
		}
		if loaded == nil || !proto.Equal(loaded.Record, order) {
			return nil, fmt.Errorf("saved order did not round-trip")
		}
		return loaded.Record, nil
	})
	if err != nil {
		return fmt.Errorf("record transaction: %w", err)
	}
	order := result.(*demopb.Order)
	_, err = fmt.Fprintf(out, "order %d: price=%d\n", order.GetOrderId(), order.GetPrice())
	return err
}
