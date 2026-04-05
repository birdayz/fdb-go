#!/bin/bash
# build.sh — Compile fdb-diff-oracle inside FDB's Docker build image.
#
# Usage: ./build.sh <fdb-source-dir> [output-binary-path]
#
# Produces a statically-linked diff-oracle binary. Uses cached cmake
# build from fdb-schema-extract if available.

set -euo pipefail

FDB_SRC="${1:?usage: $0 <fdb-source-dir> [output-binary-path]}"
OUTPUT="${2:-./diff-oracle}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="foundationdb/build:rockylinux9-latest"
JOBS="${JOBS:-$(nproc)}"

# Reuse cmake build cache from schema-extract.
BUILD_CACHE="${FDB_BUILD_CACHE:-/tmp/fdb-schema-extract-build}"
SRC_CACHE="${FDB_SRC_CACHE:-/tmp/fdb-schema-extract-src}"
TMPOUT="$(mktemp -d)"
mkdir -p "$BUILD_CACHE" "$SRC_CACHE" "$TMPOUT"
chmod 777 "$TMPOUT"

docker run --rm \
    --user "$(id -u):$(id -g)" \
    -e JOBS="$JOBS" \
    -v "$FDB_SRC:/fdb_src:ro" \
    -v "$SCRIPT_DIR/main.cpp:/work/main.cpp:ro" \
    -v "$TMPOUT:/out" \
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

        cp /work/main.cpp /fdb/diff_oracle_main.cpp
        touch /fdb/diff_oracle_main.cpp

        # Patches
        sed -i "s/package_bindingtester/#package_bindingtester/" /fdb/bindings/CMakeLists.txt 2>/dev/null || true

        cat > /fdb/diff_oracle.cmake << "CMAKE_EOF"
add_executable(diff_oracle diff_oracle_main.cpp)
target_link_libraries(diff_oracle PRIVATE fdbclient fdbrpc flow)
target_include_directories(diff_oracle PRIVATE
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
        if ! grep -q diff_oracle.cmake /fdb/CMakeLists.txt; then
            echo "include(diff_oracle.cmake)" >> /fdb/CMakeLists.txt
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

        echo "=== Building fdb_c + diff_oracle (-j$JOBS) ==="
        ninja -C $BUILD -j$JOBS fdb_c diff_oracle 2>&1 | tail -5

        cp $BUILD/bin/diff_oracle /out/diff-oracle
        chmod 755 /out/diff-oracle
        echo "=== Built diff-oracle ==="
    '

mv "$TMPOUT/diff-oracle" "$OUTPUT"
rm -rf "$TMPOUT"
echo "Oracle binary: $OUTPUT"
