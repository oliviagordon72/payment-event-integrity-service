# Reconciliation fault-injection campaign

This add-on exercises the existing Go reconciliation implementation against real
PostgreSQL, with an independent synthetic provider inventory. It deliberately
creates 120 mismatches, checks their exact keys and issue classifications, and
checks clean controls for false positives. It makes no requests to Stripe.

## Install into the existing project

The add-on ZIP contains only new files. Merge its `internal`, `scripts`, and
`docs` folders into your existing `~/projects/payment-integrity` directory.
Keep your existing `.env`, Compose configuration, code, and payment data.

The resulting paths must include:

```text
internal/service/reconciliation_campaign_test.go
scripts/run-reconciliation-campaign.sh
docs/RECONCILIATION_CAMPAIGN.md
```

The test uses helper functions already supplied in `event_test.go` and
`integration_test.go`. Use this with the Payment Event Integrity Service from
this conversation, rather than a different reconciliation implementation.

## Run using your existing Docker project

In WSL, from the existing project directory:

```bash
cd ~/projects/payment-integrity
docker compose -p payment-integrity ps db
bash scripts/run-reconciliation-campaign.sh
```

The `db` service must be running. If it is stopped, start it with:

```bash
docker compose -p payment-integrity up -d db
```

The wrapper runs the existing `test` service with a Go test command override and
an evidence-output environment variable. It uses the project's existing Go
module cache and mounted source. No image rebuild or Stripe credentials are
needed. The wrapper deliberately uses the observed `payment-integrity` project
name, not `payment-stripe-sandbox`.

The Go test creates two uniquely named schemas and drops those schemas in test
cleanup. Its connection search path points only at its own schema, so the
application's public-schema demo and Stripe records are excluded. It preserves
the journal's append-only guards and uses ordinary inbox insertion, lease claim,
and worker processing to populate each isolated ledger. It does not rewrite,
delete, or truncate the application's existing journal.

## Expected summary

```text
Clean baseline issues: 0
missing_capture: 20/20 detected
missing_refund: 20/20 detected
wrong_amount: 20/20 detected
wrong_currency: 20/20 detected
wrong_parent_charge: 20/20 detected
wrong_refund_status: 20/20 detected
Total detected: 120 / 120
False positives: 0
False negatives: 0
Clean control lifecycles: 200
Clean control postings flagged: 0
Passed: True
Evidence: docs/evidence/reconciliation-campaign.json
```

The test fails if expected findings are missed, unexpected findings appear,
classification is wrong, or a clean control is flagged. Actual results come
from your run; the reference report is evidence of the packaged test's earlier
validation, not evidence that you have run it yourself.

## What each injection changes

| Failure type | Cases | Controlled difference | Required finding |
| --- | ---: | --- | --- |
| Missing capture | 20 | Provider has a captured charge; its event is omitted locally | `missing_posting` for its capture key |
| Missing refund | 20 | Provider has a successful refund; its event is omitted locally | `missing_posting` for its refund key |
| Wrong amount | 20 | The local successful refund has a different valid amount | `value_mismatch` for its refund key |
| Wrong currency | 20 | Local capture currency differs from provider currency | `value_mismatch` for its capture key |
| Wrong refund status | 20 | Local successful refund was posted; provider inventory marks it failed | `state_mismatch` for its refund key |
| Wrong parent charge | 20 | Local refund points at a different valid captured charge | `value_mismatch` for its refund key |

There are six injection types and three existing report classifications. The
reconciler does not label amount, currency, and parent-charge discrepancies as
three separate report kinds; the injector's ground truth supplies those labels.

For the wrong-parent case, both charges exist and reconcile correctly. This
ensures the test actually exercises comparison of the refund's parent identity,
rather than merely reporting a missing or unsupported parent.

Amounts and currencies vary deterministically under seed 42, including USD, EUR,
GBP, and JPY. Values are always minor units. Provider list order differs from
event write order. The fixture's timestamps and cutoff are fixed test values,
not the date or time of execution.

## Baseline, faulted dataset, and denominators

The campaign has 320 scenario cases: 120 fault scenarios and 200 untouched clean
payment lifecycles. Some scenarios contain only a capture; wrong-parent cases
contain two captures. The clean lifecycles contain a capture and a partial refund.

| Quantity | Clean baseline | Faulted scenario |
| --- | ---: | ---: |
| Provider objects | 620 | 620 |
| Expected financial postings | 620 | 600 |
| Actual journal postings | 620 | 580 |
| Expected discrepancy findings | 0 | 120 |

The faulted report must contain 40 missing postings, 60 value mismatches, and 20
state mismatches. Twenty failed provider refunds reduce expected postings, while
omitted captures/refunds reduce local postings. A mismatch count is not simply
the difference between the expected and actual totals.

The faulted dataset has 400 clean-control posting keys and 100 other healthy
supporting posting keys, for 500 healthy keys total. A perfect result means:

- All 120 independently specified `(posting key, issue kind)` pairs were found.
- No issues were reported for the 500 healthy keys, including 400 control keys.
- The clean baseline's 620 postings produced zero issues.
- Every local journal posting remained balanced with exactly two legs.

False negatives are missing expected key/type pairs. False positives include
unexpected key/type pairs and duplicate findings. A wrong classification counts
as one false positive and one false negative, and is also reported separately.
The scorer has its own tests covering empty results, wrong classifications,
healthy-record flags, and duplicate findings. A detector that returns nothing
cannot pass, nor can one that merely reports 120 arbitrary issues.

## Evidence and interpretation

The generated `docs/evidence/reconciliation-campaign.json` includes:

- Source label `synthetic_postgresql_campaign`, seed, and dataset SHA-256
- Independent ground-truth keys, classifications, and injection labels
- Full clean-baseline and faulted reconciliation reports
- True-positive, false-positive, false-negative, and per-type counts
- Control counts, classification mistakes, duplicate findings, and pass status

The dataset digest covers the provider inventories, local events, expected
findings, and key lists. Same seed and implementation produce the same fixture.
Keep the test source and generated report in the same commit for reproducibility.

A successful run supports the statement that this controlled dataset detected
120/120 seeded discrepancies across six injection types, with zero false
positives and zero false negatives. It does not establish accuracy for unseen
production data, or mean 120 failures were injected into Stripe's real API.

## CI and native execution

Because its filename ends with `_test.go`, the existing GitHub Actions
`go test -race ... ./...` step automatically includes this campaign.
`REQUIRE_DB_TESTS=1` prevents missing database configuration from silently skipping
the integration test. No existing workflow change is required for coverage.

To generate the JSON evidence on a native Go/PostgreSQL runner, set the usual
`TEST_DATABASE_URL` and run:

```bash
bash scripts/run-reconciliation-campaign.sh --local
```

In CI, that command can be followed by uploading the generated JSON as an
artifact. The native route and the Docker route run the same Go tests.

## Validation of this add-on

The supplied reference report records 120/120 detections with zero false
positives, false negatives, or clean-control flags. The full Go suite passed with
the race detector against native PostgreSQL 16.15; `go vet` and both fuzz targets
also passed. Service-package statement coverage in that run was 73.6%.

The native PostgreSQL launcher used the same local UID shim described in the
original project's validation notes, to permit startup in this root-only test
environment. The shim is not part of this add-on. Docker is unavailable in this
execution environment, so the Docker branch of the runner was checked against
the Compose documentation and parsed as shell, but was not executed here. Run
the wrapper in your working Docker setup to produce your own evidence.
