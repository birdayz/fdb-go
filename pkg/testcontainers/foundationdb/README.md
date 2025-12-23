# FoundationDB Testcontainer

Testcontainer module for FoundationDB. Spins up a single-node FDB instance for testing.

## Why This Exists

FoundationDB has a stupid requirement: client port and server port must match. Docker's random port mapping breaks this. This module uses socat to proxy the connection and make the ports match. It works.

## Installation

```bash
go get github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb
```

## Usage

```go
import (
    "context"
    foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

func TestSomething(t *testing.T) {
    ctx := context.Background()

    // Start container
    container, err := foundationdbtc.Run(ctx, "",
        foundationdbtc.WithAPIVersion(720),
    )
    if err != nil {
        t.Fatal(err)
    }
    defer container.Terminate(ctx)

    // Initialize database (required)
    err = container.InitializeDatabase(ctx)
    if err != nil {
        t.Fatal(err)
    }

    // Get FDB database connection
    db, err := container.GetFDBDatabase(ctx)
    if err != nil {
        t.Fatal(err)
    }

    // Use db for your tests
    _, err = db.Transact(func(tr fdb.Transaction) (interface{}, error) {
        tr.Set(fdb.Key("foo"), []byte("bar"))
        return nil, nil
    })
}
```

## API

### Run(ctx, image, opts...)

Starts a FoundationDB container. Pass empty string for image to use defaults.

Options:
- `WithAPIVersion(int)` - FDB API version (default: 720)
- `WithDatabase(string)` - Database name (default: "test")
- `WithVersion(string)` - FDB Docker tag (default: "7.3.46")
- `WithMemory(string)` - Memory limit (default: "4GB")

### Container.InitializeDatabase(ctx)

Runs `fdbcli configure new single memory`. Must be called after Run() before using the database.

### Container.GetFDBDatabase(ctx)

Returns `fdb.Database` ready to use. Caches the connection - calling multiple times returns the same instance.

### Container.ConnectionString(ctx)

Returns `host:port` connection string via socat proxy.

### Container.ClusterFile(ctx)

Returns cluster file content in format: `docker:docker@host:port`

### Container.Terminate(ctx)

Stops containers, removes network, cleans up temp files.

## How It Works

1. Creates dedicated Docker network
2. Starts socat container, gets random mapped port
3. Uses that port for both FDB server and socat listener
4. FDB client connects via socat proxy
5. Port matching requirement satisfied

The socat and FDB containers use custom entrypoints that wait for config injection via `CopyToContainer`. This ensures proper initialization order. Both entrypoints have 30-second timeouts to prevent hanging on configuration failures.

## Limitations

Single-node only. This is for testing, not production. If you need multi-node clusters for testing, you're doing something wrong.

The API version must match your FoundationDB server version. Don't mix 7.3.x with API 630. Read the docs.

FDB API version can only be set once per process. If you run multiple tests in parallel with different API versions, they'll fail. Use the same version everywhere or run tests sequentially.

## Requirements

- Docker
- FoundationDB Go bindings
- FoundationDB client libraries installed locally (libfdb_c.so)

## License

Same as parent project.
