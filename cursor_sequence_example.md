# RecordCursor with Go 1.23+ Sequence Interfaces

The FDB Record Layer Go port now supports Go 1.23+ sequence interfaces (`iter.Seq` and `iter.Seq2`), making iteration more idiomatic and interoperable with Go's standard library.

## Basic Usage

### Traditional OnNext() Pattern
```go
cursor := store.ScanRecords(nil, recordlayer.ForwardScan)
defer cursor.Close()

for {
    result, err := cursor.OnNext(ctx)
    if err != nil {
        return err
    }
    if !result.HasNext() {
        break
    }
    
    record := result.GetValue()
    // Process record...
}
```

### Modern Sequence Interface (Go 1.23+)
```go
cursor := store.ScanRecords(nil, recordlayer.ForwardScan)

// Simple iteration - errors are hidden
for record := range cursor.Seq(ctx) {
    order := record.Record.(*gen.Order)
    fmt.Printf("Order ID: %d, Price: %d\n", *order.OrderId, *order.Price)
}

// Error-aware iteration
for record, err := range cursor.Seq2(ctx) {
    if err != nil {
        log.Printf("Scan error: %v", err)
        continue
    }
    // Process record...
}

// Pagination-aware iteration
for record, continuation := range cursor.SeqWithContinuation(ctx) {
    // Process record...
    // Save continuation for resuming later
    if shouldPaginate {
        saveForLater(continuation.ToBytes())
        break
    }
}
```

## Functional Operations

### Filtering and Mapping
```go
// Using cursor transformations for complex operations
filterCursor := recordlayer.NewFilterCursor(
    store.ScanRecords(nil, recordlayer.ForwardScan),
    func(record *recordlayer.FDBStoredRecord[proto.Message]) bool {
        order := record.Record.(*gen.Order)
        return *order.Price > 50 // Only expensive orders
    },
)

mapCursor := recordlayer.NewMapCursor(
    filterCursor,
    func(record *recordlayer.FDBStoredRecord[proto.Message]) (int64, error) {
        order := record.Record.(*gen.Order)
        return *order.OrderId, nil // Extract just the ID
    },
)

// Use slices.Collect (Go 1.23+) to collect results
expensiveOrderIDs := slices.Collect(mapCursor.Seq(ctx))
fmt.Printf("Expensive order IDs: %v\n", expensiveOrderIDs)
```

### Limiting Results
```go
cursor := store.ScanRecords(nil, recordlayer.ForwardScan)

// Get first 10 records using manual break
var firstTen []*recordlayer.FDBStoredRecord[proto.Message]
for record := range cursor.Seq(ctx) {
    firstTen = append(firstTen, record)
    if len(firstTen) >= 10 {
        break
    }
}
```

### Counting and First Element
```go
cursor := store.ScanRecords(nil, recordlayer.ForwardScan)

// Count all records using range loop
totalCount := 0
for range cursor.Seq(ctx) {
    totalCount++
}

cursor2 := store.ScanRecords(nil, recordlayer.ForwardScan)

// Get first record using range with break
var firstRecord *recordlayer.FDBStoredRecord[proto.Message]
for record := range cursor2.Seq(ctx) {
    firstRecord = record
    break
}
if firstRecord != nil {
    order := firstRecord.Record.(*gen.Order)
    fmt.Printf("First order: %d\n", *order.OrderId)
}
```

## Interoperability with Standard Library

Since `RecordCursor` implements `iter.Seq` and `iter.Seq2`, it works seamlessly with any Go standard library functions that accept these interfaces:

```go
import (
    "slices"
)

cursor := store.ScanRecords(nil, recordlayer.ForwardScan)

// Use with slices.Collect (Go 1.23+)
allRecords := slices.Collect(cursor.Seq(ctx))

// For more complex transformations, use cursor transformations
mapCursor := recordlayer.NewMapCursor(
    store.ScanRecords(nil, recordlayer.ForwardScan),
    func(record *recordlayer.FDBStoredRecord[proto.Message]) (string, error) {
        order := record.Record.(*gen.Order)
        return fmt.Sprintf("Order-%d", *order.OrderId), nil
    },
)

results := slices.Collect(mapCursor.Seq(ctx))layer.FDBStoredRecord[proto.Message]) string {
            order := record.Record.(*gen.Order)
            return fmt.Sprintf("Order-%d", *order.OrderId)
        },
    ),
)
```

## Benefits of This Approach

### Why Sequence Functions Instead of Cursor Types?

1. **Universal**: These functions work with ANY `iter.Seq`, not just cursors:
   ```go
   // Works with cursors
   filtered := Filter(cursor.Seq(ctx), predicate)
   
   // Also works with slices
   filtered := Filter(slices.Values(mySlice), predicate)
   
   // Works with maps, channels, or any iter.Seq source!
   ```

2. **Composable**: Chain operations naturally:
   ```go
   result := slices.Collect(
       Limit(
           Map(
               Filter(cursor.Seq(ctx), isExpensive),
               extractOrderID,
           ),
           10,
       ),
   )
   ```

3. **Standard Library Integration**: Use Go 1.23+ built-ins:
   - `slices.Collect()` to gather results
   - `slices.Values()` to create sequences
   - Any future stdlib sequence utilities

4. **Simple Implementation**: Each function is ~10 lines of clear code

5. **Type Safe**: Full generic support with compile-time checking

6. **Memory Efficient**: Lazy evaluation, no intermediate collections

7. **Idiomatic Go**: Follows Go's philosophy of simple, composable functions

### When to Use What?

- **`cursor.Seq(ctx)`** - Simple iteration, errors hidden
- **`cursor.Seq2(ctx)`** - When you need error handling
- **`cursor.SeqWithContinuation(ctx)`** - For pagination
- **`cursor.OnNext(ctx)`** - Maximum control, Java compatibility