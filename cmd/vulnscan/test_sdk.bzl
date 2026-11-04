def _test_go_sdk_impl(ctx):
    sdk = ctx.toolchains["@rules_go//go:toolchain"].sdk
    return [DefaultInfo(
        files = depset([sdk.go]),
        runfiles = ctx.runfiles(
            files = [sdk.go],
            transitive_files = depset(transitive = [sdk.headers, sdk.srcs, sdk.libs, sdk.tools]),
        ),
    )]

# Expose the selected, version/integrity-pinned SDK, including its runtime files.
test_go_sdk = rule(
    implementation = _test_go_sdk_impl,
    toolchains = ["@rules_go//go:toolchain"],
)
