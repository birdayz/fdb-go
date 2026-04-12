package foundationdb

import (
	"context"
	"fmt"
	"log"
)

func ExampleRun() {
	ctx := context.Background()

	// Start a FoundationDB container — auto-initialized, ready to use.
	container, err := Run(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer container.Terminate(ctx)

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Cluster file: %s\n", clusterFile)
}

func ExampleRun_withCustomConfiguration() {
	ctx := context.Background()

	container, err := Run(ctx, "",
		WithDatabase("my_test_db"),
		WithAPIVersion(720),
		WithVersion("7.1.61"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer container.Terminate(ctx)

	fmt.Printf("Database: %s\n", container.Database())
	fmt.Printf("API Version: %d\n", container.APIVersion())
	fmt.Printf("Version: %s\n", container.Version())
}

func ExampleContainer_FDBCLIExec() {
	ctx := context.Background()

	container, err := Run(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer container.Terminate(ctx)

	output, err := container.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("FDB status: %s\n", output)
}

func ExampleWithNetwork() {
	ctx := context.Background()

	// Create a shared network for multi-container setups.
	nw, err := CreateNetwork(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer nw.Remove(ctx)

	container, err := Run(ctx, "",
		WithNetwork(nw, "fdb-primary"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer container.Terminate(ctx)

	fmt.Printf("Network: %s\n", container.NetworkName())
	fmt.Printf("Internal address: %s\n", container.InternalAddress())
}
