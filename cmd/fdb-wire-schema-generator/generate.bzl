"""Custom rule for wire schema generation."""

def _generate_wire_schema_impl(ctx):
    schema_dir = ctx.actions.declare_directory("schema")
    gen_cpp = ctx.actions.declare_file("generated_messages.cpp")

    fdb_srcs = ctx.attr.fdb_srcs[DefaultInfo].files.to_list()
    fdb_root_file = None
    for f in fdb_srcs:
        if f.path.endswith("flow/flat_buffers.cpp"):
            fdb_root_file = f
            break
    if not fdb_root_file:
        fail("Could not find flow/flat_buffers.cpp in FDB sources")

    ctx.actions.run_shell(
        outputs = [schema_dir, gen_cpp],
        inputs = fdb_srcs,
        tools = [ctx.executable.schema_generator],
        command = """
            FDB_SRC=$(dirname $(dirname {fdb_root_file}))
            {schema_gen} "$FDB_SRC" {schema_dir} --gen-cpp={gen_cpp}
        """.format(
            fdb_root_file = fdb_root_file.path,
            schema_gen = ctx.executable.schema_generator.path,
            schema_dir = schema_dir.path,
            gen_cpp = gen_cpp.path,
        ),
        mnemonic = "GenerateWireSchema",
        progress_message = "Parsing FDB headers → wire schemas + C++ test source",
    )

    return [
        DefaultInfo(files = depset([schema_dir, gen_cpp])),
        OutputGroupInfo(
            schema = depset([schema_dir]),
            cpp = depset([gen_cpp]),
        ),
    ]

generate_wire_schema = rule(
    implementation = _generate_wire_schema_impl,
    attrs = {
        "fdb_srcs": attr.label(mandatory = True),
        "schema_generator": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
        ),
    },
)
