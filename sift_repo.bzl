"""Repository rule to download and extract the SIFT-1M dataset from INRIA.

The canonical source (ftp://ftp.irisa.fr) uses FTP which Bazel's built-in
downloader does not support. We use ctx.execute with curl (which handles
FTP natively) and then ctx.extract for the tar.gz.

Usage in MODULE.bazel:
    sift_dataset = use_repo_rule("//:sift_repo.bzl", "sift_dataset")
    sift_dataset(name = "sift1m")
"""

_URL = "ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz"

def _sift_dataset_impl(ctx):
    ctx.report_progress("Downloading SIFT-1M dataset (~160MB)...")
    ctx.execute(
        ["curl", "-fSL", "-o", "sift.tar.gz", _URL],
        timeout = 600,
        quiet = False,
    )
    ctx.extract("sift.tar.gz", stripPrefix = "sift")
    ctx.delete("sift.tar.gz")
    ctx.file("BUILD.bazel", """\
filegroup(
    name = "data",
    srcs = glob(["*.fvecs", "*.ivecs"]),
    visibility = ["//visibility:public"],
)
""")

sift_dataset = repository_rule(
    implementation = _sift_dataset_impl,
    doc = "Downloads the SIFT-1M dataset from INRIA TEXMEX (FTP via curl). Cached by Bazel.",
)
