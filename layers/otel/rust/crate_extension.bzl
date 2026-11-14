"""OTEL's separately named, pinned crate universe."""
load("@rules_rust//crate_universe:extensions.bzl", _crate = "crate")
otel_crates = _crate
