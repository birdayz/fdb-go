"""Repository rule to download and extract the SIFT-1M dataset from INRIA.

The canonical source uses FTP which Bazel's http_archive doesn't support.
This rule shells out to curl (which handles FTP) and caches the result
in Bazel's repository cache like any other external dependency.

Usage in MODULE.bazel:
    sift_dataset = use_repo_rule("//:sift_repo.bzl", "sift_dataset")
    sift_dataset(name = "sift1m")
"""

def _sift_dataset_impl(ctx):
    # Download via curl (supports FTP).
    ctx.report_progress("Downloading SIFT-1M dataset (~160MB compressed)...")
    result = ctx.execute(
        ["curl", "-L", "-o", "sift.tar.gz", "ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz"],
        timeout = 600,
    )
    if result.return_code != 0:
        fail("Failed to download SIFT-1M: " + result.stderr)

    # Extract.
    ctx.report_progress("Extracting SIFT-1M...")
    result = ctx.execute(
        ["tar", "xzf", "sift.tar.gz", "--strip-components=1"],
        timeout = 120,
    )
    if result.return_code != 0:
        fail("Failed to extract SIFT-1M: " + result.stderr)

    # Clean up tarball.
    ctx.execute(["rm", "sift.tar.gz"])

    # Write BUILD file exposing the data files.
    ctx.file("BUILD.bazel", """
filegroup(
    name = "data",
    srcs = glob(["*.fvecs", "*.ivecs"]),
    visibility = ["//visibility:public"],
)
""")

sift_dataset = repository_rule(
    implementation = _sift_dataset_impl,
    doc = "Downloads the SIFT-1M dataset from INRIA TEXMEX (FTP). Cached by Bazel.",
)
