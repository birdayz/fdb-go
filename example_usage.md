# FoundationDB Record Layer Go - Usage Examples

## Basic Usage

### Setting up the Store

```go
package main

import (
    "context"
    "github.com/apple/foundationdb/bindings/go/src/fdb"
    "github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
    "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
    "github.com/yourapp/proto/gen" // Your generated protobuf code
)

func main() {
    // Initialize FDB
    fdb.MustAPIVersion(630)
    db := fdb.MustOpenDefault()
    recordDB := recordlayer.NewFDBDatabase(db)

    // Create metadata from your protobuf schema
    builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_your_schema_proto)
    builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
    metaData := builder.Build()

    // Use the store within a transaction
    _, err := recordDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
        // Create base store
        baseStore, err := recordlayer.NewStoreBuilder().
            SetContext(rtx).
            SetMetaDataProvider(metaData).
            SetSubspace(subspace.Sub("myapp", "orders")).
            CreateOrOpen()
        if err != nil {
            return nil, err
        }

        // Option 1: Use base store (works with any proto.Message)
        return useBaseStore(baseStore)

        // Option 2: Use typed store (type-safe for specific message type)
        // return useTypedStore(baseStore)
    })
}
```

### Option 1: Base Store (Flexible, Works with Multiple Types)

```go
func useBaseStore(store *recordlayer.FDBRecordStore) error {
    // Save any protobuf message
    order := &gen.Order{
        OrderId: proto.Int64(123),
        Price:   proto.Int32(100),
    }

    // Save record
    _, err := store.SaveRecord(order)
    if err != nil {
        return err
    }

    // Load record (requires type assertion)
    storedRecord, err := store.LoadRecord(tuple.Tuple{int64(123)})
    if err != nil {
        return err
    }

    if storedRecord != nil {
        // Need to type assert
        loadedOrder := storedRecord.ProtoMessage.(*gen.Order)
        fmt.Printf("Loaded order: %d, price: %d\n", 
            *loadedOrder.OrderId, *loadedOrder.Price)
    }

    return nil
}
```

### Option 2: Typed Store (Type-Safe, Compile-Time Checking)

```go
func useTypedStore(baseStore *recordlayer.FDBRecordStore) error {
    // Create typed store - much simpler than before!
    orderStore, err := recordlayer.GetTypedRecordStore[*gen.Order](baseStore, "Order")
    if err != nil {
        return err
    }

    // Save record (compile-time type checking)
    order := &gen.Order{
        OrderId: proto.Int64(123),
        Price:   proto.Int32(100),
    }

    _, err = orderStore.SaveRecord(order) // ✅ Only accepts *gen.Order
    if err != nil {
        return err
    }

    // Load record (no type assertion needed)
    storedRecord, err := orderStore.LoadRecord(tuple.Tuple{int64(123)})
    if err != nil {
        return err
    }

    if storedRecord != nil {
        // No type assertion needed!
        order := storedRecord.Record // Already *gen.Order
        fmt.Printf("Loaded order: %d, price: %d\n", 
            *order.OrderId, *order.Price)
    }

    return nil
}
```

## Key Features

### Java Compatibility ✅
- Identical wire format to Java Record Layer
- Can read/write data created by Java applications
- Supports both collision-prone and collision-safe primary keys

### Type Safety ✅ 
- Base store: Runtime type checking (like Java)
- Typed store: Compile-time type checking (better than Java!)

### Collision Handling ✅

```go
// Collision-prone keys (matches Java's field("order_id"))
builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))

// Collision-safe keys (matches Java's recordType().nest("order_id"))  
builder.GetRecordType("Order").SetPrimaryKey(
    recordlayer.RecordTypeKey().Nest(recordlayer.Field("order_id"))
)
```

## Migration from Java

| Java | Go |
|------|-----|
| `FDBRecordStore` | `FDBRecordStore` |
| `FDBRecordStore.getTypedRecordStore()` | `GetTypedRecordStore[T]()` |
| `saveRecord(Message)` | `SaveRecord(proto.Message)` |
| `loadRecord(Tuple)` | `LoadRecord(tuple.Tuple)` |
| `Key.Expressions.field("id")` | `Field("id")` |
| `Key.Expressions.recordType().nest("id")` | `RecordTypeKey().Nest(Field("id"))` |

The Go implementation provides the same functionality as Java with better type safety!