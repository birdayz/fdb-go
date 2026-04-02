#!/bin/bash
# build.sh — Compile and run fdb-schema-extract inside FDB's Docker build image.
#
# Usage: ./build.sh <fdb-source-dir> <output-dir>
#
# <fdb-source-dir> is the FDB source tree (e.g., from Bazel's @foundationdb//:all_srcs).
# <output-dir> is where generated Go files are written.
#
# Called by Bazel genrule or manually via: just generate-wire-types

set -euo pipefail

FDB_SRC="${1:?usage: $0 <fdb-source-dir> <output-dir>}"
OUTPUT_DIR="${2:?usage: $0 <fdb-source-dir> <output-dir>}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="foundationdb/build:rockylinux9-latest"
JOBS="${JOBS:-12}"

# Docker volume for cmake build cache (survives across runs).
BUILD_CACHE="${FDB_BUILD_CACHE:-/tmp/fdb-schema-extract-build}"
SRC_CACHE="${FDB_SRC_CACHE:-/tmp/fdb-schema-extract-src}"
mkdir -p "$OUTPUT_DIR" "$BUILD_CACHE" "$SRC_CACHE"

docker run --rm \
    --user "$(id -u):$(id -g)" \
    -e JOBS="$JOBS" \
    -v "$FDB_SRC:/fdb_src:ro" \
    -v "$SCRIPT_DIR/main.cpp:/work/main.cpp:ro" \
    -v "$SCRIPT_DIR/extract.h:/work/extract.h:ro" \
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

        cp /work/main.cpp /fdb/schema_extract_main.cpp
        cp /work/extract.h /fdb/extract.h
        touch /fdb/schema_extract_main.cpp /fdb/extract.h

        # Patches: disable binding tester + suppress warnings-as-errors.
        sed -i "s/package_bindingtester/#package_bindingtester/" /fdb/bindings/CMakeLists.txt 2>/dev/null || true
        sed -i "s/COMPILE_OPTIONS \"-w\"/COMPILE_OPTIONS \"-w;-Wno-error\"/" /fdb/CMakeLists.txt 2>/dev/null || true

        cat > /fdb/schema_extract.cmake << "CMAKE_EOF"
add_executable(schema_extract schema_extract_main.cpp)
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
        if ! grep -q schema_extract.cmake /fdb/CMakeLists.txt; then
            echo "include(schema_extract.cmake)" >> /fdb/CMakeLists.txt
        fi

        BUILD=/tmp/build

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
            2>&1 | tail -5

        echo "=== Building fdb_c + schema_extract (-j$JOBS) ==="
        ninja -C $BUILD -j$JOBS fdb_c schema_extract 2>&1 | tail -5

        echo "=== Running schema_extract ==="
        $BUILD/bin/schema_extract /output
    '

echo "Generated $(ls "$OUTPUT_DIR"/*_generated.go 2>/dev/null | wc -l) files in $OUTPUT_DIR"
