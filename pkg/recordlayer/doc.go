// Package recordlayer stores protobuf records in FoundationDB with secondary
// indexes, schema evolution and resumable cursors. It is a port of Apple's Java
// FoundationDB Record Layer; Go and Java can share record stores in the formats
// that docs/compatibility.md lists.
//
// Metadata comes from a protobuf file whose union message lists the record
// types. A store is opened inside a transaction, where records are saved,
// loaded, scanned and queried:
//
//	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(shoppb.File_shop_proto)
//	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
//	builder.AddIndex("Order", recordlayer.NewIndex("price_idx", recordlayer.Field("price")))
//	metadata, err := builder.Build()
//	if err != nil {
//		return err
//	}
//
//	rdb := recordlayer.NewFDBDatabase(db)
//	_, err = rdb.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
//		store, err := recordlayer.NewStoreBuilder().
//			SetContext(rtx).
//			SetMetaDataProvider(metadata).
//			SetSubspace(subspace.FromBytes([]byte("app"))).
//			CreateOrOpen()
//		if err != nil {
//			return nil, err
//		}
//		return store.SaveRecord(&shoppb.Order{OrderId: proto.Int64(1), Price: proto.Int32(25)})
//	})
//
// Run retries retryable errors in a fresh transaction, so keep external side
// effects outside its callback. The runnable example is example/getting_started.go;
// docs/operations.md covers online index builds and schema evolution.
package recordlayer
