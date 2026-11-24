def _go_sdk_binary_impl(ctx):
    sdk = ctx.toolchains["@rules_go//go:toolchain"].sdk
    return [DefaultInfo(
        files = depset([sdk.go]),
        runfiles = ctx.runfiles(
            files = [sdk.go, sdk.root_file],
            transitive_files = depset(transitive = [sdk.headers, sdk.srcs, sdk.libs, sdk.tools]),
        ),
    )]

# Expose the Bazel-selected Go SDK binary and its stdlib/tool runfiles tree.
go_sdk_binary = rule(
    implementation = _go_sdk_binary_impl,
    toolchains = ["@rules_go//go:toolchain"],
)
