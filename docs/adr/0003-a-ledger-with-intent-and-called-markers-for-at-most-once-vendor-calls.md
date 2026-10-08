# ADR-0003: A ledger with intent and called markers, plus find-or-create calls, for vendor actions that must not happen twice

## Status
Proposed

## Context

Some outside calls must not happen twice for one person: enrolling them in an email sequence,
creating a CRM deal, sending a message. Network calls fail in an awkward way. A timeout does not
tell you whether the vendor acted. A crash after the call but before saving its result leaves no
trace of the call at all. A retry loop that treats "no result" as "never happened" will
eventually act twice.

The usual fixes each cover half the problem. A "done" flag written after the call misses the crash
window. Looking the object up at the vendor before each call (reconcile by lookup) misses objects
the vendor's search has not indexed yet. Idempotency keys work only where the vendor supports them,
and Apollo and HubSpot mostly do not.

leadscore must never cold-contact a person twice. See `docs/reference.md`, sections "Store tables"
(`Pushes`) and "Plug-ins" ("Sinks", "Conformance suites"). Code: `internal/engine/lanes.go`,
`internal/engine/push.go`, `sinktest/sinktest.go`.

## Decision

**Keep a ledger with one row per (subject, action, step).** Each row has a state and two markers:

1. **`intent_run`**, written in a save *before* a batch of calls, naming the run about to call.
   Cleared by the save after the batch.
2. **`called_at`**, written in the save *after* the batch for every step actually called, and
   **never cleared**.

A crash between the two saves leaves `intent_run` set. The next run reads that as "this call may
have happened" and turns it into `called_at`. So the ledger can lose the result of a call, but
never the fact that a call may have been made. That is the safe direction: at worst we skip a
person we never reached; we never reach them twice.

**Decide "may we call again?" from the markers, not the outcome.** A step with `called_at` or a
foreign `intent_run` holds the person, whatever the outcome (timeout, error, crash). Only a step
that was never called releases them.

**Make every call find-or-create, keyed by the step.** Before creating, the call looks for an object
it may have created earlier (by our own id stored on the vendor object, then by natural key such as
email), and reuses it. This covers the case the ledger cannot see: a call that succeeded at the
vendor whose answer was lost.

**Enforce it with a shared contract test.** Every sink runs one suite (`sinktest`) that calls a
step, throws the answer away, builds a fresh sink as the next process would, calls again, and
asserts exactly one object exists at the vendor.

**Trust the ledger only when it has not shrunk.** The run records the ledger's row count; if a load
finds fewer rows (someone deleted rows in a spreadsheet), all such calls stop until it is fixed.

## Alternatives Considered

- **A "done" flag after the call only.** Rejected: a crash after the call and before the save
  loses the fact that it was called.
- **Reconcile by vendor lookup only.** Rejected: vendor search can lag a fresh write (HubSpot's
  does), so a lookup right after a lost create finds nothing. It stays as the find-or-create half.
- **Vendor idempotency keys.** Preferred where a vendor supports them for the call in question;
  they replace find-or-create for that call, not the ledger.
- **Exactly-once.** Not available across a network to a vendor without transactions. This design
  chooses at-most-once for actions that must not repeat, and accepts that a crash can leave a
  person not contacted.

## Consequences

**Easier.** "Could we have contacted this person?" has a single, durable answer. Retries are safe
to automate. A new sink proves its safety by passing one suite.

**Harder.** Two saves per batch instead of one. Every sink must store our id on the vendor
object, or have a natural key, to make find-or-create possible. Retry policy needs care: a step
that keeps failing is held, not released, so a person can be stuck until someone resets it (a
`retry` row in `Overrides`, and `leadscore doctor` lists failed steps).

**Accepted cost.** A crash in the window between the two saves can leave a person marked as
possibly contacted who never was. They are skipped, not contacted twice.

**Related.** The two-phase save and run lease this ledger sits inside are
[ADR-0002](0002-batch-job-on-a-store-without-transactions-saves-in-two-phases-under-a-cas-lease.md).
