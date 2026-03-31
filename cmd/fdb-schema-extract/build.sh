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

BUILD_CACHE="${FDB_BUILD_CACHE:-/tmp/fdb-schema-build}"
SRC_CACHE="${FDB_SRC_CACHE:-/tmp/fdb-schema-src}"
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

        # Patch: disable binding tester (references python_binding which doesnt exist).
        sed -i "s/package_bindingtester/#package_bindingtester/" /fdb/bindings/CMakeLists.txt 2>/dev/null || true

        # Add our cmake target (idempotent).
        if ! grep -q schema_extract /fdb/CMakeLists.txt; then
            cat >> /fdb/CMakeLists.txt << "CMAKE_EOF"

# Schema extractor — separate from gen_testvecs.
# Two compilation units: main.cpp (normal FDB) + name_capture.cpp (redefined serializer).
get_target_property(FDBSERVER_SRCS fdbserver SOURCES)
get_target_property(FDBSERVER_INCS fdbserver INCLUDE_DIRECTORIES)
if(NOT TARGET fdbserver_lib)
  add_library(fdbserver_lib STATIC EXCLUDE_FROM_ALL ${FDBSERVER_SRCS})
  target_include_directories(fdbserver_lib PUBLIC ${FDBSERVER_INCS})
  target_link_libraries(fdbserver_lib PUBLIC fdbclient fdbrpc flow)
  set_target_properties(fdbserver_lib PROPERTIES COMPILE_OPTIONS "-w")
endif()

add_executable(schema_extract schema_extract_main.cpp schema_extract_names.cpp)
target_link_libraries(schema_extract PRIVATE fdbserver_lib fdbclient fdbrpc flow)
target_include_directories(schema_extract PRIVATE
    ${CMAKE_SOURCE_DIR}
    ${CMAKE_SOURCE_DIR}/fdbserver/include
    ${CMAKE_SOURCE_DIR}/fdbserver
    ${CMAKE_BINARY_DIR}
    ${CMAKE_BINARY_DIR}/fdbserver/include
    ${CMAKE_BINARY_DIR}/fdbserver
    ${CMAKE_BINARY_DIR}/fdbclient/include
    ${CMAKE_BINARY_DIR}/fdbclient
    ${CMAKE_BINARY_DIR}/fdbrpc/include
    ${CMAKE_BINARY_DIR}/fdbrpc
    ${CMAKE_BINARY_DIR}/flow/include
    ${CMAKE_BINARY_DIR}/flow
)
CMAKE_EOF
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

        echo "=== Building fdbserver (-j$JOBS) ==="
        ninja -C $BUILD -j$JOBS fdbserver 2>&1 | tail -3
        echo "=== fdbserver built ==="

        echo "=== Building schema_extract ==="
        ninja -C $BUILD -j$JOBS schema_extract 2>&1 | tail -5
        echo "=== schema_extract built ==="

        echo "=== Running schema_extract ==="
        $BUILD/bin/schema_extract /output
    '

echo "Done: $(ls "$OUTPUT_DIR"/*.json 2>/dev/null | wc -l) schema files in $OUTPUT_DIR"
