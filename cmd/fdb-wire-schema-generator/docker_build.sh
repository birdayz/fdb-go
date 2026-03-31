#!/bin/bash
# docker_build.sh — Compile & run test vector generator inside FDB's official Docker image.
#
# Strategy: inject our generated_messages.cpp into FDB's cmake build as a new target.
# Link against fdbclient + fdbrpc + flow. For the few missing fdbserver symbols,
# we link the specific .o files.
#
# Usage: docker_build.sh <fdb-src-dir> <gen-cpp> <output-dir>

set -euo pipefail

FDB_SRC="$1"
GEN_CPP="$2"
OUTPUT_DIR="$3"

IMAGE="foundationdb/build:rockylinux9-latest"
JOBS=12

BUILD_CACHE="${FDB_BUILD_CACHE:-/tmp/fdb-docker-build}"
SRC_CACHE="${FDB_SRC_CACHE:-/tmp/fdb-docker-src}"
mkdir -p "$OUTPUT_DIR" "$BUILD_CACHE" "$SRC_CACHE"

docker run --rm \
    -e JOBS="$JOBS" \
    -v "$FDB_SRC:/fdb_src:ro" \
    -v "$(realpath "$GEN_CPP"):/work/generated_messages.cpp:ro" \
    -v "$(realpath "$(dirname "$GEN_CPP")/fdb_stubs.h"):/work/fdb_stubs.h:ro" \
    -v "$(realpath "$(dirname "$0")/schema_extractor.h"):/work/schema_extractor.h:ro" \
    -v "$(realpath "$(dirname "$0")/name_capture.h"):/work/name_capture.h:ro" \
    -v "$(realpath "$(dirname "$0")/name_capture.cpp"):/work/name_capture.cpp:ro" \
    -v "$(realpath "$OUTPUT_DIR"):/output" \
    -v "$BUILD_CACHE:/tmp/fdb-build" \
    -v "$SRC_CACHE:/fdb" \
    "$IMAGE" \
    bash -c '
        set -e
        source /opt/rh/gcc-toolset-13/enable

        if [ ! -f /fdb/CMakeLists.txt ]; then
            echo "First run: copying FDB source..."
            cp -r /fdb_src/* /fdb/
        fi
        cp /work/generated_messages.cpp /fdb/generated_messages.cpp
        cp -f /work/schema_extractor.h /fdb/schema_extractor.h
        cp -f /work/name_capture.h /fdb/name_capture.h
        cp -f /work/name_capture.cpp /fdb/name_capture.cpp
        # Also copy fdb_stubs.h if mounted
        if [ -f /work/fdb_stubs.h ]; then
            cp /work/fdb_stubs.h /fdb/fdb_stubs.h
        fi

        # Add our target (idempotent).
        if ! grep -q gen_testvecs /fdb/CMakeLists.txt; then
        cat >> /fdb/CMakeLists.txt << "CMAKE_EOF"

# Build fdbserver sources as a static library so we can link against it.
# (fdbserver is normally an executable, but we need its object files for
# NetNotifiedQueue vtables used by Interface types.)
get_target_property(FDBSERVER_SRCS fdbserver SOURCES)
get_target_property(FDBSERVER_INCS fdbserver INCLUDE_DIRECTORIES)
add_library(fdbserver_lib STATIC EXCLUDE_FROM_ALL ${FDBSERVER_SRCS})
target_include_directories(fdbserver_lib PUBLIC ${FDBSERVER_INCS})
target_link_libraries(fdbserver_lib PUBLIC fdbclient fdbrpc flow)
set_target_properties(fdbserver_lib PROPERTIES COMPILE_OPTIONS "-w")

# Test vector + schema generator. Two compilation units:
# - generated_messages.cpp: normal FDB includes, test vectors + schema types
# - name_capture.cpp: redefined serializer, captures field names as strings
add_executable(gen_testvecs generated_messages.cpp name_capture.cpp)
target_link_libraries(gen_testvecs PRIVATE fdbserver_lib fdbclient fdbrpc flow)
target_include_directories(gen_testvecs PRIVATE
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

        BUILD=/tmp/fdb-build

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
            2>&1 | tail -3
        echo "=== cmake configured ==="

        echo "=== Building fdbserver (-j$JOBS) ==="
        ninja -C $BUILD -j$JOBS fdbserver 2>&1 | tail -3
        echo "=== fdbserver built ==="

        echo "=== Building gen_testvecs ==="
        ninja -C $BUILD -j$JOBS gen_testvecs 2>&1 | tail -5
        echo "=== gen_testvecs built ==="

        $BUILD/bin/gen_testvecs /output
    '

echo "Docker build complete: $(ls "$OUTPUT_DIR"/*.json 2>/dev/null | wc -l) test vectors"
