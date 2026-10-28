# Validation performed on this copy

Executed on October 27, 2026. These are measured local results, not production
claims or a record of a real Stripe sandbox transaction.

## Passed

| Check | Result |
| --- | --- |
| Go build | Native executable built with Go 1.22.2 and `-buildvcs=false` |
| Formatting | Go files formatted with gofmt |
| Static analysis | `go vet ./...` passed |
| PostgreSQL integration and race detection | `REQUIRE_DB_TESTS=1 go test -race -count=1 -v -coverprofile=coverage.out ./...` passed against PostgreSQL 16.15 |
| Service-package statement coverage | 73.4%; command package coverage was 0% in the Go test run |
| Signature fuzzing | 5-second run passed; 32,753 executions in the final run |
| Payload fuzzing | 5-second run passed; 9,920 executions in the final run |
| Migration | Schema creation and a second idempotent migration both passed |
| HTTP demo | 30 lifecycles, 240 deliveries, 120 unique event IDs, 120 duplicate receipts; all events completed |
| Demo balance | Expected and observed clearing-balance delta: 22,500 USD cents |
| Clean fixture reconciliation | 60 expected and 60 actual postings, zero issues |
| Seeded fixture reconciliation | Exactly three issues: missing posting, value mismatch, state mismatch; command exited with status 1 |
| Configuration syntax | Compose and GitHub Actions YAML parsed; Python demo syntax checked |

The database suite covers inbox replay, financial-object deduplication,
refund-before-capture retries, concurrent workers, refund limits, lease fencing,
transaction rollback, terminal-attempt recovery, immutable journal guards, and
randomized deliveries. The crash test launches a child process that exits after
transaction writes and before commit, then recovers the expired job.
`TestCrashHelper` skips in the parent test run by design and runs in that child.
No database integration tests were skipped for missing database configuration.

The command package was exercised separately by building the executable, running
the migration twice, starting its HTTP server, running the Python demo, and
running both clean and seeded fixture reconciliation commands. Those manual
command checks are not included in the Go statement-coverage percentage.

The isolated runner used a local UID shim to start native PostgreSQL in a
root-only sandbox. The shim is outside the project and is not shipped. Docker's
ordinary PostgreSQL launch was not exercised in this environment.

## Saved evidence

- [Synthetic HTTP workload result](evidence/demo-report.json)
- [Clean reconciliation result](evidence/reconciliation-clean.json)
- [Seeded reconciliation result](evidence/reconciliation-seeded.json)

The JSON contains synthetic identifiers only. It identifies the source as a
synthetic fixture or local workload. It does not contain credentials.

## Not executed here

- Docker image build and Docker Compose startup, because Docker was unavailable.
- GitHub Actions on a hosted runner. The workflow is provided but was not pushed
  to GitHub or executed there.
- Actual Stripe test-account webhooks or reconciliation API calls. API pagination,
  headers, error handling, and event shapes were exercised with synthetic fixtures
  and a mocked HTTP transport. Complete the README's Stripe test-environment
  procedure to validate against your account and its selected API version.
- Extended fuzzing, prolonged soak tests, throughput/latency benchmarks, or
  production deployment. No claims are made for those checks.
