// unicode15store is a cross-process persisted-key compatibility probe.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/tools/unicode15store/harness"
)

func main() {
	cluster := flag.String("cluster-file", "", "FoundationDB cluster file")
	prefix := flag.String("prefix", "", "isolated store prefix (required)")
	operation := flag.String("operation", "inspect", "write, update, delete, rebuild, or inspect")
	flag.Parse()
	fmt.Fprintln(os.Stderr, "compiler="+runtime.Version())
	if *cluster == "" || *prefix == "" {
		fmt.Fprintln(os.Stderr, "-cluster-file and -prefix are required")
		os.Exit(2)
	}
	fdb.MustAPIVersion(730)
	db, err := fdb.OpenDatabase(*cluster)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := harness.Run(ctx, db, *prefix, *operation)
	if err != nil {
		fail(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
