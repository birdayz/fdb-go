# Source papers & licensing

The two PDFs in this directory are the **algorithmic spec** the `spfresh-reviewer`
persona reads to judge our SPFresh implementation (the analog of the Graefe paper
for the Cascades optimizer). They are included **verbatim and unmodified** under
their respective open licenses. Attribution below; do not modify the PDFs (one is
No-Derivatives — see SPFresh).

## `spann-paper.pdf`

- **Title:** SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search
- **Authors:** Qi Chen, Bing Zhao, Haidong Wang, Mingqin Li, Chuanjie Liu, Zengzhong Li, Mao Yang, Jingdong Wang
- **Venue:** NeurIPS 2021
- **arXiv:** [2111.08566](https://arxiv.org/abs/2111.08566)
- **License:** **CC BY 4.0** (<https://creativecommons.org/licenses/by/4.0/>) — redistribution and adaptation permitted with attribution. The verbatim PDF here is the CC BY arXiv version.

## `spfresh-paper.pdf`

- **Title:** SPFresh: Incremental In-Place Update for Billion-Scale Vector Search
- **Authors:** Yuming Xu, Hengyu Liang, Jin Li, Shuotao Xu, Qi Chen, Qianxi Zhang, Cheng Li, Ziyue Yang, Fan Yang, Yuqing Yang, Peng Cheng, Mao Yang
- **Venue:** SOSP 2023
- **arXiv:** [2410.14452](https://arxiv.org/abs/2410.14452)
- **License:** **CC BY-NC-ND 4.0** (<https://creativecommons.org/licenses/by-nc-nd/4.0/>) — Attribution-NonCommercial-NoDerivatives. The verbatim, unmodified arXiv PDF may be redistributed for non-commercial use with attribution. **No derivatives**: do not commit a reformatted/markdown version of this paper; keep the PDF as-is.

## Related papers referenced elsewhere in the repo

These are **not** included as PDFs (no Creative-Commons grant), so they live as
own-words technical summaries instead:

- **VBASE** (Unifying Online Vector Similarity Search and Relational Queries via Relaxed Monotonicity, OSDI '23) — USENIX open-access (read/download only), no CC license. Summary: [`docs/vbase-osdi-2023.md`](../../../docs/vbase-osdi-2023.md). Directly on this lineage (overlapping authors; integrates SPANN) and the basis for RFC-156.
- **Graefe, The Cascades Framework for Query Optimization** (IEEE Data Engineering Bulletin 18(3), 1995) — free-to-read, author-retained copyright, no CC license. Summary: [`docs/graefe-cascades-1995.md`](../../../docs/graefe-cascades-1995.md).
