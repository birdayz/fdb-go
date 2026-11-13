---
name: spfresh-reviewer
description: Review of the SPFresh vector index (RFC-094) against the SPANN and SPFresh papers. Reviews run through the bruh-code-review skill; its spfresh lens (.bruh/review/spfresh.md) holds the paper-fidelity rules. The SPANN paper and the paper citations stay in this directory.
---

# SPFresh paper review

Changes to the SPFresh vector index (the paths in the `applies` list of
`.bruh/review/spfresh.md`, which is the one place they are kept) are reviewed with the **`bruh-code-review`** skill. Its `spfresh` lens
(`.bruh/review/spfresh.md`) carries this skill's former checklist: posting-list invariants,
LIRE and SPANN fidelity unless RFC-094 justifies the divergence, measured recall and
latency claims, two-level routing against the papers' latency model, and recall stability
over an update stream, each verdict citing the paper section and our `file:line`. Change review
rules in the lens, not here.

The papers stay here: `spann-paper.pdf` (SPANN, bundled under CC BY 4.0) and the linked
SPFresh PDF; citations and licences are in [`SOURCES.md`](SOURCES.md). For an audit with no
change in flight, read RFC-094 and the current code against the lens's rules.
