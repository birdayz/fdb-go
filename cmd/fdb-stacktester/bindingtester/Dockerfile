FROM foundationdb/foundationdb:7.3.75

# Override entrypoint (default runs fdbserver).
ENTRYPOINT []

# Install pip. The FDB image is Ubuntu-based.
RUN apt-get update -qq && apt-get install -y -qq python3-pip >/dev/null 2>&1

# Install FDB Python binding. It uses the libfdb_c.so already in the image.
RUN pip3 install foundationdb --break-system-packages 2>/dev/null || pip3 install foundationdb

# bindingtester source is copied from FDB source at build time.
COPY bindingtester/ /opt/fdb/bindings/bindingtester/
# Python fdb binding is also needed (bindingtester imports it via sys.path).
COPY python/ /opt/fdb/bindings/python/

# Our stacktester binary.
COPY fdb-stacktester /usr/local/bin/fdb-stacktester
RUN chmod +x /usr/local/bin/fdb-stacktester

WORKDIR /opt/fdb/bindings/bindingtester
