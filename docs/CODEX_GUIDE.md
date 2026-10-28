# Recreate and learn the project with Codex

## Start from this implementation

Unzip the project, open its folder in your editor, and open Codex in that folder.
Keep the work in your WSL filesystem if that is where Go and Docker are configured.
Start with this prompt:

```text
Read AGENTS.md, README.md, and docs/VALIDATION.md. I am recreating this project
for my portfolio and need to understand every important design choice.

Inspect the implementation and run the documented local checks. Use real
PostgreSQL for transaction and concurrency tests. Report what ran, what passed,
what was skipped, and what remains unverified. Fix real defects without
weakening the tests. Do not use live Stripe payments or print secrets.

Then explain the path of one $10 capture and a $2.50 refund through the HTTP
handler, inbox, worker lease, transaction, and journal. Show the actual relevant
code and SQL. Keep explanations direct and suitable for an undergraduate.
```

Read the explanations and trace one event yourself. A generated repository becomes
useful interview experience when you can explain why its failure cases are safe.

## Rebuild it yourself in stages

For a blank-folder recreation, copy the following specification into a separate
working folder. Do one phase at a time and inspect the diff and checks after each.
Do not point the blank-folder build at the reference implementation's database.

### Initial specification

```text
Build Payment Event Integrity Service using Go, PostgreSQL 16, Docker Compose,
Stripe's test API, and GitHub Actions. Keep it small enough that I can understand
and explain it. First write an implementation plan and AGENTS.md containing
the invariants below, then implement Phase 1. Avoid unnecessary services.

Scope: one direct Stripe account, test-mode card payments, one final capture
per charge, partial refunds. Snapshot webhook events with a pinned API version.
No UI, actual payment creation, fees, disputes, FX, payouts, or live mode.

Invariants:
1. Verify the raw webhook body, timestamp, and Stripe-Signature before parsing.
2. Return success only after durable inbox insertion. Unique provider event ID.
3. Use separate unique financial keys capture:charge_id and refund:refund_id.
4. Each append-only journal posting has equal debit and credit in one currency.
5. Represent money with integers in minor units. Never floating point.
6. One database transaction contains journal posting, projection update, and
   inbox completion. Crashes before commit leave none of those effects.
7. Lock each capture before checking and incrementing its cumulative refunds.
8. Lease workers with SKIP LOCKED, expirations, unique tokens, retries, and a
   terminal failure state. Expired workers cannot post using old tokens.
9. Retry refunds that arrive before captures. Arrival timestamps cannot define
   provider event ordering.
10. Reconciliation reads independent, fully paginated provider inventory,
    reports differences, identifies its source, and never silently repairs money.

Use standard Go HTTP handlers and pgx. Keep the architecture and dependencies
small. Explain decisions and run each phase's acceptance checks before moving on.
```

### Phase 1: Database and ledger

```text
Implement the database schema and posting logic. Store each two-leg posting in
one immutable journal row and expose the two legs through a view. Add unique
event and financial keys, integer money constraints, and append-only guards.
Use a capture projection row to serialize refunds and enforce total refunds <=
captured amount. Add a migration command and PostgreSQL integration tests.

Acceptance: capture 1000, refund 250, net clearing 750, every journal balances.
Duplicate financial effects cannot change net balances. Two concurrent refunds
must not exceed the capture. Journal UPDATE, DELETE, and TRUNCATE are rejected.
```

### Phase 2: Signed webhooks and durable inbox

```text
Implement POST /webhooks/stripe. Verify the original bytes and the timestamp,
support multiple v1 signatures during rotation, bound request size, and reject
unsupported mode/account/version envelopes. Persist verified events before
responding. Duplicate provider event IDs must return success. Keep processing
out of the request path. Add health, authenticated balance, and event-status reads.

Acceptance: invalid signatures cannot enqueue. A valid response means an inbox
row exists. Twenty replayed deliveries still produce one inbox row. A distinct
event ID for the same capture still produces one financial posting.
```

### Phase 3: Worker ownership and crash recovery

```text
Implement concurrent workers with PostgreSQL SKIP LOCKED, a lease expiry and
fresh random token per claim. Check token ownership and expiry under an inbox
row lock before posting. Atomically commit money and event completion. Retry
refund-before-capture dependencies with bounded backoff and dead-letter state.
Make sure a crash on the final allowed attempt cannot leave a job stuck forever.

Acceptance: multiple workers are safe; a reclaimed job rejects the old token;
an injected precommit failure rolls back all writes; a child process that exits
after journal insertion but before commit is recovered after lease expiration.
Explain the difference between a transaction lock and a time-based lease.
```

### Phase 4: Provider reconciliation

```text
Implement an on-demand reconciliation command using Stripe test API GET
/v1/charges and /v1/refunds with explicit API version, timeouts, and complete
pagination. Do not rely only on locally received event IDs or an embedded
charge.refunds list. Add an explicitly labeled synthetic fixture provider.

Compare provider object identity, amount, currency, parent charge, and eligibility
state against local journal postings. Persist reports and exit unsuccessfully
on discrepancies or incomplete provider reads. Do not automatically repair.
Document the creation-cutoff and cross-system snapshot limitations.

Acceptance: seed missing captures/refunds and changed amount/currency/status/
charge relationships. Report the differences. Test multiple API pages and API
failure. Synthetic reports must identify themselves as synthetic.
```

### Phase 5: Reproducibility and evidence

```text
Add a small signed-webhook workload generator with repeated IDs, distinct events
for the same financial object, and deterministic shuffled order. Include an
independent provider fixture and intentional omissions. Check final balances.
Add signature and payload fuzz targets, race-enabled PostgreSQL tests, Docker
Compose, and a GitHub Actions workflow that cannot silently skip DB tests.

Acceptance: another person can run the demo and tests from the README. Print
measured counts. Write a validation report separating executed checks from
future checks. Do not invent latency, throughput, coverage, or workload totals.
```

## Review prompt

Use this after a meaningful implementation change:

```text
Review the current diff as a payments-backend reviewer. Trace failures around
every transaction boundary and every lease transition. Find ways duplicates,
out-of-order snapshots, concurrent refunds, provider timeouts, or worker death
could create missing or duplicated postings. Check whether database constraints
actually enforce the documented guarantees. Show concrete counterexamples and
add a regression test for each confirmed defect. Report unresolved limits.
```

## What to understand before interviewing

1. Why event-ID deduplication alone is insufficient.
2. Why a separate read followed by insert can race and what makes the transaction safe.
3. How row locks prevent two concurrent refunds from overspending the capture.
4. What happens if the service dies before/after inbox persistence or posting commit.
5. Why a lease needs a fresh token and how stale workers lose authority.
6. How debit and credit keep each posting balanced while balances still change.
7. Why an event with an earlier timestamp can arrive later.
8. How reconciliation finds an event that never reached your endpoint.
9. Why you paginate refunds separately and why snapshots can disagree during activity.
10. Which guarantees depend on PostgreSQL and which are outside this project's scope.

Describe the work using the behavior and evidence you personally ran and
understand. The included tests do not establish production readiness or prove a
real Stripe sandbox run. Keep your resume's counts aligned with actual reports.
