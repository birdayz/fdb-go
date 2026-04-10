package bench

import (
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
)

var (
	goClient  gofdb.Database
	cgoClient cgofdb.Database
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tc.Run(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "start container: %v\n", err)
		os.Exit(1)
	}
	defer container.Terminate(context.Background())

	// Configure — exact same as fdb_test.go openTestDB
	exitCode, _, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "configure: exit %d\n", exitCode)
		os.Exit(1)
	}
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		code, reader, execErr := container.Exec(ctx, []string{"fdbcli", "--exec", "status minimal"})
		if execErr != nil || reader == nil {
			continue
		}
		if code == 0 {
			out, _ := io.ReadAll(reader)
			if strings.Contains(string(out), "Healthy") {
				break
			}
		}
	}

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
		_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
			goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
				goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
	goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		for i := 0; i < 50; i++ {
			tx.Set(gofdb.Key(fmt.Sprintf("bench_batch_%04d", i)), make([]byte, 200))
		}
		return nil, nil
	})

	for _, n := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("Go/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
				goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
			goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
