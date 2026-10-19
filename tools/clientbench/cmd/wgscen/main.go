// Command wgscen runs the wire-golden scenarios (internal/wgscenarios) on the
// client compiled in: pure Go, or libfdb_c with the libfdbc tag.
package main

import (
	"flag"
	"fmt"
	"os"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/tools/clientbench/internal/wgscenarios"
)

func main() {
	cluster := flag.String("cluster-file", "", "cluster file")
	list := flag.Bool("list", false, "print the scenario names and exit")
	flag.Parse()
	if *list {
		for _, name := range wgscenarios.Names() {
			fmt.Println(name)
		}
		return
	}
	if err := fdb.APIVersion(730); err != nil {
		fail(err)
	}
	db, err := open(*cluster)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	if err := wgscenarios.Run(db); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
