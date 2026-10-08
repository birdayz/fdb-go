// Command plan-trace plans one query without FDB and reports where the
// planner's work went: tasks, time and memo groups per phase, task kind and
// rule, then a census of the memo the run left, including groups explored
// more than once.
//
//	go run ./cmd/plan-trace -ddl 'CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))' -sql 'SELECT * FROM t WHERE a = 1'
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/core/embedded"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("plan-trace", flag.ContinueOnError)
	flags.SetOutput(out)
	ddl := flags.String("ddl", "", "schema DDL: CREATE TABLE and CREATE INDEX statements")
	sql := flags.String("sql", "", "query to plan")
	top := flags.Int("top", 25, "attribution rows to print, most expensive first")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *ddl == "" || *sql == "" {
		return errors.New("plan-trace: -ddl and -sql are required")
	}
	trace := cascades.NewPlannerTrace()
	started := time.Now()
	plan, planErr := embedded.PlanPhysicalForTestTraced(*sql, *ddl, nil, trace)
	elapsed := time.Since(started).Round(time.Microsecond)
	if planErr != nil {
		fmt.Fprintf(out, "planning failed after %s: %v\n", elapsed, planErr)
	} else {
		fmt.Fprintf(out, "plan (%s): %s\n", elapsed, plan.Explain())
	}
	if err := trace.WriteReport(out, *top); err != nil {
		return err
	}
	return planErr
}
