# Design decisions

The load-bearing calls in this system, and why they went the way they did.

This is not a feature list — the [README](../README.md) has that. It is the set of
decisions where **the obvious implementation is wrong**, and understanding *why* is the
difference between running this system and operating it.

Each entry is: the decision, the alternative, and the failure the alternative invites.

---

## 1. The database is never on the hot path of seat allocation

**The decision.** Reserving a seat touches Redis and nothing else. The Postgres row is
written later, asynchronously, by a worker.

**The alternative.** `SELECT … FOR UPDATE` on the event row inside the request. It looks
simpler, and it serialises every buyer in the country on one row lock.

**Why it fails.** A Postgres connection pool caps out around 100 connections. Under a
drop, 1,000 concurrent buyers queue on the pool, the HTTP workers fill up waiting for
it, latency climbs, clients retry, and the retries make the pool worse. The database
becomes the thing that has to survive the traffic spike it is causing.

Redis, by contrast, executes a Lua script on a single thread. That single-threadedness
is the whole trick: the script is atomic, so it *is* the lock. The seat count moves and
the hold is created in one indivisible step, with no round-trip in between to lose.

**The consequence.** The hot path is Redis-only and cannot be slow because Postgres is
slow. `/readyz` checks both, but `/reserve` does not depend on either.

---

## 2. One Lua script decides the winner — and it also carries the per-user hold

**The decision.** `reserve.lua` does three things in one atomic step: check the seat
count, check that this user does not already hold a seat for this event, then decrement
and set the hold.

**The alternative.** A `DECRBY` followed by a `SET` of the hold key. Two commands, two
round-trips — and a second caller can slip between them and both win.

**Why it fails.** The check-then-act window. It is the TOCTOU race, and in a Lua script
there is no window, because Redis does not interleave anything into the middle of it.

**The consequence.** 1,000 concurrent callers, 100 seats: exactly 100 reservations and
900 clean `409`s. Every time, with no oversell.

---

## 3. A reservation is written to Postgres with `ON CONFLICT DO NOTHING`

**The decision.** The reservation id is a UUID the API mints, and it is the primary key.
The worker's insert cannot create a duplicate.

**The alternative.** A serial primary key, or an insert that upserts.

**Why it fails.** SQS delivers **at least once**. A worker that crashes between the
insert and the message acknowledgement gets the same message again. With a serial key
that retry would create a *second* reservation for the same purchase — two seats, one
customer, and no way to tell which is real.

**The consequence.** Redelivery is free. The retry is a no-op, and the customer is
never charged twice for one hold.

---

## 4. The idempotency key for a retry is the client's key, and the *in-flight* lock is separate

**The decision.** `Idempotency-Key` on `POST /reserve` stores the successful response
for 24 hours and replays it verbatim. A separate, short-lived lock (`SETNX`) stops two
parallel requests with the same key from both reserving.

**The alternative.** A single key holding the response. Then the second request reads a
cache miss (the first has not finished), reserves a second seat, and stores a second
response.

**Why it fails.** The response cache and the concurrency lock solve different problems
and have different lifetimes. A cached response must outlive the request by hours; a
lock must be released the moment the request ends.

**The consequence.** A retry storm is safe: one reservation, one seat, one queue
message. A parallel pair gets `409 in_flight` and reserves nothing.

---

## 5. Every status change goes through one function, and it writes the audit row in the same transaction

**The decision.** `db.TransitionStatus(ctx, pool, id, from, to, audit)` does a
compare-and-set `UPDATE` guarded by a table of legal edges, and inserts the audit row
before committing. The actor is a required argument.

**The alternative.** `UPDATE reservations SET status = … WHERE id = …` wherever the need
arises.

**Why it fails.** Three ways, all of them real:

- **Lost updates.** Two writers read the same status and both write. The compare-and-set
  (`WHERE status = $from`) means only one wins; the other gets `(false, nil)` and has to
  re-read to find out who.
- **Impossible transitions.** `CANCELLED → CONFIRMED` would succeed. The edge table
  makes it impossible *before* the SQL runs.
- **An unaudited transition.** If the audit insert is a separate statement, a crash
  between the two leaves a status change with no record. Inside one transaction, the row
  and the reason for it commit together or not at all.

**The consequence.** "Who confirmed this reservation, and when?" has an answer that does
not depend on log retention.

---

## 6. `CANCELLED` and `REFUNDED` are different statuses, and a partial refund is an *event*

**The decision.** `CONFIRMED → CANCELLED` does not exist. `CONFIRMED → REFUNDED` does.
And a partial refund is recorded as an audit event with `old_status == new_status` —
the status stays `CONFIRMED`.

**The alternative.** One status for "the money went back", and a boolean for "was it
partial".

**Why it fails.** "The customer never paid" and "the customer paid and we gave the money
back" are different facts, and merging them means every report — revenue, refunds,
support lookups — has to re-derive the distinction from other columns. It also makes a
partial refund impossible to represent: the money moved, but the reservation is still
valid.

**The consequence.** The **event** records what was done (money came back). The
**status** records where the reservation ended up (still a valid ticket). A partial
refund is the case that proves they are different concepts.

---

## 7. The price is a server-side snapshot, taken in the same statement that creates the reservation

**The decision.** `events.price_cents` is the source of truth. The reservation carries a
frozen copy — `unit_price_cents` and `currency` — read via scalar subqueries in the
same `INSERT` that creates the row.

**The alternative.** Looking the price up at request time, or trusting a price from the
client.

**Why it fails.** If the organiser changes the price after a ticket is sold, a live
lookup silently changes what that ticket cost. And a client that can name its own price
can ask for a free ticket.

**The consequence.** The total owed is computed in SQL from two frozen columns, so it
can never disagree with them. An event with no price is a `409`, not a zero.

---

## 8. The payment webhook verifies, snapshots and queues — and publishes **before** it answers 200

**The decision.** `POST /webhooks/stripe` verifies the signature over the raw bytes,
snapshots the event into our own struct, publishes it to `stripe-events`, and only then
returns 200. The state machine lives in `internal/payment`, behind that queue.

**The alternative.** Verify and apply inline — read the row, transition it, write the
audit row, return 200.

**Why it fails.** Two problems, one obvious and one not.

- The obvious: Stripe has a short timeout and retries anything that is not a 2xx. An
  inline handler that does database work is one slow query away from a retry storm.
- The subtle one: if the handler applies the event and *then* fails to answer 200,
  Stripe retries, and the retry re-applies. Worse — if the handler answers 200 and the
  event is *not yet durable* (published to a queue that then loses it), the payment
  confirmation is **gone permanently**. Publishing before the 200 means the only way to
  fail is a 500, which Stripe retries.

**The consequence.** The window between "we said 200" and "the event is durable" does
not exist. The same shape as the reservation path — `reserve → SQS → worker`.

---

## 9. The idempotency key for a PaymentIntent is the `reservation_id`

**The decision.** The payment row is written **before** Stripe is called, with a
nullable `stripe_payment_intent_id`. The `reservation_id` is both the row's unique key
and Stripe's idempotency key.

**The alternative.** A random idempotency key, and the Stripe id written back after the
call succeeds.

**Why it fails.** The crash window is between "we wrote the row" and "Stripe answered".
With a random key, the retry is a *new* PaymentIntent — and the customer's card is
charged twice. With the `reservation_id` as the key, the retry returns the **same**
PaymentIntent.

**The consequence.** The crash window is survivable: the row exists with a null Stripe
id, the retry converges on the same intent, and the `reservation_id UNIQUE` constraint
makes a second `/pay` a no-op at the database level too.

---

## 10. The cancel-vs-webhook race **alerts**; it never auto-refunds

**The decision.** If the customer's payment lands after the hold expired, the system does
**not** confirm (the seat may be sold) and does **not** refund. It raises
`stripe_payment_after_expiry_total`, logs an ERROR, and leaves the payment row saying
`SUCCEEDED` while the reservation says `EXPIRED`.

**The alternative.** Auto-refund the customer, or auto-confirm the reservation.

**Why it fails.** Both are money movements on a path that did not reason about the
question. A refund on a race is its own class of bug: it moves money without deciding
what happens to partial refunds, who owns the seat, or whether the seat was re-sold in
the meantime. Confirming sells a seat that may already be gone.

**The consequence.** The mismatch is *visible* — the metric, the log line, and the two
rows that disagree. An operator resolves it. The system does not guess with money.

---

## 11. Stripe is optional — but half-configured is a startup error

**The decision.** No Stripe keys → the payment routes answer 503 and the reservation
core is untouched. Exactly one key → `config.Load()` fails and the process refuses to
start. A malformed key → also a startup error.

**The alternative.** Treat all three the same way.

**Why it fails.** Making a ticket server refuse to boot without a payment account is a
strange dependency — the reservation core does not need Stripe. But an API that can
create PaymentIntents and cannot verify webhooks is exactly the state where payments
get **silently stuck**: the customer is charged, the confirmation never arrives, and
nothing errors.

**The consequence.** An absent key is a choice. A half-present one is a bug, and bugs
that corrupt money should fail loudly at startup.

---

## 12. The admin API signs the request, and the audit records the key's *fingerprint*

**The decision.** Each admin request carries an HMAC over the method, the **full URI**
(query string included), the body and a timestamp. The audit trail records the first 8
hex characters of the signing key's SHA-256 — never the key.

**The alternative.** A bearer token, like the user API.

**Why it fails.** A bearer token travels with every request, and a captured one is
replayable forever. And signing only the path would let a captured signature be
replayed against a different `user_id` — turning a support lookup into a
data-exfiltration tool.

**The consequence.** A captured signature is good for one request, one body, five
minutes. The trail records *which* key acted without the secret being stored anywhere.
And the power is still bounded by the state machine: a paid reservation is refunded,
not cancelled.

---

## 13. Releasing a seat is one atomic operation, and the hold key is the guard

**The decision.** `release_seat.lua` checks whether the hold still exists and puts the
seat back on sale in the **same** Redis operation.

**The alternative.** `INCRBY` the inventory, then `DEL` the hold key. Two commands.

**Why it fails.** Two callers can interleave between them, and the seat goes back on
sale **twice** — a phantom seat, which is an oversell. The hold key is also the only
thing that knows whether the seat was already released: a missing hold means the
expiry path already put it back on sale, so releasing it again would oversell.

**The consequence.** Exactly one caller can ever release a given hold, and a second
attempt gets `409 seat_not_held` instead of a phantom seat.

---

## The through-line

Every one of these is the same shape: **the obvious implementation is one command
shorter, and that command is where the bug lives.** A second round-trip, a second
statement, a second status, a second key. The system is careful because the cheap
version of each of these loses money or loses seats.

That is also why the tests are written the way they are. A test that asserts a status
code is weak; a test that asserts **the invariant** — 100 reservations, never 101 — is
what actually holds the line.
