# WS-J design gate v1

TREE `eb426a53f814802d5c9ff1f4fca67ee199f4c1cd`; design SHA256 `1388036404d405a028df761a1055370d267e36dd6fdda6c93cb9b00a058a5814`. First gate of the WS-J design (existence
policies, metadata assembly order, literal carriers, unnest-sourced indexes, enum
DDL, bit/bitmap lanes, unified DDL front end) plus the already-landed field-path
trie fix. Live oracle: 118 pinned Java outcomes + 11 query probes (ws-j-oracle/).
Lenses: Graefe, Torvalds, storage/wire (query-engine parts: Graefe and Torvalds
mandatory).
