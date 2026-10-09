package testkit

import (
	"context"
	"database/sql"
	"runtime"
	"sync"
)

// Read is one read-only statement against one handle, for ReadAll.
type Read struct {
	DB  *sql.DB
	SQL string
}

// ReadResult is one Read's answer, as the scan function returned it.
type ReadResult struct {
	Rows []string
	Err  error
}

// ReadWorkers is how many reads ReadAll runs at once: GOMAXPROCS, since the
// cost of a sweep statement is planner CPU. Sharing the box with other tests
// is Bazel's job (the target's resources:cpu tag), not a cap here.
func ReadWorkers() int { return runtime.GOMAXPROCS(0) }

// ReadAll runs reads concurrently on up to ReadWorkers goroutines and returns
// the answers in the order the reads were given. A statement repeated in the
// list (same handle, same SQL) runs once and every occurrence gets its answer.
//
// It is for metamorphic sweeps whose statements are generated independently
// of each other's answers: the caller generates first (keeping its random draw
// sequence), reads everything here, then checks in its original order, so
// counts, samples and failure output are the serial ones. Only statements
// over a fixture that does not change between them may share one call.
func ReadAll(ctx context.Context, reads []Read, scan func(context.Context, *sql.DB, string) ([]string, error)) []ReadResult {
	type key struct {
		db  *sql.DB
		sql string
	}
	first := make(map[key]int, len(reads))
	var distinct []int
	for i, r := range reads {
		k := key{r.DB, r.SQL}
		if _, ok := first[k]; !ok {
			first[k] = i
			distinct = append(distinct, i)
		}
	}
	out := make([]ReadResult, len(reads))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(ReadWorkers(), max(len(distinct), 1)) {
		wg.Go(func() {
			for i := range work {
				rows, err := scan(ctx, reads[i].DB, reads[i].SQL)
				out[i] = ReadResult{rows, err}
			}
		})
	}
	for _, i := range distinct {
		work <- i
	}
	close(work)
	wg.Wait()
	for i, r := range reads {
		if j := first[key{r.DB, r.SQL}]; j != i {
			out[i] = out[j]
		}
	}
	return out
}
