# ADR-0001: Store a webhook before acknowledging it; parse it later

## Status
Proposed

## Context

A vendor sends us an event by webhook. The vendor treats our 2xx as "delivered" and forgets the
event. If we answer 2xx and then lose the event (a crash, a failed write, a redeploy mid-request),
nobody will ever send it again. For some events that loss is harmless. For an opt-out it is not: a
lost unsubscribe means we may email someone who asked us to stop.

The leadscore receiver hits this in its sharpest form. It can write events to Google Sheets, which
allows about 60 writes a minute per account, is slow, and sometimes answers "slow down". The easy
designs all answer first and write later: an in-memory buffer flushed every few seconds, a goroutine
per request, or parsing the body and writing only the fields we use. Each one acknowledges an event
that is not yet safe.

See `docs/reference.md`, section "Receiver". Code: `internal/receiver/queue.go` and
`internal/receiver/handler.go`.

## Decision

1. **A 2xx means "durably stored".** The handler answers 2xx only after the store has confirmed
   the write. It never answers from a buffer.
2. **Batch to fit the store, but hold the request open while batching.** Requests are gathered
   for a short window (2 seconds) and written in one call. Each request waits for its batch's
   write.
3. **Cap the hold and fail loudly.** The whole hold (window plus retries) has a hard cap, meant
   to be below the sender's own timeout (10 seconds). Past the cap, or on any write failure,
   every request in the batch gets 5xx, so the sender can retry.
4. **Store the raw request, not the parsed result.** The receiver checks the secret, strips it,
   and stores the body as-is (shrunk to fit one cell when too large) with a sequence number.
   Parsing happens later, in the run. A parser bug is then fixed by re-reading stored bodies,
   not by asking the vendor to resend.
5. **Make redelivery harmless.** Because we ask senders to retry, every event gets a dedupe key
   from its content, and processed keys are kept for a year. A retry of an event we did store is
   then a no-op.

## Alternatives Considered

- **Answer 2xx at once and write in the background.** Rejected: any crash or failed write between
  the answer and the write loses the event for good. Reconsider only for events whose loss changes
  nothing (pure analytics), and say so in the handler.
- **Parse on receipt and store only the fields we need.** Rejected: a parser bug or a field we
  later need is unrecoverable, and parsing before storing widens what an unauthenticated request
  can reach. Reconsider if raw bodies are too large or too sensitive to keep, and then keep a
  scrubbed copy.
- **Put a queue (Pub/Sub, SQS) in front.** Not wrong in general: a managed queue is the same rule
  with someone else's durable store. Rejected here because each self-hosting team would need to
  set one up.

## Consequences

**Easier.** "Did we lose an event?" has one answer: if the sender got a 2xx, the event is stored.
Parser fixes replay history. Silence detection can trust the store.

**Harder.** Requests are held open for seconds, so the receiver bounds what can pile
up (unauthenticated body reads and waiting batches) and answers 503 when full. Retries mean duplicates are normal, so the
dedupe key is part of correctness, not a nice-to-have. Its rules (what makes two events "the
same") need their own tests, including a retry that crosses a day or month boundary.

**Depends on the sender, and that is not confirmed.** The rule only prevents loss if the sender
retries on 5xx or timeout. Apollo's request timeout, and whether it retries at all, are
unconfirmed. The handler can also answer at about 11 seconds (the 10-second cap plus one second
for a slow store). So "below the sender's timeout" in point 3 is a target, not a checked fact.
Until it is checked, silence detection and the pre-push opt-out lookups are the backstop.

**Secret before body, with one exception.** The secret is checked before the body is parsed, so
an unauthenticated request never reaches a parser. The exception: the secret may be sent in a
top-level body field for senders that cannot set headers, so the receiver reads the body's first
level to find it. The header is preferred.
