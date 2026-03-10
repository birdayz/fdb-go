# RFC 001: Conformance Test Restructure — Kill Gradle, Go Full Bazel

## Status: Draft

## Problem

The conformance test infrastructure has three layers of indirection that don't need to exist:

1. **Gradle build** (`conformance/java/build.gradle`, `gradlew`, wrapper jars) — Bazel already compiles Java via `rules_java` + `rules_jvm_external`. Gradle is dead weight. The CI previously needed `setup-java` + `./gradlew build` as a separate step. Bazel handles the JDK toolchain itself.

2. **`conformance/helpers/` package** (19 files, ~3,800 lines) — a separate Go package containing store wrappers, the Java invoker, container setup, and test data builders. This exists solely because Go requires a separate package for shared test code when tests are in a different package. If tests and helpers are in the same package, no separate package is needed.

3. **Separated Java/Go** — Java source is buried in `conformance/java/src/main/java/com/birdayz/conformance/`, Go tests are in `conformance/`, helpers in `conformance/helpers/`. Three directories for what is conceptually one test suite. You can't look at `index_conformance_test.go` and immediately find the Java steps it calls.

## Proposed Architecture

### Directory Layout: Before vs After

**Before** (3 directories, 2 build systems):
```
conformance/
  BUILD.bazel                          # go_test (29 test srcs)
  conformance_suite_test.go
  crud_test.go
  index_conformance_test.go
  count_index_conformance_test.go
  ...                                  # 29 Go test files total
  proto/
    record_layer_demo.proto            # Duplicate of java/src/main/proto/
  helpers/
    BUILD.bazel                        # go_library
    javainvoker.go                     # 252 lines
    container.go                       # 155 lines
    testdata.go                        # 87 lines
    conformance_store.go               # 16 store wrappers
    index_conformance_store.go
    ...                                # 19 files, ~3,800 lines
  java/
    BUILD.bazel                        # proto_library, java_proto_library, java_binary
    build.gradle                       # DEAD WEIGHT
    gradlew                            # DEAD WEIGHT
    gradlew.bat                        # DEAD WEIGHT
    gradle/                            # DEAD WEIGHT (wrapper jars)
    src/main/proto/
      record_layer_demo.proto          # Duplicate
    src/main/java/com/birdayz/conformance/
      ConformanceServer.java           # 229 lines — HTTP server
      ConformanceStep.java             # 28 lines — annotation
      ConformanceSteps.java            # 1,807 lines — 69 step methods, monolith
```

**After** (1 directory, 1 build system):
```
conformance/
  BUILD.bazel                          # Everything: proto, java, go_test

  # Proto (single source of truth)
  record_layer_demo.proto

  # Java: server infra
  ConformanceServer.java               # HTTP server + multi-class step dispatch
  ConformanceStep.java                 # @ConformanceStep annotation

  # Java: per-feature step files (paired with Go test files)
  CrudSteps.java                       # ← paired with crud_test.go
  IndexSteps.java                      # ← paired with index_conformance_test.go
  CountIndexSteps.java                 # ← paired with count_index_conformance_test.go
  ScanSteps.java                       # ← paired with scan_conformance_test.go
  SplitSteps.java                      # ← paired with split_conformance_test.go
  VersionSteps.java                    # ← paired with version_conformance_test.go
  CustomerSteps.java                   # ← paired with customer_conformance_test.go
  ContinuationSteps.java              # ← paired with continuation_conformance_test.go
  CountNotNullIndexSteps.java          # ← paired with count_not_null_index_conformance_test.go
  CountUpdatesIndexSteps.java          # ← paired with count_updates_index_conformance_test.go
  CompositeIndexSteps.java             # ← paired with composite_index_conformance_test.go
  FanoutIndexSteps.java                # ← paired with fanout_index_conformance_test.go
  MinMaxEverIndexSteps.java            # ← paired with min_max_ever_index_conformance_test.go
  MinMaxEverTupleIndexSteps.java       # ← paired with min_max_ever_tuple_index_conformance_test.go
  SumIndexSteps.java                   # ← paired with sum_index_conformance_test.go
  ClearWhenZeroSteps.java              # ← paired with clear_when_zero_conformance_test.go
  IndexStateSteps.java                 # ← paired with index_state_conformance_test.go
  RebuildIndexSteps.java               # ← paired with rebuild_index_conformance_test.go
  RangeSetSteps.java                   # ← paired with rangeset_conformance_test.go
  StoreHeaderSteps.java                # ← paired with store_header_conformance_test.go
  DeleteAllSteps.java                  # ← paired with delete_all_conformance_test.go

  # Go: test infrastructure (all *_test.go — same package, no helpers/ import)
  conformance_suite_test.go            # Ginkgo suite: container, java server lifecycle
  java_invoker_test.go                 # HTTP client to Java server (was helpers/javainvoker.go)
  container_test.go                    # TestEnvironment, TenantEnvironment (was helpers/container.go)
  testdata_test.go                     # Order/Customer builders (was helpers/testdata.go)
  store_helpers_test.go                # Common store wrapper base (was helpers/conformance_store.go)

  # Go: per-feature test + store wrapper (merged from helpers/*_store.go + test file)
  crud_test.go
  index_conformance_test.go            # Test + inlined index store wrapper
  count_index_conformance_test.go
  scan_conformance_test.go
  ...                                  # Same 29 test files, store wrappers inlined
```

### Naming Convention

Java files use PascalCase (Java convention), Go files use snake_case (Go convention). The pairing is by concept prefix:

| Go test file | Java steps file | Feature |
|---|---|---|
| `crud_test.go` | `CrudSteps.java` | Basic CRUD |
| `index_conformance_test.go` | `IndexSteps.java` | VALUE indexes |
| `count_index_conformance_test.go` | `CountIndexSteps.java` | COUNT indexes |
| ... | ... | ... |

Alphabetical `ls` interleaves them naturally (C before c, I before i).

### BUILD.bazel

Single file. Everything in one place.

```python
load("@protobuf//bazel:java_proto_library.bzl", "java_proto_library")
load("@rules_go//go:def.bzl", "go_test")
load("@rules_java//java:defs.bzl", "java_binary", "java_library")
load("@rules_proto//proto:defs.bzl", "proto_library")

proto_library(
    name = "demo_proto",
    srcs = ["record_layer_demo.proto"],
    strip_import_prefix = "/conformance",
    deps = ["//proto/apple:apple_proto"],
)

java_proto_library(
    name = "demo_java_proto",
    deps = [":demo_proto"],
)

java_library(
    name = "conformance_lib",
    srcs = glob(["*.java"]),
    deps = [
        ":demo_java_proto",
        "@maven//:com_google_code_gson_gson",
        "@maven//:com_google_protobuf_protobuf_java",
        "@maven//:com_google_protobuf_protobuf_java_util",
        "@maven//:org_foundationdb_fdb_extensions",
        "@maven//:org_foundationdb_fdb_java",
        "@maven//:org_foundationdb_fdb_record_layer_core",
    ],
)

java_binary(
    name = "conformance_server",
    main_class = "com.birdayz.conformance.ConformanceServer",
    runtime_deps = [":conformance_lib"],
)

go_test(
    name = "conformance_test",
    srcs = glob(["*_test.go"]),
    data = [":conformance_server"],
    deps = [
        "//gen",
        "//pkg/recordlayer",
        "//pkg/testcontainers/foundationdb",
        "@com_github_apple_foundationdb_bindings_go//src/fdb",
        "@com_github_apple_foundationdb_bindings_go//src/fdb/subspace",
        "@com_github_apple_foundationdb_bindings_go//src/fdb/tuple",
        "@com_github_google_uuid//:uuid",
        "@com_github_onsi_ginkgo_v2//:ginkgo",
        "@com_github_onsi_gomega//:gomega",
        "@org_golang_google_protobuf//proto",
    ],
)
```

Key point: `go_test` depends on `:conformance_server` (java_binary) via `data`. Bazel builds Java first, then runs Go tests. Zero external tooling.

## Java Changes

### Split ConformanceSteps.java (1,807 lines) Into Per-Feature Files

The monolithic `ConformanceSteps.java` gets split along the same feature boundaries as the Go test files. Each file is a plain class with `@ConformanceStep`-annotated static methods. Shared infrastructure (database caching, `runInContext`, tenant handling) stays in a base class or utility class.

**ConformanceBase.java** (~200 lines) — extracted from ConformanceSteps.java:
```java
package com.birdayz.conformance;

public class ConformanceBase {
    // Shared state
    static final Map<String, FDBDatabase> cachedDatabases = new ConcurrentHashMap<>();

    // Common methods used by all step classes
    static FDBDatabase createDatabase(String clusterFileContent) { ... }
    static <T> T runInContext(String clusterFile, String tenantName, Function<...> action) { ... }
    static FDBRecordContext createContextFromTransaction(...) { ... }
    static RecordMetaData createMetaData() { ... }
    // ... other shared builders (createIndexMetaData, etc.)
}
```

**Per-feature step file** (example — IndexSteps.java, ~120 lines):
```java
package com.birdayz.conformance;

public class IndexSteps extends ConformanceBase {
    @ConformanceStep("saveOrderWithIndex")
    public static Object saveOrderWithIndex(String clusterFile, int[] subspace, ...) { ... }

    @ConformanceStep("scanIndex")
    public static Object scanIndex(String clusterFile, int[] subspace, ...) { ... }

    @ConformanceStep("deleteOrderWithIndex")
    public static Object deleteOrderWithIndex(String clusterFile, int[] subspace, ...) { ... }
}
```

### Update ConformanceServer Step Dispatch

Current: scans only `ConformanceSteps.class` for annotated methods.

New: scan all classes that extend `ConformanceBase` (or maintain an explicit registry):

```java
// Option A: explicit list (simple, no classpath scanning)
private static final Class<?>[] STEP_CLASSES = {
    CrudSteps.class,
    IndexSteps.class,
    CountIndexSteps.class,
    ScanSteps.class,
    // ...
};

private Object invokeStep(String stepName, JsonObject params) {
    for (Class<?> cls : STEP_CLASSES) {
        for (Method m : cls.getDeclaredMethods()) {
            ConformanceStep ann = m.getAnnotation(ConformanceStep.class);
            if (ann != null && ann.value().equals(stepName)) {
                return invoke(m, params);
            }
        }
    }
    throw new IllegalArgumentException("Unknown step: " + stepName);
}
```

Option B (classpath scanning via reflection, heavier) is unnecessary — the list of step classes is known at compile time and changes rarely.

### Proto Consolidation

Remove `conformance/java/src/main/proto/record_layer_demo.proto` (duplicate). Keep `conformance/proto/record_layer_demo.proto` and move it to `conformance/record_layer_demo.proto`. The `proto_library` references it directly.

The `strip_import_prefix` in `proto_library` must be adjusted so Java codegen produces the correct package path. The proto's `java_package` option (if set) or `package` declaration controls the generated Java package — verify this doesn't break.

## Go Changes

### Fold helpers/ Into Test Files

The `conformance/helpers/` package exists because Go test files in `conformance/` couldn't import code from a sibling package without it being a separate library. By making all helper code `*_test.go` files in the `conformance/` package, they're automatically available to all test files in that package.

**What moves where:**

| helpers/ file | Destination | Notes |
|---|---|---|
| `javainvoker.go` (252 lines) | `java_invoker_test.go` | Rename, add `_test.go` suffix. Package becomes `conformance_test`. |
| `container.go` (155 lines) | `container_test.go` | Same treatment. |
| `testdata.go` (87 lines) | `testdata_test.go` | Same treatment. |
| `conformance_store.go` | `store_helpers_test.go` | Base store wrapper shared across tests. |
| `index_conformance_store.go` (etc.) | Inline into `index_conformance_test.go` | Each specialized store wrapper merges into its test file. |

The 16 specialized store wrappers (one per feature) are each only used by one test file. Inlining them eliminates 16 files and the `helpers/BUILD.bazel` entirely.

**Package declaration**: All `*_test.go` files use `package conformance_test` (external test package — already the case). Helper `_test.go` files in the same directory are part of the same test binary and visible to all test files.

### Store Wrapper Inlining

Each conformance test file currently does:
```go
import "github.com/birdayz/fdb-record-layer-go/conformance/helpers"

store, _ := helpers.NewIndexConformanceStore(...)
```

After inlining, the store wrapper struct and constructor live in the same file:
```go
// index_conformance_test.go

type indexConformanceStore struct { ... }
func newIndexConformanceStore(...) *indexConformanceStore { ... }

var _ = Describe("Index Conformance", func() {
    store := newIndexConformanceStore(...)
    ...
})
```

The type is unexported (lowercase) since it's only used within that file.

## What Gets Deleted

| Path | Lines | Reason |
|---|---|---|
| `conformance/java/build.gradle` | ~40 | Bazel handles it |
| `conformance/java/gradlew` | ~240 | Bazel handles it |
| `conformance/java/gradlew.bat` | ~90 | Bazel handles it |
| `conformance/java/gradle/` | (binary) | Gradle wrapper JARs |
| `conformance/java/build/` | (generated) | Gradle output |
| `conformance/java/src/main/proto/` | ~30 | Duplicate proto |
| `conformance/helpers/BUILD.bazel` | ~20 | Package eliminated |
| `conformance/helpers/*.go` (19 files) | ~3,800 | Inlined into test files |
| `conformance/java/BUILD.bazel` | ~37 | Merged into parent BUILD.bazel |
| `conformance/proto/` | ~30 | Proto moves up one level |

**Net effect**: ~4,300 lines of build/helper code deleted. Java + Go code is preserved but reorganized.

## Migration Plan

Mechanical, low-risk. Each step is independently testable.

### Phase 1: Kill Gradle (no behavior change)

1. Delete `conformance/java/build.gradle`, `gradlew`, `gradlew.bat`, `gradle/`, `build/`.
2. Verify `bazelisk test //conformance:conformance_test` still passes.
3. Commit.

Gradle is already unused by Bazel. The CI doesn't call Gradle anymore (switched to Bazel-only). This is pure deletion.

### Phase 2: Flatten Java Into conformance/ (no behavior change)

1. Move Java sources from `conformance/java/src/main/java/com/birdayz/conformance/*.java` to `conformance/`.
2. Move proto from `conformance/java/src/main/proto/record_layer_demo.proto` — actually already at `conformance/proto/record_layer_demo.proto`, so just delete the Java copy and move `conformance/proto/record_layer_demo.proto` to `conformance/record_layer_demo.proto`.
3. Delete `conformance/java/` directory entirely.
4. Delete `conformance/proto/` directory.
5. Update `conformance/BUILD.bazel` to include proto_library, java_proto_library, java_library, java_binary (merge from old java/BUILD.bazel).
6. Verify `bazelisk test //conformance:conformance_test` passes.
7. Commit.

### Phase 3: Split ConformanceSteps.java

1. Extract `ConformanceBase.java` with shared infrastructure.
2. Split `ConformanceSteps.java` (69 methods) into ~20 per-feature step files.
3. Update `ConformanceServer.java` to scan all step classes.
4. Verify `bazelisk test //conformance:conformance_test` passes.
5. Commit.

### Phase 4: Fold helpers/ Into Test Files

1. Copy `javainvoker.go` → `java_invoker_test.go`, `container.go` → `container_test.go`, `testdata.go` → `testdata_test.go` in `conformance/`. Change package to `conformance_test`. Adjust imports.
2. Copy `conformance_store.go` → `store_helpers_test.go`.
3. For each specialized store wrapper (`index_conformance_store.go`, etc.): inline the struct + constructor into the corresponding test file.
4. Update `conformance/BUILD.bazel`: remove `//conformance/helpers` dep, add new `_test.go` files to srcs glob.
5. Delete `conformance/helpers/` directory entirely.
6. Run `just gazelle` to regenerate BUILD files.
7. Verify `bazelisk test //conformance:conformance_test` passes.
8. Commit.

## Risks

1. **Java package path**: Moving `.java` files out of `src/main/java/com/birdayz/conformance/` means the filesystem path no longer matches `package com.birdayz.conformance`. Bazel's `rules_java` doesn't care — it compiles from any path. But IDEs might complain. Non-issue for us (no IDE for Java, it's purely a test helper).

2. **Proto `strip_import_prefix`**: Changing the proto location requires adjusting `strip_import_prefix` so Java codegen produces correct import paths. Straightforward but must be tested.

3. **Go package test visibility**: All `*_test.go` files in `conformance/` share the test binary. If any helper has a name collision with a test file's local variable, it'll break. Unlikely given the naming convention (unexported types in helpers).

4. **Bazel `glob(["*.java"])`**: Picks up all Java files in the directory. If someone drops a random `.java` file in `conformance/`, it gets compiled. Minor — same risk exists with `glob(["*_test.go"])` for Go.

## Open Questions

1. **Should we rename Go test files for consistency?** Currently some are `*_test.go` and some are `*_conformance_test.go`. Could normalize to all `*_conformance_test.go` while we're restructuring. Mild preference: yes, consistency is free when you're already moving files.

2. **Keep `conformance/README.md`?** Currently exists. Worth updating with the new structure, or delete it (the structure is self-documenting with co-located files).
