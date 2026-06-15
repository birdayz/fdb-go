package bench

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

var (
	goClient      gofdb.Database
	cgoClient     cgofdb.Database
	testContainer *tc.Container
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tc.Run(ctx, "", tc.WithStorageEngine("ssd"), tc.WithDirectIP(),
		testcontainers.WithHostConfigModifier(func(hc *dockercontainer.HostConfig) {
			hc.CapAdd = append(hc.CapAdd, "NET_ADMIN")
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start container: %v\n", err)
		os.Exit(1)
	}
	testContainer = container
	defer container.Terminate(context.Background())

	// Cluster file
	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster file: %v\n", err)
		os.Exit(1)
	}

	// Pure Go client — exact same hybrid cluster as fdb_test.go openTestDB
	cf, err := client.ParseClusterString(connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse cluster: %v\n", err)
		os.Exit(1)
	}

	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "read internal: %v\n", err)
		os.Exit(1)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := client.ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse internal: %v\n", err)
		os.Exit(1)
	}

	connectCF := &client.ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	gofdb.MustAPIVersion(730)
	goClient, err = gofdb.OpenDatabaseFromConfig(ctx, connectCF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open go db: %v\n", err)
		os.Exit(1)
	}
	defer goClient.Close()

	// CGo client
	tmpFile, _ := os.CreateTemp("", "bench_cluster_*.txt")
	tmpFile.WriteString(connStr)
	tmpFile.Close()
	cgofdb.MustAPIVersion(730)
	cgoClient = cgofdb.MustOpenDatabase(tmpFile.Name())

	// Seed data via CGo (guaranteed to work)
	_, err = cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Set(cgofdb.Key("bench_key_100b"), make([]byte, 100))
		tx.Set(cgofdb.Key("bench_key_1kb"), make([]byte, 1000))
		tx.Set(cgofdb.Key("bench_key_10kb"), make([]byte, 10000))
		for i := 0; i < 100; i++ {
			k := fmt.Sprintf("bench_range_%04d", i)
			v := make([]byte, 8)
			binary.LittleEndian.PutUint64(v, uint64(i))
			tx.Set(cgofdb.Key(k), v)
		}
		return nil, nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func BenchmarkGet(b *testing.B) {
	b.Run("Go/100B", func(b *testing.B) { benchGetGo(b, "bench_key_100b") })
	b.Run("CGo/100B", func(b *testing.B) { benchGetCGo(b, "bench_key_100b") })
	b.Run("Go/1KB", func(b *testing.B) { benchGetGo(b, "bench_key_1kb") })
	b.Run("CGo/1KB", func(b *testing.B) { benchGetCGo(b, "bench_key_1kb") })
	b.Run("Go/10KB", func(b *testing.B) { benchGetGo(b, "bench_key_10kb") })
	b.Run("CGo/10KB", func(b *testing.B) { benchGetCGo(b, "bench_key_10kb") })
}

func benchGetGo(b *testing.B, key string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			return tx.Get(gofdb.Key(key)).MustGet(), nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchGetCGo(b *testing.B, key string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.Get(cgofdb.Key(key)).MustGet(), nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSet(b *testing.B) {
	val := make([]byte, 100)
	b.Run("Go/100B", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				tx := txw.(gofdb.Transaction)
				tx.Set(gofdb.Key(fmt.Sprintf("bench_set_%d", i)), val)
				return nil, nil
			})
		}
	})
	b.Run("CGo/100B", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key(fmt.Sprintf("bench_set_%d", i)), val)
				return nil, nil
			})
		}
	})
}

func BenchmarkGetRange(b *testing.B) {
	for _, n := range []int{10, 100} {
		end := fmt.Sprintf("bench_range_%04d", n)
		b.Run(fmt.Sprintf("Go/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
					tx := txw.(gofdb.Transaction)
					rr := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key("bench_range_0000"), End: gofdb.Key(end)}, gofdb.RangeOptions{})
					return rr.GetSliceWithError()
				})
			}
		})
		b.Run(fmt.Sprintf("CGo/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
					rr := tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key("bench_range_0000"), End: cgofdb.Key(end)}, cgofdb.RangeOptions{})
					return rr.GetSliceWithError()
				})
			}
		})
	}
}

// BenchmarkBatchGet simulates the HNSW pattern: fire N Gets in parallel
// within one transaction, then resolve all futures.
func BenchmarkBatchGet(b *testing.B) {
	// Seed batch data
	goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
		tx := txw.(gofdb.Transaction)
		for i := 0; i < 50; i++ {
			tx.Set(gofdb.Key(fmt.Sprintf("bench_batch_%04d", i)), make([]byte, 200))
		}
		return nil, nil
	})

	for _, n := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("Go/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
					tx := txw.(gofdb.Transaction)
					futures := make([]gofdb.FutureByteSlice, n)
					for j := 0; j < n; j++ {
						futures[j] = tx.Get(gofdb.Key(fmt.Sprintf("bench_batch_%04d", j)))
					}
					for _, f := range futures {
						f.MustGet()
					}
					return nil, nil
				})
			}
		})
		b.Run(fmt.Sprintf("Go-serial/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
					tx := txw.(gofdb.Transaction)
					for j := 0; j < n; j++ {
						tx.Get(gofdb.Key(fmt.Sprintf("bench_batch_%04d", j))).MustGet()
					}
					return nil, nil
				})
			}
		})
		b.Run(fmt.Sprintf("CGo/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
					futures := make([]cgofdb.FutureByteSlice, n)
					for j := 0; j < n; j++ {
						futures[j] = tx.Get(cgofdb.Key(fmt.Sprintf("bench_batch_%04d", j)))
					}
					for _, f := range futures {
						f.MustGet()
					}
					return nil, nil
				})
			}
		})
	}
}

// BenchmarkPipelinedGet measures pipelining: fire all Gets first, then resolve.
// Same pattern as BenchmarkBatchGet but uses ReadTransact (no commit).
func BenchmarkPipelinedGet(b *testing.B) {
	for _, n := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("Go/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
					futures := make([]gofdb.FutureByteSlice, n)
					for j := 0; j < n; j++ {
						futures[j] = tx.Get(gofdb.Key(fmt.Sprintf("bench_batch_%04d", j)))
					}
					for _, f := range futures {
						f.MustGet()
					}
					return nil, nil
				})
			}
		})
		b.Run(fmt.Sprintf("CGo/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
					futures := make([]cgofdb.FutureByteSlice, n)
					for j := 0; j < n; j++ {
						futures[j] = tx.Get(cgofdb.Key(fmt.Sprintf("bench_batch_%04d", j)))
					}
					for _, f := range futures {
						f.MustGet()
					}
					return nil, nil
				})
			}
		})
	}
}

// BenchmarkReadTransact isolates read-path overhead (no commit).
func BenchmarkReadTransact(b *testing.B) {
	b.Run("Go/1", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
				return tx.Get(gofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
	b.Run("CGo/1", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
				return tx.Get(cgofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
}

func BenchmarkRYW(b *testing.B) {
	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				tx := txw.(gofdb.Transaction)
				tx.Set(gofdb.Key("bench_ryw"), []byte("v"))
				return tx.Get(gofdb.Key("bench_ryw")).MustGet(), nil
			})
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key("bench_ryw"), []byte("v"))
				return tx.Get(cgofdb.Key("bench_ryw")).MustGet(), nil
			})
		}
	})
}

// TestBenchmarkSanity verifies both clients return identical results for
// every operation used in the benchmarks. If this fails, the benchmarks
// are not comparing the same thing.
func TestBenchmarkSanity(t *testing.T) {
	t.Parallel()

	// Single Get: same key, same bytes.
	for _, key := range []string{"bench_key_100b", "bench_key_1kb", "bench_key_10kb"} {
		goResult, err := goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
			return tx.Get(gofdb.Key(key)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("go Get %s: %v", key, err)
		}
		cgoResult, err := cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
			return tx.Get(cgofdb.Key(key)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("cgo Get %s: %v", key, err)
		}
		goBytes := goResult.([]byte)
		cgoBytes := cgoResult.([]byte)
		if len(goBytes) != len(cgoBytes) {
			t.Errorf("Get %s: Go len=%d, CGo len=%d", key, len(goBytes), len(cgoBytes))
		} else if !bytes.Equal(goBytes, cgoBytes) {
			t.Errorf("Get %s: bytes differ", key)
		}
	}

	// GetRange: same range, same keys and values.
	for _, n := range []int{10, 100} {
		end := fmt.Sprintf("bench_range_%04d", n)
		goResult, _ := goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
			rr := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key("bench_range_0000"), End: gofdb.Key(end)}, gofdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		cgoResult, _ := cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
			rr := tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key("bench_range_0000"), End: cgofdb.Key(end)}, cgofdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		goKVs := goResult.([]gofdb.KeyValue)
		cgoKVs := cgoResult.([]cgofdb.KeyValue)
		if len(goKVs) != len(cgoKVs) {
			t.Fatalf("GetRange %d: Go %d keys, CGo %d keys", n, len(goKVs), len(cgoKVs))
		}
		for i := range goKVs {
			if !bytes.Equal(goKVs[i].Key, cgoKVs[i].Key) {
				t.Errorf("GetRange %d key[%d]: Go=%q CGo=%q", n, i, goKVs[i].Key, cgoKVs[i].Key)
			}
			if !bytes.Equal(goKVs[i].Value, cgoKVs[i].Value) {
				t.Errorf("GetRange %d val[%d]: Go=%q CGo=%q", n, i, goKVs[i].Value, cgoKVs[i].Value)
			}
		}
	}

	// RYW: Set + Get in same tx returns identical bytes.
	goRYW, _ := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
		tx := txw.(gofdb.Transaction)
		tx.Set(gofdb.Key("bench_sanity_ryw"), []byte("sanity"))
		return tx.Get(gofdb.Key("bench_sanity_ryw")).MustGet(), nil
	})
	cgoRYW, _ := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Set(cgofdb.Key("bench_sanity_ryw"), []byte("sanity"))
		return tx.Get(cgofdb.Key("bench_sanity_ryw")).MustGet(), nil
	})
	if !bytes.Equal(goRYW.([]byte), cgoRYW.([]byte)) {
		t.Errorf("RYW: Go=%q CGo=%q", goRYW, cgoRYW)
	}

	t.Log("sanity check passed: Go and CGo return identical bytes for all benchmark operations")
}

// BenchmarkThroughputRead measures sustained read throughput (MB/s).
// Reads 1KB values sequentially within a single transaction to measure
// raw data throughput, not per-transaction overhead.
func BenchmarkThroughputRead(b *testing.B) {
	const valueSize = 1024 // 1KB values
	const batchSize = 100  // keys per transaction

	// Seed with 1000 keys of 1KB each.
	for batch := 0; batch < 10; batch++ {
		goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			for i := 0; i < batchSize; i++ {
				key := fmt.Sprintf("bench_tp_%04d", batch*batchSize+i)
				tx.Set(gofdb.Key(key), make([]byte, valueSize))
			}
			return nil, nil
		})
	}

	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(batchSize * valueSize)) // report MB/s
		for i := 0; i < b.N; i++ {
			goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
				rr := tx.GetRange(
					gofdb.KeyRange{Begin: gofdb.Key("bench_tp_0000"), End: gofdb.Key("bench_tp_0100")},
					gofdb.RangeOptions{},
				)
				kvs, err := rr.GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(kvs) != batchSize {
					b.Fatalf("expected %d keys, got %d", batchSize, len(kvs))
				}
				return nil, nil
			})
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(batchSize * valueSize))
		for i := 0; i < b.N; i++ {
			cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
				rr := tx.GetRange(
					cgofdb.KeyRange{Begin: cgofdb.Key("bench_tp_0000"), End: cgofdb.Key("bench_tp_0100")},
					cgofdb.RangeOptions{},
				)
				kvs, err := rr.GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(kvs) != batchSize {
					b.Fatalf("expected %d keys, got %d", batchSize, len(kvs))
				}
				return nil, nil
			})
		}
	})
}

// BenchmarkThroughputWrite measures sustained write throughput (MB/s).
// Writes batches of 1KB values per transaction.
func BenchmarkThroughputWrite(b *testing.B) {
	const valueSize = 1024
	const batchSize = 10 // keys per transaction (within 10MB tx limit)

	val := make([]byte, valueSize)

	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(batchSize * valueSize))
		for i := 0; i < b.N; i++ {
			goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				tx := txw.(gofdb.Transaction)
				for j := 0; j < batchSize; j++ {
					key := fmt.Sprintf("bench_tpw_%d_%04d", i, j)
					tx.Set(gofdb.Key(key), val)
				}
				return nil, nil
			})
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(batchSize * valueSize))
		for i := 0; i < b.N; i++ {
			cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				for j := 0; j < batchSize; j++ {
					key := fmt.Sprintf("bench_tpw_%d_%04d", i, j)
					tx.Set(cgofdb.Key(key), val)
				}
				return nil, nil
			})
		}
	})
}

// execTC runs a tc command inside the container. Returns error if it fails.
func execTC(ctx context.Context, args ...string) error {
	exitCode, reader, err := testContainer.Exec(ctx, args, tcexec.Multiplexed())
	if err != nil {
		return fmt.Errorf("exec %v: %w", args, err)
	}
	if exitCode != 0 {
		out, _ := io.ReadAll(reader)
		return fmt.Errorf("tc exit %d: %s", exitCode, out)
	}
	return nil
}

// BenchmarkLatencyGet measures Go vs CGo with simulated 2ms RTT.
// Injects 1ms delay each way via tc netem inside the container.
func BenchmarkLatencyGet(b *testing.B) {
	ctx := context.Background()

	// Install iproute-tc (for tc). Rocky 9 splits tc into a separate package.
	exitCode, installOut, _ := testContainer.Exec(ctx, []string{
		"sh", "-c", "command -v tc > /dev/null 2>&1 || microdnf install -y iproute-tc > /dev/null 2>&1 || yum install -y iproute > /dev/null 2>&1",
	}, tcexec.Multiplexed())
	if exitCode != 0 {
		out, _ := io.ReadAll(installOut)
		b.Skipf("yum install iproute failed: exit=%d out=%s", exitCode, out)
	}

	// Add 1ms delay (1ms each way = 2ms RTT). Requires NET_ADMIN capability.
	tcBin := "/usr/sbin/tc" // Rocky 9 path; CentOS 7 uses /sbin/tc
	if err := execTC(ctx, tcBin, "qdisc", "add", "dev", "eth0", "root", "netem", "delay", "1ms"); err != nil {
		b.Skipf("tc netem failed (need NET_ADMIN): %v", err)
	}
	b.Cleanup(func() {
		execTC(context.Background(), tcBin, "qdisc", "del", "dev", "eth0", "root")
	})

	// Verify delay is active.
	_, verifyReader, _ := testContainer.Exec(ctx, []string{tcBin, "qdisc", "show", "dev", "eth0"}, tcexec.Multiplexed())
	out, _ := io.ReadAll(verifyReader)
	b.Logf("tc qdisc: %s", strings.TrimSpace(string(out)))

	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
				return tx.Get(gofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
				return tx.Get(cgofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
}

// BenchmarkLatencyGet500ms measures Go vs CGo with 1s RTT (500ms each way).
func BenchmarkLatencyGet500ms(b *testing.B) {
	ctx := context.Background()

	exitCode, installOut, _ := testContainer.Exec(ctx, []string{
		"sh", "-c", "command -v tc > /dev/null 2>&1 || microdnf install -y iproute-tc > /dev/null 2>&1 || yum install -y iproute > /dev/null 2>&1",
	}, tcexec.Multiplexed())
	if exitCode != 0 {
		out, _ := io.ReadAll(installOut)
		b.Skipf("install iproute-tc failed: exit=%d out=%s", exitCode, out)
	}

	tcBin := "/usr/sbin/tc"
	if err := execTC(ctx, tcBin, "qdisc", "add", "dev", "eth0", "root", "netem", "delay", "500ms"); err != nil {
		b.Skipf("tc netem failed: %v", err)
	}
	b.Cleanup(func() {
		execTC(context.Background(), tcBin, "qdisc", "del", "dev", "eth0", "root")
	})

	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
				return tx.Get(gofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
				return tx.Get(cgofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
}

// BenchmarkLatencyGet5ms measures Go vs CGo with 10ms RTT (5ms each way).
func BenchmarkLatencyGet5ms(b *testing.B) {
	ctx := context.Background()

	exitCode, installOut, _ := testContainer.Exec(ctx, []string{
		"sh", "-c", "command -v tc > /dev/null 2>&1 || microdnf install -y iproute-tc > /dev/null 2>&1 || yum install -y iproute > /dev/null 2>&1",
	}, tcexec.Multiplexed())
	if exitCode != 0 {
		out, _ := io.ReadAll(installOut)
		b.Skipf("install iproute-tc failed: exit=%d out=%s", exitCode, out)
	}

	tcBin := "/usr/sbin/tc"
	if err := execTC(ctx, tcBin, "qdisc", "add", "dev", "eth0", "root", "netem", "delay", "5ms"); err != nil {
		b.Skipf("tc netem failed: %v", err)
	}
	b.Cleanup(func() {
		execTC(context.Background(), tcBin, "qdisc", "del", "dev", "eth0", "root")
	})

	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			goClient.ReadTransact(func(tx gofdb.ReadTransaction) (any, error) {
				return tx.Get(gofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cgoClient.ReadTransact(func(tx cgofdb.ReadTransaction) (any, error) {
				return tx.Get(cgofdb.Key("bench_key_100b")).MustGet(), nil
			})
		}
	})
}
