"""Repository rule to download and extract the SIFT-1M dataset from INRIA.

Uses Bazel's built-in ctx.download_and_extract() for proper caching,
checksum verification, and no external tool dependencies (no curl needed).

Usage in MODULE.bazel:
    sift_dataset = use_repo_rule("//:sift_repo.bzl", "sift_dataset")
    sift_dataset(name = "sift1m")
"""

def _sift_dataset_impl(ctx):
    ctx.download_and_extract(
        url = "ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz",
        sha256 = ctx.attr.sha256,
        stripPrefix = "sift",
    )

    ctx.file("BUILD.bazel", """
filegroup(
    name = "data",
    srcs = glob(["*.fvecs", "*.ivecs"]),
    visibility = ["//visibility:public"],
)
""")

sift_dataset = repository_rule(
    implementation = _sift_dataset_impl,
    attrs = {
        "sha256": attr.string(
            # Set to empty string to skip verification on first download,
            # then pin from the log output. Empty = no verification.
            default = "",
        ),
    },
    doc = "Downloads the SIFT-1M dataset from INRIA TEXMEX. Cached by Bazel.",
)
