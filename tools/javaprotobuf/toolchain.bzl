"""Java-only proto compiler/runtime pin for the Record Layer oracle."""

load("@protobuf//bazel/common:proto_info.bzl", "ProtoInfo")
load("@protobuf//bazel/common:proto_lang_toolchain_info.bzl", "ProtoLangToolchainInfo")
load("@rules_java//java/common:java_info.bzl", "JavaInfo")

_JAVA_TOOLCHAIN_TYPE = Label("@protobuf//bazel/private:java_toolchain_type")

def _java_proto_toolchain_impl(ctx):
    # The public language-toolchain rule selects the native module's compiler.
    # Supply a Java-specific compiler without changing any native toolchain.
    info = ProtoLangToolchainInfo(
        out_replacement_format_flag = "--java_out=%s",
        output_files = "single",
        plugin_format_flag = "",
        plugin = None,
        runtime = ctx.attr.runtime,
        provided_proto_sources = depset(
            transitive = [
                dep[ProtoInfo].transitive_sources
                for dep in ctx.attr.denylisted_protos
            ],
        ).to_list(),
        proto_compiler = ctx.attr.protoc[DefaultInfo].files_to_run,
        protoc_opts = [],
        progress_message = "Generating Java proto %{label}",
        mnemonic = "GenProto",
        allowlist_different_package = None,
        toolchain_type = _JAVA_TOOLCHAIN_TYPE,
    )
    return [info, platform_common.ToolchainInfo(proto = info)]

java_proto_toolchain = rule(
    implementation = _java_proto_toolchain_impl,
    attrs = {
        "protoc": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
            allow_files = True,
        ),
        "runtime": attr.label(
            mandatory = True,
            providers = [JavaInfo],
        ),
        "denylisted_protos": attr.label_list(
            providers = [ProtoInfo],
        ),
    },
    provides = [ProtoLangToolchainInfo, platform_common.ToolchainInfo],
)
