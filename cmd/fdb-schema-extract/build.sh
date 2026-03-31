#!/bin/bash
# build.sh — Compile and run fdb-schema-extract inside FDB's Docker image.
#
# Usage: ./build.sh <fdb-source-dir> <output-dir>
#
# Produces one JSON schema file per type in <output-dir>.

set -euo pipefail

FDB_SRC="$1"
OUTPUT_DIR="$2"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="foundationdb/build:rockylinux9-latest"
JOBS=12

BUILD_CACHE="${FDB_BUILD_CACHE:-/tmp/fdb-docker-build}"
SRC_CACHE="${FDB_SRC_CACHE:-/tmp/fdb-docker-src}"
mkdir -p "$OUTPUT_DIR" "$BUILD_CACHE" "$SRC_CACHE"

docker run --rm \
    -e JOBS="$JOBS" \
    -v "$FDB_SRC:/fdb_src:ro" \
    -v "$SCRIPT_DIR/main.cpp:/work/main.cpp:ro" \
    -v "$SCRIPT_DIR/name_capture.cpp:/work/name_capture.cpp:ro" \
    -v "$(realpath "$OUTPUT_DIR"):/output" \
    -v "$BUILD_CACHE:/tmp/build" \
    -v "$SRC_CACHE:/fdb" \
    "$IMAGE" \
    bash -c '
        set -e
        source /opt/rh/gcc-toolset-13/enable

        if [ ! -f /fdb/CMakeLists.txt ]; then
            echo "First run: copying FDB source..."
            cp -r /fdb_src/* /fdb/
        fi

        # Copy our source files.
        cp /work/main.cpp /fdb/schema_extract_main.cpp
        cp /work/name_capture.cpp /fdb/schema_extract_names.cpp

        # Patches: disable binding tester + fix any missing includes.
        sed -i "s/package_bindingtester/#package_bindingtester/" /fdb/bindings/CMakeLists.txt 2>/dev/null || true
        # Suppress errors in fdbserver_lib (we only need it for linking, not correctness).
        sed -i "s/COMPILE_OPTIONS \"-w\"/COMPILE_OPTIONS \"-w;-Wno-error\"/" /fdb/CMakeLists.txt 2>/dev/null || true

        # Write a standalone cmake fragment (idempotent — always overwrite).
        cat > /fdb/schema_extract.cmake << "CMAKE_EOF"
add_executable(schema_extract schema_extract_main.cpp schema_extract_names.cpp)
target_link_libraries(schema_extract PRIVATE fdbclient fdbrpc flow)
target_include_directories(schema_extract PRIVATE
    ${CMAKE_SOURCE_DIR}
    ${CMAKE_BINARY_DIR}
    ${CMAKE_BINARY_DIR}/fdbclient/include
    ${CMAKE_BINARY_DIR}/fdbclient
    ${CMAKE_BINARY_DIR}/fdbrpc/include
    ${CMAKE_BINARY_DIR}/fdbrpc
    ${CMAKE_BINARY_DIR}/flow/include
    ${CMAKE_BINARY_DIR}/flow
)
CMAKE_EOF
        # Include it from main CMakeLists.txt (idempotent).
        if ! grep -q schema_extract.cmake /fdb/CMakeLists.txt; then
            echo "include(schema_extract.cmake)" >> /fdb/CMakeLists.txt
        fi

        BUILD=/tmp/build

        echo "=== Configuring cmake ==="
        cmake -S /fdb -B $BUILD -G Ninja \
            -DCMAKE_BUILD_TYPE=Release \
            -DBUILD_PYTHON_BINDING=OFF \
            -DBUILD_C_BINDING=ON \
            -DBUILD_JAVA_BINDING=OFF \
            -DBUILD_GO_BINDING=OFF \
            -DBUILD_SWIFT_BINDING=OFF \
            -DBUILD_RUBY_BINDING=OFF \
            -DBUILD_DOCUMENTATION=OFF \
            -DWITH_CSHARP=OFF \
            -DWITH_PYTHON=OFF \
            -DUSE_WERROR=OFF \
            -DBUILD_TESTING=OFF \
            2>&1 | tail -20
        echo "=== cmake configured ==="

        echo "=== Building fdbclient (-j$JOBS) ==="
        ninja -C $BUILD -j$JOBS fdb_c 2>&1 | tail -3
        echo "=== fdbclient built ==="

        echo "=== Building schema_extract ==="
        ninja -C $BUILD -j$JOBS schema_extract 2>&1 | tail -30
        echo "=== schema_extract built ==="

        echo "=== Running schema_extract ==="
        $BUILD/bin/schema_extract /output
    '

echo "Done: $(ls "$OUTPUT_DIR"/*.json 2>/dev/null | wc -l) schema files in $OUTPUT_DIR"
