# Source papers & licensing

The papers below are the **algorithmic spec** the `spfresh-reviewer` persona
reads to judge our SPFresh implementation. Only SPANN is bundled, verbatim and
unmodified. SPFresh is linked rather than redistributed because its stated
license includes NonCommercial and NoDerivatives restrictions.

## `spann-paper.pdf`

- **Title:** SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search
- **Authors:** Qi Chen, Bing Zhao, Haidong Wang, Mingqin Li, Chuanjie Liu, Zengzhong Li, Mao Yang, Jingdong Wang
- **Venue:** NeurIPS 2021
- **arXiv:** [2111.08566](https://arxiv.org/abs/2111.08566)
- **License:** **CC BY 4.0** (<https://creativecommons.org/licenses/by/4.0/>) — redistribution and adaptation permitted with attribution. The verbatim PDF here is the CC BY arXiv version.

## SPFresh (external paper)

- **Title:** SPFresh: Incremental In-Place Update for Billion-Scale Vector Search
- **Authors:** Yuming Xu, Hengyu Liang, Jin Li, Shuotao Xu, Qi Chen, Qianxi Zhang, Cheng Li, Ziyue Yang, Fan Yang, Yuqing Yang, Peng Cheng, Mao Yang
- **Venue:** SOSP 2023
- **arXiv:** [2410.14452](https://arxiv.org/abs/2410.14452)
- **License stated by arXiv:** **CC BY-NC-ND 4.0** (<https://creativecommons.org/licenses/by-nc-nd/4.0/>) — Attribution-NonCommercial-NoDerivatives. Read the [official PDF](https://arxiv.org/pdf/2410.14452); it is not included in this repository.

## Related papers referenced elsewhere in the repo

These are **not** included as PDFs; the repository contains own-words technical
summaries and links to the publications instead:

- **VBASE** (Unifying Online Vector Similarity Search and Relational Queries via Relaxed Monotonicity, OSDI '23) — [USENIX publication page](https://www.usenix.org/conference/osdi23/presentation/zhang-qianxi). Summary: [`docs/vbase-osdi-2023.md`](../../../../docs/vbase-osdi-2023.md). Directly on this lineage (overlapping authors; integrates SPANN) and the basis for RFC-156.
- **Graefe, The Cascades Framework for Query Optimization** (IEEE Data Engineering Bulletin 18(3), 1995) — [IEEE Data Engineering Bulletin archive](http://sites.computer.org/debull/95SEP-CD.pdf). Summary: [`docs/graefe-cascades-1995.md`](../../../../docs/graefe-cascades-1995.md).
