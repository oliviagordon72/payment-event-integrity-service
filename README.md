# Payment Event Integrity Service

A small Go/PostgreSQL REST service for receiving signed Stripe snapshot webhooks,
recording card captures and refunds in an append-only double-entry journal, and
finding differences between provider records and the local ledger.

## Run the synthetic demo

Requires Docker with Compose and Python 3. Run these commands in this directory,
for example in your WSL terminal with Docker Desktop's WSL integration enabled.

```bash
cp .env.example .env
docker compose up --build -d app
curl http://localhost:8080/healthz
python3 scripts/demo.py
docker compose --profile tools run --rm test
```

The default demo creates no Stripe payments and needs no Stripe account. It sends
240 signed deliveries for 30 synthetic lifecycles, including two distinct events
for each capture and refund, repeated event IDs, and shuffled delivery order.
It waits for processing and checks that the clearing balance increased by 22,500
USD cents, which is 30 times a 1,000-cent capture minus a 250-cent refund.
It writes `demo-report.json` with measured results and `demo-provider.json` with
the independently generated provider inventory. IDs include a fresh run prefix.

```bash
# Reconcile the generated fixture against a fresh, otherwise empty demo database.
docker compose cp demo-provider.json app:/tmp/provider.json
docker compose exec app /payments reconcile-fixture /tmp/provider.json

# Inspect account balances. Use the ADMIN_TOKEN value in your .env file.
curl -H 'Authorization: Bearer local-demo-token-change-me' localhost:8080/v1/balances
```

For a seeded omission, use a separate fresh checkout with a separate Compose
project name, then run `python3 scripts/demo.py --omit-refunds 2`. Copy and reconcile
the new fixture as above. The report should contain two `missing_posting` issues
and the command should exit unsuccessfully. Reconciliation compares the full
in-scope inventory, so fixtures from one demo run should not be compared to a
database containing unrelated runs. Pause other writes when checking a fixture.

For a larger synthetic exercise, use `--lifecycles 3000 --timeout 300` and report
the counts actually printed. Larger volume is optional and has no preset timing claim.

## Use Stripe's test environment

1. Use a dedicated Stripe test account or sandbox containing only this project's
   supported card-payment workload.
2. Run `stripe listen --events charge.succeeded,charge.captured,refund.created,refund.updated --forward-to localhost:8080/webhooks/stripe`.
3. Put that listener's `whsec_...` value in `.env` as `STRIPE_WEBHOOK_SECRET`.
   Set a random `ADMIN_TOKEN` of at least 16 characters.
4. Match `STRIPE_API_VERSION` to your snapshot event destination and API calls.
   This project defaults to `2026-09-30.endive`. A mismatched envelope is rejected.
5. Recreate the app with `docker compose up -d --force-recreate app`. Generate a
   test card payment and partial refund, then inspect the delivered events and balances.
6. Put a test-mode secret or restricted read key in `.env` as `STRIPE_SECRET_KEY`.
   Run `docker compose --profile tools run --rm reconcile`.

The reconciliation command issues read-only GET requests to `/v1/charges` and
`/v1/refunds`, follows `starting_after` pagination, and persists a JSON report.
It rejects live API keys. The service rejects live-mode and Connect/context events.
Its API integration is implemented with Go's HTTP client; it does not require a
Stripe SDK. Webhook HMAC verification uses standard cryptographic primitives.

## What happens to an event

1. `POST /webhooks/stripe` verifies the raw request body's HMAC and timestamp,
   validates the snapshot envelope, and inserts it into `inbox` by event ID.
   It returns HTTP 202 only after the insert succeeds. A duplicate event also succeeds.
2. Workers claim due rows using `FOR UPDATE SKIP LOCKED`, a random lease token,
   and a 30-second lease. Failed jobs retry with exponential delay, up to 12 claims.
3. Processing locks the inbox row and checks its lease. It normalizes an eligible
   charge/refund, locks the charge's projection row, and posts any new financial effect.
4. The journal insertion, refund-limit counter, and inbox completion commit in
   one database transaction. A failure before commit rolls them all back.
5. If a worker dies, another worker can reclaim its expired lease. An older token
   cannot post after reclamation. Once processing holds the inbox row lock,
   claimers skip that row until the transaction ends.

Refunds received before their capture are retried. If the capture never arrives,
the refund ultimately becomes `dead`. Inspect `last_error`, reconcile with the
provider, and explicitly replay after resolving the dependency. No automatic
financial repair or dead-letter replay endpoint is included.

## Ledger model

| Effect | Financial key | Debit | Credit |
| --- | --- | --- | --- |
| Capture | `capture:ch_...` | Stripe clearing | Merchant payable |
| Refund | `refund:re_...` | Merchant payable | Stripe clearing |

All amounts are integers in the currency's minor unit. For USD, 1,000 means $10.
Each `journal` row holds the two equal legs as one immutable record; the
`ledger_entries` view exposes those legs as separate signed rows. Positive means
debit and negative means credit. The sum per posting and per currency is zero.
The schema fixes account direction by posting kind. Triggers reject journal
updates, deletes, and truncation by ordinary application queries.

`inbox.event_id` handles delivery replay. `journal.posting_key` and
`UNIQUE(kind, object_id)` handle separate events that describe the same capture or
refund. A charge row lock serializes refunds for that charge, preventing two
concurrent refunds from each passing a stale remaining-amount check.

The `captures` table is a mutable projection and synchronization point. The
append-only claim applies to the journal. This is effectively-once financial
projection within PostgreSQL, using at-least-once deliveries and transactions.
The project does not promise exactly-once distributed delivery.

## REST endpoints and commands

| Interface | Purpose | Protection |
| --- | --- | --- |
| `GET /healthz` | Database readiness | Local deployment binding |
| `POST /webhooks/stripe` | Persist signed events | Stripe signature |
| `GET /v1/events/{id}` | Event state, attempts, error | Admin bearer token |
| `GET /v1/balances` | Debit minus credit by account/currency | Admin bearer token |
| `payments migrate` | Create local schema | Database credentials |
| `payments serve` | HTTP server and workers | Configured secrets |
| `payments reconcile` | Compare to Stripe test API | Test read API key |
| `payments reconcile-fixture PATH.json` | Explicit synthetic comparison | Local command access |

Reports identify their source, include the cutoff and object/posting counts, and
classify missing postings, changed values, provider-state disagreements, and
unexpected local postings. A report with issues makes the command fail, so a
scheduler can alert on it. The job runs on demand; no external schedule is installed.

## Development checks

With Go 1.22+ and PostgreSQL 16 available locally:

```bash
go mod download
export DATABASE_URL='postgres://payments:local-development-only@localhost:5432/payments?sslmode=disable'
go run ./cmd/payments migrate
export TEST_DATABASE_URL="$DATABASE_URL"
make integration
make fuzz
go vet ./...
```

Tests create and drop a unique schema per test. Use a dedicated local test
database with schema-creation privileges. Never point this harness at an
important database. `go test ./...` without `TEST_DATABASE_URL` runs unit tests
and skips database tests; `REQUIRE_DB_TESTS=1` converts missing configuration to failure.
The Compose test service and GitHub Actions workflow require the database tests.

| Test | Evidence |
| --- | --- |
| Signature tests and fuzzing | Raw bytes, changed payloads, secret mismatch, timestamp bounds, rotation |
| HTTP integration | Durable inbox acknowledgement, replay, access control |
| Refund-before-capture | Dependency retry and eventual correct posting |
| Concurrent workers | 8 workers, 120 distinct refund events, 40 financial refunds |
| Concurrent refund limit | Only one of two competing oversized total refunds can commit |
| Lease fencing | Reclaimed jobs reject the old token |
| Precommit failure | Journal, projection, and event state roll back together |
| Process death | Child exits after writes but before commit; expired lease is recovered |
| Randomized delivery | 360 deliveries for 30 lifecycles, including replay and reordering |
| Reconciliation | Seeded omissions and changed amount, currency, charge, and status |
| API pagination | Separate charge/refund pages, cursor and version headers |

## Scope and limits

- One direct Stripe account and test-mode card payments. One final capture per
  charge, including a final partial capture. Incremental/multiple captures are
  unsupported: differing capture amounts produce a conflict rather than a second posting.
- Only `charge.succeeded`, `charge.captured`, `refund.created`, and `refund.updated`
  produce financial effects. Refunds post when `status` is `succeeded`. Pending,
  failed, and canceled snapshots do not create refund postings. Unknown event
  types are durably accepted and marked done without financial effects.
- Later failure/reversal of an already posted refund is surfaced by
  reconciliation. Automated reversal entries are future work. No fees, payouts,
  disputes, FX, settlement accounting, revenue recognition, or general ledger integration.
- Reconciliation reads paginated provider state and a consistent local database
  snapshot. Those systems do not share a transaction. `created <= cutoff` bounds
  object creation, not later captures/refund-state changes. Run on quiet test
  data; recheck discrepancies after queues drain. It is an observation, not
  proof of fraud or data loss. The full test-account inventory is the scope.
- Compose binds to localhost and uses development database credentials. The
  app's database role owns the schema for simplicity. Production would need a
  separate migration owner, restricted runtime privileges, versioned migrations,
  audited reversal workflows, secret management, HTTPS, monitoring, and retention.
  A database owner can alter triggers or drop tables; the journal is not tamper-proof.
- The demo and mocked provider tests are synthetic evidence. A real Stripe
  sandbox run is a separate validation step. Do not present generated counts or
  unrun CI checks as measurements.

## References

- [Stripe webhooks](https://docs.stripe.com/webhooks)
- [Webhook delivery and signatures](https://docs.stripe.com/events/manage-webhook-endpoints)
- [Stripe charge object](https://docs.stripe.com/api/charges/object)
- [Stripe refund object](https://docs.stripe.com/api/refunds/object)
- [List charges](https://docs.stripe.com/api/charges/list) and [refunds](https://docs.stripe.com/api/refunds/list)
- [PostgreSQL SELECT and row locks](https://www.postgresql.org/docs/16/sql-select.html)

See [VALIDATION.md](docs/VALIDATION.md) for checks actually run while creating this copy.
