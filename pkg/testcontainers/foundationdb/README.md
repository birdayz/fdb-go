# FoundationDB Testcontainer

Testcontainer module for FoundationDB. Spins up a single-node FDB instance for testing with direct bridge IP connectivity — no port mapping, no socat proxy, no DNAT.

## Installation

```bash
go get github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb
```

## Quick Start

```go
import (
    "context"
    foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

func TestSomething(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
    defer cancel()

    // Start container — auto-initialized, ready to use.
    container, err := foundationdbtc.Run(ctx, "")
    if err != nil {
        t.Fatal(err)
    }
    defer container.Terminate(ctx)

    // Get cluster file path for your FDB client.
    path, _ := container.ClusterFilePath(ctx)
    db, _ := fdb.OpenDatabase(path)
}
```

## How It Works

1. Starts FDB container with Docker port mapping (4500/tcp exposed)
2. Waits for `FDBD joined cluster` log
3. Reads the cluster file from inside the container (via `Exec` with multiplexed stream demuxing)
4. Constructs external cluster file using `localhost:mappedPort`
5. Runs `fdbcli configure new single memory tenant_mode=optional_experimental`
6. Returns ready-to-use container

External clients (host, Bazel sandbox, CI) connect via `localhost:mappedPort`.
Internal clients (containers on same Docker network) connect via `containerIP:4500`.

## Options

```go
// FDB version (Docker image tag). Default: FDB_VERSION env or 7.3.77.
foundationdbtc.WithVersion("7.3.77")

// FDB API version (metadata for callers). Default: 730.
foundationdbtc.WithAPIVersion(730)

// Skip auto-initialization (call InitializeDatabase manually).
foundationdbtc.WithoutInit()

// Storage engine: "memory" (default), "ssd", "ssd-redwood-1", etc.
foundationdbtc.WithStorageEngine("memory")

// Redundancy: "single" (default), "double", "triple".
foundationdbtc.WithRedundancyMode("single")

// Tenant mode: "disabled", "optional_experimental" (default), "required".
foundationdbtc.WithTenantMode("optional_experimental")

// Custom FDB port (default: 4500).
foundationdbtc.WithFDBPort(4500)

// Attach to an existing Docker network (for multi-container setups).
foundationdbtc.WithNetwork(nw, "fdb-node-1")

// Startup timeout (default: 60s).
foundationdbtc.WithStartupTimeout(2 * time.Minute)

// Mix with standard testcontainers options:
testcontainers.WithEnv(map[string]string{"KEY": "VALUE"})
```

## Multi-Container Setup

For tests that need multiple containers on the same network (e.g., binding tester):

```go
// Create a shared network first.
nw, _ := foundationdbtc.CreateNetwork(ctx)
defer nw.Remove(ctx)

fdb, _ := foundationdbtc.Run(ctx, "", foundationdbtc.WithNetwork(nw))

// Attach another container to the same network.
testerReq := testcontainers.ContainerRequest{
    Networks: []string{fdb.NetworkName()},
}

// The other container can reach FDB via the internal cluster file.
clusterFile := fdb.InternalClusterFile()  // "docker:docker@172.17.0.2:4500"
```

## Chaos Testing

```go
// Pause FDB (simulates network partition).
err := container.Pause(ctx)

// ... verify your application handles the outage ...

// Resume FDB.
err = container.Unpause(ctx)
```

## Container Methods

| Method | Returns | Description |
|---|---|---|
| `ClusterFile(ctx)` | `(string, error)` | Cluster file content for external clients |
| `MustClusterFile(ctx)` | `string` | Panics on error |
| `ClusterFilePath(ctx)` | `(string, error)` | Writes cluster file to temp file |
| `ConnectionString(ctx)` | `(string, error)` | `host:port` string |
| `NetworkName()` | `string` | Docker network name |
| `InternalAddress()` | `string` | `containerIP:4500` (bridge IP) |
| `InternalClusterFile()` | `string` | Cluster file for containers on same network |
| `FDBCLIExec(ctx, cmd)` | `(string, error)` | Run fdbcli command |
| `Status(ctx)` | `(string, error)` | FDB status details |
| `Pause(ctx)` | `error` | Freeze container (Docker pause) |
| `Unpause(ctx)` | `error` | Resume container |
| `InitializeDatabase(ctx)` | `error` | Configure database (auto-called by Run) |
| `Terminate(ctx)` | `error` | Stop container, clean up network |

## Requirements

- Docker (Linux recommended — bridge IP connectivity required)
- No FDB client libraries needed on host (pure Go client or connects via cluster file)
