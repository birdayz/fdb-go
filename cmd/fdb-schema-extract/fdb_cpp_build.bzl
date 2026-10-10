"""Local Docker C++ actions with scheduler-accounted, enforced resource bounds."""

# Compact fits the 4 vCPU / 8 GB CI runners: -j1 outran the CI job, -j3 OOM-killed cc1plus
# (CommitProxyInterface.cpp) at 5120 MB. Default, since a non-default flag re-roots CI outputs.
_COMPACT_CPUS = 2
_COMPACT_MEMORY_MB = 5120
_LARGE_CPUS = 4
_LARGE_MEMORY_MB = 24576
_BUILD_IMAGE = "foundationdb/build@sha256:c6133f7e7c2bde2130f712baf56f642ef3e6966bd4f9e4e6b83de14f2db3fd48"

FDBCppResourcesInfo = provider(fields = ["value"])

def _resources_setting_impl(ctx):
    value = ctx.build_setting_value
    if value not in ["compact", "large"]:
        fail("FDB C++ resources must be compact or large, got " + value)
    return [FDBCppResourcesInfo(value = value)]

fdb_cpp_resources = rule(
    implementation = _resources_setting_impl,
    build_setting = config.string(flag = True),
)

def _compact_resources(_os, _input_count):
    return {"cpu": _COMPACT_CPUS, "memory": _COMPACT_MEMORY_MB}

def _large_resources(_os, _input_count):
    return {"cpu": _LARGE_CPUS, "memory": _LARGE_MEMORY_MB}

def _fdb_cpp_build_impl(ctx):
    if len(ctx.outputs.outs) != 1:
        fail("fdb_cpp_build requires exactly one output")
    profile = ctx.attr._resources[FDBCppResourcesInfo].value
    cxx_flags = "-O3 -DNDEBUG"
    if profile == "compact":
        cpus = _COMPACT_CPUS
        memory = _COMPACT_MEMORY_MB
        resources = _compact_resources
    else:
        cpus = _LARGE_CPUS
        memory = _LARGE_MEMORY_MB
        resources = _large_resources
    command = ctx.expand_location(ctx.attr.cmd, targets = ctx.attr.srcs)
    command = ctx.expand_make_variables("cmd", command, {
        "@": ctx.outputs.outs[0].path,
        "SRCS": " ".join([f.path for f in ctx.files.srcs]),
        "FDB_BUILD_CPUS": str(cpus),
        "FDB_BUILD_MEMORY_MB": str(memory),
        "FDB_CXX_FLAGS": cxx_flags,
        "FDB_BUILD_IMAGE": _BUILD_IMAGE,
    })
    ctx.actions.run_shell(
        inputs = ctx.files.srcs,
        outputs = ctx.outputs.outs,
        command = command,
        mnemonic = "FDBCppBuild",
        progress_message = "Building FDB C++ %{label}",
        # Docker needs host mounts, not remote execution. Unlike "local", these
        # requirements still permit the pinned outputs in disk/remote caches.
        execution_requirements = {"no-sandbox": "1", "no-remote-exec": "1"},
        resource_set = resources,
        use_default_shell_env = True,
    )
    return [DefaultInfo(files = depset(ctx.outputs.outs))]

fdb_cpp_build = rule(
    implementation = _fdb_cpp_build_impl,
    attrs = {
        "srcs": attr.label_list(allow_files = True),
        "outs": attr.output_list(mandatory = True),
        "cmd": attr.string(mandatory = True),
        "_resources": attr.label(default = "//cmd/fdb-schema-extract:resources"),
    },
)
