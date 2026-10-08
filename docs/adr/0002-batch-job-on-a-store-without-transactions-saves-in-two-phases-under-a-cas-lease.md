# ADR-0002: A batch job on a store without transactions saves in two phases, under a lease held in a store that has compare-and-swap

## Status
Proposed

## Context

Some jobs keep their state in a store that cannot do what a database does: no multi-row
transactions across calls, no row locks, no compare-and-swap. Google Sheets is the common case; a
folder of files or a CSV in a bucket is another. These stores get picked for good reasons (the
people using the tool already live in them), and then the job is written as if they were a
database.

Two things go wrong:

1. **Partial saves.** A job that writes as it goes, row by row, can stop halfway. With no rollback,
   the next run starts from a state nobody designed.
2. **Two runs at once.** A scheduler retries, a person starts a manual run, a redeploy overlaps the
   old task. If both runs act on the outside world (send an email, create a CRM record), the same
   action happens twice. A "lock" written into the same store does not help: without
   compare-and-swap, two runs can both read "free" and both write "mine".

leadscore can run on Google Sheets and contacts real people, so both failures mean emailing
someone twice. See `docs/reference.md`, sections "Store tables" and "Plug-ins" (`Backend`).
Code: `internal/store/sheets/lease.go`, `internal/store/sheets/commit.go`,
`internal/engine/run.go`.

## Decision

**Load, work in memory, save in phases around side effects.**

1. Load every table the run needs at the start into one in-memory model.
2. Do all the thinking in memory. The store only moves rows; it holds no logic.
3. **Phase 1, before any outside call:** save everything that decides who may be acted on (new ids,
   identity keys, dedupe keys, cursors) in one write the store applies all-or-nothing (one Sheets
   `batchUpdate`). After a crash, the next run knows everything this run knew.
4. **During side effects:** each batch saves its own small record of what it is about to do and
   what it did (see [ADR-0003](0003-a-ledger-with-intent-and-called-markers-for-at-most-once-vendor-calls.md)).
5. **Phase 2, after side effects:** save the rest (reports, derived views, health).
6. Anything fully recomputed each run (a ranked view) is written last and may be skipped; losing it
   costs nothing.

**Hold the run lock somewhere that has compare-and-swap.**

1. The lock is a lease: an owner and an expiry, kept in a store with a conditional write. On
   Sheets it is one Cloud Storage file, written with `ifGenerationMatch`, so two runs cannot both
   take it. On SQLite it is a row, taken inside a transaction.
2. The lease expiry is the run's deadline, plus its save budget, plus a margin. The run's context
   is cancelled before the lease expires, so no run outlives its lease.
3. The run checks it still holds the lease before every save and before every batch of side
   effects, and releases it only if it still holds it. A stalled run that lost its lease writes
   nothing.
4. An expired lease is taken over and logged; a live one means "skip this run", also logged.
5. **One writer.** Only the leased run writes the job's own tables. People and CLI commands write
   request rows (the `Overrides` tab) that the next run applies, so they need no lease and never
   race a run.

## Alternatives Considered

- **A lock row inside the same store.** Rejected: without compare-and-swap, two runs can both take
  it. Reconsider if the store gains a conditional write.
- **"Only one scheduler per spreadsheet" as a rule in the README.** Rejected: manual runs,
  scheduler retries and redeploys still overlap, and the failure is silent.
- **Write as you go, row by row.** Rejected: every crash leaves a new partial state to reason
  about.
- **Move to a real database.** The right answer once the store is no longer the point. leadscore
  ships SQLite (a transaction and a lease row) for teams that can use it. This ADR is for when the
  weak store is a requirement.

## Consequences

**Easier.** Crash recovery is "load and run again". Overlapping runs are impossible by
construction, not by convention. Plug-in stores implement three calls (read, lease, commit).

**Harder.** The whole working set must fit in memory and in one all-or-nothing write; large first
imports must be chunked, with progress saved in the same write. Setup needs a second store (a
bucket) just for the lock. The run needs a hard deadline sized against the lease, and the deploy
config (job timeout, schedule) must agree with it; `leadscore doctor` checks that.

**Accepted gap.** Between "check the lease" and "write", the lease can expire. The deadline being
shorter than the lease makes this rare; it is not impossible.
