ARG FDB_VERSION=7.3.77
FROM foundationdb/foundationdb:${FDB_VERSION}

ENTRYPOINT []
ENV LD_LIBRARY_PATH=/usr/lib

# Bootstrap pip, install FDB Python binding matching the image version.
ARG FDB_VERSION=7.3.77
RUN python3 -m ensurepip && \
    FDB_MAJOR_MINOR=$(echo ${FDB_VERSION} | cut -d. -f1,2) && \
    python3 -m pip install "foundationdb>=${FDB_MAJOR_MINOR},<${FDB_MAJOR_MINOR}.99"

COPY bindingtester/ /opt/fdb/bindings/bindingtester/

# Patch __init__.py: remove sys.path hack that shadows pip fdb with source tree.
RUN sed -i "s|sys.path\[:0\].*||" /opt/fdb/bindings/bindingtester/__init__.py && \
    sed -i "s|import util|from bindingtester import util|" /opt/fdb/bindings/bindingtester/__init__.py && \
    sed -i "s|from fdb import LATEST_API_VERSION|LATEST_API_VERSION = 730|" /opt/fdb/bindings/bindingtester/__init__.py

COPY fdb-stacktester /usr/local/bin/fdb-stacktester
RUN chmod +x /usr/local/bin/fdb-stacktester

WORKDIR /opt/fdb/bindings/bindingtester
