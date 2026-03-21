#!/bin/bash
set -euo pipefail
DIR="pkg/recordlayer/testdata"
mkdir -p "$DIR"
if [ -f "$DIR/sift_base.fvecs" ]; then
    echo "SIFT-1M already downloaded"
    exit 0
fi
echo "Downloading SIFT-1M (~500MB)..."
curl -L -o /tmp/sift.tar.gz "ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz"
tar xzf /tmp/sift.tar.gz -C "$DIR" --strip-components=1
rm /tmp/sift.tar.gz
echo "Done: $DIR/sift_base.fvecs ($( wc -c < "$DIR/sift_base.fvecs" ) bytes)"
