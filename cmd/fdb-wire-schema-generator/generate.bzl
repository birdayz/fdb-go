"""Custom rule to generate wire schema + test vectors as directory outputs."""

def _generate_wire_data_impl(ctx):
    schema_dir = ctx.actions.declare_directory("schema")
    testdata_dir = ctx.actions.declare_directory("testdata")

    # Find the FDB source root from the all_srcs filegroup.
    fdb_srcs = ctx.attr.fdb_srcs[DefaultInfo].files.to_list()
    # Pick any file to derive the root directory.
    fdb_root_file = None
    for f in fdb_srcs:
        if f.path.endswith("flow/flat_buffers.cpp"):
            fdb_root_file = f
            break
    if not fdb_root_file:
        fail("Could not find flow/flat_buffers.cpp in FDB sources")

    # The FDB root is two levels up from flow/flat_buffers.cpp.
    # e.g. external/foundationdb+/flow/flat_buffers.cpp → external/foundationdb+

    ctx.actions.run_shell(
        outputs = [schema_dir, testdata_dir],
        inputs = fdb_srcs,
        tools = [ctx.executable.schema_generator, ctx.executable.testvec_generator],
        command = """
            FDB_SRC=$(dirname $(dirname {fdb_root_file}))
            {schema_gen} "$FDB_SRC" {schema_dir}
            {testvec_gen} {testdata_dir}
        """.format(
            fdb_root_file = fdb_root_file.path,
            schema_gen = ctx.executable.schema_generator.path,
            testvec_gen = ctx.executable.testvec_generator.path,
            schema_dir = schema_dir.path,
            testdata_dir = testdata_dir.path,
        ),
        mnemonic = "GenerateWireData",
        progress_message = "Generating FDB wire schema + test vectors",
    )

    return [DefaultInfo(files = depset([schema_dir, testdata_dir]))]

generate_wire_data = rule(
    implementation = _generate_wire_data_impl,
    attrs = {
        "fdb_srcs": attr.label(mandatory = True),
        "schema_generator": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
        ),
        "testvec_generator": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
        ),
    },
)
