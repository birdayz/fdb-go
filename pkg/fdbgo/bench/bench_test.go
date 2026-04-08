// Package bench provides side-by-side benchmarks of the pure Go FDB client vs
// Apple CGo client. Both connect to the same FDB testcontainer.
//
// Run all:
//
//	bazelisk run //pkg/fdbgo/bench:bench_test -- \
//	  -test.run='^$' -test.bench=. -test.benchtime=5s -test.benchmem -test.count=3
//
// Run specific:
//
//	bazelisk run //pkg/fdbgo/bench:bench_test -- \
//	  -test.run='^$' -test.bench='BenchmarkGet/' -test.benchtime=5s -test.benchmem
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
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb/gofdbhelper"
)

var (
	goClient  gofdb.Database
	cgoClient cgofdb.Database
	container *tc.Container
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var err error
	container, err = tc.Run(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "start container: %v\n", err)
		os.Exit(1)
	}
	// Use same initialization as fdb_test.go (configure new single ssd)
	exitCode, _, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "configure: exit %d\n", exitCode)
		os.Exit(1)
	}
	// Wait for database to be available
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		code, reader, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "status minimal"})
		if code == 0 && reader != nil {
			out, _ := io.ReadAll(reader)
			if strings.Contains(string(out), "Healthy") {
				break
			}
		}
	}

	// Wait for FDB to be fully ready after initialization.
	time.Sleep(3 * time.Second)

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster file: %v\n", err)
		os.Exit(1)
	}
	// Pure Go client via gofdbhelper (handles hybrid cluster file)
	gofdb.MustAPIVersion(730)
	goClient, err = gofdbhelper.OpenDatabase(ctx, container)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open go db: %v\n", err)
		os.Exit(1)
	}

	// CGo client
	tmpFile, _ := os.CreateTemp("", "bench_cluster_*.txt")
	tmpFile.WriteString(clusterContent)
	tmpFile.Close()
	cgofdb.MustAPIVersion(730)
	cgoClient = cgofdb.MustOpenDatabase(tmpFile.Name())

	// Seed data
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

	code := m.Run()
	goClient.Close()
	container.Terminate(context.Background())
	os.Exit(code)
}

// ============================================================
// 1. Single Get (point read) — measures GRV + locate + read + parse
// ============================================================

func BenchmarkGet(b *testing.B) {
	b.Run("Go/100B", func(b *testing.B) {
		benchGetGo(b, "bench_key_100b")
	})
	b.Run("CGo/100B", func(b *testing.B) {
		benchGetCGo(b, "bench_key_100b")
	})
	b.Run("Go/1KB", func(b *testing.B) {
		benchGetGo(b, "bench_key_1kb")
	})
	b.Run("CGo/1KB", func(b *testing.B) {
		benchGetCGo(b, "bench_key_1kb")
	})
	b.Run("Go/10KB", func(b *testing.B) {
		benchGetGo(b, "bench_key_10kb")
	})
	b.Run("CGo/10KB", func(b *testing.B) {
		benchGetCGo(b, "bench_key_10kb")
	})
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

// ============================================================
// 2. Single Set + Commit — measures GRV + commit round-trip
// ============================================================

func BenchmarkSet(b *testing.B) {
	val := make([]byte, 100)
	b.Run("Go/100B", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
				tx.Set(gofdb.Key(fmt.Sprintf("bench_set_%d", i)), val)
				return nil, nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CGo/100B", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key(fmt.Sprintf("bench_set_%d", i)), val)
				return nil, nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ============================================================
// 3. GetRange — scan N keys
// ============================================================

func BenchmarkGetRange(b *testing.B) {
	for _, n := range []int{10, 100} {
		b.Run(fmt.Sprintf("Go/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
					rr := tx.GetRange(
						gofdb.KeyRange{Begin: gofdb.Key("bench_range_0000"), End: gofdb.Key(fmt.Sprintf("bench_range_%04d", n))},
						gofdb.RangeOptions{},
					)
					return rr.GetSliceWithError()
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("CGo/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
					rr := tx.GetRange(
						cgofdb.KeyRange{Begin: cgofdb.Key("bench_range_0000"), End: cgofdb.Key(fmt.Sprintf("bench_range_%04d", n))},
						cgofdb.RangeOptions{},
					)
					return rr.GetSliceWithError()
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ============================================================
// 4. Set + Get in same transaction (RYW)
// ============================================================

func BenchmarkRYW(b *testing.B) {
	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
				tx.Set(gofdb.Key("bench_ryw_key"), []byte("value"))
				return tx.Get(gofdb.Key("bench_ryw_key")).MustGet(), nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key("bench_ryw_key"), []byte("value"))
				return tx.Get(cgofdb.Key("bench_ryw_key")).MustGet(), nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ============================================================
// 5. Batch write — N sets per transaction
// ============================================================

func BenchmarkBatchWrite(b *testing.B) {
	val := make([]byte, 100)
	for _, n := range []int{10, 50} {
		b.Run(fmt.Sprintf("Go/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
					for j := 0; j < n; j++ {
						tx.Set(gofdb.Key(fmt.Sprintf("bench_batch_%d_%d", i, j)), val)
					}
					return nil, nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("CGo/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
					for j := 0; j < n; j++ {
						tx.Set(cgofdb.Key(fmt.Sprintf("bench_batch_%d_%d", i, j)), val)
					}
					return nil, nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ============================================================
// 6. Mixed workload — Set + Get + GetRange in one transaction
// ============================================================

func BenchmarkMixed(b *testing.B) {
	b.Run("Go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
				tx.Set(gofdb.Key(fmt.Sprintf("bench_mixed_%d", i)), []byte("v"))
				_, err := tx.Get(gofdb.Key("bench_key_100b")).Get()
				if err != nil {
					return nil, err
				}
				rr := tx.GetRange(
					gofdb.KeyRange{Begin: gofdb.Key("bench_range_0000"), End: gofdb.Key("bench_range_0010")},
					gofdb.RangeOptions{},
				)
				_, err = rr.GetSliceWithError()
				return nil, err
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CGo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tx.Set(cgofdb.Key(fmt.Sprintf("bench_mixed_%d", i)), []byte("v"))
				_, err := tx.Get(cgofdb.Key("bench_key_100b")).Get()
				if err != nil {
					return nil, err
				}
				rr := tx.GetRange(
					cgofdb.KeyRange{Begin: cgofdb.Key("bench_range_0000"), End: cgofdb.Key("bench_range_0010")},
					cgofdb.RangeOptions{},
				)
				_, err = rr.GetSliceWithError()
				return nil, err
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
