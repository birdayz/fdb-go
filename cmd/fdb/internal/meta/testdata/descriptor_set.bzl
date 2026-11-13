"""Dependency-complete descriptor sets for metadata loader fixtures."""

load("@protobuf//bazel/common:proto_info.bzl", "ProtoInfo")

def _descriptor_set_impl(ctx):
    descriptor_sets = depset(transitive = [
        dep[ProtoInfo].transitive_descriptor_sets
        for dep in ctx.attr.deps
    ])
    output = ctx.outputs.out
    # FileDescriptorSet contains only a repeated message field, so concatenation
    # merges the compiler outputs without decoding or rewriting custom options.
    ctx.actions.run_shell(
        inputs = descriptor_sets,
        outputs = [output],
        arguments = [output.path] + [f.path for f in descriptor_sets.to_list()],
        command = 'out="$1"; shift; cat "$@" > "$out"',
        mnemonic = "MergeDescriptorSets",
    )
    return [DefaultInfo(
        files = depset([output]),
        runfiles = ctx.runfiles(files = [output]),
    )]

descriptor_set = rule(
    implementation = _descriptor_set_impl,
    attrs = {
        "deps": attr.label_list(
            mandatory = True,
            providers = [ProtoInfo],
        ),
        "out": attr.output(mandatory = True),
    },
)
