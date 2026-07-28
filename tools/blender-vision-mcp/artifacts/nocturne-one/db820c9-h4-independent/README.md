# NOCTURNE/ONE H4 independent condition

This directory preserves the independent `gpt-5.6-sol` H4 condition, including
its successful sealed-builder receipt, complete JSONL transcript, prompt,
candidate receipt, two candidate portfolios, 13 failure/repair records, source
archive, and frozen evaluator results.

Boundary receipts:

- sealed builder:
  `147c7aaa88b195bf10335dda1556f263a5e4dcdc9ea8316e0b4cfff6c899479f`;
- candidate:
  `75c94955ff91c2b9fccb539fb3ea241abf564df4c2cf9c196db160b4bfcb1d66`;
- source archive:
  `d53c3ff3e1da4f08d078859db664eacbc7c66e914e4abfb919c488609b0feb91`;
- frozen 3D evaluator:
  `13f85fe4c8084a46ff5178f777a202d01889b499952b888d8423357867e52f07`;
- frozen app evaluator:
  `528a0ccc88afcea5d1fd2794e6f29efe0ba432af06a4442bb85c626ea9ea0b5e`.

The builder process passed its local gates and sealed the candidate, but the
frozen evaluator rejected both 3D and application output. The negative result
is authoritative and retained.

`evaluator-app-invalid-operator/` records an earlier evaluator invocation with
an incorrect hidden-trace path. It is preserved as operator-error evidence and
is not used to score H4. Generated `node_modules`, `dist`, data, and duplicated
fresh-clone trees are excluded; the source archive and evaluator logs/receipts
preserve the material result.
