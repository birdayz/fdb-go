package foundationdb

import (
	"context"
	"fmt"
	"log"
)

// ExampleRun demonstrates how to start a FoundationDB container
func ExampleRun() {
	ctx := context.Background()

	// Start a FoundationDB container with default settings
	container, err := Run(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("Failed to terminate container: %v", err)
		}
	}()

	// Get connection information
	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("FoundationDB connection string: %s\n", connStr)
}

// ExampleRun_withCustomConfiguration demonstrates how to start a FoundationDB container with custom configuration
func ExampleRun_withCustomConfiguration() {
	ctx := context.Background()

	// Start FoundationDB container with custom settings
	container, err := Run(ctx, "",
		WithDatabase("my_test_db"),
		WithAPIVersion(720),
		WithVersion("7.1.61"),
		WithMemory("4GB"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("Failed to terminate container: %v", err)
		}
	}()

	// Get cluster file content for FDB client
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Database: %s\n", container.Database())
	fmt.Printf("API Version: %d\n", container.APIVersion())
	fmt.Printf("Version: %s\n", container.Version())
	fmt.Printf("Cluster file: %s\n", clusterFile)
}

// ExampleContainer_ConnectionString demonstrates how to get the connection string from a running container
func ExampleContainer_ConnectionString() {
	ctx := context.Background()

	container, err := Run(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("Failed to terminate container: %v", err)
		}
	}()

	// Get the connection string to connect to FoundationDB
	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Connect to FoundationDB at: %s\n", connStr)
}

// ExampleContainer_ClusterFile demonstrates how to get the cluster file content
func ExampleContainer_ClusterFile() {
	ctx := context.Background()

	container, err := Run(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("Failed to terminate container: %v", err)
		}
	}()

	// Get cluster file content for FDB client configuration
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Cluster file content: %s\n", clusterFile)

	// This cluster file content can be written to a file or used directly
	// with FDB client libraries that accept cluster file content
}

// ExampleWithVersion demonstrates how to run a specific FoundationDB version
func ExampleWithVersion() {
	ctx := context.Background()

	// Run FoundationDB 7.1.61 (older version for compatibility testing)
	container, err := Run(ctx, "",
		WithVersion("7.1.61"),
		WithAPIVersion(710),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("Failed to terminate container: %v", err)
		}
	}()

	fmt.Printf("Running FoundationDB version: %s\n", container.Version())
	fmt.Printf("Using API version: %d\n", container.APIVersion())
}
