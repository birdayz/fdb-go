# Java/Go Conformance Tests

This directory contains tests that verify the Go Record Layer implementation is compatible with the Java Record Layer implementation.

## Purpose

Every test in this directory asserts that Go and Java produce identical results when working with FoundationDB Record Layer. This ensures bidirectional compatibility - Java can read what Go writes, and vice versa.

## Structure

```
conformance/
├── README.md                     # This file
├── conformance_suite_test.go     # Ginkgo test suite entry point
├── crud_test.go                  # CRUD operations conformance
├── continuation_test.go          # Continuation token exchange
├── delete_test.go                # Delete operations conformance
├── helpers/
│   ├── java.go                   # Java test orchestration
│   ├── testdata.go               # Test data builders (Order protobuf)
│   └── container.go              # TestEnvironment setup/teardown
└── java/
    ├── build.gradle              # Gradle build configuration
    └── src/main/java/com/birdayz/conformance/
        ├── CrudConformanceTest.java        # Java CRUD companion
        ├── ContinuationConformanceTest.java # Java continuation companion
        └── DeleteConformanceTest.java      # Java delete companion
```

## Test Pattern

Every conformance test follows this pattern:

1. **Go orchestrates** - Ginkgo test controls the flow
2. **Both languages participate** - Go and Java both perform operations
3. **Results must match** - Assertions verify identical behavior

Example:
```go
var _ = Describe("CRUD Conformance", func() {
    var env *helpers.TestEnvironment
    var java *helpers.JavaTestRunner

    BeforeEach(func() {
        env, _ = helpers.SetupTestEnvironment(ctx, "test_name")
        java = helpers.NewJavaTestRunner()
    })

    It("should match Java behavior", func() {
        // 1. Go writes record
        order := helpers.StandardOrder(1001)
        writeRecordWithGo(ctx, env, order)

        // 2. Java reads and validates
        output, _ := java.RunCrud(ctx, "read", "1001")
        Expect(output).To(ContainSubstring("price=10010"))

        // 3. Go re-reads to verify round-trip
        readBack, _ := readRecordWithGo(ctx, env, 1001)
        Expect(*readBack.Price).To(Equal(int32(10010)))
    })
})
```

## Java Companion Classes

Each conformance area has a dedicated Java class:

- **CrudConformanceTest** - Basic read/write operations
  - `./gradlew runCrud --args="read 1001"`
  - `./gradlew runCrud --args="write 2002"`

- **ContinuationConformanceTest** - Cursor continuation tokens
  - `./gradlew runConformance --args="<hex-subspace> <hex-continuation> <limit> <start> <end>"`

- **DeleteConformanceTest** - Delete operations
  - `./gradlew runDelete --args="delete 1001"`
  - `./gradlew runDelete --args="verify-deleted 1001"`

## Adding a New Conformance Test

### 1. Create Go Test File

```go
// conformance/yourfeature_test.go
package conformance_test

import (
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
    "github.com/birdayz/fdb-record-layer-go/conformance/helpers"
)

var _ = Describe("YourFeature Conformance", func() {
    var env *helpers.TestEnvironment
    var java *helpers.JavaTestRunner

    BeforeEach(func() {
        env, _ = helpers.SetupTestEnvironment(ctx, "yourfeature_conformance")
        java = helpers.NewJavaTestRunner()
    })

    It("should match Java behavior", func() {
        // Test implementation
    })
})
```

### 2. Create Java Companion Class

```java
// conformance/java/src/main/java/com/birdayz/conformance/YourFeatureConformanceTest.java
package com.birdayz.conformance;

public class YourFeatureConformanceTest {
    public static void main(String[] args) {
        // Parse args, perform operation, print results
    }
}
```

### 3. Add Gradle Task

```gradle
// conformance/java/build.gradle
task runYourFeature(type: JavaExec) {
    classpath = sourceSets.main.runtimeClasspath
    mainClass = 'com.birdayz.conformance.YourFeatureConformanceTest'
    if (project.hasProperty('args')) {
        args = project.property('args').split(' ')
    }
}
```

### 4. Update Java Helper (if needed)

```go
// conformance/helpers/java.go
func (j *JavaTestRunner) RunYourFeature(ctx context.Context, args ...string) (result, error) {
    return j.runGradle(ctx, "runYourFeature", "--args="+strings.Join(args, " "))
}
```

## Running Tests

```bash
# All conformance tests
ginkgo ./conformance

# Specific test suite
ginkgo --focus="CRUD" ./conformance

# Verbose output
ginkgo -v ./conformance

# Run with race detector
ginkgo --race ./conformance
```

## Requirements

- Go 1.21+
- Java 11+
- FoundationDB client libraries installed
- Docker (for testcontainers)
- Ginkgo CLI: `go install github.com/onsi/ginkgo/v2/ginkgo@latest`

## What's NOT a Conformance Test

These tests belong in other directories:

- **Internal Go tests** → `pkg/recordlayer/*_test.go`
  - Transaction isolation behavior
  - Conflict detection
  - Record existence checks
  - Any test that doesn't need Java validation

- **Testcontainer tests** → `pkg/testcontainers/foundationdb/*_test.go`
  - Container startup/shutdown
  - Cluster file generation
  - Network configuration

## Output Format

Java classes output parseable key-value pairs:

```
KEY: value
ANOTHER_KEY: another_value
```

The Go helper `ParseJavaOutput()` parses this into `map[string]string`.

## Common Pitfalls

1. **Subspace mismatch** - Ensure Java and Go use identical subspace names
2. **Cluster file format** - Must be `docker:docker@host:port`
3. **Protobuf schema** - Both must use identical proto definitions
4. **Key construction** - Primary key encoding must match exactly
5. **Continuation tokens** - Must be exchangeable as opaque byte arrays

## Testing Strategy

1. **Bidirectional** - Every operation tested in both directions (Go→Java, Java→Go)
2. **Round-trip** - Data written by one can be read by the other
3. **Edge cases** - Empty values, large values, special characters
4. **Parameterized** - Use Ginkgo's DescribeTable for variations

## Success Criteria

A conformance test passes when:
- Go and Java produce byte-identical records in FDB
- Continuation tokens are exchangeable
- Both languages agree on record existence/deletion
- No data loss or corruption in round-trip operations
