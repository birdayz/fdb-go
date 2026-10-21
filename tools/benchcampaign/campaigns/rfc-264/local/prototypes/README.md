# Historical RFC-264 prototype inputs

These patches are preserved only to reconstruct the rejected local comparison
against `68b8a64d274d5e5a041912934198ece79e7e009a`. This PR does not apply either
patch to the production client.

- `p0.patch`: ready-own-reply probe before PendingGet flush; SHA-256
  `22444af5231c7a7c85eafa7bb89dd7d5a4b0937ee3c0393b74efabc05d0b03cb`.
- `p2t.patch`: cache each timer's RawConn while retaining Control calls; SHA-256
  `c3a18eda8d65c9dc1e6d8d731d5710c4c386bfdc70b156ffc927b0f60bd60f7b`.

See the [result](../../../../../../docs/benchmarks/rfc-264/local-quick-screen.md)
for the populations, comparison rule and rejection decision. The common duration
harness and driver are separate from these candidate patches.
