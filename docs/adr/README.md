# Architecture decision records

An ADR records one design decision: the problem, what we chose, what we rejected and why, and
what it costs. It is short. It explains why the code is the way it is, so nobody has to argue the
same choice again.

Each ADR is a file `NNNN-title.md`, numbered in order. Its status is `Proposed` until a maintainer
accepts it, then `Accepted`. An accepted ADR is not rewritten: a later ADR replaces it and says so.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-store-a-webhook-before-acknowledging-it.md) | The receiver answers 2xx only after an event is stored, and parses it later. | Proposed |
| [0002](0002-batch-job-on-a-store-without-transactions-saves-in-two-phases-under-a-cas-lease.md) | A run on a store without transactions saves in two phases, under a lease held where compare-and-swap exists. | Proposed |
| [0003](0003-a-ledger-with-intent-and-called-markers-for-at-most-once-vendor-calls.md) | A ledger with intent and called markers, plus find-or-create calls, so a vendor action never happens twice. | Proposed |
