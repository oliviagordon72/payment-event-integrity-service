# Working on Payment Event Integrity Service

Read README.md and docs/CODEX_GUIDE.md before changing behavior. Keep this a small
Go/PostgreSQL project that a student can explain in an interview.

## Invariants

- Verify webhook signatures using the original body and a bounded timestamp.
- Acknowledge only after durable inbox insertion. Duplicate deliveries return success.
- Deduplicate delivery IDs separately from financial capture/refund IDs.
- Each journal posting has two equal and opposite legs in the same currency.
- Use integers in provider minor units. Never use floating point for money.
- Keep the journal append-only. Corrections need new postings and an explicit design.
- Lock the capture row before deciding whether a refund fits the remaining amount.
- Commit financial effects and event completion together.
- A worker must own its current lease token before processing. Expired tokens cannot post.
- Reconciliation must use an independent provider inventory, paginate completely,
  identify its source, and report differences without silently changing balances.
- Keep direct-account test mode, card payments, one final capture per charge,
  and successful refunds as the documented scope until explicitly expanded.

## Verification

Run gofmt, go vet, unit tests, PostgreSQL tests with the race detector, and the
two fuzz targets after relevant code changes. Set REQUIRE_DB_TESTS=1 in required
checks so missing PostgreSQL cannot silently skip integration coverage.
Use Docker Compose if Go or PostgreSQL is not installed locally.
Do not substitute SQLite or mocks for PostgreSQL concurrency tests.
Do not weaken assertions to make a broken implementation pass.
Do not claim that a test ran unless its command actually succeeded.

Never commit .env, API keys, webhook secrets, or real customer data.
Do not add a UI, Kafka, Redis, Kubernetes, live payments, or extra services unless requested.
Keep dependencies small. Explain transaction boundaries and limitations in documentation.
