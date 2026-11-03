#!/usr/bin/env python3
"""Bundle the website's runnable RL schema with its licensed proto imports."""

import gzip
import io
from pathlib import Path
import tarfile


ROOT = Path(__file__).resolve().parents[1]
DEST = ROOT / "website/static/examples/record-layer"
IMPORTS = (
    "record_key_expression.proto",
    "record_metadata.proto",
    "record_metadata_options.proto",
    "record_query_plan.proto",
)


def main():
    for name in IMPORTS:
        (DEST / name).write_bytes((ROOT / "proto/apple" / name).read_bytes())
    for name in ("LICENSE", "NOTICE"):
        (DEST / name).write_bytes((ROOT / name).read_bytes())
    names = sorted((*IMPORTS, "buf.yaml", "records.proto", "LICENSE", "NOTICE"))
    archive = io.BytesIO()
    with tarfile.open(fileobj=archive, mode="w", format=tarfile.USTAR_FORMAT) as tar:
        for name in names:
            data = (DEST / name).read_bytes()
            header = tarfile.TarInfo(name)
            header.size = len(data)
            header.mode = 0o644
            tar.addfile(header, io.BytesIO(data))
    # Stored DEFLATE blocks keep this small source bundle reproducible across
    # zlib versions; tar metadata and the gzip timestamp are fixed as well.
    with (DEST / "record-layer.tar.gz").open("wb") as out:
        with gzip.GzipFile(filename="", mode="wb", compresslevel=0, mtime=0, fileobj=out) as zipped:
            zipped.write(archive.getvalue())
    print(f"Bundled {len(names)} source/license files in {DEST.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
